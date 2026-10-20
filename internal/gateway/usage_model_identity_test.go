package gateway

import (
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func TestNormalizeUsageModelIdentitiesFoldsProviderPinnedRows(t *testing.T) {
	router := NewRouter(domain.GatewayState{Providers: []domain.Provider{{
		ID:     "qoder-cn-账号",
		Models: []domain.Model{{ID: "dfmodel"}},
	}}})
	server := NewServer(router, monitor.NewStore(), nil)
	snapshot := &monitor.UsageStatsSnapshot{
		Today: monitor.TodayStatsSnapshot{ByModel: []monitor.ModelDayStats{
			{Model: "qoder-cn/dfmodel", RequestCount: 3, InputTokens: 100},
			{Model: "dfmodel", RequestCount: 2, InputTokens: 40},
		}},
		Month: monitor.PeriodStatsSnapshot{ByModel: []monitor.ModelDayStats{
			{Model: "workbuddy/deepseek-v4.1-flash", RequestCount: 1},
		}},
		Range: &monitor.PeriodStatsSnapshot{ByModel: []monitor.ModelDayStats{
			{Model: "qoder-cn/dfmodel", InputTokens: 10, OutputTokens: 5},
		}},
	}

	server.normalizeUsageModelIdentities(snapshot)

	if len(snapshot.Today.ByModel) != 1 || snapshot.Today.ByModel[0].Model != "dfmodel" || snapshot.Today.ByModel[0].RequestCount != 5 || snapshot.Today.ByModel[0].InputTokens != 140 {
		t.Fatalf("today rows not folded: %+v", snapshot.Today.ByModel)
	}
	if len(snapshot.Month.ByModel) != 1 || snapshot.Month.ByModel[0].Model != "workbuddy/deepseek-v4.1-flash" {
		t.Fatalf("an id the catalog does not carry must stay untouched: %+v", snapshot.Month.ByModel)
	}
	if snapshot.Range == nil || len(snapshot.Range.ByModel) != 1 || snapshot.Range.ByModel[0].Model != "dfmodel" {
		t.Fatalf("range rows not folded: %+v", snapshot.Range)
	}
}
