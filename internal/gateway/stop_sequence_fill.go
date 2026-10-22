package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

// 停止序列回填。
//
// 客户端（Claude Code 等）发 stop_sequences 时，只在收到 stop_reason=
// stop_sequence 且 stop_sequence 为具体字符串时才认定"按预期停下"；上游是
// Chat / Responses 模型时，跨协议只能看到 finish_reason=stop，于是这些客户端
// 会把正常的停止当成异常结束（有的还会据此重试或续写）。
//
// 网关在发出请求前把这些"我们自己转换出来的"停止序列记进请求 context，响应
// 落地时若可见正文以其中之一结尾，就回填 stop_sequence 与 stop_reason。只记
// 转换产生的序列，直通 Claude 请求不带 context 值，因此不会覆盖上游自己的答案。

type stopSequencesContextKey struct{}

// withConvertedStopSequences 记录本次请求由转换器放进上游的停止序列。
func withConvertedStopSequences(r *http.Request, sequences []string) *http.Request {
	if r == nil || len(sequences) == 0 {
		return r
	}
	return r.WithContext(withStopSequences(r.Context(), sequences))
}

func withStopSequences(ctx context.Context, sequences []string) context.Context {
	return context.WithValue(ctx, stopSequencesContextKey{}, append([]string(nil), sequences...))
}

func convertedStopSequences(r *http.Request) []string {
	if r == nil || r.Context() == nil {
		return nil
	}
	sequences, _ := r.Context().Value(stopSequencesContextKey{}).([]string)
	return sequences
}

// matchedStopSequence returns the first of sequences that text ends with, and
// "" when none does. An empty sequence never matches (it would "end" any text).
func matchedStopSequence(text string, sequences []string) string {
	if text == "" {
		return ""
	}
	for _, sequence := range sequences {
		if sequence != "" && strings.HasSuffix(text, sequence) {
			return sequence
		}
	}
	return ""
}

// fillNonStreamStopSequence rewrites body (a Chat or Responses JSON answer) so a
// turn that visibly ended on one of the converted stop sequences reports it the
// way Anthropic does: finish_reason/status "stop" plus the matched sequence.
func fillNonStreamStopSequence(body []byte, sequences []string) []byte {
	if len(sequences) == 0 || len(body) == 0 {
		return body
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return body
	}
	changed := false
	if matched := matchedStopSequence(firstChatChoiceVisibleText(payload), sequences); matched != "" {
		// stop_sequence sits beside the choices (OpenAI's own placement for
		// logprobs.choices) rather than inside the message.
		payload["stop_sequence"] = matched
		changed = true
	}
	if _, isResponses := payload["output"]; isResponses {
		if matched := matchedStopSequence(responsesOutputText(payload["output"]), sequences); matched != "" {
			payload["status"] = "completed"
			payload["stop_sequence"] = matched
			changed = true
		}
	}
	if !changed {
		return body
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return rewritten
}

// chatChoiceVisibleText is the assistant text of one Chat choice, content and
// streaming delta alike.
func chatChoiceVisibleText(choice map[string]any) string {
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		message, _ = choice["delta"].(map[string]any)
	}
	if message == nil {
		return ""
	}
	if text, ok := message["content"].(string); ok {
		return text
	}
	if parts, ok := message["content"].([]any); ok {
		var builder strings.Builder
		for _, raw := range parts {
			if part, ok := raw.(map[string]any); ok {
				builder.WriteString(stringValue(part["text"]))
			}
		}
		return builder.String()
	}
	return ""
}

// claudeStopReasonWithSequence is the stop_reason / stop_sequence pair a
// Claude-format answer carries when its visible text ends on a converted
// sequence; otherwise the upstream reason stands with a nil sequence.
func claudeStopReasonWithSequence(stopReason, text string, sequences []string) (string, any) {
	if matched := matchedStopSequence(text, sequences); matched != "" {
		return "stop_sequence", matched
	}
	return stopReason, nil
}

// claudeBlocksVisibleText concatenates the text blocks of Claude content.
func claudeBlocksVisibleText(blocks []any) string {
	var builder strings.Builder
	for _, raw := range blocks {
		if block, ok := raw.(map[string]any); ok && stringValue(block["type"]) == "text" {
			builder.WriteString(stringValue(block["text"]))
		}
	}
	return builder.String()
}

// openAIAssistantVisibleText is the assistant content of a Chat message.
func openAIAssistantVisibleText(message map[string]any) string {
	return chatChoiceVisibleText(map[string]any{"message": message})
}

// rememberConvertedStopSequences records the stop sequences a converter placed
// in an upstream request (Claude stop_sequences / Chat stop / Responses stop)
// onto the in-flight request, and returns them for the response converter to
// read back. r itself is only replaced when there is something to remember.
func rememberConvertedStopSequences(r *http.Request, raw any) (*http.Request, []string) {
	sequences := stopSequencesFromAny(raw)
	if len(sequences) == 0 {
		return r, nil
	}
	return withConvertedStopSequences(r, sequences), sequences
}

// firstChatChoiceVisibleText is the assistant text of the first Chat choice of
// a whole response body.
func firstChatChoiceVisibleText(payload map[string]any) string {
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	return chatChoiceVisibleText(choice)
}

func tailForStopSequences(text string, sequences []string) string {
	if len(sequences) == 0 {
		return text
	}
	longest := 0
	for _, sequence := range sequences {
		if n := len([]rune(sequence)); n > longest {
			longest = n
		}
	}
	keep := longest + 2
	// Keep the raw bytes of the last keep runes: cut never lands inside a rune,
	// and the window is all a suffix match needs.
	keepBytes := 0
	for cut, kept := len(text), 0; cut > 0 && kept < keep; kept++ {
		_, size := utf8.DecodeLastRuneInString(text[:cut])
		if size <= 0 {
			break
		}
		cut -= size
		keepBytes += size
	}
	return text[len(text)-keepBytes:]
}
