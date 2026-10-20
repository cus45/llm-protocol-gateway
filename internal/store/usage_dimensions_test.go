package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func TestBackfillUsageKeyDimensionsPreservesTotals(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	day := time.Now().Format("2006-01-02")
	delta := monitor.UsagePersistDelta{Day: day, KeyID: "key", ProviderID: "provider", Model: "model", OutputProtocol: "openai", InputTokens: 100, StatusClass: "2xx", LatencyMs: 80, TTFTMs: 10, RxBytes: 40, TxBytes: 50}
	if err := db.ApplyUsageDeltas([]monitor.UsagePersistDelta{delta, delta}); err != nil {
		t.Fatal(err)
	}
	replay := delta
	replay.RxBytes, replay.TxBytes = 0, 0
	// Partial retained logs must leave both old totals and dimensions intact.
	if err := db.BackfillUsageKeyDimensions(time.Time{}, []monitor.UsagePersistDelta{replay}); err != nil {
		t.Fatal(err)
	}
	check := func() {
		days, _, err := db.LoadUsageSince(time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		bucket := days[day]
		child := bucket.ByKey["key"]
		if bucket.Total.RequestCount != 2 || bucket.Total.RxBytes != 80 || child.Status2xx != 2 || child.ByModel["model"].RequestCount != 2 {
			t.Fatalf("backfill damaged aggregates: %+v / %+v", bucket, child)
		}
	}
	check()
	// Repeated complete backfills replace dimensions rather than double count.
	for i := 0; i < 2; i++ {
		if err := db.BackfillUsageKeyDimensions(time.Time{}, []monitor.UsagePersistDelta{replay, replay}); err != nil {
			t.Fatal(err)
		}
		check()
	}
}
