package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"testing"
)

// 宿主分离的约定：UI 域名只放控制台，模型协议一律 404；API 域名只放模型协议
// 与 /__health，控制台接口一律 404。M9 审查曾报「UI 域名能访问 /openai/v1/models」，
// 复核不成立（该前缀已被 isModelProtocolPath 覆盖），这里把约定钉住。
func TestHostSeparationKeepsModelRoutesOffTheUIDomain(t *testing.T) {
	hit := false
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	})
	handler := withHostSeparatedServing(api, "", func(host string) string {
		switch host {
		case "ui.example.com":
			return "ui"
		case "api.example.com":
			return "api"
		}
		return ""
	})

	modelPaths := []string{
		"/openai/v1/models",
		"/openai/v1/responses",
		"/v1/models",
		"/v1/chat/completions",
		"/v1/embeddings",
		"/v1/audio/transcriptions",
		"/v1beta/models/gemini:generateContent",
		"/anthropic/v1/messages",
		"/chat/completions",
	}
	for _, path := range modelPaths {
		hit = false
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "http://ui.example.com"+path, nil))
		if hit {
			t.Fatalf("UI host reached the API handler for %s", path)
		}
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("UI host %s = %d, want 404", path, recorder.Code)
		}
	}

	// API 域名上控制台接口同样不可达。
	for _, path := range []string{"/__state", "/__host-metrics", "/__public/control", "/callback", "/"} {
		hit = false
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://api.example.com"+path, nil))
		if hit {
			t.Fatalf("API host reached the console handler for %s", path)
		}
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("API host %s = %d, want 404", path, recorder.Code)
		}
	}
}

// 组合顺序：宿主分离必须在鉴权外层。反过来的话，API 域名上的 /__state 会先被
// 鉴权拦成 401——等于对外确认"这里有个管理接口，只是你没登录"，而不是 404。
func TestAPIHostAnswers404NotUnauthorizedForConsolePaths(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	// 配好双域名与暴露开关，让 publicHostRole 能认出 api 角色。
	settings := server.router.State().PublicAccess
	settings.CustomDomain = "api.test.local"
	settings.UIDomain = "ui.test.local"
	settings.ExposeAPI = true
	settings.ExposeUI = true
	server.router.UpdatePublicAccess(settings)

	handler := server.Handler()
	for _, path := range []string{"/__state", "/__logs", "/__host-metrics", "/__public/control"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "http://api.test.local"+path, nil)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("API host %s = %d, want 404 (a 401 confirms the endpoint exists)", path, recorder.Code)
		}
		if !strings.Contains(recorder.Body.String(), "management UI is not available") {
			t.Fatalf("API host %s body = %q, want the host-separation message", path, recorder.Body.String())
		}
	}
}
