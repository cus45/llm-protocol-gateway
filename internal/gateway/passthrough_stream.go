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
//
// cut is what streamEndReader saw of the bytes that actually went out: which
// blocks a Claude turn left open, and the id, model and last sequence number a
// Responses stream had already used. It is read from every byte of the stream,
// not from the capped log copy, so a long answer is closed as accurately as a
// short one.
type streamInterrupter interface {
	closeInterruptedStream(w http.ResponseWriter, cut streamCut)
}

// claudeStreamInterrupter ends an Anthropic Messages stream that has no
// message_stop with a stop_reason, so the client shows the text it has and
// knows the turn did not finish cleanly.
type claudeStreamInterrupter struct{}

func (claudeStreamInterrupter) closeInterruptedStream(w http.ResponseWriter, cut streamCut) {
	if !cut.messageStarted {
		// Nothing the client could be holding open: the turn ends as an error,
		// as magpie ends a cut reply, and not with a stop_reason the model never
		// chose.
		_ = writeClaudeSSE(w, "error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": streamCutMessage,
			},
		})
		_ = writeClaudeSSE(w, "message_stop", map[string]any{"type": "message_stop"})
		return
	}
	// Every block left open is closed first: otherwise a client SDK waits for
	// the block's content_block_stop and never reads message_stop.
	_ = closeInterruptedClaudeStream(w, cut.turn(), cut.usage, int(cut.total))
}

// streamCut is what the bytes of one forwarded stream say about how it ended:
// enough to close it in its own protocol, whatever its size.
type streamCut struct {
	usage          TokenUsage
	total          int64
	messageStarted bool
	openBlocks     []int
	id, model      string // Responses stream identity
	lastSequence   int    // Responses: highest sequence_number already sent
}

// turn is the Claude view of the cut, in the shape the closing helper wants.
func (c streamCut) turn() claudeInterruptedTurn {
	turn := claudeInterruptedTurn{messageStarted: c.messageStarted, toolOpen: map[string]int{}}
	for _, index := range c.openBlocks {
		turn.toolOpen[fmt.Sprintf("block-%d", index)] = index
	}
	return turn
}

// openAIChatStreamInterrupter ends an OpenAI Chat Completions stream with a
// finish_reason=length chunk and [DONE], the only way a Chat client learns the
// answer stopped early.
type openAIChatStreamInterrupter struct {
	model string
}

func (o openAIChatStreamInterrupter) closeInterruptedStream(w http.ResponseWriter, _ streamCut) {
	// An error event, not a finish reason: "length" and [DONE] together tell the
	// client the model finished its answer, and a client that trusts them keeps
	// half a sentence as the whole of it. Magpie says the same thing here
	// (streamFailure, gateway.go:4940) and names the cause (cutMidReply, :4934).
	writeChatStreamFailure(w, o.model, streamCutMessage)
}

// streamCutMessage is what a client is told when the upstream connection died
// mid-reply: the same wording magpie uses, so a person reading the transcript
// knows it was the connection and not the model.
const streamCutMessage = "connection lost mid-reply"

// writeChatStreamFailure ends a Chat stream with the protocol's own error event.
func writeChatStreamFailure(w http.ResponseWriter, model, message string) {
	payload := map[string]any{
		"error": map[string]any{
			"message": model + ": " + message,
			"type":    "api_error",
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = io.WriteString(w, "data: ")
	_, _ = w.Write(body)
	_, _ = io.WriteString(w, "\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// responsesStreamInterrupter ends an OpenAI Responses stream with
// response.incomplete instead of leaving the client at EOF.
type responsesStreamInterrupter struct{}

// closeInterruptedStream repeats the id, model and sequence number the stream
// it closes established. A Responses SDK keys its events on sequence_number and
// raises on one that repeats or skips, so a closing frame with a fresh number
// of its own would make the interruption look worse than the bare EOF it
// replaces.
func (responsesStreamInterrupter) closeInterruptedStream(w http.ResponseWriter, cut streamCut) {
	id, model, sequence := responsesStreamIdentity(cut)
	// response.failed, not response.incomplete: an incomplete answer is one the
	// model stopped on purpose (max_output_tokens), and a client that sees it
	// keeps the half sentence as the answer. Magpie reports a cut reply the same
	// way (streamFailure, gateway.go:4940).
	response := map[string]any{
		"id":     id,
		"object": "response",
		"status": "failed",
		"error": map[string]any{
			"code":    "server_error",
			"message": streamCutMessage,
		},
	}
	if model != "" {
		response["model"] = model
	}
	_ = writeResponsesSSEEvent(w, "response.failed", map[string]any{
		"type":            "response.failed",
		"sequence_number": sequence,
		"response":        response,
	})
}

// responsesStreamIdentity is the response id, model and the next unused
// sequence_number for the frame that closes a cut stream. A stream the upstream
// cut before it sent anything keeps the shape it would have had from the start.
func responsesStreamIdentity(cut streamCut) (id, model string, sequence int) {
	id, model, last := cut.id, cut.model, cut.lastSequence
	if id == "" {
		id = fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	if last < 0 {
		return id, model, 0
	}
	return id, model, last + 1
}

// escapeRune is the JSON string escape character, kept as a named constant so
// the scanner below does not have to spell it in a literal.
const escapeRune = 0x5C

// trimToEventEnd cuts a body at the end of its last JSON object, since the
// stream's last line may be a partial frame the upstream never finished.
func trimToEventEnd(stream []byte) []byte {
	depth := 0
	inString := false
	escaped := false
	for index, char := range stream {
		switch {
		case escaped:
			escaped = false
		case char == escapeRune && inString:
			escaped = true
		case char == '"':
			inString = !inString
		case inString:
		case char == '{':
			depth++
		case char == '}':
			depth--
			if depth == 0 {
				return stream[:index+1]
			}
		}
	}
	return stream
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
func writePassThroughResponse(w http.ResponseWriter, response *http.Response, stream bool, kind usageStreamKind, parseUsage func([]byte) TokenUsage, end ...streamInterrupter) (int, TokenUsage, []byte, error) {
	if fw, ok := w.(*failoverResponseWriter); ok && stream && response.StatusCode < 400 {
		// Headers go into the attempt's own header map, not the client's: a
		// retry must not inherit them (a stale Retry-After outliving its 429).
		return writeBufferedStream(fw, response, kind, parseUsage, end...)
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
	usage := newStreamUsageReader(kind)
	hw := newSSEHeartbeatResponseWriter(w)
	hw.WriteHeader(response.StatusCode)
	stopHeartbeat := startSSEHeartbeat(hw, sseHeartbeatInterval)
	defer stopHeartbeat()
	hw.Flush()

	// hw.Write already serializes; flush after each chunk via flushWriter so
	// we do not re-enter hw.Flush while holding hw's write lock.
	writer := io.MultiWriter(&flushWriter{w: hw, flusher: hw}, tee, usage)
	_, copyErr := io.Copy(writer, response.Body)
	// io.Copy only reports an error when the transport itself failed; a stream
	// cut mid-answer ends quietly as EOF, so the stream itself is the evidence —
	// read from every byte that went out, not from the capped log copy (an
	// answer longer than the cap would otherwise look like it never closed).
	if copyErr == nil && isUpstreamStreamInterruptedResponse(response, usage.closed()) {
		if len(end) > 0 && end[0] != nil && tee.Total() > 0 {
			// Part of the answer already reached the client, so it cannot be
			// rewound onto another account; close the turn instead of hanging.
			end[0].closeInterruptedStream(w, usage.cut(parseUsage, tee.Bytes()))
		}
		// The tokens the stream did report are spent all the same.
		return response.StatusCode, usage.final(parseUsage, tee.Bytes()), tee.Bytes(),
			newUpstreamStreamInterruption(io.ErrUnexpectedEOF, int(tee.Total()))
	}
	if copyErr != nil {
		return response.StatusCode, usage.final(parseUsage, tee.Bytes()), tee.Bytes(), copyErr
	}
	return response.StatusCode, usage.final(parseUsage, tee.Bytes()), tee.Bytes(), nil
}

// streamTee keeps the capped log copy of a forwarded stream (and, through
// limitedBuffer.Total, its true size for daily traffic accounting). Usage does
// not come from here but from streamUsageReader, which sees every byte.
type streamTee struct {
	*limitedBuffer
}

func newStreamTee(_ *http.Response) *streamTee {
	return &streamTee{limitedBuffer: &limitedBuffer{max: passThroughLogBufferMax}}
}

// converterStreamWriter is the writer a converter streams through when the
// failover loop is in front of it. Its output goes straight to the client —
// the first byte of the answer is what streaming is for, so the turn is
// committed then and never replayed onto another account — while a capped copy
// is kept for the logs and the true size is counted.
//
// A converter that fails before writing anything commits nothing, so the
// device stays deferrable and the loop can still hand the turn to a backup.
type converterStreamWriter struct {
	fw   *failoverResponseWriter
	hw   *sseHeartbeatResponseWriter // nil until the first write commits
	tee  *limitedBuffer
	stop func()
	// namedStatus says the converter named the turn's status through
	// WriteHeader. The status itself is handed to the failover writer at once,
	// not kept here for Write to read back: a status held in a field and read
	// from the top of Write was not seen at the first byte (same shape that
	// lost providers' ids in round27). Without it, the stream opening's own
	// status — never a default 200 — is the attempt's.
	namedStatus bool
	// pending is what the turn wrote before it said anything of the answer:
	// sent as a whole at the first content event, or dropped with the attempt if
	// the account fails first.
	pending      bytes.Buffer
	holdingSince time.Time
}

// converterHoldLimit bounds how long a turn may hold its leading frames: past it
// the client would rather see them than wait for content.
const converterHoldLimit = 1500 * time.Millisecond

func newConverterStreamWriter(fw *failoverResponseWriter) *converterStreamWriter {
	return &converterStreamWriter{fw: fw, tee: &limitedBuffer{max: passThroughLogBufferMax}}
}

func (s *converterStreamWriter) Header() http.Header { return s.fw.Header() }

// WriteHeader is held back until the first byte: a converter sets its headers
// before it knows whether the upstream will deliver anything. The status is
// kept, not dropped — it is the answer's own status once the answer has bytes,
// and a stream that produced none is written out by finishConvertedProxy with
// the upstream's status instead.
func (s *converterStreamWriter) WriteHeader(statusCode int) {
	if s.hw == nil {
		if statusCode > 0 {
			s.fw.WriteHeader(statusCode)
			s.namedStatus = true
		}
		return
	}
	s.hw.WriteHeader(statusCode)
}

func (s *converterStreamWriter) Write(p []byte) (int, error) {
	if s.hw == nil {
		// Held until the turn says something: the leading frames of every
		// protocol (message_start, response.created, the first Chat role chunk)
		// say only that a reply began, and a failure behind them can still be
		// answered by the next account. Magpie holds the same way (holdWriter,
		// fallback.go:1779-1834) and lets go at the first content event.
		if !s.turnHasContent(p) && !s.overdue() {
			_, _ = s.pending.Write(p)
			if s.pending.Len() > closingBodyMax {
				// A reply that leads on and on is not a reply: let it go so no
				// memory is held for an answer that never starts.
				s.release()
			}
			return len(p), nil
		}
		s.release()
	}
	n, err := s.hw.Write(p)
	_, _ = s.tee.Write(p[:n])
	s.hw.Flush()
	return n, err
}

// release opens the turn and sends whatever was held before it.
func (s *converterStreamWriter) release() {
	held := append([]byte(nil), s.pending.Bytes()...)
	s.pending.Reset()
	s.commit()
	if len(held) > 0 {
		_, _ = s.hw.Write(held)
		_, _ = s.tee.Write(held)
		s.hw.Flush()
	}
}

// overdue says the turn has been held long enough that a client waiting on it
// would rather see the leading frames than keep waiting for content.
func (s *converterStreamWriter) overdue() bool {
	if s.holdingSince.IsZero() {
		s.holdingSince = time.Now()
		return false
	}
	return time.Since(s.holdingSince) > converterHoldLimit
}

// turnHasContent says the bytes the converter just wrote carry part of the
// answer itself, not only the opening of the reply.
func (s *converterStreamWriter) turnHasContent(p []byte) bool {
	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimSpace(line)
		if payload, ok := strings.CutPrefix(line, "data:"); ok {
			line = strings.TrimSpace(payload)
		}
		if line == "" || line == "[DONE]" {
			continue
		}
		// An event whose type names a delta is content; the openings are not.
		if kind := streamEventType([]byte(line)); kind != "" {
			if strings.Contains(kind, "delta") || strings.HasSuffix(kind, ".done") {
				return true
			}
			continue
		}
		// A Chat chunk: content only when it carries text or tool arguments.
		if strings.Contains(line, `"content":"`) || strings.Contains(line, `"arguments":"`) || strings.Contains(line, `"text":"`) {
			if !strings.Contains(line, `"content":""`) && !strings.Contains(line, `"text":""`) {
				return true
			}
		}
	}
	return false
}

// heldTurn is what the writer holds before the turn says anything.
func (s *converterStreamWriter) heldTurn() []byte {
	return append([]byte(nil), s.pending.Bytes()...)
}

// sendHeld writes the held turn out as it stands, still holding nothing back.
func (s *converterStreamWriter) sendHeld() {
	if s.hw != nil {
		return
	}
	s.release()
}

// commit opens the turn on its first byte, through the failover writer so the
// attempt's status — not a default 200 — is what the client's response carries.
func (s *converterStreamWriter) commit() {
	if !s.namedStatus {
		// A stream is a success by definition; a converter that never named a
		// status is writing the answer of a successful upstream.
		s.fw.WriteHeader(http.StatusOK)
	}
	s.hw = newSSEHeartbeatResponseWriter(s.fw)
	s.fw.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	s.fw.Header().Set("Cache-Control", "no-cache")
	s.fw.Header().Set("Connection", "keep-alive")
	s.fw.FlushError()
	s.stop = startSSEHeartbeat(s.hw, sseHeartbeatInterval)
}

func (s *converterStreamWriter) Flush() {
	if s.hw != nil {
		s.hw.Flush()
	}
}

// Committed reports whether the client has seen part of this attempt.
func (s *converterStreamWriter) Committed() bool { return s.hw != nil && s.hw.Committed() }

// Finish stops the heartbeat and returns the capped copy of what went out.
func (s *converterStreamWriter) Finish() []byte {
	// A turn that never said anything but its opening still sent that opening:
	// it is not held back from a client that has nothing else.
	if s.hw == nil && s.pending.Len() > 0 {
		s.release()
	}
	if s.stop != nil {
		s.stop()
	}
	return s.tee.Bytes()
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
func writeBufferedStream(fw *failoverResponseWriter, response *http.Response, kind usageStreamKind, parseUsage func([]byte) TokenUsage, end ...streamInterrupter) (int, TokenUsage, []byte, error) {
	tee := newStreamTee(response)
	usage := newStreamUsageReader(kind)
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
	hw := newSSEHeartbeatResponseWriter(fw)
	stopHeartbeat := startSSEHeartbeat(hw, sseHeartbeatInterval)
	defer stopHeartbeat()
	writer := io.MultiWriter(&flushWriter{w: hw, flusher: hw}, tee, usage)
	_, copyErr := io.Copy(writer, response.Body)
	// The response is committed as soon as anything reached the client, and
	// from then on this attempt is the answer: the failover loop must not drop
	// it as if it had never answered.
	delivered := hw.Committed() || tee.Total() > 0
	if delivered {
		fw.FlushError()
	}
	if copyErr != nil {
		if delivered {
			// The connection died mid-answer after part of it reached the
			// client: that prefix is the answer now, so its turn is closed and
			// the cut reported, instead of leaving a client waiting at EOF.
			if len(end) > 0 && end[0] != nil {
				end[0].closeInterruptedStream(hw, usage.cut(parseUsage, tee.Bytes()))
				hw.Flush()
			}
			return response.StatusCode, usage.final(parseUsage, tee.Bytes()), tee.Bytes(),
				newUpstreamStreamInterruption(copyErr, int(tee.Total()))
		}
		return response.StatusCode, usage.final(parseUsage, tee.Bytes()), tee.Bytes(), copyErr
	}
	// io.Copy reports nothing when the body simply ends, so the closing frame —
	// read from every byte that went out — is what says whether it was cut.
	if isUpstreamStreamInterruptedResponse(response, usage.closed()) {
		if delivered && len(end) > 0 && end[0] != nil {
			// The client holds part of the answer, so the turn is closed on the
			// wire instead of ending at a EOF it would parse as a stall.
			end[0].closeInterruptedStream(hw, usage.cut(parseUsage, tee.Bytes()))
			hw.Flush()
		}
		return response.StatusCode, usage.final(parseUsage, tee.Bytes()), tee.Bytes(),
			newUpstreamStreamInterruption(io.ErrUnexpectedEOF, int(tee.Total()))
	}
	fw.FlushError()
	return response.StatusCode, usage.final(parseUsage, tee.Bytes()), tee.Bytes(), nil
}
