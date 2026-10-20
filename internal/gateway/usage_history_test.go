package gateway

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"github.com/luca/llm-protocol-gateway/internal/store"
)

func TestUsageKeyDimensionsSurviveRestartWithoutRequestLogs(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	at := now.AddDate(0, 0, -40)
	logs := monitor.NewStore()
	logs.SetUsageDailyStore(db)
	for _, key := range []string{"own", "other"} {
		for i := 0; i < 2; i++ {
			logs.ApplyUsageEventSync(monitor.UsageEvent{Time: at, APIKeyID: key, ProviderID: key + "-provider", Model: key + "-model", OutputProtocol: "openai", Status: 200, InputTokens: 100, OutputTokens: 20, CacheTokens: 30, LatencyMs: 80, TTFTMs: 10, RxBytes: 40, TxBytes: 50})
		}
	}
	days, last, err := db.LoadUsageSince(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	restarted := monitor.NewStore()
	restarted.BootstrapUsageDays(days, last)
	from := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.Local)
	snapshot := restarted.UsageStatsRangeForKeys(now, from, from.AddDate(0, 0, 1), []string{"own"})
	if snapshot.Range.Total.RequestCount != 2 || len(snapshot.Range.ByProvider) != 1 || snapshot.Range.ByProvider[0].ProviderID != "own-provider" || len(snapshot.Range.ByModel) != 1 || snapshot.Range.ByModel[0].Model != "own-model" || len(snapshot.Range.ByProtocol) != 1 {
		t.Fatalf("lost or leaked dimensions: %+v", snapshot.Range)
	}
	if snapshot.Status[0].RequestCount != 2 || snapshot.Daily[0].AvgLatencyMs != 80 || snapshot.Daily[0].AvgTTFTMs != 10 || snapshot.Daily[0].RxBytes != 80 {
		t.Fatalf("lost status or timing: %+v", snapshot)
	}
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore(), db)
	server.RebuildUsageStats()
	historical := server.logs.UsageStatsRange(now, from, from.AddDate(0, 0, 1))
	if historical.Range.Total.RequestCount != 4 {
		t.Fatalf("startup erased permanent history: %+v", historical.Range)
	}
	// The schema version is now current: repeated startups must not erase or double count.
	server.RebuildUsageStats()
	historical = server.logs.UsageStatsRange(now, from, from.AddDate(0, 0, 1))
	if historical.Range.Total.RequestCount != 4 {
		t.Fatalf("second startup changed history: %+v", historical.Range)
	}
}
