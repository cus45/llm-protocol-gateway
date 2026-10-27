package gateway

import (
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// 一张 5 节点的链（首选 a + 备选 b/c/d/e）。把 b 提为首选、同时把 a 放回
// b 原来的位次，是控制台「设为首选」现在提交的形态（见 web/src/lib.tsx 的
// promoteApiKeyProvider）。这条测试钉住后端随后做的那步归一化不会把节点吞掉：
// 它只该剔除「等于新首选」的条目，总数一个不少。
func TestAPIKeyPromoteKeepsEveryChainNode(t *testing.T) {
	t.Parallel()
	newPromoteRouter := func(t *testing.T) *Router {
		t.Helper()
		return NewRouter(domain.GatewayState{
			Providers: []domain.Provider{
				{ID: "a", Name: "a", Protocol: domain.ProtocolOpenAIChat},
				{ID: "b", Name: "b", Protocol: domain.ProtocolOpenAIChat},
				{ID: "c", Name: "c", Protocol: domain.ProtocolOpenAIChat},
				{ID: "d", Name: "d", Protocol: domain.ProtocolOpenAIChat},
				{ID: "e", Name: "e", Protocol: domain.ProtocolOpenAIChat},
			},
			Routes: []domain.Route{
				{ID: "r-a", Name: "r-a", ProviderID: "a", OutputProtocol: domain.ProtocolOpenAIChat, Mode: domain.RouteModeAuto, Enabled: true},
				{ID: "r-b", Name: "r-b", ProviderID: "b", OutputProtocol: domain.ProtocolOpenAIChat, Mode: domain.RouteModeAuto, Enabled: true},
			},
		})
	}

	router := newPromoteRouter(t)
	key, err := router.AddAPIKey(domain.APIKey{
		Name: "chain", RouteID: "r-a",
		FallbackProviderIDs: []string{"b", "c", "d", "e"},
	})
	if err != nil {
		t.Fatalf("add key: %v", err)
	}

	// 设为首选：绑定换到 b 的路由，a 站回 b 的位置。
	updated, err := router.UpdateAPIKey(key.ID, domain.APIKey{
		Name:                   "chain",
		RouteID:                "r-b",
		FallbackProviderIDs:    []string{"a", "c", "d", "e"},
		FallbackModelOverrides: map[string]string{},
	})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if updated.RouteID != "r-b" {
		t.Fatalf("RouteID = %q, want r-b", updated.RouteID)
	}
	want := []string{"a", "c", "d", "e"}
	if len(updated.FallbackProviderIDs) != len(want) {
		t.Fatalf("fallbacks = %v, want %v: promoting must not drop a node", updated.FallbackProviderIDs, want)
	}
	for i, id := range want {
		if updated.FallbackProviderIDs[i] != id {
			t.Fatalf("fallbacks = %v, want %v (order preserved, old primary back in place)", updated.FallbackProviderIDs, want)
		}
	}

	// 对照：只改 routeId 的旧写法（不提备选列表）会把原首选挤丢——这正是
	// 「设为首选少一个节点」的复现，归一化只认新首选、不替我们补回旧的。
	router = newPromoteRouter(t)
	key, err = router.AddAPIKey(domain.APIKey{
		Name: "chain", RouteID: "r-a",
		FallbackProviderIDs: []string{"b", "c", "d", "e"},
	})
	if err != nil {
		t.Fatalf("add key: %v", err)
	}
	stale, err := router.UpdateAPIKey(key.ID, domain.APIKey{Name: "chain", RouteID: "r-b"})
	if err != nil {
		t.Fatalf("route-only patch: %v", err)
	}
	if len(stale.FallbackProviderIDs) != 3 {
		t.Fatalf("the old route-only patch left %d fallbacks, want 3 — if this changed, revisit the console fix: %v",
			len(stale.FallbackProviderIDs), stale.FallbackProviderIDs)
	}
}
