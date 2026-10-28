package gateway

import (
	"runtime"
	"testing"
	"time"
)

func TestDiagnoseHostFlagsSustainedPressure(t *testing.T) {
	prev := HostMetrics{CollectedAt: time.Unix(1_700_000_000, 0)}
	cur := HostMetrics{
		CollectedAt:       prev.CollectedAt.Add(time.Second),
		CPUCount:          4,
		Load1:             6.2,
		CPUPercent:        96,
		MemAvailable:      true,
		MemTotal:          4 * 1024 * 1024 * 1024,
		MemUsed:           4240 * 1024 * 1024,
		MemPercent:        98.7,
		DiskAvailable:     true,
		DiskPercent:       96,
		DiskDevice:        "vda",
		ProcessCPUPct:     80,
		NetAvailable:      true,
		NetDrops:          20,
		PressureAvailable: true,
		PressureIO:        30,
	}
	prev.NetAvailable = true
	prev.NetDrops = 12
	s := &Server{providerLanes: newProviderLanes()}
	got := s.diagnoseHost(prev, cur, time.Second)
	if got.Severity != "critical" {
		t.Fatalf("severity=%q verdict=%q", got.Severity, got.Verdict)
	}
	ids := map[string]string{}
	for _, finding := range got.Findings {
		ids[finding.ID] = finding.Severity
	}
	for _, want := range []string{"cpu-load", "cpu-usage", "mem", "disk", "process-cpu", "net-drops", "psi-io"} {
		if _, ok := ids[want]; !ok {
			t.Fatalf("missing finding %q in %v", want, ids)
		}
	}
	if ids["mem"] != "critical" || ids["disk"] != "critical" {
		t.Fatalf("memory/disk must be critical: %v", ids)
	}
	if got.Findings[0].Severity != "critical" {
		t.Fatalf("findings must be sorted by severity, first=%q", got.Findings[0].Severity)
	}
}

func TestDiagnoseHostReportsHealthyBox(t *testing.T) {
	prev := HostMetrics{CollectedAt: time.Unix(1_700_000_000, 0)}
	cur := HostMetrics{
		CollectedAt:   prev.CollectedAt.Add(time.Second),
		CPUCount:      8,
		Load1:         0.4,
		CPUPercent:    6,
		MemAvailable:  true,
		MemTotal:      8 << 30,
		MemUsed:       2 << 30,
		MemPercent:    25,
		DiskAvailable: true,
		DiskPercent:   40,
	}
	got := (&Server{}).diagnoseHost(prev, cur, time.Second)
	if got.Severity != "ok" || len(got.Findings) != 1 || got.Findings[0].ID != "ok" {
		t.Fatalf("diagnosis=%+v", got)
	}
	if got.Gateway.Goroutines == 0 {
		t.Fatal("gateway activity must always carry the goroutine count")
	}
}

func TestHostDiagnosticsBundleCarriesRuntimeContext(t *testing.T) {
	prev := HostMetrics{CollectedAt: time.Unix(1_700_000_000, 0)}
	cur := HostMetrics{CollectedAt: prev.CollectedAt.Add(time.Second), CPUCount: runtime.NumCPU()}
	bundle := (&Server{}).hostDiagnosticsBundle(t.Context(), prev, cur, time.Second)
	for _, key := range []string{"generatedAt", "metrics", "previous", "diagnosis", "runtimeStats"} {
		if _, ok := bundle[key]; !ok {
			t.Fatalf("bundle missing %q: %v", key, bundle)
		}
	}
	stats, ok := bundle["runtimeStats"].(map[string]any)
	if !ok || stats["goroutines"] == nil || stats["heapAllocMiB"] == nil {
		t.Fatalf("runtimeStats=%v", bundle["runtimeStats"])
	}
}

func TestFormatMiB(t *testing.T) {
	if got := formatMiB(512 * 1024 * 1024); got != "512 MiB" {
		t.Fatalf("512MiB = %q", got)
	}
	if got := formatMiB(3 * 1024 * 1024 * 1024); got != "3.0 GiB" {
		t.Fatalf("3GiB = %q", got)
	}
}
