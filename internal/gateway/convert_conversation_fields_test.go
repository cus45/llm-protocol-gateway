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
