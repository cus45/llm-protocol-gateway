package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// 同 Provider 上游空流最多尝试次数（含首次）。偶发 SSE 建连后无 chunk 时自动重试。
const maxUpstreamEmptyStreamAttempts = 2

func isEmptyUpstreamStreamError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "stream ended without any chunks") ||
		strings.Contains(msg, "stream ended without any events") ||
		strings.Contains(msg, "stream ended without any text deltas")
}

// errThinkingOnlyEmptyResponse marks an upstream turn that Anthropic/the
// provider reported as fully completed (not truncated by max_tokens/refusal)
// but which produced zero visible output: no text, no tool_use, only
// thinking/redacted_thinking blocks (or nothing at all). Seen in practice on a
// long session where the request history carried a stray trailing thinking
// block (see dropTrailingAssistantThinking in convert_responses_claude.go):
// some upstreams silently echo another "thinking-only, no answer" turn
// instead of erroring, which otherwise reads to the client as the assistant
// going silent. Call sites wrap this with fmt.Errorf("...: %w", ...) so
// errors.Is still matches through the wrapping.
var errThinkingOnlyEmptyResponse = errors.New("claude turn completed with no visible output (thinking-only)")

// isThinkingOnlyEmptyStreamError reports whether err (or anything it wraps)
// is errThinkingOnlyEmptyResponse.
func isThinkingOnlyEmptyStreamError(err error) bool {
	return errors.Is(err, errThinkingOnlyEmptyResponse)
}

// isUpstreamStreamInterruption reports whether err means the upstream stopped
// talking mid-answer — the connection dropped, the body ended before its
// Content-Length, or the stream closed without the closing event. The answer is
// truncated, not wrong, so another account can still deliver it whole. A
// converter-side failure (bad JSON shape, write error) is excluded on purpose:
// the next provider would fail exactly the same way.
func isUpstreamStreamInterruption(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, errUpstreamStreamInterrupted) || isEmptyUpstreamStreamError(err) || isThinkingOnlyEmptyStreamError(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	// A converter that ran out of a body that never announced a Content-Length
	// cannot tell a clean end from a cut one on its own: it reports the missing
	// closing event and the transport words for it (a chunked stream cut by the
	// peer reads as "unexpected EOF" on the metrics, "unexpected EOF" in Go).
	for _, marker := range []string{
		"without its closing event",
		"connection reset",
		"connection aborted",
		"broken pipe",
		"unexpected eof",
		"server closed idle connection",
		"http2: server sent goaway",
		"stream error: stream id",
		"transport is closing",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// isUpstreamStreamInterruptedResponse reports whether a body copy stopped
// before the upstream finished sending: no body at all, or (for a body that
// announced nothing) a stream with no closing frame. The length is deliberately
// not trusted: live SSE responses arrive as chunked bodies with no announced
// length, which is every stream worth retrying.
func isUpstreamStreamInterruptedResponse(resp *http.Response, sawClosingFrame bool) bool {
	if resp == nil || resp.Body == nil {
		return true
	}
	if resp.ContentLength < 0 {
		return !sawClosingFrame
	}
	return false
}

// upstreamStreamInterruptionError marks a stream the upstream stopped
// mid-answer. It is a distinct type (not a bare sentinel) so callers can tell a
// truncated upstream from a converter failure without consulting the HTTP
// response: the response says nothing about why the stream ended.
type upstreamStreamInterruptionError struct {
	cause error // scanner error, or ErrUnexpectedEOF when the stream just ended
	total int   // body bytes the converter consumed
}

func (e *upstreamStreamInterruptionError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("upstream stream interrupted after %d bytes: %v", e.total, e.cause)
	}
	return fmt.Sprintf("upstream stream ended after %d bytes without its closing event", e.total)
}

// Unwrap exposes errUpstreamStreamInterrupted so errors.Is answers the coarse
// question "was this an interruption?".
func (e *upstreamStreamInterruptionError) Unwrap() error { return errUpstreamStreamInterrupted }

// countingReader remembers how much of the upstream body a converter consumed,
// so an interruption can say whether anything at all arrived (a drop before the
// first frame is an empty stream, retryable on the same provider).
type countingReader struct {
	r   io.Reader
	n   int
	err error // the error that ended the read, io.EOF for a body that just ran out
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	if err != nil {
		c.err = err
	}
	return n, err
}

// committed reports whether anything of the attempt reached the client, which
// is what makes an empty-stream retry unsafe. A buffered attempt has reached
// nobody: the failover buffer holds it until the loop settles.
func (w *deferredSSEWriter) committed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.wroteHeader || w.wroteBody
}

// deferredSSEWriter 推迟 WriteHeader，直到首次 Write / heartbeat（或显式 Commit）。
// 用于流式转换：上游空流时尚未向客户端提交任何字节，可安全同 Provider 重试。
// mu 串行化正常 SSE 写出与后台 heartbeat，避免并发写 ResponseWriter。
type deferredSSEWriter struct {
	mu          sync.Mutex
	base        http.ResponseWriter
	header      http.Header
	status      int
	wroteHeader bool
	wroteBody   bool
	bodyLog     *limitedBuffer
	buf         bytes.Buffer // the stream itself while a failover buffer holds the attempt
}

func newDeferredSSEWriter(base http.ResponseWriter) *deferredSSEWriter {
	return &deferredSSEWriter{
		base:    base,
		header:  make(http.Header),
		status:  http.StatusOK,
		bodyLog: &limitedBuffer{max: passThroughLogBufferMax},
	}
}

func (w *deferredSSEWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wroteHeader {
		return w.base.Header()
	}
	return w.header
}

func (w *deferredSSEWriter) WriteHeader(statusCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wroteHeader {
		return
	}
	w.status = statusCode
}

func (w *deferredSSEWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.wroteHeader {
		w.commitLocked(w.status)
	}
	w.wroteBody = true
	_, _ = w.bodyLog.Write(p)
	if buffered, ok := w.base.(*failoverResponseWriter); ok && buffered.deferring() {
		// The attempt's stream stays inside the failover buffer until the whole
		// stream is known to be complete: an interrupted turn is retried (here
		// or on the backup) and the client must never see the half answer.
		return w.buf.Write(p)
	}
	return w.base.Write(p)
}

// copyHeadersLocked copies the attempt's headers onto dst. Caller holds w.mu.
func (w *deferredSSEWriter) copyHeadersLocked(dst http.Header) {
	for key, values := range w.header {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

// flushBuffered hands a complete stream to the failover buffer, which the
// failover loop commits once it has settled on this attempt. A stream already
// flushed, or one on the last attempt (nothing left to retry), has nothing to
// hand over: flushBuffered must then commit it like the commit it replaces.
func (w *deferredSSEWriter) flushBuffered() {
	w.mu.Lock()
	defer w.mu.Unlock()
	buffered, ok := w.base.(*failoverResponseWriter)
	if !ok {
		return
	}
	if !buffered.deferring() {
		if !w.wroteHeader && buffered.failoverUnavailable() {
			w.commitLocked(w.status)
		}
		return
	}
	if w.buf.Len() == 0 {
		return
	}
	w.copyHeadersLocked(buffered.Header())
	buffered.WriteHeader(w.status)
	_, _ = buffered.Write(w.buf.Bytes())
}

// BodyBytes returns the stream the converter wrote, for logging and for the
// failover buffer. A buffered attempt's copy is uncapped: it is also the bytes
// the client will receive. Once it has been flushed it is empty, so the copy
// outliving that flush is the caller's to keep.
func (w *deferredSSEWriter) BodyBytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.bodyLog.Bytes()...)
}

func (w *deferredSSEWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.wroteHeader {
		return
	}
	if f, ok := w.base.(http.Flusher); ok {
		f.Flush()
	}
}

// WriteSSEHeartbeat commits headers if needed and writes an SSE comment
// keep-alive. Deliberately does NOT set wroteBody so short thinking-only /
// empty-stream retries remain possible until a real data frame is written.
func (w *deferredSSEWriter) WriteSSEHeartbeat() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.wroteHeader {
		w.commitLocked(w.status)
	}
	if _, err := w.base.Write(sseHeartbeatComment); err != nil {
		return err
	}
	if f, ok := w.base.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (w *deferredSSEWriter) commit(statusCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.commitLocked(statusCode)
}

func (w *deferredSSEWriter) commitLocked(statusCode int) {
	if w.wroteHeader {
		return
	}
	dst := w.base.Header()
	for key, values := range w.header {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
	w.base.WriteHeader(statusCode)
	w.status = statusCode
	w.wroteHeader = true
}

func (w *deferredSSEWriter) Committed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.wroteHeader || w.wroteBody
}

func (w *deferredSSEWriter) WroteBody() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.wroteBody
}

// errUpstreamStreamInterrupted marks a stream the upstream stopped mid-answer.
// Converters wrap it so the passthrough writer, the failover loop and the logs
// can tell "the answer is cut short" from "the converter itself failed".
var errUpstreamStreamInterrupted = errors.New("upstream stream interrupted")
