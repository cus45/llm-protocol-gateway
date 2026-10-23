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
	allowed, ok := choice["allowed_tools"].(map[string]any)
	if !ok {
		t.Fatalf("allowed_tools missing: %#v", choice)
	}
	tools, _ := allowed["tools"].(map[string]any)
	if _, hasRead := tools["read"]; !hasRead || len(tools) != 1 {
		t.Fatalf("allowed_tools = %#v", tools)
	}
	toolList, _ := claudeReq["tools"].([]any)
	if len(toolList) != 1 {
		t.Fatalf("tools must be narrowed to the allow-list: %#v", claudeReq["tools"])
	}
	if name := stringValue(toolList[0].(map[string]any)["name"]); name != "read" {
		t.Fatalf("kept tool = %q", name)
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
