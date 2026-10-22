package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Claude → Responses streaming: message_start holds the prompt and cache
// counts, message_delta only the output count. Replacing the usage on
// message_delta reported input_tokens 0 on every streamed turn.
func TestClaudeStreamToResponsesKeepsInputTokens(t *testing.T) {
	input := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4","usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":5,"output_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	recorder := httptest.NewRecorder()
	usage, err := streamClaudeToResponsesEventsDirect(recorder, strings.NewReader(input), "claude-sonnet-4", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 115 || usage.OutputTokens != 20 {
		t.Fatalf("returned usage = %+v, want input=115 output=20", usage)
	}

	response := lastResponsesCompletedResponse(t, recorder.Body.String())
	reported, _ := response["usage"].(map[string]any)
	if got := int64FromAny(reported["input_tokens"]); got != 115 {
		t.Fatalf("input_tokens = %d, want 115; usage=%v", got, reported)
	}
	if got := int64FromAny(reported["output_tokens"]); got != 20 {
		t.Fatalf("output_tokens = %d, want 20", got)
	}
	if got := int64FromAny(reported["total_tokens"]); got != 135 {
		t.Fatalf("total_tokens = %d, want 135", got)
	}
}
