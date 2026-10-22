package store

import (
	"path/filepath"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// The per-minute request budget rides the same paired Save/load column list as
// the other admission knobs: a missing case in either direction silently drops
// the cap, and the account starts answering 429 again.
func TestProviderRequestsPerMinuteRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	state := domain.GatewayState{
		Providers: []domain.Provider{
			{ID: "capped", Name: "capped", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://a.example/v1", RequestsPerMinute: 30},
			{ID: "open", Name: "open", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://b.example/v1"},
		},
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(domain.GatewayState{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]domain.Provider{}
	for _, provider := range loaded.Providers {
		byID[provider.ID] = provider
	}
	if got := byID["capped"].RequestsPerMinute; got != 30 {
		t.Fatalf("expected the rate to survive a restart, got %d", got)
	}
	if got := byID["open"].RequestsPerMinute; got != 0 {
		t.Fatalf("an unset rate must load as no limit, got %d", got)
	}
}
