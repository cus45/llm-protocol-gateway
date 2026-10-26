package gateway

import (
	"errors"
	"io"
	"net/http"
	"time"
)

// Interrupted streams: the upstream stopped mid-answer before sending its
// closing event. Every converter below has already written whatever the client
// saw; these helpers close the client's protocol properly so a truncated answer
// is never presented as a finished one — and, when nothing visible was written,
// report the turn as the interruption it was so a backup account can take over.

// isUpstreamStreamInterruptedReader reports whether a converter's read of the
// upstream ended because the upstream dropped. A live SSE response has no
// Content-Length, so the transport cannot say the body was truncated; only the
// read error can. io.EOF is the normal end of a body — the closing frame is
// what decides completeness (see streamEndReader) — while io.ErrUnexpectedEOF
// and any other error mean the bytes stopped early.
func isUpstreamStreamInterruptedReader(r countingReader) bool {
	return r.err != nil && !errors.Is(r.err, io.EOF)
}

// newUpstreamStreamInterruption wraps a scanner error from a stream the
// upstream cut short.
func newUpstreamStreamInterruption(cause error, bytesRead int) error {
	return &upstreamStreamInterruptionError{cause: cause, total: bytesRead}
}

// closeInterruptedChatStream ends an OpenAI Chat Completions stream the
// upstream stopped mid-answer. If the client has already seen output it gets a
// finish_reason=length chunk and [DONE] — the closest honest signal Chat has —
// and the interruption is returned for the logs. With no visible output the
// stream is left untouched so the failover loop can retry it on another
// account (writing [DONE] here would commit a fake empty answer).
func closeInterruptedChatStream(
	w http.ResponseWriter,
	chunkID, model string,
	usage TokenUsage,
	sawVisibleOutput, sawAnyEvent *bool,
	stopReason *string,
	sentRole *bool,
	_ bool,
	bytesRead int,
) error {
	interrupted := &upstreamStreamInterruptionError{total: bytesRead}
	if !*sawVisibleOutput && !*sawAnyEvent {
		return interrupted
	}
	if !*sawVisibleOutput {
		// Nothing a client could keep: do not pass off silence as an answer.
		return interrupted
	}
	// A cut turn carries no finish reason at all: "length" and [DONE] together
	// tell the client the model finished its answer, and a client that trusts
	// them keeps half a sentence. The usage the stream reported stays with the
	// answer — a cut costs what it spent — and the turn then ends as the
	// protocol's error frame, as magpie ends a severed reply.
	if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.CacheTokens > 0 {
		_ = writeOpenAISSEChunk(w, map[string]any{
			"id":      chunkID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": nil}},
			"usage": claudeUsageToOpenAIUsage(usage, map[string]any{
				"cache_read_input_tokens":     usage.CacheTokens,
				"cache_creation_input_tokens": usage.CacheWriteTokens,
			}),
		})
	}
	writeChatStreamFailure(w, model, streamCutMessage)
	_ = sentRole
	_ = stopReason
	return interrupted
}

// claudeInterruptedTurn describes what a Claude-shaped stream had open when the
// upstream dropped.
type claudeInterruptedTurn struct {
	messageStarted    bool
	textBlockOpen     bool
	thinkingBlockOpen bool
	toolOpen          map[string]int // call/block id -> index, or indexes directly
	toolIndexes       []int
	nextIndex         int
}

// closeInterruptedClaudeStream ends an Anthropic Messages stream the upstream
// stopped mid-answer: open blocks are closed, and the turn reports max_tokens
// with the truncated text so the client shows it and knows it was cut.
func closeInterruptedClaudeStream(w http.ResponseWriter, turn claudeInterruptedTurn, usage TokenUsage, bytesRead int) error {
	interrupted := &upstreamStreamInterruptionError{total: bytesRead}
	if !turn.messageStarted {
		return interrupted
	}
	indexes := append([]int(nil), turn.toolIndexes...)
	for _, index := range turn.toolOpen {
		indexes = append(indexes, index)
	}
	for _, index := range indexes {
		if err := writeClaudeSSE(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": index,
		}); err != nil {
			return err
		}
	}
	if turn.textBlockOpen || turn.thinkingBlockOpen {
		if err := writeClaudeSSE(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": turn.nextIndex,
		}); err != nil {
			return err
		}
	}
	// The turn ends as an error, not as a finished answer: stop_reason
	// max_tokens says the model chose to stop, and a client that trusts it keeps
	// half a sentence as the whole of it. Magpie sends the protocol's error event
	// for a cut reply (streamFailure, gateway.go:4940).
	if err := writeClaudeSSE(w, "error", map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": streamCutMessage,
		},
	}); err != nil {
		return err
	}
	if err := writeClaudeSSE(w, "message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return err
	}
	return interrupted
}

// closeInterruptedClaudeChatStream is closeInterruptedClaudeStream for the
// Chat -> Claude converter, whose open blocks live in closure variables.
func closeInterruptedClaudeChatStream(w http.ResponseWriter, messageID, model string, usage TokenUsage, messageStarted, textBlockOpen, thinkingOpen bool, toolOpen map[int]int, blockIndex int, hasToolUse bool, bytesRead int) error {
	turn := claudeInterruptedTurn{
		messageStarted:    messageStarted,
		textBlockOpen:     textBlockOpen,
		thinkingBlockOpen: thinkingOpen,
		nextIndex:         blockIndex,
	}
	for _, index := range toolOpen {
		turn.toolIndexes = append(turn.toolIndexes, index)
	}
	_, _ = messageID, model
	return closeInterruptedClaudeStream(w, turn, usage, bytesRead)
}

// closeInterruptedChatToResponsesStream ends an OpenAI Responses stream the
// upstream stopped mid-answer with response.incomplete, so a client waiting for
// the terminal event is released and can tell the answer was truncated.
func closeInterruptedChatToResponsesStream(state *chatToResponsesStreamState, bytesRead int) error {
	interrupted := &upstreamStreamInterruptionError{total: bytesRead}
	if !state.hasOutput() {
		return interrupted
	}
	if err := state.ensureResponseCreated(); err != nil {
		return err
	}
	// The frame tells the client the turn stopped early; the interruption is
	// reported either way, so the loop and the logs never read a cut turn as a
	// finished one.
	// response.failed, not response.incomplete: "incomplete" is what a model
	// reports when it stopped on purpose, and a client keeps it as the answer.
	if err := writeResponsesSSEEvent(state.w, "response.failed", map[string]any{
		"type":            "response.failed",
		"sequence_number": state.nextSeq(),
		"response": map[string]any{
			"id":     state.responseID,
			"object": "response",
			"model":  state.model,
			"status": "failed",
			"error": map[string]any{
				"code":    "server_error",
				"message": streamCutMessage,
			},
		},
	}); err != nil {
		return interrupted
	}
	return interrupted
}

// closeInterruptedResponsesToChatStream ends a Chat stream converted from
// Responses the same way closeInterruptedChatStream ends a passed-through one.
func closeInterruptedResponsesToChatStream(w http.ResponseWriter, chunkID, model string, usage TokenUsage, sawAny *bool, textSeen *bool, roleSent *bool, hasToolCall bool, bytesRead int) error {
	interrupted := &upstreamStreamInterruptionError{total: bytesRead}
	if !*sawAny || (!*textSeen && !hasToolCall) {
		return interrupted
	}
	// A cut turn is an error, not a finish reason: "length" plus [DONE] reads as
	// the model having finished, and the half sentence becomes the answer. The
	// usage the stream reported stays with it.
	if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.CacheTokens > 0 {
		_ = writeOpenAISSEChunk(w, map[string]any{
			"id":      chunkID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": nil}},
			"usage": map[string]any{
				"prompt_tokens":     usage.InputTokens,
				"completion_tokens": usage.OutputTokens,
				"total_tokens":      usage.InputTokens + usage.OutputTokens,
			},
		})
	}
	writeChatStreamFailure(w, model, streamCutMessage)
	_ = roleSent
	_ = hasToolCall
	return interrupted
}

// cleanEOFInterruption reports the stream a converter's reads ended as an
// interruption when the upstream stopped at a clean EOF without its protocol's
// closing frame. A client sees a truncated answer as a finished one otherwise:
// a live SSE response has no Content-Length to disagree with, so the closing
// frame is the only evidence the answer was whole. The converter supplies that
// verdict from what it saw, since it alone knows which frame closes its
// upstream's stream.
//
// Only an event stream can be cut this way: an account may answer a request
// asked for as a stream with one whole JSON message instead, and a message has
// no closing frame to lose. A reader that did not keep what it read cannot tell
// the two apart, so it is judged as before.
func cleanEOFInterruption(r countingReader, closed bool) bool {
	if r.err == nil || !errors.Is(r.err, io.EOF) {
		// Nothing read, or a read that failed: isUpstreamStreamInterruptedReader
		// is the verdict.
		return false
	}
	if r.n == 0 || closed {
		return false
	}
	if r.kept && !isUpstreamEventStream(r.seen.Bytes()) {
		// The body was one whole message, not a stream.
		return false
	}
	return true
}

// errStreamWithoutClosingFrame stands in for the read error of a body that
// ended cleanly at EOF without the closing frame that would have made it whole:
// there was no error, and the missing frame is the one fact that says the
// answer stopped short.
var errStreamWithoutClosingFrame = errors.New("upstream stream ended without its closing frame")
