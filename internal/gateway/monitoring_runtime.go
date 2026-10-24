package gateway

// The monitoring runtime: one Service per gateway, started from the app runtime
// and fed by the source above. It also owns the low-frequency model-list check
// that feeds the model_unavailable rule.

import (
	"context"
	"log/slog"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitoring"
)

const (
	// modelMonitorFirstDelay lets startup work finish before the first model
	// listing; the loop then runs every modelMonitorInterval.
	modelMonitorFirstDelay = 2 * time.Minute
	modelMonitorInterval   = 15 * time.Minute
)

// StartMonitoring assembles the monitoring subsystem. It returns quietly when
// the store cannot persist monitoring state, so the gateway keeps running with
// the legacy alert scanner alone.
func (s *Server) StartMonitoring(ctx context.Context) {
	if s.monitoringStore == nil || s.monitoring != nil {
		return
	}
	svc, src, engine := s.buildMonitoringService()
	s.monitoringSrc = src
	s.monitoring = svc
	s.monitoringEngine = engine
	if _, err := s.monitoringStore.InsertMonitorEvent(monitoring.Event{
		Time: time.Now().UTC(), Kind: monitoring.EventKindSystem, Severity: monitoring.SeverityInfo,
		Title: "监控中心已启动", TargetType: monitoring.TargetGateway, Status: monitoring.StatusInfo,
		Detail: map[string]any{"version": 1},
	}); err != nil {
		slog.Warn("monitoring start event failed", "error", err)
	}
	go svc.Run(ctx)
	go s.startModelMonitor(ctx)
	go s.startMonitorProber(ctx)
}

// buildMonitoringService wires collector → engine → dispatcher behind one
// Service. It is split out of StartMonitoring so an integration test can drive
// the exact production wiring with a simulated clock instead of the live
// ticker.
func (s *Server) buildMonitoringService() (*monitoring.Service, *monitoringSource, *monitoring.Engine) {
	cfg := monitoring.NewConfig(s.monitoringStore)
	src := newMonitoringSource(s)
	collector := monitoring.NewCollector(src, s.monitoringStore)
	engine := monitoring.NewEngine(s.monitoringStore, cfg.Rules())
	dispatcher := monitoring.NewDispatcher(s.monitoringStore, cfg.Channels(), cfg.Dispatch(), engine.Rules())
	return monitoring.NewService(collector, engine, dispatcher, s.monitoringStore, s.monitoringStore), src, engine
}

// MonitoringService exposes the running service (nil before StartMonitoring).
func (s *Server) MonitoringService() *monitoring.Service { return s.monitoring }

func (s *Server) startModelMonitor(ctx context.Context) {
	timer := time.NewTimer(modelMonitorFirstDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.runModelMonitor()
			timer.Reset(modelMonitorInterval)
		}
	}
}

// runModelMonitor re-reads every account's model list and only counts failures:
// the console already refreshes catalogs on its own schedule, and this loop is
// the input for model_unavailable, not a second notification channel.
func (s *Server) runModelMonitor() {
	if s.monitoringSrc == nil {
		return
	}
	state := s.router.State()
	for _, p := range state.Providers {
		if p.Deleted || p.Disabled || !providerHasModelSync(p) {
			continue
		}
		s.monitoringSrc.setModelFailure(p.ID, s.syncModelsOnce(p))
	}
}

func (s *Server) syncModelsOnce(p domain.Provider) bool {
	var err error
	switch p.AuthType {
	case domain.AuthTypeCursorOAuth:
		_, err = s.syncCursorProviderModels(p.ID)
	case domain.AuthTypeQoderPAT, domain.AuthTypeQoderCN:
		_, err = s.syncQoderProviderModels(p.ID)
	case domain.AuthTypeZCode, domain.AuthTypeZCodeBigModel:
		_, err = s.syncZCodeModels(p)
	case domain.AuthTypeTraeCN, domain.AuthTypeTraeGlobal:
		_, err = s.syncTraeModels(p)
	case domain.AuthTypeWorkBuddy, domain.AuthTypeWorkBuddyAI:
		_, err = s.syncWorkBuddyModels(p.ID)
	default:
		return false
	}
	if err != nil {
		slog.Debug("monitoring model sync failed", "provider", p.ID, "authType", p.AuthType, "error", err)
		return true
	}
	return false
}

// recordRenewal is the Task 11 hook: every credential refresh reports its
// outcome here, and the raw error never leaves this process.
func (s *Server) recordRenewal(provider domain.Provider, vendor string, started time.Time, refreshErr error) {
	if s == nil || s.monitoring == nil {
		return
	}
	s.monitoring.RecordRenewal(context.Background(), monitoring.RenewalRecord{
		ProviderID: provider.ID, Vendor: vendor, StartedAt: started,
		Duration: time.Since(started), Err: refreshErr,
		ExpiresAt: providerCredentialExpiry(provider),
	})
}
