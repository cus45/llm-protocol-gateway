package gateway

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// A converter's upstream that ends at a clean EOF without its closing frame was
// cut: the client must be told so in its own protocol rather than handed a half
// answer that looks finished.
func TestConvertersTreatCleanEOFWithoutClosingFrameAsInterruption(t *testing.T) {
	claudeWithoutStop := strings.Join([]string{
		`data: {"type":"message_start","message":{"model":"claude-sonnet-4","usage":{"input_tokens":10,"output_tokens":1}}}`,
		``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half a sent"}}`,
		``,
	}, "\n")
	chatWithoutDone := strings.Join([]string{
		`data: {"id":"c1","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":"half a sent"}}]}`,
		``,
	}, "\n")
	responsesWithoutTerminal := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"gpt-5"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","sequence_number":1,"delta":"half a sent"}`,
		``,
	}, "\n")

	cases := []struct {
		name    string
		input   string
		convert func(*httptest.ResponseRecorder, string) error
		closing string
	}{
		{
			name:  "claude to chat",
			input: claudeWithoutStop,
			convert: func(recorder *httptest.ResponseRecorder, input string) error {
				_, err := streamClaudeToOpenAIChatEvents(recorder, strings.NewReader(input), "claude-sonnet-4", nil)
				return err
			},
			closing: `"type":"api_error"`,
		},
		{
			name:  "chat to claude",
			input: chatWithoutDone,
			convert: func(recorder *httptest.ResponseRecorder, input string) error {
				_, err := streamOpenAIChatToClaudeEvents(recorder, strings.NewReader(input), "claude-sonnet-4", nil)
				return err
			},
			closing: "message_stop",
		},
		{
			name:  "claude to responses",
			input: claudeWithoutStop,
			convert: func(recorder *httptest.ResponseRecorder, input string) error {
				_, err := streamClaudeToResponsesEventsDirect(recorder, strings.NewReader(input), "gpt-5", nil)
				return err
			},
			closing: "response.failed",
		},
		{
			name:  "responses to claude",
			input: responsesWithoutTerminal,
			convert: func(recorder *httptest.ResponseRecorder, input string) error {
				_, err := streamResponsesToClaudeEventsDirect(recorder, strings.NewReader(input), "claude-sonnet-4")
				return err
			},
			closing: "message_stop",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			err := testCase.convert(recorder, testCase.input)
			if !isUpstreamStreamInterruption(err) {
				t.Fatalf("a body cut at EOF without its closing frame must be an interruption, got %v", err)
			}
			if body := recorder.Body.String(); !strings.Contains(body, testCase.closing) {
				t.Fatalf("the client was not told the turn stopped early (want %q):\n%s", testCase.closing, body)
			}
		})
	}
}

// A whole stream, closing frame included, is not an interruption even though
// its body ends in the same clean EOF.
func TestConverterCleanEOFWithClosingFrameIsComplete(t *testing.T) {
	complete := strings.Join([]string{
		`data: {"type":"message_start","message":{"model":"claude-sonnet-4","usage":{"input_tokens":10,"output_tokens":1}}}`,
		``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"whole"}}`,
		``,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":3}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	recorder := httptest.NewRecorder()
	if _, err := streamClaudeToOpenAIChatEvents(recorder, strings.NewReader(complete), "claude-sonnet-4", nil); err != nil {
		t.Fatalf("a complete stream must convert cleanly: %v", err)
	}
	if !strings.Contains(recorder.Body.String(), `"finish_reason":"stop"`) {
		t.Fatalf("a complete stream must finish with stop:\n%s", recorder.Body.String())
	}
}

// cleanEOFInterruption tells the three ways a read can end apart.
func TestCleanEOFInterruption(t *testing.T) {
	cases := []struct {
		name   string
		reader countingReader
		closed bool
		want   bool
	}{
		{name: "nothing read", reader: countingReader{}, want: false},
		{name: "complete", reader: countingReader{n: 100, err: io.EOF}, closed: true, want: false},
		{name: "cut at a clean EOF", reader: countingReader{n: 100, err: io.EOF}, want: true},
		{name: "transport error", reader: countingReader{n: 100, err: io.ErrUnexpectedEOF}, want: false},
		{name: "empty body", reader: countingReader{err: io.EOF}, want: false},
	}
	for _, testCase := range cases {
		if got := cleanEOFInterruption(testCase.reader, testCase.closed); got != testCase.want {
			t.Fatalf("%s: got %v, want %v", testCase.name, got, testCase.want)
		}
	}
	if !errors.Is(errStreamWithoutClosingFrame, errStreamWithoutClosingFrame) {
		t.Fatal("the stand-in error is not comparable")
	}
}

// The terminal event of a turn the vendor stopped at max_output_tokens is
// response.incomplete, and its usage is the only place the answer's token
// count is reported: it must be folded, not dropped for lacking ".completed".
func TestStreamUsageReaderFoldsIncompleteTerminalUsage(t *testing.T) {
	reader := newStreamUsageReader(usageStreamResponses)
	stream := `event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"gpt-5"}}

event: response.incomplete
data: {"type":"response.incomplete","sequence_number":1,"response":{"id":"resp_1","model":"gpt-5","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1234,"output_tokens":567,"total_tokens":1801}}}

`
	if _, err := reader.Write([]byte(stream)); err != nil {
		t.Fatalf("write: %v", err)
	}
	usage := reader.final(ParseResponsesUsage, []byte(stream))
	if usage.InputTokens != 1234 || usage.OutputTokens != 567 {
		t.Fatalf("the incomplete terminal event's usage was dropped: %+v", usage)
	}
}

// The Chat→Responses leg was the last one to be told its closing frame: a Chat
// body that stops at EOF without [DONE] used to reach the client as a completed
// response.
func TestChatToResponsesTreatsCleanEOFWithoutDoneAsInterruption(t *testing.T) {
	withoutDone := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"half a sent"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"ence"}}]}`,
		"",
	}, "\n")
	recorder := httptest.NewRecorder()
	_, err := streamOpenAIChatToResponsesEventsWithOptions(recorder, strings.NewReader(withoutDone), "gpt-5", StandardChatToResponsesStreamOptions())
	if !isUpstreamStreamInterruption(err) {
		t.Fatalf("a Chat body cut at EOF without [DONE] must be an interruption, got %v", err)
	}
	if body := recorder.Body.String(); strings.Contains(body, `"type":"response.completed"`) || !strings.Contains(body, "response.failed") {
		t.Fatalf("a cut Chat-to-Responses turn must not complete:\n%s", body)
	}
}
