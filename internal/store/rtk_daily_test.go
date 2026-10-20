package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func TestRTKDailyApplyAndLoad(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	delta := monitor.RTKPersistDelta{
		Day: "2026-07-10", ProviderID: "p1",
		Commands: 2, OriginalBytes: 8000, CompressedBytes: 1000,
		OriginalTokens: 2000, CompressedTokens: 250,
	}
	if err := s.ApplyRTKDelta(delta); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRTKDelta(delta); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRTKDelta(monitor.RTKPersistDelta{
		Day: "2026-07-11", ProviderID: "p2",
		Commands: 1, OriginalBytes: 4000, CompressedBytes: 2000,
		OriginalTokens: 1000, CompressedTokens: 500,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.LoadRTKStats(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	first := rows[0]
	if first.Date != "2026-07-10" || first.ProviderID != "p1" {
		t.Fatalf("first row = %+v", first)
	}
	if first.Commands != 4 || first.OriginalBytes != 16000 || first.CompressedBytes != 2000 {
		t.Fatalf("accumulated bucket = %+v", first)
	}
	if first.SavedTokens != 3500 {
		t.Fatalf("saved tokens = %d, want 3500", first.SavedTokens)
	}
	if first.SavedPercent <= 0 || first.SavedPercent >= 100 {
		t.Fatalf("saved percent = %v", first.SavedPercent)
	}

	from := time.Date(2026, 7, 11, 0, 0, 0, 0, time.Local)
	filtered, err := s.LoadRTKStats(from, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].ProviderID != "p2" {
		t.Fatalf("filtered rows = %+v", filtered)
	}

	// Zero-command deltas are no-ops and never create rows.
	if err := s.ApplyRTKDelta(monitor.RTKPersistDelta{Day: "2026-07-12", ProviderID: "p3"}); err != nil {
		t.Fatal(err)
	}
	all, err := s.LoadRTKStats(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("rows after zero delta = %d, want 2", len(all))
	}
}

func TestClearUsageDailyResetsRTK(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.ApplyRTKDelta(monitor.RTKPersistDelta{
		Day: "2026-07-10", ProviderID: "p1", Commands: 1,
		OriginalTokens: 100, CompressedTokens: 40,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearUsageDaily(); err != nil {
		t.Fatal(err)
	}
	rows, err := s.LoadRTKStats(time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows after clear = %d, want 0", len(rows))
	}
}
