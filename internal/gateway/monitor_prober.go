package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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
	// monitorProbeMaxTokens 刻意给到最小：探测不该消耗订阅额度。tools case 同样
	// 16：判定只看「200 且可解析」，模型是否真调工具不影响结论，额度理由优先。
	monitorProbeMaxTokens = 16
	probeKindLight        = "light"
	probeCaseChat         = "chat"
	// probeCaseTools 在 chat 之外再发一发带工具声明的请求，覆盖工具 schema 被
	// 上游拒收这一类回归（见计划 §7 第 4 条）。
	probeCaseTools  = "tools"
	probeChatPrompt = "reply with the single word ok"
	probeToolPrompt = "call the get_time tool"
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
	// Skipped 非空表示这一发没跑（深检的 cli_missing / 整轮超时 / 取消）。
	// 没跑不等于失败：失败计数与告警只认真正打出去的结果。
	Skipped string
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

// FailedCase 返回第一个失败 case 的名字，供事件 detail 标出是哪一发坏了。
func (r probeRun) FailedCase() string {
	for _, item := range r.Cases {
		if !item.OK {
			return item.Case
		}
	}
	return ""
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

// runLightProbe 对一个账号发一轮轻探测：chat 与 tools 两个 case。
//
// 它刻意走 chat-test 而不是 /v1 数据面：chat-test 不写 request_logs，所以既不进
// 账号成功率也不进成本账本。与额度保活共用同一把单飞锁，避免「保活一发 + 监控一发」
// 同时打在同一账号上——那既多花额度又像滥用。
//
// 一轮 = 一次配额：两个 case 各写一行 monitor_probe_runs，但当日计数按「轮」去重
// （见 store.MonitorProbeOutcome），所以 maxPerDayPerAccount 按次而不是按上游请求数算。
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

	run.Cases = append(run.Cases,
		s.probeChatCase(ctx, providerID, model, probeCaseChat, probeChatPrompt, false),
		s.probeChatCase(ctx, providerID, model, probeCaseTools, probeToolPrompt, true))
	s.recordProbeRun(run)
	return run
}

// probeChatCase 发一个 case：chat-test 路径与普通对话测试完全一致，只是这里带固定
// 的 max_tokens，tools case 再多带一份工具声明。判定只看 HTTP 2xx 且响应体可解析
// 成 JSON——模型是否真的调用工具是模型行为，不是链路问题。
func (s *Server) probeChatCase(ctx context.Context, providerID, model, caseKey, prompt string, includeTools bool) probeCaseResult {
	started := time.Now().UTC()
	req := providerChatTestRequest{
		Model:        model,
		Message:      prompt,
		UserPrompt:   prompt,
		MaxTokens:    monitorProbeMaxTokens,
		IncludeTools: includeTools,
	}
	callCtx, cancel := context.WithTimeout(ctx, lightProbeTimeout)
	defer cancel()
	request, err := quotaProbeHTTPRequest(callCtx, providerID, req)
	if err != nil {
		return probeCaseResult{Case: caseKey, Status: 0, Duration: time.Since(started).Milliseconds(), Error: err.Error()}
	}
	result, _ := s.testProviderChat(request, providerID, req, started)
	status := mapIntValue(result, "status")
	ok, _ := result["success"].(bool)
	caseResult := probeCaseResult{
		Case:     caseKey,
		OK:       ok,
		Status:   status,
		Duration: mapInt64Value(result, "latencyMs"),
		Model:    mapStringValue(result, "model"),
	}
	switch {
	case !ok:
		caseResult.Error = truncateRunReason(mapStringValue(result, "error"))
	case !probeResponseParseable(mapStringValue(result, "responseBody")):
		// 2xx 但不是 JSON：多半是 HTML 错误页或网关伪装的成功应答，链路不可信。
		// 状态码原样保留，结论按失败算。
		caseResult.OK = false
		caseResult.Error = "unparseable response body"
	}
	if !caseResult.OK && caseResult.Error == "" {
		// success=false 却也没带 error（例如上游直接回了非 2xx 却没经过错误分支），
		// 留一句状态说明，否则 detail 里只看得到一个没有原因的失败。
		caseResult.Error = fmt.Sprintf("upstream status %d", status)
	}
	return caseResult
}

// probeResponseParseable 判响应体是不是一个 JSON 对象。不做字段级校验：不同上游
// 的错误信封、usage 形状差异很大，验字段会把「能用」误判成「坏了」。
func probeResponseParseable(body string) bool {
	var payload map[string]any
	trimmed := strings.TrimSpace(body)
	if trimmed == "" || json.Unmarshal([]byte(trimmed), &payload) != nil {
		return false
	}
	return payload != nil
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
//
// 一轮是一个时间戳，写入走单事务：周检的「本周跑过没有」就靠读这个时间戳，
// 半轮可见会让重启后的调度误判。
func (s *Server) persistProbeRun(run probeRun) error {
	if s.monitoringStore == nil || len(run.Cases) == 0 {
		return nil
	}
	day := monitorProbeDay(s, run.At)
	records := make([]monitoring.ProbeRunRecord, 0, len(run.Cases))
	for _, item := range run.Cases {
		records = append(records, probeRunRecordOf(run, item, day))
	}
	if err := s.monitoringStore.InsertMonitorProbeRuns(records); err != nil {
		slog.Warn("monitoring probe run persist failed", "provider", run.ProviderID, "kind", run.Kind, "error", err)
		return err
	}
	return nil
}

// probeRunRecordOf 构造一行探测记录。run.Skipped 在这里读取并写进行：和循环
// 放在同一函数里时，这一列在落库前会被读成零值（Go 1.26 Windows，见 memory
// go126-windows-slice-compare-miscompile）。
func probeRunRecordOf(run probeRun, item probeCaseResult, day string) monitoring.ProbeRunRecord {
	// 逐字段赋值而不是一次大结构化字面量：run / item 两组字段写在同一个字面量里
	// 时，Go 1.26 Windows 会把 run 的字段对错位（Kind 拿到 Skipped 的值），
	// 落库的 kind 变成跳过原因、skipped 变成空。见 memory
	// go126-windows-slice-compare-miscompile。
	var record monitoring.ProbeRunRecord
	record.ProviderID = run.ProviderID
	record.Kind = run.Kind
	record.Case = item.Case
	record.OK = item.OK
	record.Status = item.Status
	record.Skipped = run.Skipped
	record.DurationMs = item.Duration
	record.Model = item.Model
	record.ErrorCode = capProbeError(item.Error)
	record.Day = day
	record.Time = run.At
	return record
}

// capProbeError 截断错误码：库里只留前 200 字节。
func capProbeError(text string) string {
	if len(text) > 200 {
		return text[:200]
	}
	return text
}

// recordProbeRun 把一轮探测写进事件流。成功与失败都只写 info 事件：成级由
// probe_failed 规则按连续次数决定，探测自己不该报警。
func (s *Server) recordProbeRun(run probeRun) {
	if s.monitoringStore == nil {
		return
	}
	_ = s.persistProbeRun(run)
	s.recordProbeEvent(run)
}

// recordProbeEvent 只写事件流（落库由调用方负责）。
func (s *Server) recordProbeEvent(run probeRun) {
	if s.monitoringStore == nil {
		return
	}
	name := run.ProviderName
	if name == "" {
		name = run.ProviderID
	}
	if _, err := s.monitoringStore.InsertMonitorEvent(probeEventOf(run, name, probeEventTitle(run, name))); err != nil {
		// 探测记录失败不影响探测本身，也不能把账号判成不可用。
		slog.Warn("monitoring probe event failed", "provider", run.ProviderID, "error", err)
	}
}

// probeEventTitle 按探测类型给事件标题：CLI 深检不能写「轻探测」，否则事件流
// 与告警列表会把两类探测混成一件。深检没通过也只是 info——它不进 probe_failed。
func probeEventTitle(run probeRun, name string) string {
	if run.Kind == probeKindCLI {
		if run.Failed() {
			return name + " CLI 深检未通过"
		}
		return name + " CLI 深检通过"
	}
	if run.Failed() {
		return name + " 轻探测失败"
	}
	return name + " 轻探测通过"
}

// probeEventOf 构造探测器的那一条 info 事件。
func probeEventOf(run probeRun, name, title string) monitoring.Event {
	event := monitoring.Event{
		Time: run.At, Kind: monitoring.EventKindProbe, Severity: monitoring.SeverityInfo,
		TargetType: monitoring.TargetProvider, TargetID: run.ProviderID, TargetName: name,
		Title: title, Status: monitoring.StatusInfo,
		Detail: map[string]any{
			"kind":    run.Kind,
			"ok":      !run.Failed(),
			"skipped": run.Skipped,
			// case 标出第一个失败的 case（chat / tools），界面和告警一眼能看出
			// 是「对话不通」还是「工具声明被拒」。
			"case":  run.FailedCase(),
			"cases": probeCaseDetails(run.Cases),
		},
	}
	return event
}

// probeCaseDetails 是事件 detail 里的逐用例列表。
func probeCaseDetails(cases []probeCaseResult) []map[string]any {
	out := make([]map[string]any, 0, len(cases))
	for _, item := range cases {
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
		out = append(out, entry)
	}
	return out
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
			s.runProbeTick(ctx, s.probeClock().UTC())
		}
	}
}

// runProbeTick 是探测调度的外形：先看本周深检到点没有，然后跑轻探测那半边。
// 两件事各在各的函数里，tick 本身不做任何判定。
func (s *Server) runProbeTick(ctx context.Context, now time.Time) {
	if cliDeepCheckAllowed(s) == false {
		return
	}
	if dueWeeklyCLIDeepCheck(s, now) {
		s.runWeeklyCLIIfDue(ctx, now)
	}
	s.runLightProbeTick(ctx, now)
}

// cliDeepCheckAllowed 是 tick 里「要不要看周检」的闸门。周检的一切都读库，
// 没有 store 就无从谈起。
func cliDeepCheckAllowed(s *Server) bool {
	return s.monitoringStore != nil
}

// runLightProbeTick 是 tick 里轻探测那半边。配置在这里重新读，只传 now。
func (s *Server) runLightProbeTick(ctx context.Context, now time.Time) {
	if s.monitoringStore == nil {
		return
	}
	cfg := monitoring.NewConfig(s.monitoringStore).Probe()
	if !cfg.EventDriven.Enabled && !cfg.Daily.Enabled {
		return
	}
	s.runLightProbeSweep(ctx, now, cfg)
}

// runLightProbeSweep 扫一遍账号，按事件驱动与每日窗口两套条件决定要不要发轻探测。
// 正在跑深检（probeInFlight）的账号本 tick 直接跳过：同一账号不并发双发。
func (s *Server) runLightProbeSweep(ctx context.Context, now time.Time, cfg monitoring.ProbeConfig) {
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
		if s.probeInFlight(provider.ID) {
			continue
		}
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

// ── 每周 CLI 深检 ─────────────────────────────────────────────────────────

const (
	// probeKindCLI 是深检在 monitor_probe_runs.kind 里的值。周检的「本周跑过
	// 没有」只看这一类行，与轻探测互不干扰。
	probeKindCLI = "cli"
	// probeCLICasePrefix 拼成 case_key='selfcheck:<caseID>'。深检的请求日志
	// 靠 selfcheck- 临时密钥前缀标成 monitor_probe，两者同源、外部客户端
	// 都伪造不了。
	probeCLICasePrefix = "selfcheck:"
	// probeCLICaseTimeout 与 selfcheckDefaultTimeoutMs 对齐：单条用例 90 秒。
	probeCLICaseTimeout = 90 * time.Second
	// probeCLIRoundBudget 是整轮上限。6 条用例串行、每条 90 秒封顶，正常整轮
	// 约 9 分钟；这里留足余量给慢 CLI 启动，超出后剩下的用例记 skipped。
	probeCLIRoundBudget = 2 * time.Hour
	// probeCLISkipCLIMissing：这台机器缺必需 CLI。只记 skipped，不判失败、
	// 不告警——缺 CLI 是网关部署的问题，不是账号的问题。
	probeCLISkipCLIMissing = "cli_missing"
	// probeCLISkipRunning 是本轮「已在跑」的占位行。它让周检的「本周已跑」在
	// 执行器返回之前就成立，从而在进程重启后不会对同一账号重起一轮。
	probeCLISkipRunning = "running"
	// probeCLISkipNotRun 是「整轮结束时还有用例没跑」的兜底原因。
	probeCLISkipNotRun = "not_run"
	// probeCLIRoundMaxAccounts 限制一轮最多串几个账号。深检每条用例都要起一个
	// 真实 CLI，账号多了单轮自然超出 2 小时上限；排在后面的账号本轮不跑，
	// 不发任何行，下一个 tick 按库里记录接着判，直到本周全部跑完。
	probeCLIRoundMaxAccounts = 3
)

// weeklyCLIDue 判「这一发该不该在本周打」。它只看两个时刻：配置写的那天那个
// 时刻（按投递时区），以及库里上一轮的落库时间。重启后从库里重算，不在内存
// 里记「本周已跑」。
//
// 判据是「上一轮早于本周排程点」。排程点过点后的 7 天内恰有一小时满足它：
// 网关那一晚停机，开机后的首个 tick 也会补跑，而且不会跑第二次。
func weeklyCLIDue(now time.Time, loc *time.Location, weekday int, at string, lastRun time.Time) bool {
	if loc == nil {
		loc = time.UTC
	}
	if weekday < 0 || weekday > 6 {
		return false
	}
	minutes, err := clockMinutes(at)
	if err != nil {
		return false
	}
	local := now.In(loc)
	daysSince := (int(local.Weekday()) - weekday + 7) % 7
	scheduled := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).
		AddDate(0, 0, -daysSince).Add(time.Duration(minutes) * time.Minute)
	if local.Before(scheduled) {
		return false
	}
	return lastRun.IsZero() || lastRun.Before(scheduled)
}

func (s *Server) probeClock() time.Time {
	if s.probeNow != nil {
		return s.probeNow()
	}
	return time.Now()
}

// lastCLIRunAt 读 monitor_probe_runs 里最近一轮深检的时间。含 round:running:*
// 占位行：占位存在 = 本轮已在跑，同样算「本周已跑」（重启后不再重起一轮）。
func (s *Server) lastCLIRunAt() (time.Time, error) {
	at, ok, err := s.monitoringStore.LastCLIProbeRunAt()
	if err != nil {
		return time.Time{}, err
	}
	if ok == false {
		return time.Time{}, nil
	}
	return at, nil
}

// missingSelfcheckCLIs 返回必需 CLI 里缺了的那几个 id。
func missingSelfcheckCLIs(tools []selfcheckToolInfo) []string {
	missing := make([]string, 0, len(tools))
	for _, tool := range tools {
		if !tool.Found {
			missing = append(missing, tool.ID)
		}
	}
	return missing
}

// dueWeeklyCLIDeepCheck 判本周深检到点没有。调用方拿到的只有一个 bool；它刻意
// 不是 Server 的方法：内联进 tick 后方法接收者上的判定会被相邻的调用串掉
// （Go 1.26.1/1.26.8 Windows，见 memory go126-windows-slice-compare-miscompile）。
//

//go:noinline
func dueWeeklyCLIDeepCheck(s *Server, now time.Time) bool {
	cfg := monitoring.NewConfig(s.monitoringStore).Probe()
	if cfg.WeeklyCLI.Enabled == false {
		return false
	}
	lastRun, err := s.lastCLIRunAt()
	if err != nil {
		slog.Warn("monitoring cli deep check: read history failed", "error", err)
		return false
	}
	return weeklyCLIDue(now, monitorProbeLocation(s), cfg.WeeklyCLI.Weekday, cfg.WeeklyCLI.Time, lastRun)
}

// runWeeklyCLIIfDue 在既有 60 秒 tick 里判周检：到点且本周没跑过才跑。判定只
// 读库，重启后不会重复消耗。
//
// 判定全用「算出一个结论再比大小」的写法，不在调用之间携带布尔/切片派生状态：
// Go 1.26.1 Windows 会把这种跨调用状态在内联后串到一起（见 memory
// go126-windows-slice-compare-miscompile 与 runLightProbeSweep 的注释）。
//

//go:noinline
func (s *Server) runWeeklyCLIIfDue(ctx context.Context, now time.Time) {
	if s.monitoringStore == nil {
		return
	}
	if dueWeeklyCLIDeepCheck(s, now) == false {
		return
	}
	all := s.liveProbeTargets()
	count := len(all)
	if count == 0 {
		return
	}
	if count > probeCLIRoundMaxAccounts {
		count = probeCLIRoundMaxAccounts
	}
	picked := make([]domain.Provider, count)
	copy(picked, all[:count])
	started, _ := s.startDeepCheck(ctx, picked)
	if started == false {
		slog.Info("monitoring cli deep check deferred: a probe is in flight", "accounts", count)
	}
}

// startDeepCheck 在后台跑一轮深检，立即返回是否起得来。同账号已有探测在飞
// （不管轻探测还是深检）就回 false，调用方回 409：运维点是「现在确认」，排队
// 只会让他以为没生效再点一次。
//
// 单飞在起 goroutine 之前就占住，409 才是确定的；整轮结束（含 panic）一定
// release，并发度也由它封顶为一轮。
func (s *Server) startDeepCheck(ctx context.Context, targets []domain.Provider) (bool, string) {
	if s.monitoringStore == nil {
		return false, "monitoring_disabled"
	}
	if len(targets) == 0 {
		return false, "no_target"
	}
	// CLI 缺失要在占单飞之前判：缺项时整轮只记 skipped=cli_missing、一条命令都不跑，
	// 若先 claim，本 tick 的轻探测扫描会把这些账号当「正在飞」而全部跳过
	// （runLightProbeSweep 的 probeInFlight 分支），每日轮会白白空掉一个 tick。
	if missing := missingSelfcheckCLIs(s.selfcheckTools()); len(missing) > 0 {
		ids := make([]string, 0, len(targets))
		for _, provider := range targets {
			ids = append(ids, provider.ID)
		}
		go s.recordCLIDeepCheckCLIMissing(ctx, ids, missing)
		return true, ""
	}
	claimed := make([]string, 0, len(targets))
	release := func() {
		for _, id := range claimed {
			s.releaseQuotaProbe(id)
		}
	}
	for _, provider := range targets {
		if !s.claimQuotaProbe(provider.ID) {
			release()
			return false, "in_flight"
		}
		claimed = append(claimed, provider.ID)
	}
	go func() {
		// 兜底只针对起 goroutine 之前的那一小段：一旦执行体接手，它的 defer 负责
		// 释放，这里的重复释放会删掉别的调用者的登记。
		defer func() {
			if recovered := recover(); recovered != nil {
				release()
				panic(recovered)
			}
		}()
		s.runCLIDeepCheck(ctx, claimed)
	}()
	return true, ""
}

// runCLIDeepCheck 是深检的执行体，全程持有所见账号的单飞登记。
func (s *Server) runCLIDeepCheck(ctx context.Context, providerIDs []string) {
	defer func() {
		for _, id := range providerIDs {
			s.releaseQuotaProbe(id)
		}
	}()
	s.runCLIDeepCheckRounds(ctx, providerIDs)
}

// runCLIDeepCheckRounds 先查 CLI：缺项就每个账号记一行 skipped=cli_missing
// 收工，一条命令都不执行；否则逐个账号跑。
// recordCLIDeepCheckCLIMissing 给每个账号记一行 skipped=cli_missing。没跑不等于失败：
// 不占单飞、不写事件告警，只留「为什么没有深检」的凭证。
func (s *Server) recordCLIDeepCheckCLIMissing(ctx context.Context, providerIDs []string, missing []string) {
	if s.monitoringStore == nil || len(providerIDs) == 0 {
		return
	}
	startedAt := s.probeClock().UTC()
	slog.Info("monitoring cli deep check skipped: cli missing", "cli", strings.Join(missing, ","))
	for _, providerID := range providerIDs {
		if ctx.Err() != nil {
			return
		}
		provider, _ := s.router.ProviderByID(providerID)
		s.recordProbeRun(probeRun{
			ProviderID: providerID, ProviderName: provider.Name, Kind: probeKindCLI, At: startedAt,
			Cases:   []probeCaseResult{{Case: "cli", Skipped: probeCLISkipCLIMissing, Error: "missing CLI: " + strings.Join(missing, ",")}},
			Skipped: probeCLISkipCLIMissing,
		})
	}
}

func (s *Server) runCLIDeepCheckRounds(ctx context.Context, providerIDs []string) {
	if s.monitoringStore == nil || len(providerIDs) == 0 {
		return
	}
	if missing := missingSelfcheckCLIs(s.selfcheckTools()); len(missing) > 0 {
		s.recordCLIDeepCheckCLIMissing(ctx, providerIDs, missing)
		return
	}
	startedAt := s.probeClock().UTC()
	deadline := startedAt.Add(probeCLIRoundBudget)
	for index, providerID := range providerIDs {
		if ctx.Err() != nil {
			return
		}
		s.runCLIDeepCheckRound(ctx, providerID, deadline)
		// 给后面排队的账号留出 2 小时上限；剩不下就收工，没跑的账号不记任何
		// 行，下一个 tick 会重新排到它们。
		if index+1 < len(providerIDs) && !s.probeClock().Before(deadline) {
			slog.Info("monitoring cli deep check: round budget spent, deferring remaining accounts", "left", len(providerIDs)-index-1)
			return
		}
	}
}

// runCLIDeepCheckRound 对一个账号跑 6 条用例：3 个客户端 × chat/tool。
//
// 复用 runSelfcheckJob 而不是另写一份：临时 route/key 的准备与清理只有那一处
// 实现，复制一份迟早会漂移成泄漏。整轮一个 deadline 控住执行器（到点会把在跑
// 的 CLI 连同 exec.CommandContext 一起杀掉），执行器退出后把没跑到的用例补记
// 为 skipped。
//
// 返回值是本轮最终落库的行：手动 handler 直接拿它组响应，不再回查库去猜
// 「哪一轮是我跑的」（M6）。
//
// 重启安全（M1）：起执行器之前先落一行占位（skipped='running'），本轮收尾时
// 在同一事务里删掉占位、写入正式行。占位行让 weeklyCLIDue 把「本周已跑」判为
// 真：进程若在执行中崩掉，新进程不会对同一账号重起一轮（代价是本周少一次深检，
// 方向保守）。占位的 case_key 不是 selfcheck:*，所以 MonitorProbeOutcome 的
// 「按轮连续失败」与当日配额都不把它算成一发探测。
func (s *Server) runCLIDeepCheckRound(ctx context.Context, providerID string, deadline time.Time) []probeRun {
	// jobID 是本轮的标记：自检 job 目录、临时密钥作用域都用它，两轮同时活着
	// 时各自的密钥名不同，互不删除。
	return s.runCLIDeepCheckRoundKeyed(ctx, providerID, deadline, newProbeCLIJobID(s.probeClock().UTC()))
}

func (s *Server) runCLIDeepCheckRoundKeyed(ctx context.Context, providerID string, deadline time.Time, jobID string) []probeRun {
	roundAt := s.probeClock().UTC()
	state := s.router.State()
	provider, ok := findProvider(state, providerID)
	if !ok || provider.Disabled {
		run := probeRun{
			ProviderID: providerID, Kind: probeKindCLI, At: roundAt, Skipped: "provider_missing",
			Cases: []probeCaseResult{{Case: probeCLICasePrefix + "prepare", Skipped: "provider_missing", Error: "provider not found or disabled"}},
		}
		s.recordProbeRun(run)
		return []probeRun{run}
	}
	job := &selfcheckJob{
		ID:        jobID,
		Status:    "running",
		Prompt:    selfcheckDefaultPrompt,
		TimeoutMs: int(probeCLICaseTimeout / time.Millisecond),
		LANRoot:   selfcheckLANRoot(state),
		StartedAt: roundAt.Format(time.RFC3339),
		Total:     selfcheckCasesPerProvider,
		cases:     make(map[string]preparedCase),
		running:   make(map[string]struct{}),
	}
	if err := s.markCLIDeepCheckRunning(job.ID, providerID, roundAt); err != nil {
		slog.Warn("monitoring cli deep check: running marker failed", "provider", providerID, "error", err)
	}
	// 执行器只受整轮 deadline 约束，不继承调用方的 ctx：手动深检的 ctx 是那个
	// HTTP 请求的 ctx，响应一返回就被取消，执行器会在准备完 route/key 之后、
	// 自己的 defer 清理之前被掐断，留下 selfcheck-* 临时密钥。
	jobCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	selfcheckDone := make(chan struct{})
	go func() {
		defer close(selfcheckDone)
		if s.selfcheckRunFunc != nil {
			s.selfcheckRunFunc(jobCtx, job, []string{providerID})
		} else {
			s.runSelfcheckJob(job, []string{providerID})
		}
	}()
	<-selfcheckDone
	// 执行器自己的 defer 已经清过一次；这里再扫一遍兜底（cleanup 幂等），
	// 保证收尾时刻起库里与内存里都不会再有本轮残留。
	s.cleanupSelfcheckArtifacts(nil)
	job.mu.Lock()
	results := make([]selfcheckCaseResult, len(job.Results))
	copy(results, job.Results)
	job.mu.Unlock()
	// M2/M3：reason 看「现在是否已过整轮上限」，而不是 jobCtx 是否被取消——执行器
	// 恰在 deadline 那一刻返回时，jobCtx.Err() 可能已是 deadline exceeded，但
	// 这仍是「跑满预算、剩余没跑」。
	now := s.probeClock().UTC()
	overBudget := now.After(deadline)
	runs := s.buildCLIDeepCheck(provider, results, probeRoundEnd(now, roundAt), overBudget)
	s.persistDeepCheckReplacingRunning(job.ID, providerID, runs)
	return runs
}

// cliRunningMarkerCase 是占位行的 case_key。刻意不用 selfcheck: 前缀，避免被
// MonitorProbeOutcome 当成一次真实探测（它会跳过 case_key 以 selfcheck: 开头的
// 行做当日配额，见 store.MonitorProbeOutcome）。
const cliRunningMarkerCasePrefix = "round:running:"

// markCLIDeepCheckRunning 写一行「本轮已在跑」的占位（case_key 里带 jobID，
// 与本轮收尾时删除的那一行一一对应）。
func (s *Server) markCLIDeepCheckRunning(jobID, providerID string, roundAt time.Time) error {
	var marker monitoring.ProbeRunRecord
	marker.ProviderID = providerID
	marker.Kind = probeKindCLI
	marker.Case = cliRunningMarkerCasePrefix + jobID
	marker.Skipped = probeCLISkipRunning
	marker.Day = monitorProbeDay(s, roundAt)
	marker.Time = roundAt
	return s.monitoringStore.InsertMonitorProbeRun(marker)
}

// persistDeepCheckReplacingRunning 在同一事务里删掉本轮占位、写入正式行。
func (s *Server) persistDeepCheckReplacingRunning(jobID, providerID string, runs []probeRun) {
	records := make([]monitoring.ProbeRunRecord, 0, selfcheckCasesPerProvider)
	for _, run := range runs {
		day := monitorProbeDay(s, run.At)
		for _, item := range run.Cases {
			records = append(records, probeRunRecordOf(run, item, day))
		}
	}
	if err := s.monitoringStore.ReplaceMonitorProbeRun(providerID, cliRunningMarkerCasePrefix+jobID, records); err != nil {
		slog.Warn("monitoring cli deep check: replace running marker failed, appending", "provider", providerID, "error", err)
		s.persistDeepCheck(runs)
	}
	for _, run := range runs {
		s.recordProbeEvent(run)
	}
}

// persistDeepCheck 逐条落库并写事件。
func (s *Server) persistDeepCheck(runs []probeRun) {
	for _, run := range runs {
		s.recordProbeRun(run)
	}
}

// buildCLIDeepCheck 把执行器的用例结论转成探测行。没出现在结果里的用例补记
// skipped：它们不进失败计数，但占住「本周已跑」，避免每个 tick 反复起一轮。
// 返回的顺序是按 case 的，落库时才落；测试也直接调它，不经后台。
func (s *Server) buildCLIDeepCheck(provider domain.Provider, results []selfcheckCaseResult, roundAt time.Time, roundBudget bool) []probeRun {
	// 每轮组装都是独立的小函数（cliSingleSkippedRun / cliDeepCheckRun），这里
	// 只用返回值拼一张表；跳过原因经无循环的 cliSkipReason 传递。在同一函数里
	// 带循环组装再写库时，标志会在落库前被读成零值（Go 1.26 Windows，见
	// memory go126-windows-slice-compare-miscompile）。
	reason := cliSkipReason(roundBudget)
	out := make([]probeRun, 0, 1+selfcheckCasesPerProvider)
	for _, caseID := range cliMissingCaseIDs(provider.ID, results) {
		out = append(out, cliSingleSkippedRun(provider, caseID, reason, roundAt))
	}
	out = append(out, cliDeepCheckRun(provider, results, roundAt))
	return out
}

// probeRoundEnd 给「这一轮结束时」的时间戳：正常就是 now，但不早于开跑时刻
// 加 1ms，保证注入的假时钟下收尾轮与开始行不会撞在同一时刻。
func probeRoundEnd(now, startedAt time.Time) time.Time {
	floor := startedAt.Add(time.Millisecond)
	if now.Before(floor) {
		return floor
	}
	return now
}

// cliSkipReason 给整轮没跑到的用例一个跳过原因。刻意没有循环、没有跨调用状态。
//
// 目前唯一的来源是「整轮 2 小时上限已过」（round_budget）。原先还有一个
// cancelled（网关停时中止），它没有任何产生路径，已删除，避免界面上出现一条
// 永远走不到的文案。
func cliSkipReason(roundBudget bool) string {
	if roundBudget {
		return "round_budget"
	}
	return probeCLISkipNotRun
}

// cliSingleSkippedRun 构造「一条没跑到的用例」落在哪一轮。
func cliSingleSkippedRun(provider domain.Provider, caseID, skip string, roundAt time.Time) probeRun {
	run := probeRun{ProviderID: provider.ID, ProviderName: provider.Name, Kind: probeKindCLI, At: roundAt}
	// 行级 Skipped 才是落库那一列（probeRunRecordOf 读 run.Skipped）；用例级
	// 那一份只给事件与后续展示用。两处都必须写。
	run.Skipped = skip
	item := probeCaseResult{Case: probeCLICasePrefix + caseID, Skipped: skip, Error: "not run: round did not finish in time"}
	run.Cases = append(run.Cases, item)
	return run
}

// cliMissingCaseIDs 列出执行器没交回结果的用例（整轮 deadline 掐断后的剩余）。
func cliMissingCaseIDs(providerID string, results []selfcheckCaseResult) []string {
	seen := make(map[string]bool, len(results))
	for _, result := range results {
		seen[result.CaseID] = true
	}
	missing := make([]string, 0, selfcheckCasesPerProvider)
	for _, caseID := range selfcheckAllCaseIDs(providerID) {
		if !seen[caseID] {
			missing = append(missing, caseID)
		}
	}
	return missing
}

// cliDeepCheckRun 组装执行器交回的结果（纯组装，不落库）。
func cliDeepCheckRun(provider domain.Provider, results []selfcheckCaseResult, roundAt time.Time) probeRun {
	// 用执行结束后的时钟由调用方决定 roundAt；这里不再读时钟，组装是纯函数。
	run := probeRun{ProviderID: provider.ID, ProviderName: provider.Name, Kind: probeKindCLI, At: roundAt}
	for _, result := range results {
		item := probeCaseResult{
			Case: probeCLICasePrefix + result.CaseID, OK: result.Success && result.ContentOK,
			Status: selfcheckStatus(result), Duration: result.LatencyMs,
			Model: result.Model, Error: truncateRunReason(result.Error),
		}
		run.Cases = append(run.Cases, item)
	}
	return run
}

// selfcheckAllCaseIDs 按执行器的固定次序列出 3 客户端 × chat/tool 的 6 个 case。
func selfcheckAllCaseIDs(providerID string) []string {
	ids := make([]string, 0, selfcheckCasesPerProvider)
	for _, client := range []selfcheckClientCase{selfcheckClientOpenCode, selfcheckClientCodex, selfcheckClientClaude} {
		for _, kind := range []selfcheckCaseKind{selfcheckKindChat, selfcheckKindTool} {
			ids = append(ids, selfcheckCaseID(providerID, client, kind))
		}
	}
	return ids
}

// selfcheckStatus 给深检行一个状态码。CLI 不在 HTTP 层，200 表示用例通过；
// 超时记 0（与传输层失败同义）；其余失败记 502，便于和轻探测的 401/5xx
// 区分开——深检失败几乎总是「CLI 或协议转换坏了」，不是账号鉴权坏了。
func selfcheckStatus(result selfcheckCaseResult) int {
	switch {
	case result.Success && result.ContentOK:
		return http.StatusOK
	case strings.Contains(result.Error, "timed out") || strings.Contains(result.Error, "deadline exceeded"):
		return 0
	default:
		return http.StatusBadGateway
	}
}

// newProbeCLIJobID 生成 selfcheck 目录约定要求的 job id（prune 只认 sc- 前缀）。
func newProbeCLIJobID(roundAt time.Time) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "sc-cli-" + strconv.FormatInt(roundAt.UnixNano(), 36)
	}
	return "sc-cli-" + hex.EncodeToString(buf)
}
