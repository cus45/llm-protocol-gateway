package gateway

// selfcheck cleanup left behind by the retired CLI self-check.
//
// The console's CLI self-check (codex/claude/opencode smoke tests with the
// /__selfcheck* routes) is gone, and so is the weekly CLI deep check that
// reused its executor. What remains here is only the cleanup the gateway still
// needs at startup: a run interrupted by a crash/kill leaves "selfcheck-*" API
// keys and ~100 MiB job directories (.cache/selfcheck/sc-*) behind, and
// SweepSelfcheckLeftovers (called from app runtime) clears them.
//
// The "selfcheck-" key prefix is still meaningful elsewhere: server.go maps
// request_logs.access_source=monitor_probe from it, so it is kept here.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// selfcheckKeyPrefix is the name prefix of the transient keys the retired
// self-check created. Nothing creates them anymore; existing keys must still
// be recognized (cleanup here, access_source=monitor_probe in server.go).
const selfcheckKeyPrefix = "selfcheck-"

// cleanupSelfcheckArtifacts removes every self-check API key (matched by the
// `selfcheck-` name prefix), keeping the console free of leftover entries.
// Failures are logged but never abort.
//
// 曾经还要按 routeIDs 删临时路由；执行器移除后路由随密钥一起不再产生，
// 唯一调用方也只传 nil，所以那个参数一并去掉了。
func (s *Server) cleanupSelfcheckArtifacts() {
	keyRemoved := false
	for _, key := range s.router.State().APIKeys {
		if !strings.HasPrefix(key.Name, selfcheckKeyPrefix) {
			continue
		}
		if err := s.router.DeleteAPIKey(key.ID); err != nil {
			s.logs.AddApp("warn", "selfcheck key cleanup failed", fmt.Sprintf("key=%s err=%v", key.ID, err))
			continue
		}
		keyRemoved = true
		if s.apiKeyStore != nil {
			if err := s.apiKeyStore.DeleteAPIKey(key.ID); err != nil {
				s.logs.AddApp("warn", "selfcheck key cleanup (db) failed", fmt.Sprintf("key=%s err=%v", key.ID, err))
			}
		}
		s.logs.AddApp("info", "selfcheck api key removed", key.ID)
	}
	// Persist deletions. Always save when anything changed so in-memory/router
	// snapshots cannot resurrect selfcheck keys after a crash/restart.
	if keyRemoved {
		if err := s.saveState(); err != nil {
			s.logs.AddApp("warn", "selfcheck cleanup save failed", err.Error())
		}
	}
}

// SweepSelfcheckLeftovers removes any leftover selfcheck-* API keys after a
// crash/restart interrupted a prior run, and prunes stale job directories out
// of the cache.
func (s *Server) SweepSelfcheckLeftovers() {
	s.cleanupSelfcheckArtifacts()
	if removed, freed := pruneSelfcheckJobDirs(""); removed > 0 {
		s.logs.AddApp("info", "selfcheck cache pruned",
			fmt.Sprintf("dirs=%d freedMiB=%d kept=%d", removed, freed/(1024*1024), selfcheckJobDirsKept))
	}
}

func selfcheckCacheRoot() string {
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		return filepath.Join(cwd, ".cache", "selfcheck")
	}
	return filepath.Join(os.TempDir(), "llm-gateway-selfcheck")
}

// selfcheckJobDirsKept is how many past job directories survive a sweep.
//
// Each job dir held a full per-case client home (codex-home, opencode config,
// auth files) and routinely reached 100+ MiB. Keeping every run forever is what
// let this cache grow to ~800 MiB once; no new dirs are written any more, so
// the sweep just trims the leftovers to the newest few.
const selfcheckJobDirsKept = 2

// pruneSelfcheckJobDirs deletes all but the newest selfcheckJobDirsKept job
// directories under the selfcheck cache root, plus any directory named
// keepJobID regardless of age (empty = keep nothing special).
func pruneSelfcheckJobDirs(keepJobID string) (removed int, freedBytes int64) {
	return pruneSelfcheckJobDirsIn(selfcheckCacheRoot(), keepJobID)
}

// pruneSelfcheckJobDirsIn is pruneSelfcheckJobDirs with an explicit root, so
// tests need not chdir the whole process to reach a temp cache.
func pruneSelfcheckJobDirsIn(root, keepJobID string) (removed int, freedBytes int64) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, 0
	}
	type jobDir struct {
		name    string
		modTime time.Time
	}
	dirs := make([]jobDir, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "sc-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		dirs = append(dirs, jobDir{name: entry.Name(), modTime: info.ModTime()})
	}
	// Newest first; keep the head of the list.
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].modTime.After(dirs[j].modTime) })

	kept := 0
	for _, dir := range dirs {
		if dir.name == keepJobID {
			continue
		}
		if kept < selfcheckJobDirsKept {
			kept++
			continue
		}
		path := filepath.Join(root, dir.name)
		size := dirSizeBytes(path)
		if err := os.RemoveAll(path); err != nil {
			continue
		}
		removed++
		freedBytes += size
	}
	return removed, freedBytes
}

// dirSizeBytes best-effort sums a directory's file sizes (for the log line).
func dirSizeBytes(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
