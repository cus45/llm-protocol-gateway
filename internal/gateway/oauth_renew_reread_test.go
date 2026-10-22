package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// 锁里拿到的凭证如果已经是别人续好的（refresh token 换了人），即便新令牌因为
// 有效期设得很短依旧判为"该续期"，也不能再花手上这枚已经作废的旧令牌——否
// 则上游回 401，整条令牌链被判无效。
//
// 复现方式：先把路由里的凭证换成"别人续好的"（旧 refresh 已被花掉、新令牌
// expiresAt 已过），再让一个还拿着旧凭证的请求走续期；续期端点只接受新令牌。
func cursorRefreshRound(t *testing.T, wantRefresh string) *int32 {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != wantRefresh {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid refresh token"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"good-access","refreshToken":"good-refresh"}`))
	}))
	t.Cleanup(server.Close)
	previous := cursorOAuthRefreshURLOverride
	cursorOAuthRefreshURLOverride = server.URL
	t.Cleanup(func() { cursorOAuthRefreshURLOverride = previous })
	return &calls
}

func TestCursorRefreshUsesCredentialRereadUnderLock(t *testing.T) {
	calls := cursorRefreshRound(t, "already-rotated-refresh")
	server, router := newRefreshLockServer(t)
	added, err := router.AddProvider(domain.Provider{
		ID:       "cur-reread",
		Name:     "cur-reread",
		Protocol: domain.ProtocolOpenAIChat,
		AuthType: domain.AuthTypeCursorOAuth,
		CursorOAuth: &domain.CursorOAuthCredential{
			AccessToken:  "old-access",
			RefreshToken: "alice-spent-this",
			ExpiresAt:    time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("add provider: %v", err)
	}
	stale := added
	// 等锁期间另一个请求先续好了：路由里已是新令牌，只是有效期已过。
	if _, err := router.SetProviderCursorOAuth(added.ID, domain.CursorOAuthCredential{
		AccessToken:  "stale-but-newer",
		RefreshToken: "already-rotated-refresh",
		ExpiresAt:    time.Now().Add(-time.Second).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	refreshed, err := server.ensureFreshCursorToken(stale)
	if err != nil {
		t.Fatalf("refresh from stale credential: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("refresh endpoint called %d times, want 1", got)
	}
	if refreshed.CursorOAuth == nil || refreshed.CursorOAuth.RefreshToken != "good-refresh" {
		t.Fatalf("credential not refreshed from the reread token: %+v", refreshed.CursorOAuth)
	}
}

// 同一枚令牌的续期锁在续完、过期后必须被回收：旧实现只增不删，一枚令牌一把
// 锁，长期运行的网关会一直攒下去。
func TestOauthRenewLockRetiresRotatedToken(t *testing.T) {
	calls := workBuddyRefreshRound(t, 0)
	server, router := newRefreshLockServer(t)
	provider := workBuddyExpiredProvider("wb-retire")
	provider.WorkBuddy.RefreshToken = "retire-refresh-token"
	added, err := router.AddProvider(provider)
	if err != nil {
		t.Fatalf("add provider: %v", err)
	}
	before := oauthRenewLockCount()
	if _, exists := oauthRenewLocks.entries[oauthRenewLockKey(added.ID, "retire-refresh-token")]; exists {
		t.Fatal("lock created before any refresh")
	}
	if _, err := server.ensureFreshWorkBuddyToken(added); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("refresh called %d times", got)
	}
	oauthRenewLocks.Lock()
	_, stillThere := oauthRenewLocks.entries[oauthRenewLockKey(added.ID, "retire-refresh-token")]
	oauthRenewLocks.Unlock()
	if stillThere {
		t.Fatal("spent refresh token's lock was not retired")
	}
	if after := oauthRenewLockCount(); after > before {
		t.Fatalf("renew lock table grew after a successful rotation: %d -> %d", before, after)
	}
}

// 空闲够久的锁会被回收，表不会无限增长。
func TestOauthRenewLocksReclaimIdleEntries(t *testing.T) {
	entry := oauthRenewLock("idle-provider", "idle-token")
	if entry == nil {
		t.Fatal("nil lock")
	}
	oauthRenewLocks.Lock()
	if current, ok := oauthRenewLocks.entries["idle-provider\x00idle-token"]; ok {
		current.last = time.Now().Add(-2 * oauthRenewIdleAfter)
	}
	oauthRenewLocks.Unlock()
	_ = oauthRenewLock("other-provider", "other-token")
	if count := oauthRenewLockCount(); count > 2 {
		t.Fatalf("lock table not reclaimed: %d entries", count)
	}
	oauthRenewLocks.Lock()
	_, stillThere := oauthRenewLocks.entries["idle-provider\x00idle-token"]
	oauthRenewLocks.Unlock()
	if stillThere {
		t.Fatal("idle entry survived a reclaim sweep")
	}
}

// Cursor 续期不能挂死在 http.DefaultClient 上：上游不回包时要按超时返回。
func TestCursorRefreshRespectsTimeout(t *testing.T) {
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(block) })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := refreshCursorOAuthTokenContext(ctx, "tok"); err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("refresh blocked for %s", elapsed)
	}
}

// 新旧令牌不同、而新令牌仍被判为过期（续期方拿到的有效期比时钟偏差还短）：
// 这种情况下也只能用新令牌去续，不能再花已作废的旧令牌。
func TestWorkBuddyRefreshPrefersRereadTokenEvenWhenItStillLooksExpired(t *testing.T) {
	var gotRefresh atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRefresh.Store(strings.TrimSpace(r.Header.Get("X-Refresh-Token")))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"fresh-access","refreshToken":"rotated-3","expiresIn":3600,"tokenType":"Bearer"}}`))
	}))
	t.Cleanup(server.Close)
	previous := workBuddyCNEndpoint
	workBuddyCNEndpoint = server.URL
	t.Cleanup(func() { workBuddyCNEndpoint = previous })

	_, router := newRefreshLockServer(t)
	gw := NewServer(router, monitor.NewStore())
	added, err := router.AddProvider(workBuddyExpiredProvider("wb-reread"))
	if err != nil {
		t.Fatalf("add provider: %v", err)
	}
	stale := added
	if _, err := router.SetProviderWorkBuddy(added.ID, domain.AuthTypeWorkBuddy, domain.WorkBuddyCredential{
		AccessToken:  "someone-else-access",
		RefreshToken: "rotated-2",
		ExpiresAt:    time.Now().Add(-time.Second).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := gw.ensureFreshWorkBuddyToken(stale); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := fmt.Sprint(gotRefresh.Load()); got != "rotated-2" {
		t.Fatalf("refresh used token %q, want the rotated one", got)
	}
}
