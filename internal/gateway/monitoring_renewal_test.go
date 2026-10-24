package gateway

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"github.com/luca/llm-protocol-gateway/internal/monitoring"
	"github.com/luca/llm-protocol-gateway/internal/store"
)

// A failed refresh must land in the event stream as a classified outcome: the
// console and the rules only ever see the kind, never the upstream error text
// (which may quote tokens or request bodies).
func TestRecordRenewalWritesEvent(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	server := &Server{logs: monitor.NewStore()}
	server.monitoring = monitoring.NewService(nil, nil, nil, db, db)

	provider := domain.Provider{ID: "p-claude", AuthType: domain.AuthTypeClaudeOAuth}
	started := time.Now().Add(-2 * time.Second).UTC()
	secretErr := errors.New(`refresh failed: 401 oauth_token=sk-ant-oat01-SECRET`)
	server.recordRenewal(provider, "claude", started, secretErr)

	events, err := db.QueryMonitorEvents(monitoring.EventQuery{Kind: monitoring.EventKindRenewal})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != 1 {
		t.Fatalf("renewal events = %d, want 1", len(events.Items))
	}
	event := events.Items[0]
	if event.Severity != monitoring.SeverityP0 {
		t.Fatalf("failed renewal severity = %q, want P0", event.Severity)
	}
	if event.TargetID != "p-claude" {
		t.Fatalf("target = %q", event.TargetID)
	}
	detail, _ := json.Marshal(event.Detail)
	if !strings.Contains(string(detail), "auth_failed") {
		t.Fatalf("detail should classify the failure: %s", detail)
	}
	if strings.Contains(string(detail), "SECRET") {
		t.Fatalf("detail leaked the raw upstream error: %s", detail)
	}
	if event.PushError != "" && strings.Contains(event.PushError, "SECRET") {
		t.Fatalf("push error leaked the raw upstream error: %q", event.PushError)
	}

	// A successful refresh is recorded too, so the console can show that the
	// credential is being kept alive rather than merely not failing.
	server.recordRenewal(provider, "claude", time.Now().UTC(), nil)
	events, err = db.QueryMonitorEvents(monitoring.EventQuery{Kind: monitoring.EventKindRenewal})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != 2 {
		t.Fatalf("renewal events after success = %d, want 2", len(events.Items))
	}
	if events.Items[0].Severity != monitoring.SeverityInfo {
		t.Fatalf("successful renewal severity = %q", events.Items[0].Severity)
	}
	if events.Items[1].Severity != monitoring.SeverityP0 {
		t.Fatalf("failed renewal severity = %q", events.Items[1].Severity)
	}
}

// Before StartMonitoring the field is nil and renewals must stay silent: the
// legacy alert scanner is the only notifier then, and a half-built server (as
// every existing test constructs) must not panic on a refresh.
func TestRecordRenewalWithoutMonitoringIsNoop(t *testing.T) {
	server := &Server{logs: monitor.NewStore()}
	server.recordRenewal(domain.Provider{ID: "p"}, "claude", time.Now(), errors.New("boom"))
	var nilServer *Server
	nilServer.recordRenewal(domain.Provider{ID: "p"}, "claude", time.Now(), nil)
}

// Once the monitoring center runs it owns delivery. The legacy alert row is
// kept as history but marked as not pushed, so a Telegram alert is not sent
// twice: once by the old scanner and once by the new channel dispatcher.
func TestLegacyAlertPushDisabledWhenMonitoringRuns(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	insert := func() int64 {
		id, insertErr := db.InsertAlert(monitor.Alert{
			Time: time.Now().UTC(), Rule: "apikey_multi_ip", Severity: "warn",
			APIKeyID: "k1", APIKeyName: "main", IPs: []string{"1.1.1.1", "2.2.2.2"},
			IPCount: 2, WindowMinutes: 10, RequestCount: 4, Status: "unread",
		})
		if insertErr != nil {
			t.Fatal(insertErr)
		}
		return id
	}
	pushStatus := func(id int64) string {
		alert, ok, lookupErr := db.AlertByID(id)
		if lookupErr != nil || !ok {
			t.Fatalf("alert %d missing: %v", id, lookupErr)
		}
		return alert.PushStatus
	}

	server := &Server{logs: monitor.NewStore(), alertStore: db}
	server.monitoring = monitoring.NewService(nil, nil, nil, db, db)

	// Telegram is fully configured, so without the switch this would attempt a
	// real send. With monitoring running it must short-circuit instead.
	telegram := domain.TelegramSettings{Enabled: true, BotToken: "t", ChatID: "1"}
	id := insert()
	server.pushAlert(monitor.Alert{ID: id, Time: time.Now().UTC()}, telegram)
	if got := pushStatus(id); got != domain.AlertPushDisabled {
		t.Fatalf("push status with monitoring = %q, want %q", got, domain.AlertPushDisabled)
	}

	// Without monitoring the legacy path still owns delivery: a configured but
	// unreachable Telegram records a failure rather than short-circuiting.
	legacy := &Server{logs: monitor.NewStore(), alertStore: db}
	legacyID := insert()
	legacy.pushAlert(monitor.Alert{ID: legacyID, Time: time.Now().UTC(), Rule: "apikey_multi_ip"}, telegram)
	if got := pushStatus(legacyID); got == domain.AlertPushDisabled {
		t.Fatalf("legacy server skipped the push it should have attempted")
	}
}
