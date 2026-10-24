package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWritePassThroughResponseStreamsAndTees(t *testing.T) {
	upstreamBody := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}
	recorder := httptest.NewRecorder()
	status, _, logBody, err := writePassThroughResponse(recorder, response, true, usageStreamOpenAI, ParseOpenAIUsage)
	if err != nil {
		t.Fatalf("writePassThroughResponse: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	if got := recorder.Body.String(); got != upstreamBody {
		t.Fatalf("client body mismatch: %q", got)
	}
	// The client gets every byte; the log copy may be a prefix of it (a stream
	// on the wire is not held for logging), never a different body.
	if got := string(logBody); !strings.HasPrefix(upstreamBody, got) {
		t.Fatalf("tee body is not a prefix of the stream: %q", got)
	}
	if recorder.Header().Get("Content-Length") != "" {
		t.Fatalf("stream response should not set Content-Length")
	}
}

func TestWritePassThroughResponseBuffersErrors(t *testing.T) {
	upstreamBody := `{"error":{"message":"boom"}}`
	response := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}
	recorder := httptest.NewRecorder()
	status, _, logBody, err := writePassThroughResponse(recorder, response, true, usageStreamOpenAI, ParseOpenAIUsage)
	if err != nil {
		t.Fatalf("writePassThroughResponse: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d", status)
	}
	if string(logBody) != upstreamBody || recorder.Body.String() != upstreamBody {
		t.Fatalf("error body not preserved")
	}
}

func TestRequestBodyWantsStream(t *testing.T) {
	if !requestBodyWantsStream([]byte(`{"stream":true}`)) {
		t.Fatal("expected stream true")
	}
	if requestBodyWantsStream([]byte(`{"stream":false}`)) {
		t.Fatal("expected stream false")
	}
}

// The logged body is capped at passThroughLogBufferMax, but daily traffic must
// reflect the true streamed size, so limitedBuffer counts every byte written.
func TestLimitedBufferCountsBeyondCap(t *testing.T) {
	buf := &limitedBuffer{max: 16}
	chunk := make([]byte, 10)
	for i := 0; i < 5; i++ {
		if _, err := buf.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if len(buf.Bytes()) != 16 {
		t.Fatalf("buffered bytes should be capped at 16, got %d", len(buf.Bytes()))
	}
	if buf.Total() != 50 {
		t.Fatalf("total should count every byte written, got %d", buf.Total())
	}
}

func TestLimitedBufferTotalWithZeroCap(t *testing.T) {
	buf := &limitedBuffer{max: 0}
	if _, err := buf.Write(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if len(buf.Bytes()) != 0 {
		t.Fatalf("expected nothing buffered, got %d", len(buf.Bytes()))
	}
	if buf.Total() != 32 {
		t.Fatalf("total must still count, got %d", buf.Total())
	}
}

// recordStreamedTxBytes is called on paths where timing may be absent.
func TestRequestTimingStreamedTxNilSafe(t *testing.T) {
	var timing *requestTiming
	timing.recordStreamedTxBytes(123) // must not panic
	if got := timing.streamedTx(); got != 0 {
		t.Fatalf("nil timing should report 0, got %d", got)
	}
	real := newRequestTiming(time.Now())
	real.recordStreamedTxBytes(4096)
	if got := real.streamedTx(); got != 4096 {
		t.Fatalf("expected 4096, got %d", got)
	}
	// Zero/negative writes must not clobber a recorded value.
	real.recordStreamedTxBytes(0)
	if got := real.streamedTx(); got != 4096 {
		t.Fatalf("zero must not overwrite, got %d", got)
	}
}

// The Responses closing frame repeats the stream's id, model and next sequence
// number: an SDK keyed on sequence_number rejects a repeat or a skip.
func TestResponsesClosingFrameRepeatsStreamIdentity(t *testing.T) {
	stream := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"model\":\"gpt-5\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"delta\":\"hi\"}\n\n")
	recorder := httptest.NewRecorder()
	responsesStreamInterrupter{}.closeInterruptedStream(recorder, cutFromBody(stream))
	body := recorder.Body.String()
	var event map[string]any
	data := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body), "event: response.failed"))
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(data, "data: "))), &event); err != nil {
		t.Fatalf("closing frame is not JSON: %v (%q)", err, body)
	}
	if event["type"] != "response.failed" {
		t.Fatalf("unexpected type %v", event["type"])
	}
	if event["sequence_number"] != float64(2) {
		t.Fatalf("sequence_number repeats or skips: %v", event["sequence_number"])
	}
	response, _ := event["response"].(map[string]any)
	if response["id"] != "resp_1" || response["model"] != "gpt-5" || response["status"] != "failed" {
		t.Fatalf("closing frame lost the stream identity: %v", response)
	}
}

// A long stream is judged by every byte it sent, not by the capped log copy: a
// complete answer larger than the cap must not be reported as an interruption,
// and must report the usage it carried.
func TestWritePassThroughResponseJudgesLongStreamWhole(t *testing.T) {
	var upstream strings.Builder
	chunk := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + strings.Repeat("x", 1000) + "\"}}]}\n\n"
	for upstream.Len() < 3*passThroughLogBufferMax {
		upstream.WriteString(chunk)
	}
	upstream.WriteString("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":22,\"total_tokens\":33}}\n\n")
	upstream.WriteString("data: [DONE]\n\n")
	body := upstream.String()
	response := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: -1,
		Header:        http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:          io.NopCloser(strings.NewReader(body)),
	}
	recorder := httptest.NewRecorder()
	_, usage, logBody, err := writePassThroughResponse(recorder, response, true, usageStreamOpenAI, ParseOpenAIUsage)
	if err != nil {
		t.Fatalf("a stream that ended with [DONE] is complete, got %v", err)
	}
	if got := recorder.Body.String(); got != body {
		t.Fatalf("client body lost bytes: %d of %d", len(got), len(body))
	}
	if len(logBody) > passThroughLogBufferMax {
		t.Fatalf("log body must stay capped, got %d", len(logBody))
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 22 {
		t.Fatalf("usage beyond the cap was lost: %+v", usage)
	}
}

// A stream cut after part of the answer reached the client is still an
// interruption, but the tokens it reported before the cut were spent all the
// same and are reported with it.
func TestWritePassThroughResponseKeepsUsageOfCutStream(t *testing.T) {
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\n"
	response := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: -1,
		Header:        http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:          io.NopCloser(strings.NewReader(body)),
	}
	recorder := httptest.NewRecorder()
	_, usage, _, err := writePassThroughResponse(recorder, response, true, usageStreamOpenAI, ParseOpenAIUsage, openAIChatStreamInterrupter{model: "m"})
	if !errors.Is(err, errUpstreamStreamInterrupted) {
		t.Fatalf("expected an interruption, got %v", err)
	}
	if usage.InputTokens != 7 || usage.OutputTokens != 3 {
		t.Fatalf("usage reported before the cut was dropped: %+v", usage)
	}
	// A cut turn is told as an error, not as a finished answer (magpie's
	// streamFailure): a client that sees a finish reason plus [DONE] keeps half a
	// sentence as the whole of it.
	out := recorder.Body.String()
	if !strings.Contains(out, `"type":"api_error"`) || strings.Contains(out, "finish_reason") {
		t.Fatalf("cut turn was not told as an error: %q", out)
	}
}

// A Responses stream longer than the log cap still closes with its own id and
// the next sequence number: the identity is read from the stream, not the copy.
func TestWritePassThroughResponseClosesLongStreamWithItsIdentity(t *testing.T) {
	var upstream strings.Builder
	upstream.WriteString("event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_long\",\"model\":\"gpt-5\"}}\n\n")
	for seq := 1; upstream.Len() < 3*passThroughLogBufferMax; seq++ {
		upstream.WriteString("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":" + strconv.Itoa(seq) + ",\"delta\":\"" + strings.Repeat("y", 1000) + "\"}\n\n")
	}
	body := upstream.String()
	response := &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: -1,
		Header:        http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:          io.NopCloser(strings.NewReader(body)),
	}
	recorder := httptest.NewRecorder()
	if _, _, _, err := writePassThroughResponse(recorder, response, true, usageStreamResponses, ParseResponsesUsage, responsesStreamInterrupter{}); !errors.Is(err, errUpstreamStreamInterrupted) {
		t.Fatalf("expected an interruption, got %v", err)
	}
	out := recorder.Body.String()
	if !strings.HasPrefix(out, body) {
		t.Fatalf("the closing frames must follow the stream unchanged")
	}
	closing := strings.TrimPrefix(out, body)
	if !strings.Contains(closing, `"id":"resp_long"`) || !strings.Contains(closing, `"model":"gpt-5"`) {
		t.Fatalf("closing frame lost the stream identity: %q", closing)
	}
	var event struct {
		Sequence int `json:"sequence_number"`
	}
	if err := json.Unmarshal([]byte(closing[strings.Index(closing, "{"):strings.LastIndex(closing, "}")+1]), &event); err != nil {
		t.Fatalf("closing frame is not JSON: %v (%q)", err, closing)
	}
	if event.Sequence < 700 {
		t.Fatalf("closing frame ignored the stream's own sequence numbers: %d", event.Sequence)
	}
}
