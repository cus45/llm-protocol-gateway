package gateway

import (
	"errors"
	"testing"
	"time"
)

// A failed probe must be remembered: without it every console poll asks the
// vendor again, and a 429 on the usage endpoint is re-fired once per poll.
func TestUsageCacheRemembersAFailureAndSkipsTheUpstream(t *testing.T) {
	cache := newOAuthUsageCache()
	key := "claude:p1"
	if !cache.needsRefresh(key) {
		t.Fatal("an empty cache should want a refresh")
	}
	cache.setFailure(key, errors.New("429 too many requests"))
	if cache.needsRefresh(key) {
		t.Fatal("a recorded failure should hold off the next fetch")
	}
	if wait := cache.waitForRefresh(key); wait <= 0 {
		t.Fatalf("waitForRefresh = %v, want a positive cooldown", wait)
	}
	if err := cache.lastError(key); err == nil || err.Error() != "429 too many requests" {
		t.Fatalf("lastError = %v", err)
	}
}

// A later failure must not erase the snapshot the console is showing.
func TestUsageCacheFailureKeepsTheLastGoodValue(t *testing.T) {
	cache := newOAuthUsageCache()
	key := "claude:p1"
	report := ClaudeOAuthUsageReport{Available: true, FetchedAt: "2026-10-23T00:00:00Z"}
	cache.set(key, report)
	cache.setFailure(key, errors.New("boom"))
	cached, ok := cache.getAllowStale(key)
	if !ok {
		t.Fatal("the last good value should survive a failed refresh")
	}
	if got, ok := cached.(ClaudeOAuthUsageReport); !ok || !got.Available {
		t.Fatalf("cached value = %#v", cached)
	}
	if _, ok := cache.get(key); ok {
		t.Fatal("a value under a recorded failure is not fresh")
	}
}

// Consecutive failures back off, and the backoff is capped so a vendor outage is
// not turned into a permanently unreachable panel.
func TestUsageCacheFailureBackoffIsCapped(t *testing.T) {
	cache := newOAuthUsageCache()
	key := "cursor:p1"
	var previous time.Duration
	for i := 0; i < 12; i++ {
		cache.setFailure(key, errors.New("boom"))
		wait := cache.waitForRefresh(key)
		if wait > oauthUsageFailureCap {
			t.Fatalf("failure %d waited %v, above the cap %v", i+1, wait, oauthUsageFailureCap)
		}
		if i > 0 && wait < previous && wait != oauthUsageFailureCap {
			t.Fatalf("failure %d waited less (%v) than the one before (%v)", i+1, wait, previous)
		}
		previous = wait
	}
	if previous <= oauthUsageFailureTTL {
		t.Fatalf("backoff never grew past the first interval: %v", previous)
	}
}

// A successful refresh clears the failure so the panel does not stay red.
func TestUsageCacheSuccessClearsTheFailure(t *testing.T) {
	cache := newOAuthUsageCache()
	key := "claude:p1"
	cache.setFailure(key, errors.New("boom"))
	cache.clearFailure(key)
	if err := cache.lastError(key); err != nil {
		t.Fatalf("lastError after success = %v", err)
	}
	if !cache.needsRefresh(key) {
		t.Fatal("after a success the entry is empty and should refresh")
	}
	cache.set(key, ClaudeOAuthUsageReport{Available: true})
	if cache.needsRefresh(key) {
		t.Fatal("a fresh report should not trigger a fetch")
	}
}

// The overage window turns requests away once the plan is spent, so it must be
// parsed and it must count when deciding a report is usable.
func TestClaudeUsageParsesTheOverageWindow(t *testing.T) {
	report := ClaudeOAuthUsageReport{
		SevenDayOverageIncluded: parseClaudeOAuthUsageBucket(map[string]any{"utilization": 42.0, "resets_at": "2026-11-01T00:00:00Z"}),
	}
	if report.SevenDayOverageIncluded == nil || report.SevenDayOverageIncluded.Utilization != 42 {
		t.Fatalf("overage window not parsed: %#v", report.SevenDayOverageIncluded)
	}
}

// A failure is not a reading: it ages out by when the upstream was last asked,
// so a vendor that stays broken does not pin a stale error in the console.
func TestUsageCacheExpiresAFailureEntry(t *testing.T) {
	cache := newOAuthUsageCache()
	key := "claude:p1"
	asked := time.Now().Add(-oauthUsageStaleTTL - time.Minute)
	cache.setFailureAt(key, errUsageProbeFailed, asked)
	// setFailureAt keeps the failure cooling down from the ask as well.
	cache.mu.Lock()
	entry := cache.entries[key]
	entry.nextRetry = asked
	cache.entries[key] = entry
	cache.mu.Unlock()
	if _, ok := cache.getAllowStale(key); ok {
		t.Fatal("a failure older than the stale TTL must not be served")
	}
	if err := cache.lastError(key); err != nil {
		t.Fatalf("an expired failure must be forgotten, got %v", err)
	}
	if !cache.needsRefresh(key) {
		t.Fatal("an expired failure must let the next poll ask again")
	}
}

// The last good snapshot is still served while the probe that followed it
// failed, and the error is reported beside it.
func TestUsageCacheKeepsValueAndErrorTogether(t *testing.T) {
	cache := newOAuthUsageCache()
	key := "claude:p1"
	cache.set(key, ClaudeOAuthUsageReport{Available: true})
	cache.setFailure(key, errors.New("upstream 500"))
	value, ok := cache.getAllowStale(key)
	if !ok {
		t.Fatal("the last good snapshot must still be served")
	}
	if report, ok := value.(ClaudeOAuthUsageReport); !ok || !report.Available {
		t.Fatalf("value lost: %#v", value)
	}
	if err := cache.lastError(key); err == nil || err.Error() != "upstream 500" {
		t.Fatalf("the failure must be readable beside the value, got %v", err)
	}
}
