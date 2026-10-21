package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamOpenAIChatToClaudeEventsWithEmptyToolCalls(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"cmb-1","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"finish_reason":""}]}`,
		``,
		`data: {"id":"cmb-1","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"你好","reasoning_content":"","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"finish_reason":""}]}`,
		``,
		`data: {"id":"cmb-1","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"！","reasoning_content":"","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"finish_reason":"stop"}],"usage":{"prompt_tokens":18395,"completion_tokens":8,"total_tokens":18403}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	recorder := httptest.NewRecorder()
	usage, err := streamOpenAIChatToClaudeEvents(recorder, strings.NewReader(input), "deepseek-v4.1-flash", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 18395 || usage.OutputTokens != 8 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	body := recorder.Body.String()
	for _, expected := range []string{
		"event: content_block_start",
		`"text":"你好"`,
		`"text":"！"`,
		"event: content_block_stop",
		"event: message_stop",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected stream to contain %q, got:\n%s", expected, body)
		}
	}
	if strings.Contains(body, "tool_use") {
		t.Fatalf("empty tool_calls produced a tool_use block:\n%s", body)
	}
}

func TestStreamOpenAIChatToClaudeEvents(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chatcmpl-test","model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"2"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-test","model":"deepseek-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":13,"completion_tokens":1,"total_tokens":14}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	recorder := httptest.NewRecorder()
	usage, err := streamOpenAIChatToClaudeEvents(recorder, strings.NewReader(input), "deepseek-chat", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 13 || usage.OutputTokens != 1 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	body := recorder.Body.String()
	for _, expected := range []string{
		"event: message_start",
		"event: content_block_delta",
		`"text":"2"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected stream to contain %q, got:\n%s", expected, body)
		}
	}
}
