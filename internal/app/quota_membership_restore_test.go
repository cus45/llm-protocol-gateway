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
// delete through the real store + router + server brings the task back. This is
// the end-to-end shape of the keeper fix: delete → tick → restore → membership
// intact and the task enabled again, without anyone re-editing the schedule.
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

	server.RunQuotaKeeperTickForTest(context.Background())

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
	server.HandleRestoreProviderForTest(recorder, "p1")
	if recorder.Code != http.StatusOK {
		t.Fatalf("restore status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	server.RunQuotaKeeperTickForTest(context.Background())

	stored, err = db.ScheduledTaskByID("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Enabled {
		t.Fatal("restoring the provider must re-enable the task")
	}
	if got := strings.Join(stored.ProviderIDs, ","); got != "p1" {
		t.Fatalf("providers after restore = %q", got)
	}
}
