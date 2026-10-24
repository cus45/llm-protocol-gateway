package monitoring

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Service drives one monitoring tick per minute: collect, evaluate, dispatch.
// A separate watchdog keeps checking the collector heartbeat when ticks fail.
type Service struct {
	collector  *Collector
	engine     *Engine
	dispatcher *Dispatcher
	store      ConfigStore
	baselines  BaselineComputer
	now        func() time.Time

	lastRules      string
	lastChannels   string
	lastDispatch   string
	lastSuccess    time.Time
	lastBaselineAt time.Time
	mu             sync.Mutex
}

func NewService(collector *Collector, engine *Engine, dispatcher *Dispatcher, store ConfigStore, baselines BaselineComputer) *Service {
	return &Service{
		collector: collector, engine: engine, dispatcher: dispatcher,
		store: store, baselines: baselines, now: time.Now,
	}
}

func (s *Service) Run(ctx context.Context) {
	go s.watchdog(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	// Run immediately so a restart does not wait a full minute to observe.
	_ = s.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.Tick(ctx)
		}
	}
}

// Tick evaluates with the real clock; TickAt lets tests drive simulated time.
func (s *Service) Tick(ctx context.Context) error { return s.tickAt(ctx, s.now()) }

func (s *Service) TickAt(ctx context.Context, now time.Time) error { return s.tickAt(ctx, now) }

func (s *Service) watchdog(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			last := s.lastSuccess
			s.mu.Unlock()
			s.evaluateCollectorHealth(ctx, last)
		}
	}
}

func (s *Service) tickAt(ctx context.Context, now time.Time) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("monitoring tick panic", "panic", recovered)
			err = fmt.Errorf("monitoring tick panic: %v", recovered)
		}
	}()
	s.reloadConfigIfChanged()
	snap, snapshotErr := s.collector.Snapshot(ctx, now)
	if snapshotErr != nil {
		slog.Warn("monitoring snapshot failed", "error", snapshotErr)
		return snapshotErr
	}
	s.mu.Lock()
	s.lastSuccess = now
	s.mu.Unlock()
	_ = s.store.SetSetting("monitor_collector_last_success", now.UTC().Format(time.RFC3339Nano))
	s.maybeRecomputeBaselines(now)

	transitions, evaluateErr := s.engine.Evaluate(ctx, snap)
	if evaluateErr != nil {
		slog.Warn("monitoring evaluate failed", "error", evaluateErr)
		return evaluateErr
	}
	for _, transition := range transitions {
		if dispatchErr := s.dispatcher.Dispatch(ctx, transition); dispatchErr != nil {
			slog.Warn("monitoring dispatch failed", "fingerprint", transition.State.Fingerprint, "error", dispatchErr)
		}
	}
	return s.dispatcher.Flush(ctx)
}

// reloadConfigIfChanged compares raw settings each tick so console edits (or
// direct DB edits) take effect without a restart.
func (s *Service) reloadConfigIfChanged() {
	rawRules := s.store.Setting("monitor_rules")
	if rawRules != s.lastRules {
		rules, err := ParseRules(rawRules)
		if err != nil {
			slog.Warn("monitoring rules parse failed; keep previous", "error", err)
		} else {
			s.engine.SetRules(rules)
			s.lastRules = rawRules
		}
	}
	rawChannels := s.store.Setting("monitor_channels")
	rawDispatch := s.store.Setting("monitor_dispatch")
	if rawChannels != s.lastChannels || rawDispatch != s.lastDispatch {
		cfg := NewConfig(s.store)
		s.dispatcher.Reload(cfg.Channels(), cfg.Dispatch(), s.engine.Rules())
		s.lastChannels = rawChannels
		s.lastDispatch = rawDispatch
	}
}

func (s *Service) evaluateCollectorHealth(ctx context.Context, lastSuccess time.Time) {
	transitions, err := s.engine.EvaluateCollectorHealth(ctx, s.now(), lastSuccess)
	if err != nil {
		slog.Warn("collector health evaluate failed", "error", err)
		return
	}
	for _, transition := range transitions {
		if dispatchErr := s.dispatcher.Dispatch(ctx, transition); dispatchErr != nil {
			slog.Warn("collector health dispatch failed", "error", dispatchErr)
		}
	}
}

func (s *Service) maybeRecomputeBaselines(now time.Time) {
	if s.baselines == nil {
		return
	}
	if !s.lastBaselineAt.IsZero() && now.Sub(s.lastBaselineAt) < time.Hour {
		return
	}
	loc := monitoringConfigDispatchLocation(s.store)
	if err := s.baselines.RecomputeTTFTBaselines(now, loc); err != nil {
		slog.Warn("ttft baseline recompute failed", "error", err)
		return
	}
	s.lastBaselineAt = now
}

func monitoringConfigDispatchLocation(store ConfigStore) *time.Location {
	return NewConfig(store).Dispatch().Location()
}

func classifyRenewalError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "401"), strings.Contains(text, "403"), strings.Contains(text, "unauthorized"),
		strings.Contains(text, "invalid_grant"), strings.Contains(text, "sign in"), strings.Contains(text, "登录"):
		return "auth_failed"
	case strings.Contains(text, "timeout"), strings.Contains(text, "deadline"), strings.Contains(text, "connection"):
		return "network"
	default:
		return "unknown"
	}
}

// RecordRenewal stores the classified outcome of one credential refresh. The
// raw error stays in the caller's log: it may contain tokens or request bodies.
func (s *Service) RecordRenewal(ctx context.Context, rec RenewalRecord) {
	if s == nil || s.store == nil {
		return
	}
	severity := SeverityInfo
	title := "凭证续期成功"
	detail := map[string]any{"vendor": rec.Vendor, "durationMs": rec.Duration.Milliseconds()}
	if !rec.ExpiresAt.IsZero() {
		detail["expiresAt"] = rec.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if rec.Err != nil {
		severity = SeverityP0
		title = "凭证续期失败"
		errorKind := classifyRenewalError(rec.Err)
		detail["errorKind"] = errorKind
		slog.Warn("monitoring: credential renewal failed", "provider", rec.ProviderID, "errorKind", errorKind)
	}
	at := rec.StartedAt
	if at.IsZero() {
		at = s.now()
	}
	event := Event{
		Time: at, Kind: EventKindRenewal, Severity: severity, Title: title,
		TargetType: TargetProvider, TargetID: rec.ProviderID, Detail: detail, Status: StatusInfo,
	}
	if _, err := s.store.InsertMonitorEvent(event); err != nil {
		slog.Warn("record renewal event failed", "error", err)
	}
}
