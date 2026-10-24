package gateway

import (
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
)

func TestMonitorEventsAndAlertsQuery(t *testing.T) {
	server, db := monitorTestServer(t, monitorClaudeProvider())
	now := time.Now().UTC()
	for index := 0; index < 3; index++ {
		at := now.Add(-time.Duration(index) * time.Hour)
		fingerprint := fmt.Sprintf("quota_low|provider:p-claude|%d", index)
		if _, err := db.InsertMonitorEvent(monitoring.Event{
			Time: at, Kind: monitoring.EventKindQuota, Severity: monitoring.SeverityP1,
			TargetType: monitoring.TargetProvider, TargetID: "p-claude", TargetName: "Claude 主号",
			Title: "额度接近用尽", Fingerprint: fingerprint, Status: monitoring.StatusFiring,
			Count: 1, FirstSeen: at, LastSeen: at,
			Detail: map[string]any{"ratio": 0.05 * float64(index+1)},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.UpsertMonitorState(monitoring.AlertState{
			Fingerprint: fingerprint, RuleKey: "quota_low", Severity: monitoring.SeverityP1,
			TargetType: monitoring.TargetProvider, TargetID: "p-claude", TargetName: "Claude 主号",
			Status: monitoring.StatusFiring, Disposition: monitoring.DispositionOpen,
			Count: 1, FirstSeen: at, LastSeen: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 窗口外的一条系统事件不得混进 24h 查询。
	if _, err := db.InsertMonitorEvent(monitoring.Event{
		Time: now.Add(-72 * time.Hour), Kind: monitoring.EventKindSystem, Severity: monitoring.SeverityInfo,
		TargetType: monitoring.TargetGateway, Title: "监控中心已启动", Status: monitoring.StatusInfo,
	}); err != nil {
		t.Fatal(err)
	}

	events := monitorCall(t, server.handleMonitorEvents, http.MethodGet, "/__monitor/events?range=24h&kind=quota", "", nil, monitorAdminID())
	if events.Code != http.StatusOK {
		t.Fatalf("events = %d body=%s", events.Code, events.Body.String())
	}
	var eventPage monitoring.EventPage
	if err := json.Unmarshal(events.Body.Bytes(), &eventPage); err != nil {
		t.Fatal(err)
	}
	if eventPage.Total != 3 || len(eventPage.Items) != 3 {
		t.Fatalf("24h quota events = %+v", eventPage)
	}
	for _, item := range eventPage.Items {
		if item.Kind != monitoring.EventKindQuota {
			t.Fatalf("kind filter leaked: %+v", item)
		}
	}

	all := monitorCall(t, server.handleMonitorEvents, http.MethodGet, "/__monitor/events?range=7d", "", nil, monitorAdminID())
	var allPage monitoring.EventPage
	if err := json.Unmarshal(all.Body.Bytes(), &allPage); err != nil {
		t.Fatal(err)
	}
	if allPage.Total != 4 {
		t.Fatalf("7d events = %d, want 4", allPage.Total)
	}

	paged := monitorCall(t, server.handleMonitorEvents, http.MethodGet, "/__monitor/events?range=7d&page=2&pageSize=2", "", nil, monitorAdminID())
	var pagedPage monitoring.EventPage
	if err := json.Unmarshal(paged.Body.Bytes(), &pagedPage); err != nil {
		t.Fatal(err)
	}
	if pagedPage.Total != 4 || len(pagedPage.Items) != 2 || pagedPage.Page != 2 {
		t.Fatalf("paged events = %+v", pagedPage)
	}

	targeted := monitorCall(t, server.handleMonitorEvents, http.MethodGet, "/__monitor/events?range=7d&targetId=p-claude", "", nil, monitorAdminID())
	var targetedPage monitoring.EventPage
	if err := json.Unmarshal(targeted.Body.Bytes(), &targetedPage); err != nil {
		t.Fatal(err)
	}
	if targetedPage.Total != 3 {
		t.Fatalf("target filter = %d, want 3", targetedPage.Total)
	}

	alerts := monitorCall(t, server.handleMonitorAlerts, http.MethodGet, "/__monitor/alerts?status=firing&severity=p1", "", nil, monitorAdminID())
	if alerts.Code != http.StatusOK {
		t.Fatalf("alerts = %d body=%s", alerts.Code, alerts.Body.String())
	}
	var alertPage monitoring.AlertPage
	if err := json.Unmarshal(alerts.Body.Bytes(), &alertPage); err != nil {
		t.Fatal(err)
	}
	if alertPage.Total != 3 {
		t.Fatalf("firing p1 alerts = %d", alertPage.Total)
	}
	if len(alertPage.Items) == 0 || alertPage.Items[0].LatestEventID == 0 ||
		len(alertPage.Items[0].RelatedEventIDs) == 0 {
		t.Fatalf("alert rows must carry their events: %+v", alertPage.Items)
	}

	filtered := monitorCall(t, server.handleMonitorAlerts, http.MethodGet, "/__monitor/alerts?status=resolved", "", nil, monitorAdminID())
	var resolvedPage monitoring.AlertPage
	if err := json.Unmarshal(filtered.Body.Bytes(), &resolvedPage); err != nil {
		t.Fatal(err)
	}
	if resolvedPage.Total != 0 {
		t.Fatalf("status filter leaked firing rows: %+v", resolvedPage.Items)
	}

	alertID := fmt.Sprint(alertPage.Items[0].ID)
	detail := monitorCall(t, server.handleMonitorAlertByID, http.MethodGet, "/__monitor/alerts/"+alertID, "",
		map[string]string{"id": alertID}, monitorAdminID())
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
	if detailBody.Alert.RuleKey != "quota_low" || detailBody.LatestEvent == nil {
		t.Fatalf("alert detail = %+v", detailBody)
	}

	badID := monitorCall(t, server.handleMonitorAlertByID, http.MethodGet, "/__monitor/alerts/not-a-number", "",
		map[string]string{"id": "not-a-number"}, monitorAdminID())
	if badID.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d, want 400", badID.Code)
	}
	missing := monitorCall(t, server.handleMonitorAlertByID, http.MethodGet, "/__monitor/alerts/9999", "",
		map[string]string{"id": "9999"}, monitorAdminID())
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing id = %d, want 404", missing.Code)
	}
}

// 处置只对 firing 告警开放；确认/忽略要落到该周期最新事件的 timeline，
// 而不是再造一条新事件（spec §8.2）。
func TestMonitorAlertAckIgnore(t *testing.T) {
	server, db := monitorTestServer(t, monitorClaudeProvider())
	now := time.Now().UTC()
	fingerprint := "auth_invalid|provider:p-claude"
	state, err := db.UpsertMonitorState(monitoring.AlertState{
		Fingerprint: fingerprint, RuleKey: "auth_invalid", Severity: monitoring.SeverityP0,
		TargetType: monitoring.TargetProvider, TargetID: "p-claude", TargetName: "Claude 主号",
		Status: monitoring.StatusFiring, Disposition: monitoring.DispositionOpen,
		Count: 1, FirstSeen: now, LastSeen: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := db.InsertMonitorEvent(monitoring.Event{
		Time: now, Kind: monitoring.EventKindAlert, Severity: monitoring.SeverityP0,
		RuleKey: "auth_invalid", TargetType: monitoring.TargetProvider, TargetID: "p-claude",
		TargetName: "Claude 主号", Title: "Claude 主号 鉴权连续失败", Fingerprint: fingerprint,
		Status: monitoring.StatusFiring, Count: 1, FirstSeen: now, LastSeen: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprint(state.ID)

	ack := monitorCall(t, server.handleMonitorAlertAck, http.MethodPost, "/__monitor/alerts/"+id+"/ack", "",
		map[string]string{"id": id}, monitorAdminID())
	if ack.Code != http.StatusOK {
		t.Fatalf("ack = %d body=%s", ack.Code, ack.Body.String())
	}
	var acked struct {
		Alert monitoring.AlertView `json:"alert"`
	}
	if err := json.Unmarshal(ack.Body.Bytes(), &acked); err != nil {
		t.Fatal(err)
	}
	if acked.Alert.Disposition != monitoring.DispositionAcknowledged || acked.Alert.AcknowledgedAt.IsZero() {
		t.Fatalf("ack not recorded: %+v", acked.Alert)
	}

	event, found, err := db.MonitorEventByID(eventID)
	if err != nil || !found {
		t.Fatalf("event %d missing: %v", eventID, err)
	}
	raw, _ := json.Marshal(event.Detail)
	if !strings.Contains(string(raw), "acknowledged") || !strings.Contains(string(raw), "admin") {
		t.Fatalf("timeline entry missing from the latest event: %s", raw)
	}

	ignore := monitorCall(t, server.handleMonitorAlertIgnore, http.MethodPost, "/__monitor/alerts/"+id+"/ignore", "",
		map[string]string{"id": id}, monitorAdminID())
	if ignore.Code != http.StatusOK {
		t.Fatalf("ignore = %d body=%s", ignore.Code, ignore.Body.String())
	}
	ignored, _, err := db.MonitorAlertByID(state.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ignored.Disposition != monitoring.DispositionIgnored {
		t.Fatalf("disposition = %q", ignored.Disposition)
	}

	// 已 resolved 的告警没有可处置的东西。
	resolved, err := db.UpsertMonitorState(monitoring.AlertState{
		Fingerprint: "auth_invalid|provider:p-glm", RuleKey: "auth_invalid", Severity: monitoring.SeverityP0,
		TargetType: monitoring.TargetProvider, TargetID: "p-glm", TargetName: "智谱",
		Status: monitoring.StatusResolved, Disposition: monitoring.DispositionOpen,
		Count: 1, FirstSeen: now, LastSeen: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	conflict := monitorCall(t, server.handleMonitorAlertAck, http.MethodPost,
		fmt.Sprintf("/__monitor/alerts/%d/ack", resolved.ID), "",
		map[string]string{"id": fmt.Sprint(resolved.ID)}, monitorAdminID())
	if conflict.Code != http.StatusConflict {
		t.Fatalf("ack on resolved = %d body=%s, want 409", conflict.Code, conflict.Body.String())
	}

	notFound := monitorCall(t, server.handleMonitorAlertAck, http.MethodPost, "/__monitor/alerts/4242/ack", "",
		map[string]string{"id": "4242"}, monitorAdminID())
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("ack unknown = %d, want 404", notFound.Code)
	}
}

// 渠道测试与手动重推只回分类与脱敏文本；事件的重推必须把结果折回事件行的
// 聚合 push_status，否则列表依旧显示旧状态。
func TestMonitorChannelTestAndRetryPush(t *testing.T) {
	var received int
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "success"})
	}))
	t.Cleanup(receiver.Close)

	server, db := monitorTestServer(t, monitorClaudeProvider())
	cfg := monitoring.NewConfig(server.monitoringStore)
	if err := cfg.SaveChannels([]monitoring.Channel{{
		ID: "ch-feishu", Type: "feishu", Name: "值班群",
		Config:      monitoring.ChannelConfig{Webhook: receiver.URL},
		MinSeverity: monitoring.SeverityP1, Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}

	tested := monitorCall(t, server.handleMonitorTestChannel, http.MethodPost, "/__monitor/channels/ch-feishu/test", "",
		map[string]string{"id": "ch-feishu"}, monitorAdminID())
	if tested.Code != http.StatusOK {
		t.Fatalf("test channel = %d body=%s", tested.Code, tested.Body.String())
	}
	var testResult map[string]any
	if err := json.Unmarshal(tested.Body.Bytes(), &testResult); err != nil {
		t.Fatal(err)
	}
	if testResult["ok"] != true || received != 1 {
		t.Fatalf("test push did not reach the webhook: %v received=%d", testResult, received)
	}

	missing := monitorCall(t, server.handleMonitorTestChannel, http.MethodPost, "/__monitor/channels/ch-nope/test", "",
		map[string]string{"id": "ch-nope"}, monitorAdminID())
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown channel test = %d, want 404", missing.Code)
	}

	now := time.Now().UTC()
	fingerprint := "auth_invalid|provider:p-claude"
	if _, err := db.UpsertMonitorState(monitoring.AlertState{
		Fingerprint: fingerprint, RuleKey: "auth_invalid", Severity: monitoring.SeverityP0,
		TargetType: monitoring.TargetProvider, TargetID: "p-claude", TargetName: "Claude 主号",
		Status: monitoring.StatusFiring, Disposition: monitoring.DispositionOpen,
		Count: 1, FirstSeen: now, LastSeen: now,
	}); err != nil {
		t.Fatal(err)
	}
	eventID, err := db.InsertMonitorEvent(monitoring.Event{
		Time: now, Kind: monitoring.EventKindAlert, Severity: monitoring.SeverityP0,
		RuleKey: "auth_invalid", TargetType: monitoring.TargetProvider, TargetID: "p-claude",
		TargetName: "Claude 主号", Title: "Claude 主号 鉴权连续失败", Fingerprint: fingerprint,
		Status: monitoring.StatusFiring, Count: 1, FirstSeen: now, LastSeen: now,
		PushStatus: monitoring.PushFailed, PushError: "feishu HTTP 500",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/__monitor/events/%d/retry-push", eventID)
	retried := monitorCall(t, server.handleMonitorRetryPush, http.MethodPost, path, `{"channelId":"ch-feishu"}`,
		map[string]string{"eventId": fmt.Sprint(eventID)}, monitorAdminID())
	if retried.Code != http.StatusOK {
		t.Fatalf("retry = %d body=%s", retried.Code, retried.Body.String())
	}
	var retryResult map[string]any
	if err := json.Unmarshal(retried.Body.Bytes(), &retryResult); err != nil {
		t.Fatal(err)
	}
	if retryResult["ok"] != true || retryResult["pushStatus"] != monitoring.PushSent {
		t.Fatalf("retry result = %v", retryResult)
	}
	if received != 2 {
		t.Fatalf("webhook calls = %d, want 2", received)
	}
	event, _, err := db.MonitorEventByID(eventID)
	if err != nil {
		t.Fatal(err)
	}
	if event.PushStatus != monitoring.PushSent || event.PushError != "" {
		t.Fatalf("aggregate status not folded back: %+v", event)
	}

	noEvent := monitorCall(t, server.handleMonitorRetryPush, http.MethodPost, "/__monitor/events/9999/retry-push",
		`{"channelId":"ch-feishu"}`, map[string]string{"eventId": "9999"}, monitorAdminID())
	if noEvent.Code != http.StatusNotFound {
		t.Fatalf("retry unknown event = %d, want 404", noEvent.Code)
	}
	noChannel := monitorCall(t, server.handleMonitorRetryPush, http.MethodPost, path, `{"channelId":"ch-nope"}`,
		map[string]string{"eventId": fmt.Sprint(eventID)}, monitorAdminID())
	if noChannel.Code != http.StatusNotFound {
		t.Fatalf("retry unknown channel = %d, want 404", noChannel.Code)
	}
	badID := monitorCall(t, server.handleMonitorRetryPush, http.MethodPost, "/__monitor/events/x/retry-push",
		`{"channelId":"ch-feishu"}`, map[string]string{"eventId": "x"}, monitorAdminID())
	if badID.Code != http.StatusBadRequest {
		t.Fatalf("bad event id = %d, want 400", badID.Code)
	}
}

func TestMonitorDispatchGetPut(t *testing.T) {
	server, _ := monitorTestServer(t, monitorClaudeProvider())

	got := monitorCall(t, server.handleMonitorDispatch, http.MethodGet, "/__monitor/dispatch", "", nil, monitorAdminID())
	if got.Code != http.StatusOK {
		t.Fatalf("dispatch get = %d body=%s", got.Code, got.Body.String())
	}
	var defaults monitoring.DispatchConfig
	if err := json.Unmarshal(got.Body.Bytes(), &defaults); err != nil {
		t.Fatal(err)
	}
	if defaults.DigestMinutes != 15 || defaults.Timezone != "Asia/Shanghai" || !defaults.QuietEnabled {
		t.Fatalf("dispatch defaults = %+v", defaults)
	}

	put := monitorCall(t, server.handleMonitorUpdateDispatch, http.MethodPut, "/__monitor/dispatch",
		`{"digestMinutes":30,"quietEnabled":true,"quietStart":"22:30","quietEnd":"07:00","recoveryNotify":false,"timezone":"UTC","baseUrl":"https://gw.example.com/"}`,
		nil, monitorAdminID())
	if put.Code != http.StatusOK {
		t.Fatalf("dispatch put = %d body=%s", put.Code, put.Body.String())
	}
	var updated monitoring.DispatchConfig
	if err := json.Unmarshal(put.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.DigestMinutes != 30 || updated.QuietStart != "22:30" || updated.QuietEnd != "07:00" ||
		updated.RecoveryNotify || updated.Timezone != "UTC" || updated.BaseURL != "https://gw.example.com" {
		t.Fatalf("dispatch after put = %+v", updated)
	}
	// 局部 PATCH 不能把没提交的字段抹回默认值。
	partial := monitorCall(t, server.handleMonitorUpdateDispatch, http.MethodPut, "/__monitor/dispatch",
		`{"digestMinutes":45}`, nil, monitorAdminID())
	var afterPartial monitoring.DispatchConfig
	if err := json.Unmarshal(partial.Body.Bytes(), &afterPartial); err != nil {
		t.Fatal(err)
	}
	if afterPartial.DigestMinutes != 45 || afterPartial.QuietStart != "22:30" || afterPartial.Timezone != "UTC" {
		t.Fatalf("partial patch reset untouched fields: %+v", afterPartial)
	}

	badZone := monitorCall(t, server.handleMonitorUpdateDispatch, http.MethodPut, "/__monitor/dispatch",
		`{"timezone":"Mars/Olympus"}`, nil, monitorAdminID())
	if badZone.Code != http.StatusBadRequest {
		t.Fatalf("bad timezone = %d, want 400", badZone.Code)
	}
	badClock := monitorCall(t, server.handleMonitorUpdateDispatch, http.MethodPut, "/__monitor/dispatch",
		`{"quietStart":"25:00"}`, nil, monitorAdminID())
	if badClock.Code != http.StatusBadRequest {
		t.Fatalf("bad quiet clock = %d, want 400", badClock.Code)
	}
	badWindow := monitorCall(t, server.handleMonitorUpdateDispatch, http.MethodPut, "/__monitor/dispatch",
		`{"digestMinutes":0}`, nil, monitorAdminID())
	if badWindow.Code != http.StatusBadRequest {
		t.Fatalf("zero digest window = %d, want 400", badWindow.Code)
	}
}

// P1 摘要间隔只接受 5–120 分钟：越界一律 400，并且库里保持原值不落库。
func TestMonitorDispatchDigestMinutesRange(t *testing.T) {
	cases := []struct {
		window int
		want   int
	}{
		{window: 4, want: http.StatusBadRequest},
		{window: 0, want: http.StatusBadRequest},
		{window: -5, want: http.StatusBadRequest},
		{window: 121, want: http.StatusBadRequest},
		{window: 1440, want: http.StatusBadRequest},
		{window: 5, want: http.StatusOK},
		{window: 120, want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d", tc.window), func(t *testing.T) {
			server, db := monitorTestServer(t, monitorClaudeProvider())
			cfg := monitoring.NewConfig(db)
			before := db.Setting("monitor_dispatch")

			rec := monitorCall(t, server.handleMonitorUpdateDispatch, http.MethodPut, "/__monitor/dispatch",
				fmt.Sprintf(`{"digestMinutes":%d}`, tc.window), nil, monitorAdminID())
			if rec.Code != tc.want {
				t.Fatalf("digestMinutes=%d -> %d body=%s", tc.window, rec.Code, rec.Body.String())
			}
			after := db.Setting("monitor_dispatch")
			if tc.want == http.StatusBadRequest {
				if after != before {
					t.Fatalf("rejected value was persisted: before=%q after=%q", before, after)
				}
				return
			}
			if cfg.Dispatch().DigestMinutes != tc.window {
				t.Fatalf("accepted value not stored: %+v", cfg.Dispatch())
			}
		})
	}
}

// 监控端点全部管理员专属：普通用户 403、无身份 401，并且每条路由都真的注册在
// mux 上（漏注册会 404）。
func TestMonitorEndpointsRequireAdmin(t *testing.T) {
	server, _ := monitorTestServer(t, monitorClaudeProvider())

	cases := []struct {
		method, target string
		values         map[string]string
		body           string
		handler        http.HandlerFunc
	}{
		{http.MethodGet, "/__monitor/overview", nil, "", server.handleMonitorOverview},
		{http.MethodGet, "/__monitor/accounts", nil, "", server.handleMonitorAccounts},
		{http.MethodGet, "/__monitor/accounts/p-claude", map[string]string{"providerId": "p-claude"}, "", server.handleMonitorAccount},
		{http.MethodPost, "/__monitor/accounts/p-claude/refresh", map[string]string{"providerId": "p-claude"}, "", server.handleMonitorAccountRefresh},
		{http.MethodPost, "/__monitor/accounts/p-claude/quota/refresh", map[string]string{"providerId": "p-claude"}, "", server.handleMonitorQuotaRefresh},
		{http.MethodGet, "/__monitor/events", nil, "", server.handleMonitorEvents},
		{http.MethodGet, "/__monitor/alerts", nil, "", server.handleMonitorAlerts},
		{http.MethodGet, "/__monitor/alerts/1", map[string]string{"id": "1"}, "", server.handleMonitorAlertByID},
		{http.MethodPost, "/__monitor/alerts/1/ack", map[string]string{"id": "1"}, "", server.handleMonitorAlertAck},
		{http.MethodPost, "/__monitor/alerts/1/ignore", map[string]string{"id": "1"}, "", server.handleMonitorAlertIgnore},
		{http.MethodPost, "/__monitor/events/1/retry-push", map[string]string{"eventId": "1"}, `{"channelId":"ch"}`, server.handleMonitorRetryPush},
		{http.MethodGet, "/__monitor/channels", nil, "", server.handleMonitorChannels},
		{http.MethodPost, "/__monitor/channels", nil, `{"type":"feishu"}`, server.handleMonitorCreateChannel},
		{http.MethodPut, "/__monitor/channels/ch-1", map[string]string{"id": "ch-1"}, `{"name":"x"}`, server.handleMonitorUpdateChannel},
		{http.MethodDelete, "/__monitor/channels/ch-1", map[string]string{"id": "ch-1"}, "", server.handleMonitorDeleteChannel},
		{http.MethodPost, "/__monitor/channels/ch-1/test", map[string]string{"id": "ch-1"}, "", server.handleMonitorTestChannel},
		{http.MethodGet, "/__monitor/dispatch", nil, "", server.handleMonitorDispatch},
		{http.MethodPut, "/__monitor/dispatch", nil, `{"digestMinutes":15}`, server.handleMonitorUpdateDispatch},
	}

	for _, tc := range cases {
		forbidden := monitorCall(t, tc.handler, tc.method, tc.target, tc.body, tc.values, monitorUserID())
		if forbidden.Code != http.StatusForbidden {
			t.Fatalf("%s %s as user = %d body=%s, want 403", tc.method, tc.target, forbidden.Code, forbidden.Body.String())
		}
		// 完全没有身份：handler 自己挡在 401。
		naked := httptest.NewRequest(tc.method, "http://127.0.0.1:18093"+tc.target, strings.NewReader(tc.body))
		for key, value := range tc.values {
			naked.SetPathValue(key, value)
		}
		recorder := httptest.NewRecorder()
		tc.handler(recorder, naked)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s anonymous = %d body=%s, want 401", tc.method, tc.target, recorder.Code, recorder.Body.String())
		}
	}

	// 走完整的 Handler() 链：本机回环即管理员，逐条 GET 必须 200（不是 404），
	// 这同时证明路由注册与鉴权放行。
	server.SetAdminAuthStore(newMemoryAdminAuthStore())
	for _, target := range []string{
		"/__monitor/overview", "/__monitor/accounts", "/__monitor/accounts/p-claude",
		"/__monitor/events", "/__monitor/alerts", "/__monitor/channels", "/__monitor/dispatch",
	} {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18093"+target, nil)
		request.RemoteAddr = "127.0.0.1:55555"
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s as local admin = %d body=%s, want 200", target, recorder.Code, recorder.Body.String())
		}
	}

	// 同一批路由经公网地址、无会话：由鉴权层挡在 401，绝不进 handler。
	for _, target := range []string{"/__monitor/overview", "/__monitor/channels", "/__monitor/dispatch"} {
		request := httptest.NewRequest(http.MethodGet, "https://gw.example.com"+target, nil)
		request.RemoteAddr = "203.0.113.7:4444"
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s anonymous over LAN = %d, want 401", target, recorder.Code)
		}
	}
}

// 监控未启用（没有可用持久层）时接口回 503，而不是 panic。
func TestMonitorEndpointsWithoutStoreAreUnavailable(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{Providers: []domain.Provider{monitorClaudeProvider()}}), monitor.NewStore())
	if server.monitoringStore != nil {
		t.Fatal("server without a store should not report a monitoring store")
	}
	calls := []struct {
		target  string
		handler http.HandlerFunc
	}{
		{"/__monitor/overview", server.handleMonitorOverview},
		{"/__monitor/accounts", server.handleMonitorAccounts},
		{"/__monitor/events", server.handleMonitorEvents},
		{"/__monitor/alerts", server.handleMonitorAlerts},
		{"/__monitor/channels", server.handleMonitorChannels},
		{"/__monitor/dispatch", server.handleMonitorDispatch},
	}
	for _, call := range calls {
		recorder := monitorCall(t, call.handler, http.MethodGet, call.target, "", nil, monitorAdminID())
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET %s = %d body=%s, want 503", call.target, recorder.Code, recorder.Body.String())
		}
	}
}
