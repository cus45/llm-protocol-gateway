package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func throttleRequest(remoteAddr, host string, headers map[string]string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/__auth/login", strings.NewReader(`{"username":"admin","password":"x"}`))
	request.Host = host
	request.RemoteAddr = remoteAddr
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	return request
}

// 限流键必须是网关自己观测到的东西。直连的公网对端写在请求头里的转发地址不参与：
// 否则攻击者每次换一个 X-Forwarded-For 就是一个新桶，8 次失败的上限永远触发不了。
func TestAuthThrottleIgnoresForwardedHeadersFromPublicPeers(t *testing.T) {
	keys := map[string]bool{}
	for _, spoofed := range []string{"198.51.100.1", "198.51.100.2", "203.0.113.9", "10.1.2.3"} {
		request := throttleRequest("192.0.2.44:5555", "console.example.com", map[string]string{
			"X-Forwarded-For":  spoofed,
			"CF-Connecting-IP": spoofed,
			"X-Real-IP":        spoofed,
		})
		keys[authThrottleSource(request)] = true
	}
	if len(keys) != 1 {
		t.Fatalf("a public peer chose its own throttle key: %v", keys)
	}
	if !keys["192.0.2.44"] {
		t.Fatalf("the throttle key should be the socket peer, got %v", keys)
	}
}

// 受信任代理（cloudflared / 本机反代）后面，取转发链里最早的公网地址：它由最外层
// 反代写入，后面的环节改不了。
func TestAuthThrottleUsesTheOutermostForwardedAddressBehindAProxy(t *testing.T) {
	request := throttleRequest("127.0.0.1:4444", "console.example.com", map[string]string{
		"X-Forwarded-For": "198.51.100.7, 10.0.0.2",
	})
	if got := authThrottleSource(request); got != "198.51.100.7" {
		t.Fatalf("authThrottleSource = %q, want the client the proxy named", got)
	}
}

// 本机旁路成立时，内网对端只能拿到「内网」这一个来源：否则它报一个和运维本机相同的
// 转发地址，就能把运维锁在登录页外面（运维自己的请求也会被算进同一个桶）。
func TestAuthThrottleCannotPinALockoutOnTheOperatorFromInside(t *testing.T) {
	operator := throttleRequest("127.0.0.1:1234", "127.0.0.1:18093", nil)
	for _, spoofed := range []string{"127.0.0.1", "::1"} {
		attacker := throttleRequest("10.0.0.9:4321", "127.0.0.1:18093", map[string]string{"X-Forwarded-For": spoofed})
		if got, want := authThrottleSource(attacker), authThrottleSource(operator); got != want {
			t.Fatalf("an internal peer reporting %q chose %q while the loopback operator has %q: it can lock the operator out", spoofed, got, want)
		}
	}
	// 内网对端 + 公网 Host（不是本机旁路）：各自按转发地址记，互不牵连。
	a := throttleRequest("10.0.0.9:4321", "console.example.com", map[string]string{"X-Forwarded-For": "198.51.100.7"})
	b := throttleRequest("10.0.0.10:4321", "console.example.com", map[string]string{"X-Forwarded-For": "198.51.100.8"})
	if authThrottleSource(a) == authThrottleSource(b) {
		t.Fatal("two different forwarded clients behind the proxy must not share a bucket")
	}
}

// 账号维度与来源无关：换来源绕不过账号，换账号绕不过来源。
func TestAuthThrottleBlocksAcrossSourcesAndAcrossAccounts(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	server.SetAdminAuthStore(newMemoryAdminAuthStore())

	fail := func(remoteAddr, username string) {
		request := throttleRequest(remoteAddr, "console.example.com", nil)
		request.Body = http.NoBody
		server.recordAuthFailure(request, username)
	}
	blocked := func(remoteAddr, username string) bool {
		request := throttleRequest(remoteAddr, "console.example.com", nil)
		blocked, _ := server.blocked(request, username)
		return blocked
	}

	// 同一个账号，authAccountMaxFailures 个不同来源：账号维度到阈值，之后换一个
	// 从没试过的来源也被挡（单台机器靠来源维度本来就会被挡，这条测的是跨来源）。
	for i := 0; i < authAccountMaxFailures; i++ {
		fail(fmt.Sprintf("192.0.2.%d:1000", i+1), "admin")
	}
	if !blocked("198.51.100.200:1000", "admin") {
		t.Fatal("an account with many failures was not blocked from a new source")
	}

	// 同一个来源，8 个不同账号：来源维度到阈值，第 9 个账号也被挡。
	server2 := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	server2.SetAdminAuthStore(newMemoryAdminAuthStore())
	for i := 0; i < authMaxFailures; i++ {
		request := throttleRequest("192.0.2.77:1000", "console.example.com", nil)
		request.Body = http.NoBody
		server2.recordAuthFailure(request, fmt.Sprintf("user%d", i))
	}
	request := throttleRequest("192.0.2.77:1000", "console.example.com", nil)
	// 来源维度从不依赖用户名：同一来源的失败累计到 authMaxFailures 就挡。
	for i := 0; i < authMaxFailures-1; i++ {
		request.Body = http.NoBody
		server2.recordAuthFailure(request, fmt.Sprintf("user%d", i))
	}
	if blocked, _ := server2.blocked(request, "another-name"); !blocked {
		t.Fatal("a source with 8 failures was not blocked for a new account name")
	}

	// 一次成功登录清空该账号的两个桶。
	success := throttleRequest("198.51.100.201:1000", "console.example.com", nil)
	server.recordAuthSuccess(success, "admin")
	if blocked("198.51.100.202:1000", "admin") {
		t.Fatal("a successful login must clear the account bucket")
	}
}
