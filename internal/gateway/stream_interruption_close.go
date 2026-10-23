package gateway

import (
	"io"
	"net/http"
	"strings"
	"time"
)

// Interrupted streams: the upstream stopped mid-answer before sending its
// closing event. Every converter below has already written whatever the client
// saw; these helpers close the client's protocol properly so a truncated answer
// is never presented as a finished one — and, when nothing visible was written,
// report the turn as the interruption it was so a backup account can take over.

// isUpstreamStreamInterruptedReader reports whether r ended because the
// upstream dropped. A live SSE response has no Content-Length, so the transport
// cannot say the body was truncated; net/http leaves the length negative until
// the reader reaches the end, and only sets it after a complete body, which is
// when the converter saw the closing event anyway.
func isUpstreamStreamInterruptedReader(r countingReader) bool {
	return r.err == io.EOF
}

// closesUpstreamStream reports whether an SSE body ended with the closing frame
// of its protocol, the only mark a stream carries that it was complete:
// [DONE] for Chat, response.completed/incomplete for Responses, message_stop
// for Anthropic, response.completed for Codex Chat (a Chat body shaped as a
// Responses event). A body cut mid-frame keeps its partial tail, which matches
// none of them.
func closesUpstreamStream(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	tail := body
	if len(tail) > 4096 {
		tail = tail[len(tail)-4096:]
	}
	text := string(tail)
	for _, marker := range []string{"response.completed", "response.incomplete", "message_stop", "response.failed", "[DONE]"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
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
	finishReason := mapClaudeStopReasonToOpenAI(*stopReason)
	if finishReason == "" || finishReason == "stop" {
		finishReason = "length"
	}
	finalChunk := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finishReason,
		}},
	}
	if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.CacheTokens > 0 {
		finalChunk["usage"] = claudeUsageToOpenAIUsage(usage, map[string]any{
			"cache_read_input_tokens":     usage.CacheTokens,
			"cache_creation_input_tokens": usage.CacheWriteTokens,
		})
	}
	if err := writeOpenAISSEChunk(w, finalChunk); err != nil {
		return err
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	_ = sentRole
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
	if err := writeClaudeSSE(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "max_tokens", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": usage.OutputTokens},
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
	return writeResponsesSSEEvent(state.w, "response.incomplete", map[string]any{
		"type":            "response.incomplete",
		"sequence_number": state.nextSeq(),
		"response": map[string]any{
			"id":                 state.responseID,
			"object":             "response",
			"model":              state.model,
			"status":             "incomplete",
			"incomplete_details": map[string]any{"reason": "upstream_stream_interrupted"},
		},
	})
}

// closeInterruptedResponsesToChatStream ends a Chat stream converted from
// Responses the same way closeInterruptedChatStream ends a passed-through one.
func closeInterruptedResponsesToChatStream(w http.ResponseWriter, chunkID, model string, usage TokenUsage, sawAny *bool, textSeen *bool, roleSent *bool, hasToolCall bool, bytesRead int) error {
	interrupted := &upstreamStreamInterruptionError{total: bytesRead}
	if !*sawAny || (!*textSeen && !hasToolCall) {
		return interrupted
	}
	_ = roleSent
	finishReason := "length"
	if hasToolCall {
		finishReason = "tool_calls"
	}
	_ = writeOpenAISSEChunk(w, map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	_ = usage
	return interrupted
}
