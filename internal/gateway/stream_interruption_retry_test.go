package gateway

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func TestIsUpstreamStreamInterruption(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unexpected eof", fmt.Errorf("openai stream: %w", io.ErrUnexpectedEOF), true},
		{"wrapped eof", io.EOF, true},
		{"connection reset", errors.New("read tcp 10.0.0.1:443: read: connection reset by peer"), true},
		{"remote disconnect", errors.New("http2: server sent GOAWAY"), true},
		{"empty stream", fmt.Errorf("openai stream ended without any chunks"), true},
		{"thinking only", errThinkingOnlyEmptyResponse, true},
		// A converter-side failure is not an upstream interruption: the next
		// provider would fail the same way.
		{"converter bug", errors.New("json: cannot unmarshal array into Go value"), false},
	}
	for _, tc := range cases {
		if got := isUpstreamStreamInterruption(tc.err); got != tc.want {
			t.Fatalf("%s: err=%v got=%v want=%v", tc.name, tc.err, got, tc.want)
		}
	}
	if !isUpstreamStreamInterruptedResponse(&http.Response{}, false) {
		t.Fatal("a nil body means the upstream never finished sending")
	}
}

// A stream that breaks after the client already saw text can still be handed to
// the backup provider when the attempt was buffered: nothing was committed, so
// finishConvertedProxy must report it retryable instead of ending the turn.
func TestFinishConvertedProxyMidStreamDropIsRetryableWhenDeferred(t *testing.T) {
	s := &Server{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}
	rec := httptest.NewRecorder()
	fw := newFailoverResponseWriter(rec, true)
	streamConvert := func(w http.ResponseWriter, reader io.Reader, model string) (TokenUsage, error) {
		if _, err := w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")); err != nil {
			return TokenUsage{}, err
		}
		return TokenUsage{}, newUpstreamStreamInterruption(io.ErrUnexpectedEOF, 64)
	}
	status, _, _, err, retryable := s.finishConvertedProxy(fw, resp, "glm-4.5", true, nil, streamConvert)
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	if err == nil || !retryable {
		t.Fatalf("want retryable interruption, got err=%v retryable=%v", err, retryable)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("client should not see the interrupted attempt, got %q", rec.Body.String())
	}
}

// A request with no provider chain never gets a failover buffer; the stream
// goes straight to the client, so an interruption simply ends the turn.
func TestFinishConvertedProxyMidStreamDropEndsStreamWithoutFailoverWriter(t *testing.T) {
	s := &Server{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}
	rec := httptest.NewRecorder()
	streamConvert := func(w http.ResponseWriter, reader io.Reader, model string) (TokenUsage, error) {
		if _, err := w.Write([]byte("data: partial" + "\n\n")); err != nil {
			return TokenUsage{}, err
		}
		return TokenUsage{}, newUpstreamStreamInterruption(io.ErrUnexpectedEOF, 64)
	}
	if _, _, _, err, retryable := s.finishConvertedProxy(rec, resp, "glm-4.5", true, nil, streamConvert); retryable {
		t.Fatalf("unbuffered stream must not be retried, err=%v", err)
	}
	if !strings.Contains(rec.Body.String(), "partial") {
		t.Fatalf("client body=%q", rec.Body.String())
	}
}

// The last attempt of the loop has no backup left, but it is still buffered:
// nothing reached the client, so the loop decides whether the truncated turn is
// worth one more try on the same account. The converter sent nothing itself, so
// a retry cannot replay a half answer the client already saw; when the loop does
// stop, the turn is written out as it stands instead of vanishing.
func TestFinishConvertedProxyMidStreamDropRetryableOnLastAttempt(t *testing.T) {
	s := &Server{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}
	rec := httptest.NewRecorder()
	fw := newFailoverResponseWriter(rec, false)
	streamConvert := func(w http.ResponseWriter, reader io.Reader, model string) (TokenUsage, error) {
		if _, err := w.Write([]byte("data: partial" + "\n\n")); err != nil {
			return TokenUsage{}, err
		}
		return TokenUsage{}, newUpstreamStreamInterruption(io.ErrUnexpectedEOF, 64)
	}
	status, _, _, err, retryable := s.finishConvertedProxy(fw, resp, "glm-4.5", true, nil, streamConvert)
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	if err == nil || !retryable {
		t.Fatalf("want retryable interruption, got err=%v retryable=%v", err, retryable)
	}
	if !strings.Contains(rec.Body.String(), "partial") {
		t.Fatalf("the half answer must not vanish when nothing is left to retry: %q", rec.Body.String())
	}
	if strings.Count(rec.Body.String(), "partial") != 1 {
		t.Fatalf("the half answer was replayed: %q", rec.Body.String())
	}
}

// Same-provider empty-stream retry is still bounded; an interruption that
// dropped headers only is retryable, but a converter bug is not.
func TestFinishConvertedProxyInterruptionClassification(t *testing.T) {
	s := &Server{}
	run := func(streamErr error) (bool, string) {
		resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}
		rec := httptest.NewRecorder()
		streamConvert := func(w http.ResponseWriter, reader io.Reader, model string) (TokenUsage, error) {
			return TokenUsage{}, streamErr
		}
		_, _, _, _, retryable := s.finishConvertedProxy(rec, resp, "m", true, nil, streamConvert)
		return retryable, rec.Body.String()
	}
	if retryable, _ := run(newUpstreamStreamInterruption(io.ErrUnexpectedEOF, 0)); !retryable {
		t.Fatal("headers-only interruption should be retryable")
	}
	if retryable, _ := run(errors.New("json: cannot unmarshal array")); retryable {
		t.Fatal("a converter bug must surface, not retry")
	}
}

// End to end: the primary opens the stream and then aborts before finishing its
// turn; the backup must serve the client a complete answer.
func TestFailoverAfterMidStreamInterruption(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-x\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"half an ans\"}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Abort without message_delta/message_stop.
		panic(http.ErrAbortHandler)
	}))
	defer primary.Close()

	var backupHits int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-x\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"the whole answer\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer backup.Close()

	state := newStreamDropState(t, primary.URL, backup.URL)
	server := NewServer(NewRouter(state), monitor.NewStore())

	route, err := server.router.RouteByID("stream-drop-route")
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	dropKey := streamDropKey(route.ID, "stream-drop-backup")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	status, _, _, _, model, execErr := server.executeProtocolFlowWithFailover(
		rec, req, route, streamDropDecision(route.ID, route.ProviderID),
		"client-model", map[string]any{"model": "client-model", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		domain.ProtocolClaude, true, dropKey, true,
	)
	if execErr != nil {
		t.Fatalf("exec err: %v", execErr)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	if model != "client-model" {
		t.Fatalf("model=%s", model)
	}
	if backupHits != 1 {
		t.Fatalf("backup hits=%d, want 1", backupHits)
	}
	body := rec.Body.String()
	if strings.Contains(body, "half an ans") {
		t.Fatalf("client saw the aborted attempt: %q", body)
	}
	if !strings.Contains(body, "the whole answer") || !strings.Contains(body, "message_stop") {
		t.Fatalf("client did not get the backup's stream: %q", body)
	}
}

// Without any backup the same drop must end the turn instead of replaying the
// same provider and duplicating text.
func TestNoFailoverReplayAfterMidStreamInterruption(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-x\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"once\"}}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Give the bytes time to travel before the connection dies; the abort
		// can otherwise discard what the server already queued.
		time.Sleep(50 * time.Millisecond)
		panic(http.ErrAbortHandler)
	}))
	defer upstream.Close()

	state := newStreamDropState(t, upstream.URL, "")
	server := NewServer(NewRouter(state), monitor.NewStore())

	route, err := server.router.RouteByID("stream-drop-route")
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	dropKey := streamDropKey(route.ID)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	_, _, _, _, _, _ = server.executeProtocolFlowWithFailover(
		rec, req, route, streamDropDecision(route.ID, route.ProviderID),
		"client-model", map[string]any{"model": "client-model", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		domain.ProtocolClaude, true, dropKey, true,
	)
	if hits != 1 {
		t.Fatalf("upstream hits=%d, want 1 (no same-provider replay)", hits)
	}
	if got := strings.Count(rec.Body.String(), `"once"`); got != 1 {
		t.Fatalf("client saw the text %d times: %q", got, rec.Body.String())
	}
}

// The Responses client path answers through the same converter and must not
// leave the client hanging at EOF: a truncated upstream turn is closed with
// response.incomplete instead of a normal response.completed.
func TestClaudeToResponsesInterruptedStreamEmitsIncomplete(t *testing.T) {
	upstreamSSE := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":5,"output_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut"}}`,
		``,
	}, "\n")
	rec := httptest.NewRecorder()
	gated := newVisibilityGatedSSEWriter(rec)
	_, err := streamClaudeToResponsesEventsDirect(gated, io.MultiReader(strings.NewReader(upstreamSSE), errReader{}), "claude-x", nil)
	if !isUpstreamStreamInterruption(err) {
		t.Fatalf("want interruption err, got %v", err)
	}
	gated.markVisible()
	out := rec.Body.String()
	if strings.Contains(out, "response.completed") {
		t.Fatalf("truncated turn must not look complete: %q", out)
	}
	if !strings.Contains(out, "response.output_text.delta") || !strings.Contains(out, "cut") {
		t.Fatalf("client should keep the text it got: %q", out)
	}
	if !strings.Contains(out, "response.incomplete") {
		t.Fatalf("truncated turn must end response.incomplete, got %q", out)
	}
	if !strings.Contains(out, `"reason":"upstream_stream_interrupted"`) {
		t.Fatalf("missing incomplete reason: %q", out)
	}
}
func TestUpstreamStreamInterruptedResponse(t *testing.T) {
	open := &http.Response{StatusCode: 200, Body: io.NopCloser(&errReader{}), ContentLength: -1}
	if !isUpstreamStreamInterruptedResponse(open, false) {
		t.Fatal("a stream without its closing frame should count as interrupted")
	}
	if isUpstreamStreamInterruptedResponse(open, true) {
		t.Fatal("a stream that sent its closing frame is complete")
	}
	chunked, err := http.ReadResponse(bufio.NewReader(strings.NewReader(
		"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n"+
			"11\r\ndata: [DONE]\n\n\r\n0\r\n\r\n")), nil)
	if err != nil {
		t.Fatalf("read chunked response: %v", err)
	}
	if !isUpstreamStreamInterruptedResponse(chunked, false) {
		t.Fatal("a live chunked body is not known to be truncated")
	}
	for _, body := range []string{
		"data: {\"type\":\"message_stop\"}\n\n",
		"data: [DONE]\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
	} {
		if !closesUpstreamStream([]byte(body)) {
			t.Fatalf("closing frame not recognised: %q", body)
		}
	}
	for _, body := range []string{"", "data: {\"delta\":\"half", ": ping\n\n"} {
		if closesUpstreamStream([]byte(body)) {
			t.Fatalf("partial body taken for complete: %q", body)
		}
	}
}

// newStreamDropState builds a two-account chain: the primary (Claude-protocol
// passthrough target for the test's own URL) and an optional backup.
func newStreamDropState(t *testing.T, primaryURL, backupURL string) domain.GatewayState {
	t.Helper()
	state := domain.GatewayState{
		Providers: []domain.Provider{{
			ID: "stream-drop-primary", Name: "stream-drop-primary", Protocol: domain.ProtocolClaude,
			BaseURL: primaryURL, AuthType: "api_key", APIKeySource: "static", HealthStatus: "healthy",
		}},
		Routes: []domain.Route{{
			ID: "stream-drop-route", Name: "stream-drop-route", ProviderID: "stream-drop-primary",
			OutputProtocol: domain.ProtocolClaude, Mode: domain.RouteModePassThrough, Enabled: true,
		}},
	}
	if backupURL != "" {
		state.Providers = append(state.Providers, domain.Provider{
			ID: "stream-drop-backup", Name: "stream-drop-backup", Protocol: domain.ProtocolClaude,
			BaseURL: backupURL, AuthType: "api_key", APIKeySource: "static", HealthStatus: "healthy",
		})
		state.Routes = append(state.Routes, domain.Route{
			ID: "stream-drop-backup-route", Name: "stream-drop-backup-route", ProviderID: "stream-drop-backup",
			OutputProtocol: domain.ProtocolClaude, Mode: domain.RouteModePassThrough, Enabled: true,
		})
	}
	return state
}

func streamDropKey(routeID string, fallbacks ...string) domain.APIKey {
	return domain.APIKey{ID: "stream-drop-key", RouteID: routeID, FallbackProviderIDs: fallbacks}
}

func streamDropDecision(routeID, providerID string) domain.RouteDecision {
	return domain.RouteDecision{RouteID: routeID, ProviderID: providerID}
}

// errReader fails the way a dropped connection does, before EOF.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
