package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// 预检只在「这条路径真的接受预检里声明的方法」时才由中间件回 204。以前任何
// OPTIONS 都在 mux 之前 204：跨站页面用 OPTIONS 探路径，控制台路由的存不存在
// 一探便知；错方法的探测还会被 mux 回 405 + Allow，等于免鉴权地列出路由表。
//
// 每条路径用不同的方法标识判定结果。
func TestCORSPreflightOnlyForAcceptedMethods(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		method     string
		wantStatus int
	}{
		{name: "unknown admin route PUT", path: "/__novah", method: "PUT", wantStatus: http.StatusNotFound},
		{name: "unknown model variant", path: "/v1/nope", method: "POST", wantStatus: http.StatusNotFound},
		{name: "model models, wrong method", path: "/v1/models", method: "HEAD", wantStatus: http.StatusMethodNotAllowed},
		{name: "admin bogus method", path: "/__state", method: "TRACE", wantStatus: http.StatusMethodNotAllowed},
		{name: "real admin route", path: "/__state", method: "GET", wantStatus: http.StatusNoContent},
		{name: "admin setup", path: "/__auth/setup", method: "POST", wantStatus: http.StatusNoContent},
		{name: "model chat", path: "/v1/chat/completions", method: "POST", wantStatus: http.StatusNoContent},
		{name: "model models", path: "/v1/models", method: "GET", wantStatus: http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := corsPreflightProbe(t, tc.path, tc.method)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("OPTIONS preflight on %s answered by the mux with an unexpected status", tc.path)
			}
			if tc.wantStatus == http.StatusNoContent {
				if recorder.Header().Get("Access-Control-Allow-Methods") == "" {
					t.Fatalf("a preflight answer must carry Access-Control-Allow-Methods")
				}
				vary := recorder.Header().Values("Vary")
				if !containsHeader(vary, "Access-Control-Request-Method") || !containsHeader(vary, "Access-Control-Request-Headers") {
					t.Fatalf("preflight Vary = %v, want Access-Control-Request-Method and -Headers", vary)
				}
			}
		})
	}
}

// 真正的跨域预检（模型端点）照旧拿到放行头，且只宣告这条路径接受的方法。
func TestCORSPreflightForModelEndpointStillAllowed(t *testing.T) {
	recorder := corsPreflightProbe(t, "/v1/chat/completions", "POST")
	// The client only posts chat completions here (the models list is its own
	// path), so POST is all a preflight may advertise.
	status := recorder.Code
	allowOrigin := recorder.Header().Get("Access-Control-Allow-Origin")
	allowMethods := recorder.Header().Get("Access-Control-Allow-Methods")
	allowHeaders := recorder.Header().Get("Access-Control-Allow-Headers")
	if status != http.StatusNoContent {
		t.Fatalf("a cross-origin preflight for a model endpoint must be answered by the middleware")
	}
	if allowOrigin != "*" {
		t.Fatalf("a model endpoint preflight must allow any origin")
	}
	if allowMethods != "POST" {
		t.Fatalf("a preflight may only advertise the methods the path accepts")
	}
	if allowHeaders == "" {
		t.Fatalf("a preflight must name the allowed request headers")
	}
}

func corsPreflightProbe(t *testing.T, path, method string) *httptest.ResponseRecorder {
	t.Helper()
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	request := httptest.NewRequest(http.MethodOptions, path, nil)
	request.Header.Set("Origin", "http://127.0.0.1")
	request.Host = "127.0.0.1"
	request.RemoteAddr = "127.0.0.1:54321"
	request.Header.Set("Access-Control-Request-Method", method)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func containsHeader(values []string, want string) bool {
	for _, value := range values {
		for _, part := range splitCommaList(value) {
			if part == want {
				return true
			}
		}
	}
	return false
}

func splitCommaList(value string) []string {
	var parts []string
	current := ""
	for _, r := range value {
		if r == ',' || r == ' ' {
			if current != "" {
				parts = append(parts, current)
				current = ""
			}
			continue
		}
		current += string(r)
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}
