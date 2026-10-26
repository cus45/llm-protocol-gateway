package gateway

import (
	"strings"
	"sync"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// 续期令牌必须串行：Trae / WorkBuddy / Cursor 的 refresh token 一般只能用一次，
// 两个并发请求同时发现令牌快过期、拿同一枚 refresh token 去换，后到的那个会被
// 判无效，严重的还会把整条令牌链作废、逼用户重新登录。
//
// 锁按「provider + refresh token」取：同一枚令牌永远只被一个请求去换。
//
// 只靠「等锁后读一次路由」并不够：别人续完的令牌可能因为时钟偏差（expires_in
// 很短、或代理改了时钟）依旧判为"该续期"，于是又被花一次。所以拿到锁之后
// 以路由里的最新凭证为准：refresh token 换了人，就说明别人已经续过，直接用它。

// oauthRenewIdleAfter 是一枚锁空闲多久后可以从表里回收：锁只在"某个 refresh
// token 正在被花"期间有意义，过期令牌的锁永远回收不掉才是真的泄漏（旧版本就是
// 只要有新令牌就一直往表里加，永不删除）。
const oauthRenewIdleAfter = 10 * time.Minute

// oauthRenewOutcomeWindow is how long a failed renewal is remembered for the
// waiters that queued behind it. Past it a new attempt is allowed: a vendor
// outage ends, and the next request should be the one that finds out.
const oauthRenewOutcomeWindow = 10 * time.Second

// oauthRenewOutcome is one renewal's answer, shared with its waiters.
type oauthRenewOutcome struct {
	provider domain.Provider
	err      error
	at       time.Time
	// current is the provider as it stands when the waiter takes the answer,
	// read from the router under the lock: never the renewer's own snapshot,
	// which a sign-in may have overtaken.
	current domain.Provider
}

type oauthRenewLockEntry struct {
	mu   sync.Mutex
	last time.Time
	// outcome is what the renewal of this token answered, kept for the callers
	// that were waiting on the lock while it ran: a failed renewal must not be
	// retried once per waiter with the same one-shot refresh token.
	outcome *oauthRenewOutcome
}

// oauthRenewOutcomeFrom wraps a renewal's result so later waiters can reuse it.
func oauthRenewOutcomeFrom(provider domain.Provider, err error) *oauthRenewOutcome {
	return &oauthRenewOutcome{provider: provider, err: err}
}

var oauthRenewLocks struct {
	sync.Mutex
	entries map[string]*oauthRenewLockEntry
}

func oauthRenewLockKey(providerID, refreshToken string) string {
	return providerID + "\x00" + strings.TrimSpace(refreshToken)
}

// readOauthRenewProvider re-reads the provider behind the lock while the caller
// holds it.
type readOauthRenewProvider func() (domain.Provider, bool)

// beginOauthRenew is the lock plus the one question a renewal has to ask under
// it: did the renewal this caller waited for already finish, and how? A caller
// that took the lock and then met a failure that just happened does not spend
// the same refresh token again — it is given the same answer, together with the
// provider as the router has it now, so it can act on the state the failure
// left behind rather than on a copy the renewal predates.
//
// The lock is returned so the caller can hand it to
// retireOauthRenewLock/settleOauthRenew on its way out.
func beginOauthRenew(providerID, refreshToken string, fresh readOauthRenewProvider) (*sync.Mutex, *oauthRenewOutcome) {
	lock := oauthRenewLock(providerID, refreshToken)
	lock.Lock()
	now := time.Now()
	oauthRenewLocks.Lock()
	sweepOauthRenewLocksLocked(now)
	key := oauthRenewLockKey(providerID, refreshToken)
	var settled *oauthRenewOutcome
	if entry, ok := oauthRenewLocks.entries[key]; ok && &entry.mu == lock {
		entry.last = now
		if entry.outcome != nil && now.Sub(entry.outcome.at) < oauthRenewOutcomeWindow {
			settled = entry.outcome
		}
	}
	oauthRenewLocks.Unlock()
	if settled == nil {
		return lock, nil
	}
	// The router is read without oauthRenewLocks held, and the entry is kept
	// fresh afterwards: holding the table across a router read would stall every
	// waiter behind the renewer and pin them to the reading moment's credential.
	handed := *settled
	provider, ok := fresh()
	oauthRenewLocks.Lock()
	if entry, ok := oauthRenewLocks.entries[key]; ok && &entry.mu == lock {
		entry.last = time.Now()
	}
	oauthRenewLocks.Unlock()
	if ok {
		handed.current = provider
	}
	return lock, &handed
}

// settleOauthRenew records the outcome of a renewal that failed, so the callers
// queued behind it share its answer instead of retrying the same token. A
// renewal that succeeded retires the lock instead (retireOauthRenewLock): its
// token is spent and nobody should wait on it again.
//
// The outcome is recorded even when this caller no longer holds the entry (the
// renewal it waited on succeeded while its own attempt failed) and even when
// the lock it waited on is already retired: the waiters take the router's
// current credential, so the old token's error cannot be pinned on requests
// that already carry a new one — but a spent token that just failed must not
// have its failure forgotten either.
func settleOauthRenew(providerID, refreshToken string, provider domain.Provider, err error) {
	oauthRenewLocks.Lock()
	if oauthRenewLocks.entries == nil {
		oauthRenewLocks.entries = map[string]*oauthRenewLockEntry{}
	}
	entry, ok := oauthRenewLocks.entries[oauthRenewLockKey(providerID, refreshToken)]
	if !ok {
		entry = &oauthRenewLockEntry{}
		oauthRenewLocks.entries[oauthRenewLockKey(providerID, refreshToken)] = entry
	}
	entry.outcome = &oauthRenewOutcome{provider: provider, err: err, at: time.Now()}
	entry.last = time.Now()
	oauthRenewLocks.Unlock()
}

func oauthRenewLock(providerID, refreshToken string) *sync.Mutex {
	key := oauthRenewLockKey(providerID, refreshToken)
	now := time.Now()
	oauthRenewLocks.Lock()
	defer oauthRenewLocks.Unlock()
	if oauthRenewLocks.entries == nil {
		oauthRenewLocks.entries = map[string]*oauthRenewLockEntry{}
	}
	if entry, ok := oauthRenewLocks.entries[key]; ok {
		return &entry.mu
	}
	sweepOauthRenewLocksLocked(now)
	entry := &oauthRenewLockEntry{last: now}
	oauthRenewLocks.entries[key] = entry
	return &entry.mu
}

// retireOauthRenewLock 删掉一枚已经花掉的令牌对应的锁，避免旧版本"只增不删"
// 造成的表项泄漏。只在续期的持有者（lock 此时正被自己持有）续成功时调用：
// 续完的令牌不会再被正常请求拿来上锁，表项删掉只影响此后"同一枚旧令牌"的
// 迟到请求——它们等锁后会重读路由，发现令牌已换人，直接改用新凭证。
//
// 带失败结论的表项不删：重新登录并不会让那枚死令牌"变成能用"，删掉结论只会让
// 排队的请求再各花一次同一枚死令牌。结论由 sweep 在 oauthRenewOutcomeWindow
// 之后连同表项一起回收，表不会因此增长。
func retireOauthRenewLock(providerID, refreshToken string, lock *sync.Mutex) {
	key := oauthRenewLockKey(providerID, refreshToken)
	oauthRenewLocks.Lock()
	if entry, ok := oauthRenewLocks.entries[key]; ok && !entry.hasLiveOutcomeLocked(time.Now()) && (&entry.mu == lock || entry.mu.TryLock()) {
		delete(oauthRenewLocks.entries, key)
		if &entry.mu != lock {
			entry.mu.Unlock()
		}
	}
	oauthRenewLocks.Unlock()
}

// hasLiveOutcomeLocked reports whether the entry still owes an answer to
// waiters: a failure recorded within oauthRenewOutcomeWindow.
func (e *oauthRenewLockEntry) hasLiveOutcomeLocked(now time.Time) bool {
	return e.outcome != nil && now.Sub(e.outcome.at) < oauthRenewOutcomeWindow
}

// retireSpreadOauthRenewLock retires a spent token's lock after late arrivals
// already shared its failure: a success now (the provider recovered) means the
// failure no longer describes anything, and a success followed by a retire would
// otherwise leave the table growing one entry per spent token. The failure's
// waiters are gone — they took it and moved on — so only a new failure may
// settle the entry again.
func retireSpreadOauthRenewLock(providerID, refreshToken string, lock *sync.Mutex) {
	key := oauthRenewLockKey(providerID, refreshToken)
	oauthRenewLocks.Lock()
	if entry, ok := oauthRenewLocks.entries[key]; ok && &entry.mu == lock && entry.outcome != nil &&
		time.Since(entry.outcome.at) >= oauthRenewOutcomeWindow {
		delete(oauthRenewLocks.entries, key)
	}
	oauthRenewLocks.Unlock()
}

// sweepOauthRenewLocksLocked drops the entries nothing can use any more: idle for
// oauthRenewIdleAfter, or carrying an outcome that has been shared for longer
// than oauthRenewOutcomeWindow. Called by both entry points under
// oauthRenewLocks, so a request path cannot grow the table with tokens whose
// outcome every waiter has already taken.
func sweepOauthRenewLocksLocked(now time.Time) {
	for otherKey, other := range oauthRenewLocks.entries {
		stale := now.Sub(other.last) > oauthRenewIdleAfter ||
			(other.outcome != nil && now.Sub(other.outcome.at) > oauthRenewOutcomeWindow)
		if stale && other.mu.TryLock() {
			// 空闲够久且没人用：同一枚令牌的请求早都结束了，删掉。
			delete(oauthRenewLocks.entries, otherKey)
			other.mu.Unlock()
		}
	}
}

func oauthRenewLockCount() int {
	oauthRenewLocks.Lock()
	defer oauthRenewLocks.Unlock()
	return len(oauthRenewLocks.entries)
}

// freshTraeCredential 读出路由里最新的 Trae 凭证：调用方手里的可能是续期前的旧值。
func (s *Server) freshTraeCredential(providerID string) (domain.TraeCredential, bool) {
	provider, ok := s.oauthRenewProvider(providerID)
	if !ok || provider.Trae == nil || provider.Trae.Disconnected {
		return domain.TraeCredential{}, false
	}
	return *provider.Trae, true
}

// freshWorkBuddyCredential 同上，给 WorkBuddy 用。
func (s *Server) freshWorkBuddyCredential(providerID string) (domain.WorkBuddyCredential, bool) {
	provider, ok := s.oauthRenewProvider(providerID)
	if !ok || provider.WorkBuddy == nil {
		return domain.WorkBuddyCredential{}, false
	}
	return *provider.WorkBuddy, true
}

// freshCursorCredential 同上，给 Cursor 用。
func (s *Server) freshCursorCredential(providerID string) (domain.CursorOAuthCredential, bool) {
	provider, ok := s.oauthRenewProvider(providerID)
	if !ok || provider.CursorOAuth == nil {
		return domain.CursorOAuthCredential{}, false
	}
	return *provider.CursorOAuth, true
}

// oauthRenewProvider reads a provider from the router.
func (s *Server) oauthRenewProvider(providerID string) (domain.Provider, bool) {
	if s == nil || s.router == nil {
		return domain.Provider{}, false
	}
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		return domain.Provider{}, false
	}
	return provider, true
}

// oauthRenewProviderReader is the readOauthRenewProvider for one server.
func (s *Server) oauthRenewProviderReader(providerID string) readOauthRenewProvider {
	return func() (domain.Provider, bool) { return s.oauthRenewProvider(providerID) }
}

// settledOauthRenewFailure turns an answer a waiter queued behind into what the
// waiter acts on: the failure itself, and the provider carrying the router's
// credential as it stands now. The shared answer suppresses the network spend of
// one token, never the use of a credential that has changed since (a sign-in).
func settledOauthRenewFailure(settled *oauthRenewOutcome, forProvider domain.Provider) (domain.Provider, error) {
	if settled.current.ID != "" {
		return settled.current, settled.err
	}
	return forProvider, settled.err
}
