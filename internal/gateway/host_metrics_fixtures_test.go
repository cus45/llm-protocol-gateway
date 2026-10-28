//go:build linux

package gateway

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeHostFixture lays out a fake /proc + /sys tree and points hostRoot at it,
// so these parser tests also run on macOS/Windows CI where /proc does not exist.
func writeHostFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	previous := hostRoot
	hostRoot = root
	t.Cleanup(func() { hostRoot = previous })
	return root
}

// fixtureBootTime is "now − 5 days" so btime, uptime and process start agree.
var fixtureBootTime = time.Now().Unix() - 5*24*3600

const fixtureStatTemplate = `cpu  1000 0 500 8000 100 0 100 0 0 0
cpu0 500 0 250 4000 50 0 50 0 0 0
cpu1 500 0 250 4000 50 0 50 0 0 0
intr 12345
ctxt 678901
btime %d
processes 9876
`

const fixtureMeminfo = `MemTotal:        3960900 kB
MemFree:          300000 kB
MemAvailable:    3231932 kB
Buffers:          118252 kB
Cached:           806416 kB
SwapCached:            0 kB
Active:          1200000 kB
Inactive:         900000 kB
AnonPages:        280924 kB
Slab:             136076 kB
SReclaimable:      67512 kB
SUnreclaim:        68488 kB
SwapTotal:        524284 kB
SwapFree:         524284 kB
`

const fixtureNetDev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1429023929 1014012    0    0    0     0          0         0 1429023929 1014012    0    0    0     0       0          0
  ens3: 7536313974 8555776    3 9925    0     0          0         0 7896272281 3646565    1    2    0     0       0          0
docker0: 100 1 0 0 0 0 0 0 200 2 0 0 0 0 0 0
`

const fixtureDiskstats = `   7       0 loop0 11 0 28 5 0 0 0 0 0 5 5 0 0 0 0 0 0
 253       0 vda 50852 7580 1483150 18424 116464 23524 6884582 77440 0 22174 96506 523 0 2987016 65 3848 575
 253       1 vda1 50142 6882 1445632 18283 116433 23496 6884258 77428 0 23482 95777 523 0 2987016 65 0 0
`

const fixtureMountinfo = `29 1 253:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw,discard,errors=remount-ro,commit=30
28 29 0:25 / /run rw,nosuid,nodev - tmpfs tmpfs rw,size=396092k
44 29 259:0 / /boot rw,relatime shared:29 - ext4 /dev/vda16 rw
46 44 253:15 / /boot/efi rw,relatime shared:44 - vfat /dev/vda15 rw,fmask=0077
200 29 0:60 / /snap/core22/1 ro - squashfs /dev/loop3 ro
`

const fixturePressure = `some avg10=3.71 avg60=3.93 avg300=3.86 total=302255423
full avg10=0.00 avg60=0.00 avg300=0.00 total=0
`

func fixtureStat() string { return fmt.Sprintf(fixtureStatTemplate, fixtureBootTime) }

func fixtureOSRelease() string {
	return "PRETTY_NAME=\"Ubuntu 24.04.4 LTS\"\nNAME=\"Ubuntu\"\nVERSION_ID=\"24.04\"\n"
}

func fixtureCPUInfo() string {
	return "processor\t: 0\nmodel name\t: Intel Core Processor (Broadwell, IBRS)\ncpu MHz\t\t: 2199.998\n"
}

func TestParseProcStatPerCore(t *testing.T) {
	total, cores := parseProcStat(fixtureStat())
	// total = 1000+500+8000+100+100 = 9700, idle = 8000+100 = 8100
	if total.total != 9700 || total.idle != 8100 {
		t.Fatalf("aggregate=%+v", total)
	}
	if len(cores) != 2 {
		t.Fatalf("cores=%d", len(cores))
	}
	if cores[1].total != 4850 || cores[1].idle != 4050 {
		t.Fatalf("core1=%+v", cores[1])
	}
	if pct := tickBusyPercent(cpuTicks{idle: 100, total: 200}, cpuTicks{idle: 100, total: 400}); pct != 100 {
		t.Fatalf("busy pct=%v want 100", pct)
	}
	// A row without a per-core breakdown must not panic the strip.
	if _, cores := parseProcStat("cpu 1 2 3 4\nintr 1\n"); len(cores) != 0 {
		t.Fatalf("cores=%+v", cores)
	}
}

func TestLinuxMemoryMatchesFree(t *testing.T) {
	mi, ok := parseMemInfo(fixtureMeminfo)
	if !ok || mi.total != 3960900*1024 {
		t.Fatalf("total=%d ok=%v", mi.total, ok)
	}
	_, used, ok := parseProcMeminfo(fixtureMeminfo)
	if !ok || used != (3960900-3231932)*1024 {
		t.Fatalf("used=%d want %d", used, (3960900-3231932)*1024)
	}
	// Cached must include SReclaimable, as free(1) reports it.
	if mi.cached != (806416+67512)*1024 {
		t.Fatalf("cached=%d", mi.cached)
	}
	var m HostMetrics
	applyLinuxMemory(&m, fixtureMeminfo)
	if !m.MemAvailable || m.MemCached != mi.cached || m.MemAnon != 280924*1024 {
		t.Fatalf("metrics mem=%+v", m)
	}
	if m.SwapUsed != 0 || !m.SwapAvailable {
		t.Fatalf("swap=%d/%d avail=%v", m.SwapUsed, m.SwapTotal, m.SwapAvailable)
	}
}

func TestParseProcNetDevCounters(t *testing.T) {
	list := parseProcNetDev(fixtureNetDev)
	if len(list) != 3 {
		t.Fatalf("interfaces=%d", len(list))
	}
	sum := sumInterfaceCounters(list)
	if !sum.ok || sum.interfaces != 1 {
		t.Fatalf("loopback/docker must be excluded: %+v", sum)
	}
	if sum.rx != 7536313974 || sum.tx != 7896272281 {
		t.Fatalf("bytes=%d/%d", sum.rx, sum.tx)
	}
	if sum.rxPkts != 8555776 || sum.txPkts != 3646565 {
		t.Fatalf("packets=%d/%d", sum.rxPkts, sum.txPkts)
	}
	if sum.rxErrors != 3 || sum.txErrors != 1 || sum.drops != 9925 {
		t.Fatalf("errors rx=%d tx=%d drops=%d", sum.rxErrors, sum.txErrors, sum.drops)
	}
	prev := sum
	next := sumInterfaceCounters(parseProcNetDev(fixtureNetDev))
	next.rx += 2048
	next.tx += 1024
	next.rxPkts += 4
	next.at = prev.at.Add(2e9)
	if rx, tx, ok := netRates(prev, next); !ok || rx != 1024 || tx != 512 {
		t.Fatalf("rates=%v/%v ok=%v", rx, tx, ok)
	}
	if rx, _, ok := packetRates(prev, next); !ok || rx != 2 {
		t.Fatalf("packet rate=%v ok=%v", rx, ok)
	}
}

func TestDiskIORatesUseRootDisk(t *testing.T) {
	// /sys/dev/block/<maj:min> is a relative symlink into the device tree; the
	// resolver must follow it from the link's own directory.
	writeHostFixture(t, map[string]string{})
	if err := os.MkdirAll(filepath.Join(hostRoot, "sys", "dev", "block"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join("..", "..", "devices", "pci0000:00", "virtio1", "block", "vda", "vda1")
	if err := os.Symlink(target, filepath.Join(hostRoot, "sys", "dev", "block", "253:1")); err != nil {
		t.Skipf("symlinks unavailable on this filesystem: %v", err)
	}
	if got := rootParentDisk("253:1"); got != "vda" {
		t.Fatalf("parent disk=%q", got)
	}
	prev := parseProcDiskstats(fixtureDiskstats, "253:1")
	// IO is attributed to the whole disk behind the root partition.
	if prev.root != "vda" {
		t.Fatalf("root device=%q", prev.root)
	}
	next := parseProcDiskstats(fixtureDiskstats, "253:1")
	next.sectors["vda"] += 2048 // +1 MiB
	next.requests["vda"] += 4
	next.done["vda"] += 2   // 2 ms servicing 4 requests → await 0.5 ms
	next.busy["vda"] += 500 // 50% of the 1 s window
	next.at = prev.at.Add(1e9)
	io, reset, ok := diskIOAverages(prev, next)
	if reset || !ok {
		t.Fatalf("reset=%v ok=%v", reset, ok)
	}
	if io.ReadRate != 1024*1024 || io.Util != 50 || io.AwaitMs != 0.5 {
		t.Fatalf("io=%+v", io)
	}
	// A counter reset (reboot) must drop the baseline, not emit a spike.
	lower := parseProcDiskstats(fixtureDiskstats, "253:1")
	lower.sectors["vda"] = 1
	lower.at = prev.at.Add(1e9)
	if _, reset, ok := diskIOAverages(prev, lower); !reset || ok {
		t.Fatalf("reset=%v ok=%v", reset, ok)
	}
	// Without the sysfs link the partition itself is still a usable answer.
	os.Remove(filepath.Join(hostRoot, "sys", "dev", "block", "253:1"))
	if got := parseProcDiskstats(fixtureDiskstats, "vda1").root; got != "vda1" {
		t.Fatalf("fallback root device=%q", got)
	}
}

func TestLinuxVolumesHidePseudoFS(t *testing.T) {
	files := map[string]string{
		"/proc/self/mountinfo": fixtureMountinfo,
	}
	writeHostFixture(t, files)
	volumes := collectLinuxVolumes(fixtureMountinfo)
	// tmpfs and squashfs are filtered before statfs is attempted.
	for _, v := range volumes {
		if v.FSType == "tmpfs" || v.FSType == "squashfs" {
			t.Fatalf("pseudo fs leaked: %+v", v)
		}
	}
	if got := linuxRootDeviceID(fixtureMountinfo); got != "253:1" {
		t.Fatalf("root dev=%q", got)
	}
	if showLinuxVolume("/", "overlay", "overlay") != true {
		t.Fatal("root must always be shown")
	}
	if showLinuxVolume("/var/lib/docker", "ext4", "/dev/vdb1") != true {
		t.Fatal("physical mount must be shown")
	}
	if showLinuxVolume("/proc", "proc", "proc") != false {
		t.Fatal("procfs must be hidden")
	}
}

func TestCollectFromFixedFixture(t *testing.T) {
	files := map[string]string{
		"/proc/stat":                            fixtureStat(),
		"/proc/meminfo":                         fixtureMeminfo,
		"/proc/net/dev":                         fixtureNetDev,
		"/proc/loadavg":                         "0.65 0.74 0.49 2/203 59003\n",
		"/proc/uptime":                          "108158.52 900000.00\n",
		"/proc/cpuinfo":                         fixtureCPUInfo(),
		"/proc/diskstats":                       fixtureDiskstats,
		"/proc/self/mountinfo":                  fixtureMountinfo,
		"/proc/sys/kernel/osrelease":            "6.8.0-147-generic\n",
		"/etc/os-release":                       fixtureOSRelease(),
		"/proc/pressure/cpu":                    fixturePressure,
		"/proc/pressure/memory":                 fixturePressure,
		"/proc/pressure/io":                     fixturePressure,
		"/proc/self/status":                     "VmHWM:\t  162120 kB\nVmRSS:\t  110528 kB\nThreads:\t11\n",
		"/proc/self/stat":                       "52922 (lm-protocol-gate) S 1 52922 52922 0 -1 4194560 1000 0 0 0 61645 4730 0 0 20 0 11 0 1743781 999 100 0 0 0 0 0\n",
		"/sys/class/thermal/thermal_zone0/type": "x86_pkg_temp\n",
		"/sys/class/thermal/thermal_zone0/temp": "62000\n",
	}
	writeHostFixture(t, files)
	m := collectHostMetrics(nil).metrics
	if m.CPUCount == 0 || len(m.CPUCores) != 2 {
		t.Fatalf("cores=%d/%v", m.CPUCount, m.CPUCores)
	}
	if m.CPUMHz != 2199.998 {
		t.Fatalf("mhz=%v", m.CPUMHz)
	}
	if !m.TempAvailable || m.TempC != 62 {
		t.Fatalf("temp=%v %v", m.TempC, m.TempAvailable)
	}
	if !m.MemAvailable || m.MemCached != (806416+67512)*1024 {
		t.Fatalf("mem=%+v", m.MemTotal)
	}
	if !m.NetAvailable || m.NetInterfaces != 1 || m.NetRxBytes == 0 {
		t.Fatalf("net=%+v", m)
	}
	if !m.PressureAvailable || m.PressureCPU != 3.71 || m.PressureIO != 3.71 {
		t.Fatalf("pressure=%+v", m)
	}
	if m.Kernel != "6.8.0-147-generic" || m.OSName != "Ubuntu 24.04.4 LTS" {
		t.Fatalf("os=%q %q", m.OSName, m.Kernel)
	}
	if m.BootTime.Unix() != fixtureBootTime {
		t.Fatalf("boot=%v", m.BootTime)
	}
	if m.ProcessThreads != 11 || m.ProcessRSSMiB < 100 || m.ProcessPeakMiB < 158 {
		t.Fatalf("process rss=%v threads=%d peak=%v", m.ProcessRSSMiB, m.ProcessThreads, m.ProcessPeakMiB)
	}
	if m.ProcessStarted.IsZero() {
		t.Fatal("process start time missing")
	}
	if m.SampleCost <= 0 {
		t.Fatal("sample cost missing")
	}
	if m.ProcessRestarts != -1 && m.ProcessRestarts != 0 {
		t.Fatalf("restarts=%d", m.ProcessRestarts)
	}
}

func TestLinuxCPUTempSkipsNamelessZone(t *testing.T) {
	writeHostFixture(t, map[string]string{
		"/sys/class/thermal/thermal_zone0/type": "",
		"/sys/class/thermal/thermal_zone0/temp": "27800\n",
		"/sys/class/thermal/thermal_zone1/type": "acpitz\n",
		"/sys/class/thermal/thermal_zone1/temp": "45000\n",
	})
	if temp, source, ok := linuxCPUTempC(); !ok || temp != 45 || source != "thermal_zone" {
		t.Fatalf("temp=%v source=%q ok=%v", temp, source, ok)
	}
}

func TestOSReleaseAndBootTimeParsers(t *testing.T) {
	if got := parseOSRelease(fixtureOSRelease()); got != "Ubuntu 24.04.4 LTS" {
		t.Fatalf("os name=%q", got)
	}
	if got := parseOSRelease("NAME=Debian\n"); got != "Debian" {
		t.Fatalf("fallback name=%q", got)
	}
	if boot := linuxBootTime(fixtureStat(), 0); boot.Unix() != fixtureBootTime {
		t.Fatalf("boot=%v", boot)
	}
	if boot := linuxBootTime("", 60); boot.IsZero() || boot.Unix() < 1 {
		t.Fatalf("fallback boot=%v", boot)
	}
}
