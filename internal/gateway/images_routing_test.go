package gateway

import (
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

func imagesTestServer(t *testing.T, providers ...domain.Provider) *Server {
	t.Helper()
	router := NewRouter(domain.GatewayState{})
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
