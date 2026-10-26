package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// routeGateUpstream is an upstream that counts what reaches it. Reaching it at
// all is what "the gateway spent somebody's account" means here.
func routeGateUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"up-1","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	return upstream, &calls
}

func routeGateState(upstreamURL string) domain.GatewayState {
	return domain.GatewayState{
		Providers: []domain.Provider{{
			ID: "p1", Name: "P1", Protocol: domain.ProtocolClaude,
			BaseURL: upstreamURL, AuthType: domain.AuthTypeAPIKey, APIKeySource: "sk-upstream-secret",
		}},
		Models: []domain.Model{{ID: "claude-x", ProviderID: "p1", InMenu: true}},
		Routes: []domain.Route{
			{ID: "r1", Name: "R1", ProviderID: "p1", OutputProtocol: domain.ProtocolClaude, Enabled: true},
			{ID: "r2", Name: "R2", ProviderID: "p1", OutputProtocol: domain.ProtocolOpenAIChat, Enabled: true},
		},
		APIKeys: []domain.APIKey{{ID: "k1", Name: "console", Key: "sk-console-key-1", RouteID: "r1", Enabled: true, StreamEnabled: true}},
	}
}

// 控制台「对话测试」(testRoute) 让协议入口跳过 API Key 校验，靠的是
// X-Gateway-Route-Id + X-Gateway-Internal-Test 两个头。这两个头是客户端随便写的，
// 而控制台接口（Claude / Gemini / Chat 的转换入口）本来对不带 key 的请求一律 401：
// 任何人只要知道一条路由 id（/__state 里就有），带上这两个头就能跳过校验、直接
// 用 Provider 自己的上游凭据发请求，既不记 key 额度也不归因到任何 key。
// 现在这个捷径只认「本进程自己发的」请求（request context 里的 routeTestToken）。
func TestClientCannotClaimTheRouteTestShortcut(t *testing.T) {
	upstream, upstreamCalls := routeGateUpstream(t)
	server := NewServer(NewRouter(routeGateState(upstream.URL)), monitor.NewStore())
	server.SetAdminAuthStore(newMemoryAdminAuthStore())
	handler := server.Handler()

	for _, target := range []string{"/anthropic/v1/messages", "/v1beta/models/claude-x:generateContent"} {
		t.Run(target, func(t *testing.T) {
			before := upstreamCalls.Load()
			body := `{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
			if strings.HasPrefix(target, "/v1beta/") {
				body = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
			}
			request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
			request.Host = "gateway.example.com"
			request.RemoteAddr = "203.0.113.70:443"
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(headerGatewayRouteID, "r1")
			request.Header.Set(headerGatewayInternalTest, "1")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if got := upstreamCalls.Load(); got != before {
				t.Fatalf("a client with no API key reached the provider %d time(s): status=%d body=%s", got-before, recorder.Code, recorder.Body.String())
			}
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// 反过来：带的 key 照常工作（上面的 401 不是因为把整条链路关了）。
func TestAnAPIKeyIsAccepted(t *testing.T) {
	upstream, _ := routeGateUpstream(t)
	server := NewServer(NewRouter(routeGateState(upstream.URL)), monitor.NewStore())
	server.SetAdminAuthStore(newMemoryAdminAuthStore())
	request := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	request.Host = "gateway.example.com"
	request.RemoteAddr = "203.0.113.70:443"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "sk-console-key-1")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	// The fixture's provider speaks Claude, so Claude→Claude has no conversion
	// to run: 502 here is that gap, not a refused credential. What matters is
	// that the key got through and the turn was built.
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "protocol conversion is not implemented") {
		t.Fatalf("status = %d, want the conversion-gap 502 the keyed path reaches (body %s)", recorder.Code, recorder.Body.String())
	}
}

// internalTestRouteID 是这段判定的唯一入口：两个头齐了还不够，请求自己得带着
// 服务端发的标记；标记只在服务端代码里放得进去。
func TestInternalTestRouteIDRequiresTheServerSideMark(t *testing.T) {
	cases := []struct {
		name     string
		headers  bool
		marked   bool
		wantOK   bool
		wantRout string
	}{
		{name: "headers only, no mark", headers: true, marked: false, wantOK: false},
		{name: "mark only, no headers", headers: false, marked: true, wantOK: false},
		{name: "headers and mark", headers: true, marked: true, wantOK: true, wantRout: "r1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
			if tc.headers {
				request.Header.Set(headerGatewayRouteID, "r1")
				request.Header.Set(headerGatewayInternalTest, "1")
			}
			if tc.marked {
				request = request.WithContext(withRouteTestToken(request.Context()))
			}
			routeID, ok := internalTestRouteID(request)
			if ok != tc.wantOK || routeID != tc.wantRout {
				t.Fatalf("internalTestRouteID = (%q, %v), want (%q, %v)", routeID, ok, tc.wantRout, tc.wantOK)
			}
		})
	}
}
