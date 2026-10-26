package gateway

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// F3（.claude/audits/review-round2.md）：一枚已经死掉的 refresh token 的失败结论
// 只该在天真里保留 oauthRenewOutcomeWindow 那么一会儿；有人重新登录（retire）时
// 不能连结论一起删掉，否则排队的请求又各花一次同一枚死令牌。
func TestOauthRenewRetireKeepsLiveFailure(t *testing.T) {
	providerID := "f3-retire"
	refreshToken := "f3-dead-token"
	key := oauthRenewLockKey(providerID, refreshToken)
	lock := oauthRenewLock(providerID, refreshToken)
	lock.Lock()
	settleOauthRenew(providerID, refreshToken, domain.Provider{ID: providerID}, errDeadRefreshTokenForTest)
	retireOauthRenewLock(providerID, refreshToken, lock)
	lock.Unlock()
	oauthRenewLocks.Lock()
	_, survived := oauthRenewLocks.entries[key]
	oauthRenewLocks.Unlock()
	if !survived {
		t.Fatal("a live failure's entry was deleted by a login (retire)")
	}
	t.Cleanup(func() {
		oauthRenewLocks.Lock()
		delete(oauthRenewLocks.entries, key)
		oauthRenewLocks.Unlock()
	})

	waiter, settled := beginOauthRenew(providerID, refreshToken, func() (domain.Provider, bool) { return domain.Provider{}, false })
	waiter.Unlock()
	if settled == nil || settled.err == nil {
		t.Fatalf("a live failure was forgotten by a login (retire): settled = %+v", settled)
	}

	// 只有等结论本身也过了窗口，retire 才能把表项删掉（sweep 本来就负责这件事）。
	oauthRenewLocks.Lock()
	oauthRenewLocks.entries[key].outcome.at = time.Now().Add(-2 * oauthRenewOutcomeWindow)
	oauthRenewLocks.Unlock()
	lock2 := oauthRenewLock(providerID, refreshToken)
	lock2.Lock()
	retireOauthRenewLock(providerID, refreshToken, lock2)
	lock2.Unlock()
	oauthRenewLocks.Lock()
	_, stillThere := oauthRenewLocks.entries[key]
	oauthRenewLocks.Unlock()
	if stillThere {
		t.Fatal("an expired outcome's entry survived retire")
	}
}

// waitForRefreshCalls waits until the fake token endpoint has been called want
// times, or gives up after limit.
func waitForRefreshCalls(calls *int32, want int32, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(calls) >= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

var errDeadRefreshTokenForTest = &deadRefreshTokenError{}

type deadRefreshTokenError struct{}

func (*deadRefreshTokenError) Error() string { return "refresh token revoked" }

// F2（review-round2）：401 触发的强制续期失败后，等锁的请求不能再拿到调用方那
// 份带着死 access token 的 provider——那会让 sendWorkBuddyChat 在建立任何 HTTP
// 响应之前就返回，failover 把它记成传输错误，401 该有的重试永远不发生。
//
// 等锁时路由里已经换了凭证（用户在控制台重新登录），而续期依然失败：共享的失败
// 结论只省掉那次网络开销，waiter 拿到的 provider 必须带上路由里当前的凭证，
// 而不是调用方手里那份旧快照。
func TestWorkBuddyForceRefreshFailureHandsWaitersTheCurrentCredential(t *testing.T) {
	hold := make(chan struct{})
	calls := workBuddyFailingRefreshRound(t, hold)
	server, router := newRefreshLockServer(t)
	provider := workBuddyExpiredProvider("wb-f2")
	provider.WorkBuddy.AccessToken = "dead-access"
	provider.WorkBuddy.RefreshToken = "f2-shared-refresh"
	added, err := router.AddProvider(provider)
	if err != nil {
		t.Fatalf("add provider: %v", err)
	}
	t.Cleanup(func() {
		oauthRenewLocks.Lock()
		for key, entry := range oauthRenewLocks.entries {
			if entry.outcome != nil {
				entry.outcome.at = time.Now().Add(-2 * oauthRenewOutcomeWindow)
			}
			delete(oauthRenewLocks.entries, key)
		}
		oauthRenewLocks.Unlock()
	})

	type result struct {
		provider domain.Provider
		err      error
	}
	// 第一个请求拿到锁去做那次共享的续期（它自己也会失败）。
	renewer := make(chan result, 1)
	go func() {
		p, err := server.forceRefreshWorkBuddyToken(added.ID)
		renewer <- result{provider: p, err: err}
	}()
	if !waitForRefreshCalls(calls, 1, 10*time.Second) {
		t.Fatal("the shared refresh never reached the endpoint")
	}
	// 续期还挂在上游时，排队的请求到了（都停在同一枚令牌的锁上）。
	results := make(chan result, 5)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 调用方手里的是续期前那份带着死 access token 的快照。
			p, err := server.forceRefreshWorkBuddyToken(added.ID)
			results <- result{provider: p, err: err}
		}()
	}
	time.Sleep(200 * time.Millisecond) // 五个请求都已走到锁上
	// 等锁期间用户重新登录：路由里换成了另一份凭证（新的 access、新的 refresh）。
	signedIn := domain.WorkBuddyCredential{
		AccessToken:  "fresh-login-access",
		RefreshToken: "f2-signed-in-again",
		ExpiresAt:    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	if _, err := router.SetProviderWorkBuddy(added.ID, added.AuthType, signedIn); err != nil {
		t.Fatalf("sign in: %v", err)
	}
	close(hold)
	wg.Wait()
	close(results)
	lead := <-renewer

	if lead.err == nil {
		t.Fatalf("the shared renewal fails, so the request that ran it must see the error")
	}
	all := make([]result, 0, 5)
	for res := range results {
		all = append(all, res)
	}
	if len(all) != 5 {
		t.Fatalf("got %d answers, want 5", len(all))
	}
	for _, res := range all {
		if res.err == nil {
			t.Fatalf("a failed renewal must reach the caller as an error, not a provider to retry with")
		}
		if got := res.provider.WorkBuddy.AccessToken; got != signedIn.AccessToken {
			t.Fatalf("a waiter got access token %q, want the router's current %q: a dead snapshot turns the upstream 401 into a transport error", got, signedIn.AccessToken)
		}
	}
}
