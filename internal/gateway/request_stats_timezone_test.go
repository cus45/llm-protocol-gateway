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

func TestRequestStatsTimezoneOffsetMatching(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer db.Close()

	router := NewRouter(domain.GatewayState{
		APIKeys: []domain.APIKey{
			{ID: "key-bj", Name: "Beijing User Key", OwnerUserID: "user-bj"},
		},
	})
	server := NewServer(router, monitor.NewStore(), db)
	server.SetAdminAuthStore(newMemoryAdminAuthStore())

	// Request happened at 2026-10-14 05:41:52 UTC.
	// In Beijing (UTC+8): 2026-10-14 13:41:52.
	// In US PDT (UTC-7): 2026-10-13 22:41:52 (previous day).
	reqTime := time.Date(2026, 10, 14, 5, 41, 52, 0, time.UTC)
	server.recordRequestLog(
		reqTime, domain.APIKey{ID: "key-bj", Name: "Beijing User Key", OwnerUserID: "user-bj"}, true,
		"r1", "provider-openai", "claude-3-5-sonnet", "chat", "claude->claude", "/v1/messages",
		200, TokenUsage{InputTokens: 100, OutputTokens: 50, CacheTokens: 20},
		nil, nil,
	)
	server.FlushRequestLogs()

	// Browser in Beijing queries "today" (2026-10-14):
	// from = 2026-10-14T00:00:00+08:00, to = 2026-10-14T23:59:59.999+08:00
	fromParam := "2026-10-14T00:00:00+08:00"
	toParam := "2026-10-14T23:59:59.999+08:00"
	queryURL := "/__request-stats?from=" + url.QueryEscape(fromParam) + "&to=" + url.QueryEscape(toParam)

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

	if snapshot.Range == nil {
		t.Fatalf("expected Range stats to be present")
	}
	if snapshot.Range.Total.RequestCount != 1 {
		t.Fatalf("expected Range.Total.RequestCount = 1, got %d", snapshot.Range.Total.RequestCount)
	}
	if snapshot.Range.Total.InputTokens != 100 {
		t.Fatalf("expected InputTokens = 100, got %d", snapshot.Range.Total.InputTokens)
	}
	if snapshot.Range.Total.OutputTokens != 50 {
		t.Fatalf("expected OutputTokens = 50, got %d", snapshot.Range.Total.OutputTokens)
	}
	if snapshot.Range.Total.CacheTokens != 20 {
		t.Fatalf("expected CacheTokens = 20, got %d", snapshot.Range.Total.CacheTokens)
	}

	// Verify Hourly distribution: in Beijing time (loc = +08:00), 05:41 UTC is hour 13!
	if len(snapshot.Hourly) != 24 {
		t.Fatalf("expected 24 hourly buckets, got %d", len(snapshot.Hourly))
	}
	if snapshot.Hourly[13].RequestCount != 1 {
		t.Fatalf("expected Hourly[13].RequestCount = 1, got %d", snapshot.Hourly[13].RequestCount)
	}

	// Verify Status stats: 2xx = 1
	var status2xx int64
	for _, s := range snapshot.Status {
		if s.Class == "2xx" {
			status2xx = s.RequestCount
		}
	}
	if status2xx != 1 {
		t.Fatalf("expected 2xx status count = 1, got %d", status2xx)
	}

	// Verify Daily: date label in +08:00 should be 2026-10-14
	if len(snapshot.Daily) == 0 {
		t.Fatalf("expected daily points, got none")
	}
	var foundDaily bool
	for _, d := range snapshot.Daily {
		if d.Date == "2026-10-14" && d.RequestCount == 1 {
			foundDaily = true
		}
	}
	if !foundDaily {
		t.Fatalf("expected daily point for 2026-10-14 with 1 request, got %v", snapshot.Daily)
	}

	// Verify ByModel, ByProvider, ByAPIKey, ByUser
	if len(snapshot.Range.ByModel) == 0 || snapshot.Range.ByModel[0].Model != "claude-3-5-sonnet" {
		t.Fatalf("expected ByModel claude-3-5-sonnet, got %v", snapshot.Range.ByModel)
	}
	if len(snapshot.Range.ByProvider) == 0 || snapshot.Range.ByProvider[0].ProviderID != "provider-openai" {
		t.Fatalf("expected ByProvider provider-openai, got %v", snapshot.Range.ByProvider)
	}
	if len(snapshot.Range.ByAPIKey) == 0 || snapshot.Range.ByAPIKey[0].APIKeyName != "Beijing User Key" {
		t.Fatalf("expected ByAPIKey Beijing User Key, got %v", snapshot.Range.ByAPIKey)
	}
	if len(snapshot.Range.ByUser) == 0 || snapshot.Range.ByUser[0].UserID != "user-bj" {
		t.Fatalf("expected ByUser user-bj, got %v", snapshot.Range.ByUser)
	}
}
