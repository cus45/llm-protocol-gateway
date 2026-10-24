package gateway

import (
	"encoding/json"
	"testing"
)

func geminiBody(t *testing.T, body string) map[string]any {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("bad test body: %v", err)
	}
	return parsed
}

// 一次普通对话：systemInstruction、user/model 两种角色、generationConfig 的
// 采样参数都要落到 Chat 的对应字段上。
func TestGeminiRequestBecomesAChatRequest(t *testing.T) {
	gemini := geminiBody(t, `{
		"systemInstruction": {"parts": [{"text": "be brief"}]},
		"contents": [
			{"role": "user", "parts": [{"text": "hi"}]},
			{"role": "model", "parts": [{"text": "hello"}]},
			{"role": "user", "parts": [{"text": "and now?"}]}
		],
		"generationConfig": {"maxOutputTokens": 64, "temperature": 0.2, "topP": 0.9, "topK": 40, "stopSequences": ["END"]}
	}`)
	req, err := geminiToOpenAIChatRequest(gemini, "gpt-test")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got := stringValue(req["model"]); got != "gpt-test" {
		t.Fatalf("model = %q", got)
	}
	if got, _ := intFromAny(req["max_tokens"]); got != 64 {
		t.Fatalf("max_tokens = %v, want 64", req["max_tokens"])
	}
	if got, _ := req["temperature"].(float64); got != 0.2 {
		t.Fatalf("temperature = %v", req["temperature"])
	}
	if stops, _ := req["stop"].([]any); len(stops) != 1 || stops[0] != "END" {
		t.Fatalf("stop = %#v", req["stop"])
	}

	messages, ok := req["messages"].([]any)
	if !ok || len(messages) != 4 {
		t.Fatalf("messages = %#v, want system + 3 turns", req["messages"])
	}
	want := []struct{ role, content string }{
		{"system", "be brief"},
		{"user", "hi"},
		{"assistant", "hello"},
		{"user", "and now?"},
	}
	for i, expected := range want {
		message, _ := messages[i].(map[string]any)
		if got := stringValue(message["role"]); got != expected.role {
			t.Fatalf("message %d role = %q, want %q", i, got, expected.role)
		}
		if got := stringValue(message["content"]); got != expected.content {
			t.Fatalf("message %d content = %q, want %q", i, got, expected.content)
		}
	}
}

// 工具调用与工具结果：functionCall 变 tool_calls，functionResponse 变 role:tool
// 消息；functionDeclarations 变 Chat 的 function 工具，toolConfig 变 tool_choice。
func TestGeminiToolCallRoundTrip(t *testing.T) {
	gemini := geminiBody(t, `{
		"contents": [
			{"role": "user", "parts": [{"text": "weather in Paris?"}]},
			{"role": "model", "parts": [{"functionCall": {"name": "get_weather", "args": {"city": "Paris"}}}]},
			{"role": "user", "parts": [{"functionResponse": {"name": "get_weather", "response": {"tempC": 21}}}]}
		],
		"tools": [{"functionDeclarations": [
			{"name": "get_weather", "description": "weather lookup", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}},
			{"name": "", "description": "nameless declarations are skipped"}
		]}],
		"toolConfig": {"functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": ["get_weather"]}}
	}`)
	req, err := geminiToOpenAIChatRequest(gemini, "m")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	messages, _ := req["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %#v, want 3", req["messages"])
	}
	assistant, _ := messages[1].(map[string]any)
	if got := stringValue(assistant["role"]); got != "assistant" {
		t.Fatalf("assistant role = %q", got)
	}
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %#v", assistant["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	function, _ := call["function"].(map[string]any)
	if got := stringValue(function["name"]); got != "get_weather" {
		t.Fatalf("tool name = %q", got)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(stringValue(function["arguments"])), &args); err != nil {
		t.Fatalf("tool arguments are not JSON: %v", err)
	}
	if args["city"] != "Paris" {
		t.Fatalf("tool arguments = %#v", args)
	}

	toolMessage, _ := messages[2].(map[string]any)
	if got := stringValue(toolMessage["role"]); got != "tool" {
		t.Fatalf("tool result role = %q, want tool", got)
	}
	if got := stringValue(toolMessage["tool_call_id"]); got != "call_get_weather" {
		t.Fatalf("tool_call_id = %q, want it to match the call", got)
	}

	tools, _ := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v, want the one named declaration", req["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if got := stringValue(tool["type"]); got != "function" {
		t.Fatalf("tool type = %q", got)
	}
	declaration, _ := tool["function"].(map[string]any)
	if got := stringValue(declaration["name"]); got != "get_weather" {
		t.Fatalf("declaration name = %q", got)
	}
	if _, ok := declaration["parameters"].(map[string]any); !ok {
		t.Fatalf("declaration lost its parameters: %#v", declaration)
	}

	choice, _ := req["tool_choice"].(map[string]any)
	if got := stringValue(choice["type"]); got != "function" {
		t.Fatalf("tool_choice = %#v, want the single allowed function", req["tool_choice"])
	}
}

// Chat 的回答变回 Gemini：文本进 parts[].text，工具调用进 functionCall，
// finish_reason 与 usage 换成 Gemini 的名字。
func TestChatResponseBecomesAGeminiResponse(t *testing.T) {
	chat := geminiBody(t, `{
		"choices": [{"message": {"role": "assistant", "content": "here you go",
			"tool_calls": [{"id": "x", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}}]},
			"finish_reason": "tool_calls"}],
		"usage": {"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}
	}`)
	out, err := openAIChatToGeminiResponse(chat, "gemini-x")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	candidates, _ := out["candidates"].([]any)
	if len(candidates) != 1 {
		t.Fatalf("candidates = %#v", out["candidates"])
	}
	candidate, _ := candidates[0].(map[string]any)
	if got := stringValue(candidate["finishReason"]); got != "STOP" {
		t.Fatalf("finishReason = %q, want STOP", got)
	}
	content, _ := candidate["content"].(map[string]any)
	if got := stringValue(content["role"]); got != "model" {
		t.Fatalf("content role = %q, want model", got)
	}
	parts, _ := content["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %#v, want text + functionCall", parts)
	}
	textPart, _ := parts[0].(map[string]any)
	if got := stringValue(textPart["text"]); got != "here you go" {
		t.Fatalf("text part = %q", got)
	}
	callPart, _ := parts[1].(map[string]any)
	call, _ := callPart["functionCall"].(map[string]any)
	if got := stringValue(call["name"]); got != "get_weather" {
		t.Fatalf("functionCall name = %q", got)
	}
	args, _ := call["args"].(map[string]any)
	if args["city"] != "Paris" {
		t.Fatalf("functionCall args = %#v, want them decoded from the JSON string", args)
	}
	usage, _ := out["usageMetadata"].(map[string]any)
	if got, _ := intFromAny(usage["totalTokenCount"]); got != 18 {
		t.Fatalf("totalTokenCount = %v", usage)
	}
	if got := stringValue(out["modelVersion"]); got != "gemini-x" {
		t.Fatalf("modelVersion = %q", got)
	}
}

// 坏的工具参数不能静默变成空对象：那是把一个客户端错误当成成功回答。
func TestGeminiResponseRejectsBadToolArguments(t *testing.T) {
	chat := geminiBody(t, `{"choices": [{"message": {"tool_calls": [
		{"function": {"name": "f", "arguments": "{not json"}}
	]}}]}`)
	if _, err := openAIChatToGeminiResponse(chat, "m"); err == nil {
		t.Fatalf("malformed tool arguments should be an error")
	}
}

func TestGeminiFinishReasonTable(t *testing.T) {
	cases := map[string]string{
		"stop":           "STOP",
		"length":         "MAX_TOKENS",
		"tool_calls":     "STOP",
		"content_filter": "SAFETY",
		"weird":          "OTHER",
	}
	for finish, want := range cases {
		if got := geminiFinishReason(finish); got != want {
			t.Fatalf("geminiFinishReason(%q) = %q, want %q", finish, got, want)
		}
	}
}

// 同名工具被并行调用两次（读两个文件）时，两次调用与两条结果必须一一配对：
// Gemini 不传 id，网关自己编的 id 一旦重复，上游会把两条结果当成同一次调用。
func TestGeminiParallelSameNameCallsGetDistinctIDs(t *testing.T) {
	gemini := geminiBody(t, `{
		"contents": [
			{"role": "user", "parts": [{"text": "read a and b"}]},
			{"role": "model", "parts": [
				{"functionCall": {"name": "read", "args": {"path": "a"}}},
				{"functionCall": {"name": "read", "args": {"path": "b"}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"name": "read", "response": {"out": "A"}}},
				{"functionResponse": {"name": "read", "response": {"out": "B"}}}
			]}
		]
	}`)
	req, err := geminiToOpenAIChatRequest(gemini, "m")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	messages, _ := req["messages"].([]any)
	assistant, _ := messages[1].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	first, _ := calls[0].(map[string]any)
	second, _ := calls[1].(map[string]any)
	if first["id"] == second["id"] {
		t.Fatalf("parallel same-name calls share the id %v", first["id"])
	}
	// 两条 tool 结果分别配上两次调用，且顺序一致。
	for i, want := range []any{first["id"], second["id"]} {
		message, _ := messages[2+i].(map[string]any)
		if got := message["tool_call_id"]; got != want {
			t.Fatalf("tool result %d pairs with %v, want %v", i+1, got, want)
		}
	}
}

// Claude 的 output_config.format 没有 name（Claude GA 拒收），转成 Chat 的
// response_format 时 OpenAI 形状要求 name 必填：必须补一个稳定默认值，否则
// 上游直接 400。
func TestClaudeFormatToChatCarriesAName(t *testing.T) {
	format := outputConfigFormatToChatResponseFormat(map[string]any{
		"format": map[string]any{
			"type":   "json_schema",
			"schema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	})
	if format == nil {
		t.Fatalf("conversion returned nothing")
	}
	inner, _ := format["json_schema"].(map[string]any)
	if name := stringValue(inner["name"]); name == "" {
		t.Fatalf("json_schema is missing the name OpenAI requires: %#v", format)
	}
	// 稳定：同一份输入两次转换结果一致（否则会打散上游的 prompt cache）。
	again := outputConfigFormatToChatResponseFormat(map[string]any{
		"format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object", "properties": map[string]any{}}},
	})
	innerAgain, _ := again["json_schema"].(map[string]any)
	if innerAgain["name"] != inner["name"] {
		t.Fatalf("the synthesized name is not stable")
	}
}
