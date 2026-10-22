package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// Responses reasoning must survive the hop to Chat as reasoning_content: it is
// what the client replays on the next turn. It must never appear as content,
// or a tool client mistakes the model's thinking for the answer.

func TestResponsesToChatKeepsReasoningContent(t *testing.T) {
	body := []byte(`{"id":"resp_1","object":"response","model":"m","status":"completed","output":[` +
		`{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"Plan the edit."}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}]}`)
	out, _, err := responsesToOpenAIChatResponse(body, "m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	choices, _ := payload["choices"].([]any)
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if got := stringValue(message["reasoning_content"]); got != "Plan the edit." {
		t.Fatalf("reasoning_content = %q, want %q", got, "Plan the edit.")
	}
	if got := stringValue(message["content"]); got != "Done." {
		t.Fatalf("content = %q, want Done.", got)
	}
}

func TestResponsesToChatReasoningTextShape(t *testing.T) {
	// The other upstream shape: reasoning_text content parts instead of summary.
	body := []byte(`{"id":"resp_1","object":"response","model":"m","status":"completed","output":[` +
		`{"type":"reasoning","content":[{"type":"reasoning_text","text":"think "},{"type":"reasoning_text","text":"twice"}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	out, _, err := responsesToOpenAIChatResponse(body, "m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	choices, _ := payload["choices"].([]any)
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if got := stringValue(message["reasoning_content"]); got != "think twice" {
		t.Fatalf("reasoning_content = %q, want %q", got, "think twice")
	}
}

func TestResponsesStreamToChatEmitsReasoningDeltas(t *testing.T) {
	input := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}`,
		``,
		`data: {"type":"response.reasoning_summary_text.delta","exclude":false,"item_id":"rs_1","summary_index":0,"delta":"Plan the "}`,
		``,
		`data: {"type":"response.reasoning_summary_text.delta","exclude":false,"item_id":"rs_1","summary_index":0,"delta":"edit."}`,
		``,
		`data: {"type":"response.reasoning_summary_text.done","exclude":false,"item_id":"rs_1","summary_index":0,"text":"Plan the edit."}`,
		``,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"Done."}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"Plan the edit."}]},{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Done."}]}],"usage":{"input_tokens":5,"output_tokens":3}}}`,
		``,
	}, "\n")

	recorder := httptest.NewRecorder()
	if _, err := streamResponsesToOpenAIChatEvents(recorder, strings.NewReader(input), "m"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	reasoning, content := collectChatText(t, recorder.Body.String())
	if reasoning != "Plan the edit." {
		t.Fatalf("reasoning_content = %q, want %q", reasoning, "Plan the edit.")
	}
	if content != "Done." {
		t.Fatalf("content = %q, want Done. (reasoning must not leak into content)", content)
	}
}

// An upstream that only puts reasoning on the terminal event still has to
// deliver it.
func TestResponsesStreamToChatDeliversTerminalOnlyReasoning(t *testing.T) {
	input := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}`,
		``,
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"Done."}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"Terminal thought"}]},{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Done."}]}],"usage":{"input_tokens":5,"output_tokens":3}}}`,
		``,
	}, "\n")

	recorder := httptest.NewRecorder()
	if _, err := streamResponsesToOpenAIChatEvents(recorder, strings.NewReader(input), "m"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	reasoning, content := collectChatText(t, recorder.Body.String())
	if reasoning != "Terminal thought" {
		t.Fatalf("reasoning_content = %q", reasoning)
	}
	if content != "Done." {
		t.Fatalf("content = %q", content)
	}
}

func collectChatText(t *testing.T, body string) (reasoning, content string) {
	t.Helper()
	var reasoningBuf, contentBuf strings.Builder
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
			delta, _ := choice["delta"].(map[string]any)
			reasoningBuf.WriteString(stringValue(delta["reasoning_content"]))
			contentBuf.WriteString(stringValue(delta["content"]))
		}
	}
	return reasoningBuf.String(), contentBuf.String()
}
