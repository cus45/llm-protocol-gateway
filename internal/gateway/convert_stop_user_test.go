package gateway

import (
	"encoding/json"
	"testing"
)

func mustRequest(t *testing.T, fn func() (map[string]any, error)) map[string]any {
	t.Helper()
	req, err := fn()
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	return req
}

// TestClaudeStopAndMetadataToChat: Claude 的 stop_sequences / metadata.user_id
// 必须在 Chat 请求里成为 stop / user，否则上游会一直生成到 max_tokens，
// 且终端用户标识丢失（限流与审计按匿名算）。
func TestClaudeStopAndMetadataToChat(t *testing.T) {
	claudeReq := map[string]any{
		"model":          "claude-sonnet-4-5",
		"max_tokens":     128,
		"stop_sequences": []any{"\n\nHuman:", "END"},
		"metadata":       map[string]any{"user_id": "user-42"},
		"messages":       []any{map[string]any{"role": "user", "content": "hi"}},
	}
	chat := mustRequest(t, func() (map[string]any, error) {
		return claudeRequestToOpenAIChat(claudeReq, "gpt-4.1")
	})
	stop, _ := chat["stop"].([]any)
	if len(stop) != 2 || stop[0] != "\n\nHuman:" || stop[1] != "END" {
		t.Fatalf("stop = %#v, want the two sequences", chat["stop"])
	}
	if chat["user"] != "user-42" {
		t.Fatalf("user = %#v, want user-42", chat["user"])
	}

	// A single sequence still goes out as an array: strict upstreams such as
	// GLM's ChatCompletionRequest refuse the string form with a 400.
	claudeReq["stop_sequences"] = []any{"END"}
	chat = mustRequest(t, func() (map[string]any, error) {
		return claudeRequestToOpenAIChat(claudeReq, "gpt-4.1")
	})
	single, _ := chat["stop"].([]any)
	if len(single) != 1 || single[0] != "END" {
		t.Fatalf("single stop = %#v, want [\"END\"]", chat["stop"])
	}
}

// TestResponsesStopUserTopKToClaude: Responses 客户端的 stop / user / top_k 进
// Claude 后应分别成为 stop_sequences / metadata.user_id / top_k。
func TestResponsesStopUserTopKToClaude(t *testing.T) {
	responsesReq := map[string]any{
		"model":             "claude-sonnet-4-5",
		"input":             "hi",
		"stop":              []any{"END"},
		"user":              "user-7",
		"top_k":             float64(40),
		"temperature":       0.2,
		"max_output_tokens": float64(64),
	}
	claude := mustRequest(t, func() (map[string]any, error) {
		req, _, err := responsesToClaudeRequestDirect(responsesReq, "claude-sonnet-4-5", 0)
		return req, err
	})
	stop, _ := claude["stop_sequences"].([]any)
	if len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop_sequences = %#v", claude["stop_sequences"])
	}
	metadata, _ := claude["metadata"].(map[string]any)
	if metadata["user_id"] != "user-7" {
		t.Fatalf("metadata = %#v", claude["metadata"])
	}
	if claude["top_k"] != float64(40) {
		t.Fatalf("top_k = %#v", claude["top_k"])
	}
	if claude["temperature"] != 0.2 {
		t.Fatalf("temperature = %#v", claude["temperature"])
	}
}

// TestResponsesStopUserToChat: Responses→Chat 同样要带上 stop 与 user。
func TestResponsesStopUserToChat(t *testing.T) {
	responsesReq := map[string]any{
		"model": "gpt-4.1",
		"input": "hi",
		"stop":  "END",
		"user":  "user-9",
	}
	chat := mustRequest(t, func() (map[string]any, error) {
		req, _, err := responsesToOpenAIChatRequest(responsesReq, "gpt-4.1")
		return req, err
	})
	stop, _ := chat["stop"].([]any)
	if len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop = %#v", chat["stop"])
	}
	if chat["user"] != "user-9" {
		t.Fatalf("user = %#v", chat["user"])
	}
}

// TestChatStopUserToResponses: Chat→Responses 把 stop / user 带过去。
func TestChatStopUserToResponses(t *testing.T) {
	chatReq := map[string]any{
		"model":    "gpt-4.1",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stop":     []any{"END", "STOP"},
		"user":     "user-3",
	}
	responses := mustRequest(t, func() (map[string]any, error) {
		return openAIChatToResponsesRequest(chatReq, "gpt-4.1")
	})
	stop, _ := responses["stop"].([]any)
	if len(stop) != 2 || stop[1] != "STOP" {
		t.Fatalf("stop = %#v", responses["stop"])
	}
	if responses["user"] != "user-3" {
		t.Fatalf("user = %#v", responses["user"])
	}
}

// TestClaudeStopMetadataToResponses: Claude→Responses 是 Responses→Claude 的
// 反向，stop_sequences 必须能回来。
func TestClaudeStopMetadataToResponses(t *testing.T) {
	claudeReq := map[string]any{
		"model":          "claude-sonnet-4-5",
		"max_tokens":     64,
		"stop_sequences": []any{"END"},
		"metadata":       map[string]any{"user_id": "user-5"},
		"messages":       []any{map[string]any{"role": "user", "content": "hi"}},
	}
	responses := mustRequest(t, func() (map[string]any, error) {
		return claudeToResponsesRequestDirect(claudeReq, "gpt-4.1")
	})
	// Responses takes the single-string form.
	if responses["stop"] != "END" {
		t.Fatalf("stop = %#v, want \"END\"", responses["stop"])
	}
	if responses["user"] != "user-5" {
		t.Fatalf("user = %#v", responses["user"])
	}
	// Round trip through the serialised body: the field must survive JSON.
	if _, err := json.Marshal(responses); err != nil {
		t.Fatalf("marshal: %v", err)
	}
}
