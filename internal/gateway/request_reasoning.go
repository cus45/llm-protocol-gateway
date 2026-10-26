package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The console's 推理强度 column needs one label per request, but the protocols
// spell the setting four ways: OpenAI Chat has `reasoning_effort`, Responses
// nests it under `reasoning.effort`, Claude carries either a `thinking` block
// (type + budget) or the newer `output_config.effort`. This reads whichever is
// present, most-explicit-first.
//
// It runs on the decoded payload, not on the logged request body: request bodies
// are truncated in storage (~110KB today), and a Claude Code request is bigger
// than that, so parsing the stored copy silently yields nothing — which is
// exactly how the column came out empty in production.

func reasoningLabelFromMap(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	if effort := stringField(payload, "reasoning_effort"); effort != "" {
		return effort
	}
	if effort := nestedStringField(payload, "reasoning", "effort"); effort != "" {
		return effort
	}
	if effort := nestedStringField(payload, "output_config", "effort"); effort != "" {
		return effort
	}
	if thinking, ok := payload["thinking"].(map[string]any); ok {
		kind := stringField(thinking, "type")
		if kind == "" {
			return ""
		}
		if budget := int64FromAny(thinking["budget_tokens"]); budget > 0 {
			return fmt.Sprintf("%s·%d", kind, budget)
		}
		// 自适应思考用 thinking.effort 表达强度（不是预算）：控制台的推理强度
		// 一列不能只剩一个 "adaptive"。
		if effort := stringField(thinking, "effort"); effort != "" {
			return fmt.Sprintf("%s·%s", kind, effort)
		}
		return kind
	}
	return ""
}

func stringField(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func nestedStringField(payload map[string]any, object, key string) string {
	nested, ok := payload[object].(map[string]any)
	if !ok {
		return ""
	}
	return stringField(nested, key)
}

// requestReasoningLabel is the fallback for request paths that never stash the
// label on the context. A truncated or unparsable body simply yields nothing.
func requestReasoningLabel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return reasoningLabelFromMap(payload)
}

type reasoningEffortKey struct{}

// withReasoningEffort records the effective reasoning setting on the request so
// it reaches the log even when the stored body is truncated, and even when the
// value came from the route/key default rather than the client.
func withReasoningEffort(r *http.Request, effort string) *http.Request {
	if strings.TrimSpace(effort) == "" {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), reasoningEffortKey{}, strings.TrimSpace(effort)))
}

func reasoningEffortFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(reasoningEffortKey{}).(string)
	return strings.TrimSpace(value)
}
