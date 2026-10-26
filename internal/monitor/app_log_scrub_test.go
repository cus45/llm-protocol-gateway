package monitor

import (
	"strings"
	"testing"
)

// 这里钉住应用日志这一份规则本身（缩进、占位符与 internal/gateway/log_scrub.go 的
// 形状规则一致，但两个包各有一份，改动时要一起改；网关那边有契约测试同时跑三处）。
func TestScrubAppLogCredentialShapes(t *testing.T) {
	cases := []struct{ text, secret, survivor string }{
		{"https://api.example.com/v1/chat?api_key=3f9a1c2b4d5e6f708192a3b4&x=1", "3f9a1c2b4d5e6f708192a3b4", "api_key="},
		{"connect to https://admin:hunter2@internal.example.com/v1 failed", "hunter2", "@internal.example.com"},
		{"Authorization: Bearer 0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef", "Bearer"},
		{"cloudflared service install eyJhIjoiYWNjb3VudC1pZCIsInQiOiJ0dW5uZWwtdG9rZW4ifQ==", "YWNjb3VudC1pZCIsInQiOiJ0dW5uZWwtdG9rZW4ifQ", ""},
		{"Incorrect API key provided: sk-01234567-89ab-cdef-0123-456789abcdef", "01234567-89ab-cdef", ""},
	}
	for _, tc := range cases {
		got := scrubAppLogText(tc.text)
		if strings.Contains(got, tc.secret) {
			t.Fatalf("credential %q survived the app-log scrub: %q", tc.secret, got)
		}
		if tc.survivor != "" && !strings.Contains(got, tc.survivor) {
			t.Fatalf("the scrub removed %q too, the line is unreadable now: %q", tc.survivor, got)
		}
	}
}
