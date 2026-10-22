package gateway

import (
	"context"
	"net/http"
	"testing"
)

// 额度查询的地址钉死在厂商主机上：个人版走 bigmodel 站点，Key 不会因为
// BaseURL 写错而发到别处。
func TestFetchZhipuUsageQueriesPinnedHost(t *testing.T) {
	var gotPath, gotAuth string
	probe := useProbeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"level":"lite","limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":42}]}}`))
	}))

	report, err := fetchZhipuUsage(context.Background(), "https://open.bigmodel.cn/api/paas/v4", "zhipu-key")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if probe.lastHost != "open.bigmodel.cn" || gotPath != zhipuQuotaPath {
		t.Fatalf("queried %q %q, want open.bigmodel.cn %q", probe.lastHost, gotPath, zhipuQuotaPath)
	}
	// 智谱不带 Bearer 前缀，换了就得跟着改。
	if gotAuth != "zhipu-key" {
		t.Fatalf("authorization header = %q, want the bare key", gotAuth)
	}
	if !report.Available || report.Level != "lite" || report.FiveHour == nil || report.FiveHour.Utilization != 42 {
		t.Fatalf("unexpected report: %+v", report)
	}
}
