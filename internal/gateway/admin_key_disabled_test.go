package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// newAdminDisabledTestServer: an admin key whose chain walks from live
// "live-a" into "paused-b" (disabled) and "gone-c" (soft-deleted).
func newAdminDisabledTestServer(t *testing.T) *Server {
	t.Helper()
	state := domain.GatewayState{
		Providers: []domain.Provider{
			{ID: "live-a", Name: "live-a", Protocol: domain.ProtocolClaude, BaseURL: "https://a.example.com"},
			{ID: "paused-b", Name: "paused-b", Protocol: domain.ProtocolClaude, BaseURL: "https://b.example.com", Disabled: true},
			{ID: "gone-c", Name: "gone-c", Protocol: domain.ProtocolClaude, BaseURL: "https://c.example.com", Deleted: true},
		},
		Routes: []domain.Route{
			{ID: "live-a-claude", ProviderID: "live-a", OutputProtocol: domain.ProtocolClaude, Enabled: true},
		},
	}
	return NewServer(NewRouter(state), monitor.NewStore())
}

// An admin key has no owner, so the user grant table cannot filter it — but a
// disabled or deleted provider must still be dropped from its chain, or a
// paused account keeps taking traffic.
func TestAllowedProviderChainForKeyFiltersAdminDisabledAndDeleted(t *testing.T) {
	server := newAdminDisabledTestServer(t)
	admin := domain.APIKey{ID: "admin", FallbackProviderIDs: []string{"paused-b", "gone-c", "live-a"}}
	got := server.allowedProviderChainForKey(admin, apiKeyProviderChain("live-a", admin.FallbackProviderIDs))
	if len(got) != 1 || got[0] != "live-a" {
		t.Fatalf("admin chain = %v, want [live-a] with paused-b/gone-c dropped", got)
	}

	// A chain that is only disabled/deleted providers must not come back whole.
	dead := domain.APIKey{ID: "admin-2"}
	if got := server.allowedProviderChainForKey(dead, []string{"paused-b", "gone-c"}); len(got) != 0 {
		t.Fatalf("all-dead admin chain = %v, want empty", got)
	}
}

// End to end through the failover loop: an admin key must not reach the
// disabled fallback, it stays on the live provider.
func TestExecuteFailoverAdminKeySkipsDisabledProvider(t *testing.T) {
	server := newAdminDisabledTestServer(t)
	route, err := server.router.RouteByID("live-a-claude")
	if err != nil {
		t.Fatalf("route lookup: %v", err)
	}
	key := domain.APIKey{
		ID:                  "admin",
		RouteID:             "live-a-claude",
		FallbackProviderIDs: []string{"paused-b", "gone-c"},
		ActiveProviderID:    "paused-b",
	}
	chain := server.allowedProviderChainForKey(key, apiKeyProviderChain(route.ProviderID, key.FallbackProviderIDs))
	if len(chain) != 1 || chain[0] != "live-a" {
		t.Fatalf("chain = %v, want [live-a]", chain)
	}
	// Only the disabled/absent chain is left: refused, not silently routed to
	// whatever the route default happens to be.
	deadKey := domain.APIKey{ID: "admin", RouteID: "live-a-claude", FallbackProviderIDs: []string{"paused-b", "gone-c"}}
	deadChain := server.allowedProviderChainForKey(deadKey, []string{"paused-b", "gone-c"})
	if len(deadChain) != 0 {
		t.Fatalf("dead chain = %v, want empty", deadChain)
	}

	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	rec := httptest.NewRecorder()
	status, _, _, _, _, execErr := server.executeProtocolFlowWithFailover(
		rec, req, route, domain.RouteDecision{RouteID: route.ID, ProviderID: route.ProviderID},
		"claude-opus-5", map[string]any{"model": "claude-opus-5"},
		domain.ProtocolClaude, true, key, true,
	)
	if status == http.StatusForbidden || execErr == nil {
		t.Fatalf("status=%d err=%v: admin key must not fail over into a disabled provider", status, execErr)
	}
}
