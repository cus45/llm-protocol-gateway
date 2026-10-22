package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func corsProbe(t *testing.T, method, path, host, origin string) *httptest.ResponseRecorder {
	t.Helper()
	handler := withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, path, nil)
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// 模型协议端点给第三方网页/插件直连用，继续放行任意来源。
func TestCORSModelEndpointsAllowAnyOrigin(t *testing.T) {
	for _, path := range []string{
		"/v1/chat/completions",
		"/openai/v1/responses",
		"/anthropic/v1/messages",
		"/__health",
	} {
		rec := corsProbe(t, http.MethodPost, path, "gw.example.com", "https://any-site.example")
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Fatalf("%s: Allow-Origin = %q, want *", path, got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
			t.Fatalf("%s: credentials must not be allowed with *: %q", path, got)
		}
	}
}

// 管理端不再回 *：陌生来源拿不到 Allow-Origin，浏览器自己就会拦下响应。
func TestCORSAdminRejectsForeignOrigin(t *testing.T) {
	rec := corsProbe(t, http.MethodGet, "/__state", "gw.example.com", "https://evil.example")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("foreign origin must not be allowed on admin paths: %q", got)
	}
	preflight := corsProbe(t, http.MethodOptions, "/__state", "gw.example.com", "https://evil.example")
	if preflight.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", preflight.Code)
	}
	if got := preflight.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("foreign preflight must not be allowed: %q", got)
	}
}

// 生产由网关自己托管控制台，同源请求照常放行（带凭据）。
func TestCORSAdminAllowsSameOrigin(t *testing.T) {
	cases := []struct{ host, origin string }{
		{"gw.example.com", "https://gw.example.com"},
		{"gw.example.com:18093", "https://gw.example.com:18093"},
		{"localhost:5173", "http://localhost:5173"},
		{"192.168.1.20:18093", "http://192.168.1.20:18093"},
	}
	for _, tc := range cases {
		rec := corsProbe(t, http.MethodGet, "/__state", tc.host, tc.origin)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.origin {
			t.Fatalf("host %s origin %s: Allow-Origin = %q", tc.host, tc.origin, got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
			t.Fatalf("host %s: credentials = %q", tc.host, got)
		}
		if vary := rec.Header().Values("Vary"); len(vary) == 0 {
			t.Fatalf("host %s: responses must Vary on Origin", tc.host)
		}
	}
}

// 不同端口、不同域名都不算同源（默认端口除外）。
func TestCORSAdminOriginComparison(t *testing.T) {
	cases := []struct {
		host, origin string
		allowed      bool
	}{
		{"gw.example.com", "https://gw.example.com:8443", false},
		{"gw.example.com", "https://evil.example", false},
		{"192.168.1.20:18093", "http://127.0.0.1:18093", false},
		{"localhost:18093", "http://localhost:9999", false},
		{"gw.example.com:443", "https://gw.example.com", true},
		{"gw.example.com", "not-a-url", false},
		{"gw.example.com", "null", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/__state", nil)
		req.Host = tc.host
		got := adminCORSOriginAllowed(tc.origin, req)
		if got != tc.allowed {
			t.Fatalf("host %s origin %s: allowed = %v, want %v", tc.host, tc.origin, got, tc.allowed)
		}
	}
}

// 放行浏览器直连模型端点时，预检要带上 Anthropic 的浏览器标记头。
func TestCORSPreflightHeaders(t *testing.T) {
	rec := corsProbe(t, http.MethodOptions, "/anthropic/v1/messages", "gw.example.com", "https://claude.ai")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", rec.Code)
	}
	headers := rec.Header().Get("Access-Control-Allow-Headers")
	if !containsToken(headers, "anthropic-dangerous-direct-browser-access") {
		t.Fatalf("Allow-Headers = %q", headers)
	}
	if rec.Header().Get("Access-Control-Max-Age") == "" {
		t.Fatalf("preflight must be cacheable")
	}
}

func containsToken(list, token string) bool {
	for _, part := range strings.Split(list, ",") {
		if strings.TrimSpace(part) == token {
			return true
		}
	}
	return false
}
