//go:build windows || darwin

package gateway

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"
	psprocess "github.com/shirou/gopsutil/v4/process"
)

// On macOS and Windows the cards (per-core CPU, volumes, disk throughput,
// process RSS, thread / FD counts) come from gopsutil, which talks to the OS
// directly — Win32 counters and registry on Windows, sysctl/IORegistry on
// macOS. Nothing here shells out, and everything is collected only while the
// console page polls it.

const gatherTimeout = 1500 * time.Millisecond

// gopsutilSampleState keeps the CPU-tick, disk and process baselines between
// samples. The identity block only changes on reboot, so it is cached here and
// refreshed every ten minutes instead of on every poll.
type gopsutilSampleState struct {
	cpu      []cpu.TimesStat
	disk     diskCounters
	net      netCounters
	proc     processSelfSample
	cpuInfo  hostCPUInfo
	identity hostIdentity
}

// cpuModel refreshes rarely: the model string and physical core count never
// change while the process is alive.
type hostCPUInfo struct {
	known    bool
	model    string
	mhz      float64
	physical int
}

// hostIdentity is the OS identity shown on the machine page.
type hostIdentity struct {
	known  bool
	at     time.Time
	osName string
	kernel string
	boot   time.Time
}

func collectHostMetricsExtra(m *HostMetrics, prev any) gopsutilSampleState {
	state, _ := prev.(gopsutilSampleState)
	next := gopsutilSampleState{disk: state.disk, net: state.net, proc: state.proc, cpuInfo: state.cpuInfo, identity: state.identity}
	ctx, cancel := context.WithTimeout(context.Background(), gatherTimeout)
	defer cancel()

	cur, err := cpu.TimesWithContext(ctx, true)
	next.cpu = applyGopsutilCPU(m, state.cpu, cur, err)
	if !state.cpuInfo.known {
		state.cpuInfo = readGopsutilCPUInfo(ctx)
	}
	next.cpuInfo = state.cpuInfo
	m.CPUModel, m.CPUMHz, m.CPUPhysicalCores = state.cpuInfo.model, state.cpuInfo.mhz, state.cpuInfo.physical
	if !state.identity.known || time.Since(state.identity.at) > 10*time.Minute {
		state.identity = readHostIdentity(ctx)
	}
	next.identity = state.identity
	m.BootTime, m.OSName, m.Kernel = state.identity.boot, state.identity.osName, state.identity.kernel
	if m.UptimeSeconds == 0 && !m.BootTime.IsZero() {
		m.UptimeSeconds = time.Since(m.BootTime).Seconds()
	}

	curSelf, selfErr := readGopsutilProcessSelf(ctx)
	if selfErr == nil {
		applyProcessCPUDelta(m, state.proc, curSelf)
	}
	m.Volumes = gatherVolumes(ctx)
	applyGopsutilRootVolume(m)
	curDisk := gatherDiskCounters(ctx, m.DiskDevice)
	if io, reset, ok := diskIOAverages(state.disk, curDisk); ok {
		m.DiskIO = &io
		m.DiskReadRate, m.DiskWriteRate = io.ReadRate, io.WriteRate
		m.DiskIOREady = true
	} else if !reset {
		next.disk = curDisk
	}
	applyGopsutilMemory(m, ctx)
	applyGopsutilNet(m, &next, ctx)
	if selfErr == nil {
		next.proc = curSelf
	}
	applyGopsutilProcessDetails(ctx, m)
	applyGopsutilThermal(m)
	applyGopsutilDiskTemps(m)
	applyGopsutilFans(m)
	applyGopsutilRestarts(m)
	return next
}

// readGopsutilCPUInfo reads the model string, the nominal clock and the
// physical core count. On hybrid Windows machines the per-processor Mhz can
// read 0, which the page shows as "未知" rather than "0 MHz".
func readGopsutilCPUInfo(ctx context.Context) hostCPUInfo {
	out := hostCPUInfo{known: true}
	if info, err := cpu.InfoWithContext(ctx); err == nil {
		for _, item := range info {
			if out.model == "" && strings.TrimSpace(item.ModelName) != "" {
				out.model = strings.Join(strings.Fields(item.ModelName), " ")
			}
			if item.Mhz > out.mhz {
				out.mhz = item.Mhz
			}
		}
	}
	out.physical, _ = cpu.CountsWithContext(ctx, false)
	return out
}

// readHostIdentity asks the OS for its name, kernel build and boot time.
func readHostIdentity(ctx context.Context) hostIdentity {
	info, err := host.InfoWithContext(ctx)
	if err != nil {
		return hostIdentity{at: time.Now()}
	}
	return hostIdentity{
		known:  true,
		at:     time.Now(),
		kernel: info.KernelVersion,
		osName: strings.TrimSpace(info.Platform + " " + info.PlatformVersion),
		boot:   time.Unix(int64(info.BootTime), 0),
	}
}

// applyGopsutilCPU fills the per-core strip and refreshes CPUPercent from tick
// deltas. A tick delta needs a baseline; the first sample falls back to the
// since-boot average.
func applyGopsutilCPU(m *HostMetrics, prev, cur []cpu.TimesStat, err error) []cpu.TimesStat {
	if err != nil || len(cur) == 0 {
		return prev
	}
	m.CPUCoresAvailable = true
	m.CPUCores = make([]HostCPUCore, 0, len(cur))
	for index, t := range cur {
		m.CPUCores = append(m.CPUCores, HostCPUCore{ID: index, Percent: timesBusyPercent(t)})
	}
	if m.CPUCount < len(cur) {
		m.CPUCount = len(cur)
	}
	if prev != nil && len(prev) == len(cur) {
		var busyDelta, totalDelta float64
		for index := range cur {
			if busy, total, ok := tickDelta(prev[index], cur[index]); ok {
				busyDelta += busy
				totalDelta += total
			}
		}
		if totalDelta > 0 {
			m.CPUPercent = clampFloat(busyDelta/totalDelta*100, 0, 100)
		}
	} else {
		var sumBusy, sumTotal float64
		for _, t := range cur {
			total := timesTotal(t)
			sumTotal += total
			sumBusy += total - t.Idle
		}
		if sumTotal > 0 {
			m.CPUPercent = clampFloat(sumBusy/sumTotal*100, 0, 100)
		}
	}
	if m.CPULoad == 0 {
		m.CPULoad = loadPercent(m.Load1, m.CPUCount)
	}
	return cur
}

// readGopsutilProcessSelf snapshots the gateway's own CPU time. Windows has no
// /proc and the stdlib cannot read per-process ticks on macOS, so this goes
// through the OS query interface.
func readGopsutilProcessSelf(ctx context.Context) (processSelfSample, error) {
	proc, err := psprocess.NewProcessWithContext(ctx, int32(os.Getpid()))
	if err != nil {
		return processSelfSample{}, err
	}
	times, err := proc.TimesWithContext(ctx)
	if err != nil || times == nil {
		return processSelfSample{}, err
	}
	return processSelfSample{cpuSeconds: times.Total(), at: time.Now(), ok: true}, nil
}

// applyGopsutilProcessDetails adds thread count, open handles and start time —
// the process-level numbers that /proc would otherwise provide on Linux.
func applyGopsutilProcessDetails(ctx context.Context, m *HostMetrics) {
	proc, err := psprocess.NewProcessWithContext(ctx, int32(os.Getpid()))
	if err != nil {
		return
	}
	if threads, err := proc.NumThreadsWithContext(ctx); err == nil && threads > 0 {
		m.OSThreads = int(threads)
		m.ProcessThreads = int(threads)
	}
	if fds, err := proc.NumFDsWithContext(ctx); err == nil && fds > 0 {
		m.ProcessFDs = int(fds)
	}
	if createdAt, err := proc.CreateTimeWithContext(ctx); err == nil && createdAt > 0 {
		m.ProcessStarted = time.UnixMilli(createdAt)
	}
	if info, err := proc.MemoryInfoWithContext(ctx); err == nil && info != nil && info.RSS > 0 {
		m.ProcessRSSMiB = float64(info.RSS) / (1 << 20)
		m.ProcessRSSFromRuntime = false
	}
}

// applyGopsutilMemory fills memory the way `free` does where the platform
// exposes the split: used = total − available, cached = cached + buffers.
func applyGopsutilMemory(m *HostMetrics, ctx context.Context) {
	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil || vm.Total == 0 {
		return
	}
	used := vm.Total - vm.Available
	if used > vm.Total {
		used = vm.Total
	}
	m.applyMem(vm.Total, used)
	m.MemCached = vm.Cached + vm.Buffers + vm.Sreclaimable
	m.MemSlab = vm.Slab
	// Anon = used minus reclaimable cache, the part that cannot be dropped.
	if cache := minU64(m.MemCached, used); used > cache {
		m.MemAnon = used - cache
	}
	m.SwapTotal, m.SwapUsed = vm.SwapTotal, vm.SwapTotal-vm.SwapFree
	if vm.SwapFree > vm.SwapTotal {
		m.SwapUsed = 0
	}
	m.SwapAvailable = vm.SwapTotal > 0
}

// applyGopsutilNet sums the physical interfaces (see isVirtualInterface).
func applyGopsutilNet(m *HostMetrics, next *gopsutilSampleState, ctx context.Context) {
	per, err := psnet.IOCountersWithContext(ctx, true)
	if err != nil {
		return
	}
	list := make([]interfaceCounter, 0, len(per))
	for _, c := range per {
		list = append(list, interfaceCounter{
			Name:     c.Name,
			RX:       c.BytesRecv,
			TX:       c.BytesSent,
			RXPkts:   c.PacketsRecv,
			TXPkts:   c.PacketsSent,
			RXErrors: c.Errin,
			TXErrors: c.Errout,
			RXDrops:  c.Dropin,
		})
	}
	cur := sumInterfaceCounters(list)
	m.applyNet(next.net, cur)
	if cur.ok {
		next.net = cur
	}
}

// applyGopsutilRootVolume picks "/" on macOS and the OS drive on Windows.
func applyGopsutilRootVolume(m *HostMetrics) {
	if len(m.Volumes) == 0 {
		return
	}
	root := volumeByMount(m.Volumes, "/")
	if root == nil {
		root = &m.Volumes[0]
	}
	m.DiskTotal, m.DiskUsed, m.DiskPercent, m.DiskAvailable = root.Total, root.Used, root.Percent, true
	m.DiskDevice = root.Device
	if m.DiskDevice == "" {
		m.DiskDevice = root.Mount
	}
}

// timesTotal sums the per-state seconds of one CPU. (v4 also exposes a Total
// method; summing keeps this independent of its signature.)
func timesTotal(t cpu.TimesStat) float64 {
	return t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq + t.Softirq + t.Steal + t.Guest + t.GuestNice
}

// timesBusyPercent is one core's utilisation: (total − idle) / total.
func timesBusyPercent(t cpu.TimesStat) float64 {
	total := timesTotal(t)
	if total <= 0 {
		return 0
	}
	return clampFloat((total-t.Idle)/total*100, 0, 100)
}

// tickDelta returns the busy and total seconds between two samples.
func tickDelta(prev, cur cpu.TimesStat) (busy, total float64, ok bool) {
	total = timesTotal(cur) - timesTotal(prev)
	busy = (timesTotal(cur) - cur.Idle) - (timesTotal(prev) - prev.Idle)
	if total <= 0 || busy < 0 {
		return 0, 0, false
	}
	return busy, total, true
}

// gatherVolumes lists fixed local filesystems (no network shares, no tmpfs).
func gatherVolumes(ctx context.Context) []HostDisk {
	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		return nil
	}
	var out []HostDisk
	seen := map[string]bool{}
	for _, part := range parts {
		mount := part.Mountpoint
		if mount == "" || seen[mount] || !showVolumeFS(part.Fstype) {
			continue
		}
		usage, err := disk.UsageWithContext(ctx, mount)
		if err != nil || usage.Total == 0 {
			continue
		}
		seen[mount] = true
		out = append(out, HostDisk{
			Mount:   mount,
			Device:  part.Device,
			FSType:  part.Fstype,
			Total:   usage.Total,
			Used:    usage.Used,
			Percent: percentOf(usage.Used, usage.Total),
		})
	}
	sortVolumes(out)
	return out
}

// showVolumeFS filters pseudo filesystems; macOS volumes are APFS/HFS/exFAT on
// /dev/disk*, Windows volumes are NTFS/ReFS/FAT on a drive letter.
func showVolumeFS(fs string) bool {
	switch strings.ToLower(fs) {
	case "", "tmpfs", "devfs", "autofs", "overlay", "squashfs", "proc", "sysfs", "cgroup", "cgroup2", "ramfs", "nsfs", "fuseblk", "0":
		return false
	}
	return true
}

// gatherDiskCounters reads aggregate IO of the physical disk behind the root
// volume. Summing every counter would double-count partitions of one disk.
func gatherDiskCounters(ctx context.Context, rootDevice string) diskCounters {
	out := diskCounters{
		sectors:  map[string]uint64{},
		done:     map[string]uint64{},
		requests: map[string]uint64{},
		busy:     map[string]uint64{},
		at:       time.Now(),
	}
	counters, err := disk.IOCountersWithContext(ctx)
	if err != nil || len(counters) == 0 {
		return out
	}
	for name, c := range counters {
		// gopsutil reports byte counts; store them as sectors so the shared
		// rate math (sectors × 512) applies unchanged.
		out.sectors[name] = (c.ReadBytes + c.WriteBytes) / 512
		out.done[name] = c.ReadTime + c.WriteTime
		out.busy[name] = c.IoTime
		out.requests[name] = c.ReadCount + c.WriteCount
	}
	out.root = resolveGopsutilDisk(out, rootDevice)
	return out
}

// resolveGopsutilDisk maps the root volume's device to a physical counter:
// "/dev/disk3s1" → "disk3", "C:" → "PhysicalDrive0", "/dev/sda2" → "sda".
func resolveGopsutilDisk(counters diskCounters, rootDevice string) string {
	if len(counters.sectors) == 0 {
		return ""
	}
	keys := sortedKeys(counters.sectors)
	if rootDevice == "" {
		return keys[0]
	}
	if _, ok := counters.sectors[rootDevice]; ok {
		return rootDevice
	}
	lower := strings.ToLower(strings.TrimSpace(rootDevice))
	for _, name := range keys {
		if strings.ToLower(name) == lower {
			return name
		}
	}
	base := lower
	if index := strings.LastIndex(base, "/"); index >= 0 {
		base = base[index+1:]
	}
	// "/dev/disk3s1" → "disk3s" → "disk3", "/dev/sda2" → "sda".
	base = strings.TrimRight(base, "0123456789:")
	if len(base) > 1 && strings.HasSuffix(base, "s") && base[0] == 'd' {
		base = strings.TrimSuffix(base, "s")
	}
	for _, name := range keys {
		if strings.ToLower(name) == base {
			return name
		}
	}
	for _, name := range keys {
		if base != "" && strings.HasPrefix(strings.ToLower(name), base) {
			return name
		}
	}
	return keys[0]
}

func sortedKeys(values map[string]uint64) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
