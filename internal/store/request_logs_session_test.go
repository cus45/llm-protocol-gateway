package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// 控制台的会话视图按 sessionId 过滤日志：它存在 analysis_json 里，过滤必须在
// 数据库侧完成（json_extract），否则每次翻页都要把全部行读出来再比对。
func TestQueryRequestLogsBySession(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC()
	logs := []monitor.RequestLog{
		{ID: 1, Time: now.Add(-3 * time.Minute), Model: "m", Status: 200, Analysis: monitor.RequestAnalysis{SessionID: "sess-a"}},
		{ID: 2, Time: now.Add(-2 * time.Minute), Model: "m", Status: 200, Analysis: monitor.RequestAnalysis{SessionID: "sess-b"}},
		{ID: 3, Time: now.Add(-time.Minute), Model: "m", Status: 200, Analysis: monitor.RequestAnalysis{SessionID: "sess-a"}},
	}
	for _, log := range logs {
		if err := store.AppendRequestLog(log); err != nil {
			t.Fatalf("append %d: %v", log.ID, err)
		}
	}

	page, err := store.QueryRequestLogs(monitor.RequestLogQuery{SessionID: "sess-a", Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	items := page.Items
	if len(items) != 2 {
		t.Fatalf("session-a returned %d rows, want 2", len(items))
	}
	for _, item := range items {
		if item.Analysis.SessionID != "sess-a" {
			t.Fatalf("a row of another session leaked in: %q", item.Analysis.SessionID)
		}
	}

	// No filter: every row is still visible.
	all, err := store.QueryRequestLogs(monitor.RequestLogQuery{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("query all: %v", err)
	}
	if len(all.Items) != 3 {
		t.Fatalf("unfiltered query returned %d rows, want 3", len(all.Items))
	}
}
