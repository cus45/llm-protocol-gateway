package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func writeClaudeSSE(w http.ResponseWriter, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// streamOpenAIChatToClaudeEvents reads an OpenAI Chat Completions SSE stream and
// writes an Anthropic Messages API SSE stream to w. declared is the request's
// own tool list, used to put back a spelling the model changed the case of.
func streamOpenAIChatToClaudeEvents(w http.ResponseWriter, reader io.Reader, model string, declared map[string]struct{}, clientStopSequences ...string) (TokenUsage, error) {
	messageID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	resolvedModel := strings.TrimSpace(model)
	messageStarted := false
	blockIndex := 0
	textBlockOpen := false
	thinkingOpen := false
	// A thinking block must precede every text block: Anthropic clients
	// (Claude Code among them) refuse a thinking block that follows text.
	textBlocksStarted := 0
	toolCallsInOrder := make([]int, 0, 4)
	toolBlocks := make(map[int]bool)
	toolBlockIndexByCall := make(map[int]int)
	usage := TokenUsage{}
	stopReason := "end_turn"
	// Text seen so far (tail only), for reporting a converted stop sequence.
	visibleText := ""

	closeTextBlock := func() error {
		if !textBlockOpen {
			return nil
		}
		if err := writeClaudeSSE(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": blockIndex,
		}); err != nil {
			return err
		}
		textBlockOpen = false
		blockIndex++
		return nil
	}

	closeThinkingBlock := func() error {
		if !thinkingOpen {
			return nil
		}
		if err := writeClaudeSSE(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": blockIndex,
		}); err != nil {
			return err
		}
		thinkingOpen = false
		blockIndex++
		return nil
	}

	emitThinking := func(text string) error {
		if text == "" {
			return nil
		}
		if !thinkingOpen {
			if err := writeClaudeSSE(w, "content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			}); err != nil {
				return err
			}
			thinkingOpen = true
		}
		return writeClaudeSSE(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": text},
		})
	}

	startTextBlock := func() error {
		if textBlockOpen {
			return nil
		}
		if err := closeThinkingBlock(); err != nil {
			return err
		}
		if err := writeClaudeSSE(w, "content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         blockIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		}); err != nil {
			return err
		}
		textBlockOpen = true
		textBlocksStarted++
		return nil
	}

	closed := false
	r := countingReader{r: reader}
	scanner := bufio.NewScanner(&r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	// Some upstreams (Qoder) report failures as an `event: error` frame whose data
	// payload is a bare error object with no "error" wrapper key. Track the event
	// name so it is recognized rather than skipped as an ordinary chunk.
	eventName := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			eventName = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			closed = true
			continue
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		errorValue, hasError := chunk["error"]
		if !hasError && strings.EqualFold(eventName, "error") {
			errorValue = chunk
			hasError = true
		}
		if hasError {
			claudeBody, _, convErr := openAIErrorValueToClaude(errorValue, resolvedModel)
			if convErr == nil && len(claudeBody) > 0 {
				var payload map[string]any
				_ = json.Unmarshal(claudeBody, &payload)
				_ = writeClaudeSSE(w, "error", payload)
			}
			errText := extractResponseErrorMessage(claudeBody)
			if errText == "" {
				errText, _ = errorMessageFromValue(errorValue, "upstream stream error")
			}
			return usage, fmt.Errorf("%s", errText)
		}

		if value, ok := chunk["model"].(string); ok && strings.TrimSpace(value) != "" {
			resolvedModel = value
		}
		if chunkUsage := ParseOpenAIUsage([]byte(payload)); chunkUsage.InputTokens > 0 || chunkUsage.OutputTokens > 0 {
			usage = chunkUsage
		}

		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		choice, ok := choices[0].(map[string]any)
		if !ok {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		text := stringValue(delta["content"])
		if reason := stringValue(choice["finish_reason"]); reason != "" {
			stopReason = mapOpenAIFinishReasonToClaude(reason)
		}

		if !messageStarted {
			if err := writeClaudeSSE(w, "message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":            messageID,
					"type":          "message",
					"role":          "assistant",
					"model":         firstNonEmpty(resolvedModel, model),
					"content":       []any{},
					"stop_reason":   nil,
					"stop_sequence": nil,
					"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
				},
			}); err != nil {
				return usage, err
			}
			messageStarted = true
		}

		if thinking := stringValue(delta["reasoning_content"]); thinking != "" {
			// Only the opening reasoning can be a thinking block; once the answer
			// has begun, Anthropic refuses a thinking block after text.
			if textBlocksStarted == 0 && !textBlockOpen {
				if err := emitThinking(thinking); err != nil {
					return usage, err
				}
			}
		}

		// WorkBuddy sends "tool_calls":[] on every delta; an empty array is not a
		// tool call, and treating it as one swallowed the chunk's text.
		if rawToolCalls, ok := delta["tool_calls"].([]any); ok && len(rawToolCalls) > 0 {
			for _, item := range rawToolCalls {
				call, ok := item.(map[string]any)
				if !ok {
					continue
				}
				callIndex := int(int64FromAny(call["index"]))
				functionValue, _ := call["function"].(map[string]any)
				if !toolBlocks[callIndex] {
					if err := closeThinkingBlock(); err != nil {
						return usage, err
					}
					if err := closeTextBlock(); err != nil {
						return usage, err
					}
					toolCallsInOrder = append(toolCallsInOrder, callIndex)
					toolBlockIndexByCall[callIndex] = blockIndex
					toolBlock := map[string]any{
						"type":  "tool_use",
						"id":    stringValue(call["id"]),
						"name":  restoreDeclaredToolName(stringValue(functionValue["name"]), declared),
						"input": map[string]any{},
					}
					if err := writeClaudeSSE(w, "content_block_start", map[string]any{
						"type":          "content_block_start",
						"index":         blockIndex,
						"content_block": toolBlock,
					}); err != nil {
						return usage, err
					}
					toolBlocks[callIndex] = true
					blockIndex++
				}
				if partial := stringValue(functionValue["arguments"]); partial != "" {
					claudeIndex := toolBlockIndexByCall[callIndex]
					if err := writeClaudeSSE(w, "content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": claudeIndex,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
					}); err != nil {
						return usage, err
					}
				}
			}
			continue
		}

		if text != "" {
			if err := startTextBlock(); err != nil {
				return usage, err
			}
			// Keep the tail: a converted stop sequence is recognised when the
			// visible text ends on it, and it can arrive split across chunks.
			visibleText = tailForStopSequences(visibleText+text, clientStopSequences)
			if err := writeClaudeSSE(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]any{"type": "text_delta", "text": text},
			}); err != nil {
				return usage, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return usage, newUpstreamStreamInterruption(err, r.n)
	}
	// Decided from what the reads actually saw: before the loop nothing has
	// been read, a normal end of body is io.EOF, and a body that ends without
	// [DONE] was cut however cleanly the socket closed.
	if isUpstreamStreamInterruptedReader(r) || cleanEOFInterruption(r, closed) {
		openToolBlocks := make(map[int]int, len(toolBlocks))
		for callIndex, open := range toolBlocks {
			if open {
				openToolBlocks[callIndex] = toolBlockIndexByCall[callIndex]
			}
		}
		return usage, closeInterruptedClaudeChatStream(w, messageID, resolvedModel, usage, messageStarted, textBlockOpen, thinkingOpen, openToolBlocks, blockIndex, len(openToolBlocks) > 0, r.n)
	}

	if !messageStarted {
		return usage, fmt.Errorf("openai stream ended without any chunks")
	}
	if err := closeThinkingBlock(); err != nil {
		return usage, err
	}
	if err := closeTextBlock(); err != nil {
		return usage, err
	}
	// Tool blocks close in the order they opened. Iterating the map closed them
	// in a random order, which desynchronizes strict clients' block tracking.
	for _, callIndex := range toolCallsInOrder {
		if !toolBlocks[callIndex] {
			continue
		}
		if err := writeClaudeSSE(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": toolBlockIndexByCall[callIndex],
		}); err != nil {
			return usage, err
		}
	}
	// Chat only says "stop"; when the visible answer ends on a stop sequence
	// the client sent, report it the way Anthropic does.
	var stopSequenceValue any
	stopReason, stopSequenceValue = claudeStopReasonWithSequence(stopReason, visibleText, clientStopSequences)
	claudeUsage := map[string]any{"output_tokens": usage.OutputTokens}
	if usage.InputTokens > 0 || usage.CacheTokens > 0 {
		// Anthropic clients expect input_tokens to exclude cache hits.
		claudeUsage["input_tokens"] = claudeExclusiveInputTokens(usage, 0)
	}
	if usage.CacheTokens > 0 {
		claudeUsage["cache_read_input_tokens"] = usage.CacheTokens
	}
	if err := writeClaudeSSE(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": stopSequenceValue},
		"usage": claudeUsage,
	}); err != nil {
		return usage, err
	}
	if err := writeClaudeSSE(w, "message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return usage, err
	}
	return usage, nil
}
