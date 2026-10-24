package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
