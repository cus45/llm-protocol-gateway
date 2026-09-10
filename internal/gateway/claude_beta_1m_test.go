package gateway

import "testing"

func TestMergeAnthropicBetaFlags(t *testing.T) {
	cases := []struct {
		existing string
		extras   []string
		want     string
	}{
		{"", []string{claudeContext1MBeta}, claudeContext1MBeta},
		{claudeContext1MBeta, []string{claudeContext1MBeta}, claudeContext1MBeta},
		{"interleaved-thinking-2025-05-14", []string{claudeContext1MBeta}, "interleaved-thinking-2025-05-14," + claudeContext1MBeta},
		{"a,b", nil, "a,b"},
	}
	for _, tc := range cases {
		if got := mergeAnthropicBetaFlags(tc.existing, tc.extras...); got != tc.want {
			t.Fatalf("mergeAnthropicBetaFlags(%q, %v) = %q, want %q", tc.existing, tc.extras, got, tc.want)
		}
	}
}

func TestStringFromRequestBody(t *testing.T) {
	if got := stringFromRequestBody([]byte(`{"model":"claude-opus-5","max_tokens":8}`)); got != "claude-opus-5" {
		t.Fatalf("model = %q", got)
	}
	if got := stringFromRequestBody([]byte(`not json`)); got != "" {
		t.Fatalf("expected empty model, got %q", got)
	}
}

func TestContext1MBetaAppliedFor1MModels(t *testing.T) {
	if !isClaude1MContextModel("claude-opus-5") {
		t.Fatal("claude-opus-5 should be a 1M-context model")
	}
	beta := mergeAnthropicBetaFlags("", claudeContext1MBeta)
	if beta != claudeContext1MBeta {
		t.Fatalf("beta = %q", beta)
	}
	if isClaude1MContextModel("claude-haiku-4-5-20251001") {
		t.Fatal("haiku should not be a 1M-context model")
	}
}
