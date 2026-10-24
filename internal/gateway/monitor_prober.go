package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"github.com/luca/llm-protocol-gateway/internal/monitoring"
)

const (
	// lightProbeTimeout 是一发轻探测的上限。慢到 30 秒以上的账号交给 TTFT 规则
	// 去盯，探测只回答「还能不能用」。
	lightProbeTimeout = 30 * time.Second
	// monitorProbeMaxTokens 刻意给到最小：探测不该消耗订阅额度。
	monitorProbeMaxTokens = 16
	probeKindLight        = "light"
	probeCaseChat         = "chat"
	probeChatPrompt       = "reply with the single word ok"
)

// probeCaseResult 是一发探测的结论。Status 为 0 表示传输层失败（连不上/超时）——
// 它同样算失败，只是没法按状态码归类。
type probeCaseResult struct {
	Case     string
	OK       bool
	Status   int
	Duration int64
	Model    string
	Error    string
}

// probeRun 是一个账号一轮探测的结果。Skipped 非空表示这一轮根本没跑成
// （账号没了/没配模型/已有一发在飞），它不算失败——把「没跑成」记成失败会让
// 探测自己的调度问题变成账号告警。
type probeRun struct {
	ProviderID   string
	ProviderName string
	Kind         string
	At           time.Time
	Cases        []probeCaseResult
	Skipped      string
}

func (r probeRun) Failed() bool {
	if r.Skipped != "" || len(r.Cases) == 0 {
		return false
	}
	for _, item := range r.Cases {
		if !item.OK {
			return true
		}
	}
	return false
}

// AuthFailure 区分「凭证废了」和「上游抖了一下」：401/403 正是 probe_failed
// 要抬到 P0 的那一类。
func (r probeRun) AuthFailure() bool {
	for _, item := range r.Cases {
		if item.Status == http.StatusUnauthorized || item.Status == http.StatusForbidden {
			return true
		}
	}
	return false
}

// LastStatus 取第一个非零状态码，供规则与界面显示「上游到底回了什么」。
func (r probeRun) LastStatus() int {
	for _, item := range r.Cases {
		if item.Status != 0 {
			return item.Status
		}
	}
	return 0
}

func lightProbeModel(provider domain.Provider) string {
	if model := strings.TrimSpace(provider.DefaultModel); model != "" {
		return model
	}
	for _, model := range provider.Models {
		if model.InMenu && strings.TrimSpace(model.ID) != "" {
			return strings.TrimSpace(model.ID)
		}
	}
	return ""
}

// runLightProbe 对一个账号发一轮最小 chat 探测。
//
// 它刻意走 chat-test 而不是 /v1 数据面：chat-test 不写 request_logs，所以既不进
// 账号成功率也不进成本账本。与额度保活共用同一把单飞锁，避免「保活一发 + 监控一发」
// 同时打在同一账号上——那既多花额度又像滥用。
func (s *Server) runLightProbe(ctx context.Context, providerID string) probeRun {
	run := probeRun{ProviderID: providerID, Kind: probeKindLight, At: time.Now().UTC()}
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		run.Skipped = "provider_missing"
		return run
	}
	run.ProviderName = provider.Name
	if provider.Disabled {
		run.Skipped = "provider_disabled"
		return run
	}
	model := lightProbeModel(provider)
	if model == "" {
		run.Skipped = "no_model"
		return run
	}
	if !s.claimQuotaProbe(providerID) {
		run.Skipped = "in_flight"
		return run
	}
	defer s.releaseQuotaProbe(providerID)

	run.Cases = append(run.Cases, s.probeChatCase(ctx, providerID, model))
	s.recordProbeRun(run)
	return run
}

func (s *Server) probeChatCase(ctx context.Context, providerID, model string) probeCaseResult {
	started := time.Now().UTC()
	req := providerChatTestRequest{
		Model:      model,
		Message:    probeChatPrompt,
		UserPrompt: probeChatPrompt,
		MaxTokens:  monitorProbeMaxTokens,
	}
	callCtx, cancel := context.WithTimeout(ctx, lightProbeTimeout)
	defer cancel()
	request, err := quotaProbeHTTPRequest(callCtx, providerID, req)
	if err != nil {
		return probeCaseResult{Case: probeCaseChat, Status: 0, Duration: time.Since(started).Milliseconds(), Error: err.Error()}
	}
	result, _ := s.testProviderChat(request, providerID, req, started)
	status := mapIntValue(result, "status")
	ok, _ := result["success"].(bool)
	caseResult := probeCaseResult{
		Case:     probeCaseChat,
		OK:       ok,
		Status:   status,
		Duration: mapInt64Value(result, "latencyMs"),
		Model:    mapStringValue(result, "model"),
	}
	if !ok {
		caseResult.Error = truncateRunReason(mapStringValue(result, "error"))
	}
	return caseResult
}

// monitorProbeDay 按投递时区给出探测所属的自然日。当日次数上限与每日窗口都以它
// 为准——按 UTC 算的话，+08:00 的部署要到早上 8 点才"新的一天"，配额会白白少给一天。
func monitorProbeLocation(s *Server) *time.Location {
	if s != nil && s.monitoringStore != nil {
		if configured := monitoring.NewConfig(s.monitoringStore).Dispatch().Location(); configured != nil {
			return configured
		}
	}
	return time.UTC
}

func monitorProbeDay(s *Server, now time.Time) string {
	return now.In(monitorProbeLocation(s)).Format("2006-01-02")
}

// persistProbeRun 把每一发探测落库。库里存的是「跑成了什么」，规则与调度都只读库，
// 所以重启不会把连续失败计数和当日配额清零。
func (s *Server) persistProbeRun(run probeRun) {
	if s.monitoringStore == nil || len(run.Cases) == 0 {
		return
	}
	day := monitorProbeDay(s, run.At)
	for _, item := range run.Cases {
		errorCode := item.Error
		if len(errorCode) > 200 {
			errorCode = errorCode[:200]
		}
		if err := s.monitoringStore.InsertMonitorProbeRun(monitoring.ProbeRunRecord{
			ProviderID: run.ProviderID, Kind: run.Kind, Case: item.Case, OK: item.OK,
			Status: item.Status, DurationMs: item.Duration, Model: item.Model,
			ErrorCode: errorCode, Day: day, Time: run.At,
		}); err != nil {
			slog.Warn("monitoring probe run persist failed", "provider", run.ProviderID, "error", err)
			return
		}
	}
}

// recordProbeRun 把一轮探测写进事件流。成功与失败都只写 info 事件：成级由
// probe_failed 规则按连续次数决定，探测自己不该报警。
func (s *Server) recordProbeRun(run probeRun) {
	if s.monitoringStore == nil {
		return
	}
	s.persistProbeRun(run)
	name := run.ProviderName
	if name == "" {
		name = run.ProviderID
	}
	cases := make([]map[string]any, 0, len(run.Cases))
	for _, item := range run.Cases {
		entry := map[string]any{"case": item.Case, "ok": item.OK, "durationMs": item.Duration}
		if item.Status != 0 {
			entry["status"] = item.Status
		}
		if item.Model != "" {
			entry["model"] = item.Model
		}
		if item.Error != "" {
			entry["error"] = item.Error
		}
		cases = append(cases, entry)
	}
	title := name + " 轻探测通过"
	if run.Failed() {
		title = name + " 轻探测失败"
	}
	if _, err := s.monitoringStore.InsertMonitorEvent(monitoring.Event{
		Time: run.At, Kind: monitoring.EventKindProbe, Severity: monitoring.SeverityInfo,
		TargetType: monitoring.TargetProvider, TargetID: run.ProviderID, TargetName: name,
		Title: title, Status: monitoring.StatusInfo,
		Detail: map[string]any{
			"kind":    run.Kind,
			"ok":      !run.Failed(),
			"skipped": run.Skipped,
			"cases":   cases,
		},
	}); err != nil {
		// 探测记录失败不影响探测本身，也不能把账号判成不可用。
		slog.Warn("monitoring probe event failed", "provider", run.ProviderID, "error", err)
	}
}

const (
	// probeTickInterval 每分钟看一次探测决策；真正发出去的节奏由下面的间隔与
	// 当日配额封顶，tick 密不等于探测密。
	probeTickInterval = time.Minute
	// probeFailureRetryWindow 是「账号被判不可用后补一发」的最小间隔。可用性标记
	// 会被重探循环反复命中，不限流就会对同一个坏账号连环打。
	probeFailureRetryWindow = 10 * time.Minute
	// probeDailyBatchPerTick 限制每日窗口内一个 tick 最多补几发：20 个账号一次串完
	// 会把这个 tick 拖好几分钟。分批就自然顺延到窗口结束，也不会跨日堆积。
	probeDailyBatchPerTick = 2
	// probeDailyLookback 是「这个账号到底有没有人在用」的观察窗。
	probeDailyLookback = 24 * time.Hour
)

// probeGate 是「这一发该不该打」的输入。做成纯函数好测：调度竞态与策略分开，
// 竞态（同账号已有探测在飞）由 claimQuotaProbe 在真正发之前守住。
type probeGate struct {
	Enabled           bool
	Now               time.Time
	Day               string
	UsedToday         int
	MaxPerDay         int
	LastProbeAt       time.Time
	MinInterval       time.Duration
	LastRealSuccess   time.Time
	Idle              time.Duration
	MarkedUnavailable bool
}

func (g probeGate) budgetSpent() bool {
	return g.MaxPerDay > 0 && g.UsedToday >= g.MaxPerDay
}

// allowEventDriven 判规格 §9.2 的六个条件。没人在用的账号不催：探测的目的是
// 「他明天要用时别是坏的」，不是替一个闲置账号刷存在感。
func (g probeGate) allowEventDriven() bool {
	if !g.Enabled || g.budgetSpent() {
		return false
	}
	if g.MarkedUnavailable {
		// 连续失败要立刻补一发：它必须排在常规最小间隔之前，否则 6 小时的
		// 节流会把「失败后马上确认一下」这条永远挡死。当日配额照旧生效。
		return g.LastProbeAt.IsZero() || g.Now.Sub(g.LastProbeAt) >= probeFailureRetryWindow
	}
	if !g.LastProbeAt.IsZero() && g.Now.Sub(g.LastProbeAt) < g.MinInterval {
		return false
	}
	if g.LastRealSuccess.IsZero() {
		return false
	}
	return g.Now.Sub(g.LastRealSuccess) >= g.Idle
}

// allowDaily 判每日轮：今天还没跑成才补。它和事件驱动共用同一个当日计数，
// 所以一个账号早上被事件驱动催过，晚上那一轮就不会再花一次额度。
func (g probeGate) allowDaily() bool {
	return g.Enabled && !g.budgetSpent() && g.UsedToday == 0
}

// withinClockWindow 判 now 是否落在 [start, end) 的时钟窗口里；start > end 视为
// 跨零点。解析失败一律判不在窗口内——配置写错时宁可少跑，不要全天候发探测。
func withinClockWindow(start, end string, now time.Time) bool {
	from, errFrom := clockMinutes(start)
	to, errTo := clockMinutes(end)
	if errFrom != nil || errTo != nil {
		return false
	}
	minutes := now.Hour()*60 + now.Minute()
	if from <= to {
		return minutes >= from && minutes < to
	}
	return minutes >= from || minutes < to
}

// clockMinutes 把 "HH:MM" 换成当天第几分钟。parseHourMinute 不校验范围，
// 而 "25:00" 这种坏值一旦放过就会把窗口撑成全天——配置写错时宁可一次都不跑。
func clockMinutes(value string) (int, error) {
	hour, minute, err := parseHourMinute(value)
	if err != nil {
		return 0, err
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("clock out of range: %q", value)
	}
	return hour*60 + minute, nil
}

func (s *Server) liveProbeTargets() []domain.Provider {
	state := s.router.State()
	out := make([]domain.Provider, 0, len(state.Providers))
	for _, provider := range state.Providers {
		if provider.Deleted || provider.Disabled {
			continue
		}
		if lightProbeModel(provider) == "" {
			continue
		}
		out = append(out, provider)
	}
	return out
}

// lastRealSuccessAt 返回该账号最近一次真实成功请求的时间。轻探测走 chat-test
// 不写请求日志，所以查到的天然是真实流量；CLI 深检的行带 monitor_probe 标记，
// 万一被查到这里也只会让账号显得「刚用过」而少发一发，方向是保守的。
func (s *Server) lastRealSuccessAt(providerID string) time.Time {
	if s.requestLogStore == nil {
		return time.Time{}
	}
	page, err := s.requestLogStore.QueryRequestLogs(monitor.RequestLogQuery{
		ProviderID: providerID,
		From:       time.Now().Add(-probeDailyLookback),
		Status:     "2xx",
		Page:       1,
		PageSize:   1,
	})
	if err != nil || len(page.Items) == 0 {
		return time.Time{}
	}
	return page.Items[0].Time
}

// startMonitorProber 是探测的调度心跳。它刻意独立于监控引擎的 tick：引擎只读快照，
// 而这里是会对外发请求的一方，出问题时要把两者分开看。
func (s *Server) startMonitorProber(ctx context.Context) {
	ticker := time.NewTicker(probeTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runProbeTick(ctx, time.Now().UTC())
		}
	}
}

func (s *Server) runProbeTick(ctx context.Context, now time.Time) {
	if s.monitoringStore == nil {
		return
	}
	cfg := monitoring.NewConfig(s.monitoringStore).Probe()
	if !cfg.EventDriven.Enabled && !cfg.Daily.Enabled {
		return
	}
	targets := s.liveProbeTargets()
	if len(targets) == 0 {
		return
	}
	day := monitorProbeDay(s, now)
	idle := time.Duration(cfg.EventDriven.IdleMinutes) * time.Minute
	minInterval := time.Duration(cfg.EventDriven.MinIntervalMinutes) * time.Minute
	unavailable := map[string]bool{}
	for _, id := range s.providerUnavailableIDs() {
		unavailable[id] = true
	}
	dailyOpen := cfg.Daily.Enabled && withinClockWindow(cfg.Daily.WindowStart, cfg.Daily.WindowEnd, now.In(monitorProbeLocation(s)))
	dailyBudget := probeDailyBatchPerTick

	for _, provider := range targets {
		outcome, used, err := s.monitoringStore.MonitorProbeOutcome(provider.ID, day)
		if err != nil {
			continue
		}
		gate := probeGate{
			Now: now, Day: day, UsedToday: used, MaxPerDay: cfg.EventDriven.MaxPerDayPerAccount,
			LastProbeAt: outcome.LastAt, MinInterval: minInterval, Idle: idle,
			LastRealSuccess:   s.lastRealSuccessAt(provider.ID),
			MarkedUnavailable: unavailable[provider.ID],
		}
		gate.Enabled = cfg.EventDriven.Enabled
		if gate.allowEventDriven() {
			s.runLightProbe(ctx, provider.ID)
			continue
		}
		if dailyOpen && dailyBudget > 0 {
			dailyGate := gate
			dailyGate.Enabled = true
			dailyGate.MaxPerDay = cfg.EventDriven.MaxPerDayPerAccount
			if dailyGate.allowDaily() {
				s.runLightProbe(ctx, provider.ID)
				dailyBudget--
			}
		}
	}
}
