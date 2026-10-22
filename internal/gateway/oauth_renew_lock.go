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

type oauthRenewLockEntry struct {
	mu   sync.Mutex
	last time.Time
}

var oauthRenewLocks struct {
	sync.Mutex
	entries map[string]*oauthRenewLockEntry
}

func oauthRenewLockKey(providerID, refreshToken string) string {
	return providerID + "\x00" + strings.TrimSpace(refreshToken)
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
	for otherKey, other := range oauthRenewLocks.entries {
		if now.Sub(other.last) > oauthRenewIdleAfter && other.mu.TryLock() {
			// 空闲够久且没人用：同一枚令牌的请求早都结束了，删掉。
			delete(oauthRenewLocks.entries, otherKey)
			other.mu.Unlock()
		}
	}
	entry := &oauthRenewLockEntry{last: now}
	oauthRenewLocks.entries[key] = entry
	return &entry.mu
}

// retireOauthRenewLock 删掉一枚已经花掉的令牌对应的锁，避免旧版本"只增不删"
// 造成的表项泄漏。只在续期的持有者（lock 此时正被自己持有）续成功时调用：
// 续完的令牌不会再被正常请求拿来上锁，表项删掉只影响此后"同一枚旧令牌"的
// 迟到请求——它们等锁后会重读路由，发现令牌已换人，直接改用新凭证。
func retireOauthRenewLock(providerID, refreshToken string, lock *sync.Mutex) {
	key := oauthRenewLockKey(providerID, refreshToken)
	oauthRenewLocks.Lock()
	if entry, ok := oauthRenewLocks.entries[key]; ok && &entry.mu == lock {
		delete(oauthRenewLocks.entries, key)
	}
	oauthRenewLocks.Unlock()
}

func oauthRenewLockCount() int {
	oauthRenewLocks.Lock()
	defer oauthRenewLocks.Unlock()
	return len(oauthRenewLocks.entries)
}

// freshTraeCredential 读出路由里最新的 Trae 凭证：调用方手里的可能是续期前的旧值。
func (s *Server) freshTraeCredential(providerID string) (domain.TraeCredential, bool) {
	provider, err := s.router.ProviderByID(providerID)
	if err != nil || provider.Trae == nil || provider.Trae.Disconnected {
		return domain.TraeCredential{}, false
	}
	return *provider.Trae, true
}

// freshWorkBuddyCredential 同上，给 WorkBuddy 用。
func (s *Server) freshWorkBuddyCredential(providerID string) (domain.WorkBuddyCredential, bool) {
	provider, err := s.router.ProviderByID(providerID)
	if err != nil || provider.WorkBuddy == nil {
		return domain.WorkBuddyCredential{}, false
	}
	return *provider.WorkBuddy, true
}

// freshCursorCredential 同上，给 Cursor 用。
func (s *Server) freshCursorCredential(providerID string) (domain.CursorOAuthCredential, bool) {
	provider, err := s.router.ProviderByID(providerID)
	if err != nil || provider.CursorOAuth == nil {
		return domain.CursorOAuthCredential{}, false
	}
	return *provider.CursorOAuth, true
}
