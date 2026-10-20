package gateway

import (
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

// Regression: with persistent daily usage enabled the old fallback was
// skipped entirely, so hourly buckets only ever covered whatever happened to
// sit in the in-memory log ring (e.g. 500 of 1,911 requests after a restart).
func TestRequestStatsCompletesHourlyFromPersistedLogs(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer db.Close()

	router := NewRouter(domain.GatewayState{
		APIKeys: []domain.APIKey{{ID: "key-a", Name: "Key A", OwnerUserID: "user-a"}},
	})
	logs := monitor.NewStore()
	server := NewServer(router, logs, db)
	server.SetAdminAuthStore(newMemoryAdminAuthStore())

	dayStart := time.Date(2026, 7, 8, 0, 0, 0, 0, time.Local)
	persisted := make([]monitor.RequestLog, 0, 3)
	for i := 0; i < 3; i++ {
		at := dayStart.Add(time.Duration(9+i) * time.Hour)
		persisted = append(persisted, monitor.RequestLog{
			Time: at, APIKeyID: "key-a", APIKeyName: "Key A",
			ProviderID: "provider-a", Model: "model-a", Status: 200,
			InputTokens: 10, OutputTokens: 5,
		})
		logs.ApplyUsageEventSync(monitor.UsageEvent{
			Time: at, APIKeyID: "key-a", APIKeyName: "Key A", UserID: "user-a",
			ProviderID: "provider-a", Model: "model-a", Status: 200,
			InputTokens: 10, OutputTokens: 5,
		})
	}
	if err := db.AppendRequestLogs(persisted); err != nil {
		t.Fatalf("failed to persist request logs: %v", err)
	}
	// Simulate a restart that only restored the newest row into memory.
	logs.Add(persisted[len(persisted)-1])

	queryURL := "/__request-stats?from=" + url.QueryEscape(dayStart.Format(time.RFC3339)) + "&to=" + url.QueryEscape(dayStart.Add(24*time.Hour).Format(time.RFC3339))
	req := httptest.NewRequest(http.MethodGet, queryURL, nil)
	req.Host = "127.0.0.1:18093"
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var snapshot monitor.UsageStatsSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if snapshot.Range == nil || snapshot.Range.Total.RequestCount != 3 {
		t.Fatalf("expected range total 3, got %+v", snapshot.Range)
	}
	var hourly int64
	for _, bucket := range snapshot.Hourly {
		hourly += bucket.RequestCount
	}
	if hourly != 3 {
		t.Fatalf("expected hourly total 3 restored from persisted logs, got %d", hourly)
	}
	for i, hour := range []int{9, 10, 11} {
		if snapshot.Hourly[hour].RequestCount != 1 {
			t.Fatalf("expected hourly[%d] = 1, got %d (case %d)", hour, snapshot.Hourly[hour].RequestCount, i)
		}
	}
	var daily, status int64
	for _, point := range snapshot.Daily {
		daily += point.RequestCount
	}
	for _, bucket := range snapshot.Status {
		status += bucket.RequestCount
	}
	if daily != 3 || status != 3 {
		t.Fatalf("expected daily and status totals to match range (3), got daily=%d status=%d", daily, status)
	}
}

// A range longer than retention can only receive hourly data from the logs that
// still exist. Those partial logs must not overwrite the permanent range, daily
// or status totals kept in the daily buckets.
func TestRequestStatsKeepsPermanentTotalsBeyondRetention(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer db.Close()

	router := NewRouter(domain.GatewayState{
		APIKeys: []domain.APIKey{{ID: "key-a", Name: "Key A", OwnerUserID: "user-a"}},
	})
	logs := monitor.NewStore()
	server := NewServer(router, logs, db)
	server.SetAdminAuthStore(newMemoryAdminAuthStore())

	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	from := dayStart.AddDate(0, 0, -29)
	to := dayStart.AddDate(0, 0, 1)

	// Permanent buckets hold five requests across days the log retention no
	// longer reaches.
	for i := 0; i < 5; i++ {
		at := dayStart.AddDate(0, 0, -(i + 2)).Add(9 * time.Hour)
		logs.ApplyUsageEventSync(monitor.UsageEvent{
			Time: at, APIKeyID: "key-a", APIKeyName: "Key A", UserID: "user-a",
			ProviderID: "provider-a", Model: "model-a", Status: 200,
			InputTokens: 10, OutputTokens: 5,
		})
	}

	// Only three recent requests survive in persisted logs.
	persisted := make([]monitor.RequestLog, 0, 3)
	for i := 0; i < 3; i++ {
		at := dayStart.AddDate(0, 0, -1).Add(time.Duration(9+i) * time.Hour)
		persisted = append(persisted, monitor.RequestLog{
			Time: at, APIKeyID: "key-a", APIKeyName: "Key A",
			ProviderID: "provider-a", Model: "model-a", Status: 200,
			InputTokens: 10, OutputTokens: 5,
		})
	}
	if err := db.AppendRequestLogs(persisted); err != nil {
		t.Fatalf("failed to persist request logs: %v", err)
	}
	logs.Add(persisted[len(persisted)-1])

	queryURL := "/__request-stats?from=" + url.QueryEscape(from.Format(time.RFC3339)) + "&to=" + url.QueryEscape(to.Format(time.RFC3339))
	req := httptest.NewRequest(http.MethodGet, queryURL, nil)
	req.Host = "127.0.0.1:18093"
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var snapshot monitor.UsageStatsSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if snapshot.Range == nil || snapshot.Range.Total.RequestCount != 5 {
		t.Fatalf("expected permanent range total 5, got %+v", snapshot.Range)
	}
	var hourly, daily, status int64
	for _, bucket := range snapshot.Hourly {
		hourly += bucket.RequestCount
	}
	for _, point := range snapshot.Daily {
		daily += point.RequestCount
	}
	for _, bucket := range snapshot.Status {
		status += bucket.RequestCount
	}
	if hourly != 3 {
		t.Fatalf("expected hourly total 3 from retained logs, got %d", hourly)
	}
	if daily != 5 || status != 5 {
		t.Fatalf("partial logs overwrote permanent totals: daily=%d status=%d", daily, status)
	}
}
