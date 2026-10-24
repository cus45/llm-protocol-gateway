package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// Gemini 请求接到 Claude 上游：上游收到的是 Claude Messages 形状（system 顶层、
// messages、max_tokens），回包转回 Gemini 的 candidates 形状。
func TestGeminiThroughClaudeProvider(t *testing.T) {
	var gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",
			"content":[{"type":"text","text":"bonjour"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":9,"output_tokens":2}}`))
	}))
	t.Cleanup(upstream.Close)

	provider := imagesImageProvider("gem-claude", false)
	provider.Protocol = domain.ProtocolClaude
	provider.BaseURL = upstream.URL
	server, key := geminiLegServer(t, provider)

	recorder := geminiLegRequest(t, server, key, `{
		"systemInstruction":{"parts":[{"text":"answer in French"}]},
		"contents":[{"role":"user","parts":[{"text":"hi"}]}],
		"generationConfig":{"maxOutputTokens":50}
	}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if !strings.HasSuffix(gotPath, "/messages") {
		t.Fatalf("upstream path = %q, want /v1/messages", gotPath)
	}
	if !strings.Contains(gotBody, `"max_tokens"`) || !strings.Contains(gotBody, "answer in French") {
		t.Fatalf("upstream body is not the Claude shape: %s", gotBody)
	}
	if !strings.Contains(recorder.Body.String(), `"bonjour"`) || !strings.Contains(recorder.Body.String(), "candidates") {
		t.Fatalf("response is not the Gemini shape: %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "STOP") {
		t.Fatalf("finish reason missing: %s", recorder.Body.String())
	}
}

// Gemini 请求接到 Responses 上游：上游收到 Responses 形状，回包按同一中间形状
// 转回 Gemini。
func TestGeminiThroughResponsesProvider(t *testing.T) {
	var gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-x",
			"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hola"}]}],
			"usage":{"input_tokens":4,"output_tokens":[5],"total_tokens":9}}`))
	}))
	t.Cleanup(upstream.Close)

	provider := imagesImageProvider("gem-resp", false)
	provider.Protocol = domain.ProtocolOpenAIResponses
	provider.BaseURL = upstream.URL
	server, key := geminiLegServer(t, provider)

	recorder := geminiLegRequest(t, server, key, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if !strings.HasSuffix(gotPath, "/responses") {
		t.Fatalf("upstream path = %q, want /responses", gotPath)
	}
	if !strings.Contains(gotBody, `"input"`) {
		t.Fatalf("upstream body is not the Responses shape: %s", gotBody)
	}
	if !strings.Contains(recorder.Body.String(), `"hola"`) || !strings.Contains(recorder.Body.String(), "candidates") {
		t.Fatalf("response is not the Gemini shape: %s", recorder.Body.String())
	}
}

func geminiLegServer(t *testing.T, provider domain.Provider) (*Server, string) {
	t.Helper()
	route := imagesRoute(provider.ID)
	route.OutputProtocol = domain.ProtocolGemini
	router := NewRouter(domain.GatewayState{
		Providers: []domain.Provider{provider},
		Routes:    []domain.Route{route},
	})
	if _, err := router.AddAPIKey(domain.APIKey{ID: "k1", Name: "k1", Key: "sk-gw-test", RouteID: route.ID}); err != nil {
		t.Fatalf("add key: %v", err)
	}
	return NewServer(router, monitor.NewStore()), "sk-gw-test"
}

func geminiLegRequest(t *testing.T, server *Server, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-goog-api-key", key)
	recorder := httptest.NewRecorder()
	server.handleGeminiGenerateContent(recorder, request)
	return recorder
}
