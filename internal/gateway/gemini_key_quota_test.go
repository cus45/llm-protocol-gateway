package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// 按 Key 的 token 额度靠「预估提示 + 已用 + 在途」一起挡：只算已完成用量的话，
// 并发请求会在额度边上同时挤过去。Gemini 入口曾经按 0 预估（别的协议入口都按真实
// 提示预估），于是一个在 900/1000 的 Key 可以继续从 /v1beta 连发请求。
func TestGeminiEndpointReservesThePromptAgainstKeyQuota(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP"}]}`))
	}))
	defer upstream.Close()

	state := domain.GatewayState{
		Providers: []domain.Provider{{ID: "p1", Name: "p1", Protocol: domain.ProtocolOpenAIChat, BaseURL: upstream.URL,
			AuthType: domain.AuthTypeAPIKey, APIKeySource: "sk-up"}},
		Routes: []domain.Route{{ID: "r1", ProviderID: "p1", OutputProtocol: domain.ProtocolGemini, Enabled: true}},
		APIKeys: []domain.APIKey{{ID: "k1", Name: "k1", Key: "sk-gw-gemini", RouteID: "r1", Enabled: true, StreamEnabled: true,
			QuotaWindow: "day", QuotaTokens: 1000}},
	}
	server := NewServer(NewRouter(state), monitor.NewStore())
	server.SetAdminAuthStore(newMemoryAdminAuthStore())
	server.logs.ApplyUsageEventSync(monitor.UsageEvent{Time: time.Now(), APIKeyID: "k1", APIKeyName: "k1", Status: 200, InputTokens: 900})

	// 一句足够长的提示（预估远超剩下的 100 token）。
	prompt := strings.Repeat("x", 2000)
	body := `{"contents":[{"role":"user","parts":[{"text":"` + prompt + `"}]}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-x:generateContent", strings.NewReader(body))
	request.Host = "gateway.example.com"
	request.RemoteAddr = "203.0.113.70:443"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-goog-api-key", "sk-gw-gemini")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: 900 used + a long prompt does not fit a 1000-token window (body %s)", recorder.Code, recorder.Body.String())
	}
}
