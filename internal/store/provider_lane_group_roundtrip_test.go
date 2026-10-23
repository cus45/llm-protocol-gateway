package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// saveCloseReopen writes one state, closes the store and reads it back through
// a freshly opened store on the same file, so the test exercises the disk
// columns rather than the in-memory value Save was handed.
func saveCloseReopen(t *testing.T, state domain.GatewayState) domain.GatewayState {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	loaded, err := reopened.Load(domain.GatewayState{})
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

// The lane group is what makes several Provider rows of one subscription share
// a single budget. It has no other home than the providers table, so a missing
// column silently splits the account back into one full budget per row.
func TestProviderLaneGroupRoundTrip(t *testing.T) {
	t.Parallel()
	loaded := saveCloseReopen(t, domain.GatewayState{
		Providers: []domain.Provider{
			{ID: "a", Name: "a", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://a.example/v1", LaneGroup: "acct"},
			{ID: "b", Name: "b", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://b.example/v1", LaneGroup: "acct"},
			{ID: "own", Name: "own", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://c.example/v1"},
		},
	})
	byID := map[string]domain.Provider{}
	for _, provider := range loaded.Providers {
		byID[provider.ID] = provider
	}
	if got := byID["a"].LaneGroup; got != "acct" {
		t.Fatalf("expected the lane group to survive a restart, got %q", got)
	}
	if got := byID["b"].LaneGroup; got != "acct" {
		t.Fatalf("both rows of the account must load under one group, got %q", got)
	}
	if got := byID["own"].LaneGroup; got != "" {
		t.Fatalf("an ungrouped provider must load ungrouped, got %q", got)
	}
}

// SignInAt is the only thing a WorkBuddy credential that signed in without an
// expiry can be aged from. Left off disk, a restart makes it look freshly
// signed in forever and it is never refreshed before the vendor refuses it.
func TestProviderWorkBuddySignInAtRoundTrip(t *testing.T) {
	t.Parallel()
	const signInAt = "2026-07-01T08:09:10Z"
	loaded := saveCloseReopen(t, domain.GatewayState{
		Providers: []domain.Provider{{
			ID:           "wb",
			Name:         "wb",
			Protocol:     domain.ProtocolOpenAIChat,
			BaseURL:      "https://wb.example",
			AuthType:     domain.AuthTypeWorkBuddy,
			APIKeySource: "static",
			WorkBuddy: &domain.WorkBuddyCredential{
				AccessToken:  "at",
				RefreshToken: "rt",
				AccountLabel: "acct",
				UserID:       "u-1",
				Domain:       "example.com",
				SignInAt:     signInAt,
				Connected:    true,
			},
		}},
	})
	if len(loaded.Providers) != 1 || loaded.Providers[0].WorkBuddy == nil {
		t.Fatalf("expected the WorkBuddy credential back, got %+v", loaded.Providers)
	}
	credential := loaded.Providers[0].WorkBuddy
	if credential.SignInAt != signInAt {
		t.Fatalf("expected the sign-in time to survive a restart, got %q", credential.SignInAt)
	}
	if credential.AccessToken != "at" || credential.RefreshToken != "rt" || credential.UserID != "u-1" ||
		credential.Domain != "example.com" || credential.AccountLabel != "acct" {
		t.Fatalf("the whole credential must load, got %+v", *credential)
	}
}

// A database created before lane_group existed must gain the column with no
// data loss, and a WorkBuddy row stored in the columns only (no oauth_json)
// must keep loading.
func TestMigrateAddsLaneGroupToOldDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gateway.db")
	db, err := sql.Open("sqlite", path+
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=auto_vacuum(INCREMENTAL)")
	if err != nil {
		t.Fatal(err)
	}
	// The providers schema as it stood before requests_per_minute/lane_group and
	// oauth_json were added.
	statements := []string{
		`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE providers (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			protocol TEXT NOT NULL,
			base_url TEXT NOT NULL,
			api_key_source TEXT NOT NULL DEFAULT '',
			default_model TEXT NOT NULL DEFAULT '',
			default_thinking_depth TEXT NOT NULL DEFAULT '',
			health_status TEXT NOT NULL DEFAULT '',
			auth_header TEXT NOT NULL DEFAULT '',
			extra_endpoint TEXT NOT NULL DEFAULT '',
			position INTEGER NOT NULL DEFAULT 0,
			auth_type TEXT NOT NULL DEFAULT '',
			oauth_access_token TEXT NOT NULL DEFAULT '',
			oauth_refresh_token TEXT NOT NULL DEFAULT '',
			oauth_expires_at TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT INTO providers (id, name, protocol, base_url, position, auth_type, oauth_access_token, oauth_refresh_token)
		 VALUES ('legacy', 'legacy', 'openai_chat', 'https://legacy.example/v1', 0, 'workbuddy', 'old-at', 'old-rt')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("migrating an old database failed: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	loaded, err := store.Load(domain.GatewayState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Providers) != 1 {
		t.Fatalf("expected the legacy row back, got %+v", loaded.Providers)
	}
	provider := loaded.Providers[0]
	if provider.LaneGroup != "" || provider.RequestsPerMinute != 0 {
		t.Fatalf("added columns must default to off, got group=%q rpm=%d", provider.LaneGroup, provider.RequestsPerMinute)
	}
	if provider.WorkBuddy == nil || provider.WorkBuddy.AccessToken != "old-at" || provider.WorkBuddy.RefreshToken != "old-rt" {
		t.Fatalf("a column-only WorkBuddy row must still load, got %+v", provider.WorkBuddy)
	}

	// The migrated column is writable: a save after the migration must round-trip.
	loaded.Providers[0].LaneGroup = "acct"
	if err := store.Save(loaded); err != nil {
		t.Fatal(err)
	}
	again, err := store.Load(domain.GatewayState{})
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Providers[0].LaneGroup; got != "acct" {
		t.Fatalf("the migrated column must round-trip, got %q", got)
	}
}
