package gateway

import "testing"

func TestFitModelEffortAlignsToModelLevels(t *testing.T) {
	for _, tc := range []struct{ want, model, expected string }{
		// 模型认得的档位原样通过
		{"max", "deepseek-v4.1-flash", "max"},
		{"max", "glm-5.3", "max"},
		{"low", "glm-5.3", "low"},
		// 模型没有的档位：取最近的一档，距离相同往上
		{"medium", "deepseek-v4-pro", "high"},
		{"medium", "deepseek-v4.1-flash", "high"},
		{"xhigh", "deepseek-v4.1-flash", "max"},
		{"low", "deepseek-v4-pro", "high"},
		{"minimal", "glm-5.3", "low"},
		// 档位未知的模型不猜，原样透传
		{"max", "some-unknown-model", "max"},
		{"medium", "gpt-5.5", "medium"},
		// 关闭思考没有档位
		{"", "glm-5.3", ""},
		{"off", "glm-5.3", ""},
	} {
		if got := fitModelEffort(tc.want, tc.model); got != tc.expected {
			t.Errorf("fitModelEffort(%q, %q) = %q, want %q", tc.want, tc.model, got, tc.expected)
		}
	}
}

func TestClaudePayloadEffortReadsClientShapes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		payload  map[string]any
		expected string
	}{
		{"Claude Code 的思考深度", map[string]any{
			"thinking":      map[string]any{"type": "adaptive", "display": "omitted"},
			"output_config": map[string]any{"effort": "max"},
		}, "max"},
		{"output_config 优先于预算", map[string]any{
			"thinking":      map[string]any{"type": "enabled", "budget_tokens": 4096},
			"output_config": map[string]any{"effort": "max"},
		}, "max"},
		{"只有预算时反推", map[string]any{
			"thinking": map[string]any{"type": "enabled", "budget_tokens": 32000},
		}, "max"},
		{"预算落在 medium 档", map[string]any{
			"thinking": map[string]any{"type": "enabled", "budget_tokens": 8000},
		}, "medium"},
		{"部分中转写在 thinking.effort", map[string]any{
			"thinking": map[string]any{"type": "adaptive", "effort": "high"},
		}, "high"},
		{"关闭思考没有档位", map[string]any{
			"thinking":      map[string]any{"type": "disabled"},
			"output_config": map[string]any{"effort": "max"},
		}, ""},
		{"没提思考", map[string]any{"max_tokens": 1024}, ""},
	} {
		if got := claudePayloadEffort(tc.payload); got != tc.expected {
			t.Errorf("%s: claudePayloadEffort = %q, want %q", tc.name, got, tc.expected)
		}
	}
}

// Claude Code 选「思考深度 max」发出的就是 adaptive + output_config.effort 的组合；
// 转成 Chat 上行时必须变成 reasoning_effort，否则上游按自己的默认档跑。
func TestClaudeRequestToOpenAIChatCarriesEffort(t *testing.T) {
	claudeReq := map[string]any{
		"model":         "deepseek-v4.1-flash",
		"max_tokens":    64000,
		"thinking":      map[string]any{"type": "adaptive", "display": "omitted"},
		"output_config": map[string]any{"effort": "max"},
		"messages":      []any{map[string]any{"role": "user", "content": "hi"}},
	}
	openAIReq, err := claudeRequestToOpenAIChat(claudeReq, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if openAIReq["reasoning_effort"] != "max" {
		t.Fatalf("reasoning_effort = %#v, want max", openAIReq["reasoning_effort"])
	}
}

// 目标模型没有 medium 档（GLM-5.3 只有 low/high/max）时按档位对齐后再发。
func TestClaudeRequestToOpenAIChatFitsEffortToModel(t *testing.T) {
	claudeReq := map[string]any{
		"thinking":      map[string]any{"type": "adaptive"},
		"output_config": map[string]any{"effort": "medium"},
		"max_tokens":    4096,
		"messages":      []any{map[string]any{"role": "user", "content": "hi"}},
	}
	openAIReq, err := claudeRequestToOpenAIChat(claudeReq, "glm-5.3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if openAIReq["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v, want high (GLM-5.3 has no medium)", openAIReq["reasoning_effort"])
	}
}

// 关闭思考的请求不能因为「带了 output_config」就被开出 reasoning_effort，
// 那会让上游重新思考、把分类/标题请求的预算吃光。
func TestClaudeRequestToOpenAIChatKeepsThinkingOffOff(t *testing.T) {
	claudeReq := map[string]any{
		"thinking":      map[string]any{"type": "disabled"},
		"output_config": map[string]any{"effort": "high"},
		"max_tokens":    2112,
		"messages":      []any{map[string]any{"role": "user", "content": "Give this conversation a title"}},
	}
	openAIReq, err := claudeRequestToOpenAIChat(claudeReq, "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, exists := openAIReq["reasoning_effort"]; exists {
		t.Fatalf("thinking-off request must not carry reasoning_effort: %#v", openAIReq["reasoning_effort"])
	}
}

func TestApplyClaudeEffort(t *testing.T) {
	// 自适应模型：写 output_config.effort，thinking 仍是 adaptive
	adaptive := map[string]any{"thinking": map[string]any{"type": "adaptive"}, "max_tokens": 64000}
	if !applyClaudeEffort(adaptive, "deepseek-v4.1-flash", "max") {
		t.Fatal("adaptive payload should be rewritten")
	}
	if effort := outputConfigEffort(adaptive); effort != "max" {
		t.Fatalf("output_config.effort = %q, want max", effort)
	}

	// 老式预算模型：写 budget_tokens
	legacy := map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}, "max_tokens": 64000}
	if !applyClaudeEffort(legacy, "claude-sonnet-4-5", "max") {
		t.Fatal("legacy payload should be rewritten")
	}
	if budget := int64FromAny(legacy["thinking"].(map[string]any)["budget_tokens"]); budget != 32000 {
		t.Fatalf("budget_tokens = %d, want 32000", budget)
	}

	// 关闭思考的请求不动
	off := map[string]any{"thinking": map[string]any{"type": "disabled"}, "max_tokens": 2112}
	if applyClaudeEffort(off, "deepseek-v4.1-flash", "max") {
		t.Fatal("a thinking-off request must not be turned back on")
	}

	// 压根没提思考的请求不动
	silent := map[string]any{"max_tokens": 2112}
	if applyClaudeEffort(silent, "deepseek-v4.1-flash", "max") {
		t.Fatal("a request that never asked to think must not be given thinking")
	}

	// 预算被 max_tokens 压到没有余量时不动
	tight := map[string]any{"thinking": map[string]any{"type": "enabled"}, "max_tokens": 1024}
	if applyClaudeEffort(tight, "claude-sonnet-4-5", "max") {
		t.Fatal("a budget that leaves no room for the answer must be left alone")
	}
}
