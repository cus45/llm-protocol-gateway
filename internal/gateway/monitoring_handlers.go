package gateway

// The monitoring center HTTP API (/__monitor/*). Everything here is admin-only
// and read-mostly: the console renders the account matrix, the event stream and
// the alert list from these endpoints. Credentials never leave the process —
// channel secrets come back as *Set booleans only.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitoring"
)

const (
	// monitorOverviewWindow is the traffic window the summary cards cover.
	monitorOverviewWindow = 24 * time.Hour
	// monitorAlertScanLimit bounds the one firing-alert read that feeds both the
	// overview counters and the per-account alert badges.
	monitorAlertScanLimit = 500
	// monitorProbeTimeout caps the two action endpoints, which reach upstream
	// vendors (a refresh, a quota read).
	monitorProbeTimeout = 15 * time.Second
	// monitorRelatedEvents is how many events one alert detail pulls.
	monitorRelatedEvents = 10
)

type monitorChannelView struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	MinSeverity string `json:"minSeverity"`
	Enabled     bool   `json:"enabled"`
	WebhookSet  bool   `json:"webhookSet,omitempty"`
	SecretSet   bool   `json:"secretSet,omitempty"`
	TokenSet    bool   `json:"tokenSet,omitempty"`
	ChatID      string `json:"chatId,omitempty"`
}

// channelView is the only shape a channel ever takes on the wire: GET, POST and
// PUT all go through it, so a response cannot leak a webhook token by being a
// different code path.
func channelView(channel monitoring.Channel) monitorChannelView {
	return monitorChannelView{
		ID: channel.ID, Type: channel.Type, Name: channel.Name,
		MinSeverity: string(channel.MinSeverity), Enabled: channel.Enabled,
		WebhookSet: strings.TrimSpace(channel.Config.Webhook) != "",
		SecretSet:  strings.TrimSpace(channel.Config.Secret) != "",
		TokenSet:   strings.TrimSpace(channel.Config.BotToken) != "",
		ChatID:     strings.TrimSpace(channel.Config.ChatID),
	}
}

func channelViews(channels []monitoring.Channel) []monitorChannelView {
	out := make([]monitorChannelView, 0, len(channels))
	for _, channel := range channels {
		out = append(out, channelView(channel))
	}
	return out
}

type monitorAccountView struct {
	ProviderID string `json:"providerId"`
	Name       string `json:"name"`
	Vendor     string `json:"vendor"`
	AuthType   string `json:"authType"`
	Disabled   bool   `json:"disabled"`
	// Isolated marks a disabled account whose switch was thrown by the
	// console's forced circuit-break (not by the admin's own 停用). It is the
	// same Disabled flag for scheduling; the marker only says why it is off, and
	// it is what 「恢复调度」 clears.
	Isolated     bool   `json:"isolated,omitempty"`
	IsolatedAt   string `json:"isolatedAt,omitempty"`
	IsolatedBy   string `json:"isolatedBy,omitempty"`
	HealthStatus string `json:"healthStatus"`
	// NextRetryAt is the live-request cooldown's end (RFC3339), the same runtime
	// hint the Providers page shows next to 异常. Empty unless healthStatus is
	// unavailable.
	NextRetryAt   string                     `json:"nextRetryAt,omitempty"`
	Capabilities  monitoring.Capabilities    `json:"capabilities"`
	ExpiresAt     string                     `json:"expiresAt,omitempty"`
	Quota         *monitoring.QuotaSnapshot  `json:"quota,omitempty"`
	Renewal       *monitoring.RenewalOutcome `json:"renewal,omitempty"`
	ModelFailures int                        `json:"modelFailures"`
	ActiveAlerts  int                        `json:"activeAlerts"`
}

type monitorOverviewView struct {
	GeneratedAt string `json:"generatedAt"`
	WindowHours int    `json:"windowHours"`
	Requests    struct {
		Total        int     `json:"total"`
		Success      int     `json:"success"`
		SuccessRate  float64 `json:"successRate"`
		P95LatencyMs int64   `json:"p95LatencyMs"`
		CostMicros   int64   `json:"costMicros"`
		Priced       int     `json:"priced"`
		// LatencyBuckets: <20 / 20-50 / 50-100 / >100ms 的实际请求数。
		LatencyBuckets []int `json:"latencyBuckets"`
	} `json:"requests"`
	Accounts struct {
		Total       int `json:"total"`
		Subscribed  int `json:"subscribed"`
		Unavailable int `json:"unavailable"`
		Disabled    int `json:"disabled"`
	} `json:"accounts"`
	Firing struct {
		Total int `json:"total"`
		P0    int `json:"p0"`
		P1    int `json:"p1"`
		P2    int `json:"p2"`
	} `json:"firing"`
	Channels struct {
		Total   int `json:"total"`
		Enabled int `json:"enabled"`
	} `json:"channels"`
	CollectorLastSuccess string             `json:"collectorLastSuccess,omitempty"`
	RecentEvents         []monitoring.Event `json:"recentEvents"`
}

// monitorGate is the shared preamble: admin session plus a store that can
// persist monitoring state. Without the store the console gets 503 rather than
// a panic on a nil interface.
func (s *Server) monitorGate(w http.ResponseWriter, r *http.Request) (sessionIdentity, bool) {
	identity, ok := s.requireAdmin(w, r)
	if !ok {
		return sessionIdentity{}, false
	}
	if s.monitoringStore == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "监控中心未启用")
		return sessionIdentity{}, false
	}
	return identity, true
}

func (s *Server) handleMonitorOverview(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	view, err := s.monitorOverviewOf(s.monitorProviders())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// monitorOverview builds the summary card payload. The stream snapshot reuses
// it so the two endpoints cannot drift apart.
// monitorOverviewOf builds the summary card payload for a provider list the caller
// already read, so the stream snapshot can freeze one list across all sections.
func (s *Server) monitorOverviewOf(providers []domain.Provider) (monitorOverviewView, error) {
	now := time.Now().UTC()
	summary, err := s.monitoringStore.MonitorRequestSummary(now.Add(-monitorOverviewWindow), now)
	if err != nil {
		return monitorOverviewView{}, fmt.Errorf("读取请求汇总失败")
	}
	view := monitorOverviewView{GeneratedAt: now.Format(time.RFC3339), WindowHours: 24}
	view.Requests.Total = summary.Total
	view.Requests.Success = summary.Success
	view.Requests.P95LatencyMs = summary.P95LatencyMs
	view.Requests.CostMicros = summary.CostMicros
	view.Requests.Priced = summary.Priced
	view.Requests.LatencyBuckets = summary.LatencyBuckets[:]
	if summary.Total > 0 {
		view.Requests.SuccessRate = roundRatio(float64(summary.Success) / float64(summary.Total))
	}

	for _, provider := range providers {
		view.Accounts.Total++
		if provider.Disabled {
			view.Accounts.Disabled++
		}
		if providerHasExpiry(provider) {
			view.Accounts.Subscribed++
		}
		if strings.EqualFold(provider.HealthStatus, "unavailable") {
			view.Accounts.Unavailable++
		}
	}

	firing := s.monitorFiringAlerts()
	view.Firing.Total = len(firing)
	for _, alert := range firing {
		switch alert.Severity {
		case monitoring.SeverityP0:
			view.Firing.P0++
		case monitoring.SeverityP1:
			view.Firing.P1++
		case monitoring.SeverityP2:
			view.Firing.P2++
		}
	}

	channels := monitoring.NewConfig(s.monitoringStore).Channels()
	view.Channels.Total = len(channels)
	for _, channel := range channels {
		if channel.Enabled {
			view.Channels.Enabled++
		}
	}
	view.CollectorLastSuccess = s.monitoringStore.Setting("monitor_collector_last_success")

	events, err := s.monitoringStore.QueryMonitorEvents(monitoring.EventQuery{Page: 1, PageSize: 20})
	if err != nil {
		return monitorOverviewView{}, fmt.Errorf("读取事件失败")
	}
	view.RecentEvents = events.Items
	return view, nil
}

func (s *Server) handleMonitorAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	accounts := s.monitorAccountViewsOf(s.monitorProviders())
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts, "total": len(accounts)})
}

func (s *Server) handleMonitorAccount(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	providerID := strings.TrimSpace(r.PathValue("providerId"))
	for _, account := range s.monitorAccountViewsOf(s.monitorProviders()) {
		if account.ProviderID == providerID {
			writeJSON(w, http.StatusOK, account)
			return
		}
	}
	writeOpenAIError(w, http.StatusNotFound, "账号不存在")
}

// handleMonitorAccountRefresh renews one account's credential through the same
// path a live request uses. A credential that is not inside its refresh window
// reports renewed:false: rotating a refresh token early would spend a token that
// still works, and several vendors invalidate the whole chain when one token is
// presented twice.
func (s *Server) handleMonitorAccountRefresh(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	providerID := strings.TrimSpace(r.PathValue("providerId"))
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, "账号不存在")
		return
	}
	if !providerHasExpiry(provider) {
		writeOpenAIError(w, http.StatusBadRequest, "该账号没有可续期的登录凭证")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), monitorProbeTimeout)
	defer cancel()
	started := time.Now()
	renewedProvider, renewErr := s.renewCredentialForMonitor(ctx, provider)
	rotated := providerAccessToken(renewedProvider) != providerAccessToken(provider)
	if renewErr == nil && !rotated {
		s.recordRenewalSkipped(provider, providerVendorKey(provider), started)
	}
	result := map[string]any{
		"ok":         renewErr == nil,
		"providerId": provider.ID,
		"vendor":     providerVendorKey(provider),
		"renewed":    rotated,
	}
	if expiry := providerCredentialExpiry(renewedProvider); !expiry.IsZero() {
		result["expiresAt"] = expiry.UTC().Format(time.RFC3339)
	}
	if renewErr != nil {
		// Only the classification crosses the wire; the upstream error may quote
		// the token it failed on.
		result["errorKind"] = classifyQuotaError(renewErr)
	}
	writeJSON(w, http.StatusOK, result)
}

// ── 强制熔断隔离 ─────────────────────────────────────────────────────────
//
// 隔离就是「临时停用」：调度看的仍是域模型里的 Disabled 开关，这一节不引入
// 第二套状态机。多出来的只有两样东西——一条隔离标记（谁、什么时候）和「只由
// 人工恢复」的语义。标记不进 Provider（避免动持久化 schema），而是落在
// settings 表的一个 JSON 键里；它和 Disabled 由同一把隔离锁一起改，所以「标了
// 隔离但没停用」这种半截状态不会出现在读者眼里。

const (
	// monitorIsolationSettingKey 是隔离标记在 settings 表里的键：
	// {providerId: {at, by}}。恢复时删掉对应条目。
	monitorIsolationSettingKey = "monitor_provider_isolation"
	// monitorIsolationActionHeader 让写操作带上发起它的纯文本请求头；浏览器
	// 跨站表单发不出这个头，CSRF 也就写不进来。
	monitorIsolationActionHeader = "X-Monitor-Action"
)

type monitorIsolationMark struct {
	At string `json:"at"`
	By string `json:"by"`
}

func (s *Server) monitorIsolationMarks() map[string]monitorIsolationMark {
	marks := map[string]monitorIsolationMark{}
	raw := s.monitoringStore.Setting(monitorIsolationSettingKey)
	if strings.TrimSpace(raw) == "" {
		return marks
	}
	if err := json.Unmarshal([]byte(raw), &marks); err != nil {
		return map[string]monitorIsolationMark{}
	}
	return marks
}

func (s *Server) monitorIsolationMark(providerID string) (monitorIsolationMark, bool) {
	mark, ok := s.monitorIsolationMarks()[providerID]
	return mark, ok
}

func (s *Server) saveMonitorIsolationMarks(marks map[string]monitorIsolationMark) error {
	raw, err := json.Marshal(marks)
	if err != nil {
		return err
	}
	return s.monitoringStore.SetSetting(monitorIsolationSettingKey, string(raw))
}

// monitorIsolationAccountView rebuilds one account row after a write. It goes
// through the same builder as the list so the button's reply and the next poll
// cannot disagree about what the card shows.
func (s *Server) monitorIsolationAccountView(providerID string) (monitorAccountView, bool) {
	for _, account := range s.monitorAccountViewsOf(s.monitorProviders()) {
		if account.ProviderID == providerID {
			return account, true
		}
	}
	return monitorAccountView{}, false
}

// handleMonitorIsolateAccount 强制熔断隔离一个账号。已经被手工停用的账号回 409：
// 再标一层隔离会把「谁停的」搅混，恢复时也不知道该不该重新启用。重复隔离同样
// 409，幂等由调用方按 409 当「已经是隔离态」处理。
func (s *Server) handleMonitorIsolateAccount(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.monitorGate(w, r)
	if !ok {
		return
	}
	if r.Header.Get(monitorIsolationActionHeader) == "" {
		writeOpenAIError(w, http.StatusForbidden, "写操作需要 "+monitorIsolationActionHeader+" 请求头")
		return
	}
	providerID := strings.TrimSpace(r.PathValue("providerId"))
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, "账号不存在")
		return
	}
	s.monitorIsolationMu.Lock()
	defer s.monitorIsolationMu.Unlock()
	marks := s.monitorIsolationMarks()
	if _, isolated := marks[provider.ID]; isolated {
		writeOpenAIError(w, http.StatusConflict, "该账号已处于强制熔断隔离中")
		return
	}
	if provider.Disabled {
		writeOpenAIError(w, http.StatusConflict, "该账号已被手工停用，请先在 Providers 页启用，再执行熔断隔离")
		return
	}
	now := time.Now().UTC()
	if _, err := s.router.SetProviderDisabled(provider.ID, true); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "账号不存在")
		return
	}
	// 先后顺序：先落 Provider（调度立即跳过它），再落标记。标记写失败就回滚停用，
	// 不留下一个「停了但不知道谁停的」账号。
	if err := s.saveState(); err != nil {
		_, _ = s.router.SetProviderDisabled(provider.ID, false)
		writeOpenAIError(w, http.StatusInternalServerError, "保存配置失败："+err.Error())
		return
	}
	marks[provider.ID] = monitorIsolationMark{At: now.Format(time.RFC3339), By: identity.Username}
	if err := s.saveMonitorIsolationMarks(marks); err != nil {
		_, _ = s.router.SetProviderDisabled(provider.ID, false)
		_ = s.saveState()
		writeOpenAIError(w, http.StatusInternalServerError, "保存隔离标记失败")
		return
	}
	// 与 Providers 页的停用同一条路径：停用即停止可用性重探。
	s.markProviderAvailable(provider.ID)
	s.logs.AddApp("warn", "provider isolated", fmt.Sprintf("provider=%s by=%s", provider.ID, identity.Username))
	s.monitorRecordIsolationEvent(provider, "账号已强制熔断隔离", map[string]any{
		"action": "isolate", "operator": identity.Username, "isolatedAt": now.Format(time.RFC3339), "disabled": true,
	})
	writeJSON(w, http.StatusOK, s.monitorIsolationView(provider.ID))
}

// handleMonitorRestoreAccount 解除隔离：清标记并把 Disabled 翻回 false。本来就没
// 隔离、也没停用的账号回 409——恢复只针对隔离态，不兼做「启用手工停用的账号」。
func (s *Server) handleMonitorRestoreAccount(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.monitorGate(w, r)
	if !ok {
		return
	}
	if r.Header.Get(monitorIsolationActionHeader) == "" {
		writeOpenAIError(w, http.StatusForbidden, "写操作需要 "+monitorIsolationActionHeader+" 请求头")
		return
	}
	providerID := strings.TrimSpace(r.PathValue("providerId"))
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, "账号不存在")
		return
	}
	s.monitorIsolationMu.Lock()
	defer s.monitorIsolationMu.Unlock()
	marks := s.monitorIsolationMarks()
	if _, isolated := marks[provider.ID]; !isolated {
		if provider.Disabled {
			writeOpenAIError(w, http.StatusConflict, "该账号是手工停用而非熔断隔离，请在 Providers 页启用")
			return
		}
		writeOpenAIError(w, http.StatusConflict, "该账号没有处于强制熔断隔离中")
		return
	}
	if _, err := s.router.SetProviderDisabled(provider.ID, false); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "账号不存在")
		return
	}
	if err := s.saveState(); err != nil {
		_, _ = s.router.SetProviderDisabled(provider.ID, true)
		writeOpenAIError(w, http.StatusInternalServerError, "保存配置失败："+err.Error())
		return
	}
	delete(marks, provider.ID)
	if err := s.saveMonitorIsolationMarks(marks); err != nil {
		// 账号已经回到调度里，标记留着只会让卡片显示「已隔离」，要写回原样。
		marks[provider.ID] = monitorIsolationMark{At: time.Now().UTC().Format(time.RFC3339), By: identity.Username}
		writeOpenAIError(w, http.StatusInternalServerError, "清除隔离标记失败")
		return
	}
	s.logs.AddApp("info", "provider is restored to scheduling", fmt.Sprintf("provider=%s by=%s", provider.ID, identity.Username))
	s.monitorRecordIsolationEvent(provider, "账号已恢复调度", map[string]any{
		"action": "restore", "operator": identity.Username, "disabled": false,
	})
	writeJSON(w, http.StatusOK, s.monitorIsolationView(provider.ID))
}

func (s *Server) monitorIsolationView(providerID string) map[string]any {
	account, found := s.monitorIsolationAccountView(providerID)
	if !found {
		return map[string]any{"providerId": providerID}
	}
	return map[string]any{"account": account}
}

// monitorRecordIsolationEvent 写一条系统事件到事件流。写失败不改变操作结果——
// 隔离已经生效，少一条事件不值得报错。
func (s *Server) monitorRecordIsolationEvent(provider domain.Provider, title string, detail map[string]any) {
	now := time.Now().UTC()
	detail["providerId"] = provider.ID
	if _, err := s.monitoringStore.InsertMonitorEvent(monitoring.Event{
		Time: now, Kind: monitoring.EventKindSystem, Severity: monitoring.SeverityInfo,
		TargetType: monitoring.TargetProvider, TargetID: provider.ID, TargetName: provider.Name,
		Title: title, Status: monitoring.StatusInfo, Detail: detail,
		FirstSeen: now, LastSeen: now,
	}); err != nil {
		slog.Warn("monitoring isolation event failed", "provider", provider.ID, "error", err)
	}
}

// handleMonitorQuotaRefresh forces one vendor usage read and persists it, so the
// console can show a fresh number instead of the last sampled hour.
func (s *Server) handleMonitorQuotaRefresh(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	if s.monitoringSrc == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "监控中心未启用")
		return
	}
	providerID := strings.TrimSpace(r.PathValue("providerId"))
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, "账号不存在")
		return
	}
	if !providerHasQuotaHandler(provider) {
		writeOpenAIError(w, http.StatusBadRequest, "该账号不支持额度查询")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), monitorProbeTimeout)
	defer cancel()
	snapshot, err := s.monitoringSrc.Quota(ctx, monitoring.ProviderObservation{
		ID: provider.ID, Name: provider.Name, Vendor: providerVendorKey(provider),
	})
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "额度查询失败")
		return
	}
	if snapshot == nil {
		writeOpenAIError(w, http.StatusBadGateway, "额度查询没有返回结果")
		return
	}
	if snapshot.Error != "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "errorKind": snapshot.Error})
		return
	}
	snapshot.Stale = false
	s.persistQuotaSnapshot(provider, snapshot)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "quota": snapshot})
}

// persistQuotaSnapshot stores one quota read as the provider's latest sample and
// hands it back to the collector cache. Without the cache hand-off the next tick
// would upsert the older cached snapshot over this fresh row a minute later.
func (s *Server) persistQuotaSnapshot(provider domain.Provider, snapshot *monitoring.QuotaSnapshot) {
	if snapshot == nil {
		return
	}
	snapshot.Stale = false
	if snapshot.ObservedAt.IsZero() {
		snapshot.ObservedAt = time.Now().UTC()
	}
	if s.monitoringStore != nil {
		sample := monitoring.MetricSample{
			ProviderID: provider.ID, AuthExpiresAt: monitorSampleTime(providerCredentialExpiry(provider)),
			HealthStatus: provider.HealthStatus, ObservedAt: snapshot.ObservedAt,
			QuotaWindows: snapshot.Windows,
		}
		if snapshot.Available() {
			ratio := snapshot.Ratio
			sample.QuotaRatio = &ratio
		}
		if err := s.monitoringStore.UpsertProviderMetricSample(sample); err != nil {
			slog.Warn("quota sample upsert failed", "provider", provider.ID, "error", err)
		}
	}
	if s.monitoring != nil {
		s.monitoring.NoteQuotaSnapshot(provider.ID, *snapshot)
	}
}

func (s *Server) handleMonitorEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	from, to := monitorTimeRange(r, monitorOverviewWindow)
	query := monitoring.EventQuery{
		Kind:     strings.TrimSpace(r.URL.Query().Get("kind")),
		Severity: strings.TrimSpace(r.URL.Query().Get("severity")),
		From:     from, To: to,
		Page: monitorIntParam(r, "page", 1), PageSize: monitorIntParam(r, "pageSize", 50),
	}
	if target := strings.TrimSpace(r.URL.Query().Get("targetId")); target != "" {
		query.TargetID = target
		query.TargetType = monitoring.TargetProvider
	}
	page, err := s.monitoringStore.QueryMonitorEvents(query)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "读取事件失败")
		return
	}
	writeJSONGzip(w, r, http.StatusOK, page)
}

func (s *Server) handleMonitorAlerts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	from, to := monitorTimeRange(r, 7*24*time.Hour)
	query := monitoring.AlertQuery{
		Status:      strings.TrimSpace(r.URL.Query().Get("status")),
		Disposition: strings.TrimSpace(r.URL.Query().Get("disposition")),
		Severity:    strings.TrimSpace(r.URL.Query().Get("severity")),
		Rule:        strings.TrimSpace(r.URL.Query().Get("rule")),
		From:        from, To: to,
		Page: monitorIntParam(r, "page", 1), PageSize: monitorIntParam(r, "pageSize", 50),
	}
	if target := strings.TrimSpace(r.URL.Query().Get("targetId")); target != "" {
		query.TargetID = target
		query.TargetType = monitoring.TargetProvider
	}
	page, err := s.monitoringStore.QueryMonitorAlerts(query)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "读取告警失败")
		return
	}
	writeJSONGzip(w, r, http.StatusOK, page)
}

func (s *Server) handleMonitorAlertByID(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	id, ok := monitorPathID(w, r)
	if !ok {
		return
	}
	state, found, err := s.monitoringStore.MonitorAlertByID(id)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "读取告警失败")
		return
	}
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "告警不存在")
		return
	}
	writeJSON(w, http.StatusOK, s.monitorAlertView(state))
}

// monitorAlertAck and monitorAlertIgnore only accept a firing alert: a resolved
// one has nothing left to acknowledge, and pretending otherwise would let a
// stale click erase the record of how an incident was handled.
func (s *Server) handleMonitorAlertAck(w http.ResponseWriter, r *http.Request) {
	s.monitorDisposition(w, r, monitoring.DispositionAcknowledged, "acknowledged")
}

func (s *Server) handleMonitorAlertIgnore(w http.ResponseWriter, r *http.Request) {
	s.monitorDisposition(w, r, monitoring.DispositionIgnored, "ignored")
}

func (s *Server) monitorDisposition(w http.ResponseWriter, r *http.Request, disposition, action string) {
	identity, ok := s.monitorGate(w, r)
	if !ok {
		return
	}
	id, ok := monitorPathID(w, r)
	if !ok {
		return
	}
	state, found, err := s.monitoringStore.MonitorAlertByID(id)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "读取告警失败")
		return
	}
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "告警不存在")
		return
	}
	if state.Status != monitoring.StatusFiring {
		writeOpenAIError(w, http.StatusConflict, "告警已结束，无需处置")
		return
	}
	now := time.Now().UTC()
	if err := s.monitoringStore.SetMonitorDisposition(id, disposition, now); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "处置告警失败")
		return
	}
	// The acknowledgement belongs to the incident, not to a new event row: it
	// lands in the latest event's timeline (spec §8.2).
	if err := s.monitoringStore.AppendMonitorTimeline(state.Fingerprint, monitoring.TimelineEntry{
		Action: action, Time: now, Actor: identity.Username,
	}); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "记录处置历史失败")
		return
	}
	updated, _, err := s.monitoringStore.MonitorAlertByID(id)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "读取告警失败")
		return
	}
	writeJSON(w, http.StatusOK, s.monitorAlertView(updated))
}

func (s *Server) handleMonitorChannels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, channelViews(monitoring.NewConfig(s.monitoringStore).Channels()))
}

type monitorChannelInput struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	MinSeverity string `json:"minSeverity"`
	Enabled     *bool  `json:"enabled"`
	Webhook     string `json:"webhook"`
	Secret      string `json:"secret"`
	BotToken    string `json:"botToken"`
	ChatID      string `json:"chatId"`
}

func (s *Server) handleMonitorCreateChannel(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	var input monitorChannelInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "渠道配置格式错误")
		return
	}
	cfg := monitoring.NewConfig(s.monitoringStore)
	channels := cfg.Channels()
	if _, known := monitoring.AdapterFor(strings.TrimSpace(input.Type)); !known {
		writeOpenAIError(w, http.StatusBadRequest, "不支持的渠道类型")
		return
	}
	channel := monitoring.Channel{
		ID:          "ch-" + randomHex(6),
		Type:        strings.TrimSpace(input.Type),
		Name:        strings.TrimSpace(input.Name),
		MinSeverity: monitorSeverity(input.MinSeverity, monitoring.SeverityP1),
		Enabled:     input.Enabled == nil || *input.Enabled,
		Config: monitoring.ChannelConfig{
			Webhook: strings.TrimSpace(input.Webhook), Secret: strings.TrimSpace(input.Secret),
			BotToken: strings.TrimSpace(input.BotToken), ChatID: strings.TrimSpace(input.ChatID),
		},
	}
	if channel.Name == "" {
		channel.Name = channel.Type
	}
	channels = append(channels, channel)
	if err := cfg.SaveChannels(channels); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "保存渠道失败")
		return
	}
	writeJSON(w, http.StatusCreated, channelView(channel))
}

func (s *Server) handleMonitorUpdateChannel(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	channelID := strings.TrimSpace(r.PathValue("id"))
	var input monitorChannelInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "渠道配置格式错误")
		return
	}
	cfg := monitoring.NewConfig(s.monitoringStore)
	channels := cfg.Channels()
	index := monitorChannelIndex(channels, channelID)
	if index < 0 {
		writeOpenAIError(w, http.StatusNotFound, "渠道不存在")
		return
	}
	channel := channels[index]
	// id and type are immutable: the type decides which adapter parses the
	// payload, and a push row already refers to this id.
	if name := strings.TrimSpace(input.Name); name != "" {
		channel.Name = name
	}
	channel.MinSeverity = monitorSeverity(input.MinSeverity, channel.MinSeverity)
	if input.Enabled != nil {
		channel.Enabled = *input.Enabled
	}
	if chatID := strings.TrimSpace(input.ChatID); chatID != "" {
		channel.Config.ChatID = chatID
	}
	// A blank secret means "keep the stored one", so the console can render an
	// empty password field without wiping a working webhook.
	if webhook := strings.TrimSpace(input.Webhook); webhook != "" {
		channel.Config.Webhook = webhook
	}
	if secret := strings.TrimSpace(input.Secret); secret != "" {
		channel.Config.Secret = secret
	}
	if token := strings.TrimSpace(input.BotToken); token != "" {
		channel.Config.BotToken = token
	}
	channels[index] = channel
	if err := cfg.SaveChannels(channels); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "保存渠道失败")
		return
	}
	writeJSON(w, http.StatusOK, channelView(channel))
}

func (s *Server) handleMonitorDeleteChannel(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	channelID := strings.TrimSpace(r.PathValue("id"))
	cfg := monitoring.NewConfig(s.monitoringStore)
	channels := cfg.Channels()
	index := monitorChannelIndex(channels, channelID)
	if index < 0 {
		writeOpenAIError(w, http.StatusNotFound, "渠道不存在")
		return
	}
	channels = append(channels[:index], channels[index+1:]...)
	if err := cfg.SaveChannels(channels); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "删除渠道失败")
		return
	}
	writeJSON(w, http.StatusOK, channelViews(channels))
}

func (s *Server) handleMonitorTestChannel(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	channelID := strings.TrimSpace(r.PathValue("id"))
	cfg := monitoring.NewConfig(s.monitoringStore)
	channel, found := monitorChannelByID(cfg.Channels(), channelID)
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "渠道不存在")
		return
	}
	adapter, known := monitoring.AdapterFor(channel.Type)
	if !known {
		writeOpenAIError(w, http.StatusBadRequest, "不支持的渠道类型")
		return
	}
	result := s.monitorSend(r.Context(), adapter, channel, monitoring.Notification{
		Kind:     "test",
		Severity: monitoring.SeverityInfo,
		Title:    "监控中心测试消息",
		Markdown: "这是一条来自监控中心的测试消息，收到即表示渠道配置正确。",
		At:       time.Now().UTC(),
	})
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleMonitorRetryPush(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	eventID, ok := monitorPathEventID(w, r)
	if !ok {
		return
	}
	var body struct {
		ChannelID string `json:"channelId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "渠道参数格式错误")
		return
	}
	event, found, err := s.monitoringStore.MonitorEventByID(eventID)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "读取事件失败")
		return
	}
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "事件不存在")
		return
	}
	cfg := monitoring.NewConfig(s.monitoringStore)
	channel, exists := monitorChannelByID(cfg.Channels(), strings.TrimSpace(body.ChannelID))
	if !exists {
		writeOpenAIError(w, http.StatusNotFound, "渠道不存在")
		return
	}
	adapter, known := monitoring.AdapterFor(channel.Type)
	if !known {
		writeOpenAIError(w, http.StatusBadRequest, "不支持的渠道类型")
		return
	}
	// Rebuild the original message from the alert state so a retry reads exactly
	// like the send that failed.
	state, hasState := s.monitorStateForFingerprint(event.Fingerprint)
	if !hasState {
		state = monitoring.AlertState{
			Fingerprint: event.Fingerprint, Severity: event.Severity,
			TargetType: event.TargetType, TargetID: event.TargetID, TargetName: event.TargetName,
		}
	}
	message := monitoring.BuildNotification(state, event, cfg.Dispatch().BaseURL, cfg.Dispatch().Location())
	result := s.monitorSend(r.Context(), adapter, channel, message)
	push := monitoring.EventPush{
		EventID: eventID, ChannelID: channel.ID, Attempts: 1,
		Status: monitoring.PushSent, UpdatedAt: time.Now().UTC(),
	}
	if errorText, _ := result["error"].(string); errorText != "" {
		push.Status = monitoring.PushFailed
		push.LastError = errorText
	}
	if err := s.monitoringStore.UpsertMonitorEventPush(push); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "记录推送失败")
		return
	}
	pushes, err := s.monitoringStore.ListMonitorEventPushes(eventID)
	if err == nil {
		if status, firstError, aggregate := monitoring.AggregatePushStatus(pushes); aggregate {
			_ = s.monitoringStore.UpdateMonitorEventPush(eventID, status, firstError)
		}
	}
	if refreshed, found, err := s.monitoringStore.MonitorEventByID(eventID); err == nil && found {
		result["pushStatus"] = refreshed.PushStatus
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleMonitorDispatch(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, monitoring.NewConfig(s.monitoringStore).Dispatch())
}

func (s *Server) handleMonitorUpdateDispatch(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	cfg := monitoring.NewConfig(s.monitoringStore)
	current := cfg.Dispatch()
	var input struct {
		DigestMinutes  *int    `json:"digestMinutes"`
		QuietEnabled   *bool   `json:"quietEnabled"`
		QuietStart     *string `json:"quietStart"`
		QuietEnd       *string `json:"quietEnd"`
		RecoveryNotify *bool   `json:"recoveryNotify"`
		Timezone       *string `json:"timezone"`
		BaseURL        *string `json:"baseUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "推送配置格式错误")
		return
	}
	updated := current
	if input.DigestMinutes != nil {
		// P1 摘要间隔：越界直接 400，不进入 SaveDispatch，库里保持原值。
		if !monitoring.ValidP1Digest(*input.DigestMinutes) {
			writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("合并窗口需在 %d 到 %d 分钟之间",
				monitoring.P1DigestMinMinutes, monitoring.P1DigestMaxMinutes))
			return
		}
		updated.DigestMinutes = *input.DigestMinutes
	}
	if input.QuietEnabled != nil {
		updated.QuietEnabled = *input.QuietEnabled
	}
	if input.QuietStart != nil {
		updated.QuietStart = strings.TrimSpace(*input.QuietStart)
	}
	if input.QuietEnd != nil {
		updated.QuietEnd = strings.TrimSpace(*input.QuietEnd)
	}
	for _, clock := range []string{updated.QuietStart, updated.QuietEnd} {
		if clock != "" {
			if _, err := time.Parse("15:04", clock); err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "静默时间格式应为 HH:MM")
				return
			}
		}
	}
	if input.RecoveryNotify != nil {
		updated.RecoveryNotify = *input.RecoveryNotify
	}
	if input.Timezone != nil {
		zone := strings.TrimSpace(*input.Timezone)
		if zone != "" {
			if _, err := time.LoadLocation(zone); err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "无效时区")
				return
			}
		}
		updated.Timezone = zone
	}
	if updated.Timezone == "" {
		updated.Timezone = "Asia/Shanghai"
	}
	if input.BaseURL != nil {
		updated.BaseURL = strings.TrimRight(strings.TrimSpace(*input.BaseURL), "/")
	}
	if err := cfg.SaveDispatch(updated); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "保存推送配置失败")
		return
	}
	writeJSON(w, http.StatusOK, cfg.Dispatch())
}

// ── 规则 ────────────────────────────────────────────────────────────────

// monitorRuleView 是规则唯一的线上形状：定义（名称、默认参数、检查间隔）与
// 当前配置合成一行。Interval 只以分钟输出——纳秒时长既不该泄漏，前端也用不上。
// Params 永不省略：规则页按它渲染每一行的参数格，缺省为空对象而不是 null。
type monitorRuleView struct {
	Key             string              `json:"key"`
	Name            string              `json:"name"`
	TargetType      string              `json:"targetType"`
	Enabled         bool                `json:"enabled"`
	Severity        monitoring.Severity `json:"severity"`
	CooldownMinutes int                 `json:"cooldownMinutes"`
	IntervalMinutes int                 `json:"intervalMinutes"`
	Params          map[string]float64  `json:"params"`
	DefaultParams   map[string]float64  `json:"defaultParams"`
	Consecutive     bool                `json:"consecutive"`
}

func monitorRuleViews(rules []monitoring.RuleConfig) []monitorRuleView {
	byKey := make(map[string]monitoring.RuleConfig, len(rules))
	for _, rule := range rules {
		byKey[rule.Key] = rule
	}
	out := make([]monitorRuleView, 0, len(rules))
	for _, key := range monitoring.SortedRuleKeys(rules) {
		def, ok := monitoring.RuleDefinition(key)
		if !ok {
			continue
		}
		rule := byKey[key]
		params := rule.Params
		if params == nil {
			params = map[string]float64{}
		}
		defaults := make(map[string]float64, len(def.DefaultParams))
		for k, v := range def.DefaultParams {
			defaults[k] = v
		}
		out = append(out, monitorRuleView{
			Key: key, Name: def.Name, TargetType: def.TargetType,
			Enabled: rule.Enabled, Severity: rule.Severity, CooldownMinutes: rule.CooldownMinutes,
			IntervalMinutes: int(def.Interval / time.Minute), Params: params, DefaultParams: defaults,
			Consecutive: def.Consecutive,
		})
	}
	return out
}

// monitorRuleRequest 是 PUT 的请求体。指针字段把「没给」和「给了零值」分开：
// 没给的条目整条恢复默认，给了一半的条目只覆盖给了的字段。
type monitorRuleRequest struct {
	Key             string             `json:"key"`
	Enabled         *bool              `json:"enabled"`
	Severity        *string            `json:"severity"`
	CooldownMinutes *int               `json:"cooldownMinutes"`
	Params          map[string]float64 `json:"params"`
}

// normalizeRuleRequests 按 NormalizeRules 的「缺项=默认」语义补齐，但先做两项它
// 不做的事：空参数取定义默认值（而不是保留旧值），未知规则 key 不吞（否则 PUT
// 会「成功」却没生效），重复 key 报错。
func normalizeRuleRequests(incoming []monitorRuleRequest) ([]monitoring.RuleConfig, error) {
	normalized := monitoring.NormalizeRules(nil)
	byKey := make(map[string]*monitoring.RuleConfig, len(normalized))
	for i := range normalized {
		byKey[normalized[i].Key] = &normalized[i]
	}
	for _, in := range incoming {
		key := strings.TrimSpace(in.Key)
		rule, ok := byKey[key]
		if !ok {
			return nil, fmt.Errorf("unknown rule %q", in.Key)
		}
		if in.Enabled != nil {
			rule.Enabled = *in.Enabled
		}
		if in.Severity != nil {
			rule.Severity = monitoring.Severity(strings.ToLower(strings.TrimSpace(*in.Severity)))
		}
		if in.CooldownMinutes != nil {
			rule.CooldownMinutes = *in.CooldownMinutes
		}
		if in.Params != nil {
			rule.Params = make(map[string]float64, len(in.Params))
			for k, v := range in.Params {
				rule.Params[k] = v
			}
		}
	}
	// NormalizeRules 合并重复 key 时后到者覆盖先到者，ValidateRules 因此看不到重复；
	// 这里补一刀，免得同一规则在整表里出现两次还「成功」。
	keys := make([]string, 0, len(incoming))
	for _, in := range incoming {
		keys = append(keys, strings.TrimSpace(in.Key))
	}
	sort.Strings(keys)
	for i := 1; i < len(keys); i++ {
		if keys[i] == keys[i-1] {
			return nil, fmt.Errorf("duplicate rule %q", keys[i])
		}
	}
	return normalized, nil
}

func (s *Server) handleMonitorRules(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, monitorRuleViews(monitoring.NewConfig(s.monitoringStore).Rules()))
}

// handleMonitorUpdateRules 整表替换：补齐 → 校验 → 落库。任何一步失败都 400 且
// 不写库；服务端校验是唯一防线，前端校验只算体验。返回落库后的规范化结果，调用方
// 据此看到哪些条目被补成了默认值。
func (s *Server) handleMonitorUpdateRules(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	var incoming []monitorRuleRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&incoming); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "规则配置格式不正确")
		return
	}
	// [] 与 null 补齐后会变成整表默认值：前端一个 bug 就能静默清空用户全部自定义
	// 规则。真要全恢复默认，由调用方显式发 18 条默认值。
	if len(incoming) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "规则列表为空")
		return
	}
	rules, err := normalizeRuleRequests(incoming)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := monitoring.ValidateRules(rules); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := monitoring.NewConfig(s.monitoringStore)
	if err := cfg.SaveRules(rules); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "规则配置保存失败")
		return
	}
	// 运行中的引擎下一拍会自己读到新值（Service.reloadConfigIfChanged），这里不碰
	// 状态机。
	writeJSON(w, http.StatusOK, monitorRuleViews(cfg.Rules()))
}

// ---- shared pieces ---------------------------------------------------------

// monitorSend delivers one notification and reports the outcome the console
// shows. Adapter errors are already scrubbed of the channel's own secrets by
// the adapters in internal/monitoring.
func (s *Server) monitorSend(ctx context.Context, adapter monitoring.ChannelAdapter, channel monitoring.Channel, message monitoring.Notification) map[string]any {
	sendCtx, cancel := context.WithTimeout(ctx, monitorProbeTimeout)
	defer cancel()
	started := time.Now()
	err := adapter.Send(sendCtx, channel, message)
	result := map[string]any{
		"ok":        err == nil,
		"channelId": channel.ID,
		"latencyMs": time.Since(started).Milliseconds(),
	}
	if err != nil {
		result["status"] = monitoring.PushFailed
		result["error"] = truncateMonitorError(err.Error())
		return result
	}
	result["status"] = monitoring.PushSent
	return result
}

func truncateMonitorError(text string) string {
	runes := []rune(text)
	if len(runes) > 500 {
		return string(runes[:500]) + "..."
	}
	return text
}

func (s *Server) monitorProviders() []domain.Provider {
	state := s.router.State()
	out := make([]domain.Provider, 0, len(state.Providers))
	for _, provider := range state.Providers {
		if provider.Deleted {
			continue
		}
		// The persisted health column only says what the last probe wrote; a live
		// upstream failure lives in the in-memory availability tracker (the
		// console overlays it the same way). Without this the account matrix would
		// still say 未检测 while the Providers page shows 异常.
		s.applyProviderAvailability(&provider)
		out = append(out, provider)
	}
	return out
}

func (s *Server) monitorFiringAlerts() []monitoring.AlertView {
	page, err := s.monitoringStore.QueryMonitorAlerts(monitoring.AlertQuery{
		Status: monitoring.StatusFiring, Page: 1, PageSize: monitorAlertScanLimit,
	})
	if err != nil {
		return nil
	}
	return page.Items
}

func (s *Server) monitorAccountViewsOf(providers []domain.Provider) []monitorAccountView {
	now := time.Now().UTC()
	samples := map[string]monitoring.MetricSample{}
	if latest, err := s.monitoringStore.LatestProviderMetricSamples(); err == nil {
		samples = latest
	}
	staleAfter := time.Duration(monitoring.NewConfig(s.monitoringStore).Probe().QuotaStaleMinutes) * time.Minute
	alertCounts := map[string]int{}
	for _, alert := range s.monitorFiringAlerts() {
		if alert.TargetType == monitoring.TargetProvider {
			alertCounts[alert.TargetID]++
		}
	}
	isolation := s.monitorIsolationMarks()
	out := make([]monitorAccountView, 0, len(providers))
	for _, provider := range providers {
		view := monitorAccountView{
			ProviderID: provider.ID, Name: provider.Name, Vendor: providerVendorKey(provider),
			AuthType: provider.AuthType, Disabled: provider.Disabled,
			HealthStatus: provider.HealthStatus,
			NextRetryAt:  provider.NextRetryAt,
			Capabilities: monitoring.Capabilities{
				OAuth:   providerHasExpiry(provider),
				Quota:   providerHasQuotaHandler(provider),
				Models:  providerHasModelSync(provider),
				Checkin: providerHasCheckin(provider),
			},
			ActiveAlerts:  alertCounts[provider.ID],
			ModelFailures: s.monitorModelFailures(provider.ID),
		}
		// 隔离标记只在账号确实停用时才挂出去：手工启用过的历史标记不能把卡片
		// 画成「已隔离」。标记读取失败按「没隔离」处理，不影响列表。
		if provider.Disabled {
			if mark, isolated := isolation[provider.ID]; isolated {
				view.Isolated, view.IsolatedAt, view.IsolatedBy = true, mark.At, mark.By
			}
		}
		if expiry := providerCredentialExpiry(provider); !expiry.IsZero() {
			view.ExpiresAt = expiry.UTC().Format(time.RFC3339)
		}
		if renewal, err := s.monitoringStore.RecentRenewalOutcomes(provider.ID, 10); err == nil && renewal.Known {
			value := renewal
			view.Renewal = &value
		}
		if sample, ok := samples[provider.ID]; ok {
			view.Quota = monitorQuotaFromSample(sample, staleAfter, now)
		}
		out = append(out, view)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) monitorModelFailures(providerID string) int {
	if s.monitoringSrc == nil {
		return 0
	}
	return s.monitoringSrc.modelFailureCount(providerID)
}

// monitorQuotaFromSample turns the last sampled row into the snapshot the
// console renders. An absent ratio stays absent: "unknown" and "zero left" are
// different answers and must not look the same.
func monitorQuotaFromSample(sample monitoring.MetricSample, staleAfter time.Duration, now time.Time) *monitoring.QuotaSnapshot {
	stale := sample.ObservedAt.IsZero() || (staleAfter > 0 && now.Sub(sample.ObservedAt) > staleAfter)
	snapshot := &monitoring.QuotaSnapshot{
		ObservedAt: sample.ObservedAt,
		Error:      sample.SourceError,
		Ratio:      -1,
		Stale:      stale,
		Windows:    sample.QuotaWindows,
	}
	if sample.QuotaRatio != nil {
		snapshot.Ratio = *sample.QuotaRatio
		snapshot.Error = ""
	}
	if snapshot.Error == "" && snapshot.Ratio < 0 {
		return nil
	}
	return snapshot
}

func (s *Server) monitorAlertView(state monitoring.AlertState) map[string]any {
	ids, err := s.monitoringStore.MonitorEventIDsByFingerprint(state.Fingerprint, monitorRelatedEvents)
	if err != nil {
		ids = []int64{}
	}
	view := monitoring.AlertView{AlertState: state, RelatedEventIDs: ids}
	if len(ids) > 0 {
		if event, found, err := s.monitoringStore.MonitorEventByID(ids[0]); err == nil && found {
			view.LatestEventID = event.ID
			view.PushStatus = event.PushStatus
			view.PushError = event.PushError
			return map[string]any{"alert": view, "latestEvent": event}
		}
	}
	return map[string]any{"alert": view, "latestEvent": nil}
}

func (s *Server) monitorStateForFingerprint(fingerprint string) (monitoring.AlertState, bool) {
	if fingerprint == "" {
		return monitoring.AlertState{}, false
	}
	states, err := s.monitoringStore.LoadMonitorStates()
	if err != nil {
		return monitoring.AlertState{}, false
	}
	for _, state := range states {
		if state.Fingerprint == fingerprint {
			return state, true
		}
	}
	return monitoring.AlertState{}, false
}

// renewCredentialForMonitor routes a forced renewal to the vendor's own
// refresh path. Every one of them is the same function a live request uses, so
// the token rotation rules (and the per-token locks) are never duplicated here.
func (s *Server) renewCredentialForMonitor(ctx context.Context, provider domain.Provider) (domain.Provider, error) {
	switch provider.AuthType {
	case domain.AuthTypeClaudeOAuth:
		// Claude's stored expiry is the refresh-token lifetime; the console's
		// explicit request is the one case that must renew regardless of it.
		return s.ensureFreshClaudeTokenWithin(provider, 365*24*time.Hour)
	case domain.AuthTypeChatGPTOAuth:
		return s.ensureFreshChatGPTToken(provider)
	case domain.AuthTypeCursorOAuth:
		return s.ensureFreshCursorToken(provider)
	case domain.AuthTypeQoderPAT, domain.AuthTypeQoderCN:
		return s.ensureFreshQoderToken(provider)
	case domain.AuthTypeWorkBuddy, domain.AuthTypeWorkBuddyAI:
		return s.ensureFreshWorkBuddyToken(provider)
	case domain.AuthTypeTraeCN, domain.AuthTypeTraeGlobal:
		return s.ensureFreshTraeToken(provider)
	default:
		return provider, fmt.Errorf("provider %q has no renewable credential", provider.ID)
	}
}

func providerAccessToken(p domain.Provider) string {
	switch {
	case p.ClaudeOAuth != nil:
		return p.ClaudeOAuth.AccessToken
	case p.ChatGPTOAuth != nil:
		return p.ChatGPTOAuth.AccessToken
	case p.CursorOAuth != nil:
		return p.CursorOAuth.AccessToken
	case p.QoderPAT != nil:
		return p.QoderPAT.AccessToken
	case p.WorkBuddy != nil:
		return p.WorkBuddy.AccessToken
	case p.Trae != nil:
		return p.Trae.AccessToken
	}
	return ""
}

func monitorChannelIndex(channels []monitoring.Channel, id string) int {
	for index, channel := range channels {
		if channel.ID == id {
			return index
		}
	}
	return -1
}

func monitorChannelByID(channels []monitoring.Channel, id string) (monitoring.Channel, bool) {
	if id == "" {
		return monitoring.Channel{}, false
	}
	for _, channel := range channels {
		if channel.ID == id {
			return channel, true
		}
	}
	return monitoring.Channel{}, false
}

func monitorSeverity(raw string, fallback monitoring.Severity) monitoring.Severity {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(monitoring.SeverityP0):
		return monitoring.SeverityP0
	case string(monitoring.SeverityP1):
		return monitoring.SeverityP1
	case string(monitoring.SeverityP2):
		return monitoring.SeverityP2
	case string(monitoring.SeverityInfo):
		return monitoring.SeverityInfo
	default:
		return fallback
	}
}

func monitorIntParam(r *http.Request, key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get(key)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func monitorPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id <= 0 {
		writeOpenAIError(w, http.StatusBadRequest, "无效的告警编号")
		return 0, false
	}
	return id, true
}

func monitorPathEventID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("eventId")), 10, 64)
	if err != nil || id <= 0 {
		writeOpenAIError(w, http.StatusBadRequest, "无效的事件编号")
		return 0, false
	}
	return id, true
}

// monitorTimeRange reads either an explicit from/to pair or a relative range.
func monitorTimeRange(r *http.Request, fallback time.Duration) (time.Time, time.Time) {
	to := time.Now().UTC()
	if raw := strings.TrimSpace(r.URL.Query().Get("to")); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			to = parsed.UTC()
		}
	}
	from := to.Add(-fallback)
	if raw := strings.TrimSpace(r.URL.Query().Get("from")); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			from = parsed.UTC()
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("range")); raw != "" {
		if span, ok := monitorParseSpan(raw); ok {
			from = to.Add(-span)
		}
	}
	return from, to
}

func monitorParseSpan(raw string) (time.Duration, bool) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	if strings.HasSuffix(normalized, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(normalized, "d"))
		if err == nil && days > 0 {
			return time.Duration(days) * 24 * time.Hour, true
		}
	}
	if strings.HasSuffix(normalized, "w") {
		weeks, err := strconv.Atoi(strings.TrimSuffix(normalized, "w"))
		if err == nil && weeks > 0 {
			return time.Duration(weeks) * 7 * 24 * time.Hour, true
		}
	}
	span, err := time.ParseDuration(normalized)
	if err != nil || span <= 0 {
		return 0, false
	}
	return span, true
}

func monitorSampleTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}

func roundRatio(ratio float64) float64 {
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return 0
	}
	return float64(int(ratio*10000+0.5)) / 10000
}

// ── 探测 ────────────────────────────────────────────────────────────────

// monitorProbeAccount 是探测页的账号行。Probed=false 表示这个账号还从来没探过，
// 界面要能说「暂无数据」，不能把零值显示成「0 次失败 = 健康」。
type monitorProbeAccount struct {
	ProviderID          string    `json:"providerId"`
	Name                string    `json:"name"`
	Vendor              string    `json:"vendor,omitempty"`
	Disabled            bool      `json:"disabled"`
	Probed              bool      `json:"probed"`
	ConsecutiveFailures int       `json:"consecutiveFailures"`
	LastProbeAt         time.Time `json:"lastProbeAt,omitempty"`
	LastStatus          int       `json:"lastStatus,omitempty"`
	Skipped             string    `json:"skipped,omitempty"`
	UsedToday           int       `json:"usedToday"`
	MaxPerDay           int       `json:"maxPerDay"`
}

func (s *Server) handleMonitorProbes(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	snapshot, err := s.monitorProbeSnapshotOf(s.monitorProviders())
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "探测记录读取失败")
		return
	}
	// The snapshot's run list is the newest across every provider; a
	// providerId filter narrows it, which is what the per-account drawer reads.
	if providerID := strings.TrimSpace(r.URL.Query().Get("providerId")); providerID != "" {
		runs, err := s.monitoringStore.LatestMonitorProbeRuns(providerID, 50)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "探测记录读取失败")
			return
		}
		snapshot.Runs = runs
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": snapshot.Accounts, "runs": snapshot.Runs, "config": snapshot.Config})
}

// handleMonitorProbeRun 手动跑一发，用于排障。同账号已有探测在跑时回 409 而不是
// 悄悄排队：运维点是「现在就确认」，排队只会让他以为没生效再点一次。
func (s *Server) handleMonitorProbeRun(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	var in struct {
		ProviderID string `json:"providerId"`
		Kind       string `json:"kind"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil || strings.TrimSpace(in.ProviderID) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "providerId 必填")
		return
	}
	kind := strings.TrimSpace(in.Kind)
	if kind == "" {
		kind = probeKindLight
	}
	if kind != probeKindLight {
		// 只剩轻探测一种。已撤下的 kind（如 cli）回 400 kind_unavailable，
		// 不装作没看见——旧界面/脚本据此知道该入口不存在了。
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "errorKind": "kind_unavailable"})
		return
	}
	if _, err := s.router.ProviderByID(in.ProviderID); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "Provider 不存在或已删除")
		return
	}
	run := s.runLightProbe(r.Context(), in.ProviderID)
	body := map[string]any{
		"ok":         !run.Failed(),
		"providerId": run.ProviderID,
		"kind":       run.Kind,
		"at":         run.At,
		"cases":      run.Cases,
		"skipped":    run.Skipped,
	}
	if run.Skipped == "in_flight" {
		writeJSON(w, http.StatusConflict, body)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleMonitorProbeConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, monitoring.NewConfig(s.monitoringStore).Probe())
}

// handleMonitorProbeConfigUpdate 只校验会烧额度的那几个字段：上限、间隔、窗口。
// 窗口写坏会让「每天一次」变成全天候发探测，所以宁可拒掉也不猜。
func (s *Server) handleMonitorProbeConfigUpdate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.monitorGate(w, r); !ok {
		return
	}
	var incoming monitoring.ProbeConfig
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&incoming); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "配置格式不正确")
		return
	}
	if incoming.EventDriven.MaxPerDayPerAccount < 0 || incoming.EventDriven.MaxPerDayPerAccount > 24 {
		writeOpenAIError(w, http.StatusBadRequest, "每账号每日探测次数要在 0-24 之间（0 表示不限）")
		return
	}
	if incoming.EventDriven.MinIntervalMinutes < 0 || incoming.EventDriven.MinIntervalMinutes > 1440 {
		writeOpenAIError(w, http.StatusBadRequest, "最小间隔要在 0-1440 分钟之间")
		return
	}
	if incoming.EventDriven.IdleMinutes < 0 || incoming.EventDriven.IdleMinutes > 1440 {
		writeOpenAIError(w, http.StatusBadRequest, "闲置阈值要在 0-1440 分钟之间")
		return
	}
	if _, err := clockMinutes(incoming.Daily.WindowStart); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "每日窗口开始时间格式应为 HH:MM")
		return
	}
	if _, err := clockMinutes(incoming.Daily.WindowEnd); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "每日窗口结束时间格式应为 HH:MM")
		return
	}
	if err := monitoring.NewConfig(s.monitoringStore).SaveProbe(incoming); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "配置保存失败")
		return
	}
	writeJSON(w, http.StatusOK, monitoring.NewConfig(s.monitoringStore).Probe())
}
