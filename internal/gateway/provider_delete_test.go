package gateway

import (
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

func TestDeleteProviderUnbindsAPIKeys(t *testing.T) {
	router := NewRouter(domain.GatewayState{
		Providers: []domain.Provider{
			{ID: "p1", Name: "P1", Protocol: domain.ProtocolOpenAIChat},
			{ID: "p2", Name: "P2", Protocol: domain.ProtocolOpenAIChat},
		},
		APIKeys: []domain.APIKey{
			{
				ID:                     "k1",
				Name:                   "K1",
				ActiveProviderID:       "p1",
				FallbackProviderIDs:    []string{"p1", "p2"},
				FallbackModelOverrides: map[string]string{"p1": "m1", "p2": "m2"},
			},
			{
				ID:   "k2",
				Name: "K2",
			},
		},
	})

	if err := router.DeleteProvider("p1"); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}

	state := router.State()
	if state.Providers[0].Deleted != true {
		t.Fatalf("provider should be soft-deleted: %+v", state.Providers[0])
	}

	key1 := state.APIKeys[0]
	if key1.ActiveProviderID != "" {
		t.Fatalf("activeProvider should be reset, got %q", key1.ActiveProviderID)
	}
	if len(key1.FallbackProviderIDs) != 1 || key1.FallbackProviderIDs[0] != "p2" {
		t.Fatalf("fallback list should drop p1, got %v", key1.FallbackProviderIDs)
	}
	if _, ok := key1.FallbackModelOverrides["p1"]; ok {
		t.Fatalf("model override for p1 should be removed: %v", key1.FallbackModelOverrides)
	}
	if key1.FallbackModelOverrides["p2"] != "m2" {
		t.Fatalf("model override for p2 should survive: %v", key1.FallbackModelOverrides)
	}

	key2 := state.APIKeys[1]
	if key2.ActiveProviderID != "" || len(key2.FallbackProviderIDs) != 0 {
		t.Fatalf("unrelated key should stay untouched: %+v", key2)
	}
}

func TestDeleteProviderDeletesRoutesAndUnbindsKeys(t *testing.T) {
	router := NewRouter(domain.GatewayState{
		Providers: []domain.Provider{
			{ID: "p1", Name: "P1", Protocol: domain.ProtocolOpenAIChat},
			{ID: "p2", Name: "P2", Protocol: domain.ProtocolOpenAIChat},
		},
		Routes: []domain.Route{
			{ID: "r1", Name: "R1", ProviderID: "p1"},
			{ID: "r2", Name: "R2", ProviderID: "p2"},
		},
		APIKeys: []domain.APIKey{
			{
				ID:      "k1",
				Name:    "K1",
				RouteID: "r1",
			},
			{
				ID:      "k2",
				Name:    "K2",
				RouteID: "r2",
			},
		},
	})

	if err := router.DeleteProvider("p1"); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}

	state := router.State()
	if len(state.Routes) != 1 || state.Routes[0].ID != "r2" {
		t.Fatalf("route of deleted provider should be removed, got %+v", state.Routes)
	}
	if key := state.APIKeys[0]; key.RouteID != "" {
		t.Fatalf("key bound to deleted route should have RouteID reset, got %q", key.RouteID)
	}
	if key := state.APIKeys[1]; key.RouteID != "r2" {
		t.Fatalf("key on surviving route should stay, got %q", key.RouteID)
	}
}

func TestDeleteProviderMissing(t *testing.T) {
	t.Parallel()
	router := NewRouter(domain.GatewayState{})
	if err := router.DeleteProvider("ghost"); err == nil {
		t.Fatalf("expected not-found error, got nil")
	}
}
