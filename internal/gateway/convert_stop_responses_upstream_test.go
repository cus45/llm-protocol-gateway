package gateway

import (
	"encoding/json"
	"testing"
)

// Responses 上游没有 stop 参数（OpenAI 的 Responses API 不定义它，magpie 也从
// 不发送）：带上去只换来 400 Unknown parameter: 'stop'。客户端的停止序列必须
// 由网关记住、在回包里回填，而不是转发。
func TestStopNeverForwardedToResponsesUpstream(t *testing.T) {
	claudeReq := map[string]any{
		"model":          "claude-sonnet-4-5",
		"max_tokens":     64,
		"stop_sequences": []any{"END"},
		"messages":       []any{map[string]any{"role": "user", "content": "hi"}},
	}
	claudeToResponses := mustRequest(t, func() (map[string]any, error) {
		return claudeToResponsesRequestDirect(claudeReq, "gpt-5")
	})
	if _, exists := claudeToResponses["stop"]; exists {
		t.Fatalf("Claude→Responses forwarded stop to the Responses upstream: %#v", claudeToResponses["stop"])
	}

	chatReq := map[string]any{
		"model":    "gpt-4.1",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stop":     []any{"END", "STOP"},
	}
	chatToResponses := mustRequest(t, func() (map[string]any, error) {
		return openAIChatToResponsesRequest(chatReq, "gpt-5")
	})
	if _, exists := chatToResponses["stop"]; exists {
		t.Fatalf("Chat→Responses forwarded stop to the Responses upstream: %#v", chatToResponses["stop"])
	}

	// 其余 Responses→Claude 方向仍然要保留映射：那是到 Claude 的合法字段。
	responsesReq := map[string]any{
		"model": "gpt-5",
		"stop":  "END",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	}
	claude, _, err := responsesToClaudeRequestDirect(responsesReq, "claude-sonnet-4-5", 0)
	if err != nil {
		t.Fatalf("responsesToClaudeRequestDirect: %v", err)
	}
	if stop, _ := claude["stop_sequences"].([]any); len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("Responses→Claude lost stop_sequences: %#v", claude["stop_sequences"])
	}
	if _, err := json.Marshal(claudeToResponses); err != nil {
		t.Fatalf("marshal: %v", err)
	}
}
