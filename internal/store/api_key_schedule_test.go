package store

import (
	"path/filepath"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// The scheduling policy is routing state: if the save/load pair drops it, the
// key silently falls back to "order" on the next restart and traffic that was
// being spread across accounts piles back onto the preferred one.
func TestAPIKeyScheduleModeRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	state := domain.GatewayState{
		APIKeys: []domain.APIKey{
			{ID: "k1", Name: "k1", Key: "sk-k1", RouteID: "r1", Enabled: true, ScheduleMode: "quota",
				Profiles: []domain.KeyProfile{{ID: "pf1", Name: "scheme", RouteID: "r1", ScheduleMode: "least_used", StreamEnabled: true}}},
			{ID: "k2", Name: "k2", Key: "sk-k2", RouteID: "r1", Enabled: true},
		},
	}
	// Keys live in their own rows written by Create/UpdateAPIKey: the full-state
	// Save only rewrites providers, models and routes.
	for _, key := range state.APIKeys {
		if err := store.CreateAPIKey(key); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := store.Load(domain.GatewayState{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]domain.APIKey{}
	for _, key := range loaded.APIKeys {
		byID[key.ID] = key
	}
	if got := byID["k1"].ScheduleMode; got != "quota" {
		t.Fatalf("expected the mode to survive a restart, got %q", got)
	}
	if len(byID["k1"].Profiles) != 1 || byID["k1"].Profiles[0].ScheduleMode != "least_used" {
		t.Fatalf("a profile's mode must survive too, got %#v", byID["k1"].Profiles)
	}
	if got := byID["k2"].ScheduleMode; got != "" {
		t.Fatalf("an unset mode must load as the default order, got %q", got)
	}

	// And through the update path the failover/recovery writes use.
	update := byID["k2"]
	update.ScheduleMode = "rotate"
	if err := store.UpdateAPIKey(update); err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.APIKeys()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range reloaded {
		if key.ID == "k2" && key.ScheduleMode != "rotate" {
			t.Fatalf("UpdateAPIKey lost the mode, got %q", key.ScheduleMode)
		}
	}
}
