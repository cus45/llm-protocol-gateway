package gateway

import (
	"encoding/json"
	"testing"
)

// Responses 的 assistant turn 可以同时有说明文字和工具调用（"我先查一下" +
// tool_call）。转成 Chat 时把 content 置空会丢掉这段说明，客户端回放历史时
// 模型看不到自己为什么调工具，对话质量明显下降。
func TestResponsesToChatKeepsPrefaceAlongsideToolCalls(t *testing.T) {
	responsesBody := []byte(`{"id":"resp_1","object":"response","model":"gpt-5","status":"completed",` +
		`"output":[` +
		`{"type":"message","id":"msg_1","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"Let me check the weather first.","annotations":[]}]},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}","status":"completed"}],` +
		`"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}}`)
	body, _, err := responsesToOpenAIChatResponse(responsesBody, "gpt-5")
	if err != nil {
		t.Fatalf("responsesToOpenAIChatResponse: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	choice := payload["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if content, _ := message["content"].(string); content != "Let me check the weather first." {
		t.Fatalf("preface text lost: content = %#v", message["content"])
	}
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %#v", message["tool_calls"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %#v, want tool_calls", choice["finish_reason"])
	}

	// 只有工具调用、没有文字时，content 仍是 null（Chat 的惯例，不是空串）。
	noPreface := []byte(`{"id":"resp_2","object":"response","model":"gpt-5","status":"completed",` +
		`"output":[{"type":"function_call","id":"fc_2","call_id":"call_2","name":"get_weather","arguments":"{}","status":"completed"}],` +
		`"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}}`)
	body, _, err = responsesToOpenAIChatResponse(noPreface, "gpt-5")
	if err != nil {
		t.Fatalf("responsesToOpenAIChatResponse: %v", err)
	}
	payload = map[string]any{}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	message = payload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if content, exists := message["content"]; !exists || content != nil {
		t.Fatalf("content without preface = %#v, want null", content)
	}
}
