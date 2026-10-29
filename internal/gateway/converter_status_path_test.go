package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// The real request path: a converted stream that fails after its first byte
// hands the turn to the client with the attempt's own status. The first byte
// commits through the failover writer, whose default is a 200 — a converter's
// refusal must not be rewritten into one (round26 D26-8).
func TestFinishConvertedProxySendsTheConverterStatusThroughTheFailoverWriter(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), nil)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader(nil)),
	}
	streamConvert := func(w http.ResponseWriter, _ io.Reader, _ string) (TokenUsage, error) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("data: {\"error\":\"provider is busy\"}\n\n"))
		return TokenUsage{}, newUpstreamStreamInterruption(errors.New("cut"), 32)
	}
	recorder := httptest.NewRecorder()
	fw := newFailoverResponseWriter(recorder, false)
	status, _, _, streamErr, retryable := server.finishConvertedProxy(fw, response, "m", true, nil, streamConvert)
	fw.FlushError()
	if status != http.StatusOK {
		t.Fatalf("the attempt's upstream status is what the caller logs, got %d", status)
	}
	if !isUpstreamStreamInterruption(streamErr) || retryable {
		t.Fatalf("the cut must be reported and not retried: err=%v retryable=%v", streamErr, retryable)
	}
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("the converter's status reached the client as %d", recorder.Code)
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte("provider is busy")) {
		t.Fatalf("the client was not told why: %s", recorder.Body.String())
	}
}

// An upstream refusal that arrives as plain JSON (not a stream) must be
// reported to the failover loop, not written as the client's answer: a busy
// model (Qoder's 10605 answers 403 JSON) has to be able to go to the next
// account, and before this the converter committed it on the first one.
func TestFinishConvertedProxyReportsAPlainJSONRefusalWithoutCommitting(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), nil)
	queueBody := `{"code":"10605","message":"{\"isQueued\":false,\"retryAfterSeconds\":30,\"serviceAvailable\":false}"}`
	response := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(queueBody))),
	}
	convert := func(body []byte, _ string) ([]byte, TokenUsage, error) {
		return body, TokenUsage{}, nil
	}
	recorder := httptest.NewRecorder()
	fw := newFailoverResponseWriter(recorder, true)
	status, _, _, streamErr, _ := server.finishConvertedProxy(fw, response, "m", false, convert, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if streamErr == nil {
		t.Fatal("a refusal must be reported to the failover loop")
	}
	if !busyRefusalText(strings.ToLower(streamErr.Error())) {
		t.Fatalf("the refusal lost its busy verdict: %v", streamErr)
	}
	if !fw.deferring() {
		t.Fatal("the refusal must stay deferrable so the next account can be tried")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("the refusal reached the client: %q", recorder.Body.String())
	}
}

// A stream was asked for but the upstream refused before any stream began.
// That refusal must not reach the client while the chain still has accounts:
// this is the exact shape of Qoder's 10605 (403 JSON to a streaming request),
// and committing it here is what left twelve candidates untried.
func TestFinishConvertedProxyKeepsAStreamRefusalForTheLoop(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), nil)
	queueBody := `{"code":"10605","message":"{\"isQueued\":true,\"retryAfterSeconds\":30}"}`
	response := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(queueBody))),
	}
	convert := func(body []byte, _ string) ([]byte, TokenUsage, error) {
		return body, TokenUsage{}, nil
	}
	recorder := httptest.NewRecorder()
	fw := newFailoverResponseWriter(recorder, true)
	status, _, bodyOut, streamErr, _ := server.finishConvertedProxy(fw, response, "m", true, convert, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if streamErr == nil || !busyRefusalText(strings.ToLower(streamErr.Error())) {
		t.Fatalf("the busy refusal must be reported: %v", streamErr)
	}
	if !fw.deferring() {
		t.Fatal("nothing may be committed while the chain can still try another account")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("the refusal reached the client: %q", recorder.Body.String())
	}
	if !bytes.Contains(bodyOut, []byte("10605")) {
		t.Fatalf("the refusal body must travel back for the last attempt: %q", bodyOut)
	}
}
