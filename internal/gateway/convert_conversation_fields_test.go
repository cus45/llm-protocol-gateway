package gateway

import (
	"encoding/json"
	"testing"
)

// Responses 的 prompt_cache_key / service_tier 在转 Chat 时必须带走：缓存键丢了
// 上游就按新对话算，fast 档也悄悄降级。
func TestResponsesToChatConversationFields(t *testing.T) {
	var req map[string]any
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-4o",
		"prompt_cache_key":"thread-42",
		"service_tier":"priority",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]
	}`), &req); err != nil {
		t.Fatalf("request is not json: %v", err)
	}
	chatReq, _, err := responsesToOpenAIChatRequest(req, "gpt-4o")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if chatReq["prompt_cache_key"] != "thread-42" {
		t.Fatalf("prompt_cache_key = %#v", chatReq["prompt_cache_key"])
	}
	if chatReq["service_tier"] != "priority" {
		t.Fatalf("service_tier = %#v", chatReq["service_tier"])
	}
}

// Chat → Responses 同理，且档位大小写归一。
func TestChatToResponsesConversationFields(t *testing.T) {
	var req map[string]any
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-4o",
		"prompt_cache_key":"thread-7",
		"service_tier":"FLEX",
		"messages":[{"role":"user","content":"hi"}]
	}`), &req); err != nil {
		t.Fatalf("request is not json: %v", err)
	}
	responsesReq, err := openAIChatToResponsesRequest(req, "gpt-4o")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if responsesReq["prompt_cache_key"] != "thread-7" {
		t.Fatalf("prompt_cache_key = %#v", responsesReq["prompt_cache_key"])
	}
	if responsesReq["service_tier"] != "flex" {
		t.Fatalf("service_tier = %#v", responsesReq["service_tier"])
	}
}

// 上游不认识的档位不搬：搬了整轮会 400，而档位只是优化。
func TestConversationFieldsDropUnknownTier(t *testing.T) {
	chatReq := map[string]any{"service_tier": "turbo", "prompt_cache_key": "  "}
	responsesReq := map[string]any{}
	forwardChatConversationFields(chatReq, responsesReq)
	if _, exists := responsesReq["service_tier"]; exists {
		t.Fatalf("unknown tier must not be forwarded: %#v", responsesReq)
	}
	if _, exists := responsesReq["prompt_cache_key"]; exists {
		t.Fatalf("blank cache key must not be forwarded: %#v", responsesReq)
	}

	other := map[string]any{"service_tier": "turbo", "prompt_cache_key": "k"}
	out := map[string]any{}
	forwardResponsesConversationFields(other, out)
	if _, exists := out["service_tier"]; exists {
		t.Fatalf("unknown tier must not be forwarded: %#v", out)
	}
	if out["prompt_cache_key"] != "k" {
		t.Fatalf("prompt_cache_key = %#v", out["prompt_cache_key"])
	}
}

// D32-6：safety_identifier 是 Responses 客户端每轮都带的安全审计字段，转 Chat /
// Claude 时必须随对话字段一起带走，回来时再还原。
func TestSafetyIdentifierCarriedAcrossProtocols(t *testing.T) {
	chatReq := map[string]any{"safety_identifier": "user_hash_42", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	responsesReq, err := openAIChatToResponsesRequest(chatReq, "gpt-4o")
	if err != nil {
		t.Fatalf("chat convert: %v", err)
	}
	if responsesReq["safety_identifier"] != "user_hash_42" {
		t.Fatalf("chat -> responses safety_identifier = %#v", responsesReq["safety_identifier"])
	}

	back, _, err := responsesToOpenAIChatRequest(map[string]any{
		"model":             "gpt-4o",
		"safety_identifier": "user_hash_7",
		"input":             []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}},
	}, "gpt-4o")
	if err != nil {
		t.Fatalf("responses convert: %v", err)
	}
	if back["safety_identifier"] != "user_hash_7" {
		t.Fatalf("responses -> chat safety_identifier = %#v", back["safety_identifier"])
	}

	claudeReq, _, err := responsesToClaudeRequestDirect(map[string]any{
		"model":             "claude-sonnet-5",
		"safety_identifier": "user_hash_9",
		"input":             []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}},
	}, "claude-sonnet-5", 0)
	if err != nil {
		t.Fatalf("claude convert: %v", err)
	}
	metadata, _ := claudeReq["metadata"].(map[string]any)
	if metadata["user_id"] != "user_hash_9" {
		t.Fatalf("claude metadata = %#v, want user_id from safety_identifier", metadata)
	}

	// Claude 的 metadata.user_id 回来时同时还原成 user 与 safety_identifier。
	var converted map[string]any
	if err := json.Unmarshal(claudeToResponsesRequestJSONForTest(t, `{"model":"claude-sonnet-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"user_hash_3"}}`), &converted); err != nil {
		t.Fatalf("claude -> responses: %v", err)
	}
	if converted["safety_identifier"] != "user_hash_3" || converted["user"] != "user_hash_3" {
		t.Fatalf("claude -> responses id fields = %#v / %#v", converted["user"], converted["safety_identifier"])
	}
}

// claudeToResponsesRequestJSONForTest runs the Claude -> Responses request
// conversion and returns the marshalled result.
func claudeToResponsesRequestJSONForTest(t *testing.T, raw string) []byte {
	t.Helper()
	var claudeReq map[string]any
	if err := json.Unmarshal([]byte(raw), &claudeReq); err != nil {
		t.Fatalf("request is not json: %v", err)
	}
	responsesReq, err := claudeToResponsesRequest(claudeReq, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	out, err := json.Marshal(responsesReq)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}
