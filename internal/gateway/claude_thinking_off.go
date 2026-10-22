package gateway

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Claude 模型族解析与「关闭 thinking」的写法。
//
// 各家 Claude 对 thinking.type=disabled 的接受度并不一致（对齐 magpie 的
// thinkingOffOf）：4.x / Sonnet 5 / Haiku 5 原样接受；Opus 5、Haiku 5.5 只
// 在 effort 不高于 high 时接受；Sonnet 5.5 直接 400，要求改用
// thinking.type=between_tools；Opus 5.5、Fable、Mythos 任何 effort 都拒绝
// disabled（它们只能自适应思考），只能把 thinking 去掉。
//
// 客户端（Claude Code 等）并不知道自己到达的是哪一代模型，常按目录名发
// disabled，于是这些模型会把本可以正常完成的请求 400 掉。这里在请求落地前
// 按「实际上游模型」改写；未知模型一律原样透传，不猜。

// claudeModelLine finds a Claude model's family and version however a relay
// spells it: claude-opus-5-5, claude-sonnet-5.5, a relay's sonnet-5-5,
// Bedrock's anthropic.claude-sonnet-5-5-v1:0, Vertex's
// claude-sonnet-5-5@20261001, claude-fable-5-1.
var claudeModelLine = regexp.MustCompile(`(?:^|[^a-z0-9])(?:claude-)?(opus|sonnet|haiku|fable|mythos)-(\d{1,2})(?:[-.](\d{1,2}))?(?:[^0-9]|$)`)

type claudeThinkingOff int

const (
	claudeOffUnknown      claudeThinkingOff = iota // 不认识的模型：原样透传
	claudeOffTaken                                 // 任意 effort 都接受 disabled
	claudeOffUpToHigh                              // 仅 effort ≤ high 时接受 disabled（Opus 5、Haiku 5.5）
	claudeOffBetweenTools                          // 拒绝 disabled，要求 between_tools（Sonnet 5.5）
	claudeOffNever                                 // 任何 effort 都拒绝 disabled（Opus 5.5、Fable、Mythos）
)

// claudeThinkingOffOf is how model takes thinking turned off and whether the
// model is a Claude thinking line at all.
func claudeThinkingOffOf(model string) (claudeThinkingOff, bool) {
	match := claudeModelLine.FindStringSubmatch(strings.ToLower(strings.TrimSpace(model)))
	if match == nil {
		return claudeOffUnknown, false
	}
	major, _ := strconv.Atoi(match[2])
	minor, _ := strconv.Atoi(match[3])
	switch match[1] {
	case "fable", "mythos":
		if major == 5 {
			return claudeOffNever, true
		}
	case "opus", "sonnet", "haiku":
		switch {
		case major < 5:
			return claudeOffTaken, true
		case major == 5 && minor == 0:
			if match[1] == "opus" {
				return claudeOffUpToHigh, true
			}
			return claudeOffTaken, true
		case major == 5 && minor == 5:
			switch match[1] {
			case "sonnet":
				return claudeOffBetweenTools, true
			case "opus":
				return claudeOffNever, true
			case "haiku":
				return claudeOffUpToHigh, true
			}
		}
	}
	return claudeOffUnknown, false
}

// classifierRoom is what a model that always reasons is given on top of a
// reply that was budgeted for a model that doesn't (magpie's classifierRoom).
// Claude Code's title / classifier requests ask for a few dozen tokens, which
// an always-thinking model spends on thinking alone and returns empty.
const classifierRoom = 2048

// normalizeClaudeThinkingOff rewrites a Messages payload that turns thinking
// off into the shape model actually accepts, and reports whether payload
// changed. effortAboveHigh is whether the payload asked output_config.effort
// xhigh/max.
func normalizeClaudeThinkingOff(payload map[string]any, model string) bool {
	if payload == nil {
		return false
	}
	thinking, _ := payload["thinking"].(map[string]any)
	if strings.ToLower(strings.TrimSpace(stringValue(thinking["type"]))) != "disabled" {
		return false
	}
	rule, known := claudeThinkingOffOf(model)
	if !known {
		return false
	}
	effort := strings.ToLower(strings.TrimSpace(outputConfigEffort(payload)))
	aboveHigh := effort == "xhigh" || effort == "max"
	switch rule {
	case claudeOffTaken:
		return false
	case claudeOffUpToHigh:
		if !aboveHigh {
			return false
		}
		setOutputConfigEffort(payload, "high")
		return true
	case claudeOffBetweenTools:
		// between_tools 不接受其它字段（display、budget_tokens），effort 也只能到 high。
		payload["thinking"] = map[string]any{"type": "between_tools"}
		if aboveHigh {
			setOutputConfigEffort(payload, "high")
		}
		return true
	case claudeOffNever:
		// 这类模型只能自适应思考：去掉 thinking，没指定 effort 时给 low，
		// 并把给「不思考模型」准备的过小预算加上思考所需余量。
		delete(payload, "thinking")
		if effort == "" {
			setOutputConfigEffort(payload, "low")
		}
		if n, ok := intFromAny(payload["max_tokens"]); ok && n > 0 && n < classifierRoom {
			payload["max_tokens"] = n + classifierRoom
		}
		return true
	default:
		return false
	}
}

func outputConfigEffort(payload map[string]any) string {
	config, _ := payload["output_config"].(map[string]any)
	return stringValue(config["effort"])
}

func setOutputConfigEffort(payload map[string]any, effort string) {
	config, _ := payload["output_config"].(map[string]any)
	if config == nil {
		config = map[string]any{}
	}
	config["effort"] = effort
	payload["output_config"] = config
}

// intFromAny reads a JSON number (float64, json.Number, int kinds) as int.
func intFromAny(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case json.Number:
		n, err := typed.Int64()
		return int(n), err == nil
	}
	return 0, false
}

// normalizeClaudeThinkingOffBody applies normalizeClaudeThinkingOff to a raw
// Messages body for provider's (mapped) model; a no-op when nothing changes.
func normalizeClaudeThinkingOffBody(body []byte, model string) []byte {
	if len(body) == 0 {
		return body
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	var payload map[string]any
	if decoder.Decode(&payload) != nil || payload == nil {
		return body
	}
	if !normalizeClaudeThinkingOff(payload, model) {
		return body
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return rewritten
}

// claudeBodyField reads one top-level field of a raw JSON body, nil when absent.
func claudeBodyField(body []byte, key string) any {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	return payload[key]
}

// removeClaudeThinkingOff deletes a Claude payload's thinking when it turns
// thinking off, keeping any output_config effort, and reports whether it did.
func removeClaudeThinkingOff(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	thinking, _ := payload["thinking"].(map[string]any)
	if strings.ToLower(strings.TrimSpace(stringValue(thinking["type"]))) != "disabled" {
		return false
	}
	delete(payload, "thinking")
	return true
}

// claudeEndpointReasoningEffort is the Chat-style reasoning_effort that a
// non-Claude model on an Anthropic endpoint reads as thinking off. GLM-5.2 /
// GLM-5.3 take "min": their levels are min/low/medium/high/max and "none" is
// rejected. Left absent when the model isn't known to take the field.
func claudeEndpointReasoningEffort(model string) (string, bool) {
	id := normalizeModelContextID(model)
	switch {
	case strings.Contains(id, "glm-5.2"), strings.Contains(id, "glm-5.3"):
		return "min", true
	case strings.Contains(id, "glm-"):
		return "none", true
	case strings.Contains(id, "deepseek"), strings.Contains(id, "kimi"), strings.Contains(id, "moonshot"):
		return "none", true
	}
	return "", false
}

// normalizeClaudeEndpointThinkingOffBody is normalizeClaudeThinkingOff plus
// the non-Anthropic handling, for a raw Messages body relayed as it came.
func normalizeClaudeEndpointThinkingOffBody(body []byte, model string) []byte {
	if len(body) == 0 {
		return body
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	var payload map[string]any
	if decoder.Decode(&payload) != nil || payload == nil {
		return body
	}
	rule, knownClaude := claudeThinkingOffOf(model)
	// 先按原预算判断是否要补余量：normalizeClaudeThinkingOff 对只能自适应
	// 思考的 Claude 已经补过一次，这里不能再叠加。
	budget, hasBudget := intFromAny(payload["max_tokens"])
	shortBudget := hasBudget && budget > 0 && budget < claudeEndpointThinkingRoom
	changed := normalizeClaudeThinkingOff(payload, model)
	// 非 Claude 模型：端点不解析 thinking，disabled 等于没写、模型照样思考
	// （实测 max_tokens=200 时思考吃光预算、正文为空）。删掉 thinking，并显式
	// 给出该端点"不思考"的 reasoning_effort；同时给短预算留出思考余量。
	if !knownClaude {
		if removeClaudeThinkingOff(payload) {
			if effort, ok := claudeEndpointReasoningEffort(model); ok {
				payload["reasoning_effort"] = effort
			}
			if shortBudget {
				padClaudeEndpointBudget(payload, model, budget)
			}
			changed = true
		}
	} else if rule == claudeOffNever && shortBudget {
		// 只能自适应思考的 Claude 同样关不掉思考。它可能已经被
		// normalizeClaudeThinkingOff 补过余量，这里只补到目标值。
		if after, ok := intFromAny(payload["max_tokens"]); ok && after == budget {
			padClaudeEndpointBudget(payload, model, budget)
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

// padClaudeEndpointBudget gives a thinking-on-by-default model room on top of
// a short reply (see classifierRoom), so a title/classifier-sized request
// doesn't come back empty. Only used where thinking can't be turned off.
func padClaudeEndpointBudget(payload map[string]any, model string, requested int) {
	if payload == nil {
		return
	}
	n, ok := intFromAny(payload["max_tokens"])
	if !ok || n <= 0 {
		return
	}
	// 之前补过的余量要算进去，避免短预算反复下探到余量边缘。
	room := claudeEndpointThinkingRoom - n
	if requested > n {
		room = claudeEndpointThinkingRoom - requested
	}
	if classifierRoom < room {
		room = classifierRoom
	}
	if room <= 0 {
		return
	}
	payload["max_tokens"] = n + room
}

// claudeEndpointThinkingRoom is how much room a non-Claude model that thinks
// by default gets on top of a short reply. 8192 covers GLM-5.3's deepest
// thinking: live probes with max_tokens=2000 still returned an empty answer
// (thinking alone used the whole budget), while 8192 always produced text.
const claudeEndpointThinkingRoom = 8192
