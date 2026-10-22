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
