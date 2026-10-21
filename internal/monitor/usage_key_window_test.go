package monitor

import (
	"testing"
	"time"
)

// The quota gate reads a key's window through KeyWindowUsage instead of the console
// snapshot, so the two must agree: if the cheap path missed a day label or counted
// cache tokens twice, an operator would see a key refused while the console still
// shows budget left (or the other way round).
func TestKeyWindowUsageMatchesConsoleBuckets(t *testing.T) {
	t.Parallel()
	local := time.Local
	now := time.Date(2026, 3, 15, 10, 0, 0, 0, local)
	dayStart := time.Date(2026, 3, 15, 0, 0, 0, 0, local)
	previousDay := dayStart.AddDate(0, 0, -1)
	previousMonth := time.Date(2026, 2, 20, 9, 0, 0, 0, local)

	store := NewStore()
	events := []UsageEvent{
		{Time: previousMonth, APIKeyID: "k1", APIKeyName: "key", ProviderID: "p1", Model: "m1",
			Status: 200, InputTokens: 700, OutputTokens: 300, CostMicros: 4000, Priced: true},
		{Time: previousDay, APIKeyID: "k1", APIKeyName: "key", ProviderID: "p1", Model: "m1",
			Status: 200, InputTokens: 200, OutputTokens: 50, CostMicros: 1000, Priced: true},
		{Time: now, APIKeyID: "k1", APIKeyName: "key", ProviderID: "p1", Model: "m1",
			Status: 200, InputTokens: 100, CacheTokens: 40, OutputTokens: 20, CostMicros: 600, Priced: true},
		// Another key must never be pulled into this one's window.
		{Time: now, APIKeyID: "k2", APIKeyName: "other", ProviderID: "p1", Model: "m1",
			Status: 200, InputTokens: 5000, OutputTokens: 500},
	}
	for _, event := range events {
		store.ApplyUsageEventSync(event)
	}

	today := store.KeyWindowUsage("k1", dayStart, dayStart.AddDate(0, 0, 1))
	if want := int64(120); today.Tokens != want {
		t.Fatalf("today tokens: got %d, want %d (input 100 already includes the 40 cache reads)", today.Tokens, want)
	}
	if today.CostMicros != 600 || today.RequestCount != 1 || today.PricedCount != 1 {
		t.Fatalf("today totals: %+v", today)
	}

	monthStart := time.Date(2026, 3, 1, 0, 0, 0, 0, local)
	month := store.KeyWindowUsage("k1", monthStart, monthStart.AddDate(0, 1, 0))
	// March holds the 14th (200+50) and the 15th (100+20); the February request is
	// outside the window.
	if want := int64(370); month.Tokens != want {
		t.Fatalf("march tokens: got %d, want %d (the february request is outside)", month.Tokens, want)
	}
	if month.RequestCount != 2 {
		t.Fatalf("march request count: got %d, want 2", month.RequestCount)
	}

	whole := store.KeyWindowUsage("k1", time.Date(2026, 2, 1, 0, 0, 0, 0, local), monthStart.AddDate(0, 1, 0))
	if want := int64(1370); whole.Tokens != want {
		t.Fatalf("two-month tokens: got %d, want %d", whole.Tokens, want)
	}
	if whole.CostMicros != 5600 || whole.PricedCount != 3 {
		t.Fatalf("two-month money must follow the priced requests: %+v", whole)
	}

	// A window that ends before it begins, and a key nobody ever used, both read as
	// zero rather than panicking: the quota gate calls this on every request.
	if empty := store.KeyWindowUsage("k1", now, now); empty.Tokens != 0 {
		t.Fatalf("empty interval must be zero: %+v", empty)
	}
	if none := store.KeyWindowUsage("ghost", dayStart, dayStart.AddDate(0, 0, 1)); none.Tokens != 0 {
		t.Fatalf("unknown key must be zero: %+v", none)
	}
}
