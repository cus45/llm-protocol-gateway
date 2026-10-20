package gateway

import (
	"context"
	"encoding/json"
	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"github.com/luca/llm-protocol-gateway/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageAnalysisIsolationAndPersistence(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		name := "memory"
		if persisted {
			name = "sqlite"
		}
		t.Run(name, func(t *testing.T) {
			logs := monitor.NewStore()
			router := NewRouter(domain.GatewayState{APIKeys: []domain.APIKey{{ID: "own", OwnerUserID: "user"}, {ID: "other", OwnerUserID: "other"}}})
			server := NewServer(router, logs, nil)
			var db *store.Store
			if persisted {
				var err error
				db, err = store.Open(filepath.Join(t.TempDir(), "analysis.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				server = NewServer(router, logs, db)
			}
			now := time.Now().UTC()
			for _, id := range []string{"own", "other"} {
				item := monitor.RequestLog{Time: now, APIKeyID: id, Status: 200, RequestBody: "secret-body", ResponseBody: "secret-response", ClientIP: "private-ip", ErrorDescription: "secret-error", Analysis: monitor.RequestAnalysis{Captured: true, SessionID: "digest", Messages: 3, ToolBytes: 42}}
				logs.Add(item)
				if db != nil {
					if err := db.AppendRequestLog(item); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, tc := range []struct {
				identity sessionIdentity
				count    int
			}{{adminIdentity(), 2}, {sessionIdentity{UserID: "user", Role: domain.UserRoleUser}, 1}, {sessionIdentity{UserID: "no-keys", Role: domain.UserRoleUser}, 0}} {
				req := httptest.NewRequest(http.MethodGet, "/__usage-analysis?from="+now.Add(-time.Hour).Format(time.RFC3339)+"&to="+now.Add(time.Hour).Format(time.RFC3339), nil)
				req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, tc.identity))
				rec := httptest.NewRecorder()
				server.handleUsageAnalysis(rec, req)
				if rec.Code != 200 {
					t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
				}
				var result struct {
					Items []monitor.RequestLog `json:"items"`
					Total int                  `json:"total"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Total != tc.count || len(result.Items) != tc.count {
					t.Fatalf("count: %+v", result)
				}
				for _, item := range result.Items {
					if !item.Analysis.Captured || item.Analysis.Messages != 3 || item.Analysis.ToolBytes != 42 {
						t.Fatalf("analysis not persisted: %+v", item.Analysis)
					}
					if !tc.identity.isAdmin() && item.APIKeyID != "own" {
						t.Fatal("cross-user disclosure")
					}
				}
				for _, forbidden := range []string{"secret-body", "secret-response", "private-ip", "secret-error"} {
					if strings.Contains(rec.Body.String(), forbidden) {
						t.Fatalf("disclosed %s", forbidden)
					}
				}
			}
		})
	}
}

func TestCaptureRequestAnalysis(t *testing.T) {
	body := []byte(`{"stream":true,"system":"rules","messages":[{"role":"user","content":[{"type":"tool_result","content":"done"}]}],"tools":[{"name":"read"}],"metadata":{"session_id":"private-session"}}`)
	a := captureRequestAnalysis(nil, "key-a", body)
	if !a.Captured || !a.Stream || a.Messages != 1 || a.Tools != 1 || a.ToolResults != 1 || a.SystemBytes == 0 || a.MessageBytes == 0 || a.ToolBytes == 0 {
		t.Fatalf("unexpected analysis: %+v", a)
	}
	if len(a.SessionID) != 32 || strings.Contains(a.SessionID, "private") {
		t.Fatalf("session must be a digest: %q", a.SessionID)
	}
	if b := captureRequestAnalysis(nil, "key-b", body); a.SessionID == b.SessionID {
		t.Fatal("session identity leaked across keys")
	}
	if b := captureRequestAnalysis(nil, "key-a", body); a.SessionID != b.SessionID {
		t.Fatal("session digest is not stable")
	}
	for _, invalid := range []string{"null", "[]", "bad"} {
		if captureRequestAnalysis(nil, "key", []byte(invalid)).Captured {
			t.Fatalf("captured invalid object %s", invalid)
		}
	}
	if b := captureRequestAnalysis(nil, "key", []byte(`{"metadata":{"user_id":"same-user"}}`)); b.SessionID != "" {
		t.Fatal("user ID must not become a session")
	}
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r.Header.Set("Session_id", "explicit-session")
	if b := captureRequestAnalysis(r, "key", []byte(`{"input":"hello"}`)); b.Messages != 1 || b.SessionID == "" {
		t.Fatalf("responses string input/header: %+v", b)
	}
	if b := captureRequestAnalysis(nil, "key", []byte(`{"metadata":{"user_id":"user_x_session_abc"}}`)); b.SessionID == "" {
		t.Fatal("explicit session suffix not captured")
	}
}
