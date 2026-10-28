//go:build windows

package gateway

import (
	"runtime"
	"time"
)

type windowsSampleState struct {
	gopsutilSampleState
}

func collectHostMetrics(prev any) hostMetricsCollectResult {
	started := time.Now()
	m := HostMetrics{
		CPUCount:    runtime.NumCPU(),
		CollectedAt: started,
	}
	state, _ := prev.(windowsSampleState)
	next := collectHostMetricsExtra(&m, state.gopsutilSampleState)
	if m.UptimeSeconds == 0 && !m.BootTime.IsZero() {
		m.UptimeSeconds = time.Since(m.BootTime).Seconds()
	}
	m.CPULoad = loadPercent(m.Load1, m.CPUCount)
	m.finishProcess()
	m.SampleCost = sampleCostMS(started)
	if m.ProcessStarted.IsZero() {
		m.ProcessStarted = processStartedAt
	}
	return hostMetricsCollectResult{metrics: m, prev: windowsSampleState{gopsutilSampleState: next}}
}

// applyDiskRoot is a no-op on Windows: volumes come from gopsutil and the
// first volume is the system drive.
func (m *HostMetrics) applyDiskRoot() {}

// processOSThreads on Windows would need NtQueryInformationProcess; the Go
// runtime's count is the only thread figure this build reports.
func processOSThreads() int {
	return 0
}

// statfsVolume is unused on Windows: volumes come from gopsutil.
func statfsVolume(string) (total, free uint64, ok bool) { return 0, 0, false }
