package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// A finish/stop reason is the only signal a client has that the answer is
// whole. Copying a reason the target protocol does not know (or flattening a
// truncation to "stop"/"completed") makes a cut-short turn look finished: the
// agent stops asking, or an Anthropic request is rejected for an unknown
// stop_reason. These tables pin every terminal reason in every direction.

func TestMapOpenAIFinishReasonToClaude(t *testing.T) {
	cases := map[string]string{
		"":               "end_turn",
		"stop":           "end_turn",
		"length":         "max_tokens",
		"tool_calls":     "tool_use",
		"function_call":  "tool_use",
		"content_filter": "refusal",
		"pause_turn":     "pause_turn",
		// Provider-specific labels Anthropic has never heard of.
		"eos":       "end_turn",
		"end_turn":  "end_turn",
		"stop_now!": "end_turn",
	}
	for reason, want := range cases {
		if got := mapOpenAIFinishReasonToClaude(reason); got != want {
			t.Errorf("mapOpenAIFinishReasonToClaude(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestMapClaudeStopReasonToOpenAIFinish(t *testing.T) {
	cases := map[string]string{
		"":                              "stop",
		"end_turn":                      "stop",
		"stop_sequence":                 "stop",
		"pause_turn":                    "stop",
		"refusal":                       "stop",
		"max_tokens":                    "length",
		"model_context_window_exceeded": "length",
		"tool_use":                      "tool_calls",
		"unknown_upstream_value":        "stop",
	}
	for reason, want := range cases {
		if got := mapClaudeStopReasonToOpenAIFinish(reason); got != want {
			t.Errorf("mapClaudeStopReasonToOpenAIFinish(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestMapResponsesIncompleteToOpenAIFinish(t *testing.T) {
	cases := []struct{ status, reason, want string }{
		{"completed", "", ""},
		{"completed", "max_output_tokens", ""},
		{"incomplete", "max_output_tokens", "length"},
		{"incomplete", "content_filter", "content_filter"},
		{"incomplete", "", "length"},
	}
	for _, tc := range cases {
		if got := mapResponsesIncompleteToOpenAIFinish(tc.status, tc.reason); got != tc.want {
			t.Errorf("mapResponsesIncompleteToOpenAIFinish(%q, %q) = %q, want %q", tc.status, tc.reason, got, tc.want)
		}
	}
}

// Chat → Claude: a provider's own label must not reach Anthropic's stop_reason.
func TestOpenAIFinishReasonToClaudeResponse(t *testing.T) {
	body := []byte(`{"id":"x","object":"chat.completion","model":"m","created":1,` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"partial"},"finish_reason":"eos"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	out, _, err := openAIChatResponseToClaude(body, "m", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if got := stringValue(payload["stop_reason"]); got != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", got)
	}
}

// Responses → Chat, non-streaming: an incomplete turn must report "length".
func TestResponsesIncompleteReportsLengthInChatResponse(t *testing.T) {
	body := []byte(`{"id":"resp_1","object":"response","model":"m","status":"incomplete",` +
		`"incomplete_details":{"reason":"max_output_tokens"},` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"cut"}]}],` +
		`"usage":{"input_tokens":5,"output_tokens":9,"total_tokens":14}}`)
	out, _, err := responsesToOpenAIChatResponse(body, "m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	choices, _ := payload["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %#v", payload["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	if got := stringValue(choice["finish_reason"]); got != "length" {
		t.Fatalf("finish_reason = %q, want length", got)
	}
}

// Responses → Chat, streaming: the terminal event's status decides the
// finish_reason the client sees on the final chunk.
func TestResponsesIncompleteReportsLengthInChatStream(t *testing.T) {
	input := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}`,
		``,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"cut"}`,
		``,
		`data: {"type":"response.incomplete","response":{"id":"resp_1","model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"cut"}]}],"usage":{"input_tokens":5,"output_tokens":9}}}`,
		``,
	}, "\n")

	recorder := httptest.NewRecorder()
	usage, err := streamResponsesToOpenAIChatEvents(recorder, strings.NewReader(input), "m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 5 || usage.OutputTokens != 9 {
		t.Fatalf("usage = %+v", usage)
	}
	if got := lastChatFinishReason(t, recorder.Body.String()); got != "length" {
		t.Fatalf("finish_reason = %q, want length; body=%s", got, recorder.Body.String())
	}
}

// Chat → Responses, streaming: finish_reason "length" must close the stream with
// status incomplete instead of completed.
func TestChatLengthClosesResponsesStreamAsIncomplete(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"cut"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	recorder := httptest.NewRecorder()
	if _, err := streamOpenAIChatToResponsesEvents(recorder, strings.NewReader(input), "m"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	response := lastResponsesCompletedResponse(t, recorder.Body.String())
	if got := stringValue(response["status"]); got != "incomplete" {
		t.Fatalf("status = %q, want incomplete", got)
	}
	details, _ := response["incomplete_details"].(map[string]any)
	if got := stringValue(details["reason"]); got != "max_output_tokens" {
		t.Fatalf("incomplete_details.reason = %q, want max_output_tokens", got)
	}
}

// Chat → Responses, non-streaming: same rule for the buffered response.
func TestChatLengthMarksResponsesIncomplete(t *testing.T) {
	body := []byte(`{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"cut"},"finish_reason":"length"}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	out, _, err := openAIChatToResponsesResponse(body, "m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if got := stringValue(payload["status"]); got != "incomplete" {
		t.Fatalf("status = %q, want incomplete", got)
	}
	details, _ := payload["incomplete_details"].(map[string]any)
	if got := stringValue(details["reason"]); got != "max_output_tokens" {
		t.Fatalf("incomplete_details.reason = %q, want max_output_tokens", got)
	}
}

func lastChatFinishReason(t *testing.T, body string) string {
	t.Helper()
	finish := ""
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
		choices, _ := chunk["choices"].([]any)
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			if value := stringValue(choice["finish_reason"]); value != "" {
				finish = value
			}
		}
	}
	return finish
}

func lastResponsesCompletedResponse(t *testing.T, body string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			continue
		}
		if stringValue(event["type"]) != "response.completed" {
			continue
		}
		if response, ok := event["response"].(map[string]any); ok {
			found = response
		}
	}
	if found == nil {
		t.Fatalf("no response.completed event in %s", body)
	}
	return found
}

// Claude → Chat: Anthropic-only terminal reasons must not leak into Chat's
// finish_reason, where clients switch on a known set.
func TestClaudeStopReasonMapsToChatFinish(t *testing.T) {
	cases := map[string]string{
		"end_turn":                      "stop",
		"stop_sequence":                 "stop",
		"pause_turn":                    "stop",
		"refusal":                       "stop",
		"model_context_window_exceeded": "length",
		"max_tokens":                    "length",
		"tool_use":                      "tool_calls",
	}
	for reason, want := range cases {
		if got := mapClaudeStopReasonToOpenAI(reason); got != want {
			t.Errorf("mapClaudeStopReasonToOpenAI(%q) = %q, want %q", reason, got, want)
		}
	}
}
