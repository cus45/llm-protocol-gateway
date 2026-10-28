//go:build linux

package gateway

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// hostRoot prefixes every /proc and /sys read so the parsers below can be run
// against a fixture tree on any OS (see host_metrics_fixtures_test.go).
var hostRoot = "/"

func hostPath(rel string) string {
	if hostRoot == "/" {
		return rel
	}
	return filepath.Join(hostRoot, rel)
}

func hostRead(rel string) (string, bool) {
	b, err := os.ReadFile(hostPath(rel))
	if err != nil {
		return "", false
	}
	return string(b), true
}

func hostText(rel string) string {
	text, _ := hostRead(rel)
	return text
}

// cpuTicks is one /proc/stat row reduced to the two numbers we use.
type cpuTicks struct {
	idle, total uint64
}

type linuxSampleState struct {
	total    cpuTicks
	hasTotal bool
	cores    []cpuTicks
	net      netCounters
	disk     diskCounters
	proc     procTicks
}

func collectHostMetrics(prev any) hostMetricsCollectResult {
	started := time.Now()
	m := HostMetrics{
		CPUCount:    runtime.NumCPU(),
		CollectedAt: started,
	}
	prevState, _ := prev.(linuxSampleState)
	next := linuxSampleState{net: prevState.net, disk: prevState.disk, proc: prevState.proc}

	m.Load1, m.Load5, m.Load15 = parseUnixLoadavg(hostText("/proc/loadavg"))
	total, cores := parseProcStat(hostText("/proc/stat"))
	if total.total > 0 {
		// First sample falls back to load average: a tick delta needs a baseline.
		if prevState.hasTotal && total.total > prevState.total.total {
			m.CPUPercent = tickBusyPercent(prevState.total, total)
		} else {
			m.CPUPercent = loadPercent(m.Load1, m.CPUCount)
		}
		next.total, next.hasTotal = total, true
	}
	if len(cores) > 0 {
		next.cores = cores
		m.CPUCoresAvailable = true
		m.CPUCores = make([]HostCPUCore, 0, len(cores))
		for i := range cores {
			if cores[i].total == 0 {
				continue
			}
			pct := 100 - 100*float64(cores[i].idle)/float64(cores[i].total)
			if len(prevState.cores) == len(cores) && cores[i].total > prevState.cores[i].total {
				pct = tickBusyPercent(prevState.cores[i], cores[i])
			}
			m.CPUCores = append(m.CPUCores, HostCPUCore{ID: i, Percent: clampFloat(pct, 0, 100)})
		}
	}
	m.CPULoad = loadPercent(m.Load1, m.CPUCount)
	cpuinfo := hostText("/proc/cpuinfo")
	m.CPUMHz = parseCPUModelMHz(cpuinfo)
	m.CPUModel, m.CPUPhysicalCores = parseCPUInfoModel(cpuinfo)

	if tempC, source, ok := linuxCPUTempC(); ok {
		m.TempC, m.TempAvailable, m.TempSource = tempC, true, source
	}
	applyLinuxMemory(&m, hostText("/proc/meminfo"))
	applyDiskTemps(&m)
	applyFanSpeeds(&m)

	m.Volumes = collectLinuxVolumes(hostText("/proc/self/mountinfo"))
	if root := volumeByMount(m.Volumes, "/"); root != nil {
		m.DiskTotal, m.DiskUsed, m.DiskPercent, m.DiskAvailable = root.Total, root.Used, root.Percent, true
	}
	curDisk := parseProcDiskstats(hostText("/proc/diskstats"), linuxRootDeviceID(hostText("/proc/self/mountinfo")))
	m.DiskDevice = curDisk.root
	if io, reset, ok := diskIOAverages(prevState.disk, curDisk); ok {
		m.DiskIO = &io
		m.DiskReadRate, m.DiskWriteRate = io.ReadRate, io.WriteRate
		m.DiskIOREady = true
	} else if !reset {
		next.disk = curDisk
	}

	m.UptimeSeconds = parseFloatOr(hostText("/proc/uptime"), 0)
	m.Kernel = strings.TrimSpace(hostText("/proc/sys/kernel/osrelease"))
	m.OSName = parseOSRelease(hostText("/etc/os-release"))
	procStat := hostText("/proc/stat")
	m.BootTime = linuxBootTime(procStat, m.UptimeSeconds)
	m.finishProcess()
	applyLinuxProcess(&m, hostText("/proc/self/stat"), m.BootTime, prevState.proc, &next.proc)
	applyLinuxPressure(&m)
	applyCgroupLimits(&m)
	applyServiceRestarts(&m)

	cur := sumInterfaceCounters(parseProcNetDev(hostText("/proc/net/dev")))
	m.applyNet(prevState.net, cur)
	if cur.ok {
		next.net = cur
	}
	m.SampleCost = sampleCostMS(started)
	return hostMetricsCollectResult{metrics: m, prev: next}
}

func parseFloatOr(content string, fallback float64) float64 {
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return fallback
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v < 0 {
		return fallback
	}
	return v
}

// tickBusyPercent is the share of non-idle ticks between two samples. iowait
// counts as idle, so a hung disk shows as IO pressure rather than CPU load.
func tickBusyPercent(prev, cur cpuTicks) float64 {
	if cur.total <= prev.total {
		return 0
	}
	idle := cur.idle - prev.idle
	total := cur.total - prev.total
	return clampFloat(100*(1-float64(idle)/float64(total)), 0, 100)
}

// parseProcStat returns the aggregate row and one row per logical CPU.
func parseProcStat(content string) (total cpuTicks, cores []cpuTicks) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		ticks, ok := parseCPUTickFields(fields[1:])
		if !ok {
			continue
		}
		if fields[0] == "cpu" {
			total = ticks
			continue
		}
		if _, err := strconv.Atoi(strings.TrimPrefix(fields[0], "cpu")); err != nil {
			continue
		}
		cores = append(cores, ticks)
	}
	return total, cores
}

// parseCPUTickFields sums user…steal; idle is idle + iowait.
func parseCPUTickFields(vals []string) (cpuTicks, bool) {
	var nums []uint64
	for _, f := range vals {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			break
		}
		nums = append(nums, v)
	}
	if len(nums) < 4 {
		return cpuTicks{}, false
	}
	var sum uint64
	for _, v := range nums {
		sum += v
	}
	if sum == 0 {
		return cpuTicks{}, false
	}
	idle := nums[3]
	if len(nums) > 4 {
		idle += nums[4]
	}
	return cpuTicks{idle: idle, total: sum}, true
}

// linuxBootTime prefers the kernel's btime (uptime drifts across suspend).
func linuxBootTime(procStat string, uptime float64) time.Time {
	for _, line := range strings.Split(procStat, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "btime" {
			if sec, err := strconv.ParseInt(fields[1], 10, 64); err == nil && sec > 0 {
				return time.Unix(sec, 0)
			}
		}
	}
	if uptime <= 0 {
		return time.Time{}
	}
	return time.Now().Add(-time.Duration(uptime * float64(time.Second))).Truncate(time.Second)
}

// parseOSRelease extracts PRETTY_NAME from /etc/os-release.
func parseOSRelease(content string) string {
	name := ""
	for _, line := range strings.Split(content, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		val = strings.Trim(strings.Trim(val, `"'`), " ")
		switch key {
		case "PRETTY_NAME":
			return val
		case "NAME":
			name = val
		}
	}
	return name
}

// parseCPUModelMHz reads the first "cpu MHz" line of /proc/cpuinfo.
func parseCPUModelMHz(content string) float64 {
	for _, line := range strings.Split(content, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "cpu MHz" {
			continue
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil && v > 0 {
			return v
		}
	}
	return 0
}

// parseCPUInfoModel reads the processor name and the physical core count
// ("cpu cores" is per socket, so it is multiplied by the socket count).
func parseCPUInfoModel(content string) (model string, physical int) {
	sockets, coresPerSocket := map[string]bool{}, 0
	for _, line := range strings.Split(content, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "model name", "Processor":
			if model == "" {
				model = strings.Join(strings.Fields(val), " ")
			}
		case "physical id":
			sockets[val] = true
		case "cpu cores":
			if v, err := strconv.Atoi(val); err == nil && v > coresPerSocket {
				coresPerSocket = v
			}
		}
	}
	if coresPerSocket > 0 && len(sockets) > 0 {
		physical = coresPerSocket * len(sockets)
	}
	return model, physical
}

// memInfo is the trimmed set of /proc/meminfo keys the console needs (bytes).
type memInfo struct {
	total, free, available, buffers, cached, anon, slab uint64
	swapTotal, swapFree                                 uint64
	hasAvailable, hasSwap                               bool
}

func parseMemInfo(content string) (memInfo, bool) {
	var mi memInfo
	for _, line := range strings.Split(content, "\n") {
		key, valKB, ok := parseMeminfoLine(line)
		if !ok {
			continue
		}
		v := valKB * 1024
		switch key {
		case "MemTotal":
			mi.total = v
		case "MemFree":
			mi.free = v
		case "MemAvailable":
			mi.available, mi.hasAvailable = v, true
		case "Buffers":
			mi.buffers = v
		case "Cached", "SReclaimable":
			mi.cached += v
		case "AnonPages":
			mi.anon = v
		case "Slab":
			mi.slab = v
		case "SwapTotal":
			mi.swapTotal, mi.hasSwap = v, true
		case "SwapFree":
			mi.swapFree = v
		}
	}
	return mi, mi.total > 0
}

// availableBytes mirrors what `free` treats as reclaimable.
func (mi memInfo) availableBytes() uint64 {
	free := mi.available
	if !mi.hasAvailable {
		free = mi.free + mi.buffers + mi.cached
	}
	if free > mi.total {
		free = mi.total
	}
	return free
}

// applyLinuxMemory fills usage the way `free` does: page cache stays in
// "available", so a host with a big cache is not reported as full.
func applyLinuxMemory(m *HostMetrics, content string) {
	mi, ok := parseMemInfo(content)
	if !ok {
		return
	}
	available := mi.availableBytes()
	m.applyMem(mi.total, mi.total-available)
	m.MemCached = mi.cached
	m.MemSlab = mi.slab
	if mi.anon > 0 {
		m.MemAnon = mi.anon
	} else if used := mi.total - available; used > minU64(mi.cached, used) {
		m.MemAnon = used - minU64(mi.cached, used)
	}
	if mi.hasSwap {
		m.SwapTotal, m.SwapAvailable = mi.swapTotal, true
		if mi.swapFree <= mi.swapTotal {
			m.SwapUsed = mi.swapTotal - mi.swapFree
		}
	}
}

// parseProcMeminfo returns total/used bytes, matching `free`'s used column.
func parseProcMeminfo(content string) (total, used uint64, ok bool) {
	mi, ok := parseMemInfo(content)
	if !ok {
		return 0, 0, false
	}
	return mi.total, mi.total - mi.availableBytes(), true
}

// parseProcSwap returns total/used swap bytes. ok reports whether /proc/meminfo
// contained a SwapTotal line — a real 0 there means swap is not configured.
func parseProcSwap(content string) (total, used uint64, ok bool) {
	mi, _ := parseMemInfo(content)
	if !mi.hasSwap {
		return 0, 0, false
	}
	if mi.swapTotal == 0 || mi.swapFree > mi.swapTotal {
		return mi.swapTotal, 0, true
	}
	return mi.swapTotal, mi.swapTotal - mi.swapFree, true
}

func parseMeminfoLine(line string) (key string, valueKB uint64, ok bool) {
	idx := strings.IndexByte(line, ':')
	if idx <= 0 {
		return "", 0, false
	}
	fields := strings.Fields(line[idx+1:])
	if len(fields) == 0 {
		return "", 0, false
	}
	v, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return strings.TrimSpace(line[:idx]), v, true
}

// parseProcNetDev reads rx bytes/packets/errs/drop (fields 1-4) and tx
// bytes/packets/errs/drop (fields 9-12) per interface.
func parseProcNetDev(content string) []interfaceCounter {
	var out []interfaceCounter
	for _, line := range strings.Split(content, "\n") {
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		fields := strings.Fields(line[idx+1:])
		if name == "" || len(fields) < 12 {
			continue
		}
		var nums [12]uint64
		bad := false
		for i := range nums {
			v, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				bad = true
				break
			}
			nums[i] = v
		}
		if bad {
			continue
		}
		out = append(out, interfaceCounter{
			Name:     name,
			RX:       nums[0],
			RXPkts:   nums[1],
			RXErrors: nums[2],
			RXDrops:  nums[3],
			TX:       nums[8],
			TXPkts:   nums[9],
			TXErrors: nums[10],
		})
	}
	return out
}

// linuxRootDeviceID returns the major:minor of the filesystem mounted on "/".
func linuxRootDeviceID(mountinfo string) string {
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		// id parent major:minor root mountpoint …
		if len(fields) >= 5 && fields[4] == "/" {
			return fields[2]
		}
	}
	return ""
}

// linuxCPUTempC reads /sys/class/thermal. A package/CPU zone wins; any other
// zone with a name is only a fallback, because on VMs thermal_zone0 is often an
// ACPI placeholder whose type is empty.
func linuxCPUTempC() (float64, string, bool) {
	entries, err := os.ReadDir(hostPath("/sys/class/thermal"))
	if err != nil {
		return 0, "", false
	}
	var fallback float64
	var haveFallback bool
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "thermal_zone") {
			continue
		}
		base := "/sys/class/thermal/" + name
		typ := strings.TrimSpace(hostText(base + "/type"))
		if typ == "" {
			continue
		}
		milli, err := strconv.ParseFloat(strings.TrimSpace(hostText(base+"/temp")), 64)
		if err != nil || milli <= 0 {
			continue
		}
		celsius := milli / 1000
		lower := strings.ToLower(typ)
		if strings.Contains(lower, "pkg") || strings.Contains(lower, "cpu") || strings.Contains(lower, "x86") {
			return celsius, typ, true
		}
		if !haveFallback {
			fallback, haveFallback = celsius, true
		}
	}
	if haveFallback {
		return fallback, "thermal_zone", true
	}
	return 0, "", false
}

// applyLinuxPressure reads PSI: the share of wall time some task was stalled.
func applyLinuxPressure(m *HostMetrics) {
	cpu, okCPU := hostRead("/proc/pressure/cpu")
	mem, okMem := hostRead("/proc/pressure/memory")
	io, okIO := hostRead("/proc/pressure/io")
	if !okCPU && !okMem && !okIO {
		return
	}
	m.PressureAvailable = true
	if okCPU {
		m.PressureCPU, m.PressureCPUFull = psiAvg10(cpu, "some"), psiAvg10(cpu, "full")
	}
	if okMem {
		m.PressureMem, m.PressureMemFull = psiAvg10(mem, "some"), psiAvg10(mem, "full")
	}
	if okIO {
		m.PressureIO, m.PressureIOFull = psiAvg10(io, "some"), psiAvg10(io, "full")
	}
}

func psiAvg10(content, kind string) float64 {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != kind {
			continue
		}
		for _, f := range fields[1:] {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				if pct, err := strconv.ParseFloat(v, 64); err == nil {
					return pct
				}
			}
		}
	}
	return 0
}

// procTicks carries the gateway process's CPU tick baseline.
type procTicks struct {
	utime, stime uint64
	at           time.Time
	ok           bool
}

// applyLinuxProcess reads this process's /proc/<pid> files. Any failure just
// leaves the corresponding field empty.
func applyLinuxProcess(m *HostMetrics, selfStat string, boot time.Time, prev procTicks, next *procTicks) {
	if status, ok := hostRead("/proc/self/status"); ok {
		m.ProcessThreads = int(parseStatusKB(status, "Threads:"))
		m.ProcessRSSMiB = float64(parseStatusKB(status, "VmRSS:")) / 1024
		m.ProcessPeakMiB = float64(parseStatusKB(status, "VmHWM:")) / 1024
	}
	if entries, err := os.ReadDir(hostPath("/proc/self/fd")); err == nil {
		m.ProcessFDs = len(entries)
	}
	fields := procStatFields(selfStat)
	if len(fields) < 22 {
		return
	}
	utime, errU := strconv.ParseUint(fields[13], 10, 64)
	stime, errS := strconv.ParseUint(fields[14], 10, 64)
	if errU == nil && errS == nil {
		cur := procTicks{utime: utime, stime: stime, at: time.Now(), ok: true}
		applyProcessCPUDelta(m,
			processSelfSample{cpuSeconds: float64(prev.utime+prev.stime) / clockTicks, at: prev.at, ok: prev.ok},
			processSelfSample{cpuSeconds: float64(cur.utime+cur.stime) / clockTicks, at: cur.at, ok: true})
		*next = cur
	}
	// starttime is ticks after boot, so the process start needs the host boot
	// time, not this file.
	if start, err := strconv.ParseUint(fields[21], 10, 64); err == nil && start > 0 && !boot.IsZero() {
		m.ProcessStarted = boot.Add(time.Duration(float64(start) / clockTicks * float64(time.Second)))
	}
}

// clockTicks is USER_HZ, fixed at 100 on every Linux ABI Go supports.
const clockTicks = 100

// procStatFields splits /proc/<pid>/stat on spaces but keeps the parenthesised
// command name (which may itself contain spaces) as one field.
func procStatFields(stat string) []string {
	open := strings.IndexByte(stat, '(')
	close := strings.LastIndexByte(stat, ')')
	if open < 0 || close < open {
		return nil
	}
	head := strings.Fields(stat[:open])
	tail := strings.Fields(stat[close+1:])
	return append(append(head, stat[open:close+1]), tail...)
}

func parseStatusKB(status, key string) uint64 {
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, key) {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, key)))
		if len(fields) == 0 {
			continue
		}
		if v, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
			return v
		}
	}
	return 0
}

// applyCgroupLimits reports the container's memory limit when one applies:
// a container whose cgroup cap is 512 MiB is "83% full" at 425 MiB even
// though the host still has gigabytes free.
func applyCgroupLimits(m *HostMetrics) {
	raw := strings.TrimSpace(hostText("/sys/fs/cgroup/memory.max"))
	if raw == "" {
		raw = strings.TrimSpace(hostText("/sys/fs/cgroup/memory/memory.limit_in_bytes"))
	}
	limit, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || limit == 0 || limit > 1<<50 {
		return
	}
	usage, err := strconv.ParseUint(strings.TrimSpace(hostText("/sys/fs/cgroup/memory.current")), 10, 64)
	if err != nil {
		return
	}
	m.CgroupMemLimit = limit
	m.CgroupMemUsed = usage
}

// procDiskstatsColumns indexes one /proc/diskstats row:
// major(0) minor(1) name(2) reads(3) rmerge(4) rsectors(5) rms(6) writes(7)
// wmerge(8) wsectors(9) wms(10) inflight(11) busy(12) weighted(13) …
type procDiskstatsColumns struct {
	readSec, writeSec, readMS, writeMS, inflight, busy int
}

var diskstatsColumns = procDiskstatsColumns{readSec: 5, writeSec: 9, readMS: 6, writeMS: 10, inflight: 11, busy: 12}

// parseProcDiskstats returns per-device counter totals. Sectors are 512 bytes in
// this file regardless of the device's logical block size.
func parseProcDiskstats(content, rootDev string) diskCounters {
	out := diskCounters{
		sectors:  map[string]uint64{},
		done:     map[string]uint64{},
		requests: map[string]uint64{},
		busy:     map[string]uint64{},
		at:       time.Now(),
	}
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 14 {
			continue
		}
		name := fields[2]
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "dm-") {
			continue
		}
		requests, ok1 := parseU64(fields[3])
		read, ok2 := parseU64(fields[diskstatsColumns.readSec])
		readMS, ok3 := parseU64(fields[diskstatsColumns.readMS])
		writes, ok4 := parseU64(fields[7])
		written, ok5 := parseU64(fields[diskstatsColumns.writeSec])
		writeMS, ok6 := parseU64(fields[diskstatsColumns.writeMS])
		busy, ok7 := parseU64(fields[diskstatsColumns.busy])
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 {
			continue
		}
		out.sectors[name] = read + written
		out.done[name] = readMS + writeMS // ms spent serving completed IO
		out.requests[name] = requests + writes
		out.busy[name] = busy
	}
	out.root = resolveRootDisk(out, rootDev, rootParentDisk(rootDev))
	return out
}

// rootParentDisk maps "253:1" (vda1) to the whole disk "vda" via /sys/dev/block.
func rootParentDisk(devID string) string {
	if devID == "" {
		return ""
	}
	link := hostPath("/sys/dev/block/" + devID)
	target, err := os.Readlink(link)
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	// …/block/<disk>/<partition> or …/block/<disk> for whole-disk roots, so the
	// disk name is the segment right after the last "block".
	parts := strings.Split(filepath.ToSlash(filepath.Clean(target)), "/")
	for index := len(parts) - 1; index > 0; index-- {
		if parts[index-1] == "block" && parts[index] != "" {
			return parts[index]
		}
	}
	return ""
}

func resolveRootDisk(d diskCounters, userDev, physical string) string {
	if physical != "" {
		if _, ok := d.sectors[physical]; ok {
			return physical
		}
	}
	if _, ok := d.sectors[userDev]; ok {
		return userDev
	}
	return ""
}

// collectLinuxVolumes lists the real filesystems worth showing: physical block
// devices (or their LVM paths) plus "/". tmpfs / overlay / snap squashfs
// mounts are hidden — they are noise on a server.
func collectLinuxVolumes(mountinfo string) []HostDisk {
	var out []HostDisk
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		sep := indexOfField(fields, "-")
		if len(fields) < 5 || sep < 0 || len(fields) < sep+4 {
			continue
		}
		mount, fstype, source := fields[4], fields[sep+1], fields[sep+2]
		if !showLinuxVolume(mount, fstype, source) {
			continue
		}
		total, free, ok := statfsVolume(mount)
		if !ok {
			continue
		}
		out = append(out, HostDisk{
			Mount:   mount,
			Device:  source,
			FSType:  fstype,
			Total:   total,
			Used:    total - free,
			Percent: percentOf(total-free, total),
		})
	}
	sortVolumes(out)
	return out
}

func indexOfField(fields []string, target string) int {
	for i, f := range fields {
		if f == target {
			return i
		}
	}
	return -1
}

func showLinuxVolume(mount, fstype, source string) bool {
	if mount == "/" {
		return true
	}
	switch fstype {
	case "tmpfs", "devtmpfs", "overlay", "squashfs", "proc", "sysfs", "cgroup", "cgroup2", "devpts", "mqueue", "debugfs", "tracefs", "securityfs", "pstore", "bpf", "autofs", "fusectl", "configfs", "binfmt_misc", "hugetlbfs", "nsfs", "ramfs", "rpc_pipefs":
		return false
	}
	return strings.HasPrefix(source, "/dev/")
}

func parseU64(s string) (uint64, bool) {
	v, err := strconv.ParseUint(s, 10, 64)
	return v, err == nil
}

// applyServiceRestarts asks systemd how many times the unit restarted. A
// gateway restarted five times today tells a different story from one up for a
// week. 0 means "no restarts"; an absent service (dev run) leaves -1 = unknown.
func applyServiceRestarts(m *HostMetrics) {
	m.ProcessRestarts = -1
	unit := os.Getenv("GATEWAY_SERVICE_UNIT")
	if unit == "" {
		unit = "llm-protocol-gateway"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", unit, "-p", "NRestarts", "--value").Output()
	if err != nil {
		return
	}
	if n, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && n >= 0 {
		m.ProcessRestarts = n
	}
}
