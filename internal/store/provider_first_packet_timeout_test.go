package store

import (
	"path/filepath"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// The first-packet deadline is scheduling state, not a credential, but it rides
// the same paired Save/load column list: a missing case in either direction
// drops the limit silently and every request then waits on a hung upstream.
func TestProviderFirstPacketTimeoutRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	state := domain.GatewayState{
		Providers: []domain.Provider{
			{ID: "slow", Name: "slow", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://a.example/v1", FirstPacketTimeoutSec: 12},
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
	if got := byID["slow"].FirstPacketTimeoutSec; got != 12 {
		t.Fatalf("expected the limit to survive a restart, got %d", got)
	}
	if got := byID["open"].FirstPacketTimeoutSec; got != 0 {
		t.Fatalf("an unset limit must load as no limit, got %d", got)
	}
}
