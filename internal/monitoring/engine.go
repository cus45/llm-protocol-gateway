package monitoring

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Transition is one observable outcome of an evaluation tick. Dispatcher turns
// it into pushes; the timeline tables are already updated by the engine.
type Transition struct {
	State      AlertState
	Event      Event
	Firing     bool
	Resolved   bool
	Upgraded   bool
	Reminder   bool
	Suppressed bool
}

type pendingState struct {
	Since       time.Time
	Consecutive int
	FalseStreak int
}

type flapRecord struct {
	Times []time.Time
}

type evaluation struct {
	RuleKey     string
	TargetType  string
	TargetID    string
	TargetName  string
	Fingerprint string
	Available   bool
	Hit         bool
	Severity    Severity
	Title       string
	Detail      map[string]any
	Params      map[string]float64
}

// Engine owns rule state transitions. It is pure domain logic: persistence and
// observation come in through the interfaces, so tests drive it with fakes.
type Engine struct {
	mu        sync.Mutex
	store     StateStore
	rules     []RuleConfig
	ruleByKey map[string]RuleConfig
	pending   map[string]pendingState
	flaps     map[string]flapRecord
	now       func() time.Time
}

func NewEngine(store StateStore, rules []RuleConfig) *Engine {
	return &Engine{
		store:     store,
		rules:     rules,
		ruleByKey: indexRules(rules),
		pending:   map[string]pendingState{},
		flaps:     map[string]flapRecord{},
		now:       time.Now,
	}
}

func indexRules(rules []RuleConfig) map[string]RuleConfig {
	out := make(map[string]RuleConfig, len(rules))
	for _, rule := range rules {
		out[rule.Key] = rule
	}
	return out
}

// SetRules hot-swaps thresholds and severities. Pending timers of disabled
// rules are dropped so re-enabling starts a fresh sustain window.
func (e *Engine) SetRules(rules []RuleConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules = rules
	e.ruleByKey = indexRules(rules)
	e.prunePending()
}

func (e *Engine) rule(key string) (RuleConfig, bool) {
	rule, ok := e.ruleByKey[key]
	return rule, ok && rule.Enabled
}

// Rules returns a copy of the active rule set for config reload wiring.
func (e *Engine) Rules() []RuleConfig {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]RuleConfig(nil), e.rules...)
}

func (e *Engine) Evaluate(ctx context.Context, snap Snapshot) ([]Transition, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := snap.Now
	if now.IsZero() {
		now = e.now()
	}
	states, err := e.store.LoadMonitorStates()
	if err != nil {
		return nil, err
	}
	byFingerprint := make(map[string]AlertState, len(states))
	for _, state := range states {
		byFingerprint[state.Fingerprint] = state
	}
	e.prunePending()
	evals := e.evaluateAll(snap, now)
	evals = append(evals, e.orphanEvaluations(byFingerprint, evals)...)
	sort.Slice(evals, func(i, j int) bool { return evals[i].Fingerprint < evals[j].Fingerprint })

	transitions := make([]Transition, 0, 4)
	for _, ev := range evals {
		current, exists := byFingerprint[ev.Fingerprint]
		if tr := e.apply(ev, current, exists, now); tr != nil {
			byFingerprint[ev.Fingerprint] = tr.State
			transitions = append(transitions, *tr)
		}
	}
	return transitions, nil
}

// EvaluateCollectorHealth is the watchdog path used when a normal tick fails.
// It must never run the full rule set: an empty snapshot would otherwise look
// like every firing alert recovered.
func (e *Engine) EvaluateCollectorHealth(ctx context.Context, now, lastSuccess time.Time) ([]Transition, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if now.IsZero() {
		now = e.now()
	}
	rule, ok := e.rule("collector_stale")
	if !ok {
		return nil, nil
	}
	states, err := e.store.LoadMonitorStates()
	if err != nil {
		return nil, err
	}
	ev := e.collectorEvaluation(rule, Snapshot{Now: now, CollectorLastSuccess: lastSuccess}, now)
	for _, state := range states {
		if state.Fingerprint != ev.Fingerprint {
			continue
		}
		if tr := e.apply(ev, state, true, now); tr != nil {
			return []Transition{*tr}, nil
		}
		return nil, nil
	}
	if tr := e.apply(ev, AlertState{}, false, now); tr != nil {
		return []Transition{*tr}, nil
	}
	return nil, nil
}

func (e *Engine) prunePending() {
	for fingerprint := range e.pending {
		ruleKey := fingerprint
		if idx := strings.IndexByte(fingerprint, '|'); idx >= 0 {
			ruleKey = fingerprint[:idx]
		}
		rule, ok := e.ruleByKey[ruleKey]
		if !ok || !rule.Enabled {
			delete(e.pending, fingerprint)
		}
	}
}

// orphanEvaluations marks firing states whose target vanished from the
// snapshot (deleted provider, failed key scan) as data-unavailable so the
// grace timer can eventually resolve them. Without this they would fire
// forever because no rule evaluation would ever touch them again.
func (e *Engine) orphanEvaluations(states map[string]AlertState, evals []evaluation) []evaluation {
	seen := make(map[string]bool, len(evals))
	for _, ev := range evals {
		seen[ev.Fingerprint] = true
	}
	out := make([]evaluation, 0)
	for fingerprint, state := range states {
		if state.Status != StatusFiring || seen[fingerprint] {
			continue
		}
		rule, ok := e.rule(state.RuleKey)
		if !ok {
			continue
		}
		out = append(out, evaluation{
			RuleKey: state.RuleKey, TargetType: state.TargetType, TargetID: state.TargetID,
			TargetName: state.TargetName, Fingerprint: fingerprint, Params: rule.Params,
		})
	}
	return out
}

func (e *Engine) apply(ev evaluation, current AlertState, exists bool, now time.Time) *Transition {
	if !ev.Available {
		e.pending[ev.Fingerprint] = pendingState{}
		if exists && current.Status == StatusFiring {
			if current.UnavailableSince.IsZero() {
				current.UnavailableSince = now
				current.Detail = ev.Detail
				_, _ = e.store.UpsertMonitorState(current)
			} else if now.Sub(current.UnavailableSince) > graceFor(ev.RuleKey) {
				current.Status = StatusResolved
				current.ResolutionReason = ResolutionDataUnavailable
				current.LastSeen = now
				saved, err := e.store.UpsertMonitorState(current)
				if err != nil {
					return nil
				}
				event := e.newEvent(saved, now, StatusResolved)
				event.ResolutionReason = ResolutionDataUnavailable
				id, _ := e.store.InsertMonitorEvent(event)
				event.ID = id
				return &Transition{State: saved, Event: event, Resolved: true}
			}
		}
		return nil
	}

	if exists && current.Status == StatusFiring {
		if ev.Hit {
			current.LastSeen = now
			current.Count++
			current.UnavailableSince = time.Time{}
			current.Detail = ev.Detail
			// A hit again means the false streak is broken; otherwise a
			// true-false-true-false pattern would count as two clean ticks.
			delete(e.pending, ev.Fingerprint)
			if ev.Severity.Rank() > current.Severity.Rank() {
				current.Severity = ev.Severity
				saved, err := e.store.UpsertMonitorState(current)
				if err != nil {
					return nil
				}
				event := e.newEvent(saved, now, StatusFiring)
				event.Title = ev.Title
				event.Detail = ev.Detail
				id, _ := e.store.InsertMonitorEvent(event)
				event.ID = id
				return &Transition{State: saved, Event: event, Firing: true, Upgraded: true}
			}
			saved, err := e.store.UpsertMonitorState(current)
			if err != nil {
				return nil
			}
			return e.reminder(saved, now)
		}
		p := e.pending[ev.Fingerprint]
		p.FalseStreak++
		e.pending[ev.Fingerprint] = p
		if p.FalseStreak < 2 {
			return nil
		}
		delete(e.pending, ev.Fingerprint)
		current.Status = StatusResolved
		current.Disposition = DispositionOpen
		current.ResolutionReason = ResolutionRecovered
		current.LastSeen = now
		// The recovery card must report the window that proved recovery. Keeping
		// the detail from the last firing (or last data-unavailable) tick would
		// push stale or all-zero counters, which reads as "no data" instead of
		// "healthy again".
		current.Detail = ev.Detail
		saved, err := e.store.UpsertMonitorState(current)
		if err != nil {
			return nil
		}
		event := e.newEvent(saved, now, StatusResolved)
		event.ResolutionReason = ResolutionRecovered
		id, _ := e.store.InsertMonitorEvent(event)
		event.ID = id
		saved = e.recordFlap(saved, now)
		return &Transition{State: saved, Event: event, Resolved: true}
	}

	// inactive or resolved: first hit starts a sustain window, and only a
	// sustained clean run creates a new alert.
	if !ev.Hit {
		delete(e.pending, ev.Fingerprint)
		return nil
	}
	p := e.pending[ev.Fingerprint]
	if p.Since.IsZero() {
		p.Since = now
	}
	p.Consecutive++
	e.pending[ev.Fingerprint] = p
	if !e.ready(ev, p, now) {
		return nil
	}
	delete(e.pending, ev.Fingerprint)
	state := AlertState{
		Fingerprint: ev.Fingerprint, RuleKey: ev.RuleKey, Severity: ev.Severity,
		TargetType: ev.TargetType, TargetID: ev.TargetID, TargetName: ev.TargetName,
		Status: StatusFiring, Disposition: DispositionOpen, Count: 1,
		FirstSeen: now, LastSeen: now, Detail: ev.Detail,
	}
	saved, err := e.store.UpsertMonitorState(state)
	if err != nil {
		return nil
	}
	event := e.newEvent(saved, now, StatusFiring)
	event.Title = ev.Title
	event.Detail = ev.Detail
	id, _ := e.store.InsertMonitorEvent(event)
	event.ID = id
	saved = e.recordFlap(saved, now)
	return &Transition{State: saved, Event: event, Firing: true}
}

func (e *Engine) reminder(state AlertState, now time.Time) *Transition {
	if state.Disposition != DispositionOpen || state.SuppressedUntil.After(now) || state.LastNotifiedAt.IsZero() {
		return nil
	}
	rule, ok := e.ruleByKey[state.RuleKey]
	if !ok {
		return nil
	}
	if now.Sub(state.LastNotifiedAt) < time.Duration(rule.CooldownMinutes)*time.Minute {
		return nil
	}
	eventID, err := e.store.LatestMonitorEventID(state.Fingerprint)
	if err != nil || eventID == 0 {
		return nil
	}
	// Reminders reuse the latest event row; only the push record is appended.
	event := Event{
		ID: eventID, Time: now, Kind: EventKindAlert, Severity: state.Severity,
		RuleKey: state.RuleKey, TargetType: state.TargetType, TargetID: state.TargetID,
		TargetName: state.TargetName, Title: fmt.Sprintf("%s 仍未恢复", ruleName(state.RuleKey)),
		Detail: state.Detail, Fingerprint: state.Fingerprint, Status: StatusFiring,
		Count: state.Count, FirstSeen: state.FirstSeen, LastSeen: state.LastSeen,
	}
	return &Transition{State: state, Event: event, Reminder: true}
}

func (e *Engine) ready(ev evaluation, p pendingState, now time.Time) bool {
	def, ok := RuleDefinition(ev.RuleKey)
	if ok && def.Consecutive {
		want := paramInt(ev.Params, "consecutiveFailures")
		if ev.RuleKey == "checkin_failed" {
			want = paramInt(ev.Params, "consecutiveDays")
		}
		return p.Consecutive >= want
	}
	return now.Sub(p.Since) >= sustainedFor(ev.RuleKey)
}

func (e *Engine) recordFlap(state AlertState, now time.Time) AlertState {
	rec := e.flaps[state.Fingerprint]
	cutoff := now.Add(-15 * time.Minute)
	kept := rec.Times[:0]
	for _, t := range rec.Times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	rec.Times = append(kept, now)
	e.flaps[state.Fingerprint] = rec
	if len(rec.Times) >= 3 {
		state.SuppressedUntil = now.Add(time.Hour)
		if saved, err := e.store.UpsertMonitorState(state); err == nil {
			state = saved
		}
	}
	return state
}

func (e *Engine) newEvent(state AlertState, now time.Time, status string) Event {
	title := ruleName(state.RuleKey)
	if status == StatusResolved {
		title = fmt.Sprintf("%s 已恢复", title)
	}
	event := Event{
		Time: now, Kind: EventKindAlert, Severity: state.Severity, RuleKey: state.RuleKey,
		TargetType: state.TargetType, TargetID: state.TargetID, TargetName: state.TargetName,
		Title: title, Detail: state.Detail, Fingerprint: state.Fingerprint,
		Status: status, Count: state.Count, FirstSeen: state.FirstSeen, LastSeen: state.LastSeen,
	}
	if status == StatusResolved {
		event.ResolvedAt = now
	}
	return event
}

func ruleName(key string) string {
	if def, ok := RuleDefinition(key); ok {
		return def.Name
	}
	return key
}

func (e *Engine) evaluateAll(snap Snapshot, now time.Time) []evaluation {
	out := make([]evaluation, 0, 32)
	names := make(map[string]string, len(snap.Providers))
	for _, p := range snap.Providers {
		name := p.Name
		if name == "" {
			name = p.ID
		}
		names[p.ID] = name
	}

	for _, p := range snap.Providers {
		out = append(out, e.providerEvaluations(p, now)...)
		// Model-list failures belong to an account that syncs models at all; a
		// plain API-key vendor has no sync to fail, and its zero counter must not
		// resolve an alert a previous configuration raised.
		if p.Capabilities.Models {
			if rule, ok := e.rule("model_unavailable"); ok {
				want := paramInt(rule.Params, "consecutiveFailures")
				detail := map[string]any{"consecutiveFailures": p.ModelFailures, "threshold": want}
				out = append(out, makeEvaluation(rule, TargetProvider, p.ID, providerName(p), true,
					p.ModelFailures >= want, fmt.Sprintf("%s 模型列表拉取失败", providerName(p)), detail))
			}
		}
	}

	ids := make([]string, 0, len(snap.Providers)+len(snap.Windows))
	seen := make(map[string]bool, len(snap.Providers)+len(snap.Windows))
	for _, p := range snap.Providers {
		if !seen[p.ID] {
			seen[p.ID] = true
			ids = append(ids, p.ID)
		}
	}
	for id := range snap.Windows {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	byID := make(map[string]ProviderObservation, len(snap.Providers))
	for _, p := range snap.Providers {
		byID[p.ID] = p
	}
	for _, id := range ids {
		name := names[id]
		if name == "" {
			name = id
		}
		out = append(out, e.windowEvaluations(id, name, snap.Windows[id], byID[id], now)...)
	}

	for _, key := range snap.Keys {
		out = append(out, e.keyEvaluations(key, snap.DataErrors)...)
	}

	if rule, ok := e.rule("collector_stale"); ok {
		out = append(out, e.collectorEvaluation(rule, snap, now))
	}
	return out
}

func (e *Engine) providerEvaluations(p ProviderObservation, now time.Time) []evaluation {
	out := make([]evaluation, 0, 7)
	name := p.Name
	if name == "" {
		name = p.ID
	}

	if rule, ok := e.rule("oauth_expiring"); ok {
		available := !p.ExpiresAt.IsZero() && p.Renewal.Known
		lead := time.Duration(rule.Params["leadMinutes"]) * time.Minute
		hit := available && p.ExpiresAt.Sub(now) < lead && !p.Renewal.LastOK
		detail := map[string]any{"expiresAt": timeString(p.ExpiresAt), "renewalOk": p.Renewal.LastOK}
		if !p.Renewal.LastAt.IsZero() {
			detail["renewalAt"] = timeString(p.Renewal.LastAt)
		}
		out = append(out, makeEvaluation(rule, TargetProvider, p.ID, name, available, hit,
			fmt.Sprintf("%s 凭证即将过期", name), detail))
	}

	if rule, ok := e.rule("oauth_refresh_failed"); ok {
		available := !p.ExpiresAt.IsZero() && p.Renewal.Known
		want := paramInt(rule.Params, "consecutiveFailures")
		hit := available && p.Renewal.Consecutive >= want
		detail := map[string]any{"consecutiveFailures": p.Renewal.Consecutive, "threshold": want}
		out = append(out, makeEvaluation(rule, TargetProvider, p.ID, name, available, hit,
			fmt.Sprintf("%s 凭证续期连续失败", name), detail))
	}

	if rule, ok := e.rule("quota_low"); ok {
		available := p.Quota != nil && p.Quota.Available()
		hit := available && p.Quota.Ratio < rule.Params["maxRatio"]
		detail := map[string]any{}
		if p.Quota != nil {
			detail["remaining"] = p.Quota.Remaining
			detail["limit"] = p.Quota.Limit
			detail["ratio"] = p.Quota.Ratio
			detail["source"] = p.Quota.Source
		}
		out = append(out, makeEvaluation(rule, TargetProvider, p.ID, name, available, hit,
			fmt.Sprintf("%s 额度不足", name), detail))
	}

	if rule, ok := e.rule("credit_expiring"); ok {
		available := p.Quota != nil && p.Quota.Remaining > 0 && !p.Quota.ExpiresAt.IsZero()
		lead := time.Duration(rule.Params["leadHours"]) * time.Hour
		hit := available && p.Quota.ExpiresAt.Sub(now) < lead
		detail := map[string]any{}
		if p.Quota != nil {
			detail["remaining"] = p.Quota.Remaining
			detail["expiresAt"] = timeString(p.Quota.ExpiresAt)
			detail["source"] = p.Quota.Source
		}
		out = append(out, makeEvaluation(rule, TargetProvider, p.ID, name, available, hit,
			fmt.Sprintf("%s 积分即将到期", name), detail))
	}

	if rule, ok := e.rule("probe_failed"); ok {
		// 没跑成 ≠ 跑失败：从未探测与本轮跳过都判 unavailable，让界面能说
		// 「不适用/暂无数据」而不是骗人一句「正常」。
		available := p.Probe.Known && p.Probe.Skipped == ""
		want := paramInt(rule.Params, "consecutiveFailures")
		hit := available && p.Probe.Consecutive >= want
		detail := map[string]any{
			"consecutiveFailures": p.Probe.Consecutive,
			"threshold":           want,
			"lastStatus":          p.Probe.LastStatus,
		}
		if !p.Probe.LastAt.IsZero() {
			detail["lastProbeAt"] = timeString(p.Probe.LastAt)
		}
		if p.Probe.Skipped != "" {
			detail["skipped"] = p.Probe.Skipped
		}
		ev := makeEvaluation(rule, TargetProvider, p.ID, name, available, hit,
			fmt.Sprintf("%s 轻探测失败", name), detail)
		if hit && p.Probe.AuthFailure {
			// 401/403 说明凭证已经废了，比「上游不通」更需要立刻动手；apply() 在
			// firing 期间按 Rank 只升不降，所以这里给高级别不会被后续 tick 冲掉。
			ev.Severity = severityAtRankParam(rule.Params, "authSeverity", rule.Severity)
			detail["escalated"] = true
		}
		out = append(out, ev)
	}

	if rule, ok := e.rule("checkin_failed"); ok {
		available := p.Checkin.Known && p.Checkin.Supported
		want := paramInt(rule.Params, "consecutiveDays")
		hit := available && p.Checkin.Consecutive >= want
		detail := map[string]any{"consecutiveDays": p.Checkin.Consecutive, "threshold": want}
		if !p.Checkin.LastAttemptAt.IsZero() {
			detail["lastAttemptAt"] = timeString(p.Checkin.LastAttemptAt)
		}
		out = append(out, makeEvaluation(rule, TargetProvider, p.ID, name, available, hit,
			fmt.Sprintf("%s 连续签到失败", name), detail))
	}
	return out
}

func providerName(p ProviderObservation) string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

func (e *Engine) windowEvaluations(id, name string, windows map[int]WindowStats, provider ProviderObservation, now time.Time) []evaluation {
	out := make([]evaluation, 0, 6)

	if rule, ok := e.rule("auth_invalid"); ok {
		minutes := paramInt(rule.Params, "windowMinutes")
		stats, found := windows[minutes]
		available := found && stats.Total >= paramInt(rule.Params, "minSamples")
		hit := available && stats.Auth401403 >= paramInt(rule.Params, "minCount") &&
			stats.AuthRatio() >= rule.Params["minRatio"]
		detail := map[string]any{
			"authCount": stats.Auth401403, "authRatio": stats.AuthRatio(),
			"total": stats.Total, "windowMinutes": minutes,
		}
		out = append(out, makeEvaluation(rule, TargetProvider, id, name, available, hit,
			fmt.Sprintf("%s 凭证失效", name), detail))
	}

	if rule, ok := e.rule("provider_down"); ok {
		out = append(out, e.windowEvaluation(rule, id, name, windows, func(stats WindowStats) bool {
			return stats.SuccessRatio() < rule.Params["maxSuccessRatio"]
		}, fmt.Sprintf("%s 成功率跌破 %.0f%%", name, rule.Params["maxSuccessRatio"]*100),
			func(stats WindowStats) map[string]any {
				return map[string]any{"successRatio": stats.SuccessRatio(), "success": stats.Success, "total": stats.Total}
			}))
	}

	if rule, ok := e.rule("server_error_spike"); ok {
		out = append(out, e.windowEvaluation(rule, id, name, windows, func(stats WindowStats) bool {
			return stats.ErrorRatio() > rule.Params["maxErrorRatio"]
		}, fmt.Sprintf("%s 上游 5xx 激增", name),
			func(stats WindowStats) map[string]any {
				return map[string]any{"errorRatio": stats.ErrorRatio(), "serverError": stats.ServerError, "total": stats.Total}
			}))
	}

	if rule, ok := e.rule("rate_limit_spike"); ok {
		out = append(out, e.windowEvaluation(rule, id, name, windows, func(stats WindowStats) bool {
			return stats.RateLimitRatio() > rule.Params["maxRatio"] ||
				float64(stats.RateLimited) >= rule.Params["maxCount"]
		}, fmt.Sprintf("%s 上游限流激增", name),
			func(stats WindowStats) map[string]any {
				return map[string]any{"rateLimitRatio": stats.RateLimitRatio(), "rateLimited": stats.RateLimited, "total": stats.Total}
			}))
	}

	if rule, ok := e.rule("empty_response"); ok {
		out = append(out, e.windowEvaluation(rule, id, name, windows, func(stats WindowStats) bool {
			return stats.EmptyRatio() > rule.Params["maxRatio"]
		}, fmt.Sprintf("%s 空响应增多", name),
			func(stats WindowStats) map[string]any {
				return map[string]any{"emptyRatio": stats.EmptyRatio(), "empty": stats.Empty, "total": stats.Total}
			}))
	}

	if rule, ok := e.rule("ttft_regression"); ok {
		minutes := paramInt(rule.Params, "windowMinutes")
		stats, found := windows[minutes]
		floor := rule.Params["floorMs"]
		factor := rule.Params["factor"]
		baseline := float64(provider.BaselineTTFTP95)
		available := found && stats.Total >= paramInt(rule.Params, "minSamples") && baseline > 0
		hit := available && float64(stats.TTFTp95) > math.Max(floor, factor*baseline)
		detail := map[string]any{
			"ttftP95": stats.TTFTp95, "baselineP95": provider.BaselineTTFTP95,
			"floorMs": floor, "factor": factor, "windowMinutes": minutes,
		}
		out = append(out, makeEvaluation(rule, TargetProvider, id, name, available, hit,
			fmt.Sprintf("%s 首包延迟回退", name), detail))
	}
	return out
}

func (e *Engine) windowEvaluation(rule RuleConfig, id, name string, windows map[int]WindowStats, hitFn func(WindowStats) bool, title string, detailFn func(WindowStats) map[string]any) evaluation {
	minutes := paramInt(rule.Params, "windowMinutes")
	stats, found := windows[minutes]
	available := found && stats.Total >= paramInt(rule.Params, "minSamples")
	hit := available && hitFn(stats)
	detail := map[string]any{"windowMinutes": minutes}
	if found {
		for key, value := range detailFn(stats) {
			detail[key] = value
		}
	}
	return makeEvaluation(rule, TargetProvider, id, name, available, hit, title, detail)
}

func (e *Engine) keyEvaluations(key KeyObservation, dataErrors []string) []evaluation {
	out := make([]evaluation, 0, 4)
	name := key.Name
	if name == "" {
		name = key.ID
	}

	if rule, ok := e.rule("key_budget_warn"); ok {
		available := key.TokenLimit > 0 || key.CostLimitMicros > 0
		ratio := keyBudgetRatio(key)
		hit := available && ratio >= rule.Params["warnRatio"]
		out = append(out, makeEvaluation(rule, TargetAPIKey, key.ID, name, available, hit,
			fmt.Sprintf("%s 额度即将耗尽", name), keyBudgetDetail(key, ratio)))
	}

	if rule, ok := e.rule("key_budget_exhausted"); ok {
		available := key.TokenLimit > 0 || key.CostLimitMicros > 0
		ratio := keyBudgetRatio(key)
		hit := available && ratio >= rule.Params["criticalRatio"]
		out = append(out, makeEvaluation(rule, TargetAPIKey, key.ID, name, available, hit,
			fmt.Sprintf("%s 额度已耗尽", name), keyBudgetDetail(key, ratio)))
	}

	if rule, ok := e.rule("multi_ip"); ok {
		available := !hasDataError(dataErrors, "multi_ip")
		hit := available && key.MultiIPCount >= paramInt(rule.Params, "minIPs")
		ips := key.MultiIPs
		if len(ips) > 12 {
			ips = ips[:12]
		}
		detail := map[string]any{
			"ips": ips, "ipCount": key.MultiIPCount,
			"windowMinutes": paramInt(rule.Params, "windowMinutes"),
		}
		out = append(out, makeEvaluation(rule, TargetAPIKey, key.ID, name, available, hit,
			fmt.Sprintf("%s 多 IP 使用", name), detail))
	}

	if rule, ok := e.rule("concurrent_ip"); ok {
		available := !hasDataError(dataErrors, "concurrent_ip")
		hit := available && key.ConcurrentIPs >= paramInt(rule.Params, "minIPs")
		ips := key.MultiIPs
		if len(ips) > 12 {
			ips = ips[:12]
		}
		detail := map[string]any{"ips": ips, "ipCount": key.ConcurrentIPs}
		if !key.ConcurrentAt.IsZero() {
			detail["concurrentAt"] = timeString(key.ConcurrentAt)
		}
		out = append(out, makeEvaluation(rule, TargetAPIKey, key.ID, name, available, hit,
			fmt.Sprintf("%s 并发 IP 使用", name), detail))
	}
	return out
}

func (e *Engine) collectorEvaluation(rule RuleConfig, snap Snapshot, now time.Time) evaluation {
	maxAge := time.Duration(rule.Params["maxAgeMinutes"]) * time.Minute
	last := snap.CollectorLastSuccess
	hit := last.IsZero() || now.Sub(last) > maxAge
	detail := map[string]any{
		"lastSuccess":   timeString(last),
		"maxAgeMinutes": rule.Params["maxAgeMinutes"],
	}
	if !last.IsZero() {
		detail["staleMinutes"] = now.Sub(last).Minutes()
	}
	return makeEvaluation(rule, TargetGateway, "gateway", "网关", true, hit, "监控采集器停滞", detail)
}

func makeEvaluation(rule RuleConfig, targetType, targetID, targetName string, available, hit bool, title string, detail map[string]any) evaluation {
	if detail == nil {
		detail = map[string]any{}
	}
	return evaluation{
		RuleKey: rule.Key, TargetType: targetType, TargetID: targetID, TargetName: targetName,
		Fingerprint: rule.Key + "|" + targetType + ":" + targetID,
		Available:   available, Hit: hit, Severity: rule.Severity, Title: title,
		Detail: detail, Params: rule.Params,
	}
}

func keyBudgetRatio(key KeyObservation) float64 {
	ratio := 0.0
	if key.TokenLimit > 0 {
		ratio = math.Max(ratio, float64(key.TokenUsed)/float64(key.TokenLimit))
	}
	if key.CostLimitMicros > 0 {
		ratio = math.Max(ratio, float64(key.CostUsedMicros)/float64(key.CostLimitMicros))
	}
	return ratio
}

func keyBudgetDetail(key KeyObservation, ratio float64) map[string]any {
	detail := map[string]any{
		"ratio": ratio, "tokenUsed": key.TokenUsed, "tokenLimit": key.TokenLimit,
		"costUsedMicros": key.CostUsedMicros, "costLimitMicros": key.CostLimitMicros,
	}
	if key.QuotaWindow != "" {
		detail["quotaWindow"] = key.QuotaWindow
	}
	return detail
}

func hasDataError(dataErrors []string, prefix string) bool {
	for _, entry := range dataErrors {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func paramInt(params map[string]float64, key string) int { return int(params[key]) }

// severityAtRankParam 取一个按 Severity.Rank() 编码（3=p0、2=p1、1=p2）的级别参数。
// RuleConfig.Params 只有 float64，用 rank 存级别可以省掉一张字符串对照表，
// 也让「参数越界」退化成规则自己的默认级别而不是崩溃。
func severityAtRankParam(params map[string]float64, key string, fallback Severity) Severity {
	switch int(params[key]) {
	case SeverityP0.Rank():
		return SeverityP0
	case SeverityP1.Rank():
		return SeverityP1
	case SeverityP2.Rank():
		return SeverityP2
	default:
		return fallback
	}
}

func timeString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func sustainedFor(ruleKey string) time.Duration {
	switch ruleKey {
	case "auth_invalid":
		return 3 * time.Minute
	case "provider_down":
		return 10 * time.Minute
	case "server_error_spike", "rate_limit_spike":
		return 5 * time.Minute
	case "empty_response":
		return 10 * time.Minute
	case "ttft_regression":
		return 15 * time.Minute
	case "collector_stale":
		return 5 * time.Minute
	case "key_budget_warn", "key_budget_exhausted":
		return 5 * time.Minute
	default:
		return 0
	}
}

func graceFor(ruleKey string) time.Duration {
	def, ok := RuleDefinition(ruleKey)
	if !ok {
		return 15 * time.Minute
	}
	grace := 2 * def.Interval
	if grace < 15*time.Minute {
		grace = 15 * time.Minute
	}
	return grace
}
