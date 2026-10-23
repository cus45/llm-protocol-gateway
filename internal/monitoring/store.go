package monitoring

import (
	"context"
	"time"
)

// MultiIPHit and ConcurrentIPHit mirror the legacy scanner hits; the gateway
// source converts store-specific rows into these shapes.
type MultiIPHit struct {
	APIKeyID     string
	APIKeyName   string
	IPs          []string
	IPCount      int
	RequestCount int
}

type ConcurrentIPHit struct {
	APIKeyID     string
	APIKeyName   string
	At           time.Time
	IPs          []string
	IPCount      int
	RequestCount int
}

// Source supplies everything a tick needs to observe the system.
type Source interface {
	Providers(ctx context.Context, now time.Time) ([]ProviderObservation, error)
	Keys(ctx context.Context, now time.Time) ([]KeyObservation, error)
	Windows(ctx context.Context, now time.Time) (map[string]map[int]WindowStats, error)
	Baselines(ctx context.Context, now time.Time) (map[string]int64, error)
	Quota(ctx context.Context, provider ProviderObservation) (*QuotaSnapshot, error)
	MultiIP(ctx context.Context, now time.Time) ([]MultiIPHit, error)
	ConcurrentIP(ctx context.Context, now time.Time) ([]ConcurrentIPHit, error)
	LastRenewal(ctx context.Context, providerID string) (RenewalOutcome, error)
	ModelFailures(ctx context.Context, providerID string) (int, error)
}

type StateStore interface {
	LoadMonitorStates() ([]AlertState, error)
	UpsertMonitorState(AlertState) (AlertState, error)
	InsertMonitorEvent(Event) (int64, error)
	LatestMonitorEventID(fingerprint string) (int64, error)
	UpdateMonitorEventPush(int64, string, string) error
	UpsertMonitorEventPush(EventPush) error
	ListMonitorEventPushes(int64) ([]EventPush, error)
	SetMonitorDisposition(int64, string, time.Time) error
	MonitorAlertByID(int64) (AlertState, bool, error)
}

type ConfigStore interface {
	Setting(string) string
	SetSetting(string, string) error
	InsertMonitorEvent(Event) (int64, error)
}

type SampleSink interface {
	UpsertProviderMetricSample(MetricSample) error
}

type BaselineComputer interface {
	RecomputeTTFTBaselines(now time.Time, loc *time.Location) error
}
