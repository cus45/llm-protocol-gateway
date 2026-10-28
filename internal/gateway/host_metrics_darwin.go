//go:build darwin

package gateway

import (
	"context"
	"encoding/binary"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var (
	darwinTempMu     sync.Mutex
	darwinTempCached float64
	darwinTempSource string
	darwinTempOK     bool
	darwinTempAt     time.Time
	darwinTempFailAt time.Time

	darwinPressureCached string
	darwinPressureSource string
	darwinPressureOK     bool
	darwinPressureAt     time.Time
	darwinPressureFailAt time.Time
)

// Successful °C reads are cached this long.
const darwinTempMinInterval = 30 * time.Second

// After a failed °C probe, wait before retrying (e.g. user just added sudoers).
const darwinTempRetryAfter = 2 * time.Minute

// Thermal pressure is cheap; refresh a bit more often than °C.
const darwinPressureMinInterval = 20 * time.Second
const darwinPressureRetryAfter = 2 * time.Minute

func collectHostMetrics(prev any) hostMetricsCollectResult {
	started := time.Now()
	ncpu := runtime.NumCPU()
	m := HostMetrics{
		CPUCount:    ncpu,
		CollectedAt: started,
	}
	m.Load1, m.Load5, m.Load15 = darwinLoadAvg()
	m.CPULoad = loadPercent(m.Load1, ncpu)
	// CPUPercent is filled from tick deltas by the gopsutil pass below; this is
	// only the first-sample fallback.
	m.CPUPercent = m.CPULoad

	// hw.memsize is the authoritative capacity: the page sums miss a few
	// kernel-reserved pages and would understate installed RAM.
	memCapacity, _ := unix.SysctlUint64("hw.memsize")
	m.UptimeSeconds = darwinUptimeSeconds()
	m.BootTime = time.Now().Add(-time.Duration(m.UptimeSeconds * float64(time.Second))).Truncate(time.Second)

	var prevState any
	if p, ok := prev.(gopsutilSampleState); ok {
		prevState = p
	}
	next := collectHostMetricsExtra(&m, prevState)
	if m.MemTotal < memCapacity {
		m.MemTotal = memCapacity
		m.MemPercent = percentOf(m.MemUsed, m.MemTotal)
	}
	m.finishProcess()

	cur := sumInterfaceCounters(darwinReadInterfaceCounters())
	var prevNet netCounters
	if p, ok := prev.(gopsutilSampleState); ok {
		prevNet = p.net
	}
	m.applyNet(prevNet, cur)
	m.SampleCost = sampleCostMS(started)
	if cur.ok {
		next.net = cur
	}
	return hostMetricsCollectResult{metrics: m, prev: next}
}

// darwinUptimeSeconds derives host uptime from kern.boottime.
func darwinUptimeSeconds() float64 {
	raw, err := unix.SysctlRaw("kern.boottime")
	if err != nil || len(raw) < 8 {
		return 0
	}
	sec := int64(binary.LittleEndian.Uint64(raw[0:8]))
	if sec <= 0 {
		return 0
	}
	up := time.Since(time.Unix(sec, 0)).Seconds()
	if up < 0 {
		return 0
	}
	return up
}

func darwinLoadAvg() (float64, float64, float64) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < 20 {
		return 0, 0, 0
	}
	ld0 := binary.LittleEndian.Uint32(raw[0:4])
	ld1 := binary.LittleEndian.Uint32(raw[4:8])
	ld2 := binary.LittleEndian.Uint32(raw[8:12])
	var scale int64
	if len(raw) >= 24 {
		scale = int64(binary.LittleEndian.Uint64(raw[16:24]))
	} else {
		scale = int64(binary.LittleEndian.Uint32(raw[12:16]))
	}
	if scale <= 0 {
		scale = 1
	}
	return float64(ld0) / float64(scale), float64(ld1) / float64(scale), float64(ld2) / float64(scale)
}

func darwinRunPowermetrics(ctx context.Context, sampler string) ([]byte, error) {
	candidates := [][]string{
		{"sudo", "-n", "powermetrics", "--samplers", sampler, "-i100", "-n1"},
		{"powermetrics", "--samplers", sampler, "-i100", "-n1"},
	}
	var lastErr error
	for _, args := range candidates {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return out, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// tempSensorReading is one named °C sample coming from IOKit AppleSensors.
type tempSensorReading struct {
	Name  string
	Value float64
}

// darwinCPUTempC reads the CPU/SoC die temperature.
//
// Order matters: IOKit AppleSensors works for a plain (non-root) user on both
// Apple Silicon and Intel, while powermetrics needs passwordless sudo and its
// "smc" Celsius output was removed on recent macOS. IOKit first means the
// homepage shows real °C out of the box.
func darwinCPUTempC() (float64, string, bool) {
	darwinTempMu.Lock()
	defer darwinTempMu.Unlock()
	if darwinTempOK && !darwinTempAt.IsZero() && time.Since(darwinTempAt) < darwinTempMinInterval {
		return darwinTempCached, darwinTempSource, true
	}
	if !darwinTempOK && !darwinTempFailAt.IsZero() && time.Since(darwinTempFailAt) < darwinTempRetryAfter {
		return 0, "", false
	}

	if temp, source, ok := pickCPUTempFromSensors(darwinReadTempSensors()); ok {
		darwinTempCached = temp
		darwinTempSource = source
		darwinTempOK = true
		darwinTempAt = time.Now()
		darwinTempFailAt = time.Time{}
		return temp, source, true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := darwinRunPowermetrics(ctx, "smc")
	if err == nil {
		if temp, ok := parsePowermetricsCPUTemp(out); ok {
			darwinTempCached = temp
			darwinTempSource = "powermetrics/smc"
			darwinTempOK = true
			darwinTempAt = time.Now()
			darwinTempFailAt = time.Time{}
			return temp, darwinTempSource, true
		}
	}
	darwinTempOK = false
	darwinTempFailAt = time.Now()
	return 0, "", false
}

// sensor name groups, most CPU-specific first.
var darwinCPUSensorGroups = []struct {
	source string
	match  func(lower string) bool
}{
	// Apple Silicon efficiency/performance core clusters (M1/M2/M3 Pro/Max/Ultra).
	{"iokit/cpu-cluster", func(l string) bool {
		return strings.Contains(l, "acc mtr temp") || // eACC / pACC MTR Temp Sensor
			(strings.Contains(l, "cpu") && strings.Contains(l, "temp"))
	}},
	// Intel Macs expose SMC keys such as "TC0P"/"CPU Proximity".
	{"iokit/cpu", func(l string) bool { return strings.HasPrefix(l, "tc0") || strings.Contains(l, "cpu") }},
	// Apple Silicon SoC die sensors (M1/M2/M3 base chips only expose these).
	{"iokit/soc-die", func(l string) bool {
		return strings.Contains(l, "tdie") || strings.Contains(l, "soc mtr temp")
	}},
	// Last resort: the GPU/SoC package sensors still track the die closely.
	{"iokit/soc", func(l string) bool {
		return strings.Contains(l, "gpu") || strings.Contains(l, "tcal") || strings.Contains(l, "pmu")
	}},
}

// pickCPUTempFromSensors averages the most CPU-relevant sensor group.
// Averaging (instead of max) matches what macOS monitoring tools report and
// smooths out single-sensor spikes.
func pickCPUTempFromSensors(readings []tempSensorReading) (float64, string, bool) {
	if len(readings) == 0 {
		return 0, "", false
	}
	for _, group := range darwinCPUSensorGroups {
		sum, n := 0.0, 0
		for _, r := range readings {
			if !isPlausibleCPUTemp(r.Value) {
				continue
			}
			if group.match(strings.ToLower(r.Name)) {
				sum += r.Value
				n++
			}
		}
		if n > 0 {
			return roundTemp(sum / float64(n)), group.source, true
		}
	}
	return 0, "", false
}

func isPlausibleCPUTemp(v float64) bool { return v > 1 && v < 150 }

// darwinThermalPressure reads Nominal/Fair/Serious/Critical via the "thermal" sampler
// (still present on macOS 15+/26 when "smc" Celsius is gone).
func darwinThermalPressure() (string, string, bool) {
	darwinTempMu.Lock()
	defer darwinTempMu.Unlock()
	if darwinPressureOK && !darwinPressureAt.IsZero() && time.Since(darwinPressureAt) < darwinPressureMinInterval {
		return darwinPressureCached, darwinPressureSource, true
	}
	if !darwinPressureOK && !darwinPressureFailAt.IsZero() && time.Since(darwinPressureFailAt) < darwinPressureRetryAfter {
		return "", "", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := darwinRunPowermetrics(ctx, "thermal")
	if err == nil {
		if level, ok := parsePowermetricsThermalPressure(out); ok {
			darwinPressureCached = level
			darwinPressureSource = "powermetrics/thermal"
			darwinPressureOK = true
			darwinPressureAt = time.Now()
			darwinPressureFailAt = time.Time{}
			return level, darwinPressureSource, true
		}
	}
	darwinPressureOK = false
	darwinPressureFailAt = time.Now()
	return "", "", false
}
