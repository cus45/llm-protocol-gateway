package gateway

import (
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/store"
)

// runtimeGOOS mirrors runtime.GOOS so the process helpers can branch without
// importing runtime in every platform file.
const runtimeGOOS = runtime.GOOS

// The collectors run on Linux (/proc + /sys), macOS (sysctl + IOKit) and
// Windows (gopsutil: Win32 counters). Anything a platform cannot measure stays
// zero with its `*Available` flag false, so the console hides that card instead
// of drawing a misleading flat line.

// parseUnixLoadavg reads "l1 l5 l15 …" as produced by /proc/loadavg and by
// `uptime` on macOS.
func parseUnixLoadavg(content string) (l1, l5, l15 float64) {
	fields := strings.Fields(content)
	if len(fields) < 3 {
		return 0, 0, 0
	}
	parse := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || v < 0 {
			return 0
		}
		return v
	}
	return parse(fields[0]), parse(fields[1]), parse(fields[2])
}

// loadPercent expresses the 1-minute load average against core count. On a
// quiet box it under-reads a two-second CPU sample, so it is only used when no
// tick delta exists yet.
func loadPercent(load float64, cores int) float64 {
	if cores <= 0 {
		return 0
	}
	return clampFloat(load/float64(cores)*100, 0, 100)
}

// finishProcess adds the host identity and Go runtime details shared by every
// platform collector, then normalises swap and hit-rate fields.
func (m *HostMetrics) finishProcess() {
	if name, err := os.Hostname(); err == nil {
		m.Hostname = name
	}
	m.Platform = runtime.GOOS + "/" + runtime.GOARCH
	m.Goroutines = runtime.NumGoroutine()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	m.ProcessHeapMiB = float64(ms.HeapAlloc) / (1024 * 1024)
	m.ProcessSysMiB = float64(ms.Sys) / (1024 * 1024)
	m.ProcessUptime = time.Since(processStartedAt).Seconds()
	m.applyRuntimeInUse()
	m.applyDBFile()
	for index := range m.Volumes {
		m.Volumes[index].Percent = percentOf(m.Volumes[index].Used, m.Volumes[index].Total)
	}
}

// applyRuntimeInUse adds the Go runtime's own footprint; only Linux reads the
// real process RSS from /proc, so elsewhere the runtime numbers are the best
// available answer (and the UI labels their source).
func (m *HostMetrics) applyRuntimeInUse() {
	if m.ProcessRSSMiB > 0 {
		return
	}
	m.ProcessRSSMiB = m.ProcessSysMiB
	m.ProcessRSSFromRuntime = true
	m.ProcessThreads = m.OSThreads
}

func minU64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func percentOf(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return clampFloat(float64(used)/float64(total)*100, 0, 100)
}

// diskIOAverages turns two cumulative disk snapshots into rates. reset reports
// a counter reset (reboot / device hotplug) instead of a bogus spike, so the
// caller drops the stale baseline.
func diskIOAverages(prev, cur diskCounters) (out HostDiskIO, reset, ok bool) {
	if cur.root == "" || !prev.usable() || !cur.usable() {
		return HostDiskIO{}, false, false
	}
	elapsedMS := cur.at.Sub(prev.at).Milliseconds()
	if elapsedMS <= 0 || elapsedMS > 600_000 {
		return HostDiskIO{}, false, false
	}
	prevSectors, okPrev := prev.sectors[cur.root]
	curSectors, okCur := cur.sectors[cur.root]
	if !okPrev || !okCur {
		return HostDiskIO{}, false, false
	}
	if curSectors < prevSectors || cur.busy[cur.root] < prev.busy[cur.root] ||
		cur.done[cur.root] < prev.done[cur.root] || cur.requests[cur.root] < prev.requests[cur.root] {
		return HostDiskIO{}, true, false
	}
	deltaSectors := float64(curSectors - prevSectors)
	deltaBusy := float64(cur.busy[cur.root] - prev.busy[cur.root])
	deltaServiceMS := float64(cur.done[cur.root] - prev.done[cur.root])
	deltaRequests := float64(cur.requests[cur.root] - prev.requests[cur.root])
	out = HostDiskIO{
		Device:   cur.root,
		ReadRate: deltaSectors * 512 / (float64(elapsedMS) / 1000),
		Util:     clampFloat(deltaBusy/float64(elapsedMS)*100, 0, 100),
	}
	if deltaRequests > 0 {
		out.AwaitMs = deltaServiceMS / deltaRequests
	}
	return out, false, true
}

// sampleCostMS is the wall time a collect call took, shown as "采样耗时".
func sampleCostMS(started time.Time) float64 {
	return float64(time.Since(started).Microseconds()) / 1000
}

// diskCounters is a cumulative per-device snapshot used to derive IO rates.
// Counters are summed at the physical disk behind "/", not per partition.
type diskCounters struct {
	root     string
	sectors  map[string]uint64 // 512-byte sectors read + written
	done     map[string]uint64 // ms spent serving completed IO (for await)
	busy     map[string]uint64 // ms with at least one request in flight
	requests map[string]uint64 // completed requests
	at       time.Time
}

func (d diskCounters) usable() bool {
	return d.at.IsZero() == false && len(d.sectors) > 0
}

// sortVolumes puts "/" first, then shallow mounts before deep ones.
func sortVolumes(volumes []HostDisk) {
	sort.SliceStable(volumes, func(a, b int) bool {
		if (volumes[a].Mount == "/") != (volumes[b].Mount == "/") {
			return volumes[a].Mount == "/"
		}
		if da, db := strings.Count(volumes[a].Mount, "/"), strings.Count(volumes[b].Mount, "/"); da != db {
			return da < db
		}
		return volumes[a].Mount < volumes[b].Mount
	})
}

// volumeByMount finds the volume for a mount point (nil when absent).
func volumeByMount(volumes []HostDisk, mount string) *HostDisk {
	for index := range volumes {
		if volumes[index].Mount == mount {
			return &volumes[index]
		}
	}
	return nil
}

// applyDBFile reports the SQLite file's size and last write — a jump here is
// usually the request log or usage buckets growing.
func (m *HostMetrics) applyDBFile() {
	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return
	}
	m.DBFileBytes = uint64(info.Size())
	m.DBFileUpdated = info.ModTime()
}

// processSelfSample is the gateway's own cumulative CPU time, the baseline for
// its share of host CPU. Every platform fills it (Linux from /proc, Windows and
// macOS from the gopsutil process query).
type processSelfSample struct {
	cpuSeconds float64
	at         time.Time
	ok         bool
}

// applyProcessCPUDelta turns two cumulative process samples into a percentage
// of one core (100 = one core saturated).
func applyProcessCPUDelta(m *HostMetrics, prev, cur processSelfSample) {
	if !cur.ok || !prev.ok || cur.cpuSeconds < prev.cpuSeconds {
		return
	}
	elapsed := cur.at.Sub(prev.at).Seconds()
	if elapsed <= 0 {
		return
	}
	m.ProcessCPUPct = clampFloat((cur.cpuSeconds-prev.cpuSeconds)/elapsed*100, 0, 100)
}
