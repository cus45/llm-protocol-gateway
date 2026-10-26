package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"github.com/luca/llm-protocol-gateway/internal/monitoring"
	"github.com/luca/llm-protocol-gateway/internal/store"
)

// monitorTestServer wires a real SQLite store into a Server, which is what makes
// the monitoringStore assertion in NewServer succeed.
func monitorTestServer(t *testing.T, providers ...domain.Provider) (*Server, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(NewRouter(domain.GatewayState{Providers: providers}), monitor.NewStore(), db)
	if server.monitoringStore == nil {
		t.Fatal("store does not satisfy monitoringStore; the console would never start")
	}
	server.monitoringSrc = newMonitoringSource(server)
	return server, db
}

// monitorCall invokes one handler the way ServeMux would: path values by hand
// and the identity withAdminAuth injects. Going through Handler() for this is
// impossible because that wrapper overwrites the context identity;
// TestMonitorEndpointsRequireAdmin exercises the real chain separately.
func monitorCall(t *testing.T, handler http.HandlerFunc, method, target, body string,
	values map[string]string, identity sessionIdentity) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	for key, value := range values {
		request.SetPathValue(key, value)
	}
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, identity))
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	return recorder
}

func monitorAdminID() sessionIdentity { return adminIdentity() }

func monitorUserID() sessionIdentity {
	return sessionIdentity{UserID: "u1", Username: "staff", Role: domain.UserRoleUser}
}

func monitorClaudeProvider() domain.Provider {
	return domain.Provider{
		ID: "p-claude", Name: "Claude 主号", Protocol: domain.ProtocolClaude,
		AuthType: domain.AuthTypeClaudeOAuth,
		BaseURL:  "https://api.anthropic.com",
		ClaudeOAuth: &domain.ClaudeOAuthCredential{
			AccessToken: "sk-ant-oat01-ACCESS", RefreshToken: "rt-SECRET-REFRESH",
			ExpiresAt: time.Now().Add(20 * time.Hour).UTC().Format(time.RFC3339),
		},
	}
}

func monitorAPIKeyProvider() domain.Provider {
	return domain.Provider{
		ID: "p-glm", Name: "智谱", Protocol: domain.ProtocolOpenAIChat,
		AuthType: domain.AuthTypeAPIKey, BaseURL: "https://open.bigmodel.cn/api/paas/v4",
	}
}

// 渠道的 webhook/secret/botToken 在任何一次响应里都只能是 *Set 布尔值：GET、
// POST、PUT 共用同一个脱敏结构，避免“某条代码路径忘了遮”。
func TestMonitorChannelsCRUDHidesSecrets(t *testing.T) {
	server, _ := monitorTestServer(t, monitorClaudeProvider())

	const webhook = "https://open.feishu.cn/open-apis/bot/v2/hook/FEISHU-TOKEN-SECRET"
	const signSecret = "FEISHU-SIGN-SECRET"
	created := monitorCall(t, server.handleMonitorCreateChannel, http.MethodPost, "/__monitor/channels",
		fmt.Sprintf(`{"type":"feishu","name":"值班群","minSeverity":"p0","webhook":%q,"secret":%q}`, webhook, signSecret),
		nil, monitorAdminID())
	if created.Code != http.StatusCreated {
		t.Fatalf("create channel = %d body=%s", created.Code, created.Body.String())
	}
	var view monitorChannelView
	if err := json.Unmarshal(created.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.WebhookSet || !view.SecretSet || view.MinSeverity != "p0" || !view.Enabled {
		t.Fatalf("created view = %+v", view)
	}
	assertNoSecrets(t, created.Body.String())

	// 空 secret 表示“保留原值”，不能让控制台的空密码框把可用配置抹掉。
	updated := monitorCall(t, server.handleMonitorUpdateChannel, http.MethodPut, "/__monitor/channels/"+view.ID,
		`{"name":"值班群 2","webhook":"","secret":"","botToken":""}`,
		map[string]string{"id": view.ID}, monitorAdminID())
	if updated.Code != http.StatusOK {
		t.Fatalf("update channel = %d body=%s", updated.Code, updated.Body.String())
	}
	assertNoSecrets(t, updated.Body.String())

	listed := monitorCall(t, server.handleMonitorChannels, http.MethodGet, "/__monitor/channels", "", nil, monitorAdminID())
	if listed.Code != http.StatusOK {
		t.Fatalf("list = %d", listed.Code)
	}
	assertNoSecrets(t, listed.Body.String())
	stored := monitoring.NewConfig(server.monitoringStore).Channels()
	if len(stored) != 1 || stored[0].Config.Webhook != webhook || stored[0].Config.Secret != signSecret {
		t.Fatalf("stored channels lost the secret: %+v", stored)
	}
	if stored[0].Name != "值班群 2" {
		t.Fatalf("name not updated: %+v", stored[0])
	}

	// Telegram 保留可见的 chatId，bot token 一律遮掉。
	telegram := monitorCall(t, server.handleMonitorCreateChannel, http.MethodPost, "/__monitor/channels",
		`{"type":"telegram","name":"TG","botToken":"123:TELEGRAM-BOT-SECRET","chatId":"42"}`, nil, monitorAdminID())
	if telegram.Code != http.StatusCreated {
		t.Fatalf("create telegram = %d body=%s", telegram.Code, telegram.Body.String())
	}
	assertNoSecrets(t, telegram.Body.String())
	if !strings.Contains(telegram.Body.String(), `"42"`) {
		t.Fatalf("chat id should stay visible: %s", telegram.Body.String())
	}

	// 没有适配器的类型不能建：否则通知会被静默丢弃。
	unknown := monitorCall(t, server.handleMonitorCreateChannel, http.MethodPost, "/__monitor/channels",
		`{"type":"sms","name":"短信"}`, nil, monitorAdminID())
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown type = %d, want 400", unknown.Code)
	}
	missing := monitorCall(t, server.handleMonitorUpdateChannel, http.MethodPut, "/__monitor/channels/ch-nope",
		`{"name":"x"}`, map[string]string{"id": "ch-nope"}, monitorAdminID())
	if missing.Code != http.StatusNotFound {
		t.Fatalf("update missing = %d, want 404", missing.Code)
	}

	deleted := monitorCall(t, server.handleMonitorDeleteChannel, http.MethodDelete, "/__monitor/channels/"+view.ID, "",
		map[string]string{"id": view.ID}, monitorAdminID())
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete = %d body=%s", deleted.Code, deleted.Body.String())
	}
	if remaining := monitoring.NewConfig(server.monitoringStore).Channels(); len(remaining) != 1 || remaining[0].Type != "telegram" {
		t.Fatalf("channels after delete = %+v", remaining)
	}
}

// overview 与 accounts 都是只读的：不得为了“看起来更新”而轮换 refresh token
// （并发的轮换会让后到的那枚被判无效），也不得消耗订阅额度。
func TestMonitorOverviewAndAccountsReadOnly(t *testing.T) {
	server, db := monitorTestServer(t, monitorClaudeProvider(), monitorAPIKeyProvider())

	ratio := 0.25
	if err := db.UpsertProviderMetricSample(monitoring.MetricSample{
		ProviderID:    "p-claude",
		QuotaRatio:    &ratio,
		ObservedAt:    time.Now().Add(-time.Hour).UTC(),
		AuthExpiresAt: time.Now().Add(20 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
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
	if _, err := db.InsertMonitorEvent(monitoring.Event{
		Time: now, Kind: monitoring.EventKindAlert, Severity: monitoring.SeverityP0,
		RuleKey: "auth_invalid", TargetType: monitoring.TargetProvider, TargetID: "p-claude",
		TargetName: "Claude 主号", Title: "Claude 主号 鉴权连续失败", Fingerprint: fingerprint,
		Status: monitoring.StatusFiring, Count: 1, FirstSeen: now, LastSeen: now,
	}); err != nil {
		t.Fatal(err)
	}

	overview := monitorCall(t, server.handleMonitorOverview, http.MethodGet, "/__monitor/overview", "", nil, monitorAdminID())
	if overview.Code != http.StatusOK {
		t.Fatalf("overview = %d body=%s", overview.Code, overview.Body.String())
	}
	var summary monitorOverviewView
	if err := json.Unmarshal(overview.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Accounts.Total != 2 || summary.Accounts.Subscribed != 1 || summary.Accounts.Disabled != 0 {
		t.Fatalf("account counts = %+v", summary.Accounts)
	}
	if summary.Firing.Total != 1 || summary.Firing.P0 != 1 {
		t.Fatalf("firing counts = %+v", summary.Firing)
	}
	if len(summary.RecentEvents) != 1 {
		t.Fatalf("recent events = %+v", summary.RecentEvents)
	}
	assertNoSecrets(t, overview.Body.String())

	accounts := monitorCall(t, server.handleMonitorAccounts, http.MethodGet, "/__monitor/accounts", "", nil, monitorAdminID())
	if accounts.Code != http.StatusOK {
		t.Fatalf("accounts = %d", accounts.Code)
	}
	var accountPage struct {
		Accounts []monitorAccountView `json:"accounts"`
		Total    int                  `json:"total"`
	}
	if err := json.Unmarshal(accounts.Body.Bytes(), &accountPage); err != nil {
		t.Fatal(err)
	}
	if accountPage.Total != 2 {
		t.Fatalf("accounts total = %d", accountPage.Total)
	}
	var claude, glm *monitorAccountView
	for index := range accountPage.Accounts {
		switch accountPage.Accounts[index].ProviderID {
		case "p-claude":
			claude = &accountPage.Accounts[index]
		case "p-glm":
			glm = &accountPage.Accounts[index]
		}
	}
	if claude == nil || glm == nil {
		t.Fatalf("accounts missing an entry: %+v", accountPage.Accounts)
	}
	if claude.Vendor != "claude" || !claude.Capabilities.OAuth || !claude.Capabilities.Quota {
		t.Fatalf("claude account = %+v", claude)
	}
	if claude.ExpiresAt == "" {
		t.Fatalf("claude account lost its expiry: %+v", claude)
	}
	if claude.Quota == nil || claude.Quota.Ratio != 0.25 || !claude.Quota.Stale {
		t.Fatalf("claude quota should come from the last sample and be stale: %+v", claude.Quota)
	}
	if claude.ActiveAlerts != 1 {
		t.Fatalf("claude active alerts = %d", claude.ActiveAlerts)
	}
	// 纯 API Key 厂商没有额度可采：unknown 必须保持缺席，而不是显示成 0。
	if glm.Vendor != "zhipu" || glm.Quota != nil || glm.Capabilities.OAuth || glm.ExpiresAt != "" {
		t.Fatalf("api-key account invented data: %+v", glm)
	}

	single := monitorCall(t, server.handleMonitorAccount, http.MethodGet, "/__monitor/accounts/p-claude", "",
		map[string]string{"providerId": "p-claude"}, monitorAdminID())
	if single.Code != http.StatusOK {
		t.Fatalf("single account = %d body=%s", single.Code, single.Body.String())
	}
	assertNoSecrets(t, single.Body.String())
	notFound := monitorCall(t, server.handleMonitorAccount, http.MethodGet, "/__monitor/accounts/p-nope", "",
		map[string]string{"providerId": "p-nope"}, monitorAdminID())
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("unknown account = %d, want 404", notFound.Code)
	}

	// 读取不得轮换凭证，也不得写续期事件。
	if got := server.router.State().Providers[0].ClaudeOAuth.AccessToken; got != "sk-ant-oat01-ACCESS" {
		t.Fatalf("read-only endpoints rotated the access token: %q", got)
	}
	renewals, err := db.QueryMonitorEvents(monitoring.EventQuery{Kind: monitoring.EventKindRenewal})
	if err != nil {
		t.Fatal(err)
	}
	if renewals.Total != 0 {
		t.Fatalf("read-only endpoints wrote %d renewal events", renewals.Total)
	}
}

func TestMonitorAccountRefreshAndQuotaRefresh(t *testing.T) {
	server, db := monitorTestServer(t, monitorClaudeProvider(), monitorAPIKeyProvider())

	// 普通 API Key 没有可续期的登录凭证。
	notOAuth := monitorCall(t, server.handleMonitorAccountRefresh, http.MethodPost, "/__monitor/accounts/p-glm/refresh", "",
		map[string]string{"providerId": "p-glm"}, monitorAdminID())
	if notOAuth.Code != http.StatusBadRequest {
		t.Fatalf("api-key refresh = %d, want 400", notOAuth.Code)
	}
	unknown := monitorCall(t, server.handleMonitorAccountRefresh, http.MethodPost, "/__monitor/accounts/p-nope/refresh", "",
		map[string]string{"providerId": "p-nope"}, monitorAdminID())
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown provider refresh = %d, want 404", unknown.Code)
	}

	// Claude 存的到期时间是 refresh token 的寿命，所以控制台显式点“续期”必须真的
	// 强制轮换；这里没存 refresh token，失败只能以分类返回，不能带上游原文。
	forced := monitorCall(t, server.handleMonitorAccountRefresh, http.MethodPost, "/__monitor/accounts/p-claude/refresh", "",
		map[string]string{"providerId": "p-claude"}, monitorAdminID())
	if forced.Code != http.StatusOK {
		t.Fatalf("claude refresh = %d body=%s", forced.Code, forced.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(forced.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["ok"] != false || result["renewed"] != false {
		t.Fatalf("a credential-less refresh must not report success: %v", result)
	}
	if kind, _ := result["errorKind"].(string); kind == "" {
		t.Fatalf("failure must carry a classification: %v", result)
	}
	assertNoSecrets(t, forced.Body.String())

	// 额度：API Key 厂商没有用量接口，要如实拒绝，而不是编一个数。
	noQuota := monitorCall(t, server.handleMonitorQuotaRefresh, http.MethodPost, "/__monitor/accounts/p-glm/quota/refresh", "",
		map[string]string{"providerId": "p-glm"}, monitorAdminID())
	if noQuota.Code != http.StatusBadRequest {
		t.Fatalf("api-key quota refresh = %d, want 400", noQuota.Code)
	}

	ratio := 0.6
	server.monitoringSrc.quotaFetch = func(ctx context.Context, p domain.Provider, at time.Time) (*monitoring.QuotaSnapshot, error) {
		return &monitoring.QuotaSnapshot{
			Source: "claude", Ratio: ratio, Limit: 100, Remaining: 40, ObservedAt: at.UTC(),
			Windows: []monitoring.QuotaWindow{{Key: "five_hour", UsedPercent: 12, ResetsAt: at.Add(90 * time.Minute).UTC()}},
		}, nil
	}
	fresh := monitorCall(t, server.handleMonitorQuotaRefresh, http.MethodPost, "/__monitor/accounts/p-claude/quota/refresh", "",
		map[string]string{"providerId": "p-claude"}, monitorAdminID())
	if fresh.Code != http.StatusOK {
		t.Fatalf("quota refresh = %d body=%s", fresh.Code, fresh.Body.String())
	}
	var quotaReply struct {
		OK    bool                     `json:"ok"`
		Quota monitoring.QuotaSnapshot `json:"quota"`
	}
	if err := json.Unmarshal(fresh.Body.Bytes(), &quotaReply); err != nil {
		t.Fatal(err)
	}
	if !quotaReply.OK || quotaReply.Quota.Ratio != ratio || quotaReply.Quota.Stale {
		t.Fatalf("quota reply = %+v", quotaReply)
	}
	if len(quotaReply.Quota.Windows) != 1 || quotaReply.Quota.Windows[0].Key != "five_hour" {
		t.Fatalf("per-window usage must reach the console: %+v", quotaReply.Quota.Windows)
	}
	// 强制读到的数必须落进采样，否则下一次 accounts 仍然显示旧的那小时。
	samples, err := db.LatestProviderMetricSamples()
	if err != nil {
		t.Fatal(err)
	}
	sample, ok := samples["p-claude"]
	if !ok || sample.QuotaRatio == nil || *sample.QuotaRatio != ratio {
		t.Fatalf("refresh did not persist the sample: %+v", samples)
	}
	if len(sample.QuotaWindows) != 1 || sample.QuotaWindows[0].Key != "five_hour" {
		t.Fatalf("refresh did not persist the windows: %+v", sample.QuotaWindows)
	}

	// 上游拒绝时是 502 + 分类，绝不把带 token 的原文透出去。
	server.monitoringSrc.quotaFetch = func(ctx context.Context, p domain.Provider, at time.Time) (*monitoring.QuotaSnapshot, error) {
		return nil, fmt.Errorf("usage fetch failed: 401 unauthorized oauth_token=sk-ant-oat01-SECRET")
	}
	failed := monitorCall(t, server.handleMonitorQuotaRefresh, http.MethodPost, "/__monitor/accounts/p-claude/quota/refresh", "",
		map[string]string{"providerId": "p-claude"}, monitorAdminID())
	if failed.Code != http.StatusBadGateway {
		t.Fatalf("failed quota refresh = %d body=%s, want 502", failed.Code, failed.Body.String())
	}
	var failure map[string]any
	if err := json.Unmarshal(failed.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if failure["ok"] != false || failure["errorKind"] != "auth_failed" {
		t.Fatalf("failure reply = %v", failure)
	}
	assertNoSecrets(t, failed.Body.String())

	// 没有数据源时回 503，而不是 panic。
	server.monitoringSrc = nil
	unavailable := monitorCall(t, server.handleMonitorQuotaRefresh, http.MethodPost, "/__monitor/accounts/p-claude/quota/refresh", "",
		map[string]string{"providerId": "p-claude"}, monitorAdminID())
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("quota refresh without source = %d, want 503", unavailable.Code)
	}
}

// 保活发成功以后必须顺手把额度读一遍并落库：否则「触发成功但面板还是旧数」
// 只能等下一轮 15 分钟的采样，用户就会以为保活没生效。
func TestKeepaliveProbeRefreshesQuotaSample(t *testing.T) {
	server, db := monitorTestServer(t, monitorClaudeProvider())
	observed := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	server.monitoringSrc.quotaFetch = func(ctx context.Context, p domain.Provider, at time.Time) (*monitoring.QuotaSnapshot, error) {
		return &monitoring.QuotaSnapshot{
			Source: "claude", Ratio: 0.88, Limit: 100, Remaining: 88, ObservedAt: observed,
			Windows: []monitoring.QuotaWindow{
				{Key: "five_hour", UsedPercent: 12, ResetsAt: observed.Add(4 * time.Hour)},
				{Key: "seven_day", UsedPercent: 51},
			},
		}, nil
	}
	provider, err := server.router.ProviderByID("p-claude")
	if err != nil {
		t.Fatal(err)
	}
	server.refreshQuotaAfterProbe(context.Background(), provider)

	samples, err := db.LatestProviderMetricSamples()
	if err != nil {
		t.Fatal(err)
	}
	sample, ok := samples["p-claude"]
	if !ok || sample.QuotaRatio == nil || *sample.QuotaRatio != 0.88 {
		t.Fatalf("keepalive did not persist a fresh sample: %+v", samples)
	}
	// 采样时间必须是本次读取的时刻，不是 tick 的时钟。
	if !sample.ObservedAt.Equal(observed) {
		t.Fatalf("sample observedAt = %s, want the read time %s", sample.ObservedAt, observed)
	}
	if len(sample.QuotaWindows) != 2 || sample.QuotaWindows[0].Key != "five_hour" {
		t.Fatalf("windows lost on the way to the sample: %+v", sample.QuotaWindows)
	}

	// 读取失败不能把上一次的窗口洗掉，也不能让保活本身失败。
	server.monitoringSrc.quotaFetch = func(ctx context.Context, p domain.Provider, at time.Time) (*monitoring.QuotaSnapshot, error) {
		return nil, fmt.Errorf("401 unauthorized")
	}
	server.refreshQuotaAfterProbe(context.Background(), provider)
	after, err := db.LatestProviderMetricSamples()
	if err != nil {
		t.Fatal(err)
	}
	if got := after["p-claude"]; len(got.QuotaWindows) != 2 {
		t.Fatalf("a failed read wiped the last good windows: %+v", got)
	}
}

// assertNoSecrets 是每个监控响应的安全回归闸门：凭证、webhook token、签名密钥
// 一律不得被序列化出去。
func assertNoSecrets(t *testing.T, payload string) {
	t.Helper()
	for _, forbidden := range []string{
		"sk-ant", "FEISHU-TOKEN-SECRET", "FEISHU-SIGN-SECRET", "TELEGRAM-BOT-SECRET",
		"rt-SECRET-REFRESH", "oauth_token", "accessToken", "refreshToken", "botToken",
	} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, payload)
		}
	}
}

// ── 规则接口 ────────────────────────────────────────────────────────────

func monitorRulesGet(t *testing.T, server *Server, identity sessionIdentity) (int, []monitorRuleView, string) {
	t.Helper()
	recorder := monitorCall(t, server.handleMonitorRules, http.MethodGet, "/__monitor/rules", "", nil, identity)
	var views []monitorRuleView
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &views); err != nil {
			t.Fatalf("decode rules: %v body=%s", err, recorder.Body.String())
		}
	}
	return recorder.Code, views, recorder.Body.String()
}

func monitorRulesPut(t *testing.T, server *Server, body string, identity sessionIdentity) *httptest.ResponseRecorder {
	t.Helper()
	return monitorCall(t, server.handleMonitorUpdateRules, http.MethodPut, "/__monitor/rules", body, nil, identity)
}

func monitorRulesByKey(views []monitorRuleView) map[string]monitorRuleView {
	out := make(map[string]monitorRuleView, len(views))
	for _, view := range views {
		out[view.Key] = view
	}
	return out
}

// GET 返回全部 18 条定义，字段齐全，且 intervalMinutes 是正整数（纳秒时长不得
// 出现在线上）。
func TestMonitorRulesGetComplete(t *testing.T) {
	server, _ := monitorTestServer(t)

	code, views, raw := monitorRulesGet(t, server, monitorAdminID())
	if code != http.StatusOK {
		t.Fatalf("GET rules = %d body=%s", code, raw)
	}
	if len(views) != 18 {
		t.Fatalf("rules = %d, want 18", len(views))
	}
	defs := monitoring.DefaultRules()
	wantOrder := monitoring.SortedRuleKeys(defs)
	for i, key := range wantOrder {
		if views[i].Key != key {
			t.Fatalf("rules[%d] = %q, want %q (SortedRuleKeys order)", i, views[i].Key, key)
		}
	}
	defaults := make(map[string]monitoring.RuleConfig, len(defs))
	for _, rule := range defs {
		defaults[rule.Key] = rule
	}
	for _, view := range views {
		def, ok := monitoring.RuleDefinition(view.Key)
		if !ok {
			t.Fatalf("rule %q has no definition", view.Key)
		}
		if view.Name == "" || view.TargetType == "" {
			t.Fatalf("rule %s missing name/targetType: %+v", view.Key, view)
		}
		if view.Severity == "" {
			t.Fatalf("rule %s missing severity", view.Key)
		}
		if view.IntervalMinutes <= 0 {
			t.Fatalf("rule %s intervalMinutes = %d, want > 0", view.Key, view.IntervalMinutes)
		}
		if view.IntervalMinutes != int(def.Interval/time.Minute) {
			t.Fatalf("rule %s intervalMinutes %d does not match Interval %v", view.Key, view.IntervalMinutes, def.Interval)
		}
		if len(view.DefaultParams) != len(def.DefaultParams) {
			t.Fatalf("rule %s defaultParams = %v, want %v", view.Key, view.DefaultParams, def.DefaultParams)
		}
		if len(view.Params) == 0 {
			t.Fatalf("rule %s params empty", view.Key)
		}
		// 未配置的库读出来就是默认值，GET 也必须是默认值。
		want := defaults[view.Key]
		if view.Enabled != want.Enabled || view.Severity != want.Severity ||
			view.CooldownMinutes != want.CooldownMinutes || len(view.Params) != len(want.Params) {
			t.Fatalf("rule %s = %+v, want default %+v", view.Key, view, want)
		}
	}
	if strings.Contains(raw, `"Interval"`) || strings.Contains(raw, `"interval":`) {
		t.Fatalf("GET leaked the raw Interval field: %s", raw)
	}
}

// PUT 合法值落库，回读一致（GET 与 PUT 的响应都对得上）。
func TestMonitorRulesPutRoundTrip(t *testing.T) {
	server, _ := monitorTestServer(t)
	_, views, _ := monitorRulesGet(t, server, monitorAdminID())
	body := make([]map[string]any, 0, len(views))
	for _, view := range views {
		row := map[string]any{
			"key": view.Key, "enabled": view.Enabled, "severity": string(view.Severity),
			"cooldownMinutes": view.CooldownMinutes, "params": view.Params,
		}
		if view.Key == "provider_down" {
			row["severity"] = "p0"
			row["cooldownMinutes"] = 15
			row["params"] = map[string]float64{"windowMinutes": 10, "minSamples": 30, "maxSuccessRatio": 0.7}
		}
		if view.Key == "multi_ip" {
			row["enabled"] = false
		}
		body = append(body, row)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	put := monitorRulesPut(t, server, string(payload), monitorAdminID())
	if put.Code != http.StatusOK {
		t.Fatalf("PUT rules = %d body=%s", put.Code, put.Body.String())
	}
	var putViews []monitorRuleView
	if err := json.Unmarshal(put.Body.Bytes(), &putViews); err != nil {
		t.Fatal(err)
	}
	_, getViews, _ := monitorRulesGet(t, server, monitorAdminID())
	for _, got := range [][]monitorRuleView{putViews, getViews} {
		if len(got) != 18 {
			t.Fatalf("rules after PUT = %d, want 18", len(got))
		}
		byKey := monitorRulesByKey(got)
		down := byKey["provider_down"]
		if down.Severity != "p0" || down.CooldownMinutes != 15 ||
			down.Params["windowMinutes"] != 10 || down.Params["minSamples"] != 30 || down.Params["maxSuccessRatio"] != 0.7 {
			t.Fatalf("provider_down after PUT = %+v", down)
		}
		if byKey["multi_ip"].Enabled {
			t.Fatalf("multi_ip should be disabled after PUT: %+v", byKey["multi_ip"])
		}
		if byKey["quota_low"].CooldownMinutes != 60 {
			t.Fatalf("untouched rule changed: %+v", byKey["quota_low"])
		}
	}
	stored := monitoring.NewConfig(server.monitoringStore).Rules()
	if len(stored) != 18 {
		t.Fatalf("stored rules = %d, want 18", len(stored))
	}
	var storedDown monitoring.RuleConfig
	for _, rule := range stored {
		if rule.Key == "provider_down" {
			storedDown = rule
		}
	}
	if storedDown.Severity != monitoring.SeverityP0 || storedDown.CooldownMinutes != 15 || storedDown.Params["minSamples"] != 30 {
		t.Fatalf("stored provider_down = %+v", storedDown)
	}
}

// 各类非法值各自 400，且库里配置保持 PUT 之前的样子（先写一份非默认配置做基准，
// 再确认失败后连这份基准都没被覆盖）。
func TestMonitorRulesPutRejectsInvalid(t *testing.T) {
	server, _ := monitorTestServer(t)
	baseline := `[{"key":"provider_down","severity":"p2","cooldownMinutes":30,"params":{"windowMinutes":15,"minSamples":25,"maxSuccessRatio":0.6}}]`
	if put := monitorRulesPut(t, server, baseline, monitorAdminID()); put.Code != http.StatusOK {
		t.Fatalf("baseline PUT = %d body=%s", put.Code, put.Body.String())
	}
	before := monitoring.NewConfig(server.monitoringStore).RawRules()
	if before == "" {
		t.Fatal("baseline did not reach the store")
	}

	cases := []struct {
		name string
		body string
	}{
		{"unknown rule key", `[{"key":"no_such_rule"}]`},
		{"duplicate rule key", `[{"key":"provider_down"},{"key":"provider_down","cooldownMinutes":10}]`},
		{"windowMinutes 7", `[{"key":"provider_down","params":{"windowMinutes":7,"minSamples":20,"maxSuccessRatio":0.8}}]`},
		{"cooldownMinutes 1", `[{"key":"provider_down","cooldownMinutes":1}]`},
		{"ratio 1.5", `[{"key":"provider_down","params":{"maxSuccessRatio":1.5}}]`},
		{"unknown param name", `[{"key":"provider_down","params":{"bogusParam":1}}]`},
		{"unknown severity", `[{"key":"provider_down","severity":"p9"}]`},
		{"malformed json", `{"key":"provider_down"}`},
		// [] 与 null 补齐后是整表默认值：不能借此静默清空用户的自定义规则。
		{"empty list", `[]`},
		{"null list", `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := monitorRulesPut(t, server, tc.body, monitorAdminID())
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d body=%s, want 400", tc.name, recorder.Code, recorder.Body.String())
			}
			after := monitoring.NewConfig(server.monitoringStore).RawRules()
			if after != before {
				t.Fatalf("%s wrote to the store: before=%s after=%s", tc.name, before, after)
			}
		})
	}
}

// 缺项 PUT：响应里该条必须是默认值（不能回显旧值，也不能悄悄保留），库里也落成
// 默认值。
func TestMonitorRulesPutMissingEntriesBecomeDefaults(t *testing.T) {
	server, _ := monitorTestServer(t)
	custom := `[{"key":"provider_down","enabled":false,"cooldownMinutes":30},{"key":"multi_ip","params":{"windowMinutes":15}}]`
	if put := monitorRulesPut(t, server, custom, monitorAdminID()); put.Code != http.StatusOK {
		t.Fatalf("custom PUT = %d body=%s", put.Code, put.Body.String())
	}

	put := monitorRulesPut(t, server, `[{"key":"multi_ip","cooldownMinutes":45}]`, monitorAdminID())
	if put.Code != http.StatusOK {
		t.Fatalf("partial PUT = %d body=%s", put.Code, put.Body.String())
	}
	var views []monitorRuleView
	if err := json.Unmarshal(put.Body.Bytes(), &views); err != nil {
		t.Fatal(err)
	}
	byKey := monitorRulesByKey(views)
	if got := byKey["provider_down"]; !got.Enabled || got.CooldownMinutes != 60 || got.Severity != monitoring.SeverityP1 {
		t.Fatalf("missing provider_down not reset to default: %+v", got)
	}
	if got := byKey["multi_ip"].Params["windowMinutes"]; got != 10 {
		t.Fatalf("missing param should reset to default 10, got %v", got)
	}
	if got := byKey["multi_ip"].CooldownMinutes; got != 45 {
		t.Fatalf("given cooldownMinutes not applied: %d", got)
	}
	stored := monitoring.NewConfig(server.monitoringStore).Rules()
	storedByKey := make(map[string]monitoring.RuleConfig, len(stored))
	for _, rule := range stored {
		storedByKey[rule.Key] = rule
	}
	if !storedByKey["provider_down"].Enabled || storedByKey["provider_down"].CooldownMinutes != 60 {
		t.Fatalf("store kept the old provider_down: %+v", storedByKey["provider_down"])
	}
	if storedByKey["multi_ip"].CooldownMinutes != 45 || storedByKey["multi_ip"].Params["windowMinutes"] != 10 {
		t.Fatalf("stored multi_ip = %+v", storedByKey["multi_ip"])
	}
}

// 规则接口管理员专属：普通用户 403、无身份 401，且 PUT 不落库。
func TestMonitorRulesRequireAdmin(t *testing.T) {
	server, _ := monitorTestServer(t)
	for _, tc := range []struct {
		method  string
		handler http.HandlerFunc
		body    string
	}{
		{http.MethodGet, server.handleMonitorRules, ""},
		{http.MethodPut, server.handleMonitorUpdateRules, `[{"key":"provider_down","cooldownMinutes":10}]`},
	} {
		if recorder := monitorCall(t, tc.handler, tc.method, "/__monitor/rules", tc.body, nil, monitorUserID()); recorder.Code != http.StatusForbidden {
			t.Fatalf("%s rules as user = %d body=%s, want 403", tc.method, recorder.Code, recorder.Body.String())
		}
	}
	if got := monitoring.NewConfig(server.monitoringStore).RawRules(); got != "" {
		t.Fatalf("non-admin PUT reached the store: %s", got)
	}

	// 走完整 Handler() 链：回环即管理员，路由已注册（不是 404）。
	server.SetAdminAuthStore(newMemoryAdminAuthStore())
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18093/__monitor/rules", nil)
	request.RemoteAddr = "127.0.0.1:55555"
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /__monitor/rules as local admin = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

// 空表不是「清空配置」的合法写法：整表替换语义下 `[]` 会把 18 条规则全部
// 恢复默认并落库，前端一个 bug 就能静默抹掉用户的自定义。要恢复默认必须
// 显式发 18 条默认值，所以空列表一律 400 且不落库。
func TestMonitorRulesPutRejectsEmptyTable(t *testing.T) {
	for _, body := range []string{"[]", "null"} {
		server, _ := monitorTestServer(t)
		before := monitorRulesPut(t, server, `[{"key":"provider_down","cooldownMinutes":15}]`, monitorAdminID())
		if before.Code != http.StatusOK {
			t.Fatalf("seed PUT = %d body=%s", before.Code, before.Body.String())
		}
		stored := monitoring.NewConfig(server.monitoringStore).RawRules()

		recorder := monitorRulesPut(t, server, body, monitorAdminID())
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("PUT %s = %d body=%s, want 400", body, recorder.Code, recorder.Body.String())
		}
		if got := monitoring.NewConfig(server.monitoringStore).RawRules(); got != stored {
			t.Fatalf("PUT %s reached the store: %s -> %s", body, stored, got)
		}
	}
}

// ── 强制熔断隔离 ─────────────────────────────────────────────────────────

// isolationCall 与 monitorCall 相同，只是多带一个可选的 X-Monitor-Action 头：
// 两个写接口要求它存在，普通 POST 表单因此无法跨站触发隔离。
func isolationCall(t *testing.T, handler http.HandlerFunc, method, target, body string,
	values map[string]string, identity sessionIdentity, action string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	for key, value := range values {
		request.SetPathValue(key, value)
	}
	if action != "" {
		request.Header.Set(monitorIsolationActionHeader, action)
	}
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, identity))
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	return recorder
}

func isolationTestServer(t *testing.T, providers ...domain.Provider) (*Server, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "isolation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	server := NewServer(NewRouter(domain.GatewayState{Providers: providers}), monitor.NewStore(), db)
	if server.monitoringStore == nil {
		t.Fatal("store does not satisfy monitoringStore")
	}
	return server, db
}

func isolationProvider(id, name string) domain.Provider {
	return domain.Provider{
		ID: id, Name: name, Protocol: domain.ProtocolClaude, AuthType: domain.AuthTypeAPIKey,
		BaseURL: "https://api.example.com", APIKeySource: "literal:sk", DefaultModel: "claude-sonnet-4-5",
	}
}

// isolatedAccount 跑一次隔离并断言 200，返回响应里的账号行。
func isolatedAccount(t *testing.T, server *Server, providerID string, identity sessionIdentity) monitorAccountView {
	t.Helper()
	recorder := isolationCall(t, server.handleMonitorIsolateAccount, http.MethodPost,
		"/__monitor/accounts/"+providerID+"/isolate", "", map[string]string{"providerId": providerID}, identity, "isolate")
	if recorder.Code != http.StatusOK {
		t.Fatalf("isolate code = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Account monitorAccountView `json:"account"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Account
}

func providerByIDInState(state domain.GatewayState, providerID string) (domain.Provider, bool) {
	for _, provider := range state.Providers {
		if provider.ID == providerID {
			return provider, true
		}
	}
	return domain.Provider{}, false
}

// 隔离开关就是 Disabled，但它必须真的落库：进程重启后重新从 SQLite 读回来的
// router 仍然要跳过这个账号，否则重启 = 隔离失效。
func TestMonitorIsolatePersistsDisabledAndRestoreClearsIt(t *testing.T) {
	server, db := isolationTestServer(t, isolationProvider("p-iso", "待隔离"))

	account := isolatedAccount(t, server, "p-iso", monitorAdminID())
	if !account.Disabled || !account.Isolated || account.IsolatedAt == "" || account.IsolatedBy != "admin" {
		t.Fatalf("isolated account view = %+v", account)
	}

	// 重新打开同一个库：这才是「落库」的判据，只看内存字段证明不了什么。
	reopened, err := store.Open(db.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	state, err := reopened.Load(domain.GatewayState{})
	if err != nil {
		t.Fatal(err)
	}
	if stored, found := providerByIDInState(state, "p-iso"); !found || !stored.Disabled {
		t.Fatalf("reopened store must keep provider disabled, found=%v provider=%+v", found, stored)
	}
	restarted := NewServer(NewRouter(state), monitor.NewStore(), reopened)
	if mark, ok := restarted.monitorIsolationMark("p-iso"); !ok || mark.By != "admin" {
		t.Fatalf("isolation marker must survive a restart, got %+v ok=%v", mark, ok)
	}
	if accounts := restarted.monitorAccountViewsOf(restarted.monitorProviders()); len(accounts) != 1 ||
		!accounts[0].Disabled || !accounts[0].Isolated {
		t.Fatalf("restarted server must still show the account isolated: %+v", accounts)
	}

	restored := isolationCall(t, restarted.handleMonitorRestoreAccount, http.MethodPost,
		"/__monitor/accounts/p-iso/restore", "", map[string]string{"providerId": "p-iso"}, monitorAdminID(), "restore")
	if restored.Code != http.StatusOK {
		t.Fatalf("restore code = %d body = %s", restored.Code, restored.Body.String())
	}
	var restoredBody struct {
		Account monitorAccountView `json:"account"`
	}
	if err := json.Unmarshal(restored.Body.Bytes(), &restoredBody); err != nil {
		t.Fatal(err)
	}
	if restoredBody.Account.Disabled || restoredBody.Account.Isolated {
		t.Fatalf("restored account = %+v", restoredBody.Account)
	}
	if _, ok := restarted.monitorIsolationMark("p-iso"); ok {
		t.Fatal("restore must drop the isolation marker")
	}
	state, err = reopened.Load(domain.GatewayState{})
	if err != nil {
		t.Fatal(err)
	}
	if stored, found := providerByIDInState(state, "p-iso"); !found || stored.Disabled {
		t.Fatalf("restore must be persisted too: found=%v provider=%+v", found, stored)
	}

	// 事件流里两条系统事件都要有，且带上操作者。
	page, err := db.QueryMonitorEvents(monitoring.EventQuery{Kind: monitoring.EventKindSystem, Page: 1, PageSize: 50})
	if err != nil {
		t.Fatal(err)
	}
	details := map[string]map[string]any{}
	for _, event := range page.Items {
		if event.TargetID == "p-iso" {
			details[event.Title] = event.Detail
		}
	}
	if details["账号已强制熔断隔离"]["operator"] != "admin" {
		t.Fatalf("isolation event missing or without operator: %+v", details)
	}
	if details["账号已恢复调度"]["operator"] != "admin" {
		t.Fatalf("restore event missing or without operator: %+v", details)
	}
}

// 这两个接口会改变调度行为，必须 admin-only；普通用户拿到 403 且状态不变。
func TestMonitorIsolateAndRestoreRequireAdmin(t *testing.T) {
	server, _ := isolationTestServer(t, isolationProvider("p1", "Claude"))
	for _, item := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"isolate", server.handleMonitorIsolateAccount},
		{"restore", server.handleMonitorRestoreAccount},
	} {
		recorder := isolationCall(t, item.handler, http.MethodPost, "/__monitor/accounts/p1/"+item.name,
			"", map[string]string{"providerId": "p1"}, monitorUserID(), item.name)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%s as normal user: code = %d body = %s", item.name, recorder.Code, recorder.Body.String())
		}
	}
	provider, err := server.router.ProviderByID("p1")
	if err != nil || provider.Disabled {
		t.Fatalf("a rejected call must not touch the provider: %+v err=%v", provider, err)
	}
	// 没有 X-Monitor-Action 头的写请求一律 403（CSRF 防线），管理员也不行。
	noHeader := isolationCall(t, server.handleMonitorIsolateAccount, http.MethodPost,
		"/__monitor/accounts/p1/isolate", "", map[string]string{"providerId": "p1"}, monitorAdminID(), "")
	if noHeader.Code != http.StatusForbidden {
		t.Fatalf("missing action header: code = %d", noHeader.Code)
	}
}

// 重复隔离 409；手工停用的账号隔离 409 且原样不动；没隔离的账号恢复 409。
func TestMonitorIsolateConflicts(t *testing.T) {
	manuallyOff := isolationProvider("p-off", "手工停用")
	manuallyOff.Disabled = true
	server, _ := isolationTestServer(t,
		isolationProvider("p-iso", "已隔离"),
		manuallyOff,
		isolationProvider("p-on", "正常"),
	)
	isolatedAccount(t, server, "p-iso", monitorAdminID())

	again := isolationCall(t, server.handleMonitorIsolateAccount, http.MethodPost,
		"/__monitor/accounts/p-iso/isolate", "", map[string]string{"providerId": "p-iso"}, monitorAdminID(), "isolate")
	if again.Code != http.StatusConflict {
		t.Fatalf("repeated isolate: code = %d body = %s", again.Code, again.Body.String())
	}

	manual := isolationCall(t, server.handleMonitorIsolateAccount, http.MethodPost,
		"/__monitor/accounts/p-off/isolate", "", map[string]string{"providerId": "p-off"}, monitorAdminID(), "isolate")
	if manual.Code != http.StatusConflict || !strings.Contains(manual.Body.String(), "手工停用") {
		t.Fatalf("manually disabled account: code = %d body = %s", manual.Code, manual.Body.String())
	}
	if _, ok := server.monitorIsolationMark("p-off"); ok {
		t.Fatal("a manually disabled account must not gain an isolation marker")
	}

	notIsolated := isolationCall(t, server.handleMonitorRestoreAccount, http.MethodPost,
		"/__monitor/accounts/p-on/restore", "", map[string]string{"providerId": "p-on"}, monitorAdminID(), "restore")
	if notIsolated.Code != http.StatusConflict {
		t.Fatalf("restore of a live account: code = %d body = %s", notIsolated.Code, notIsolated.Body.String())
	}

	missing := isolationCall(t, server.handleMonitorIsolateAccount, http.MethodPost,
		"/__monitor/accounts/ghost/isolate", "", map[string]string{"providerId": "ghost"}, monitorAdminID(), "isolate")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown provider: code = %d", missing.Code)
	}
}

// 隔离之后调度真的会跳过这个账号——请求一路走到真实失败（没有可用上游），
// 而不是只看字段：隔离后链首被剔除，同样的请求只能落到备用账号；恢复后它又
// 回到链首。Key 不带兜底链时隔离就等于「无可用账号」，所以这里给它一条备选。
func TestMonitorIsolateRemovesAccountFromScheduling(t *testing.T) {
	primary := isolationProvider("p-primary", "主号")
	primary.FirstPacketTimeoutSec = 2
	backup := isolationProvider("p-backup", "备号")
	backup.BaseURL = "https://api2.example.com"
	server, _ := isolationTestServer(t, primary, backup)
	route := domain.Route{ID: "r-p", Name: "主路由", ProviderID: "p-primary", OutputProtocol: domain.ProtocolClaude, Enabled: true}
	if _, err := server.router.AddRoute(route); err != nil {
		t.Fatal(err)
	}
	key := domain.APIKey{ID: "k1", Name: "admin key", RouteID: "r-p", FallbackProviderIDs: []string{"p-backup"}}
	run := func() domain.RouteDecision {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
		// 调度器从请求里取身份来决定这条 Key 能用哪些账号。
		request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, monitorAdminID()))
		// 调度轨迹挂在 handler 建的 requestTiming 上；这里自己建一个，才能读到实际尝试顺序。
		request = request.WithContext(withRequestTiming(request.Context(), newRequestTiming(time.Now())))
		// 与真实 handler 一样先取决策，再按 Key 的链与隔离状态归因到实际首选的账号。
		seeded, err := server.router.Decide(route.ID)
		if err != nil {
			t.Fatal(err)
		}
		seeded = server.decisionForAPIKey(route, key, seeded, "")
		_, _, _, _, _, execErr := server.executeProtocolFlowWithFailover(
			recorder, request, route, seeded,
			"claude-sonnet-4-5", map[string]any{"model": "claude-sonnet-4-5"},
			domain.ProtocolClaude, true, key, true,
		)
		if execErr == nil {
			t.Fatal("both example.com upstreams are unreachable; the flow must fail")
		}
		// 返回值里的 decision 只说明「最初打算发给谁」；真正发给谁看调度轨迹。
		trace := scheduleTraceOf(request.Context())
		if trace == nil || len(trace.attempts) == 0 {
			t.Fatalf("no scheduling trace recorded: %+v", trace)
		}
		return domain.RouteDecision{ProviderID: trace.attempts[0].ProviderID}
	}
	if picked := run(); picked.ProviderID != "p-primary" {
		t.Fatalf("before isolation the request must reach the primary, got %q", picked.ProviderID)
	}

	account := isolatedAccount(t, server, "p-primary", monitorAdminID())
	if !account.Disabled {
		t.Fatalf("isolated account must be disabled: %+v", account)
	}
	if decision := run(); decision.ProviderID != "p-backup" {
		chain := apiKeyProviderChain(route.ProviderID, key.FallbackProviderIDs)
		p, _ := server.router.ProviderByID("p-primary")
		t.Fatalf("after isolation the request must skip the primary and land on the backup, got %q (disabled=%v allowed=%v)", decision.ProviderID, p.Disabled, server.allowedProviderChainForKey(key, chain))
	}

	restored := isolationCall(t, server.handleMonitorRestoreAccount, http.MethodPost,
		"/__monitor/accounts/p-primary/restore", "", map[string]string{"providerId": "p-primary"}, monitorAdminID(), "restore")
	if restored.Code != http.StatusOK {
		t.Fatalf("restore code = %d body = %s", restored.Code, restored.Body.String())
	}
	if decision := run(); decision.ProviderID != "p-primary" {
		t.Fatalf("restore must put the account back at the head of the chain, got %q", decision.ProviderID)
	}
}
