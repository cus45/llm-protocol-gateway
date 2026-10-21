package gateway

import (
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func TestProviderLoadTrackerWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker := &providerLoadTracker{
		series: map[string]*providerLoadSeries{},
		now:    func() time.Time { return now },
	}
	tracker.add("p1", 100)
	tracker.add("p1", 50)
	if got := tracker.workload("p1"); got != 150 {
		t.Fatalf("expected the summed window, got %d", got)
	}
	if got := tracker.workload("never used"); got != 0 {
		t.Fatalf("an unknown provider must score zero, got %d", got)
	}
	// The window is six minutes: the same slot is still inside it at +5m and
	// gone once the ring has wrapped.
	now = now.Add(5 * time.Minute)
	if got := tracker.workload("p1"); got != 150 {
		t.Fatalf("a bucket five minutes old is still in the window, got %d", got)
	}
	now = now.Add(time.Minute)
	if got := tracker.workload("p1"); got != 0 {
		t.Fatalf("the window should have expired, got %d", got)
	}
	// A new minute reuses the slot rather than adding to the stale count.
	now = now.Add(time.Minute)
	tracker.add("p1", 7)
	if got := tracker.workload("p1"); got != 7 {
		t.Fatalf("expected only the fresh minute, got %d", got)
	}
	var nilTracker *providerLoadTracker
	nilTracker.add("p1", 10)
	if nilTracker.workload("p1") != 0 {
		t.Fatal("a nil tracker must be inert, not panic")
	}
	// Zero or negative work is not recorded: a request that produced nothing
	// must not make an account look busy.
	tracker.add("p2", 0)
	tracker.add("p2", -5)
	if _, ok := tracker.series["p2"]; ok {
		t.Fatal("non-positive work must be dropped")
	}
}

func TestProviderLoadTrackerPrunesIdleProviders(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tracker := &providerLoadTracker{
		series: map[string]*providerLoadSeries{},
		now:    func() time.Time { return now },
	}
	// Fill past the cap with providers that all went quiet five minutes ago.
	for i := 0; i < providerLoadSeriesCap+10; i++ {
		tracker.series[time.Now().String()+string(rune(i))] = &providerLoadSeries{
			minutes: [providerLoadBuckets]int64{now.Unix()/60 - 5},
		}
	}
	now = now.Add(10 * time.Minute)
	tracker.add("alive", 1)
	if len(tracker.series) > providerLoadSeriesCap {
		t.Fatalf("idle series should have been pruned, %d left", len(tracker.series))
	}
	if _, ok := tracker.series["alive"]; !ok {
		t.Fatal("the active provider must survive pruning")
	}
}

func TestNoteProviderUsageCountsUncachedWork(t *testing.T) {
	t.Parallel()
	s := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	s.noteProviderUsage("p1", TokenUsage{InputTokens: 1000, CacheTokens: 900, OutputTokens: 100})
	if got := s.providerWorkload("p1"); got != 200 {
		t.Fatalf("cached input must not count as work, got %d", got)
	}
	// An answer with no usage numbers still cost the account one request.
	s.noteProviderUsage("p2", TokenUsage{})
	if got := s.providerWorkload("p2"); got != 1 {
		t.Fatalf("a request with no usage should count as one, got %d", got)
	}
	// A server built without NewServer has no tracker and must not panic.
	bare := &Server{}
	bare.noteProviderUsage("p1", TokenUsage{InputTokens: 10})
	if got := bare.providerWorkload("p1"); got != 0 {
		t.Fatalf("no tracker must mean no load, got %d", got)
	}
}

func TestUtilizationHeadroom(t *testing.T) {
	t.Parallel()
	if got, ok := utilizationHeadroom(nil); ok {
		t.Fatalf("no windows must not be a verdict, got %v", got)
	}
	cases := []struct {
		in   []float64
		want float64
	}{
		{in: []float64{0}, want: 1},
		{in: []float64{10, 40}, want: 0.6}, // the most-used window binds
		{in: []float64{100}, want: 0},
		{in: []float64{-5}, want: 1}, // garbage in, clamped
		{in: []float64{250, 10}, want: 0},
	}
	for _, tc := range cases {
		got, ok := utilizationHeadroom(tc.in)
		if !ok || got != tc.want {
			t.Fatalf("%v -> %v/%v want %v", tc.in, got, ok, tc.want)
		}
	}
}

func TestQuotaReportFreshness(t *testing.T) {
	t.Parallel()
	fresh := ClaudeOAuthUsageReport{Available: true, FetchedAt: time.Now().Format(time.RFC3339)}
	if !quotaReportIsFresh(fresh) {
		t.Fatal("a snapshot taken now is fresh")
	}
	old := ChatGPTOAuthUsageReport{Available: true, FetchedAt: time.Now().Add(-2 * time.Hour).Format(time.RFC3339)}
	if quotaReportIsFresh(old) {
		t.Fatal("a two hour old snapshot must not drive routing")
	}
	if quotaReportIsFresh(CursorOAuthUsageReport{Available: true}) {
		t.Fatal("a report with no timestamp is not trustworthy")
	}
	if quotaReportIsFresh("not a report") {
		t.Fatal("an unknown cached type is not a quota report")
	}
}
