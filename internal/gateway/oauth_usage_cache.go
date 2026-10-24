package gateway

import (
	"errors"
	"sync"
	"time"
)

var errUsageProbeFailed = errors.New("usage probe failed")

const (
	// oauthUsageFreshTTL: serve cache without upstream fetch while fresh.
	oauthUsageFreshTTL = 3 * time.Minute
	// oauthUsageStaleTTL: still return stale cache instantly; refresh async after fresh window.
	oauthUsageStaleTTL = 1 * time.Hour
	// oauthUsageFailureTTL is the negative cache: how long a failed fetch is
	// remembered before the upstream is asked again. Without it a vendor
	// answering 429 to the usage endpoint is re-asked by every poll of an open
	// console, which is both a pointless load on the vendor and the fastest way
	// to have the usage endpoint refuse an account outright.
	oauthUsageFailureTTL = 1 * time.Minute
	// oauthUsageFailureCap bounds the negative cache: a long vendor outage must
	// not turn into a permanently unreachable report once it is over.
	oauthUsageFailureCap = 10 * time.Minute
)

type oauthUsageCacheEntry struct {
	value     any
	fetchedAt time.Time
	// err and nextRetry record a failed refresh: the entry keeps the last good
	// value (so the console still shows something) and stops the upstream from
	// being asked again until nextRetry.
	err       error
	nextRetry time.Time
	failures  int
	// accountID is the OAuth account (see oauthUsageAccountID) the entry was
	// fetched for. A report outlives its account otherwise: swapping the
	// account behind a provider would keep showing — and scheduling on — the
	// previous account's usage until the stale TTL ran out.
	accountID string
}

// oauthUsageCache stores recent Claude/Cursor usage reports keyed by provider.
type oauthUsageCache struct {
	mu      sync.Mutex
	entries map[string]oauthUsageCacheEntry
}

func newOAuthUsageCache() *oauthUsageCache {
	return &oauthUsageCache{entries: make(map[string]oauthUsageCacheEntry)}
}

func (c *oauthUsageCache) get(key string) (any, bool) {
	entry, ok := c.getEntry(key)
	if !ok || !c.isFresh(entry) || entry.err != nil {
		return nil, false
	}
	return entry.value, true
}

func (c *oauthUsageCache) getAllowStale(key string) (any, bool) {
	entry, ok := c.getEntry(key)
	if !ok || entry.value == nil {
		return nil, false
	}
	return entry.value, true
}

// lastError is the failure the cache is holding for key, or nil. The console
// shows it beside the last good value instead of an empty panel.
func (c *oauthUsageCache) lastError(key string) error {
	if c == nil {
		return nil
	}
	entry, _ := c.getEntry(key)
	return entry.err
}

func (c *oauthUsageCache) getEntry(key string) (oauthUsageCacheEntry, bool) {
	if c == nil {
		return oauthUsageCacheEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return oauthUsageCacheEntry{}, false
	}
	// A recorded failure ages out by when the upstream was last asked, so a
	// vendor that stays broken does not leave a pickled error on the console
	// for days; the entry is then simply unknown and the next poll asks again.
	askedAt := entry.fetchedAt
	if entry.err != nil && entry.nextRetry.After(askedAt) {
		askedAt = entry.nextRetry
	}
	if time.Since(askedAt) > oauthUsageStaleTTL {
		delete(c.entries, key)
		return oauthUsageCacheEntry{}, false
	}
	return entry, true
}

func (c *oauthUsageCache) isFresh(entry oauthUsageCacheEntry) bool {
	return time.Since(entry.fetchedAt) <= oauthUsageFreshTTL
}

// needsRefresh reports whether the upstream should be asked now: never while a
// recorded failure is still cooling down, otherwise once the last good value
// has aged out of the fresh window.
func (c *oauthUsageCache) needsRefresh(key string) bool {
	entry, ok := c.getEntry(key)
	if !ok {
		return true
	}
	if entry.err != nil {
		return !time.Now().Before(entry.nextRetry)
	}
	return !c.isFresh(entry)
}

// waitForRefresh is how long an entry with nothing usable cached is still
// skipped, so callers that want a value can report "retrying in N" instead of
// firing another fetch.
func (c *oauthUsageCache) waitForRefresh(key string) time.Duration {
	if c == nil {
		return 0
	}
	entry, ok := c.getEntry(key)
	if !ok || entry.nextRetry.IsZero() {
		return 0
	}
	if wait := time.Until(entry.nextRetry); wait > 0 {
		return wait
	}
	return 0
}

func (c *oauthUsageCache) set(key string, value any) {
	if c == nil || value == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := oauthUsageCacheEntry{
		value:     value,
		fetchedAt: time.Now(),
	}
	if previous, ok := c.entries[key]; ok {
		entry.accountID = previous.accountID
	}
	c.entries[key] = entry
}

// setForAccount is set, recording the account the value was fetched for.
func (c *oauthUsageCache) setForAccount(key, accountID string, value any) {
	if c == nil || value == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = oauthUsageCacheEntry{
		value:     value,
		fetchedAt: time.Now(),
		accountID: accountID,
	}
}

// bindAccount tells the cache which account backs key now. An entry fetched for
// a different account is dropped — including its recorded failure, which says
// nothing about the new account. An entry with no identity yet (written by a
// caller that does not know the account) is adopted as-is.
func (c *oauthUsageCache) bindAccount(key, accountID string) {
	if c == nil || accountID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return
	}
	if entry.accountID == "" {
		entry.accountID = accountID
		c.entries[key] = entry
		return
	}
	if entry.accountID != accountID {
		delete(c.entries, key)
	}
}

// setFailureForAccount is setFailure, but first drops an entry left by a
// different account so its last good value never survives the swap.
func (c *oauthUsageCache) setFailureForAccount(key, accountID string, err error) {
	// What was cached belongs to the account that answered; if the key is now
	// bound to a different account, both the value and its cooldown are that
	// old account's and must not describe the new one.
	if accountID != "" {
		c.mu.Lock()
		previous := c.entries[key].accountID
		c.mu.Unlock()
		if previous != "" && previous != accountID {
			c.mu.Lock()
			delete(c.entries, key)
			c.mu.Unlock()
			return
		}
	}
	c.bindAccount(key, accountID)
	c.setFailure(key, err)
}

// setFailure records a failed refresh: the last good value stays, the error is
// kept for the console, and the retry wait doubles per consecutive failure so a
// vendor outage is probed less and less often, up to oauthUsageFailureCap.
// setFailure records a failed probe against key. askedAt is when the upstream
// was asked, which is the only age a recorded failure has.
func (c *oauthUsageCache) setFailure(key string, err error) {
	c.setFailureAt(key, err, time.Now())
}

// setFailureAt is setFailure on an injected clock, so a failure entry can be
// aged in a test.
func (c *oauthUsageCache) setFailureAt(key string, err error, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry.err == nil {
		// The entry is a failure from here on, so its clock starts at the ask.
		entry.fetchedAt = now
	}
	entry.failures++
	wait := oauthUsageFailureTTL << min(entry.failures-1, 8)
	if wait > oauthUsageFailureCap || wait <= 0 {
		wait = oauthUsageFailureCap
	}
	if err != nil {
		entry.err = err
	} else {
		entry.err = errUsageProbeFailed
	}
	entry.nextRetry = now.Add(wait)
	c.entries[key] = entry
}

// clearFailure forgets a recorded failure once a fetch succeeds.
func (c *oauthUsageCache) clearFailure(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return
	}
	if entry.value == nil {
		// Nothing to keep: an entry that only ever held a failure is gone.
		delete(c.entries, key)
		return
	}
	entry.err = nil
	entry.nextRetry = time.Time{}
	entry.failures = 0
	c.entries[key] = entry
}

func (c *oauthUsageCache) invalidate(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}
