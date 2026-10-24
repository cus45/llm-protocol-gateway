package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/gateway"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"github.com/luca/llm-protocol-gateway/internal/store"
)

// A soft-deleted provider keeps its membership on a quota task, and undoing the
// delete through the real HTTP door brings the task back. This is the
// end-to-end shape of the keeper fix: delete → restore → membership intact and
// the task enabled again, without anyone re-editing the schedule.
//
// Everything goes through Server.Handler(), the same routes the console uses,
// so no test-only hook has to exist in the production package.
func TestQuotaTaskMembershipSurvivesSoftDeleteThroughTheStore(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	provider := domain.Provider{
		ID: "p1", Name: "Claude", Protocol: domain.ProtocolClaude,
		AuthType: domain.AuthTypeAPIKey, BaseURL: "http://127.0.0.1:9/v1",
		APIKeySource: "literal:sk-test", AuthHeader: "x-api-key",
		DefaultModel: "claude-sonnet-4-5",
	}
	router := gateway.NewRouter(domain.GatewayState{Providers: []domain.Provider{provider}})
	server := gateway.NewServer(router, monitor.NewStore(), db)
	if err := db.SaveScheduledTask(domain.ScheduledTask{
		ID: "task-1", Name: "早九点", Enabled: true, Mode: domain.ScheduleDailyTimes,
		Times: []string{"09:00"}, ProviderIDs: []string{"p1"},
		NextRunAt: "2099-01-01T09:00:00Z", CreatedAt: "2026-10-16T08:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := router.DeleteProvider("p1"); err != nil {
		t.Fatal(err)
	}
	// A provider that never existed stands in for a purge: that id is gone for good.
	stored, err := db.ScheduledTaskByID("task-1")
	if err != nil {
		t.Fatal(err)
	}
	stored.ProviderIDs = []string{"p1", "purged-id"}
	if err := db.SaveScheduledTask(stored); err != nil {
		t.Fatal(err)
	}

	handler := server.Handler()
	admin := loginAsAdmin(t, handler)

	// The keeper pauses a task whose members are all deleted, keeping the ids.
	runQuotaKeeperTick(t, router, db)
	stored, err = db.ScheduledTaskByID("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stored.ProviderIDs, ","); got != "p1" {
		t.Fatalf("after tick providers = %q; the soft-deleted id must stay and the purged id must go", got)
	}
	if stored.Enabled {
		t.Fatal("a task with no schedulable member must be paused")
	}

	// Restore goes through the handler, the same door the console uses.
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/__providers/p1/restore", nil)
	for _, cookie := range admin.Cookies() {
		request.AddCookie(cookie)
	}
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("restore status = %d body = %s", recorder.Code, recorder.Body.String())
	}

	runQuotaKeeperTick(t, router, db)
	stored, err = db.ScheduledTaskByID("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Enabled {
		t.Fatalf("the restored provider's task is still paused: %+v", stored)
	}
	if got := strings.Join(stored.ProviderIDs, ","); got != "p1" {
		t.Fatalf("after restore providers = %q, want p1", got)
	}
}

// loginAsAdmin sets the admin password through the real setup route and logs in,
// returning the session cookies a browser would hold.
func loginAsAdmin(t *testing.T, handler http.Handler) *http.Response {
	t.Helper()
	setup := httptest.NewRecorder()
	// A loopback request, the way the desktop app sets the first password on a
	// fresh instance.
	setupRequest := httptest.NewRequest(http.MethodPost, "/__auth/setup",
		strings.NewReader(`{"password":"test-admin-password-123"}`))
	setupRequest.Host = "127.0.0.1:18093"
	setupRequest.RemoteAddr = "127.0.0.1:50505"
	handler.ServeHTTP(setup, setupRequest)
	if setup.Code != http.StatusOK && setup.Code != http.StatusConflict {
		t.Fatalf("setup status = %d body = %s", setup.Code, setup.Body.String())
	}
	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodPost, "/__auth/login",
		strings.NewReader(`{"password":"test-admin-password-123"}`))
	loginRequest.Host = "127.0.0.1:18093"
	loginRequest.RemoteAddr = "127.0.0.1:50505"
	handler.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d body = %s", login.Code, login.Body.String())
	}
	return login.Result()
}

// runQuotaKeeperTick runs one keeper pass over the real store, through the same
// entry point the background loop uses.
func runQuotaKeeperTick(t *testing.T, router *gateway.Router, db *store.Store) {
	t.Helper()
	server := gateway.NewServer(router, monitor.NewStore(), db)
	server.RunScheduledTasksOnce(context.Background())
}
