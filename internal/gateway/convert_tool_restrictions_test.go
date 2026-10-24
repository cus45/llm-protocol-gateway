package gateway

import (
	"encoding/json"
	"testing"
)

func convertChatToolsTest(t *testing.T, raw string) map[string]any {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("request is not json: %v", err)
	}
	claudeReq, err := openAIChatToClaudeRequest(req, "claude-sonnet-4.5", 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	return claudeReq
}

// Chat 的 parallel_tool_calls=false 要落到 Claude 的
// tool_choice.disable_parallel_tool_use，否则上游照样并行发调用。
func TestChatParallelToolCallsToClaude(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantChoice map[string]any
	}{
		{
			name:       "disabled without tool_choice",
			body:       `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"a"}}],"parallel_tool_calls":false}`,
			wantChoice: map[string]any{"type": "auto", "disable_parallel_tool_use": true},
		},
		{
			name:       "disabled with named choice",
			body:       `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"a"}}],"tool_choice":{"type":"function","function":{"name":"a"}},"parallel_tool_calls":false}`,
			wantChoice: map[string]any{"type": "tool", "name": "a", "disable_parallel_tool_use": true},
		},
		{
			name:       "none is left alone",
			body:       `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"a"}}],"tool_choice":"none","parallel_tool_calls":false}`,
			wantChoice: map[string]any{"type": "none"},
		},
		{
			name:       "enabled adds nothing",
			body:       `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"a"}}],"tool_choice":"required","parallel_tool_calls":true}`,
			wantChoice: map[string]any{"type": "any"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := convertChatToolsTest(t, tc.body)["tool_choice"]
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.wantChoice)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("tool_choice = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// Claude 的 disable_parallel_tool_use 搬回 Chat 的 parallel_tool_calls=false。
func TestClaudeDisableParallelToolUseToChat(t *testing.T) {
	var req map[string]any
	if err := json.Unmarshal([]byte(`{
		"model":"claude-sonnet-4.5",
		"max_tokens":100,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"a","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"any","disable_parallel_tool_use":true}
	}`), &req); err != nil {
		t.Fatalf("request is not json: %v", err)
	}
	chatReq, err := claudeRequestToOpenAIChat(req, "gpt-4o")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if chatReq["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %#v", chatReq["tool_choice"])
	}
	if chatReq["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %#v, want false", chatReq["parallel_tool_calls"])
	}
}

// Responses 的 allowed_tools：只保留白名单里的工具并把模式带过去。
// Anthropic 的 tool_choice 只有 type/name/disable_parallel_tool_use，没有
// allowed_tools 字段（magpie 同样只下发 {"type":"any"}），白名单只靠收窄
// tools 数组达成——把白名单塞进 tool_choice 会被上游 400。
func TestResponsesAllowedToolsToClaude(t *testing.T) {
	var req map[string]any
	if err := json.Unmarshal([]byte(`{
		"model":"claude-sonnet-4.5",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[
			{"type":"function","name":"read","parameters":{"type":"object"}},
			{"type":"function","name":"write","parameters":{"type":"object"}}
		],
		"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"read"}]}
	}`), &req); err != nil {
		t.Fatalf("request is not json: %v", err)
	}
	claudeReq, _, err := responsesToClaudeRequestDirect(req, "claude-sonnet-4.5", 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	choice, ok := claudeReq["tool_choice"].(map[string]any)
	if !ok || choice["type"] != "any" {
		t.Fatalf("tool_choice = %#v, want any", claudeReq["tool_choice"])
	}
	if _, has := choice["allowed_tools"]; has {
		t.Fatalf("tool_choice must not carry allowed_tools (Claude has no such field): %#v", choice)
	}
	toolList, _ := claudeReq["tools"].([]any)
	if len(toolList) != 1 {
		t.Fatalf("tools must be narrowed to the allow-list: %#v", claudeReq["tools"])
	}
	if name := stringValue(toolList[0].(map[string]any)["name"]); name != "read" {
		t.Fatalf("kept tool = %q", name)
	}
}

// mode=auto 时 tool_choice 不下发（Claude 默认就是 auto），白名单仍然生效；
// 白名单把工具全收掉时不能留 any，否则等于要求调一个不存在的工具。
func TestResponsesAllowedToolsToClaudeAutoAndEmpty(t *testing.T) {
	cases := []struct {
		name       string
		choice     string
		wantChoice any
		wantTools  int
	}{
		{"auto keeps tools only", `{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"read"}]}`, nil, 1},
		{"required with nothing left falls back to auto", `{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"absent"}]}`, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req map[string]any
			body := `{"model":"claude-sonnet-4.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
				`"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],"tool_choice":` + tc.choice + `}`
			if err := json.Unmarshal([]byte(body), &req); err != nil {
				t.Fatalf("request is not json: %v", err)
			}
			claudeReq, _, err := responsesToClaudeRequestDirect(req, "claude-sonnet-4.5", 0)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			if tc.wantChoice == nil {
				if got, has := claudeReq["tool_choice"]; has {
					t.Fatalf("tool_choice = %#v, want absent", got)
				}
			}
			if list, _ := claudeReq["tools"].([]any); len(list) != tc.wantTools {
				t.Fatalf("tools = %#v, want %d", claudeReq["tools"], tc.wantTools)
			}
		})
	}
}

// 白名单条目同时带 Responses 的 type/name 与 Claude 的 input_schema：有些兼容
// 上游按 Responses 的形状读它，条目里没有 type/name 就没有工具能对上号。
func TestResponsesAllowedToolsKeepsTypeAndName(t *testing.T) {
	var req map[string]any
	if err := json.Unmarshal([]byte(`{
		"model":"claude-sonnet-4.5",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[
			{"type":"function","name":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}},
			{"type":"function","name":"write","parameters":{"type":"object"}}
		],
		"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[
			{"type":"function","name":"read","parameters":{"type":"object"}}
		]}
	}`), &req); err != nil {
		t.Fatalf("request is not json: %v", err)
	}
	claudeReq, _, err := responsesToClaudeRequestDirect(req, "claude-sonnet-4.5", 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	// 白名单不再下发到 tool_choice（Claude 没有该字段），但收窄后的工具条目必须
	// 仍是 Claude 的完整形状：type/name 与 input_schema 都要在。
	if _, has := claudeReq["tool_choice"]; has {
		t.Fatalf("mode=auto must not emit tool_choice: %#v", claudeReq["tool_choice"])
	}
	list, _ := claudeReq["tools"].([]any)
	if len(list) != 1 {
		t.Fatalf("tools = %#v, want the single allow-listed tool", claudeReq["tools"])
	}
	entry, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("narrowed tool is not an object: %#v", list[0])
	}
	if got := stringValue(entry["name"]); got != "read" {
		t.Fatalf("entry name = %q, want read", got)
	}
	if _, has := entry["input_schema"]; !has {
		t.Fatalf("entry lost the Claude half (input_schema): %#v", entry)
	}
}

// 线上验证用的形状（GLM 的 Anthropic 端点接受的）要能被 JSON 原样发出去：
// Chat→Claude 的工具约束要走真实转换路径：any + disable_parallel_tool_use
// 同时在场，且客户端没有 tools 时不得凭空出现 tool_choice。
func TestChatToClaudeToolChoiceWireShape(t *testing.T) {
	var req map[string]any
	if err := json.Unmarshal([]byte(`{
		"model":"glm-5.3",
		"max_tokens":100,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"get_time","parameters":{"type":"object","properties":{}}}}],
		"tool_choice":{"type":"any","disable_parallel_tool_use":true}
	}`), &req); err != nil {
		t.Fatalf("request is not json: %v", err)
	}
	claudeReq, err := openAIChatToClaudeRequest(req, "glm-5.3", 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	choice, ok := claudeReq["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice missing: %#v", claudeReq)
	}
	if choice["type"] != "any" || choice["disable_parallel_tool_use"] != true {
		t.Fatalf("tool_choice = %#v", choice)
	}
	tools, _ := claudeReq["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", claudeReq["tools"])
	}
	if name := stringValue(tools[0].(map[string]any)["name"]); name != "get_time" {
		t.Fatalf("tool name = %q", name)
	}

	noTools := map[string]any{
		"model":       "glm-5.3",
		"max_tokens":  float64(100),
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
		"tool_choice": map[string]any{"type": "auto"},
	}
	claudeNoTools, err := openAIChatToClaudeRequest(noTools, "glm-5.3", 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if _, present := claudeNoTools["tool_choice"]; present {
		t.Fatalf("tool_choice without tools: %#v", claudeNoTools["tool_choice"])
	}
}

// Responses 的 parallel_tool_calls=false 在没有 tool_choice 时也要生效。
func TestResponsesParallelToolCallsWithoutChoice(t *testing.T) {
	var req map[string]any
	if err := json.Unmarshal([]byte(`{
		"model":"claude-sonnet-4.5",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],
		"parallel_tool_calls":false
	}`), &req); err != nil {
		t.Fatalf("request is not json: %v", err)
	}
	claudeReq, _, err := responsesToClaudeRequestDirect(req, "claude-sonnet-4.5", 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	choice, ok := claudeReq["tool_choice"].(map[string]any)
	if !ok || choice["disable_parallel_tool_use"] != true {
		t.Fatalf("tool_choice = %#v, want disable_parallel_tool_use", claudeReq["tool_choice"])
	}
}
