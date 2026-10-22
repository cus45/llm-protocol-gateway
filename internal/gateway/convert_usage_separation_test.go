package gateway

import (
	"encoding/json"
	"testing"
)

// Anthropic reports cached input separately from input_tokens, while the
// Responses usage counts it inside input_tokens. The conversion must subtract
// the cache reads exactly once and never report a negative (or inflated)
// remainder; a wrong split corrupts cache-hit reporting in the console.
func TestResponsesToClaudeInputTokensExcludeCache(t *testing.T) {
	body := []byte(`{"id":"resp_1","object":"response","model":"m","status":"completed",` +
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],` +
		`"usage":{"input_tokens":500,"input_tokens_details":{"cached_tokens":400},"output_tokens":7,"total_tokens":507}}`)
	out, usage, err := responsesToClaudeResponseDirect(body, "m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 500 || usage.CacheTokens != 400 {
		t.Fatalf("internal usage = %+v, want inclusive 500 with 400 cached", usage)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	reported, _ := payload["usage"].(map[string]any)
	if got := int64FromAny(reported["input_tokens"]); got != 100 {
		t.Fatalf("input_tokens = %d, want 100 (500 - 400 cached)", got)
	}
	if got := int64FromAny(reported["cache_read_input_tokens"]); got != 400 {
		t.Fatalf("cache_read_input_tokens = %d, want 400", got)
	}
	if got := int64FromAny(reported["output_tokens"]); got != 7 {
		t.Fatalf("output_tokens = %d, want 7", got)
	}
}

// normalizeInclusiveInputTokens chooses between an inclusive prompt figure and
// an exclusive one by comparing the two counts; this pins the boundary so a
// future tweak cannot silently change what every provider's usage means.
func TestNormalizeInclusiveInputTokensBoundary(t *testing.T) {
	cases := []struct{ base, cache, miss, want int64 }{
		{120, 96, 0, 120},   // inclusive: prompt >= cache
		{678, 9218, 0, 9896}, // exclusive prompt without miss
		{10, 96, 4, 100},     // exclusive prompt with explicit miss
		{0, 0, 0, 0},
	}
	for _, tc := range cases {
		if got := normalizeInclusiveInputTokens(tc.base, tc.cache, 0, tc.miss); got != tc.want {
			t.Errorf("normalizeInclusiveInputTokens(%d,%d,0,%d) = %d, want %d", tc.base, tc.cache, tc.miss, got, tc.want)
		}
	}
}
