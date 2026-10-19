package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestReasoningLabel(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"openai chat effort", `{"model":"gpt-5","reasoning_effort":"medium"}`, "medium"},
		{"responses nested effort", `{"model":"gpt-5","reasoning":{"effort":"high"}}`, "high"},
		{"responses summary beats nested", `{"reasoning_effort":"low","reasoning":{"effort":"high"}}`, "low"},
		{"claude thinking with budget", `{"thinking":{"type":"enabled","budget_tokens":8000}}`, "enabled·8000"},
		{"claude adaptive", `{"thinking":{"type":"adaptive"}}`, "adaptive"},
		{"claude thinking disabled", `{"thinking":{"type":"disabled"}}`, "disabled"},
		{"no setting", `{"model":"claude-opus-5-5","max_tokens":64}`, ""},
		{"empty body", ``, ""},
		{"malformed json", `{oops`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestReasoningLabel([]byte(tc.body)); got != tc.want {
				t.Fatalf("label = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReasoningLabelFromMap(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"openai chat effort", `{"model":"gpt-5","reasoning_effort":"medium"}`, "medium"},
		{"responses nested effort", `{"model":"gpt-5","reasoning":{"effort":"high"}}`, "high"},
		{"claude output_config effort", `{"model":"claude-opus-5-5","output_config":{"effort":"medium"}}`, "medium"},
		{"claude thinking with budget", `{"thinking":{"type":"enabled","budget_tokens":8000}}`, "enabled·8000"},
		{"claude adaptive", `{"thinking":{"type":"adaptive"}}`, "adaptive"},
		{"effort beats thinking", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`, "low"},
		{"no setting", `{"model":"claude-opus-5-5","max_tokens":64}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var payload map[string]any
			if err := json.Unmarshal([]byte(tc.json), &payload); err != nil {
				t.Fatal(err)
			}
			if got := reasoningLabelFromMap(payload); got != tc.want {
				t.Fatalf("label = %q, want %q", got, tc.want)
			}
		})
	}
}

// The production bug this guards: a Claude Code prompt is bigger than the log's
// body cap, so the stored copy is truncated JSON and yields nothing. The label
// must come from the payload the handler already parsed.
func TestReasoningLabelNeedsParsedPayloadWhenBodyTruncated(t *testing.T) {
	body := `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"long prompt…"}],"output_config":{"effort":"medium"}}`
	truncated := body[:strings.LastIndex(body, ",")] + "…(truncated)"
	if got := requestReasoningLabel([]byte(truncated)); got != "" {
		t.Fatalf("a truncated body must yield nothing rather than a guess, got %q", got)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if got := reasoningLabelFromMap(payload); got != "medium" {
		t.Fatalf("payload label = %q, want medium", got)
	}
}

func TestWithReasoningEffortRoundTrip(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if got := reasoningEffortFrom(request.Context()); got != "" {
		t.Fatalf("fresh request should carry nothing, got %q", got)
	}
	// An empty injection must not replace the request: the body-derived label
	// is what the log should fall back to.
	if again := withReasoningEffort(request, "  "); again != request {
		t.Fatal("empty effort must leave the request untouched")
	}
	carried := withReasoningEffort(request, "high")
	if got := reasoningEffortFrom(carried.Context()); got != "high" {
		t.Fatalf("carried effort = %q", got)
	}
	if got := reasoningEffortFrom(context.Background()); got != "" {
		t.Fatalf("nil-safe read failed: %q", got)
	}
}
