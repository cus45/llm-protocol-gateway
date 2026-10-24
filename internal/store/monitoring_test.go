package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/monitoring"
)

func openMonitoringTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// InsertRequestLogRowForTest writes the minimal columns the window queries read.
func (s *Store) InsertRequestLogRowForTest(when time.Time, providerID, action string, status int, ttftMs, latencyMs, outputTokens int64) error {
	_, err := s.db.Exec(`INSERT INTO request_logs
		(time, provider_id, action, status, protocol_flow, output_tokens, ttft_ms, latency_ms)
		VALUES (?, ?, ?, ?, '', ?, ?, ?)`,
		monitorTime(when), providerID, action, status, outputTokens, ttftMs, latencyMs)
	return err
}

func TestMonitorStateRoundTrip(t *testing.T) {
	s := openMonitoringTestStore(t)
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	state := monitoring.AlertState{
		Fingerprint: "provider_down|provider:p1", RuleKey: "provider_down",
		Severity: monitoring.SeverityP1, TargetType: "provider", TargetID: "p1",
		TargetName: "P1", Status: monitoring.StatusFiring, Disposition: monitoring.DispositionOpen,
		Count: 1, FirstSeen: now, LastSeen: now, Detail: map[string]any{"successRatio": 0.5},
	}
	saved, err := s.UpsertMonitorState(state)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == 0 {
		t.Fatal("expected id")
	}
	states, err := s.LoadMonitorStates()
	if err != nil || len(states) != 1 {
		t.Fatalf("states=%v err=%v", states, err)
	}
	if states[0].Detail["successRatio"] != 0.5 {
		t.Fatalf("detail lost: %#v", states[0].Detail)
	}
}

func TestMonitorEventsQueryAndPush(t *testing.T) {
	s := openMonitoringTestStore(t)
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	id, err := s.InsertMonitorEvent(monitoring.Event{
		Time: now, Kind: monitoring.EventKindAlert, Severity: monitoring.SeverityP0,
		RuleKey: "auth_invalid", TargetType: "provider", TargetID: "p1", Title: "t",
		Detail: map[string]any{"status": 401}, Fingerprint: "auth_invalid|provider:p1",
		Status: monitoring.StatusFiring, Count: 1, FirstSeen: now, LastSeen: now,
	})
	if err != nil || id == 0 {
		t.Fatalf("insert event id=%d err=%v", id, err)
	}
	if err := s.UpdateMonitorEventPush(id, monitoring.PushSent, ""); err != nil {
		t.Fatal(err)
	}
	page, err := s.QueryMonitorEvents(monitoring.EventQuery{Page: 1, PageSize: 50})
	if err != nil || page.Total != 1 || page.Items[0].PushStatus != monitoring.PushSent {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	push := monitoring.EventPush{EventID: id, ChannelID: "ch1", Status: monitoring.PushSent, Attempts: 1, UpdatedAt: now}
	if err := s.UpsertMonitorEventPush(push); err != nil {
		t.Fatal(err)
	}
	pushes, err := s.ListMonitorEventPushes(id)
	if err != nil || len(pushes) != 1 || pushes[0].ChannelID != "ch1" {
		t.Fatalf("pushes=%+v err=%v", pushes, err)
	}
}

func TestQueryMonitorAlertsCarriesLatestEventAndTimeline(t *testing.T) {
	s := openMonitoringTestStore(t)
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	state, err := s.UpsertMonitorState(monitoring.AlertState{
		Fingerprint: "auth_invalid|provider:p1", RuleKey: "auth_invalid",
		Severity: monitoring.SeverityP0, TargetType: "provider", TargetID: "p1",
		TargetName: "P1", Status: monitoring.StatusFiring, Disposition: monitoring.DispositionOpen,
		Count: 2, FirstSeen: now, LastSeen: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.InsertMonitorEvent(monitoring.Event{
		Time: now, Kind: monitoring.EventKindAlert, Severity: monitoring.SeverityP0,
		RuleKey: "auth_invalid", TargetType: "provider", TargetID: "p1", Title: "t",
		Fingerprint: state.Fingerprint, Status: monitoring.StatusFiring,
		FirstSeen: now, LastSeen: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	latest, err := s.InsertMonitorEvent(monitoring.Event{
		Time: now.Add(time.Minute), Kind: monitoring.EventKindAlert, Severity: monitoring.SeverityP0,
		RuleKey: "auth_invalid", TargetType: "provider", TargetID: "p1", Title: "t2",
		Fingerprint: state.Fingerprint, Status: monitoring.StatusFiring,
		FirstSeen: now, LastSeen: now.Add(time.Minute), PushStatus: monitoring.PushSent,
		Detail: map[string]any{"timeline": []monitoring.TimelineEntry{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AppendMonitorTimeline(state.Fingerprint, monitoring.TimelineEntry{
		Action: "acknowledged", Time: now.Add(2 * time.Minute), Actor: "admin",
	}); err != nil {
		t.Fatal(err)
	}
	page, err := s.QueryMonitorAlerts(monitoring.AlertQuery{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("page = %+v", page)
	}
	view := page.Items[0]
	if view.LatestEventID != latest {
		t.Fatalf("latestEventId = %d, want %d", view.LatestEventID, latest)
	}
	if view.PushStatus != monitoring.PushSent {
		t.Fatalf("pushStatus = %q", view.PushStatus)
	}
	if len(view.RelatedEventIDs) != 2 || view.RelatedEventIDs[0] != latest || view.RelatedEventIDs[1] != first {
		t.Fatalf("relatedEventIds = %v", view.RelatedEventIDs)
	}
	event, ok, err := s.MonitorEventByID(latest)
	if err != nil || !ok {
		t.Fatalf("latest event missing: %v", err)
	}
	timeline, _ := event.Detail["timeline"].([]any)
	if len(timeline) != 1 {
		t.Fatalf("timeline = %#v", event.Detail["timeline"])
	}
}

func TestProviderWindowStats(t *testing.T) {
	s := openMonitoringTestStore(t)
	now := time.Now().UTC().Truncate(time.Minute)
	for i, status := range []int{200, 200, 401, 429, 500} {
		if err := s.InsertRequestLogRowForTest(now.Add(-time.Duration(i)*time.Minute), "p1", "convert", status, 10, 100, 200); err != nil {
			t.Fatal(err)
		}
	}
	// 6 分钟前的行落在 5 分钟窗口之外，只能计入 10/15 分钟窗口。
	if err := s.InsertRequestLogRowForTest(now.Add(-6*time.Minute), "p1", "convert", 200, 10, 100, 200); err != nil {
		t.Fatal(err)
	}
	windows, err := s.WindowStatsByProvider(now.Add(-15*time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	if windows["p1"][5].Total != 5 {
		t.Fatalf("5m total = %d", windows["p1"][5].Total)
	}
	if windows["p1"][10].Total != 6 {
		t.Fatalf("10m total = %d", windows["p1"][10].Total)
	}
	if windows["p1"][5].Auth401403 != 1 || windows["p1"][5].RateLimited != 1 || windows["p1"][5].ServerError != 1 {
		t.Fatalf("window stats = %+v", windows["p1"][5])
	}
}
