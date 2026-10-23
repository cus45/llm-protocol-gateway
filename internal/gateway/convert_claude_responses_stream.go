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

// streamResponsesToClaudeEventsDirect reads OpenAI Responses SSE and writes
// Anthropic Messages SSE directly (no Chat intermediate).
func streamResponsesToClaudeEventsDirect(w http.ResponseWriter, reader io.Reader, model string) (TokenUsage, error) {
	messageID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	resolvedModel := strings.TrimSpace(model)
	messageStarted := false
	blockIndex := 0
	textBlockOpen := false
	thinkingBlockOpen := false
	toolBlocksOpen := map[string]int{} // call_id -> block index
	// Responses deltas reference the output item (`item_id`, `output_index`),
	// not call_id, and parallel calls can be open at the same time. Map both
	// back to the call_id that owns the Claude tool_use block.
	toolCallByItemID := map[string]string{}
	toolCallByOutputIndex := map[int]string{}
	// toolArgsEmitted is the argument text already streamed per call. The final
	// arguments in done / item.done repeat it, and the client concatenates every
	// partial_json, so only the not-yet-sent suffix may be emitted.
	toolArgsEmitted := map[string]string{}
	usage := TokenUsage{}
	stopReason := "end_turn"
	hasToolUse := false

	ensureMessageStart := func() error {
		if messageStarted {
			return nil
		}
		messageStarted = true
		return writeClaudeSSE(w, "message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":      messageID,
				"type":    "message",
				"role":    "assistant",
				"model":   firstNonEmpty(resolvedModel, model),
				"content": []any{},
				"usage": map[string]any{
					"input_tokens":  0,
					"output_tokens": 0,
				},
			},
		})
	}

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
		if !thinkingBlockOpen {
			return nil
		}
		if err := writeClaudeSSE(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": blockIndex,
		}); err != nil {
			return err
		}
		thinkingBlockOpen = false
		blockIndex++
		return nil
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
		return nil
	}

	startThinkingBlock := func() error {
		if thinkingBlockOpen {
			return nil
		}
		if err := closeTextBlock(); err != nil {
			return err
		}
		if err := writeClaudeSSE(w, "content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         blockIndex,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		}); err != nil {
			return err
		}
		thinkingBlockOpen = true
		return nil
	}

	emitTextDelta := func(text string) error {
		if text == "" {
			return nil
		}
		if err := ensureMessageStart(); err != nil {
			return err
		}
		if err := startTextBlock(); err != nil {
			return err
		}
		return writeClaudeSSE(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
	}

	emitThinkingDelta := func(text string) error {
		if text == "" {
			return nil
		}
		if err := ensureMessageStart(); err != nil {
			return err
		}
		if err := startThinkingBlock(); err != nil {
			return err
		}
		return writeClaudeSSE(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": text},
		})
	}

	openToolBlock := func(callID, name, itemID string, outputIndex int) error {
		if callID == "" || name == "" {
			return nil
		}
		// Record the identities even for a re-announced item: later deltas may
		// only carry item_id / output_index.
		if itemID != "" {
			toolCallByItemID[itemID] = callID
		}
		if outputIndex >= 0 {
			toolCallByOutputIndex[outputIndex] = callID
		}
		if _, exists := toolBlocksOpen[callID]; exists {
			return nil
		}
		if err := ensureMessageStart(); err != nil {
			return err
		}
		if err := closeTextBlock(); err != nil {
			return err
		}
		if err := closeThinkingBlock(); err != nil {
			return err
		}
		if err := writeClaudeSSE(w, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": blockIndex,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    sanitizeAnthropicToolUseID(callID),
				"name":  name,
				"input": map[string]any{},
			},
		}); err != nil {
			return err
		}
		toolBlocksOpen[callID] = blockIndex
		// Every tool_use owns its index: parallel calls that shared one index
		// were indistinguishable to the client and their arguments collided.
		blockIndex++
		hasToolUse = true
		return nil
	}

	// toolCallFor resolves the call_id a Responses tool event belongs to, by
	// call_id, then item_id, then output_index.
	toolCallFor := func(callID, itemID string, outputIndex int) string {
		if callID != "" {
			return callID
		}
		if itemID != "" {
			if id, ok := toolCallByItemID[itemID]; ok {
				return id
			}
		}
		if outputIndex >= 0 {
			if id, ok := toolCallByOutputIndex[outputIndex]; ok {
				return id
			}
		}
		return ""
	}

	emitToolArgsDelta := func(callID, partial string) error {
		if partial == "" {
			return nil
		}
		idx, ok := toolBlocksOpen[callID]
		if !ok {
			return nil
		}
		return writeClaudeSSE(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
		})
	}

	closeToolBlock := func(callID string) error {
		idx, ok := toolBlocksOpen[callID]
		if !ok {
			return nil
		}
		if err := writeClaudeSSE(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": idx,
		}); err != nil {
			return err
		}
		delete(toolBlocksOpen, callID)
		return nil
	}

	closed := false
	r := countingReader{r: reader}
	scanner := bufio.NewScanner(&r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	eventName := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
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
		if payload == "" || payload == "[DONE]" {
			continue
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if errorValue, ok := chunk["error"]; ok {
			_ = ensureMessageStart()
			msg, typ := errorMessageFromValue(errorValue, "upstream stream error")
			_ = writeClaudeSSE(w, "error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": typ, "message": msg},
			})
			return usage, fmt.Errorf("%s", msg)
		}

		if response, ok := chunk["response"].(map[string]any); ok {
			if value := stringValue(response["model"]); value != "" {
				resolvedModel = value
			}
			if id := stringValue(response["id"]); id != "" {
				if strings.HasPrefix(id, "resp_") {
					messageID = "msg_" + strings.TrimPrefix(id, "resp_")
				} else {
					messageID = id
				}
			}
			if usageMap, ok := response["usage"].(map[string]any); ok {
				usage.InputTokens = int64FromAny(usageMap["input_tokens"])
				usage.OutputTokens = int64FromAny(usageMap["output_tokens"])
				if details, ok := usageMap["input_tokens_details"].(map[string]any); ok {
					usage.CacheTokens = int64FromAny(details["cached_tokens"])
				}
			}
		}
		if chunkUsage := ParseResponsesUsage([]byte(payload)); chunkUsage.InputTokens > 0 || chunkUsage.OutputTokens > 0 || chunkUsage.CacheTokens > 0 {
			usage = chunkUsage
		}
		// Tool events carry the output position (absent → -1 means "unknown").
		outputIndex := -1
		if raw, exists := chunk["output_index"]; exists {
			outputIndex = int(int64FromAny(raw))
		}
		// Responses SSE names the event both ways: an `event:` line, and a
		// `type` field in the payload. Some upstreams send only the latter, so
		// the event: line must not be the only source.
		kind := eventName
		if kind == "" || chunk["type"] != nil {
			if typed := stringValue(chunk["type"]); typed != "" {
				kind = typed
			}
		}

		switch kind {
		case "response.output_text.delta":
			if err := emitTextDelta(stringValue(chunk["delta"])); err != nil {
				return usage, err
			}
		case "response.output_text.done":
			// text already streamed via deltas; no-op
		case "response.reasoning_summary_text.delta":
			if err := emitThinkingDelta(stringValue(chunk["delta"])); err != nil {
				return usage, err
			}
		case "response.output_item.added":
			if item, ok := chunk["item"].(map[string]any); ok {
				switch stringValue(item["type"]) {
				case "function_call":
					if err := openToolBlock(stringValue(item["call_id"]), stringValue(item["name"]), stringValue(item["id"]), outputIndex); err != nil {
						return usage, err
					}
				case "reasoning":
					// Prefer restoring signed thinking if encrypted_content is ours.
					if block, ok := decodeAnthropicThinkingBlock(stringValue(item["encrypted_content"])); ok {
						if err := ensureMessageStart(); err != nil {
							return usage, err
						}
						if err := closeTextBlock(); err != nil {
							return usage, err
						}
						if err := closeThinkingBlock(); err != nil {
							return usage, err
						}
						if err := writeClaudeSSE(w, "content_block_start", map[string]any{
							"type":          "content_block_start",
							"index":         blockIndex,
							"content_block": block,
						}); err != nil {
							return usage, err
						}
						thinkingBlockOpen = true
					}
				}
			}
		case "response.function_call_arguments.delta":
			// Responses deltas reference item_id / output_index; open tools are
			// keyed by call_id, so resolve through the identity map instead of
			// guessing whenever a single tool happens to be open.
			callID := toolCallFor("", stringValue(chunk["item_id"]), outputIndex)
			if callID == "" {
				continue
			}
			partial := stringValue(chunk["delta"])
			if err := emitToolArgsDelta(callID, partial); err != nil {
				return usage, err
			}
			toolArgsEmitted[callID] += partial
		case "response.function_call_arguments.done":
			// The done event repeats the complete arguments; if the upstream
			// only sent the final arguments here (no deltas), emit them so the
			// client still gets the payload. The block itself is closed by
			// output_item.done (or at end of stream).
			callID := toolCallFor("", stringValue(chunk["item_id"]), outputIndex)
			if callID == "" {
				continue
			}
			if arguments := stringValue(chunk["arguments"]); arguments != "" && arguments != "{}" {
				if tail := unstremittedArgs(arguments, toolArgsEmitted[callID]); tail != "" {
					if err := emitToolArgsDelta(callID, tail); err != nil {
						return usage, err
					}
					toolArgsEmitted[callID] += tail
				}
			}
		case "response.output_item.done":
			if item, ok := chunk["item"].(map[string]any); ok {
				if stringValue(item["type"]) == "function_call" {
					callID := toolCallFor(stringValue(item["call_id"]), stringValue(item["id"]), outputIndex)
					// An upstream that sent no deltas at all only reveals the
					// arguments here; emit whatever has not been streamed yet.
					if arguments := stringValue(item["arguments"]); callID != "" && arguments != "" && arguments != "{}" {
						if tail := unstremittedArgs(arguments, toolArgsEmitted[callID]); tail != "" {
							if err := emitToolArgsDelta(callID, tail); err != nil {
								return usage, err
							}
							toolArgsEmitted[callID] += tail
						}
					}
					if err := closeToolBlock(callID); err != nil {
						return usage, err
					}
				}
			}
		case "response.completed", "response.incomplete", "response.failed":
			closed = true
			if response, ok := chunk["response"].(map[string]any); ok {
				incompleteReason := ""
				if details, ok := response["incomplete_details"].(map[string]any); ok {
					incompleteReason = stringValue(details["reason"])
				}
				stopReason = mapResponsesStatusToClaudeStopReason(stringValue(response["status"]), incompleteReason, hasToolUse)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return usage, newUpstreamStreamInterruption(err, r.n)
	}
	// Decided from what the reads actually saw: before the loop nothing has
	// been read, a normal end of body is io.EOF, and a body that ends without
	// a terminal response event was cut however cleanly the socket closed.
	if isUpstreamStreamInterruptedReader(r) || cleanEOFInterruption(r, closed) {
		turn := claudeInterruptedTurn{
			messageStarted:    messageStarted,
			textBlockOpen:     textBlockOpen,
			thinkingBlockOpen: thinkingBlockOpen,
			toolOpen:          toolBlocksOpen,
			nextIndex:         blockIndex,
		}
		return usage, closeInterruptedClaudeStream(w, turn, usage, r.n)
	}

	if !messageStarted {
		return usage, fmt.Errorf("responses stream ended without any events")
	}
	if err := closeTextBlock(); err != nil {
		return usage, err
	}
	if err := closeThinkingBlock(); err != nil {
		return usage, err
	}
	for callID := range toolBlocksOpen {
		if err := closeToolBlock(callID); err != nil {
			return usage, err
		}
	}
	if hasToolUse && stopReason == "end_turn" {
		stopReason = "tool_use"
	}
	if err := writeClaudeSSE(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{
			"output_tokens": usage.OutputTokens,
		},
	}); err != nil {
		return usage, err
	}
	if err := writeClaudeSSE(w, "message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return usage, err
	}
	return usage, nil
}

// unstremittedArgs returns the part of full that has not been streamed yet.
// Deltas and the final argument string describe the same JSON, so the final
// string normally starts with everything already sent; the remainder is what
// still has to go out. A final string that does not extend what was streamed
// is sent whole (the client would otherwise hold a corrupted fragment).
func unstremittedArgs(full, streamed string) string {
	if streamed == "" {
		return full
	}
	if strings.HasPrefix(full, streamed) {
		return full[len(streamed):]
	}
	return full
}
