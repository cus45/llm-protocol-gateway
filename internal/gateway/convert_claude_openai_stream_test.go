package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamClaudeToOpenAIPreservesToolArgumentSpaces guards against a
// regression where each Claude input_json_delta fragment was passed through
// strings.TrimSpace, dropping spaces that fell on a fragment boundary and
// corrupting reassembled tool-call arguments (e.g. "git status --short"
// became "git status--short").
func TestStreamClaudeToOpenAIPreservesToolArgumentSpaces(t *testing.T) {
	// The command string is deliberately split so a boundary lands right on a
	// space: fragment 1 ends with "git status" and fragment 2 starts with
	// " --short".
	input := strings.Join([]string{
		`data: {"type":"message_start","message":{"model":"claude-sonnet-4"}}`,
		``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Shell"}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"git status"}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":" --short\"}"}}`,
		``,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":5,"output_tokens":7}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	recorder := httptest.NewRecorder()
	if _, err := streamClaudeToOpenAIChatEvents(recorder, strings.NewReader(input), "claude-sonnet-4", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Reassemble the streamed tool-call arguments across chunks.
	var args strings.Builder
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			for _, call := range choice.Delta.ToolCalls {
				args.WriteString(call.Function.Arguments)
			}
		}
	}

	got := args.String()
	const want = `{"command":"git status --short"}`
	if got != want {
		t.Fatalf("tool arguments corrupted:\n got=%q\nwant=%q", got, want)
	}
}

// TestStreamClaudeToOpenAIUsageKeepsPromptTokens emulates a real Anthropic
// stream, which carries the whole prompt and cache accounting in message_start
// and only output_tokens in message_delta. Before this fix the message_start
// usage was ignored and message_delta's usage replaced the totals, so every
// streamed reply reported prompt_tokens 0 — wrong for key quotas, cost and the
// usage page.
func TestStreamClaudeToOpenAIUsageKeepsPromptTokens(t *testing.T) {
	input := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4","usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":5,"output_tokens":1}}}`,
		``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	recorder := httptest.NewRecorder()
	usage, err := streamClaudeToOpenAIChatEvents(recorder, strings.NewReader(input), "claude-sonnet-4", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 115 || usage.OutputTokens != 20 || usage.CacheTokens != 100 || usage.CacheWriteTokens != 5 {
		t.Fatalf("usage = %+v, want input=115 output=20 cache=100 cache_write=5", usage)
	}

	// The final chunk must carry the same numbers, not just the struct.
	finalUsage := lastChunkUsage(t, recorder.Body.String())
	if finalUsage == nil {
		t.Fatal("final chunk carried no usage")
	}
	if got := int64Value(finalUsage["prompt_tokens"]); got != 115 {
		t.Fatalf("prompt_tokens = %d, want 115", got)
	}
	if got := int64Value(finalUsage["completion_tokens"]); got != 20 {
		t.Fatalf("completion_tokens = %d, want 20", got)
	}
	details, _ := finalUsage["prompt_tokens_details"].(map[string]any)
	if got := int64Value(details["cached_tokens"]); got != 100 {
		t.Fatalf("cached_tokens = %d, want 100", got)
	}
	if got := int64Value(finalUsage["cache_creation_input_tokens"]); got != 5 {
		t.Fatalf("cache_creation_input_tokens = %d, want 5", got)
	}
}

// TestStreamClaudeToOpenAIUsageMergesAcrossDeltas covers an upstream that
// repeats the prompt fields on every message_delta: they are totals, so a
// repeat must not be added on top.
func TestStreamClaudeToOpenAIUsageMergesAcrossDeltas(t *testing.T) {
	input := strings.Join([]string{
		`data: {"type":"message_start","message":{"model":"claude-sonnet-4","usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":5,"output_tokens":1}}}`,
		``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":5,"output_tokens":12}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":5,"output_tokens":20}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	recorder := httptest.NewRecorder()
	usage, err := streamClaudeToOpenAIChatEvents(recorder, strings.NewReader(input), "claude-sonnet-4", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 115 || usage.OutputTokens != 20 {
		t.Fatalf("usage = %+v, want input=115 output=20", usage)
	}
}

func lastChunkUsage(t *testing.T, body string) map[string]any {
	t.Helper()
	var last map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if usageValue, ok := chunk["usage"].(map[string]any); ok {
			last = usageValue
		}
	}
	return last
}

func int64Value(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	}
	return 0
}
