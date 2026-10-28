//go:build darwin || linux

package gateway

import "golang.org/x/sys/unix"

// statfsVolume reports total/free bytes of a mount point (volume list).
func statfsVolume(mount string) (total, free uint64, ok bool) {
	return statfsAny(mount)
}

// processOSThreads is unknown on Unix without /proc; 0 means "not reported"
// and finishProcess then falls back to the Go runtime's thread count.
func processOSThreads() int { return 0 }

// applyDiskRoot fills the root filesystem from statfs.
func (m *HostMetrics) applyDiskRoot() {
	total, used, ok := statfsRoot()
	if !ok || total == 0 {
		return
	}
	m.DiskTotal = total
	m.DiskUsed = used
	m.DiskPercent = percentOf(used, total)
	m.DiskAvailable = true
}

// statfsRoot returns total/used bytes of the filesystem backing "/".
func statfsRoot() (total, used uint64, ok bool) {
	var st unix.Statfs_t
	if err := unix.Statfs("/", &st); err != nil {
		return 0, 0, false
	}
	bsize := uint64(st.Bsize)
	if bsize == 0 {
		return 0, 0, false
	}
	total = st.Blocks * bsize
	free := st.Bavail * bsize
	if total == 0 || free > total {
		return 0, 0, false
	}
	return total, total - free, true
}

// statfsAny reports total/free bytes of one mount point.
func statfsAny(path string) (total, free uint64, ok bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bsize := uint64(st.Bsize)
	if bsize == 0 {
		return 0, 0, false
	}
	total = st.Blocks * bsize
	free = st.Bavail * bsize
	if total == 0 || free > total {
		return 0, 0, false
	}
	return total, free, true
}
