package store

import (
	"path/filepath"
	"testing"
	"time"
)

// The pin table must exist after migration, round-trip a pin, drop expired rows
// on read, and honour an explicit removal — otherwise a restart either fails
// migration or silently forgets every conversation.
func TestSessionPinsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "pins.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	now := time.Now().UTC()
	live := SessionPin{SessionKey: "k-live", ProviderID: "p1", ExpiresAt: now.Add(10 * time.Minute).Format(time.RFC3339)}
	expired := SessionPin{SessionKey: "k-old", ProviderID: "p2", ExpiresAt: now.Add(-time.Minute).Format(time.RFC3339)}
	if err := st.SaveSessionPins([]SessionPin{live, expired}); err != nil {
		t.Fatalf("save: %v", err)
	}
	pins, err := st.ListSessionPins()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := map[string]SessionPin{}
	for _, pin := range pins {
		found[pin.SessionKey] = pin
	}
	if _, ok := found["k-old"]; ok {
		t.Fatalf("an expired pin must not survive the write: %v", found)
	}
	if got := found["k-live"]; got.ProviderID != "p1" || got.ExpiresAt != live.ExpiresAt {
		t.Fatalf("live pin = %+v", got)
	}

	// Saving again with the same key but another provider replaces the row.
	moved := SessionPin{SessionKey: "k-live", ProviderID: "p3", ExpiresAt: live.ExpiresAt}
	if err := st.SaveSessionPins([]SessionPin{moved}); err != nil {
		t.Fatalf("resave: %v", err)
	}
	if err := st.DeleteSessionPins([]string{"k-live"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	pins, err = st.ListSessionPins()
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(pins) != 0 {
		t.Fatalf("deleted pin still listed: %+v", pins)
	}
}

// Reopening the same file (a restart) must still see the pins.
func TestSessionPinsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pins.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	expires := time.Now().UTC().Add(20 * time.Minute).Format(time.RFC3339)
	if err := st.SaveSessionPins([]SessionPin{{SessionKey: "k", ProviderID: "p", ExpiresAt: expires}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	pins, err := reopened.ListSessionPins()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pins) != 1 || pins[0].SessionKey != "k" || pins[0].ProviderID != "p" {
		t.Fatalf("pins after reopen = %+v", pins)
	}
}
