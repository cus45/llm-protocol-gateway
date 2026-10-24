package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

func TestParseGeminiPath(t *testing.T) {
	cases := []struct {
		path, model, action string
		ok                  bool
	}{
		{"/v1beta/models/gemini-2.5-pro:generateContent", "gemini-2.5-pro", "generateContent", true},
		{"/v1beta/models/gemini-2.5-flash:streamGenerateContent", "gemini-2.5-flash", "streamGenerateContent", true},
		{"/v1/models/gemini-2.5-pro:generateContent", "gemini-2.5-pro", "generateContent", true},
		// Tuned models carry slashes and the action is after the last colon.
		{"/v1beta/models/tunedModels/my-model:generateContent", "tunedModels/my-model", "generateContent", true},
		{"/v1beta/models/gemini-2.5-pro", "", "", false},
		{"/v1beta/models/:generateContent", "", "", false},
		{"/v1beta/models/gemini-2.5-pro:countTokens", "", "", false},
		{"/v1/embeddings", "", "", false},
	}
	for _, tc := range cases {
		model, action, ok := parseGeminiPath(tc.path)
		if ok != tc.ok || model != tc.model || action != tc.action {
			t.Fatalf("parseGeminiPath(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.path, model, action, ok, tc.model, tc.action, tc.ok)
		}
	}
}

// Gemini 的错误信封是 {"error":{"code","message","status"}}：Google 的 SDK 从
// 那里读错误码，别的形状它们看不懂。
func TestGeminiErrorEnvelope(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeGeminiError(recorder, http.StatusTooManyRequests, "slow down")
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", recorder.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	errObj, _ := payload["error"].(map[string]any)
	if got, _ := intFromAny(errObj["code"]); got != http.StatusTooManyRequests {
		t.Fatalf("code = %v", errObj["code"])
	}
	if got := stringValue(errObj["status"]); got != "RESOURCE_EXHAUSTED" {
		t.Fatalf("status = %q, want RESOURCE_EXHAUSTED", got)
	}
	if got := stringValue(errObj["message"]); got != "slow down" {
		t.Fatalf("message = %q", got)
	}
}

// 入口必须真的挂在路由表上；同时未带 Key 时回 Gemini 形状的 401 而不是 404。
func TestGeminiEndpointIsRouted(t *testing.T) {
	provider := imagesImageProvider("gemini-a", false)
	route := imagesRoute(provider.ID)
	route.OutputProtocol = domain.ProtocolGemini
	router := NewRouter(domain.GatewayState{
		Providers: []domain.Provider{provider},
		Routes:    []domain.Route{route},
		Endpoints: []domain.OutputEndpoint{{ID: "openai-chat-output", Name: "OpenAI Chat Output", Protocol: domain.ProtocolOpenAIChat}},
	})
	server := NewServer(router, nil)
	handler := server.Handler()

	for _, path := range []string{
		"/v1beta/models/gemini-2.5-pro:generateContent",
		"/v1beta/models/gemini-2.5-pro:streamGenerateContent",
		"/v1/models/gemini-2.5-pro:generateContent",
	} {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code == http.StatusNotFound && strings.Contains(recorder.Body.String(), "page not found") {
			t.Fatalf("%s is not routed: %d %s", path, recorder.Code, recorder.Body.String())
		}
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s without a key = %d, want 401 (%s)", path, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), `"error"`) || !strings.Contains(recorder.Body.String(), "UNAUTHENTICATED") {
			t.Fatalf("%s must answer in Gemini's error envelope, got %s", path, recorder.Body.String())
		}
	}
}
