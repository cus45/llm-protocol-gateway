package gateway

import (
	"strings"
	"sync"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// 续期令牌必须串行：Trae / WorkBuddy / Cursor 的 refresh token 一般只能用一次，
// 两个并发请求同时发现令牌快过期、拿同一枚 refresh token 去换，后到的那个会被
// 判无效，严重的还会把整条令牌链作废、逼用户重新登录。
//
// 锁按「provider + refresh token」取：同一枚令牌永远只被一个请求去换；换完再
// 读一次路由里的最新凭证，等锁期间别人已经续好就直接用，不再花第二枚令牌。
var oauthRenewLocks sync.Map

func oauthRenewLock(providerID, refreshToken string) *sync.Mutex {
	key := providerID + "\x00" + strings.TrimSpace(refreshToken)
	lock, _ := oauthRenewLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
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
