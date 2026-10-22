package gateway

import (
	"encoding/json"
	"testing"
)

func TestClaudeThinkingOffOf(t *testing.T) {
	cases := []struct {
		model string
		want  claudeThinkingOff
		known bool
	}{
		// 4.x 与 Sonnet 5 / Haiku 5 原样接受 disabled。
		{"claude-opus-4-6", claudeOffTaken, true},
		{"claude-sonnet-4.5", claudeOffTaken, true},
		// 旧版命名顺序（claude-3-7-sonnet）不在规则内，按未知模型原样透传。
		{"claude-3-7-sonnet", claudeOffUnknown, false},
		{"claude-sonnet-5", claudeOffTaken, true},
		{"claude-haiku-5", claudeOffTaken, true},
		// Opus 5 / Haiku 5.5 只在 effort ≤ high 时接受。
		{"claude-opus-5", claudeOffUpToHigh, true},
		{"claude-haiku-5-5", claudeOffUpToHigh, true},
		// Sonnet 5.5 要求 between_tools。
		{"claude-sonnet-5.5", claudeOffBetweenTools, true},
		{"anthropic.claude-sonnet-5-5-v1:0", claudeOffBetweenTools, true},
		{"claude-sonnet-5-5@20261001", claudeOffBetweenTools, true},
		// 只能自适应思考的模型。
		{"claude-opus-5-5", claudeOffNever, true},
		{"claude-fable-5-1", claudeOffNever, true},
		{"claude-mythos-5", claudeOffNever, true},
		// 不是 Claude：不猜，原样透传。
		{"glm-5.3", claudeOffUnknown, false},
		{"gpt-5.6", claudeOffUnknown, false},
		{"", claudeOffUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			got, known := claudeThinkingOffOf(tc.model)
			if got != tc.want || known != tc.known {
				t.Fatalf("claudeThinkingOffOf(%q) = %v, %v; want %v, %v", tc.model, got, known, tc.want, tc.known)
			}
		})
	}
}

func TestNormalizeClaudeThinkingOff(t *testing.T) {
	disabled := func(extra map[string]any) map[string]any {
		payload := map[string]any{"model": "", "thinking": map[string]any{"type": "disabled"}}
		for k, v := range extra {
			payload[k] = v
		}
		return payload
	}
	effort := func(payload map[string]any) string {
		return outputConfigEffort(payload)
	}

	t.Run("taken stays byte-for-byte", func(t *testing.T) {
		payload := disabled(map[string]any{"output_config": map[string]any{"effort": "max"}})
		if normalizeClaudeThinkingOff(payload, "claude-sonnet-4.5") {
			t.Fatalf("4.5 accepts disabled: %#v", payload)
		}
		if effort(payload) != "max" {
			t.Fatalf("effort must be untouched")
		}
	})

	t.Run("unknown model untouched", func(t *testing.T) {
		payload := disabled(map[string]any{"output_config": map[string]any{"effort": "xhigh"}})
		if normalizeClaudeThinkingOff(payload, "glm-5.3") {
			t.Fatalf("non-Claude models must be left alone: %#v", payload)
		}
	})

	t.Run("no thinking untouched", func(t *testing.T) {
		payload := map[string]any{"model": "claude-opus-5-5"}
		if normalizeClaudeThinkingOff(payload, "claude-opus-5-5") {
			t.Fatalf("nothing to normalize: %#v", payload)
		}
	})

	t.Run("up to high clamps xhigh", func(t *testing.T) {
		payload := disabled(map[string]any{"output_config": map[string]any{"effort": "xhigh"}})
		if !normalizeClaudeThinkingOff(payload, "claude-opus-5") {
			t.Fatalf("xhigh must be clamped")
		}
		if effort(payload) != "high" {
			t.Fatalf("effort = %q, want high", effort(payload))
		}
		// high 或更低已可接受，不改动。
		payload = disabled(map[string]any{"output_config": map[string]any{"effort": "high"}})
		if normalizeClaudeThinkingOff(payload, "claude-opus-5") {
			t.Fatalf("high is accepted, must not change")
		}
	})

	t.Run("sonnet 5.5 asks between_tools", func(t *testing.T) {
		payload := disabled(map[string]any{"output_config": map[string]any{"effort": "max"}})
		if !normalizeClaudeThinkingOff(payload, "claude-sonnet-5.5") {
			t.Fatalf("must rewrite to between_tools")
		}
		thinking, _ := payload["thinking"].(map[string]any)
		if len(thinking) != 1 || thinking["type"] != "between_tools" {
			t.Fatalf("thinking = %#v, want between_tools alone", thinking)
		}
		if effort(payload) != "high" {
			t.Fatalf("effort = %q, want high", effort(payload))
		}
	})

	t.Run("always thinking drops disabled", func(t *testing.T) {
		payload := disabled(map[string]any{"max_tokens": 64})
		if !normalizeClaudeThinkingOff(payload, "claude-opus-5-5") {
			t.Fatalf("must drop thinking")
		}
		if _, exists := payload["thinking"]; exists {
			t.Fatalf("thinking must be gone: %#v", payload)
		}
		if effort(payload) != "low" {
			t.Fatalf("effort = %q, want low", effort(payload))
		}
		if payload["max_tokens"] != 64+classifierRoom {
			t.Fatalf("max_tokens = %#v, want a short reply's room for thinking", payload["max_tokens"])
		}

		// 客户端已给出 effort 与足够预算时不覆盖。
		payload = disabled(map[string]any{
			"max_tokens":    20000,
			"output_config": map[string]any{"effort": "high"},
		})
		normalizeClaudeThinkingOff(payload, "claude-fable-5")
		if effort(payload) != "high" || payload["max_tokens"] != 20000 {
			t.Fatalf("explicit effort and budget must stand: %#v", payload)
		}
	})
}

func TestNormalizeClaudeThinkingOffBody(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5.5","max_tokens":1024,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`)
	out := normalizeClaudeThinkingOffBody(body, "claude-sonnet-5.5")
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("not json: %v (%s)", err, out)
	}
	thinking, _ := payload["thinking"].(map[string]any)
	if thinking["type"] != "between_tools" {
		t.Fatalf("thinking = %#v", thinking)
	}
	// 其它字段逐字保留。
	if payload["max_tokens"] != float64(1024) || payload["model"] != "claude-sonnet-5.5" {
		t.Fatalf("other fields must survive: %s", out)
	}

	// 无需改写（或非 JSON）时逐字返回原 body。
	clean := []byte(`{"model":"claude-sonnet-4.5","thinking":{"type":"disabled"}}`)
	if got := normalizeClaudeThinkingOffBody(clean, "claude-sonnet-4.5"); string(got) != string(clean) {
		t.Fatalf("clean body changed: %s", got)
	}
	broken := []byte(`not json`)
	if got := normalizeClaudeThinkingOffBody(broken, "claude-sonnet-5.5"); string(got) != string(broken) {
		t.Fatalf("non-json changed: %s", got)
	}
}

func TestClaudeBodyField(t *testing.T) {
	if got := stringValue(claudeBodyField([]byte(`{"model":"claude-opus-5"}`), "model")); got != "claude-opus-5" {
		t.Fatalf("model = %q", got)
	}
	if got := claudeBodyField([]byte(`nope`), "model"); got != nil {
		t.Fatalf("garbage body must yield nil, got %#v", got)
	}
}

func TestNormalizeClaudeEndpointThinkingOffBody(t *testing.T) {
	// GLM 的 Anthropic 端点不解析 thinking：disabled 等于没写、模型照样思考，
	// 实测 max_tokens=200 时思考吃光预算、正文为空。必须删掉 thinking、显式
	// 给出该端点"不思考"的 reasoning_effort（GLM-5.2/5.3 是 min，没有 none），
	// 并给短预算留出余量。
	body := []byte(`{"model":"glm-5.3","max_tokens":100,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`)
	out := normalizeClaudeEndpointThinkingOffBody(body, "glm-5.3")
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("not json: %v (%s)", err, out)
	}
	if _, exists := payload["thinking"]; exists {
		t.Fatalf("thinking must be gone for GLM: %s", out)
	}
	if payload["reasoning_effort"] != "min" {
		t.Fatalf("reasoning_effort = %#v, want min", payload["reasoning_effort"])
	}
	if payload["max_tokens"] != float64(100+classifierRoom) {
		t.Fatalf("max_tokens = %#v, want room for thinking", payload["max_tokens"])
	}

	// 未知的非 Claude 模型：删 thinking，但不猜 reasoning_effort 的枚举值。
	unknown := []byte(`{"model":"mistral-large","max_tokens":20000,"thinking":{"type":"disabled"}}`)
	out = normalizeClaudeEndpointThinkingOffBody(unknown, "mistral-large")
	payload = nil
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("not json: %v (%s)", err, out)
	}
	if _, exists := payload["thinking"]; exists {
		t.Fatalf("thinking must be gone: %s", out)
	}
	if _, exists := payload["reasoning_effort"]; exists {
		t.Fatalf("reasoning_effort must not be invented for unknown models: %s", out)
	}
	if payload["max_tokens"] != float64(20000) {
		t.Fatalf("a generous budget must not be padded: %s", out)
	}

	// Anthropic 系模型走模型族改写：4.5 原样接受 disabled。
	claude := []byte(`{"model":"claude-sonnet-4.5","thinking":{"type":"disabled"}}`)
	if got := normalizeClaudeEndpointThinkingOffBody(claude, "claude-sonnet-4.5"); string(got) != string(claude) {
		t.Fatalf("Claude 4.5 accepts disabled, must stay byte-identical: %s", got)
	}

	// 只能自适应思考的 Claude：去掉 thinking，短预算补余量。
	always := []byte(`{"model":"claude-opus-5-5","max_tokens":64,"thinking":{"type":"disabled"}}`)
	out = normalizeClaudeEndpointThinkingOffBody(always, "claude-opus-5-5")
	payload = nil
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("not json: %v (%s)", err, out)
	}
	if _, exists := payload["thinking"]; exists {
		t.Fatalf("always-thinking Claude must lose disabled: %s", out)
	}
	// 族矩阵已加 classifierRoom，端点这里不能再叠加，只保证总余量够。
	if payload["max_tokens"] != float64(64+classifierRoom) {
		t.Fatalf("max_tokens = %#v", payload["max_tokens"])
	}

	// 余量已经够厚（8192）时不再追加：GLM-5.3 深思考一轮可能吃掉近万 token，
	// 预算落在余量之上说明调用方自己愿意等这么久思考，不要凭空放大。
	roomy := []byte(`{"model":"glm-5.3","max_tokens":20000,"thinking":{"type":"disabled"}}`)
	payload = nil
	if err := json.Unmarshal(normalizeClaudeEndpointThinkingOffBody(roomy, "glm-5.3"), &payload); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if payload["max_tokens"] != float64(20000) || payload["reasoning_effort"] != "min" {
		t.Fatalf("roomy budget changed: %#v / %#v", payload["max_tokens"], payload["reasoning_effort"])
	}

	// 只能自适应思考的 Claude 不会因为端点余量被叠两次：族矩阵补的
	// classifierRoom 已经够，端点只按余量目标补齐，不重复相加。
	opus := []byte(`{"model":"claude-opus-5-5","max_tokens":4000,"thinking":{"type":"disabled"}}`)
	payload = nil
	if err := json.Unmarshal(normalizeClaudeEndpointThinkingOffBody(opus, "claude-opus-5-5"), &payload); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if payload["max_tokens"] != float64(4000+classifierRoom) {
		t.Fatalf("opus max_tokens = %#v, want %d", payload["max_tokens"], 4000+classifierRoom)
	}

	// 没有 thinking 的请求逐字不变。
	plain := []byte(`{"model":"glm-5.3","messages":[]}`)
	if got := normalizeClaudeEndpointThinkingOffBody(plain, "glm-5.3"); string(got) != string(plain) {
		t.Fatalf("plain body changed: %s", got)
	}
}
