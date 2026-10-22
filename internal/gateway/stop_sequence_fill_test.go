package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMatchedStopSequence(t *testing.T) {
	cases := []struct {
		text      string
		sequences []string
		want      string
	}{
		{"1\n2\n", []string{"\n", "END"}, "\n"},
		{"hello END", []string{"STOP", "END"}, "END"},
		{"hello", []string{"END"}, ""},
		{"anything", []string{""}, ""},
		{"", []string{"END"}, ""},
	}
	for _, tc := range cases {
		if got := matchedStopSequence(tc.text, tc.sequences); got != tc.want {
			t.Errorf("matchedStopSequence(%q, %v) = %q, want %q", tc.text, tc.sequences, got, tc.want)
		}
	}
}

func TestRememberConvertedStopSequences(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	same, none := rememberConvertedStopSequences(r, nil)
	if len(none) != 0 || same != r {
		t.Fatalf("no sequences should leave the request untouched, got %v", none)
	}

	withSeq, sequences := rememberConvertedStopSequences(r, []any{"END", "STOP"})
	if len(sequences) != 2 {
		t.Fatalf("sequences = %v", sequences)
	}
	if got := convertedStopSequences(withSeq); len(got) != 2 || got[1] != "STOP" {
		t.Fatalf("context lost the sequences: %v", got)
	}
	// The original request must stay untouched for the passthrough callers.
	if got := convertedStopSequences(r); len(got) != 0 {
		t.Fatalf("original request gained sequences: %v", got)
	}
}

// TestChatToClaudeResponseReportsStopSequence: Chat 只有 finish_reason=stop，
// 但正文以客户端停止序列结尾时，必须报 stop_reason=stop_sequence + 命中的序列。
func TestChatToClaudeResponseReportsStopSequence(t *testing.T) {
	chatResp := `{"id":"x","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"1\n2\n3\n"}}],"usage":{"prompt_tokens":5,"completion_tokens":6,"total_tokens":11}}`
	body, _, err := openAIChatResponseToClaude([]byte(chatResp), "glm-5.3", nil, "\n")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if payload["stop_reason"] != "stop_sequence" {
		t.Fatalf("stop_reason = %#v, want stop_sequence", payload["stop_reason"])
	}
	if payload["stop_sequence"] != "\n" {
		t.Fatalf("stop_sequence = %#v, want newline", payload["stop_sequence"])
	}

	// Without the client's sequences the ordinary mapping stands.
	body, _, err = openAIChatResponseToClaude([]byte(chatResp), "glm-5.3", nil)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	_ = json.Unmarshal(body, &payload)
	if payload["stop_reason"] != "end_turn" || payload["stop_sequence"] != nil {
		t.Fatalf("without sequences got %#v / %#v", payload["stop_reason"], payload["stop_sequence"])
	}
}

// TestClaudeStopSequenceToChatFinish: Claude 报 stop_sequence 时 Chat 只能报
// stop（否则客户端看到 tool_calls/长度以外的未知原因）。
func TestClaudeStopSequenceToChatFinish(t *testing.T) {
	claudeResp := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"done END"}],"stop_reason":"stop_sequence","stop_sequence":"END","usage":{"input_tokens":3,"output_tokens":2}}`
	body, _, err := claudeResponseToOpenAIChat([]byte(claudeResp), "m", nil, "END")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("not json: %v", err)
	}
	choices, _ := payload["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %#v, want stop", choice["finish_reason"])
	}
}

// TestFillNonStreamStopSequenceChat: Chat→Responses 的非流式答案，正文以停止
// 序列结尾时补上 stop_sequence。
func TestFillNonStreamStopSequenceChat(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"a STOP"}}]}`)
	out := fillNonStreamStopSequence(body, []string{"STOP"})
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if payload["stop_sequence"] != "STOP" {
		t.Fatalf("stop_sequence = %#v", payload["stop_sequence"])
	}
	if out := fillNonStreamStopSequence(body, nil); string(out) != string(body) {
		t.Fatalf("body changed without sequences: %s", out)
	}
	if out := fillNonStreamStopSequence(body, []string{"NOPE"}); string(out) != string(body) {
		t.Fatalf("body changed on a non-match: %s", out)
	}
}

func TestTailForStopSequences(t *testing.T) {
	if got := tailForStopSequences("abc", []string{"END"}); got != "abc" {
		t.Fatalf("short text changed: %q", got)
	}
	if got := tailForStopSequences("123456789", []string{"END"}); got != "56789" {
		t.Fatalf("tail = %q, want 56789 (the sequence plus two runes of context)", got)
	}
	// Multi-byte text stays on a rune boundary.
	if got := tailForStopSequences("一二三四五", []string{"\x0a"}); !utf8.ValidString(got) || got != "三四五" {
		t.Fatalf("tail = %q", got)
	}
}

// TestChatToClaudeStreamReportsStopSequence: 流式 Chat→Claude 的正文以客户端
// 停止序列结尾时，message_delta 应报 stop_sequence 与命中的序列。
func TestChatToClaudeStreamReportsStopSequence(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm","choices":[{"index":0,"delta":{"role":"assistant","content":"1\n2\n6"},"finish_reason":null}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	rec := httptest.NewRecorder()
	if _, err := streamOpenAIChatToClaudeEvents(rec, strings.NewReader(stream), "glm", nil, "\x0a6"); err != nil {
		t.Fatalf("stream: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"stop_reason":"stop_sequence"`) {
		t.Fatalf("stream did not report stop_sequence:\n%s", out)
	}
	if !strings.Contains(out, `"stop_sequence":"\n6"`) {
		t.Fatalf("stream did not report the matched sequence:\n%s", out)
	}

	// Without the client's sequences the ordinary end_turn stands.
	rec = httptest.NewRecorder()
	if _, err := streamOpenAIChatToClaudeEvents(rec, strings.NewReader(stream), "glm", nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if strings.Contains(rec.Body.String(), "stop_sequence\":\"") {
		t.Fatalf("unexpected sequence without client sequences:\n%s", rec.Body.String())
	}
}
