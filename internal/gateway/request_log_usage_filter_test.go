package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"github.com/luca/llm-protocol-gateway/internal/store"
)

func TestUsageLogFiltersMatchExactEntitiesAndPreserveIsolation(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		name := "memory"
		if persisted {
			name = "sqlite"
		}
		t.Run(name, func(t *testing.T) {
			logs := monitor.NewStore()
			router := NewRouter(domain.GatewayState{APIKeys: []domain.APIKey{
				{ID: "own", Name: "duplicate", OwnerUserID: "user"},
				{ID: "other", Name: "duplicate", OwnerUserID: "other-user"},
			}})
			server := NewServer(router, logs, nil)
			var db *store.Store
			if persisted {
				var err error
				db, err = store.Open(filepath.Join(t.TempDir(), "gateway.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				server = NewServer(router, logs, db)
			}
			for _, item := range []monitor.RequestLog{
				{Time: time.Now(), APIKeyID: "own", APIKeyName: "duplicate", Model: "model", Status: 200},
				{Time: time.Now(), APIKeyID: "own", APIKeyName: "duplicate", Model: "model-extra", Status: 200},
				{Time: time.Now(), APIKeyID: "other", APIKeyName: "duplicate", Model: "model", Status: 200},
			} {
				logs.Add(item)
				if db != nil {
					if err := db.AppendRequestLog(item); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, tc := range []struct {
				name, query string
				identity    sessionIdentity
				count       int
			}{
				{"exact model", "model=model", adminIdentity(), 2},
				{"duplicate key names", "apiKeyId=own&model=model", adminIdentity(), 1},
				{"user own key", "apiKeyId=own&model=model", sessionIdentity{UserID: "user", Role: domain.UserRoleUser}, 1},
				{"user other key", "apiKeyId=other&model=model", sessionIdentity{UserID: "user", Role: domain.UserRoleUser}, 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					req := httptest.NewRequest(http.MethodGet, "/__logs?"+tc.query, nil)
					req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, tc.identity))
					rec := httptest.NewRecorder()
					server.handleLogs(rec, req)
					if rec.Code != http.StatusOK {
						t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
					}
					var result monitor.RequestLogPage
					if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
						t.Fatal(err)
					}
					if result.Total != tc.count {
						t.Fatalf("total = %d, want %d: %s", result.Total, tc.count, rec.Body.String())
					}
					for _, item := range result.Items {
						if item.Model != "model" {
							t.Fatalf("inexact model: %s", item.Model)
						}
					}
				})
			}
		})
	}
}
