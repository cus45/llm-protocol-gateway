package monitoring

import (
	"context"
	"sync"
	"time"
)

const (
	quotaCacheTTL        = 15 * time.Minute
	modelFailureCacheTTL = 15 * time.Minute
)

// Collector assembles one Snapshot from the Source. Providers and Windows are
// core: a failure there withholds the heartbeat so collector_stale can fire.
// Auxiliary sources only annotate DataErrors.
type Collector struct {
	source        Source
	store         StateStore
	quota         map[string]*QuotaSnapshot
	quotaAt       time.Time
	modelFailures map[string]int
	modelAt       time.Time
	lastSuccess   time.Time
	mu            sync.Mutex
	now           func() time.Time
}

func NewCollector(source Source, store StateStore) *Collector {
	return &Collector{
		source:        source,
		store:         store,
		quota:         map[string]*QuotaSnapshot{},
		modelFailures: map[string]int{},
		now:           time.Now,
	}
}

func (c *Collector) Snapshot(ctx context.Context, now time.Time) (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	snap := Snapshot{Now: now, Windows: map[string]map[int]WindowStats{}, DataErrors: []string{}}
	var coreErr error

	providers, err := c.source.Providers(ctx, now)
	if err != nil {
		coreErr = err
		snap.DataErrors = append(snap.DataErrors, "providers: "+err.Error())
	} else {
		snap.Providers = providers
	}
	windows, err := c.source.Windows(ctx, now)
	if err != nil {
		if coreErr == nil {
			coreErr = err
		}
		snap.DataErrors = append(snap.DataErrors, "windows: "+err.Error())
	} else if windows != nil {
		snap.Windows = windows
	}
	if keys, err := c.source.Keys(ctx, now); err != nil {
		snap.DataErrors = append(snap.DataErrors, "keys: "+err.Error())
	} else {
		snap.Keys = keys
	}
	baselines, err := c.source.Baselines(ctx, now)
	if err != nil {
		snap.DataErrors = append(snap.DataErrors, "baselines: "+err.Error())
	}

	needQuota := c.quotaAt.IsZero() || now.Sub(c.quotaAt) >= quotaCacheTTL
	needModelFailures := c.modelAt.IsZero() || now.Sub(c.modelAt) >= modelFailureCacheTTL
	nextQuota := map[string]*QuotaSnapshot{}
	for _, provider := range snap.Providers {
		previous := c.quota[provider.ID]
		if needQuota && provider.Capabilities.Quota {
			quota, err := c.source.Quota(ctx, provider)
			switch {
			case err != nil && previous != nil:
				stale := *previous
				stale.Stale = true
				stale.Error = err.Error()
				nextQuota[provider.ID] = &stale
			case err != nil:
				snap.DataErrors = append(snap.DataErrors, "quota "+provider.ID+": "+err.Error())
			case quota != nil:
				nextQuota[provider.ID] = quota
			}
		} else if previous != nil {
			nextQuota[provider.ID] = previous
		}
		if needModelFailures {
			if failures, err := c.source.ModelFailures(ctx, provider.ID); err == nil {
				c.modelFailures[provider.ID] = failures
			}
		}
	}
	if needQuota {
		c.quota = nextQuota
		c.quotaAt = now
	}
	if needModelFailures {
		c.modelAt = now
	}

	multiHits, err := c.source.MultiIP(ctx, now)
	if err != nil {
		snap.DataErrors = append(snap.DataErrors, "multi_ip: "+err.Error())
	}
	concurrentHits, err := c.source.ConcurrentIP(ctx, now)
	if err != nil {
		snap.DataErrors = append(snap.DataErrors, "concurrent_ip: "+err.Error())
	}
	multiByKey := make(map[string]MultiIPHit, len(multiHits))
	for _, hit := range multiHits {
		multiByKey[hit.APIKeyID] = hit
	}
	concurrentByKey := make(map[string]ConcurrentIPHit, len(concurrentHits))
	for _, hit := range concurrentHits {
		concurrentByKey[hit.APIKeyID] = hit
	}

	for i := range snap.Providers {
		provider := &snap.Providers[i]
		if quota := c.quota[provider.ID]; quota != nil {
			provider.Quota = quota
		}
		if baseline, ok := baselines[provider.ID]; ok {
			provider.BaselineTTFTP95 = baseline
		}
		if renewal, err := c.source.LastRenewal(ctx, provider.ID); err == nil {
			provider.Renewal = renewal
		} else {
			snap.DataErrors = append(snap.DataErrors, "renewal "+provider.ID+": "+err.Error())
		}
		provider.ModelFailures = c.modelFailures[provider.ID]
	}
	for i := range snap.Keys {
		key := &snap.Keys[i]
		if hit, ok := multiByKey[key.ID]; ok {
			key.MultiIPs = hit.IPs
			key.MultiIPCount = hit.IPCount
		}
		if hit, ok := concurrentByKey[key.ID]; ok {
			key.ConcurrentIPs = hit.IPCount
			key.ConcurrentAt = hit.At
		}
	}

	if sink, ok := c.store.(SampleSink); ok {
		hour := now.UTC().Truncate(time.Hour).Format(time.RFC3339Nano)
		for _, provider := range snap.Providers {
			sample := MetricSample{
				ProviderID:    provider.ID,
				Hour:          hour,
				AuthExpiresAt: formatSampleTime(provider.ExpiresAt),
				HealthStatus:  provider.HealthStatus,
				ObservedAt:    now,
			}
			if provider.Quota != nil {
				if provider.Quota.Available() {
					ratio := provider.Quota.Ratio
					sample.QuotaRatio = &ratio
				}
				sample.SourceError = provider.Quota.Error
			}
			_ = sink.UpsertProviderMetricSample(sample)
		}
	}

	if coreErr != nil {
		snap.CollectorLastSuccess = c.lastSuccess
		return snap, coreErr
	}
	c.lastSuccess = now
	snap.CollectorLastSuccess = now
	return snap, nil
}

func formatSampleTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
