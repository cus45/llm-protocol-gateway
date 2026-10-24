package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// proxyGeminiToOpenAIChat serves a Gemini generateContent request through an
// OpenAI-compatible provider: the request hop has no state to carry, and the
// response hop is a pure body conversion.
func (s *Server) proxyGeminiToOpenAIChat(w http.ResponseWriter, r *http.Request, provider domain.Provider, model string, geminiReq map[string]any, skipIncomingAuth bool) (int, TokenUsage, []byte, error) {
	chatReq, err := geminiToOpenAIChatRequest(geminiReq, model)
	if err != nil {
		return 0, TokenUsage{}, nil, err
	}
	stream, _ := geminiReq["stream"].(bool)
	if stream {
		chatReq["stream"] = true
	}
	return s.proxyConvertedThroughChat(w, r, provider, model, chatReq, skipIncomingAuth,
		func(chatBody []byte, model string) ([]byte, TokenUsage, error) {
			var parsed map[string]any
			if err := json.Unmarshal(chatBody, &parsed); err != nil {
				return nil, TokenUsage{}, err
			}
			gemini, err := openAIChatToGeminiResponse(parsed, model)
			if err != nil {
				return nil, TokenUsage{}, err
			}
			usage := geminiUsageFromChatUsage(parsed)
			body, err := json.Marshal(gemini)
			if err != nil {
				return nil, TokenUsage{}, err
			}
			return body, usage, nil
		},
		streamOpenAIChatToGeminiEvents,
	)
}

// geminiUsageFromChatUsage reads the Chat usage block for logging. The Gemini
// body carries the same numbers under its own names.
func geminiUsageFromChatUsage(chat map[string]any) TokenUsage {
	usage, ok := chat["usage"].(map[string]any)
	if !ok {
		return TokenUsage{}
	}
	prompt, _ := intFromAny(usage["prompt_tokens"])
	completion, _ := intFromAny(usage["completion_tokens"])
	return TokenUsage{InputTokens: int64(prompt), OutputTokens: int64(completion)}
}

// streamOpenAIChatToGeminiEvents turns an OpenAI Chat SSE stream into the
// streamGenerateContent shape: every Chat delta becomes one candidate chunk,
// usage rides the final chunk, and the stream ends with the upstream's
// [DONE] swallowed (Gemini has no terminator frame).
//
// The shape is Google's "alt=sse" form: `data: {json}` frames, no `event:`
// line, which is what their own SDKs read.
func streamOpenAIChatToGeminiEvents(w http.ResponseWriter, reader io.Reader, model string) (TokenUsage, error) {
	total := TokenUsage{}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	sentAnything := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		if usage, ok := event["usage"].(map[string]any); ok {
			prompt, _ := intFromAny(usage["prompt_tokens"])
			completion, _ := intFromAny(usage["completion_tokens"])
			total.InputTokens, total.OutputTokens = int64(prompt), int64(completion)
		}
		choices := asMapSlice(event["choices"])
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		parts := make([]any, 0, 2)
		if text := stringValue(delta["content"]); text != "" {
			parts = append(parts, map[string]any{"text": text})
		}
		for _, raw := range asMapSlice(delta["tool_calls"]) {
			call, _ := raw.(map[string]any)
			function, _ := call["function"].(map[string]any)
			name := strings.TrimSpace(stringValue(function["name"]))
			if name == "" {
				continue
			}
			args := map[string]any{}
			if rawArgs := strings.TrimSpace(stringValue(function["arguments"])); rawArgs != "" {
				if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
					return total, fmt.Errorf("tool call %q has arguments that are not JSON: %w", name, err)
				}
			}
			parts = append(parts, map[string]any{"functionCall": map[string]any{"name": name, "args": args}})
		}
		if len(parts) == 0 {
			continue
		}
		chunk := map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{"role": "model", "parts": parts},
			}},
			"modelVersion": model,
		}
		if finish := stringValue(choice["finish_reason"]); finish != "" {
			chunk["candidates"].([]any)[0].(map[string]any)["finishReason"] = geminiFinishReason(finish)
		}
		body, err := json.Marshal(chunk)
		if err != nil {
			return total, err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", body); err != nil {
			return total, err
		}
		sentAnything = true
		if flusher != nil {
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		return total, err
	}
	if !sentAnything {
		// An empty stream is still an answer shape the client can parse; say so
		// rather than closing with nothing.
		body, _ := json.Marshal(map[string]any{
			"candidates":   []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{}}}},
			"modelVersion": model,
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
		if flusher != nil {
			flusher.Flush()
		}
	}
	if total.InputTokens > 0 || total.OutputTokens > 0 {
		body, _ := json.Marshal(map[string]any{
			"usageMetadata": map[string]any{
				"promptTokenCount":     total.InputTokens,
				"candidatesTokenCount": total.OutputTokens,
				"totalTokenCount":      total.InputTokens + total.OutputTokens,
			},
			"modelVersion": model,
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
		if flusher != nil {
			flusher.Flush()
		}
	}
	return total, nil
}
