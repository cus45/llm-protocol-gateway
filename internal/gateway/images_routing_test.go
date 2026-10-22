package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// imagesImageProvider 造一个能出图的上游：OpenAI Chat 协议 + 自定义 BaseURL。
func imagesImageProvider(id string, disabled bool) domain.Provider {
	return domain.Provider{
		ID:           id,
		Name:         id,
		Protocol:     domain.ProtocolOpenAIChat,
		BaseURL:      "https://" + id + ".example/v1",
		AuthType:     "api_key",
		APIKeySource: "sk-" + id,
		Disabled:     disabled,
	}
}

// imagesTestServer 的默认路由指向第一个 Provider，各用例按它写 Key/授权。
func imagesTestServer(t *testing.T, providers ...domain.Provider) *Server {
	t.Helper()
	routeProvider := "img-a"
	if len(providers) > 0 {
		routeProvider = providers[0].ID
	}
	router := NewRouter(domain.GatewayState{
		Routes: []domain.Route{imagesRoute(routeProvider)},
		Endpoints: []domain.OutputEndpoint{{
			ID: "openai-chat-output", Name: "OpenAI Chat Output", Protocol: domain.ProtocolOpenAIChat,
		}},
	})
	for _, provider := range providers {
		if _, err := router.AddProvider(provider); err != nil {
			t.Fatalf("add provider %s: %v", provider.ID, err)
		}
		// AddProvider normalises (and may rewrite) a provider rather than
		// rejecting it; a silently dropped provider would make a "refused"
		// assertion pass for the wrong reason.
		if stored, err := router.ProviderByID(provider.ID); err != nil || stored.Protocol != provider.Protocol {
			t.Fatalf("provider %s was not stored as given: %+v (%v)", provider.ID, stored, err)
		}
		if stored, err := router.ProviderByID(provider.ID); err == nil && stored.Disabled != provider.Disabled {
			t.Fatalf("provider %s disabled=%v, want %v", provider.ID, stored.Disabled, provider.Disabled)
		}
	}
	return NewServer(router, monitor.NewStore())
}

// imagesRoute 是出图请求命中的那条路由，默认 Provider 由各用例指定。
func imagesRoute(providerID string) domain.Route {
	return domain.Route{
		ID:               "img-route",
		ProviderID:       providerID,
		OutputEndpointID: "openai-chat-output",
		OutputProtocol:   domain.ProtocolOpenAIChat,
		Enabled:          true,
	}
}

// 管理员改授权是随时的事，出图必须在请求时重新校验，不能沿用建 Key 时的
// 结果。Key 的链是建 Key 时定下的（这里只有路由默认的 img-a），owner 后来
// 只被授权 img-b：出图必须被拒，而不是照样打到 img-a——那是别人的 Provider、
// 别人的账。
func TestImagesEffectiveProviderRefusesRouteDefaultAfterRegrant(t *testing.T) {
	server := imagesTestServer(t,
		imagesImageProvider("img-a", false),
		imagesImageProvider("img-b", false),
	)
	server.userStore = newMemoryUserStore(domain.User{
		ID: "u1", Username: "u1", Role: domain.UserRoleUser,
		AllowedProviderIDs: []string{"img-b"}, Enabled: true,
	})
	key := domain.APIKey{ID: "k1", OwnerUserID: "u1", RouteID: "img-route"}

	got := server.imagesEffectiveProvider(key, imagesRoute("img-a"))
	if got != "" {
		t.Fatalf("images landed on provider %q after the grant was moved away from it, want refusal", got)
	}
}

// 用户自建的 img-own 在 Key 的兜底链里、且仍被授权：路由默认的 img-a 已被
// 撤销，出图不能还去 img-a。
func TestImagesEffectiveProviderUsesGrantedFallback(t *testing.T) {
	server := imagesTestServer(t,
		imagesImageProvider("img-a", false),
		imagesImageProvider("img-own", false),
	)
	server.userStore = newMemoryUserStore(domain.User{
		ID: "u1", Username: "u1", Role: domain.UserRoleUser,
		AllowedProviderIDs: []string{"img-own"}, Enabled: true,
	})
	key := domain.APIKey{ID: "k1", OwnerUserID: "u1", RouteID: "img-route", FallbackProviderIDs: []string{"img-own"}}

	if got := server.imagesEffectiveProvider(key, imagesRoute("img-a")); got != "img-own" {
		t.Fatalf("images landed on %q, want the granted fallback img-own", got)
	}
}

// 链上没有一个还授权的 Provider 时，出图必须被拒，而不是退回路由默认值。
func TestImagesEffectiveProviderRefusesRevokedKey(t *testing.T) {
	server := imagesTestServer(t, imagesImageProvider("img-a", false))
	server.userStore = newMemoryUserStore(domain.User{
		ID: "u1", Username: "u1", Role: domain.UserRoleUser,
		AllowedProviderIDs: []string{"img-b"}, Enabled: true,
	})
	key := domain.APIKey{ID: "k1", OwnerUserID: "u1", RouteID: "img-route"}

	if got := server.imagesEffectiveProvider(key, imagesRoute("img-a")); got != "" {
		t.Fatalf("images landed on provider %q for a revoked key, want refusal", got)
	}
}

// 管理员自建的 Key（没有 owner）不受授权表约束。
func TestImagesEffectiveProviderLeavesAdminKeysAlone(t *testing.T) {
	server := imagesTestServer(t, imagesImageProvider("img-a", false))
	key := domain.APIKey{ID: "k-admin", RouteID: "img-route"}

	if got := server.imagesEffectiveProvider(key, imagesRoute("img-a")); got != "img-a" {
		t.Fatalf("admin key landed on %q, want img-a", got)
	}
}

// 调度把 Key 切到某个兜底 Provider 后，出图也要跟着走，而不是永远打在第一个上：
// 第一个已经限额、出图却还往它身上撞的话，切了等于没切。
func TestImagesEffectiveProviderFollowsActiveProvider(t *testing.T) {
	server := imagesTestServer(t,
		imagesImageProvider("img-a", false),
		imagesImageProvider("img-b", false),
	)
	key := domain.APIKey{
		ID:                  "k-admin",
		RouteID:             "img-route",
		FallbackProviderIDs: []string{"img-b"},
		ActiveProviderID:    "img-b",
	}

	if got := server.imagesEffectiveProvider(key, imagesRoute("img-a")); got != "img-b" {
		t.Fatalf("images landed on %q, want the active img-b", got)
	}
}

// imagesUpstream 起一个假的 Images 上游，记录收到的路径与 Authorization。
func imagesUpstream(t *testing.T) (*httptest.Server, *string, *string) {
	t.Helper()
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"AA=="}]}`))
	}))
	t.Cleanup(server.Close)
	return server, &gotPath, &gotAuth
}

// imagesKeyServer 造一台挂好出图路由、并绑了一个 Key 的网关。
func imagesKeyServer(t *testing.T, provider domain.Provider, key domain.APIKey) *Server {
	t.Helper()
	server := imagesTestServer(t, provider)
	key.RouteID = "img-route"
	key.StreamEnabled = true
	if strings.TrimSpace(key.Name) == "" {
		key.Name = key.ID
	}
	if _, err := server.router.AddAPIKey(key); err != nil {
		t.Fatalf("add key: %v", err)
	}
	return server
}

func imagesRequest(t *testing.T, server *Server, key string, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	recorder := httptest.NewRecorder()
	server.handleOpenAIImagesGenerations(recorder, request)
	return recorder
}

// 授权被撤销之后，出图必须 403，而且不能再把请求发到上游去。
func TestImagesHandlerRefusesRevokedGrant(t *testing.T) {
	upstream, gotPath, _ := imagesUpstream(t)
	provider := imagesImageProvider("img-a", false)
	provider.BaseURL = upstream.URL + "/v1"
	server := imagesKeyServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-user", OwnerUserID: "u1"})
	server.userStore = newMemoryUserStore(domain.User{
		ID: "u1", Username: "u1", Role: domain.UserRoleUser,
		AllowedProviderIDs: []string{"img-b"}, Enabled: true,
	})

	recorder := imagesRequest(t, server, "sk-gw-user", `{"model":"gpt-image-2","prompt":"x"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", recorder.Code, recorder.Body.String())
	}
	if *gotPath != "" {
		t.Fatalf("revoked key still reached the upstream at %q", *gotPath)
	}
}

// Provider 自己没有 Key 时，出图不能把调用方的网关 Key 当上游凭证发出去。
func TestImagesHandlerNeverForwardsConsumerKey(t *testing.T) {
	upstream, gotPath, gotAuth := imagesUpstream(t)
	provider := imagesImageProvider("img-a", false)
	provider.BaseURL = upstream.URL + "/v1"
	provider.APIKeySource = ""
	server := imagesKeyServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})

	recorder := imagesRequest(t, server, "sk-gw-admin", `{"model":"gpt-image-2","prompt":"x"}`)
	if recorder.Code == http.StatusOK {
		t.Fatalf("expected a refusal, got 200")
	}
	if *gotPath != "" || strings.Contains(*gotAuth, "sk-gw-admin") {
		t.Fatalf("gateway key leaked upstream: path=%q auth=%q", *gotPath, *gotAuth)
	}
}

// 正常路径：打到 Provider 的 BaseURL + /images/generations，带 Provider 自己的 Key，
// 模型按真正收到请求的 Provider 解析（模型映射只在这上面才生效）。
func TestImagesHandlerUsesProviderEndpointAndModel(t *testing.T) {
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"AA=="}]}`))
	}))
	t.Cleanup(upstream.Close)
	provider := imagesImageProvider("img-a", false)
	provider.BaseURL = upstream.URL + "/v1"
	provider.RequestAdapter = &domain.RequestAdapter{ModelMapping: map[string]string{"gpt-image-2": "img-mapped"}}
	server := imagesKeyServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})

	recorder := imagesRequest(t, server, "sk-gw-admin", `{"model":"gpt-image-2","prompt":"x"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if model, _ := gotBody["model"].(string); model != "img-mapped" {
		t.Fatalf("upstream model = %q, want the provider's mapping img-mapped", model)
	}
}

// 上游地址与凭证：路径拼在 BaseURL 后面，Authorization 是 Provider 自己的 Key。
func TestImagesHandlerUpstreamURLAndAuth(t *testing.T) {
	upstream, gotPath, gotAuth := imagesUpstream(t)
	provider := imagesImageProvider("img-a", false)
	provider.BaseURL = upstream.URL + "/v1"
	server := imagesKeyServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})

	if recorder := imagesRequest(t, server, "sk-gw-admin", `{"model":"gpt-image-2","prompt":"x"}`); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if *gotPath != "/v1/images/generations" {
		t.Fatalf("upstream path = %q, want /v1/images/generations", *gotPath)
	}
	if *gotAuth != "Bearer sk-img-a" {
		t.Fatalf("upstream authorization = %q, want the provider's own key", *gotAuth)
	}
}

// 禁用/删除的 Provider 没有 failover 兜底，必须当场拒绝。
func TestImagesHandlerRefusesDisabledProvider(t *testing.T) {
	upstream, gotPath, _ := imagesUpstream(t)
	provider := imagesImageProvider("img-a", true)
	provider.BaseURL = upstream.URL + "/v1"
	server := imagesKeyServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})

	recorder := imagesRequest(t, server, "sk-gw-admin", `{"model":"gpt-image-2","prompt":"x"}`)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", recorder.Code, recorder.Body.String())
	}
	if *gotPath != "" {
		t.Fatalf("disabled provider still received a request at %q", *gotPath)
	}
}

// 路由的输出协议是 Claude：即使 Provider 说的是 OpenAI Chat，出图也不成立——
// 客户端拿到的是 Claude 格式，读不了。必须拒绝，且请求不能发出去。
func TestImagesHandlerRefusesClaudeOutputRoute(t *testing.T) {
	upstream, gotPath, _ := imagesUpstream(t)
	provider := imagesImageProvider("img-a", false)
	provider.BaseURL = upstream.URL + "/v1"
	server := imagesKeyServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin", Name: "k1"})
	if _, err := server.router.UpdateRoute("img-route", domain.Route{OutputProtocol: domain.ProtocolClaude}); err != nil {
		t.Fatalf("update route: %v", err)
	}

	recorder := imagesRequest(t, server, "sk-gw-admin", `{"model":"gpt-image-2","prompt":"x"}`)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", recorder.Code, recorder.Body.String())
	}
	if *gotPath != "" {
		t.Fatalf("claude-output route still forwarded to %q", *gotPath)
	}
}

// 路由本身停用：出图没有 failover 兜底，直接拒绝，不读凭证、不发请求。
func TestImagesRouteRefusalDisabledRoute(t *testing.T) {
	server := imagesTestServer(t, imagesImageProvider("img-a", false))
	route := imagesRoute("img-a")
	route.Enabled = false
	if refusal, ok := imagesRouteRefusal(server.router, route); !ok || refusal.status != http.StatusServiceUnavailable {
		t.Fatalf("disabled route refusal = %+v ok=%v, want 503", refusal, ok)
	}
}

// Claude 协议的上游没有 images/generations：提前说清楚，不要拼一个必 404 的地址。
func TestImagesHandlerRefusesNonOpenAIProtocol(t *testing.T) {
	provider := imagesImageProvider("img-a", false)
	provider.Protocol = domain.ProtocolClaude
	server := imagesKeyServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})

	recorder := imagesRequest(t, server, "sk-gw-admin", `{"model":"gpt-image-2","prompt":"x"}`)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "images/generations") {
		t.Fatalf("error should say why: %s", recorder.Body.String())
	}
}
