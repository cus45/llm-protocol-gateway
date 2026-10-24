package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"github.com/luca/llm-protocol-gateway/internal/store"
)

// 用量分析要把区间内的全部请求读回来。超过一页（500 条）时，若翻页同时
// 递增 Page 又回填游标，SQL 会先按游标过滤再 OFFSET，两者叠加正好跳过整页——
// 总量字段还对得上，界面于是静默少算，看不出错。
func TestUsageAnalysisPagesEveryRequestOnce(t *testing.T) {
	t.Parallel()
	db, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const rows = 1200
	base := time.Now().Add(-2 * time.Hour).UTC()
	batch := make([]monitor.RequestLog, 0, rows)
	for i := 0; i < rows; i++ {
		batch = append(batch, monitor.RequestLog{
			Time:   base.Add(time.Duration(i) * time.Second),
			Model:  "m",
			Status: 200,
		})
	}
	if err := db.AppendRequestLogs(batch); err != nil {
		t.Fatalf("append: %v", err)
	}

	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	server.requestLogStore = db
	// 管理员身份：不按 Key 过滤，这一条测的是翻页本身。
	query := url.Values{}
	query.Set("from", base.Add(-time.Minute).Format(time.RFC3339))
	query.Set("to", base.Add(time.Duration(rows+1)*time.Second).Format(time.RFC3339))
	request := httptest.NewRequest(http.MethodGet, "/__usage-analysis?"+query.Encode(), nil)
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, adminIdentity()))
	recorder := httptest.NewRecorder()
	server.handleUsageAnalysis(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v (%s)", err, recorder.Body.String())
	}
	seen := make(map[int64]struct{}, len(payload.Items))
	for _, item := range payload.Items {
		seen[item.ID] = struct{}{}
	}
	if len(seen) != len(payload.Items) {
		t.Fatalf("the same request came back twice: %d items, %d distinct", len(payload.Items), len(seen))
	}
	if len(payload.Items) != rows {
		t.Fatalf("got %d requests, want all %d (total says %d)", len(payload.Items), rows, payload.Total)
	}
}
