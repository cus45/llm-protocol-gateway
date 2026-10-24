package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Gemini 原生协议（generateContent）与 OpenAI Chat 的互转。
//
// 形状对照：
//   contents[]           ↔ messages[]          （role: user/model ↔ user/assistant，system 走 systemInstruction）
//   parts[].text         ↔ content（字符串或 [{type:text}]）
//   parts[].functionCall ↔ assistant tool_calls
//   parts[].functionResponse ↔ role:tool 消息
//   generationConfig.maxOutputTokens ↔ max_tokens
//   generationConfig.temperature/topP/topK/stopSequences ↔ temperature/top_p/top_k/stop
//   tools[].functionDeclarations[]   ↔ tools[]（Chat 的 function 工具）
//
// 客户端发来的 Gemini 原生请求是 OpenAI 客户端很少带的形状，所以这里是网关
// 主动发起的一类转换：上游是 OpenAI 兼容 Provider 时，把 Gemini 请求改成 Chat。

// geminiToOpenAIChatRequest converts a Gemini generateContent body into an
// OpenAI Chat completion request body.
func geminiToOpenAIChatRequest(gemini map[string]any, model string) (map[string]any, error) {
	if gemini == nil {
		return nil, fmt.Errorf("empty Gemini request")
	}
	messages := make([]any, 0, 8)

	if system := geminiSystemText(gemini["systemInstruction"]); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}

	for _, rawContent := range asMapSlice(gemini["contents"]) {
		content, ok := rawContent.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(stringValue(content["role"])))
		switch role {
		case "", "user":
			role = "user"
		case "model", "assistant":
			role = "assistant"
		case "tool", "function":
			role = "tool"
		default:
			return nil, fmt.Errorf("unsupported Gemini role %q", role)
		}
		parts := asMapSlice(content["parts"])
		// Gemini 把工具结果放成 user 角色下的 functionResponse part（只有少数
		// 客户端用 role:tool）；Chat 需要单独的 role:tool 消息，一条结果一条。
		if responses := geminiFunctionResponses(parts); len(responses) > 0 {
			messages = append(messages, responses...)
			// 同一 content 里的纯文本（如果有）仍按 user 消息保留，不丢。
			if text, calls := geminiParts(geminiTextOnlyParts(parts)); strings.TrimSpace(text) != "" && len(calls) == 0 {
				messages = append(messages, map[string]any{"role": "user", "content": text})
			}
			continue
		}
		if role == "tool" {
			continue
		}
		text, calls := geminiParts(parts)
		message := map[string]any{"role": role}
		if role == "assistant" && len(calls) > 0 {
			if strings.TrimSpace(text) != "" {
				message["content"] = text
			} else {
				message["content"] = nil
			}
			message["tool_calls"] = calls
		} else {
			message["content"] = text
		}
		messages = append(messages, message)
	}

	req := map[string]any{"model": model, "messages": messages}

	if config, ok := gemini["generationConfig"].(map[string]any); ok {
		if value, ok := config["maxOutputTokens"]; ok {
			req["max_tokens"] = value
		}
		if value, ok := config["temperature"]; ok {
			req["temperature"] = value
		}
		if value, ok := config["topP"]; ok {
			req["top_p"] = value
		}
		if value, ok := config["topK"]; ok {
			req["top_k"] = value
		}
		if stops := geminiStopSequences(config["stopSequences"]); len(stops) > 0 {
			req["stop"] = stops
		}
	}

	if tools := geminiFunctionDeclarations(gemini["tools"]); len(tools) > 0 {
		req["tools"] = tools
	}
	if choice := geminiToolChoice(gemini["toolConfig"]); choice != nil {
		req["tool_choice"] = choice
	}
	return req, nil
}

// geminiSystemText reads systemInstruction: either {parts:[{text}]} or a bare
// string some clients send.
func geminiSystemText(raw any) string {
	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		text, _ := geminiParts(asMapSlice(typed["parts"]))
		return strings.TrimSpace(text)
	}
	return ""
}

// geminiParts splits one content's parts into its text and its function calls.
// Inline binary parts are described in text form: an OpenAI Chat upstream has
// no equivalent for them, and dropping them silently would lose the question.
func geminiParts(parts []any) (string, []any) {
	texts := make([]string, 0, len(parts))
	calls := make([]any, 0, len(parts))
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if text := stringValue(part["text"]); text != "" {
			texts = append(texts, text)
		}
		if call, ok := part["functionCall"].(map[string]any); ok {
			name := strings.TrimSpace(stringValue(call["name"]))
			if name == "" {
				continue
			}
			args, err := json.Marshal(call["args"])
			if err != nil {
				args = []byte("{}")
			}
			// Gemini 的 functionCall 不带 id，Chat 的 tool 消息却靠 id 配对。
			// 同名工具并行调用（读两个文件）必须各拿一个不同的 id，否则两条
			// tool 结果都挂在同一个 id 上，上游看到的是一次调用的两份答案。
			callID := "call_" + name
			if len(calls) > 0 {
				callID = fmt.Sprintf("call_%s_%d", name, len(calls)+1)
			}
			calls = append(calls, map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": string(args),
				},
			})
		}
		if inline, ok := part["inlineData"].(map[string]any); ok {
			mime := stringValue(inline["mimeType"])
			texts = append(texts, fmt.Sprintf("[inline data: %s]", mime))
		}
	}
	return strings.Join(texts, "\n"), calls
}

// geminiTextOnlyParts drops functionResponse parts so the leftover text of a
// tool-result turn is not mistaken for a new question.
func geminiTextOnlyParts(parts []any) []any {
	out := make([]any, 0, len(parts))
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, isResponse := part["functionResponse"]; isResponse {
			continue
		}
		out = append(out, part)
	}
	return out
}

// geminiFunctionResponses turns functionResponse parts into Chat tool messages.
func geminiFunctionResponses(parts []any) []any {
	out := make([]any, 0, len(parts))
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		response, ok := part["functionResponse"].(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(stringValue(response["name"]))
		if name == "" {
			continue
		}
		result, err := json.Marshal(response["response"])
		if err != nil {
			result = []byte("{}")
		}
		// 与 functionCall 的编号规则一致：同一条 content 里第 n 个同名结果
		// 配第 n 个同名调用。
		callID := "call_" + name
		if len(out) > 0 {
			callID = fmt.Sprintf("call_%s_%d", name, len(out)+1)
		}
		out = append(out, map[string]any{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      string(result),
		})
	}
	return out
}

func geminiStopSequences(raw any) []any {
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return nil
	}
	out := make([]any, 0, len(values))
	for _, value := range values {
		if text := stringValue(value); strings.TrimSpace(text) != "" {
			out = append(out, text)
		}
	}
	return out
}

// geminiFunctionDeclarations maps tools[].functionDeclarations[] onto Chat
// function tools. Gemini's search/maps built-ins have no Chat equivalent and
// are skipped rather than sent as a nameless function.
func geminiFunctionDeclarations(raw any) []any {
	out := make([]any, 0, 4)
	for _, rawTool := range asMapSlice(raw) {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		for _, rawDeclaration := range asMapSlice(tool["functionDeclarations"]) {
			declaration, ok := rawDeclaration.(map[string]any)
			if !ok {
				continue
			}
			name := strings.TrimSpace(stringValue(declaration["name"]))
			if name == "" {
				continue
			}
			function := map[string]any{"name": name}
			if desc := stringValue(declaration["description"]); desc != "" {
				function["description"] = desc
			}
			if params, ok := declaration["parameters"].(map[string]any); ok {
				function["parameters"] = normalizeFunctionInputSchema(map[string]any{"parameters": params})
			}
			out = append(out, map[string]any{"type": "function", "function": function})
		}
	}
	return out
}

// geminiToolChoice maps toolConfig.functionCallingConfig.mode onto Chat's
// tool_choice.
func geminiToolChoice(raw any) any {
	config, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	calling, ok := config["functionCallingConfig"].(map[string]any)
	if !ok {
		return nil
	}
	switch strings.ToUpper(strings.TrimSpace(stringValue(calling["mode"]))) {
	case "NONE":
		return "none"
	case "ANY":
		allowed := toAnySliceOfStrings(stringSlice(calling["allowedFunctionNames"]))
		if len(allowed) > 0 {
			return map[string]any{"type": "function", "function": map[string]any{"name": allowed[0]}}
		}
		return "required"
	case "AUTO":
		return "auto"
	}
	return nil
}

func stringSlice(raw any) []string {
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text := strings.TrimSpace(stringValue(value)); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// openAIChatToGeminiResponse converts an OpenAI Chat completion body into a
// Gemini generateContent response body.
func openAIChatToGeminiResponse(chatBody map[string]any, model string) (map[string]any, error) {
	choices := asMapSlice(chatBody["choices"])
	if len(choices) == 0 {
		return nil, fmt.Errorf("Chat response has no choices")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)

	parts := make([]any, 0, 2)
	if text := stringValue(message["content"]); text != "" {
		parts = append(parts, map[string]any{"text": text})
	}
	for _, raw := range asMapSlice(message["tool_calls"]) {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		function, _ := call["function"].(map[string]any)
		name := strings.TrimSpace(stringValue(function["name"]))
		if name == "" {
			continue
		}
		args := map[string]any{}
		if rawArgs := strings.TrimSpace(stringValue(function["arguments"])); rawArgs != "" {
			if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
				return nil, fmt.Errorf("tool call %q has arguments that are not JSON: %w", name, err)
			}
		}
		parts = append(parts, map[string]any{"functionCall": map[string]any{"name": name, "args": args}})
	}

	finish := strings.TrimSpace(stringValue(choice["finish_reason"]))
	candidate := map[string]any{"content": map[string]any{"role": "model", "parts": parts}}
	if finish != "" {
		candidate["finishReason"] = geminiFinishReason(finish)
	}

	out := map[string]any{
		"candidates": []any{candidate},
		// 版本字段固定：客户端按它判断响应形状，不随上游变化。
		"modelVersion": model,
	}
	if usage, ok := chatBody["usage"].(map[string]any); ok {
		prompt, _ := intFromAny(usage["prompt_tokens"])
		completion, _ := intFromAny(usage["completion_tokens"])
		total, _ := intFromAny(usage["total_tokens"])
		if total == 0 {
			total = prompt + completion
		}
		out["usageMetadata"] = map[string]any{
			"promptTokenCount":     prompt,
			"candidatesTokenCount": completion,
			"totalTokenCount":      total,
		}
	}
	return out, nil
}

// geminiFinishReason maps Chat's finish_reason onto Gemini's FinishReason.
func geminiFinishReason(finish string) string {
	switch finish {
	case "stop":
		return "STOP"
	case "length":
		return "MAX_TOKENS"
	case "tool_calls", "function_call":
		return "STOP"
	case "content_filter":
		return "SAFETY"
	default:
		return "OTHER"
	}
}
