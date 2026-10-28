package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestHostMetricSamplesRoundTripAndPrune(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UnixMilli()
	batch := []HostMetricSample{
		{AtMs: now - 2000, CPU: 1.5, MemUsed: 100, MemTotal: 200, MemPercent: 50, NetRx: 7},
		{AtMs: now - 1000, CPU: 2.5, MemUsed: 110, MemTotal: 200, MemPercent: 55, DiskRead: 9},
	}
	if err := db.AppendHostMetricSamples(batch); err != nil {
		t.Fatal(err)
	}
	// Same timestamp twice must replace, not duplicate: a retried flush after a
	// crash would otherwise double-draw one point.
	if err := db.AppendHostMetricSamples([]HostMetricSample{{AtMs: now - 1000, CPU: 9.5}}); err != nil {
		t.Fatal(err)
	}

	got, err := db.HostMetricSamplesSince(now - 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("rows=%d: %+v", len(got), got)
	}
	if got[0].AtMs != now-2000 || got[1].CPU != 9.5 {
		t.Fatalf("unexpected rows: %+v", got)
	}

	// The window filter is inclusive on the left edge.
	recent, err := db.HostMetricSamplesSince(now - 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 {
		t.Fatalf("since filter kept %d rows", len(recent))
	}

	removed, err := db.PruneHostMetricSamples(now - 1000)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("pruned=%d", removed)
	}
	left, err := db.HostMetricSamplesSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].CPU != 9.5 {
		t.Fatalf("after prune: %+v", left)
	}
}
