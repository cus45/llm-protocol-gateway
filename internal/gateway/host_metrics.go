package gateway

import (
	"bytes"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// hostMetricsPollInterval is how often the sampler refreshes while at least one
// console client is actively requesting /__host-metrics. The console charts one
// point per poll, so this is also the chart's resolution.
const hostMetricsPollInterval = 3 * time.Second

// hostMetricsIdleStopAfter stops the background sampler when no client has
// polled for this long (≈3 missed polls). Keeps CPU near zero when the
// homepage is closed or the tab is backgrounded.
const hostMetricsIdleStopAfter = 30 * time.Second

// HostMetrics is one snapshot of the host the gateway runs on. It is only
// collected while an admin console page is polling it (see the sampler below).
type HostMetrics struct {
	Load1             float64       `json:"load1"`
	Load5             float64       `json:"load5"`
	Load15            float64       `json:"load15"`
	CPUPercent        float64       `json:"cpuPercent"`     // 0–100, all-core average
	CPULoad           float64       `json:"cpuLoadPercent"` // load1 ÷ cores
	CPUCount          int           `json:"cpuCount"`
	CPUMHz            float64       `json:"cpuMhz,omitempty"`
	CPUPhysicalCores  int           `json:"cpuPhysicalCores,omitempty"` // 0 when the platform cannot tell
	CPUModel          string        `json:"cpuModel,omitempty"`
	CPUCores          []HostCPUCore `json:"cpuCores,omitempty"`
	CPUCoresAvailable bool          `json:"cpuCoresAvailable"`

	TempC                    float64 `json:"tempC,omitempty"`
	TempAvailable            bool    `json:"tempAvailable"`
	TempSource               string  `json:"tempSource,omitempty"`
	ThermalPressure          string  `json:"thermalPressure,omitempty"`
	ThermalPressureAvailable bool    `json:"thermalPressureAvailable"`
	ThermalPressureSource    string  `json:"thermalPressureSource,omitempty"`

	// Memory (bytes). "Used" matches `free` (total − available), so page cache
	// is counted as available; MemCached is that cache for the composition bar.
	MemTotal       uint64  `json:"memTotal,omitempty"`
	MemUsed        uint64  `json:"memUsed,omitempty"`
	MemPercent     float64 `json:"memPercent,omitempty"`
	MemAvailable   bool    `json:"memAvailable"`
	MemCached      uint64  `json:"memCached,omitempty"`
	MemAnon        uint64  `json:"memAnon,omitempty"`
	MemSlab        uint64  `json:"memSlab,omitempty"`
	HostMemHitRate float64 `json:"hostMemHitRate,omitempty"` // page-cache hit, −1 when unknown
	SwapTotal      uint64  `json:"swapTotal,omitempty"`
	SwapUsed       uint64  `json:"swapUsed,omitempty"`
	SwapAvailable  bool    `json:"swapAvailable"`

	// Root filesystem (bytes) plus every mounted volume worth showing.
	DiskTotal     uint64     `json:"diskTotal,omitempty"`
	DiskUsed      uint64     `json:"diskUsed,omitempty"`
	DiskPercent   float64    `json:"diskPercent,omitempty"`
	DiskAvailable bool       `json:"diskAvailable"`
	DiskDevice    string     `json:"diskDevice,omitempty"` // physical disk behind "/"
	Volumes       []HostDisk `json:"volumes,omitempty"`

	// Disk throughput of DiskDevice, derived from two samples.
	DiskIO        *HostDiskIO `json:"diskIO,omitempty"`
	DiskReadRate  float64     `json:"diskReadRate,omitempty"`  // bytes/s
	DiskWriteRate float64     `json:"diskWriteRate,omitempty"` // bytes/s
	DiskIOREady   bool        `json:"diskIOReady"`

	// Physical disk temperatures (SMART / NVMe), when smartctl is available.
	DiskTemps []HostDiskTemp `json:"diskTemps,omitempty"`

	// Chassis / CPU fans (SMC on macOS, hwmon on Linux).
	Fans []HostFanSpeed `json:"fans,omitempty"`

	// Network counters since boot + per-second rates across samples. Only
	// physical interfaces are summed (loopback / tunnels would double count).
	NetRxBytes       uint64  `json:"netRxBytes,omitempty"`
	NetTxBytes       uint64  `json:"netTxBytes,omitempty"`
	NetRxRate        float64 `json:"netRxRate,omitempty"` // bytes/s
	NetTxRate        float64 `json:"netTxRate,omitempty"`
	NetRxPacketsRate float64 `json:"netRxPacketsRate,omitempty"`
	NetTxPacketsRate float64 `json:"netTxPacketsRate,omitempty"`
	NetRxErrors      uint64  `json:"netRxErrors,omitempty"`
	NetTxErrors      uint64  `json:"netTxErrors,omitempty"`
	NetDrops         uint64  `json:"netDrops,omitempty"`
	NetRateReady     bool    `json:"netRateReady"` // false on the very first sample
	NetAvailable     bool    `json:"netAvailable"`
	NetInterfaces    int     `json:"netInterfaces,omitempty"`

	// PSI (Linux): share of wall time some task was stalled on CPU / memory /
	// IO. Zero is a real reading, hence the separate availability flag.
	PressureCPU       float64 `json:"pressureCpu,omitempty"`
	PressureCPUFull   float64 `json:"pressureCpuFull,omitempty"`
	PressureMem       float64 `json:"pressureMem,omitempty"`
	PressureMemFull   float64 `json:"pressureMemFull,omitempty"`
	PressureIO        float64 `json:"pressureIo,omitempty"`
	PressureIOFull    float64 `json:"pressureIoFull,omitempty"`
	PressureAvailable bool    `json:"pressureAvailable"`

	// Host / OS identity.
	Hostname      string    `json:"hostname,omitempty"`
	Platform      string    `json:"platform,omitempty"` // e.g. "linux/amd64"
	OSName        string    `json:"osName,omitempty"`   // "Ubuntu 24.04.4 LTS"
	Kernel        string    `json:"kernel,omitempty"`
	BootTime      time.Time `json:"bootTime"`
	UptimeSeconds float64   `json:"uptimeSeconds,omitempty"`

	// Gateway process.
	ProcessUptime         float64   `json:"processUptimeSeconds,omitempty"`
	ProcessStarted        time.Time `json:"processStartedAt,omitempty"`
	ProcessRestarts       int       `json:"processRestarts,omitempty"`
	Goroutines            int       `json:"goroutines,omitempty"`
	OSThreads             int       `json:"osThreads,omitempty"`
	ProcessHeapMiB        float64   `json:"processHeapMiB,omitempty"`
	ProcessSysMiB         float64   `json:"processSysMiB,omitempty"`
	ProcessRSSMiB         float64   `json:"processRssMiB,omitempty"`
	ProcessRSSFromRuntime bool      `json:"processRssFromRuntime"`
	ProcessPeakMiB        float64   `json:"processPeakMiB,omitempty"`
	ProcessThreads        int       `json:"processThreads,omitempty"`
	ProcessFDs            int       `json:"processFds,omitempty"`
	ProcessCPUPct         float64   `json:"processCpuPercent,omitempty"`
	DBFileBytes           uint64    `json:"dbFileBytes,omitempty"`
	DBFileUpdated         time.Time `json:"dbFileUpdated,omitempty"`

	// Container memory limit (cgroup v2 / v1), when one applies.
	CgroupMemLimit uint64 `json:"cgroupMemLimit,omitempty"`
	CgroupMemUsed  uint64 `json:"cgroupMemUsed,omitempty"`

	SampleCost  float64   `json:"sampleCostMs,omitempty"`
	CollectedAt time.Time `json:"collectedAt"` // omitempty has no effect on time.Time
}

// HostCPUCore is one logical CPU's utilisation for the page's per-core strip.
type HostCPUCore struct {
	ID      int     `json:"id"`
	Percent float64 `json:"percent"`
}

// HostDisk is one filesystem ("/" for the root, "/boot", …) with its usage.
type HostDisk struct {
	Mount   string  `json:"mount"`
	Device  string  `json:"device,omitempty"`
	FSType  string  `json:"fstype,omitempty"`
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Percent float64 `json:"percent"`
}

// HostDiskIO is the root disk's activity between the last two samples.
type HostDiskIO struct {
	Device    string  `json:"device"`
	ReadRate  float64 `json:"readRate"`  // bytes/s
	WriteRate float64 `json:"writeRate"` // bytes/s
	Util      float64 `json:"util"`      // % of wall time with requests in flight
	AwaitMs   float64 `json:"awaitMs"`   // ms per completed request
}

// HostDiskTemp is one physical disk's temperature reading for the machine page.
type HostDiskTemp struct {
	Device   string  `json:"device"`             // e.g. disk0 / nvme0n1
	Model    string  `json:"model,omitempty"`    // SMART / MediaName
	TempC    float64 `json:"tempC"`              // Celsius
	Internal *bool   `json:"internal,omitempty"` // darwin: built-in vs external
	Source   string  `json:"source,omitempty"`   // smartctl/nvme, smartctl/ata, …
}

// HostFanSpeed is one chassis/CPU fan reading for the machine page.
type HostFanSpeed struct {
	ID      int     `json:"id"`
	Name    string  `json:"name,omitempty"`
	RPM     float64 `json:"rpm"`
	MinRPM  float64 `json:"minRpm,omitempty"`
	MaxRPM  float64 `json:"maxRpm,omitempty"`
	Percent float64 `json:"percent"` // 0–100, relative to min..max (or 0..max)
	Source  string  `json:"source,omitempty"`
}

// fanSpeedPercent maps actual RPM into 0–100 using the fan's usable range.
func fanSpeedPercent(rpm, minRPM, maxRPM float64) float64 {
	if maxRPM > minRPM && minRPM >= 0 {
		return clampFloat((rpm-minRPM)/(maxRPM-minRPM)*100, 0, 100)
	}
	if maxRPM > 0 {
		return clampFloat(rpm/maxRPM*100, 0, 100)
	}
	return 0
}

// netCounters is a cumulative counter snapshot used to derive per-second rates.
type netCounters struct {
	rx, tx                    uint64
	rxPkts, txPkts            uint64
	rxErrors, txErrors, drops uint64
	interfaces                int
	at                        time.Time
	ok                        bool
}

// netRates converts two cumulative snapshots into bytes/s. Counter resets
// (reboot, interface flap) yield ok=false instead of a bogus spike.
func netRates(prev, cur netCounters) (rxRate, txRate float64, ok bool) {
	if !prev.ok || !cur.ok || prev.at.IsZero() {
		return 0, 0, false
	}
	elapsed := cur.at.Sub(prev.at).Seconds()
	if elapsed <= 0 || elapsed > 600 {
		return 0, 0, false
	}
	if cur.rx < prev.rx || cur.tx < prev.tx {
		return 0, 0, false
	}
	return float64(cur.rx-prev.rx) / elapsed, float64(cur.tx-prev.tx) / elapsed, true
}

// packetRates is netRates for packets; a counter reset just hides the rate.
func packetRates(prev, cur netCounters) (rx, tx float64, ok bool) {
	if !prev.ok || !cur.ok || prev.at.IsZero() {
		return 0, 0, false
	}
	elapsed := cur.at.Sub(prev.at).Seconds()
	if elapsed <= 0 || elapsed > 600 {
		return 0, 0, false
	}
	if cur.rxPkts < prev.rxPkts || cur.txPkts < prev.txPkts {
		return 0, 0, false
	}
	return float64(cur.rxPkts-prev.rxPkts) / elapsed, float64(cur.txPkts-prev.txPkts) / elapsed, true
}

// applyNet fills the network fields from two snapshots.
func (m *HostMetrics) applyNet(prev, cur netCounters) {
	if !cur.ok {
		return
	}
	m.NetAvailable = true
	m.NetRxBytes = cur.rx
	m.NetTxBytes = cur.tx
	m.NetInterfaces = cur.interfaces
	m.NetRxErrors = cur.rxErrors
	m.NetTxErrors = cur.txErrors
	m.NetDrops = cur.drops
	if rx, tx, ok := netRates(prev, cur); ok {
		m.NetRxRate, m.NetTxRate, m.NetRateReady = rx, tx, true
	}
	if rx, tx, ok := packetRates(prev, cur); ok {
		m.NetRxPacketsRate, m.NetTxPacketsRate = rx, tx
	}
}

// applyMem fills memory fields, deriving the percentage.
func (m *HostMetrics) applyMem(total, used uint64) {
	if total == 0 {
		return
	}
	if used > total {
		used = total
	}
	m.MemTotal = total
	m.MemUsed = used
	m.MemPercent = clampFloat(float64(used)/float64(total)*100, 0, 100)
	m.MemAvailable = true
}

var processStartedAt = time.Now()

type hostMetricsSampler struct {
	mu          sync.Mutex
	sample      HostMetrics
	lastRequest time.Time
	loopRunning bool

	// CPU tick baseline for percent (platform-specific payload).
	cpuPrev any

	collecting atomic.Bool // true while background loop is alive (tests / debug)
}

func newHostMetricsSampler() *hostMetricsSampler {
	return &hostMetricsSampler{}
}

// Snapshot returns the latest sample and records that a client is watching.
// The first call (or a call after idle stop) starts the refresh loop that
// exits once clients stop requesting.
func (h *hostMetricsSampler) Snapshot() HostMetrics {
	h.mu.Lock()
	h.lastRequest = time.Now()
	needStart := !h.loopRunning
	empty := h.sample.CollectedAt.IsZero()
	if needStart {
		h.loopRunning = true
	}
	out := h.sample
	h.mu.Unlock()

	if needStart {
		// Synchronous first sample so the first paint is not empty.
		h.refresh()
		go h.loop()
		h.mu.Lock()
		out = h.sample
		h.mu.Unlock()
	} else if empty {
		h.refresh()
		h.mu.Lock()
		out = h.sample
		h.mu.Unlock()
	}
	return out
}

func (h *hostMetricsSampler) loop() {
	h.collecting.Store(true)
	defer h.collecting.Store(false)

	ticker := time.NewTicker(hostMetricsPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		h.mu.Lock()
		idle := time.Since(h.lastRequest) >= hostMetricsIdleStopAfter
		if idle {
			h.loopRunning = false
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()
		h.refresh()
	}
}

func (h *hostMetricsSampler) refresh() {
	started := time.Now()
	next := collectHostMetrics(h.cpuPrev)
	next.metrics.SampleCost = float64(time.Since(started).Microseconds()) / 1000
	h.mu.Lock()
	h.cpuPrev = next.prev
	h.sample = next.metrics
	h.mu.Unlock()
}

type hostMetricsCollectResult struct {
	metrics HostMetrics
	prev    any
}

func (s *Server) handleHostMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Host telemetry (load / temperature / network) is admin-only.
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	s.hostMetricsOnce.Do(func() { s.hostMetrics = newHostMetricsSampler() })
	writeJSON(w, http.StatusOK, s.hostMetrics.Snapshot())
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func roundTemp(v float64) float64 { return math.Round(v*10) / 10 }

func parsePowermetricsCPUTemp(out []byte) (float64, bool) {
	for _, line := range bytes.Split(out, []byte("\n")) {
		s := strings.TrimSpace(string(line))
		lower := strings.ToLower(s)
		if !strings.Contains(lower, "temperature") || !strings.Contains(lower, "cpu") {
			continue
		}
		fields := strings.Fields(s)
		for i := 0; i < len(fields); i++ {
			f := fields[i]
			if i+1 < len(fields) && (strings.EqualFold(fields[i+1], "C") || strings.EqualFold(fields[i+1], "°C")) {
				v, err := strconv.ParseFloat(strings.TrimSuffix(f, ","), 64)
				if err == nil && v > 0 && v < 150 {
					return v, true
				}
			}
			if strings.HasSuffix(strings.ToLower(f), "c") && len(f) > 1 {
				num := strings.TrimSuffix(strings.TrimSuffix(f, "C"), "c")
				v, err := strconv.ParseFloat(num, 64)
				if err == nil && v > 0 && v < 150 {
					return v, true
				}
			}
		}
	}
	return 0, false
}

func parsePowermetricsThermalPressure(out []byte) (string, bool) {
	// "Current pressure level: Nominal"
	for _, line := range bytes.Split(out, []byte("\n")) {
		s := strings.TrimSpace(string(line))
		lower := strings.ToLower(s)
		if !strings.Contains(lower, "pressure") {
			continue
		}
		if i := strings.LastIndex(s, ":"); i >= 0 && i+1 < len(s) {
			level := strings.TrimSpace(s[i+1:])
			if level != "" {
				return level, true
			}
		}
	}
	return "", false
}

// thermalPressureLabelZH maps powermetrics English levels to short Chinese UI text.
func thermalPressureLabelZH(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "nominal":
		return "正常"
	case "fair":
		return "偏高"
	case "serious":
		return "较高"
	case "critical":
		return "过高"
	default:
		if level == "" {
			return "—"
		}
		return level
	}
}

// interfaceCounter is one NIC's cumulative counters.
type interfaceCounter struct {
	Name     string
	RX, TX   uint64
	RXPkts   uint64
	TXPkts   uint64
	RXErrors uint64
	TXErrors uint64
	RXDrops  uint64
}

// virtualIfacePrefixes are excluded from throughput: loopback and
// software/tunnel devices would double-count real traffic. "ifb" is the
// traffic-shaping redirect device, which carries the mirrored copy of every
// packet it redirects.
var virtualIfacePrefixes = []string{"lo", "gif", "stf", "awdl", "llw", "utun", "ipsec", "ppp", "anpi", "ap", "ifb", "bridge", "veth", "docker", "tailscale", "tun", "tap", "vmnet", "vnic", "virbr", "lxc", "flannel", "cni", "kube"}

func isVirtualInterface(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return true
	}
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// sumInterfaceCounters aggregates physical interfaces into one snapshot.
func sumInterfaceCounters(list []interfaceCounter) netCounters {
	out := netCounters{at: time.Now()}
	for _, c := range list {
		if isVirtualInterface(c.Name) {
			continue
		}
		if c.RX == 0 && c.TX == 0 && c.RXPkts == 0 && c.TXPkts == 0 {
			continue
		}
		out.rx += c.RX
		out.tx += c.TX
		out.rxPkts += c.RXPkts
		out.txPkts += c.TXPkts
		out.rxErrors += c.RXErrors
		out.txErrors += c.TXErrors
		out.drops += c.RXDrops
		out.interfaces++
	}
	out.ok = out.interfaces > 0
	return out
}
