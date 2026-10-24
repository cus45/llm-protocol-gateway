package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestModelPriceRoundTrip(t *testing.T) {
	t.Parallel()
	db := openTestStore(t)
	price := domain.ModelPrice{
		ProviderID: "claude-hj", ModelID: "claude-sonnet-4-5",
		InputUSDPerMTok: 3, OutputUSDPerMTok: 15, CacheReadUSDPerMTok: 0.3, CacheWriteUSDPerMTok: 3.75,
	}
	if err := db.SaveModelPrice(price); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.LoadModelPrices()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected one row, got %d", len(loaded))
	}
	got := loaded[0]
	if got.InputUSDPerMTok != 3 || got.OutputUSDPerMTok != 15 || got.CacheReadUSDPerMTok != 0.3 || got.CacheWriteUSDPerMTok != 3.75 {
		t.Fatalf("rates drifted through the database: %#v", got)
	}
	if got.UpdatedAt == "" {
		t.Fatal("a price row needs a timestamp so the console can say when it was set")
	}
	// Upsert replaces rather than accumulates.
	if err := db.SaveModelPrice(domain.ModelPrice{ProviderID: price.ProviderID, ModelID: price.ModelID, InputUSDPerMTok: 1}); err != nil {
		t.Fatal(err)
	}
	after, err := db.LoadModelPrices()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0].InputUSDPerMTok != 1 || after[0].OutputUSDPerMTok != 0 {
		t.Fatalf("upsert left %#v", after)
	}
	if err := db.DeleteModelPrice(price.ProviderID, price.ModelID); err != nil {
		t.Fatal(err)
	}
	emptied, err := db.LoadModelPrices()
	if err != nil {
		t.Fatal(err)
	}
	if len(emptied) != 0 {
		t.Fatalf("delete removed nothing: %#v", emptied)
	}
	if err := db.SaveModelPrice(domain.ModelPrice{ModelID: "m"}); err == nil {
		t.Fatal("a price without a provider must be refused")
	}
}

// A full-state save is where providers disappear, so it is also where their
// prices must disappear: a recycled provider id must not inherit a rate.
func TestSavePrunesPricesOfRemovedProviders(t *testing.T) {
	t.Parallel()
	db := openTestStore(t)
	if err := db.Save(domain.GatewayState{Providers: []domain.Provider{
		{ID: "keep", Name: "keep", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://a/v1"},
		{ID: "gone", Name: "gone", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://b/v1"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveModelPrice(domain.ModelPrice{ProviderID: "keep", ModelID: "*", InputUSDPerMTok: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveModelPrice(domain.ModelPrice{ProviderID: "gone", ModelID: "x", InputUSDPerMTok: 9}); err != nil {
		t.Fatal(err)
	}
	// The global rate card has no provider row to match it; a save must not
	// mistake that for a deleted provider.
	if err := db.SaveModelPrice(domain.ModelPrice{ProviderID: PriceWildcard, ModelID: "claude-sonnet-4-5", InputUSDPerMTok: 3, OutputUSDPerMTok: 15}); err != nil {
		t.Fatal(err)
	}
	if err := db.Save(domain.GatewayState{Providers: []domain.Provider{
		{ID: "keep", Name: "keep", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://a/v1"},
	}}); err != nil {
		t.Fatal(err)
	}
	remaining, err := db.LoadModelPrices()
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("expected the surviving provider's price and the global row, got %#v", remaining)
	}
	for _, price := range remaining {
		if price.ProviderID != "keep" && price.ProviderID != PriceWildcard {
			t.Fatalf("unexpected row after prune: %#v", price)
		}
	}
}

// Every bucket dimension has to carry cost, cache-write tokens and the count of
// requests that actually had a price row. Reading back through all of them is
// what catches a merge site that forgot one — a missing one shows up as a
// provider/model row summing to less than the day's total.
func TestUsageDailyCarriesCostAndCacheWriteEverywhere(t *testing.T) {
	t.Parallel()
	db := openTestStore(t)
	day := time.Now().Local().Format("2006-01-02")
	deltas := []monitor.UsagePersistDelta{
		{
			Day: day, KeyID: "k1", KeyName: "key one", UserID: "u1", ProviderID: "p1",
			Model: "m1", OutputProtocol: "openai_chat", StatusClass: "2xx",
			InputTokens: 1000, OutputTokens: 200, CacheTokens: 400, CacheWriteTokens: 100,
			CostMicros: 7000, PricedCount: 1,
		},
		{
			Day: day, KeyID: "k1", KeyName: "key one", UserID: "u1", ProviderID: "p1",
			Model: "m1", OutputProtocol: "openai_chat", StatusClass: "2xx",
			InputTokens: 500, OutputTokens: 100, CacheTokens: 0, CacheWriteTokens: 0,
			CostMicros: 3000, PricedCount: 1,
		},
		{
			// A second provider/model pair, so a wrong bucket key cannot borrow
			// the first pair's total and still look right.
			Day: day, KeyID: "k2", KeyName: "key two", UserID: "u2", ProviderID: "p2",
			Model: "m2", OutputProtocol: "claude", StatusClass: "4xx",
			InputTokens: 10, OutputTokens: 1, CostMicros: 5, CacheWriteTokens: 2, PricedCount: 1,
		},
		{
			// An unpriced request: it must count toward the volume and not at all
			// toward coverage, or the console would read a half-priced period as
			// a cheap one.
			Day: day, KeyID: "k3", KeyName: "key three", UserID: "u3", ProviderID: "p3",
			Model: "m3", OutputProtocol: "openai_chat", StatusClass: "2xx",
			InputTokens: 600, OutputTokens: 50,
		},
	}
	if err := db.ApplyUsageDeltas(deltas); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := db.LoadUsageSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	buckets, ok := loaded[day]
	if !ok {
		t.Fatalf("expected the day back, got %v", loaded)
	}
	if got := buckets.Total.CostMicros; got != 10005 {
		t.Fatalf("total cost: got %d, want 10005", got)
	}
	if buckets.Total.RequestCount != 4 || buckets.Total.PricedCount != 3 {
		t.Fatalf("coverage must follow the requests that had a price: priced %d of %d",
			buckets.Total.PricedCount, buckets.Total.RequestCount)
	}
	if got := buckets.Total.CacheWriteTokens; got != 102 {
		t.Fatalf("total cache writes: got %d, want 102", got)
	}
	if got := buckets.ByProvider["p1"].CostMicros; got != 10000 {
		t.Fatalf("provider cost: got %d, want 10000", got)
	}
	if got := buckets.ByProvider["p1"].PricedCount; got != 2 {
		t.Fatalf("provider priced count: got %d, want 2", got)
	}
	if got := buckets.ByProvider["p1"].CacheWriteTokens; got != 100 {
		t.Fatalf("provider cache writes: got %d", got)
	}
	if got := buckets.ByModel["m1"].CostMicros; got != 10000 {
		t.Fatalf("model cost: got %d, want 10000", got)
	}
	if got := buckets.ByUser["u1"].CostMicros; got != 10000 {
		t.Fatalf("user cost: got %d, want 10000", got)
	}
	if got := buckets.ByProtocol["openai_chat"].CostMicros; got != 10000 {
		t.Fatalf("protocol cost: got %d, want 10000", got)
	}
	if got := buckets.ByAPIKey["k1"].CostMicros; got != 10000 {
		t.Fatalf("api key cost: got %d, want 10000", got)
	}
	if got := buckets.ByAPIKey["k3"].PricedCount; got != 0 {
		t.Fatalf("unpriced key must have no priced count: got %d", got)
	}
	child, ok := buckets.ByKey["k1"]
	if !ok {
		t.Fatal("per-key detail buckets missing")
	}
	if got := child.ByProvider["p1"].CostMicros; got != 10000 {
		t.Fatalf("per-key provider cost: got %d", got)
	}
	if got := child.ByProvider["p1"].PricedCount; got != 2 {
		t.Fatalf("per-key provider priced count: got %d, want 2", got)
	}
	if got := child.ByModel["m1"].CostMicros; got != 10000 {
		t.Fatalf("per-key model cost: got %d", got)
	}
	if got := child.ByProtocol["openai_chat"].CostMicros; got != 10000 {
		t.Fatalf("per-key protocol cost: got %d", got)
	}
	if got := buckets.ByProvider["p2"].CostMicros; got != 5 {
		t.Fatalf("second pair cost: got %d", got)
	}
}

// A legacy row (no cost columns) must come back as unpriced, not as a free day.
func TestUsageDailyLegacyRowsHaveNoCost(t *testing.T) {
	t.Parallel()
	db := openTestStore(t)
	day := time.Now().Local().Format("2006-01-02")
	if err := db.ApplyUsageDelta(monitor.UsagePersistDelta{
		Day: day, KeyID: "k1", ProviderID: "p1", Model: "m1", OutputProtocol: "openai_chat",
		StatusClass: "2xx", InputTokens: 10, OutputTokens: 1,
	}); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := db.LoadUsageSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded[day].Total.CostMicros; got != 0 {
		t.Fatalf("an unpriced day must sum to zero, got %d", got)
	}
	if got := loaded[day].Total.PricedCount; got != 0 {
		t.Fatalf("an unpriced day must report no priced requests, got %d", got)
	}
}
