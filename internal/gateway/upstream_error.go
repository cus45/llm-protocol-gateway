package gateway

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// summarizeUpstreamHTTPError builds a client/log-facing message from an upstream
// HTTP status and body. Prefers structured error.message when present. A vendor
// that refuses the request's own prompt, or a history whose tool turns cannot be
// paired, carries what the user can do about it.
func summarizeUpstreamHTTPError(status int, body []byte) string {
	message := summarizeUpstreamErrorBody(status, body)
	if workBuddyPromptRefused(status, body) && !strings.Contains(message, WorkBuddyRefusedNote) {
		message += " " + WorkBuddyRefusedNote
	}
	if chatToolDialectRefused(status, body) && !strings.Contains(message, chatToolDialectNote) {
		message += " " + chatToolDialectNote
	}
	return message
}

// chatToolDialectWords are how an upstream says the history's tool turns do not
// add up: WorkBuddy's 11133 / 11148, and the "unsupported content type …
// tool_use" a Chat endpoint answers to a request that still carries Anthropic's
// content blocks. Both come from the dialect the client sent, not from the
// account.
var chatToolDialectWords = regexp.MustCompile(`(?i)tool calls and tool results do not match|unsupported content type|["']code["']\s*:\s*["']?111(?:33|48)\b`)

const chatToolDialectNote = "这条历史里的工具调用与工具结果配不上：多半是客户端往 Chat 接口发了带 tool_use/tool_result 块的 Anthropic 结构。网关已按位置补齐缺失的调用 id，仍然被拒就请新建会话，或让该客户端按标准 Chat 方言（assistant.tool_calls + role:tool）发送。"

// chatToolDialectRefused says an upstream answer is that refusal.
func chatToolDialectRefused(status int, body []byte) bool {
	return status >= 400 && chatToolDialectWords.Match(body)
}

func summarizeUpstreamErrorBody(status int, body []byte) string {
	if msg := extractResponseErrorMessage(body); msg != "" {
		if status > 0 {
			return fmt.Sprintf("HTTP %d: %s", status, msg)
		}
		return msg
	}
	snippet := strings.TrimSpace(string(body))
	if snippet == "" {
		if status > 0 {
			return fmt.Sprintf("HTTP %d", status)
		}
		return "upstream request failed"
	}
	if !utf8.ValidString(snippet) {
		snippet = string([]rune(snippet))
	}
	const maxLen = 500
	if len(snippet) > maxLen {
		snippet = snippet[:maxLen] + "…"
	}
	if status > 0 {
		return fmt.Sprintf("HTTP %d: %s", status, snippet)
	}
	return snippet
}

// errorMessageFromValue extracts a human-readable message from a protocol error
// object, falling back to a JSON/string dump instead of a generic placeholder.
func errorMessageFromValue(errorValue any, fallback string) (message string, errorType string) {
	errorType = "api_error"
	message = strings.TrimSpace(fallback)
	if message == "" {
		message = "upstream request failed"
	}
	if item, ok := errorValue.(map[string]any); ok {
		if value := stringValue(item["message"]); value != "" {
			message = value
		} else if value := stringValue(item["msg"]); value != "" {
			message = value
		} else if value := stringValue(item["detail"]); value != "" {
			message = value
		} else if value := stringValue(item["error"]); value != "" {
			message = value
		} else if raw, err := json.Marshal(item); err == nil && len(raw) > 0 && string(raw) != "{}" {
			message = string(raw)
		}
		if value := stringValue(item["type"]); value != "" {
			errorType = value
		} else if value := stringValue(item["code"]); value != "" {
			errorType = value
		}
		return message, errorType
	}
	if errorValue != nil {
		if text := strings.TrimSpace(stringValue(errorValue)); text != "" {
			message = text
		}
	}
	return message, errorType
}

func errorValueOrBody(payload map[string]any, status int, body []byte) any {
	if payload != nil {
		if errorValue, ok := payload["error"]; ok && errorValue != nil {
			return errorValue
		}
		if detail := strings.TrimSpace(stringValue(payload["detail"])); detail != "" {
			return map[string]any{
				"type":    "api_error",
				"message": detail,
			}
		}
	}
	return map[string]any{
		"type":    "api_error",
		"message": summarizeUpstreamHTTPError(status, body),
	}
}
