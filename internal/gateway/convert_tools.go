package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// chatToolCallIDs gives every tool call that arrived with no id one, and the tool
// turns answering it the same ids in order. An upstream that leaves a streamed
// call's id out (OpenCode Zen's free DeepSeek does) leaves the agent replaying a
// call named "", which the strict ones then refuse for the rest of that
// conversation — WorkBuddy answers 400 code 11133, or 11148 "tool calls and tool
// results do not match". The id is made from the call's place in the history and
// what it says, never at random, so the next turn of the same conversation sends
// the same history byte for byte and the upstream's prompt cache still holds. A
// request whose calls and results are already named is returned unchanged.
func chatToolCallIDs(body []byte) []byte {
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil {
		return body
	}
	messages, ok := payload["messages"].([]any)
	if !ok {
		return body
	}
	changed := false
	var open []string
	for index, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch stringValue(message["role"]) {
		case "assistant":
			calls, _ := message["tool_calls"].([]any)
			if len(calls) == 0 {
				continue
			}
			open = open[:0]
			for position, raw := range calls {
				call, ok := raw.(map[string]any)
				if !ok || stringValue(call["id"]) != "" {
					continue
				}
				name, arguments := "", ""
				if function, ok := call["function"].(map[string]any); ok {
					name = stringValue(function["name"])
					if raw, exists := function["arguments"]; exists {
						if text, isText := raw.(string); isText {
							arguments = text
						} else if encoded, err := json.Marshal(raw); err == nil {
							arguments = string(encoded)
						}
					}
				}
				id := chatStableCallID(index, position, name, arguments)
				call["id"] = id
				open = append(open, id)
				changed = true
			}
		case "tool":
			if stringValue(message["tool_call_id"]) != "" || len(open) == 0 {
				continue
			}
			message["tool_call_id"] = open[0]
			open = open[1:]
			changed = true
		}
	}
	if !changed {
		return body
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(payload) != nil {
		return body
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

// chatStableCallID is the id given to the position-th call of the message-th
// turn when it came with none: the same for the same call at the same place on
// every request of the conversation.
func chatStableCallID(message, position int, name, arguments string) string {
	sum := sha256.Sum256([]byte(strconv.Itoa(message) + "." + strconv.Itoa(position) + "\x00" + name + "\x00" + arguments))
	return "call_" + hex.EncodeToString(sum[:12])
}

// anthropicToolUseIDInvalid matches characters Anthropic rejects in tool_use.id /
// tool_result.tool_use_id (must be ^[a-zA-Z0-9_-]+$). Cursor and other clients
// often emit IDs like "functions.Read:1" or "call|abc" that pass OpenAI validation
// but fail on Claude.
var anthropicToolUseIDInvalid = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// sanitizeAnthropicToolUseID rewrites a tool call id to Anthropic's allowed
// charset. The same transform is applied to tool_use.id and tool_result.tool_use_id
// so pairings stay intact.
//
// Ids that were already valid are returned untouched. Rewritten ids get a short
// hash of the original appended: "functions.Read" and "functions:Read" both
// reduce to "functions_Read", and two calls sharing one id in a single turn make
// Anthropic reject the whole request (tool_use ids must be unique). The hash is
// derived from the original id alone, so both sides of a pair still agree.
func sanitizeAnthropicToolUseID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "toolu_missing"
	}
	if !anthropicToolUseIDInvalid.MatchString(id) {
		return id
	}
	sanitized := strings.Trim(anthropicToolUseIDInvalid.ReplaceAllString(id, "_"), "_")
	if sanitized == "" {
		sanitized = "toolu_sanitized"
	}
	sum := sha256.Sum256([]byte(id))
	const maxBase = 48
	if len(sanitized) > maxBase {
		sanitized = strings.TrimRight(sanitized[:maxBase], "_")
		sanitized += "_" + hex.EncodeToString(sum[:4])
		return sanitized
	}
	return sanitized + "_" + hex.EncodeToString(sum[:4])
}

func cloneAnyMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	cloned := make(map[string]any, len(value))
	for key, item := range value {
		cloned[key] = item
	}
	return cloned
}

func asMapSlice(value any) []any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	return items
}

func parseJSONArguments(arguments string) any {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return map[string]any{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(arguments), &parsed); err != nil {
		return map[string]any{}
	}
	if parsed == nil {
		return map[string]any{}
	}
	return parsed
}

func stringifyToolArguments(input any) string {
	switch typed := input.(type) {
	case string:
		return typed
	case nil:
		return "{}"
	default:
		body, err := json.Marshal(typed)
		if err != nil {
			return "{}"
		}
		return string(body)
	}
}

func normalizeToolResultContent(content any) any {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		if len(typed) == 0 {
			return ""
		}
		// 客户端按 OpenAI 习惯把工具结果拆成 text / image_url 部件，而 Claude 的
		// tool_result 只认 text / image，原样透传会让上游 400（未知 block 类型）。
		if blocks := openAIContentToClaudeBlocks(typed); len(blocks) > 0 {
			return blocks
		}
		return ""
	default:
		text := strings.TrimSpace(fmt.Sprint(content))
		if text == "" {
			return ""
		}
		return text
	}
}

// dropCallID 从待应答列表里去掉一个已经拿到结果的调用。
func dropCallID(ids []string, id string) []string {
	for index, item := range ids {
		if item == id {
			return append(ids[:index], ids[index+1:]...)
		}
	}
	return ids
}

// appendImageNote 在工具文本后补一句「截图在下一段」，让模型知道该往下看。
// 措辞与 magpie 一致，减少同一个网关里两种说法。
func appendImageNote(text string, count int) string {
	note := fmt.Sprintf("[The tool returned %d images; they follow in the next message.]", count)
	if count == 1 {
		note = "[The tool returned an image; it follows in the next message.]"
	}
	if strings.TrimSpace(text) == "" {
		return note
	}
	return text + "\n\n" + note
}

// toolResultContentForOpenAI 拆一个 Claude tool_result 的 content：文本拼成
// OpenAI tool 消息装得下的字符串，图片块单独摘出来交给调用方安置。
//
// OpenAI Chat 的 tool 消息只能装文本，而浏览器类工具每次调用都回截图；把图丢掉
// 等于让模型点完一次就再也看不见画面，只能靠猜选择器和 uid——这就是「点元素报
// No snapshot found / querySelector 为 null」的来源。调用方把这些图跟在 tool
// 消息之后的一条 user 消息里发出去。
// toolResultFailedPrefix 标出失败的 Chat / Responses 工具结果。
const toolResultFailedPrefix = "[tool_error] "

// markFailedToolResult 给失败的工具有结果加前缀，保留原文（模型仍需要看到错误）。
func markFailedToolResult(text string) string {
	if strings.HasPrefix(text, toolResultFailedPrefix) {
		return text
	}
	return toolResultFailedPrefix + text
}

func toolResultContentForOpenAI(content any) (string, []any) {
	blocks, ok := content.([]any)
	if !ok {
		return toolResultContentToString(content), nil
	}
	texts := make([]string, 0, len(blocks))
	images := make([]any, 0, 1)
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if stringValue(block["type"]) == "image" {
			if converted := claudeImageBlockToOpenAIChat(block); converted != nil {
				images = append(images, converted)
			}
			continue
		}
		if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n"), images
}

// splitToolResultImages 把工具结果拆成「纯文本 + 图片 data URL」，给 Responses
// 两条链路用：function_call_output 只认字符串，图片必须单独作为一条 user 消息
// 跟在后面（把它塞进 output 字符串里模型看不见，等同于丢掉）。
// 同时接受 Claude 的 image 块与 Responses 的 input_image / image_url 部件。
func splitToolResultImages(content any) (string, []string) {
	blocks, ok := content.([]any)
	if !ok {
		return toolResultContentToString(content), nil
	}
	texts := make([]string, 0, len(blocks))
	urls := make([]string, 0, 1)
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch stringValue(block["type"]) {
		case "image":
			source, _ := block["source"].(map[string]any)
			data := stringValue(source["data"])
			if data == "" {
				continue
			}
			mediaType := firstNonEmpty(stringValue(source["media_type"]), "image/png")
			urls = append(urls, "data:"+mediaType+";base64,"+data)
		case "input_image", "image_url":
			if url := imageURLFromPart(block); url != "" {
				urls = append(urls, url)
			}
		default:
			if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
				texts = append(texts, text)
			}
		}
	}
	return strings.Join(texts, "\n"), urls
}

// imageURLFromPart 兼容三种写法：字符串、{url} 对象、OpenAI Chat 的
// {type:image_url, image_url:{url}}。
func imageURLFromPart(part map[string]any) string {
	switch typed := part["image_url"].(type) {
	case string:
		return typed
	case map[string]any:
		return stringValue(typed["url"])
	}
	return ""
}

// responsesInputImageParts 把 data URL 列表转成 Responses 的 input_image 部件。
func responsesInputImageParts(urls []string) []any {
	parts := make([]any, 0, len(urls))
	for _, url := range urls {
		if url == "" {
			continue
		}
		parts = append(parts, map[string]any{"type": "input_image", "image_url": url})
	}
	return parts
}

// chatImagePartsFromURLs 把 data URL 列表转成 OpenAI Chat 的 image_url 部件。
func chatImagePartsFromURLs(urls []string) []any {
	parts := make([]any, 0, len(urls))
	for _, url := range urls {
		if url == "" {
			continue
		}
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": url},
		})
	}
	return parts
}

func toolResultContentToString(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		return claudeContentToString(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(content))
	}
}

func contentBlocksFromAny(content any) []any {
	switch typed := content.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": typed}}
	case []any:
		return cloneContentBlocks(typed)
	default:
		text := strings.TrimSpace(fmt.Sprint(content))
		if text == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": text}}
	}
}

func emptyClaudeToolInputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func claudeToolInputSchema(values ...any) any {
	for _, value := range values {
		if value == nil {
			continue
		}
		return value
	}
	return emptyClaudeToolInputSchema()
}

func openAIToolToClaude(tool map[string]any) map[string]any {
	toolType := strings.TrimSpace(stringValue(tool["type"]))
	if toolType == "" {
		toolType = "function"
	}

	// Chat Completions nested function tool: {type:"function", function:{name,parameters}}
	if toolType == "function" {
		if functionValue, ok := tool["function"].(map[string]any); ok {
			claudeTool := map[string]any{"name": stringValue(functionValue["name"])}
			if description := stringValue(functionValue["description"]); description != "" {
				claudeTool["description"] = description
			}
			claudeTool["input_schema"] = claudeToolInputSchema(functionValue["parameters"], functionValue["input_schema"])
			copyToolStrictFlag(claudeTool, tool, functionValue)
			return claudeTool
		}
		// Responses API flat function tool: {type:"function", name, parameters}
		claudeTool := map[string]any{"name": stringValue(tool["name"])}
		if description := stringValue(tool["description"]); description != "" {
			claudeTool["description"] = description
		}
		claudeTool["input_schema"] = claudeToolInputSchema(tool["parameters"], tool["input_schema"])
		copyToolStrictFlag(claudeTool, tool, nil)
		return claudeTool
	}

	// Responses/Claude custom tools may be flat or nested under "custom".
	name := stringValue(tool["name"])
	description := stringValue(tool["description"])
	schemaCandidates := []any{tool["input_schema"], tool["parameters"]}
	if customValue, ok := tool["custom"].(map[string]any); ok {
		if name == "" {
			name = stringValue(customValue["name"])
		}
		if description == "" {
			description = stringValue(customValue["description"])
		}
		schemaCandidates = append(schemaCandidates, customValue["input_schema"], customValue["parameters"])
	}
	claudeTool := map[string]any{"name": name}
	if description != "" {
		claudeTool["description"] = description
	}
	claudeTool["input_schema"] = claudeToolInputSchema(schemaCandidates...)
	customValue, _ := tool["custom"].(map[string]any)
	copyToolStrictFlag(claudeTool, tool, customValue)
	return claudeTool
}

// copyToolStrictFlag carries a client's strict-mode choice into a Claude tool.
// Anthropic has no strict toggle: the equivalent is closing the schema (no extra
// properties, every property required), which is exactly what strict asked for.
// Dropping the flag silently downgraded the guarantee the client relied on.
func copyToolStrictFlag(out map[string]any, carriers ...map[string]any) {
	for _, carrier := range carriers {
		if carrier == nil {
			continue
		}
		strict, ok := carrier["strict"].(bool)
		if !ok || !strict {
			continue
		}
		if schema, ok := out["input_schema"].(map[string]any); ok {
			closeObjectSchema(schema)
		}
		return
	}
}

// closeObjectSchema sets additionalProperties=false and requires every declared
// property, the Anthropic-side meaning of OpenAI strict mode.
func closeObjectSchema(schema map[string]any) {
	if schema == nil || stringValue(schema["type"]) != "object" {
		return
	}
	schema["additionalProperties"] = false
	properties, _ := schema["properties"].(map[string]any)
	if len(properties) == 0 {
		return
	}
	required := make([]any, 0, len(properties))
	for name := range properties {
		required = append(required, name)
	}
	sort.Slice(required, func(i, j int) bool { return required[i].(string) < required[j].(string) })
	schema["required"] = required
}

func claudeToolToOpenAI(tool map[string]any) map[string]any {
	functionValue := map[string]any{"name": stringValue(tool["name"])}
	if description := stringValue(tool["description"]); description != "" {
		functionValue["description"] = description
	}
	if schema := tool["input_schema"]; schema != nil {
		functionValue["parameters"] = schema
	} else if parameters := tool["parameters"]; parameters != nil {
		functionValue["parameters"] = parameters
	} else {
		functionValue["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	out := map[string]any{"type": "function", "function": functionValue}
	// A schema Anthropic closed (strict mode on the way in) is strict mode on
	// the way out, so the flag survives a Chat round trip.
	if schema, ok := functionValue["parameters"].(map[string]any); ok && schemaIsClosed(schema) {
		functionValue["strict"] = true
		out["strict"] = true
	}
	return out
}

// schemaIsClosed reports whether a schema forbids extra properties and requires
// every one it declares.
func schemaIsClosed(schema map[string]any) bool {
	if additional, ok := schema["additionalProperties"].(bool); !ok || additional {
		return false
	}
	properties, _ := schema["properties"].(map[string]any)
	if len(properties) == 0 {
		return true
	}
	required := asStringSlice(schema["required"])
	if len(required) != len(properties) {
		return false
	}
	have := make(map[string]struct{}, len(required))
	for _, name := range required {
		have[name] = struct{}{}
	}
	for name := range properties {
		if _, ok := have[name]; !ok {
			return false
		}
	}
	return true
}

func asStringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]string); ok {
			return typed
		}
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text := stringValue(item); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// responsesToolToOpenAIChat normalizes a Responses API tool into Chat Completions
// form ({type:"function", function:{name,parameters}}). Built-in Responses tools
// without a client-callable name (web_search, file_search, ...) are dropped —
// Chat providers like BigModel/Z.AI only accept type=function.
func responsesToolToOpenAIChat(tool map[string]any) map[string]any {
	if tool == nil {
		return nil
	}
	// Already Chat Completions nested form.
	if functionValue, ok := tool["function"].(map[string]any); ok {
		name := stringValue(functionValue["name"])
		if name == "" {
			return nil
		}
		fn := map[string]any{"name": name}
		if description := stringValue(functionValue["description"]); description != "" {
			fn["description"] = description
		}
		fn["parameters"] = claudeToolInputSchema(functionValue["parameters"], functionValue["input_schema"])
		out := map[string]any{"type": "function", "function": fn}
		copyStrictToChatTool(out, fn, tool, functionValue)
		return out
	}

	toolType := strings.TrimSpace(strings.ToLower(stringValue(tool["type"])))
	switch toolType {
	case "", "function", "custom":
		name := stringValue(tool["name"])
		description := stringValue(tool["description"])
		schemaCandidates := []any{tool["parameters"], tool["input_schema"]}
		if customValue, ok := tool["custom"].(map[string]any); ok {
			if name == "" {
				name = stringValue(customValue["name"])
			}
			if description == "" {
				description = stringValue(customValue["description"])
			}
			schemaCandidates = append(schemaCandidates, customValue["parameters"], customValue["input_schema"])
		}
		if name == "" {
			return nil
		}
		fn := map[string]any{"name": name}
		if description != "" {
			fn["description"] = description
		}
		fn["parameters"] = claudeToolInputSchema(schemaCandidates...)
		out := map[string]any{"type": "function", "function": fn}
		customValue, _ := tool["custom"].(map[string]any)
		copyStrictToChatTool(out, fn, tool, customValue)
		return out
	default:
		return nil
	}
}

// copyStrictToChatTool keeps strict mode when crossing into Chat form, where the
// flag lives at both the tool and the nested function level depending on the
// upstream. Without it a strict client silently lost its schema guarantee.
func copyStrictToChatTool(out, fn map[string]any, carriers ...map[string]any) {
	for _, carrier := range carriers {
		if carrier == nil {
			continue
		}
		if strict, ok := carrier["strict"].(bool); ok {
			fn["strict"] = strict
			out["strict"] = strict
			return
		}
	}
}

func responsesToolsToOpenAIChat(tools []any) []any {
	if len(tools) == 0 {
		return nil
	}
	// Reuse Codex flatten so namespace / tool_search / custom survive the Chat hop.
	carrier := map[string]any{"tools": tools}
	ctx := buildCodexToolContextFromRequest(carrier)
	if claudeTools := ctx.ClaudeTools(); len(claudeTools) > 0 {
		out := make([]any, 0, len(claudeTools))
		for _, item := range claudeTools {
			tool, ok := item.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, claudeToolToOpenAI(tool))
		}
		if len(out) > 0 {
			return out
		}
	}
	out := make([]any, 0, len(tools))
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if converted := responsesToolToOpenAIChat(tool); converted != nil {
			out = append(out, converted)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// responsesToolChoiceToOpenAIChat maps Responses tool_choice into Chat Completions.
func responsesToolChoiceToOpenAIChat(choice any) any {
	switch typed := choice.(type) {
	case string:
		return typed
	case map[string]any:
		choiceType := strings.TrimSpace(strings.ToLower(stringValue(typed["type"])))
		switch choiceType {
		case "auto", "none", "required":
			return choiceType
		case "namespace":
			// Codex namespace tool_choice has no Chat equivalent; degrade to auto.
			return "auto"
		case "tool_search":
			return map[string]any{
				"type":     "function",
				"function": map[string]any{"name": codexToolSearchProxyName},
			}
		case "function":
			if functionValue, ok := typed["function"].(map[string]any); ok {
				if name := stringValue(functionValue["name"]); name != "" {
					return map[string]any{
						"type":     "function",
						"function": map[string]any{"name": name},
					}
				}
			}
			if name := stringValue(typed["name"]); name != "" {
				return map[string]any{
					"type":     "function",
					"function": map[string]any{"name": name},
				}
			}
		case "custom", "tool":
			if name := stringValue(typed["name"]); name != "" {
				return map[string]any{
					"type":     "function",
					"function": map[string]any{"name": name},
				}
			}
		}
		return cloneAnyMap(typed)
	default:
		return nil
	}
}

func copyResponsesToolsToChat(source map[string]any, target map[string]any) {
	if rawTools, exists := source["tools"]; exists {
		if converted := responsesToolsToOpenAIChat(asMapSlice(rawTools)); len(converted) > 0 {
			target["tools"] = converted
		}
	}
	if rawChoice, exists := source["tool_choice"]; exists {
		if converted := responsesToolChoiceToOpenAIChat(rawChoice); converted != nil {
			target["tool_choice"] = converted
		}
	}
}

func openAIToolsToClaude(tools []any) []any {
	if len(tools) == 0 {
		return nil
	}
	claudeTools := make([]any, 0, len(tools))
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if converted := openAIToolToClaude(tool); stringValue(converted["name"]) != "" {
			claudeTools = append(claudeTools, converted)
		}
	}
	return claudeTools
}

func claudeToolsToOpenAI(tools []any) []any {
	if len(tools) == 0 {
		return nil
	}
	openAITools := make([]any, 0, len(tools))
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if converted := claudeToolToOpenAI(tool); stringValue(converted["function"].(map[string]any)["name"]) != "" {
			openAITools = append(openAITools, converted)
		}
	}
	return openAITools
}

func openAIToolChoiceToClaude(choice any) any {
	switch typed := choice.(type) {
	case string:
		switch strings.TrimSpace(strings.ToLower(typed)) {
		case "", "auto":
			return map[string]any{"type": "auto"}
		case "none":
			return map[string]any{"type": "none"}
		case "required":
			return map[string]any{"type": "any"}
		default:
			return map[string]any{"type": "auto"}
		}
	case map[string]any:
		choiceType := strings.TrimSpace(strings.ToLower(stringValue(typed["type"])))
		if choiceType == "function" {
			if functionValue, ok := typed["function"].(map[string]any); ok {
				if name := stringValue(functionValue["name"]); name != "" {
					return map[string]any{"type": "tool", "name": name}
				}
			}
		}
		if name := stringValue(typed["name"]); name != "" && choiceType == "tool" {
			return map[string]any{"type": "tool", "name": name}
		}
		return cloneAnyMap(typed)
	default:
		return nil
	}
}

func claudeToolChoiceToOpenAI(choice any) any {
	switch typed := choice.(type) {
	case string:
		switch strings.TrimSpace(strings.ToLower(typed)) {
		case "", "auto":
			return "auto"
		case "none":
			return "none"
		case "any":
			return "required"
		default:
			return "auto"
		}
	case map[string]any:
		choiceType := strings.TrimSpace(strings.ToLower(stringValue(typed["type"])))
		switch choiceType {
		case "auto":
			return "auto"
		case "none":
			return "none"
		case "any":
			return "required"
		case "tool":
			if name := stringValue(typed["name"]); name != "" {
				return map[string]any{
					"type":     "function",
					"function": map[string]any{"name": name},
				}
			}
		}
		return cloneAnyMap(typed)
	default:
		return nil
	}
}

func copyToolsField(source map[string]any, target map[string]any, toClaude bool) {
	rawTools := source["tools"]
	tools := asMapSlice(rawTools)
	if len(tools) == 0 {
		// 没有可调用的工具时 tool_choice 没有意义；部分上游（Anthropic 端点）
		// 见到它而没有 tools 会直接 400。
		return
	}
	if toClaude {
		if converted := openAIToolsToClaude(tools); len(converted) > 0 {
			target["tools"] = converted
		}
		return
	}
	if converted := claudeToolsToOpenAI(tools); len(converted) > 0 {
		target["tools"] = converted
	}
}

func copyToolChoiceField(source map[string]any, target map[string]any, toClaude bool) {
	rawChoice, exists := source["tool_choice"]
	if !exists {
		return
	}
	if toClaude {
		if _, has := target["tools"]; !has {
			// Anthropic 端点在没有 tools 时会拒绝 tool_choice（400），而 Chat
			// 允许两者并存的消息里只有 tool_choice。没有可调用的工具，这个
			// 选择就没有意义，丢掉。
			return
		}
		// Chat 的 parallel_tool_calls=false 落到 Claude 的
		// tool_choice.disable_parallel_tool_use；Responses 的 allowed_tools
		// 白名单收窄成 Claude 的 allowed_tools。
		parallel, hasParallel := chatParallelToolCalls(source)
		if converted := openAIToolChoiceToClaude(rawChoice); converted != nil || hasParallel {
			choice, ok := converted.(map[string]any)
			if !ok {
				choice = map[string]any{"type": "auto"}
				if text, isText := converted.(string); isText && text != "" {
					choice = map[string]any{"type": text}
				}
			}
			// type=none 是"不许调工具"，再挂 disable_parallel_tool_use 有些上游
			// 会直接 400，这种选择不需要它。改出来的 map 是新的一份，别动原值。
			choiceType := strings.TrimSpace(strings.ToLower(stringValue(choice["type"])))
			if hasParallel && !parallel && choiceType != "none" {
				restricted := map[string]any{"type": choice["type"]}
				if name := stringValue(choice["name"]); name != "" {
					restricted["name"] = name
				}
				restricted["disable_parallel_tool_use"] = true
				choice = restricted
			}
			target["tool_choice"] = choice
		}
		return
	}
	if converted := claudeToolChoiceToOpenAI(rawChoice); converted != nil {
		target["tool_choice"] = converted
	}
	applyClaudeToolChoiceRestrictions(rawChoice, target)
}

// applyClaudeToolChoiceRestrictions 把 Claude tool_choice 上的附加约束搬回 Chat：
// disable_parallel_tool_use → parallel_tool_calls=false，allowed_tools →
// allow_tools（OpenRouter 的写法）。
func applyClaudeToolChoiceRestrictions(choice any, target map[string]any) {
	typed, ok := choice.(map[string]any)
	if !ok {
		return
	}
	restricted := false
	if disabled, ok := typed["disable_parallel_tool_use"].(bool); ok && disabled {
		target["parallel_tool_calls"] = false
		restricted = true
	}
	if names := claudeAllowedToolNames(typed["allowed_tools"]); len(names) > 0 {
		allow := make(map[string]any, len(names))
		for _, name := range names {
			allow[name] = map[string]any{}
		}
		target["allow_tools"] = allow
		restricted = true
	}
	// 约束挂在 tool_choice 上，只带了约束的 choice 也得让 Chat 侧看到用什么模式。
	if restricted {
		if _, hasChoice := target["tool_choice"]; !hasChoice {
			target["tool_choice"] = "auto"
		}
	}
}

// chatParallelToolCalls 读 Chat 的 parallel_tool_calls。
func chatParallelToolCalls(source map[string]any) (enabled bool, present bool) {
	if source == nil {
		return true, false
	}
	disabled, exists := source["disable_parallel_tool_use"]
	if exists {
		if value, ok := disabled.(bool); ok {
			return !value, true
		}
	}
	raw, exists := source["parallel_tool_calls"]
	if !exists || raw == nil {
		return true, false
	}
	if value, ok := raw.(bool); ok {
		return value, true
	}
	return true, false
}

// claudeAllowedToolNames 把白名单的几种写法拆成工具名列表。
func claudeAllowedToolNames(raw any) []string {
	var names []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" {
			names = append(names, name)
		}
	}
	switch typed := raw.(type) {
	case map[string]any:
		if tools, ok := typed["tools"].(map[string]any); ok {
			for name := range tools {
				add(name)
			}
			sort.Strings(names)
			return names
		}
		if name, ok := typed["name"].(string); ok {
			add(name)
			return names
		}
		for name := range typed {
			add(name)
		}
		sort.Strings(names)
	case []any:
		for _, item := range typed {
			switch value := item.(type) {
			case string:
				add(value)
			case map[string]any:
				add(stringValue(value["name"]))
			}
		}
	}
	return names
}

func copyToolsFieldDirect(source map[string]any, target map[string]any) {
	if rawTools, exists := source["tools"]; exists {
		if tools := asMapSlice(rawTools); len(tools) > 0 {
			target["tools"] = tools
		}
	}
	if rawChoice, exists := source["tool_choice"]; exists {
		target["tool_choice"] = rawChoice
	}
}

// openAIChatToolToResponses flattens a Chat Completions tool into Responses form
// ({type:"function", name, parameters}). Codex / ChatGPT Responses reject nested
// `function.name` with: Missing required parameter: 'tools[0].name'.
func openAIChatToolToResponses(tool map[string]any) map[string]any {
	if tool == nil {
		return nil
	}
	if functionValue, ok := tool["function"].(map[string]any); ok {
		name := stringValue(functionValue["name"])
		if name == "" {
			return nil
		}
		out := map[string]any{"type": "function", "name": name}
		if description := stringValue(functionValue["description"]); description != "" {
			out["description"] = description
		}
		out["parameters"] = claudeToolInputSchema(functionValue["parameters"], functionValue["input_schema"])
		if strict, exists := functionValue["strict"]; exists {
			out["strict"] = strict
		} else if strict, exists := tool["strict"]; exists {
			out["strict"] = strict
		}
		return out
	}

	toolType := strings.TrimSpace(strings.ToLower(stringValue(tool["type"])))
	switch toolType {
	case "", "function", "custom":
		name := stringValue(tool["name"])
		if name == "" {
			if customValue, ok := tool["custom"].(map[string]any); ok {
				name = stringValue(customValue["name"])
			}
		}
		if name == "" {
			return nil
		}
		out := map[string]any{"type": "function", "name": name}
		if description := stringValue(tool["description"]); description != "" {
			out["description"] = description
		} else if customValue, ok := tool["custom"].(map[string]any); ok {
			if description := stringValue(customValue["description"]); description != "" {
				out["description"] = description
			}
		}
		schemaCandidates := []any{tool["parameters"], tool["input_schema"]}
		if customValue, ok := tool["custom"].(map[string]any); ok {
			schemaCandidates = append(schemaCandidates, customValue["parameters"], customValue["input_schema"])
		}
		out["parameters"] = claudeToolInputSchema(schemaCandidates...)
		if strict, exists := tool["strict"]; exists {
			out["strict"] = strict
		}
		return out
	default:
		// Built-ins (web_search, file_search, ...) — pass through when already flat.
		if cloned := cloneAnyMap(tool); cloned != nil {
			return cloned
		}
		return nil
	}
}

// openAIChatToolChoiceToResponses maps Chat Completions tool_choice into Responses.
func openAIChatToolChoiceToResponses(choice any) any {
	// Responses 的 tool_choice 没有 disable_parallel_tool_use / allowed_tools 的
	// 位置；parallel_tool_calls 由 applyResponsesParallelToolCallsToChat 单独搬。
	if typed, ok := choice.(map[string]any); ok {
		choice = map[string]any{"type": typed["type"]}
		if functionValue, ok := typed["function"].(map[string]any); ok {
			choice.(map[string]any)["function"] = functionValue
		}
		if name, ok := typed["name"]; ok {
			choice.(map[string]any)["name"] = name
		}
	}
	switch typed := choice.(type) {
	case string:
		return typed
	case map[string]any:
		choiceType := strings.TrimSpace(strings.ToLower(stringValue(typed["type"])))
		switch choiceType {
		case "auto", "none", "required":
			return choiceType
		case "function":
			name := ""
			if functionValue, ok := typed["function"].(map[string]any); ok {
				name = stringValue(functionValue["name"])
			}
			if name == "" {
				name = stringValue(typed["name"])
			}
			if name == "" {
				return nil
			}
			return map[string]any{"type": "function", "name": name}
		default:
			if name := stringValue(typed["name"]); name != "" {
				return map[string]any{"type": "function", "name": name}
			}
			return cloneAnyMap(typed)
		}
	default:
		return nil
	}
}

func openAIChatToolsToResponses(tools []any) []any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]any, 0, len(tools))
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if converted := openAIChatToolToResponses(tool); converted != nil {
			out = append(out, converted)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// copyChatToolsToResponses writes Chat Completions tools/tool_choice onto a
// Responses request using the flat Responses tool shape.
func copyChatToolsToResponses(source map[string]any, target map[string]any) {
	if rawTools, exists := source["tools"]; exists {
		if converted := openAIChatToolsToResponses(asMapSlice(rawTools)); len(converted) > 0 {
			target["tools"] = converted
		}
	}
	if rawChoice, exists := source["tool_choice"]; exists {
		if converted := openAIChatToolChoiceToResponses(rawChoice); converted != nil {
			target["tool_choice"] = converted
		}
	}
}

// copyClaudeSamplingTo forwards the sampling fields a Claude request carries
// onto another protocol's request body. top_k is Anthropic-only, so callers
// that have no slot for it simply omit the key rather than misplace a number.
func copyClaudeSamplingTo(claudeReq, target map[string]any, keys ...string) {
	if claudeReq == nil || target == nil {
		return
	}
	for _, key := range keys {
		if value, exists := claudeReq[key]; exists {
			target[key] = value
		}
	}
}

// stopSequencesFromAny normalises a stop field however it was spelled: a single
// string (Chat / OpenAI SDKs) or a list. Returns nil for an empty or malformed
// value so callers can leave the field out.
func stopSequencesFromAny(raw any) []string {
	var out []string
	switch typed := raw.(type) {
	case string:
		if typed != "" {
			out = append(out, typed)
		}
	case []string:
		for _, item := range typed {
			if item != "" {
				out = append(out, item)
			}
		}
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok && text != "" {
				out = append(out, text)
			}
		}
	}
	return out
}

// chatStopToClaudeSequences maps a Chat `stop` onto Claude `stop_sequences`.
func chatStopToClaudeSequences(source map[string]any) []string {
	if source == nil {
		return nil
	}
	if raw, exists := source["stop"]; exists && raw != nil {
		return stopSequencesFromAny(raw)
	}
	// Already-spelled Claude requests (Claude → Claude via a converted hop).
	return stopSequencesFromAny(source["stop_sequences"])
}

// claudeSequencesToChatStop maps Claude `stop_sequences` onto the Chat `stop`
// field. Chat accepts a string or an array, but strict upstreams (GLM's
// ChatCompletionRequest among them) deserialize the field as an array only and
// answer a string with a 400 — so a decoded request always gets an array, even
// for a single sequence.
func claudeSequencesToChatStop(source map[string]any) []any {
	if source == nil {
		return nil
	}
	sequences := stopSequencesFromAny(source["stop_sequences"])
	if len(sequences) == 0 {
		return nil
	}
	return toAnySliceOfStrings(sequences)
}

func openAIToolCallsToClaudeBlocks(toolCalls []any) []any {
	blocks := make([]any, 0, len(toolCalls))
	for _, item := range toolCalls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		functionValue, _ := call["function"].(map[string]any)
		name := stringValue(functionValue["name"])
		if name == "" {
			continue
		}
		block := map[string]any{
			"type":  "tool_use",
			"id":    sanitizeAnthropicToolUseID(stringValue(call["id"])),
			"name":  name,
			"input": parseJSONArguments(stringValue(functionValue["arguments"])),
		}
		blocks = append(blocks, block)
	}
	return blocks
}

func extractOpenAIToolNames(openAIReq map[string]any) map[string]struct{} {
	if openAIReq == nil {
		return nil
	}
	rawTools, ok := openAIReq["tools"].([]any)
	if !ok || len(rawTools) == 0 {
		return nil
	}
	names := make(map[string]struct{}, len(rawTools))
	for _, item := range rawTools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if functionValue, ok := tool["function"].(map[string]any); ok {
			if name := stringValue(functionValue["name"]); name != "" {
				names[name] = struct{}{}
			}
		}
		if name := stringValue(tool["name"]); name != "" {
			names[name] = struct{}{}
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// chatDeclaredToolNames is the set of tool names a Chat request offers in its
// tools[], however it spells them (nested function tools and the flat
// Responses-shaped ones alike). Like claudeDeclaredToolNames it is never nil,
// so "no tools" and "no tool by this name" stay apart.
func chatDeclaredToolNames(payload map[string]any) map[string]struct{} {
	names := make(map[string]struct{})
	for _, raw := range asMapSlice(payload["tools"]) {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if function, ok := tool["function"].(map[string]any); ok {
			if name := strings.TrimSpace(stringValue(function["name"])); name != "" {
				names[name] = struct{}{}
			}
		}
		if name := strings.TrimSpace(stringValue(tool["name"])); name != "" {
			names[name] = struct{}{}
		}
	}
	return names
}

// resolveOpenAIToolNameFromClaude maps Claude tool_use names back to the client
// tool names from the original OpenAI request. Cursor sends TitleCase names like
// "Read"; blindly reverse-remapping OAuth names would turn them into "read" and
// break client-side tool dispatch even when the upstream call succeeded.
// A model that changes the case of a declared name is answered by
// restoreDeclaredToolName (D32-2): the reply paths compose the two, so a tool
// declared as "Bash" and called as "bash" reaches the client as declared.
func resolveOpenAIToolNameFromClaude(claudeName string, clientToolNames map[string]struct{}) string {
	claudeName = strings.TrimSpace(claudeName)
	if claudeName == "" {
		return claudeName
	}
	if clientToolNames != nil {
		if _, ok := clientToolNames[claudeName]; ok {
			return claudeName
		}
		lower := strings.ToLower(claudeName)
		if _, ok := clientToolNames[lower]; ok {
			return lower
		}
		if original, ok := oauthToolRenameReverseMap[claudeName]; ok {
			if _, ok := clientToolNames[original]; ok {
				return original
			}
		}
		// Claude OAuth cloaking renames client tools to TitleCase
		// (e.g. exec_command → ExecCommand) via a lossy transform that the
		// static reverse map can't invert. Re-apply the *forward* cloaking
		// transform to every client tool name and match, so we can restore
		// the exact name the client (Codex) registered. Without this the
		// client receives an unknown tool name and rejects it as
		// "unsupported call".
		for name := range clientToolNames {
			if cloaked, _ := remapClaudeOAuthToolName(name); cloaked == claudeName {
				return name
			}
		}
		// A known tool list with no match in it means the name is simply not
		// one of this request's tools — nothing about it was renamed on the
		// way in, so it is forwarded as the client spelled it. Falling back to
		// the OAuth cloak's reverse map here would turn a client's own "Bash"
		// into "bash" and the model would copy that spelling back (the client
		// then answers "No such tool available: bash, call Bash instead").
		return claudeName
	}
	return reverseRemapClaudeOAuthToolName(claudeName)
}

// claudeDeclaredToolNames is the set of tool names an Anthropic-format request
// declares in its own tools[]. It is what a history's tool_use names are
// matched against: a name in it is the client's own spelling and reaches the
// model untouched. The set is never nil, so resolveOpenAIToolNameFromClaude
// knows "no tool list" and "a tool list that does not contain this name" apart.
func claudeDeclaredToolNames(rawTools any) map[string]struct{} {
	names := make(map[string]struct{})
	for _, item := range asMapSlice(rawTools) {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if name := strings.TrimSpace(stringValue(tool["name"])); name != "" {
			names[name] = struct{}{}
		}
	}
	return names
}

// restoreDeclaredToolName is name as the client declared it, when the model
// answered with a different case of it. Some models lowercase every tool they
// call; the client only knows the spelling it registered, so the case the
// model dropped is put back. A name the declared list does not hold (or holds
// in two spellings that differ only by case) is left alone.
func restoreDeclaredToolName(name string, declared map[string]struct{}) string {
	name = strings.TrimSpace(name)
	if name == "" || len(declared) == 0 {
		return name
	}
	if _, ok := declared[name]; ok {
		return name
	}
	lower := strings.ToLower(name)
	match := ""
	for candidate := range declared {
		if strings.ToLower(candidate) != lower {
			continue
		}
		if match != "" && match != candidate {
			return name // ambiguous: two declared names differ only by case
		}
		match = candidate
	}
	if match != "" {
		return match
	}
	return name
}

func claudeToolUseBlocksToOpenAIToolCalls(blocks []any, clientToolNames map[string]struct{}) []any {
	toolCalls := make([]any, 0, len(blocks))
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok || stringValue(block["type"]) != "tool_use" {
			continue
		}
		// Some clients emit a tool_use block with an empty name (usually an
		// artifact of context compaction). Qoder and other strict upstreams
		// reject "tool_use.name: String should have at least 1 character", so
		// such blocks are dropped instead of forwarded (they cannot be
		// dispatched anyway).
		if strings.TrimSpace(stringValue(block["name"])) == "" {
			continue
		}
		toolCalls = append(toolCalls, map[string]any{
			"id":   sanitizeAnthropicToolUseID(stringValue(block["id"])),
			"type": "function",
			"function": map[string]any{
				"name":      restoreDeclaredToolName(resolveOpenAIToolNameFromClaude(stringValue(block["name"]), clientToolNames), clientToolNames),
				"arguments": stringifyToolArguments(block["input"]),
			},
		})
	}
	return toolCalls
}

// restoreClaudeToolUseNames puts the declared spelling back into a Claude
// content list's tool_use blocks.
func restoreClaudeToolUseNames(blocks []any, declared map[string]struct{}) {
	if len(declared) == 0 {
		return
	}
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok || stringValue(block["type"]) != "tool_use" {
			continue
		}
		if name := stringValue(block["name"]); name != "" {
			block["name"] = restoreDeclaredToolName(name, declared)
		}
	}
}

func claudeContentBlocksToOpenAIAssistantMessage(blocks []any, clientToolNames map[string]struct{}) map[string]any {
	message := map[string]any{"role": "assistant"}
	textParts := make([]string, 0, len(blocks))
	toolBlocks := make([]any, 0, len(blocks))
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch stringValue(block["type"]) {
		case "text":
			if text := stringValue(block["text"]); text != "" {
				textParts = append(textParts, text)
			}
		case "tool_use":
			toolBlocks = append(toolBlocks, block)
		}
	}
	if len(toolBlocks) > 0 {
		message["tool_calls"] = claudeToolUseBlocksToOpenAIToolCalls(toolBlocks, clientToolNames)
		if len(textParts) > 0 {
			message["content"] = strings.Join(textParts, "\n")
		} else {
			message["content"] = nil
		}
		return message
	}
	if len(textParts) == 1 {
		message["content"] = textParts[0]
	} else if len(textParts) > 1 {
		message["content"] = strings.Join(textParts, "\n")
	} else {
		message["content"] = ""
	}
	return message
}

func openAIAssistantMessageToClaudeContent(message map[string]any) any {
	blocks := make([]any, 0, 4)
	if content := message["content"]; content != nil && !isEmptyOpenAIContent(content) {
		blocks = append(blocks, openAIContentToClaudeBlocks(content)...)
	}
	if rawToolCalls, ok := message["tool_calls"].([]any); ok && len(rawToolCalls) > 0 {
		blocks = append(blocks, openAIToolCallsToClaudeBlocks(rawToolCalls)...)
	}
	if len(blocks) == 0 {
		return ""
	}
	if len(blocks) == 1 {
		if block, ok := blocks[0].(map[string]any); ok && stringValue(block["type"]) == "text" && len(block) <= 2 {
			return stringValue(block["text"])
		}
	}
	return blocks
}

func openAIMessagesToClaude(rawMessages []any) ([]any, []map[string]any, error) {
	systemBlocks := make([]any, 0, 4)
	claudeMessages := make([]map[string]any, 0, len(rawMessages))
	pendingToolResults := make([]any, 0, 2)

	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		claudeMessages = append(claudeMessages, map[string]any{
			"role":    "user",
			"content": pendingToolResults,
		})
		pendingToolResults = nil
	}

	for _, item := range rawMessages {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := strings.TrimSpace(stringValue(entry["role"]))
		if role == "" {
			continue
		}
		switch role {
		case "system", "developer":
			systemBlocks = append(systemBlocks, openAIContentToClaudeBlocks(entry["content"])...)
			continue
		case "tool":
			toolCallID := stringValue(entry["tool_call_id"])
			if toolCallID == "" {
				continue
			}
			pendingToolResults = append(pendingToolResults, map[string]any{
				"type":        "tool_result",
				"tool_use_id": sanitizeAnthropicToolUseID(toolCallID),
				"content":     normalizeToolResultContent(entry["content"]),
			})
			continue
		}
		flushToolResults()

		switch role {
		case "assistant":
			content := openAIAssistantMessageToClaudeContent(entry)
			if isEmptyClaudeContent(content) {
				continue
			}
			claudeMessages = append(claudeMessages, map[string]any{"role": "assistant", "content": content})
		case "user":
			content := normalizeOpenAIContentForClaude(entry["content"])
			if isEmptyClaudeContent(content) {
				continue
			}
			claudeMessages = append(claudeMessages, map[string]any{"role": "user", "content": content})
		}
	}
	flushToolResults()

	if len(claudeMessages) == 0 {
		return systemBlocks, nil, fmt.Errorf("openai request has no usable messages")
	}
	return systemBlocks, claudeMessages, nil
}

func claudeMessagesToOpenAI(rawMessages []any, clientToolNames map[string]struct{}) ([]map[string]any, error) {
	messages := make([]map[string]any, 0, len(rawMessages))
	// Chat 的 tool 消息只能装文本，工具结果里的截图得挪到紧随其后的一条 user
	// 消息里；每张图前标出它来自哪次调用，并行调工具时才分得清哪张是谁的。
	var pendingImages []any
	toolNames := make(map[string]string, 4)
	// 每个带 tool_calls 的 assistant 都要有问必答：被中断的回合（客户端断流、
	// 取消）会留下没有结果的调用，严格的上游直接 400（WorkBuddy 11148
	// "tool calls and tool results do not match"）。
	var pendingCalls []string
	fillPendingCalls := func() {
		for _, id := range pendingCalls {
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": sanitizeAnthropicToolUseID(id),
				"content":      "[The result of this tool call is unavailable: the turn was interrupted.]",
			})
		}
		pendingCalls = nil
	}
	flushImages := func() {
		if len(pendingImages) == 0 {
			return
		}
		messages = append(messages, map[string]any{"role": "user", "content": pendingImages})
		pendingImages = nil
	}
	// mergeImages 把攒下的截图并到这条 user 消息最前面，而不是另起一条：
	// 连着两条 user 消息有些模型的 chat template 会直接判不合法。
	mergeImages := func(content any) any {
		if len(pendingImages) == 0 {
			return content
		}
		parts := make([]any, 0, len(pendingImages)+1)
		parts = append(parts, pendingImages...)
		switch typed := content.(type) {
		case string:
			parts = append(parts, map[string]any{"type": "text", "text": typed})
		case []any:
			parts = append(parts, typed...)
		}
		pendingImages = nil
		return parts
	}

	for _, item := range rawMessages {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := strings.TrimSpace(stringValue(entry["role"]))
		if role == "" {
			continue
		}
		switch role {
		case "assistant":
			// 上一轮的调用若没人应答，先补上；截图只能落在 assistant 之前
			fillPendingCalls()
			flushImages()
			blocks := contentBlocksFromAny(entry["content"])
			if len(blocks) == 0 {
				continue
			}
			hasToolUse := false
			for _, blockItem := range blocks {
				block, ok := blockItem.(map[string]any)
				if !ok || stringValue(block["type"]) != "tool_use" {
					continue
				}
				name := strings.TrimSpace(stringValue(block["name"]))
				// 名字为空的 tool_use 会被整条丢掉（下游无法序列化），这类调用
				// 也就不该进待应答队列，否则会补出一条没有对应 tool_calls 的
				// 孤立 tool 消息，反而更不合规。
				if name == "" {
					continue
				}
				hasToolUse = true
				if id := strings.TrimSpace(stringValue(block["id"])); id != "" {
					toolNames[id] = name
					pendingCalls = append(pendingCalls, id)
				}
			}
			if hasToolUse {
				messages = append(messages, claudeContentBlocksToOpenAIAssistantMessage(blocks, clientToolNames))
				continue
			}
			content := normalizeClaudeContentForOpenAI(entry["content"])
			if isEmptyOpenAIContent(content) {
				continue
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": content})
		case "user":
			blocks := contentBlocksFromAny(entry["content"])
			if len(blocks) == 0 {
				continue
			}
			textBlocks := make([]any, 0, len(blocks))
			toolResults := make([]map[string]any, 0, len(blocks))
			for _, blockItem := range blocks {
				block, ok := blockItem.(map[string]any)
				if !ok {
					continue
				}
				switch stringValue(block["type"]) {
				case "tool_result":
					toolResults = append(toolResults, block)
				default:
					textBlocks = append(textBlocks, block)
				}
			}
			// 一个用户回合里工具结果在前、用户自己的话在后：严格的 OpenAI 上游要求
			// tool 消息紧邻带 tool_calls 的 assistant，中间插一条 user 会被判成
			// tool_call_id 找不到（Kimi、WorkBuddy 都会 400）。
			for _, result := range toolResults {
				toolUseID := stringValue(result["tool_use_id"])
				if toolUseID == "" {
					continue
				}
				pendingCalls = dropCallID(pendingCalls, toolUseID)
				text, images := toolResultContentForOpenAI(result["content"])
				// Chat 没有标记失败结果的位置：把失败写进文本，不要静默变成
				// 一次成功（客户端与模型都会误以为工具跑通了）。
				if failed, ok := result["is_error"].(bool); ok && failed {
					text = markFailedToolResult(text)
				}
				if len(images) > 0 {
					text = appendImageNote(text, len(images))
					source := toolUseID
					if name := toolNames[toolUseID]; name != "" {
						source = name + " (tool call " + toolUseID + ")"
					}
					pendingImages = append(pendingImages, map[string]any{
						"type": "text",
						"text": "[From the result of " + source + ":]",
					})
					pendingImages = append(pendingImages, images...)
				}
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": sanitizeAnthropicToolUseID(toolUseID),
					"content":      text,
				})
			}
			// 这一回合没应答完的调用，补在用户自己的话之前（tool 消息必须连着）
			fillPendingCalls()
			if len(textBlocks) > 0 {
				content := normalizeClaudeContentForOpenAI(textBlocks)
				if !isEmptyOpenAIContent(content) {
					messages = append(messages, map[string]any{"role": "user", "content": mergeImages(content)})
				}
			}
		}
	}
	fillPendingCalls()
	flushImages()
	if len(messages) == 0 {
		return nil, fmt.Errorf("claude request has no usable messages")
	}
	return messages, nil
}

func claudeResponseContentToOpenAIAssistantMessage(content any, clientToolNames map[string]struct{}) map[string]any {
	blocks := contentBlocksFromAny(content)
	if len(blocks) == 0 {
		return map[string]any{"role": "assistant", "content": ""}
	}
	return claudeContentBlocksToOpenAIAssistantMessage(blocks, clientToolNames)
}

func openAIAssistantMessageToClaudeResponseContent(message map[string]any) []any {
	blocks := make([]any, 0, 4)
	if content := message["content"]; content != nil && !isEmptyOpenAIContent(content) {
		blocks = append(blocks, openAIContentToClaudeBlocks(content)...)
	}
	if rawToolCalls, ok := message["tool_calls"].([]any); ok && len(rawToolCalls) > 0 {
		blocks = append(blocks, openAIToolCallsToClaudeBlocks(rawToolCalls)...)
	}
	if len(blocks) == 0 {
		return []any{map[string]any{"type": "text", "text": ""}}
	}
	return blocks
}

// mapOpenAIFinishReasonToClaude maps an OpenAI Chat finish_reason onto an
// Anthropic stop_reason. Every reason Anthropic names is spelled out: a value it
// does not know (content_filter, a provider's own label) used to be copied
// through verbatim, and Anthropic rejects an unknown stop_reason in history.
func mapOpenAIFinishReasonToClaude(reason string) string {
	switch strings.TrimSpace(reason) {
	case "", "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	case "pause_turn":
		return "pause_turn"
	default:
		return "end_turn"
	}
}

// mapClaudeStopReasonToOpenAIFinish is mapClaudeStopReasonToOpenAI for a Chat
// finish_reason. The two differ for the Anthropic-only terminal reasons: clients
// that switch on finish_reason only understand stop/length/tool_calls, so
// refusal and pause_turn become "stop" instead of leaking through.
func mapClaudeStopReasonToOpenAIFinish(reason string) string {
	switch strings.TrimSpace(reason) {
	case "refusal", "pause_turn":
		return "stop"
	default:
		return mapClaudeStopReasonToOpenAI(reason)
	}
}

// mapResponsesIncompleteToOpenAIFinish maps a Responses terminal state onto a
// Chat finish_reason. A turn cut short by max_output_tokens must report
// "length": reporting "stop" told the client the answer was whole and it never
// asked for the rest.
func mapResponsesIncompleteToOpenAIFinish(status, incompleteReason string) string {
	if strings.TrimSpace(status) != "incomplete" {
		return ""
	}
	switch strings.TrimSpace(incompleteReason) {
	case "content_filter":
		return "content_filter"
	default:
		return "length"
	}
}

// responsesIncompleteReason reads incomplete_details.reason from a Responses
// object, tolerating a missing or malformed object.
func responsesIncompleteReason(response map[string]any) string {
	if details, ok := response["incomplete_details"].(map[string]any); ok {
		return stringValue(details["reason"])
	}
	return ""
}

// responsesStopToClaudeSequences reads a Responses request's `stop` field,
// whether the SDK spelled it as a string or a list.
func responsesStopToClaudeSequences(source map[string]any) []string {
	if source == nil {
		return nil
	}
	return stopSequencesFromAny(source["stop"])
}

// toAnySliceOfStrings converts a []string to []any for JSON bodies built from
// map[string]any, so the encoder emits an array rather than a typed slice.
func toAnySliceOfStrings(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

// dedupeReasoningFields leaves a message one spelling of its thinking. Chat
// clients and relays disagree on the name: OpenAI-compatible vendors take
// "reasoning_content", pi and some relays send "reasoning", and DeepSeek (or a
// relay in front of it) reads the two as one field — a message carrying both is
// refused with "duplicate field reasoning", which fails the whole turn. When
// both are present reasoning_content wins and reasoning is dropped; a message
// with only one of them is untouched (its own name is kept), and a body with
// nothing to change comes back byte for byte.
func dedupeReasoningFields(body []byte) []byte {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return body
	}
	changed := false
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		content := strings.TrimSpace(stringValue(message["reasoning_content"]))
		reasoning := strings.TrimSpace(stringValue(message["reasoning"]))
		switch {
		case content != "" && reasoning != "":
			delete(message, "reasoning")
			changed = true
		case content == "" && reasoning != "":
			// 只有 reasoning 时也统一成 reasoning_content：DeepSeek 一系只认这个名字，
			// 留着 reasoning 会让这段思考在下一轮被丢掉。
			message["reasoning_content"] = message["reasoning"]
			delete(message, "reasoning")
			changed = true
		}
	}
	if !changed {
		return body
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(payload) != nil {
		return body
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}
