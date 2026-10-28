//go:build !darwin && !linux && !windows

package gateway

import (
	"runtime"
	"time"
)

// Platforms without a collector (BSD, plan9, …) still get host identity and
// the Go runtime's own numbers; hardware metrics stay unavailable.
func collectHostMetrics(prev any) hostMetricsCollectResult {
	_ = prev
	m := HostMetrics{
		CPUCount:    runtime.NumCPU(),
		CollectedAt: time.Now(),
	}
	m.applyDiskRoot()
	m.finishProcess()
	return hostMetricsCollectResult{metrics: m}
}

// applyDiskRoot has no filesystem probe on these platforms.
func (m *HostMetrics) applyDiskRoot() {}

// statfsRoot / statfsVolume are unsupported outside darwin/linux.
func statfsRoot() (total, used uint64, ok bool)               { return 0, 0, false }
func statfsVolume(mount string) (total, free uint64, ok bool) { return 0, 0, false }
func processOSThreads() int                                   { return 0 }
