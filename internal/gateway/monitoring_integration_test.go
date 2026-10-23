package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"github.com/luca/llm-protocol-gateway/internal/monitoring"
	"github.com/luca/llm-protocol-gateway/internal/store"
)

// TestMonitoringIntegration drives the production wiring (source → collector →
// engine → dispatcher → store → channel) on a simulated clock: one incident
// lifecycle from the first failing request to the recovery push, with no sleeps.
func TestMonitoringIntegration(t *testing.T) {
	var pushes []string
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		pushes = append(pushes, string(body[:n]))
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "success"})
	}))
	t.Cleanup(receiver.Close)

	server, db := monitorTestServer(t, domain.Provider{
		ID: "p-up", Name: "上游 A", Protocol: domain.ProtocolOpenAIChat,
		AuthType: domain.AuthTypeAPIKey, BaseURL: "https://up.example/v1",
	})
	cfg := monitoring.NewConfig(db)
	// Channels are snapshotted when the dispatcher is built and reloaded from
	// settings on each tick, so the service must be assembled after this save.
	if err := cfg.SaveChannels([]monitoring.Channel{{
		ID: "ch-feishu", Type: "feishu", Name: "值班群",
		Config: monitoring.ChannelConfig{Webhook: receiver.URL},
		// P0 only: proves the severity gate travels with the channel.
		MinSeverity: monitoring.SeverityP0, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	svc, src, _ := server.buildMonitoringService()
	server.monitoringSrc = src

	ctx := context.Background()
	base := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)

	appendRows := func(at time.Time, failures, successes int) {
		rows := make([]monitor.RequestLog, 0, failures+successes)
		for index := 0; index < failures; index++ {
			rows = append(rows, monitor.RequestLog{
				Time: at, ProviderID: "p-up", RouteID: "r-1", Model: "gpt-test",
				Action: "pass_through", Path: "/v1/chat/completions", Status: 401,
				ErrorDescription: "upstream rejected the credential",
				AccessSource:     monitor.AccessSourceLAN,
			})
		}
		for index := 0; index < successes; index++ {
			rows = append(rows, monitor.RequestLog{
				Time: at, ProviderID: "p-up", RouteID: "r-1", Model: "gpt-test",
				Action: "pass_through", Path: "/v1/chat/completions", Status: 200,
				// Output tokens must be non-zero: a 200 with an empty body would
				// also raise empty_response and muddy the assertion.
				OutputTokens: 12, LatencyMillis: 900, TTFTMillis: 200,
				AccessSource: monitor.AccessSourceLAN,
			})
		}
		if err := db.AppendRequestLogs(rows); err != nil {
			t.Fatal(err)
		}
	}

	// 5m 窗口里 10 条样本、6 条 401：ratio 0.6 ≥ 0.5 且 count ≥ 3。
	appendRows(base, 6, 4)

	// auth_invalid 要持续 3 分钟才算事故：更早的任何 tick 都不许推送。
	for offset := 0; offset <= 2; offset++ {
		if err := svc.TickAt(ctx, base.Add(time.Duration(offset)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if len(pushes) != 0 {
		t.Fatalf("fired before the sustained window: %d pushes", len(pushes))
	}
	if got := activeFiring(monitorStates(t, db)); got != 0 {
		t.Fatalf("state turned firing %d times too early", got)
	}

	if err := svc.TickAt(ctx, base.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(pushes) != 1 {
		t.Fatalf("pushes after the sustained window = %d, want 1", len(pushes))
	}
	states := monitorStates(t, db)
	if got := activeFiring(states); got != 1 {
		t.Fatalf("firing states = %d, want 1: %+v", got, states)
	}
	var alert monitoring.AlertState
	for _, state := range states {
		if state.RuleKey == "auth_invalid" && state.Status == monitoring.StatusFiring {
			alert = state
		}
	}
	if alert.ID == 0 {
		t.Fatalf("auth_invalid never reached the state table: %+v", states)
	}
	if !alert.FirstSeen.Equal(base.Add(3 * time.Minute)) {
		t.Fatalf("first seen = %s, want the tick that completed the sustained window (not an earlier pending tick)", alert.FirstSeen)
	}

	events, err := db.QueryMonitorEvents(monitoring.EventQuery{Kind: monitoring.EventKindAlert})
	if err != nil {
		t.Fatal(err)
	}
	if events.Total != 1 || events.Items[0].PushStatus != monitoring.PushSent {
		t.Fatalf("firing event not recorded as pushed: %+v", events.Items)
	}
	if events.Items[0].Severity != monitoring.SeverityP0 {
		t.Fatalf("a credential failure must be P0: %+v", events.Items[0])
	}

	// 继续供数（行会在 5 分钟后老化出窗口），并确认冷却压住了重复推送。
	appendRows(base.Add(4*time.Minute), 10, 4)
	for offset := 4; offset <= 9; offset++ {
		if err := svc.TickAt(ctx, base.Add(time.Duration(offset)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if len(pushes) != 1 {
		t.Fatalf("cooldown did not hold: %d pushes", len(pushes))
	}

	// 处置走真实端点：确认状态与 timeline 都属于当前这次事故。
	ack := monitorCall(t, server.handleMonitorAlertAck, http.MethodPost,
		fmt.Sprintf("/__monitor/alerts/%d/ack", alert.ID), "",
		map[string]string{"id": fmt.Sprint(alert.ID)}, monitorAdminID())
	if ack.Code != http.StatusOK {
		t.Fatalf("ack = %d body=%s", ack.Code, ack.Body.String())
	}
	// The detail view must carry the disposition and the latest event's push
	// state while the incident is still open. (Resolving resets disposition so a
	// re-fire starts clean; the ack itself survives in the event timeline.)
	detail := monitorCall(t, server.handleMonitorAlertByID, http.MethodGet,
		fmt.Sprintf("/__monitor/alerts/%d", alert.ID), "",
		map[string]string{"id": fmt.Sprint(alert.ID)}, monitorAdminID())
	if detail.Code != http.StatusOK {
		t.Fatalf("alert detail = %d body=%s", detail.Code, detail.Body.String())
	}
	var detailBody struct {
		Alert       monitoring.AlertView `json:"alert"`
		LatestEvent *monitoring.Event    `json:"latestEvent"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &detailBody); err != nil {
		t.Fatal(err)
	}
	if detailBody.Alert.Disposition != monitoring.DispositionAcknowledged {
		t.Fatalf("disposition lost: %+v", detailBody.Alert)
	}
	if detailBody.Alert.PushStatus != monitoring.PushSent {
		t.Fatalf("push status lost: %+v", detailBody.Alert)
	}
	timeline, _ := json.Marshal(detailBody.LatestEvent.Detail)
	if !strings.Contains(string(timeline), "acknowledged") {
		t.Fatalf("ack did not reach the incident timeline: %s", timeline)
	}

	if err := svc.TickAt(ctx, base.Add(9*time.Minute+30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(pushes) != 1 {
		t.Fatalf("an acknowledged alert re-pushed inside its cooldown: %d", len(pushes))
	}

	// 恢复：30 条成功把 5m 窗口的 401 占比稀释到阈值以下。失败行只稀释不删除，
	// 否则规则会走 data_unavailable 而不是 resolved。
	appendRows(base.Add(10*time.Minute), 0, 30)
	if err := svc.TickAt(ctx, base.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := svc.TickAt(ctx, base.Add(11*time.Minute)); err != nil {
		t.Fatal(err)
	}
	states = monitorStates(t, db)
	var final *monitoring.AlertState
	for index := range states {
		if states[index].RuleKey == "auth_invalid" {
			final = &states[index]
		}
	}
	if final == nil || final.Status != monitoring.StatusResolved {
		t.Fatalf("incident did not resolve: %+v", final)
	}
	if final.LastSeen.Sub(final.FirstSeen) < 5*time.Minute {
		t.Fatalf("recovery push needs ≥5m of duration, got %s", final.LastSeen.Sub(final.FirstSeen))
	}

	// 一次触发推送 + 一次恢复推送。
	if len(pushes) != 2 {
		t.Fatalf("pushes = %d, want 2 (firing + recovery)", len(pushes))
	}
	events, err = db.QueryMonitorEvents(monitoring.EventQuery{Kind: monitoring.EventKindAlert})
	if err != nil {
		t.Fatal(err)
	}
	if events.Total != 2 {
		t.Fatalf("alert events = %d, want firing + resolved: %+v", events.Total, events.Items)
	}
	for _, event := range events.Items {
		if event.Status == monitoring.StatusResolved && event.PushStatus != monitoring.PushSent {
			t.Fatalf("recovery was not pushed: %+v", event)
		}
	}
}

// TestStartMonitoringWithoutStore proves the wiring degrades quietly: a gateway
// whose store cannot persist monitoring state must keep running on the legacy
// alert scanner rather than half-starting the new subsystem.
func TestStartMonitoringWithoutStore(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	server.StartMonitoring(context.Background())
	if server.monitoring != nil || server.monitoringEngine != nil || server.monitoringSrc != nil {
		t.Fatal("monitoring started without a persistence store")
	}
}

// TestStartMonitoringWiresService checks the other half of the same gate: with a
// real store the service is built, one start event lands, and starting twice
// does not spawn a second loop.
func TestStartMonitoringWiresService(t *testing.T) {
	server, db := monitorTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	server.StartMonitoring(ctx)
	if server.monitoring == nil || server.monitoringEngine == nil || server.monitoringSrc == nil {
		t.Fatal("monitoring did not wire up against a capable store")
	}
	first := server.monitoring
	server.StartMonitoring(ctx)
	if server.monitoring != first {
		t.Fatal("a second start replaced the running service")
	}
	events, err := db.QueryMonitorEvents(monitoring.EventQuery{Kind: monitoring.EventKindSystem})
	if err != nil {
		t.Fatal(err)
	}
	if events.Total != 1 || events.Items[0].Title != "监控中心已启动" {
		t.Fatalf("start event = %+v", events.Items)
	}
	if got := server.MonitoringService(); got != first {
		t.Fatal("MonitoringService must hand back the running service")
	}
}

func monitorStates(t *testing.T, db *store.Store) []monitoring.AlertState {
	t.Helper()
	states, err := db.LoadMonitorStates()
	if err != nil {
		t.Fatal(err)
	}
	return states
}

func activeFiring(states []monitoring.AlertState) int {
	count := 0
	for _, state := range states {
		if state.Status == monitoring.StatusFiring {
			count++
		}
	}
	return count
}
