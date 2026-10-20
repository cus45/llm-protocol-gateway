package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func TestRequestStatsKeepsTodaySeparateFromSelectedRange(t *testing.T) {
	for _, role := range []domain.UserRole{domain.UserRoleAdmin, domain.UserRoleUser} {
		t.Run(string(role), func(t *testing.T) {
			now := time.Now().Local()
			today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
			logs := monitor.NewStore()
			key := domain.APIKey{ID: "period-key", Name: "Period key", OwnerUserID: "period-user"}
			for _, at := range []time.Time{today.AddDate(0, 0, -1).Add(time.Hour), today.AddDate(0, 0, -1).Add(2 * time.Hour), now} {
				log := monitor.RequestLog{Time: at, APIKeyID: key.ID, APIKeyName: key.Name, ProviderID: "provider", Model: "model", Status: 200, InputTokens: 10}
				logs.Add(log)
				logs.ApplyUsageEventSync(monitor.UsageEvent{Time: at, APIKeyID: key.ID, APIKeyName: key.Name, UserID: key.OwnerUserID, ProviderID: log.ProviderID, Model: log.Model, Status: 200, InputTokens: 10})
			}
			server := NewServer(NewRouter(domain.GatewayState{APIKeys: []domain.APIKey{key}}), logs, nil)
			req := httptest.NewRequest("GET", "/__request-stats?from="+today.AddDate(0, 0, -1).Format("2006-01-02")+"&to="+today.Format("2006-01-02"), nil)
			req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, sessionIdentity{UserID: key.OwnerUserID, Role: role}))
			rec := httptest.NewRecorder()
			server.handleRequestStats(rec, req)
			var snapshot monitor.UsageStatsSnapshot
			if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.Range == nil || snapshot.Range.Total.RequestCount != 3 {
				t.Fatalf("range = %+v, want 3 requests", snapshot.Range)
			}
			if snapshot.Today.Total.RequestCount != 1 || snapshot.Today.Total.InputTokens != 10 {
				t.Fatalf("today = %+v, want only today's request", snapshot.Today.Total)
			}
			if len(snapshot.Today.ByAPIKey) != 1 || snapshot.Today.ByAPIKey[0].RequestCount != 1 {
				t.Fatalf("today key breakdown = %+v", snapshot.Today.ByAPIKey)
			}
		})
	}
}
