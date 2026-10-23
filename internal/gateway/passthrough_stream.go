package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const passThroughLogBufferMax = 256 * 1024

// streamInterrupter closes a pass-through SSE stream whose upstream died after
// part of the answer was already forwarded.
type streamInterrupter interface {
	closeInterruptedStream(w http.ResponseWriter)
}

// claudeStreamInterrupter ends an Anthropic Messages stream that has no
// message_stop with a stop_reason, so the client shows the text it has and
// knows the turn did not finish cleanly.
type claudeStreamInterrupter struct{}

func (claudeStreamInterrupter) closeInterruptedStream(w http.ResponseWriter) {
	_ = writeClaudeSSE(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "max_tokens", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 0},
	})
	_ = writeClaudeSSE(w, "message_stop", map[string]any{"type": "message_stop"})
}

// openAIChatStreamInterrupter ends an OpenAI Chat Completions stream with a
// finish_reason=length chunk and [DONE], the only way a Chat client learns the
// answer stopped early.
type openAIChatStreamInterrupter struct {
	model string
}

func (o openAIChatStreamInterrupter) closeInterruptedStream(w http.ResponseWriter) {
	_ = writeOpenAISSEChunk(w, map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   o.model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "length"}},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

// responsesStreamInterrupter ends an OpenAI Responses stream with
// response.incomplete instead of leaving the client at EOF.
type responsesStreamInterrupter struct{}

func (responsesStreamInterrupter) closeInterruptedStream(w http.ResponseWriter) {
	_ = writeResponsesSSEEvent(w, "response.incomplete", map[string]any{
		"type": "response.incomplete",
		"response": map[string]any{
			"object":             "response",
			"status":             "incomplete",
			"incomplete_details": map[string]any{"reason": "upstream_stream_interrupted"},
		},
	})
}

// limitedBuffer keeps up to max bytes for logging/usage parsing while the
// remainder of a stream is still forwarded to the client. It also counts every
// byte written so callers can report true forwarded size despite the cap.
type limitedBuffer struct {
	buf   []byte
	max   int
	total int64
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.total += int64(len(p))
	if b.max <= 0 {
		return len(p), nil
	}
	if len(b.buf) < b.max {
		remain := b.max - len(b.buf)
		if len(p) > remain {
			b.buf = append(b.buf, p[:remain]...)
		} else {
			b.buf = append(b.buf, p...)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte {
	return b.buf
}

// Total is the full number of bytes streamed, including any beyond the cap.
func (b *limitedBuffer) Total() int64 {
	return b.total
}

// flushWriter writes to the response and flushes after each Write so SSE
// chunks reach the client immediately. When w already flushes internally
// (e.g. sseHeartbeatResponseWriter), flusher may be nil.
type flushWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if f.flusher != nil {
		f.flusher.Flush()
	}
	return n, err
}

func requestBodyWantsStream(body []byte) bool {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	stream, _ := req["stream"].(bool)
	return stream
}

func copyPassThroughHeaders(w http.ResponseWriter, response *http.Response) {
	for key, values := range response.Header {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
}

// writePassThroughResponse forwards an upstream response to the client.
// Non-stream (or error) responses are fully buffered.
//
// A successful stream with a backup account behind it (a failover response
// writer that can still defer) takes the buffered path: an empty stream
// retries on the same provider, and a stream the upstream cut mid-answer
// returns a retryable interruption so the failover loop can hand the turn to
// the backup — the client has seen nothing yet.
//
// Otherwise the first byte goes straight through, so nothing can be retried.
// end then names the protocol's closing frame (see streamInterrupter): a bare
// EOF is indistinguishable from a finished answer for a client that only waits
// for the closing event, so the cut turn is closed properly instead.
func writePassThroughResponse(w http.ResponseWriter, response *http.Response, stream bool, parseUsage func([]byte) TokenUsage, end ...streamInterrupter) (int, TokenUsage, []byte, error) {
	if fw, ok := w.(*failoverResponseWriter); ok && stream && response.StatusCode < 400 {
		// Headers go into the attempt's own header map, not the client's: a
		// retry must not inherit them (a stale Retry-After outliving its 429).
		return writeBufferedStream(fw, response, parseUsage, end...)
	}
	copyPassThroughHeaders(w, response)

	// Error responses are always fully buffered so clients and logs get the body.
	if !stream || response.StatusCode >= 400 {
		responseBody, readErr := io.ReadAll(response.Body)
		if readErr != nil {
			return 0, TokenUsage{}, nil, readErr
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(responseBody)))
		w.WriteHeader(response.StatusCode)
		if _, writeErr := w.Write(responseBody); writeErr != nil {
			return response.StatusCode, TokenUsage{}, responseBody, writeErr
		}
		usage := TokenUsage{}
		if parseUsage != nil {
			usage = parseUsage(responseBody)
		}
		return response.StatusCode, usage, responseBody, nil
	}

	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if w.Header().Get("Connection") == "" {
		w.Header().Set("Connection", "keep-alive")
	}
	tee := newStreamTee(response)
	hw := newSSEHeartbeatResponseWriter(w)
	hw.WriteHeader(response.StatusCode)
	stopHeartbeat := startSSEHeartbeat(hw, sseHeartbeatInterval)
	defer stopHeartbeat()
	hw.Flush()

	// hw.Write already serializes; flush after each chunk via flushWriter so
	// we do not re-enter hw.Flush while holding hw's write lock.
	writer := io.MultiWriter(&flushWriter{w: hw, flusher: hw}, tee)
	_, copyErr := io.Copy(writer, response.Body)
	// io.Copy only reports an error when the transport itself failed; a stream
	// cut mid-answer ends quietly as EOF, so the body is the remaining evidence.
	if copyErr == nil && isUpstreamStreamInterruptedResponse(response, closesUpstreamStream(tee.Bytes())) {
		if len(end) > 0 && end[0] != nil && tee.Total() > 0 {
			// Part of the answer already reached the client, so it cannot be
			// rewound onto another account; close the turn instead of hanging.
			end[0].closeInterruptedStream(w)
		}
		return response.StatusCode, TokenUsage{}, tee.Bytes(),
			newUpstreamStreamInterruption(io.ErrUnexpectedEOF, int(tee.Total()))
	}
	if copyErr != nil {
		return response.StatusCode, TokenUsage{}, tee.Bytes(), copyErr
	}
	return response.StatusCode, tee.parse(parseUsage), tee.Bytes(), nil
}

// streamTee keeps the capped log copy of a forwarded stream (and, through
// limitedBuffer.Total, its true size for daily traffic accounting).
type streamTee struct {
	*limitedBuffer
}

func newStreamTee(_ *http.Response) *streamTee {
	return &streamTee{limitedBuffer: &limitedBuffer{max: passThroughLogBufferMax}}
}

func (t *streamTee) parse(parseUsage func([]byte) TokenUsage) TokenUsage {
	if parseUsage == nil {
		return TokenUsage{}
	}
	return parseUsage(t.Bytes())
}

// interceptingStreamWriter holds everything a converter writes instead of
// sending it, so a stream the upstream cut can still be retried. It records
// what each Write meant rather than just what it said: a deferredSSEWriter
// commits headers on the first write, and replaying the bytes alone would send
// the body without them.
type interceptingStreamWriter struct {
	header  http.Header
	status  int                 // last status the converter set
	records [][]byte            // the stream, in write order
	writer  http.ResponseWriter // a fresh deferred writer that the replay fills
	last    http.ResponseWriter // the attempt's own writer, for headers
}

func newInterceptingStreamWriter() *interceptingStreamWriter {
	return &interceptingStreamWriter{header: make(http.Header)}
}

func (s *interceptingStreamWriter) Header() http.Header { return s.header }

func (s *interceptingStreamWriter) WriteHeader(statusCode int) { s.status = statusCode }

func (s *interceptingStreamWriter) Write(p []byte) (int, error) {
	s.records = append(s.records, append([]byte(nil), p...))
	return len(p), nil
}

func (s *interceptingStreamWriter) Flush() {}

// body returns the intercepted stream for logging and for the failover buffer.
func (s *interceptingStreamWriter) body() []byte {
	var b bytes.Buffer
	for _, p := range s.records {
		b.Write(p)
	}
	return b.Bytes()
}

// replay writes the intercepted stream into a deferred writer, which commits
// it on the way out just as a live stream would have been committed.
func (s *interceptingStreamWriter) replay(base http.ResponseWriter) *deferredSSEWriter {
	var w *deferredSSEWriter
	if base != nil {
		w = newDeferredSSEWriter(base)
	} else {
		w, _ = base.(*deferredSSEWriter)
	}
	for key, values := range s.header {
		w.Header()[key] = values
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	for _, p := range s.records {
		_, _ = w.Write(p)
	}
	return w
}

// writeBufferedStream forwards a successful stream through the failover
// buffer. A stream that sent its closing frame is forwarded whole; a stream cut
// mid-answer becomes a retryable interruption, because the client never saw it
// and the backup account can still deliver the turn.
//
// The retry only holds while the failover loop still has somewhere to go. The
// last attempt is written out before returning, so when the turn is closed by
// end (see streamInterrupter) the client gets the partial answer followed by a
// proper closing frame instead of a bare EOF it would parse as a stall.
func writeBufferedStream(fw *failoverResponseWriter, response *http.Response, parseUsage func([]byte) TokenUsage, end ...streamInterrupter) (int, TokenUsage, []byte, error) {
	tee := newStreamTee(response)
	// The logged body is capped, so report the true streamed size separately for
	// daily traffic accounting. response.Request carries the proxy request's
	// context, which holds the requestTiming this rides on.
	if response.Request != nil {
		defer func() { requestTimingFrom(response.Request.Context()).recordStreamedTxBytes(tee.Total()) }()
	}
	if response.Header.Get("Content-Type") != "" {
		fw.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	}
	if fw.Header().Get("Cache-Control") == "" {
		fw.Header().Set("Cache-Control", "no-cache")
	}
	streamBody, readErr := io.ReadAll(io.TeeReader(response.Body, tee))
	if readErr == nil && !closesUpstreamStream(streamBody) {
		readErr = io.ErrUnexpectedEOF
	}
	if readErr != nil {
		// Empty or cut: either way the client has seen nothing yet, and the
		// failover loop decides whether to retry here or move to the backup.
		// When it cannot (no backup behind this attempt), the partial answer is
		// sent here and the turn is closed, so the client waits for the closing
		// event it would otherwise never receive.
		if len(streamBody) > 0 && fw.failoverUnavailable() {
			fw.WriteHeader(response.StatusCode)
			hw := newSSEHeartbeatResponseWriter(fw)
			hw.WriteHeader(response.StatusCode)
			if _, writeErr := hw.Write(streamBody); writeErr != nil {
				return response.StatusCode, TokenUsage{}, tee.Bytes(), writeErr
			}
			if len(end) > 0 && end[0] != nil {
				end[0].closeInterruptedStream(hw)
			}
			hw.Flush()
			fw.FlushError()
		}
		return response.StatusCode, TokenUsage{}, tee.Bytes(),
			newUpstreamStreamInterruption(readErr, int(tee.Total()))
	}
	// fw.Write alone leaves an uncommitted failover buffer uncommitted, and the
	// failover loop would then drop this attempt as if it had never answered.
	// One WriteHeader records the status; FlushError commits the attempt below.
	fw.WriteHeader(response.StatusCode)
	hw := newSSEHeartbeatResponseWriter(fw)
	hw.WriteHeader(response.StatusCode)
	if _, writeErr := hw.Write(streamBody); writeErr != nil {
		return response.StatusCode, TokenUsage{}, tee.Bytes(), writeErr
	}
	hw.Flush()
	fw.FlushError()
	return response.StatusCode, tee.parse(parseUsage), tee.Bytes(), nil
}
