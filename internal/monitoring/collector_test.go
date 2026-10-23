package monitoring

import (
	"context"
	"testing"
	"time"
)

type fakeSource struct {
	providers  []ProviderObservation
	keys       []KeyObservation
	windows    map[string]map[int]WindowStats
	baselines  map[string]int64
	quota      map[string]*QuotaSnapshot
	quotaCalls int
	multiIP    []MultiIPHit
	concurrent []ConcurrentIPHit
}

func (f *fakeSource) Providers(ctx context.Context, now time.Time) ([]ProviderObservation, error) {
	return f.providers, nil
}
func (f *fakeSource) Keys(ctx context.Context, now time.Time) ([]KeyObservation, error) {
	return f.keys, nil
}
func (f *fakeSource) Windows(ctx context.Context, now time.Time) (map[string]map[int]WindowStats, error) {
	return f.windows, nil
}
func (f *fakeSource) Baselines(ctx context.Context, now time.Time) (map[string]int64, error) {
	return f.baselines, nil
}
func (f *fakeSource) Quota(ctx context.Context, p ProviderObservation) (*QuotaSnapshot, error) {
	f.quotaCalls++
	return f.quota[p.ID], nil
}
func (f *fakeSource) MultiIP(ctx context.Context, now time.Time) ([]MultiIPHit, error) {
	return f.multiIP, nil
}
func (f *fakeSource) ConcurrentIP(ctx context.Context, now time.Time) ([]ConcurrentIPHit, error) {
	return f.concurrent, nil
}
func (f *fakeSource) LastRenewal(ctx context.Context, providerID string) (RenewalOutcome, error) {
	return RenewalOutcome{}, nil
}
func (f *fakeSource) ModelFailures(ctx context.Context, providerID string) (int, error) {
	return 0, nil
}

func TestCollectorBuildsSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	src := &fakeSource{
		providers: []ProviderObservation{{ID: "p1", Name: "P1", ExpiresAt: now.Add(time.Hour)}},
		keys:      []KeyObservation{{ID: "k1", MultiIPs: []string{"1.1.1.1", "2.2.2.2"}, MultiIPCount: 2}},
		windows:   map[string]map[int]WindowStats{"p1": {5: {Total: 10, Success: 9}}},
	}
	c := NewCollector(src, nil)
	snap, err := c.Snapshot(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Now != now {
		t.Fatal("snapshot time not set")
	}
	if snap.Windows["p1"][5].Success != 9 {
		t.Fatal("windows not merged")
	}
	if snap.Keys[0].MultiIPCount != 2 {
		t.Fatal("keys not merged")
	}
	if snap.CollectorLastSuccess.IsZero() {
		t.Fatal("collector heartbeat missing")
	}
}

func TestCollectorKeepsQuotaCacheUntilRefresh(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	src := &fakeSource{
		providers: []ProviderObservation{{ID: "p1", Vendor: "claude", Capabilities: Capabilities{Quota: true}}},
		quota:     map[string]*QuotaSnapshot{"p1": {Remaining: 5, Limit: 100, Ratio: 0.05, ObservedAt: now}},
	}
	c := NewCollector(src, nil)
	for _, at := range []time.Time{now, now.Add(5 * time.Minute)} {
		snap, err := c.Snapshot(context.Background(), at)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Providers[0].Quota == nil || snap.Providers[0].Quota.Remaining != 5 {
			t.Fatalf("quota not attached at %s", at)
		}
	}
	if src.quotaCalls != 1 {
		t.Fatalf("quota fetched %d times inside cache window, want 1", src.quotaCalls)
	}
	if _, err := c.Snapshot(context.Background(), now.Add(16*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if src.quotaCalls != 2 {
		t.Fatalf("quota fetched %d times after cache expiry, want 2", src.quotaCalls)
	}
}
