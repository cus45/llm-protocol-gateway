package gateway

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// countingHeartbeatWriter records whether a keep-alive is still being written.
type countingHeartbeatWriter struct {
	hb        http.Header
	active    atomic.Int32
	heartbeat atomic.Int32
	sent      atomic.Int32
}

func (w *countingHeartbeatWriter) Header() http.Header         { return w.hb }
func (w *countingHeartbeatWriter) WriteHeader(int)             {}
func (w *countingHeartbeatWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *countingHeartbeatWriter) Flush()                      {}

func (w *countingHeartbeatWriter) WriteSSEHeartbeat() error {
	w.active.Add(1)
	defer w.active.Add(-1)
	w.heartbeat.Add(1)
	w.sent.Store(1)
	time.Sleep(2 * time.Millisecond)
	return nil
}

// stop must not return while the keep-alive goroutine could still write: the
// caller releases the provider lane and ends the response right after it.
func TestSSEHeartbeatStopJoinsTheGoroutine(t *testing.T) {
	writer := &countingHeartbeatWriter{hb: make(http.Header)}
	stop := startSSEHeartbeat(writer, time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for writer.sent.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if writer.sent.Load() == 0 {
		t.Fatal("heartbeat never fired")
	}
	stop()
	if active := writer.active.Load(); active != 0 {
		t.Fatalf("stop returned with %d heartbeat(s) still writing", active)
	}
	// Idempotent, and safe to call after the goroutine has gone.
	stop()
}

// Stopping a writer that never heartbeats is a no-op, not a hang.
func TestSSEHeartbeatStopWithoutSupport(t *testing.T) {
	done := make(chan struct{})
	go func() {
		recorder := httptest.NewRecorder()
		startSSEHeartbeat(recorder, time.Millisecond)()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop blocked for a writer with no heartbeat support")
	}
}
