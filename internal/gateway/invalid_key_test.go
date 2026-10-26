package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// 一个错误的 Key 不能被当成「没带 Key」。历史上没有 Key 的请求会落到当前生效路由，
// 而校验过的错误 Key 原本也走这条路：写错一个字符，请求照样被服务，日志里也归不到
// 任何 Key 上。现在错误 Key 一律 401。
func TestInvalidKeyIsRefusedNotServiced(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()
	state := domain.GatewayState{
		Providers: []domain.Provider{{ID: "p1", Name: "p1", Protocol: domain.ProtocolOpenAIChat, BaseURL: upstream.URL,
			AuthType: domain.AuthTypeAPIKey, APIKeySource: "sk-up"}},
		Routes:  []domain.Route{{ID: "r1", ProviderID: "p1", OutputProtocol: domain.ProtocolOpenAIChat, Enabled: true}},
		APIKeys: []domain.APIKey{{ID: "k1", Name: "k1", Key: "sk-gw-right", RouteID: "r1", Enabled: true, StreamEnabled: true}},
	}
	server := NewServer(NewRouter(state), monitor.NewStore())
	server.SetAdminAuthStore(newMemoryAdminAuthStore())
	for _, target := range []string{"/v1/chat/completions", "/openai/v1/responses"} {
		t.Run(target, func(t *testing.T) {
			before := hits.Load()
			request := httptest.NewRequest(http.MethodPost, target,
				strings.NewReader(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}],"input":"hi"}`))
			request.Host = "gateway.example.com"
			request.RemoteAddr = "203.0.113.70:443"
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer sk-gw-wrong")
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			if got := hits.Load(); got != before {
				t.Fatalf("a wrong key reached the upstream %d time(s)", got-before)
			}
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", recorder.Code, recorder.Body.String())
			}
		})
	}
}
