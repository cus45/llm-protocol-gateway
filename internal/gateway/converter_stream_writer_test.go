package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A converter that writes its own refusal keeps the status it asked for: the
// first write commits the response, and the held-back WriteHeader must not turn
// that refusal into a 200.
func TestConverterStreamWriterKeepsTheStatusItWasGiven(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := newConverterStreamWriter(newFailoverResponseWriter(recorder, true))
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusTooManyRequests)
	if _, err := writer.Write([]byte("data: {}\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	writer.Finish()
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("a converter's refusal reached the client as %d", recorder.Code)
	}
}

// Nothing written means the response is still the upstream's to describe: the
// converter's own default must not be committed on its behalf.
func TestConverterStreamWriterCommitsNothingWithoutBytes(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := newConverterStreamWriter(newFailoverResponseWriter(recorder, true))
	writer.WriteHeader(http.StatusOK)
	if writer.Committed() {
		t.Fatal("a converter that wrote nothing must not have committed the turn")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("nothing may be written before the first byte, code = %d", recorder.Code)
	}
}
