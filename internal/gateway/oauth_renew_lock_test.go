package gateway

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// workBuddyRefreshRound 起一个假的 WorkBuddy 令牌端点，记录被请求了几次；
// 每次慢一点再换一枚新 access token，好让并发请求必然撞在一起。
func workBuddyRefreshRound(t *testing.T, delay time.Duration) *int32 {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round := atomic.AddInt32(&calls, 1)
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"new-access-` + strconv.Itoa(int(round)) + `","refreshToken":"new-refresh","expiresIn":3600,"tokenType":"Bearer"}}`))
	}))
	t.Cleanup(server.Close)
	previous := workBuddyCNEndpoint
	workBuddyCNEndpoint = server.URL
	t.Cleanup(func() { workBuddyCNEndpoint = previous })
	return &calls
}

func workBuddyExpiredProvider(id string) domain.Provider {
	return domain.Provider{
		ID:       id,
		Name:     id,
		AuthType: domain.AuthTypeWorkBuddy,
		WorkBuddy: &domain.WorkBuddyCredential{
			AccessToken:  "old-access",
			RefreshToken: "old-refresh",
			ExpiresAt:    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		},
	}
}

// workBuddyUnexpiredProvider 是一枚「没有过期，但手上这份早该续了」的凭证：
// 距真正过期还差两天，但已进入续期提前量，续期者一定会去刷新。
// 过期时刻直接由 workBuddyTokenExpirySkew 推出，不要写死数字，否则提前量一调整
// 这个用例就会悄悄变成「根本不需要续期」，断言 calls==1 恒失败。
func workBuddyUnexpiredProvider(id string) domain.Provider {
	return domain.Provider{
		ID:       id,
		Name:     id,
		AuthType: domain.AuthTypeWorkBuddy,
		WorkBuddy: &domain.WorkBuddyCredential{
			AccessToken:  "old-access",
			RefreshToken: "old-refresh",
			ExpiresAt:    time.Now().Add(workBuddyTokenExpirySkew / 2).UTC().Format(time.RFC3339),
		},
	}
}

func newRefreshLockServer(t *testing.T) (*Server, *Router) {
	t.Helper()
	router := NewRouter(domain.GatewayState{})
	return NewServer(router, monitor.NewStore()), router
}

// 并发请求几乎同时发现令牌过期时，只能花一枚 refresh token：后到的等锁后
// 直接用别人续好的，不再发第二次请求。
func TestWorkBuddyRefreshSpentOnceUnderConcurrency(t *testing.T) {
	calls := workBuddyRefreshRound(t, 150*time.Millisecond)
	server, router := newRefreshLockServer(t)
	added, err := router.AddProvider(workBuddyExpiredProvider("wb-test"))
	if err != nil {
		t.Fatalf("add provider: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refreshed, err := server.ensureFreshWorkBuddyToken(added)
			if err != nil {
				t.Errorf("ensure fresh: %v", err)
				return
			}
			if refreshed.WorkBuddy == nil || !strings.HasPrefix(refreshed.WorkBuddy.AccessToken, "new-access-") {
				t.Errorf("provider came back with stale credential: %+v", refreshed.WorkBuddy)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("refresh endpoint called %d times, want exactly 1", got)
	}
	stored, err := router.ProviderByID(added.ID)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if stored.WorkBuddy == nil || !strings.HasPrefix(stored.WorkBuddy.AccessToken, "new-access-") {
		t.Fatalf("router kept stale credential: %+v", stored.WorkBuddy)
	}
}

// 换回来的令牌写回路由，下一次请求直接用，不再花第二枚 refresh token。
func TestWorkBuddyRefreshStoresRotatedToken(t *testing.T) {
	calls := workBuddyRefreshRound(t, 0)
	server, router := newRefreshLockServer(t)
	provider := workBuddyExpiredProvider("wb-two")
	provider.WorkBuddy.RefreshToken = "first-refresh"
	added, err := router.AddProvider(provider)
	if err != nil {
		t.Fatalf("add provider: %v", err)
	}
	if _, err := server.ensureFreshWorkBuddyToken(added); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	stored, _ := router.ProviderByID(added.ID)
	if stored.WorkBuddy.RefreshToken != "new-refresh" {
		t.Fatalf("router refresh token = %q, want new-refresh", stored.WorkBuddy.RefreshToken)
	}
	// 换过令牌后凭证已经有效：不再花第二枚。
	if _, err := server.ensureFreshWorkBuddyToken(stored); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("refresh endpoint called %d times, want 1", got)
	}
}

// workBuddyFailingRefreshRound 起一个永远 5xx 的令牌端点（可挂起，让并发请求
// 肯定撞在同一枚 refresh token 上），并记录被请求了几次。
func workBuddyFailingRefreshRound(t *testing.T, hold chan struct{}) *int32 {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-hold
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"code":502,"msg":"upstream unavailable"}`))
	}))
	t.Cleanup(server.Close)
	previous := workBuddyCNEndpoint
	workBuddyCNEndpoint = server.URL
	t.Cleanup(func() { workBuddyCNEndpoint = previous })
	return &calls
}

// 续期失败时也不能让每个等锁的请求各花一次同一枚（一次性的）refresh token：
// 失败的结论要让排队的请求共用，直到 oauthRenewOutcomeWindow 过去。
// 令牌已过期时结论里的错误就是它们全部拿到的答案。
func TestWorkBuddyFailedRefreshIsSpentOnceUnderConcurrency(t *testing.T) {
	hold := make(chan struct{})
	calls := workBuddyFailingRefreshRound(t, hold)
	server, router := newRefreshLockServer(t)
	added, err := router.AddProvider(workBuddyExpiredProvider("wb-fail"))
	if err != nil {
		t.Fatalf("add provider: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := server.ensureFreshWorkBuddyToken(added); err == nil {
				t.Error("a failed renewal of an expired token must reach the caller as an error")
			}
		}()
	}
	time.Sleep(300 * time.Millisecond) // 六个请求都已排进同一枚令牌的锁
	close(hold)
	wg.Wait()
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("the refresh endpoint was called %d times for one expired token, want 1: each waiter spent the same one-shot refresh token", got)
	}

	// 结论只保留一小段时间：过了窗口之后的新请求要能再试一次（供应商恢复了）。
	oauthRenewLocks.Lock()
	for _, entry := range oauthRenewLocks.entries {
		if entry.outcome != nil {
			entry.outcome.at = time.Now().Add(-2 * oauthRenewOutcomeWindow)
		}
	}
	oauthRenewLocks.Unlock()
	if _, err := server.ensureFreshWorkBuddyToken(added); err == nil {
		t.Fatal("the endpoint still fails, so a retried renewal must still error")
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("past the outcome window a new renewal must be attempted: calls = %d, want 2", got)
	}
}

// 令牌在续期提前量之内、但还没真正过期时，续期抖动不该让排队请求全灭：续期者
// 自己按「未过期就继续服务」放行，等锁方必须同一个口径（R39 D39-1）。
// 修复前实测 served=1 failed=3 renewCalls=1。
//
// 这个测试里除 t.Fatalf 外不要加任何 t.Logf：Go 1.26.1 的 Windows 工具链下，
// 在被调的判定前面多一次格式化调用会改变内联，workBuddyTokenNeedsRefresh 会把
// 两天后的过期时刻判成已到（memory go126-windows-slice-compare-miscompile）。
func TestWorkBuddyJitterServesEveryWaiterOnAnUnexpiredToken(t *testing.T) {
	defer resetOauthRenewLocks(t)
	hold := make(chan struct{})
	calls := workBuddyFailingRefreshRound(t, hold)
	server, router := newRefreshLockServer(t)
	// 不再过期，但手上这枚令牌早该续了（与 workBuddyExpiredProvider 的写法一致，
	// 窗口给足一小时：Windows 上 Go 1.26 对相邻时刻的比较不可靠，见 memory
	// go126-windows-slice-compare-miscompile，测试自身别踩）。
	provider := workBuddyUnexpiredProvider("wb-jitter")
	added, err := router.AddProvider(provider)
	if err != nil {
		t.Fatalf("add provider: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := server.ensureFreshWorkBuddyToken(added); err != nil {
				errs <- err
			}
		}()
	}
	time.Sleep(300 * time.Millisecond) // 四个请求都已排进同一枚令牌的锁
	close(hold)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a jitter on an unexpired token must not fail the requests queued behind it: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("the refresh endpoint was called %d times, want exactly 1", got)
	}
}

// 续期结论只对一个已过期的令牌说「没有可用的了」；同一个失败落在未过期令牌上时，
// 等锁方拿到的必须是「照常服务」，且供应商是路由里当前那份凭证。
func TestSettledOauthRenewFailureServesWhileTheTokenWorks(t *testing.T) {
	signedIn := domain.Provider{ID: "p", WorkBuddy: &domain.WorkBuddyCredential{AccessToken: "fresh"}}
	settled := &oauthRenewOutcome{provider: domain.Provider{ID: "p"}, current: signedIn, err: errDeadRefreshTokenForTest}
	got, err := settledOauthRenewFailure(settled, domain.Provider{ID: "p"})
	if err != nil {
		t.Fatalf("a failed renewal of a token that still works must not stop the waiter: %v", err)
	}
	if got.WorkBuddy == nil || got.WorkBuddy.AccessToken != "fresh" {
		t.Fatalf("the waiter got %+v, want the router's current credential", got)
	}
	settled.credentialExpired = true
	got, err = settledOauthRenewFailure(settled, domain.Provider{ID: "p"})
	if err == nil {
		t.Fatal("a failed renewal of an expired token leaves nothing to serve with: the waiter must see the error")
	}
	if got.WorkBuddy == nil || got.WorkBuddy.AccessToken != "fresh" {
		t.Fatalf("even the error carries the router's current credential, got %+v", got)
	}
}

// resetOauthRenewLocks clears the shared lock table so one test's failures are
// not another's (the table is process-wide on purpose).
func resetOauthRenewLocks(t *testing.T) {
	t.Helper()
	oauthRenewLocks.Lock()
	for key := range oauthRenewLocks.entries {
		delete(oauthRenewLocks.entries, key)
	}
	oauthRenewLocks.Unlock()
	t.Cleanup(func() {
		oauthRenewLocks.Lock()
		for key := range oauthRenewLocks.entries {
			delete(oauthRenewLocks.entries, key)
		}
		oauthRenewLocks.Unlock()
	})
}
