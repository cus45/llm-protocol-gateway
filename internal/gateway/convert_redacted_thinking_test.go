package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// Anthropic's signed thinking and redacted_thinking blocks must survive the
// Responses hop byte for byte: a follow-up tool turn that drops them is
// refused upstream, and the model loses the reasoning it was mid-way through.
// redacted_thinking has no readable text at all, so encrypted_content is its
// only carrier — it must never be silently dropped.

func TestResponsesReasoningRoundTripsRedactedThinking(t *testing.T) {
	original := map[string]any{"type": "redacted_thinking", "data": "EmwKAhgBEgyOpaqueCiphertext=="}
	item, ok := responsesReasoningItemFromAnthropicBlock("rs_1", original)
	if !ok {
		t.Fatal("redacted_thinking must produce a reasoning item")
	}
	summary, _ := item["summary"].([]any)
	if len(summary) != 0 {
		t.Fatalf("redacted_thinking summary = %v, want empty (it has no readable text)", summary)
	}
	restored, ok := decodeAnthropicThinkingBlock(stringValue(item["encrypted_content"]))
	if !ok {
		t.Fatal("encrypted_content did not decode")
	}
	if stringValue(restored["type"]) != "redacted_thinking" || stringValue(restored["data"]) != stringValue(original["data"]) {
		t.Fatalf("restored block = %#v, want the original redacted_thinking", restored)
	}
}

func TestResponsesReasoningRoundTripsSignedThinking(t *testing.T) {
	original := map[string]any{"type": "thinking", "thinking": "Let me check the file.", "signature": "sig-abc"}
	item, ok := responsesReasoningItemFromAnthropicBlock("rs_1", original)
	if !ok {
		t.Fatal("thinking must produce a reasoning item")
	}
	summary, _ := item["summary"].([]any)
	if len(summary) != 1 || stringValue(summary[0].(map[string]any)["text"]) != "Let me check the file." {
		t.Fatalf("summary = %v", summary)
	}
	restored, ok := decodeAnthropicThinkingBlock(stringValue(item["encrypted_content"]))
	if !ok {
		t.Fatal("encrypted_content did not decode")
	}
	if stringValue(restored["signature"]) != "sig-abc" || stringValue(restored["thinking"]) != "Let me check the file." {
		t.Fatalf("restored block = %#v", restored)
	}
}

// Foreign ciphertext (another provider's reasoning) must be rejected, not
// misread as ours.
func TestDecodeAnthropicThinkingBlockRejectsForeignContent(t *testing.T) {
	for _, encrypted := range []string{"", "gAAAAB-foreign-openai-ciphertext", anthropicThinkingEncryptedPrefix + "not-base64!!"} {
		if _, ok := decodeAnthropicThinkingBlock(encrypted); ok {
			t.Fatalf("decodeAnthropicThinkingBlock(%q) accepted foreign content", encrypted)
		}
	}
}

// Responses → Claude, non-streaming: the reasoning item's encrypted_content
// comes back as the exact signed block, in front of the visible answer.
func TestResponsesToClaudeRestoresSignedThinkingBlock(t *testing.T) {
	item, ok := responsesReasoningItemFromAnthropicBlock("rs_1", map[string]any{
		"type": "thinking", "thinking": "Plan.", "signature": "sig-1",
	})
	if !ok {
		t.Fatal("encode failed")
	}
	req := map[string]any{
		"model": "claude-sonnet-5",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
			item,
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Hello."}}},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "again"}}},
		},
	}
	claudeReq, _, err := responsesToClaudeRequestDirect(req, "claude-sonnet-5", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found bool
	for _, msg := range claudeMessagesFromAny(claudeReq["messages"]) {
		if stringValue(msg["role"]) != "assistant" {
			continue
		}
		blocks, _ := msg["content"].([]any)
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			if stringValue(block["type"]) == "thinking" {
				found = true
				if stringValue(block["signature"]) != "sig-1" || stringValue(block["thinking"]) != "Plan." {
					t.Fatalf("restored thinking = %#v", block)
				}
			}
		}
	}
	if !found {
		t.Fatalf("thinking block missing from %#v", claudeReq["messages"])
	}
}

// Claude → Responses stream: a redacted_thinking block becomes a reasoning
// item whose encrypted_content restores the original block on the next hop.
func TestClaudeStreamRedactedThinkingSurvivesResponsesHop(t *testing.T) {
	input := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4","usage":{"input_tokens":5,"output_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"OpaqueCipher=="}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Done."}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	recorder := httptest.NewRecorder()
	if _, err := streamClaudeToResponsesEventsDirect(recorder, strings.NewReader(input), "claude-sonnet-4", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var encrypted string
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			continue
		}
		if item, ok := event["item"].(map[string]any); ok && stringValue(item["type"]) == "reasoning" {
			if value := stringValue(item["encrypted_content"]); value != "" {
				encrypted = value
			}
		}
		if response, ok := event["response"].(map[string]any); ok {
			for _, raw := range asMapSlice(response["output"]) {
				entry, _ := raw.(map[string]any)
				if stringValue(entry["type"]) == "reasoning" {
					if value := stringValue(entry["encrypted_content"]); value != "" {
						encrypted = value
					}
				}
			}
		}
	}
	if encrypted == "" {
		t.Fatalf("no reasoning item with encrypted_content in stream:\n%s", recorder.Body.String())
	}
	restored, ok := decodeAnthropicThinkingBlock(encrypted)
	if !ok || stringValue(restored["type"]) != "redacted_thinking" || stringValue(restored["data"]) != "OpaqueCipher==" {
		t.Fatalf("restored = %#v ok=%v", restored, ok)
	}
}

// A redacted_thinking block that ends an assistant turn is trimmed so the
// message stays valid for Anthropic. The ciphertext must not vanish with it:
// the turn is kept as an empty thinking placeholder rather than dropped.
func TestResponsesToClaudeTrailingRedactedThinkingKeepsTurn(t *testing.T) {
	item, _ := responsesReasoningItemFromAnthropicBlock("rs_1", map[string]any{"type": "redacted_thinking", "data": "OpaqueCipher=="})
	req := map[string]any{
		"model": "claude-sonnet-5",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "go"}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
			item,
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "continue"}}},
		},
	}
	claudeReq, _, err := responsesToClaudeRequestDirect(req, "claude-sonnet-5", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	messages := claudeMessagesFromAny(claudeReq["messages"])
	var placeholder bool
	for _, msg := range messages {
		if stringValue(msg["role"]) != "assistant" {
			continue
		}
		for _, raw := range msg["content"].([]any) {
			block, _ := raw.(map[string]any)
			if stringValue(block["type"]) == "thinking" {
				placeholder = true
			}
		}
	}
	if !placeholder {
		t.Fatalf("redacted turn was dropped entirely: %#v", claudeReq["messages"])
	}
	// And nothing trailing is a redacted block Anthropic would reject.
	for _, msg := range messages {
		if stringValue(msg["role"]) != "assistant" {
			continue
		}
		blocks := msg["content"].([]any)
		if len(blocks) > 0 {
			last, _ := blocks[len(blocks)-1].(map[string]any)
			if stringValue(last["type"]) == "redacted_thinking" {
				t.Fatalf("assistant message still ends with redacted_thinking: %#v", blocks)
			}
		}
	}
}

// Thinking-only turns with no redacted content are still dropped: there is
// nothing worth replaying and an empty thinking block would only confuse the
// model.
func TestResponsesToClaudeDropsPlainTrailingThinking(t *testing.T) {
	item, _ := responsesReasoningItemFromAnthropicBlock("rs_1", map[string]any{"type": "thinking", "thinking": "hmm", "signature": "s"})
	req := map[string]any{
		"model": "claude-sonnet-5",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "go"}}},
			item,
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "continue"}}},
		},
	}
	claudeReq, _, err := responsesToClaudeRequestDirect(req, "claude-sonnet-5", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, msg := range claudeMessagesFromAny(claudeReq["messages"]) {
		if stringValue(msg["role"]) != "assistant" {
			continue
		}
		if len(msg["content"].([]any)) != 0 {
			t.Fatalf("interrupted thinking turn should be dropped, got %#v", msg)
		}
	}
}
