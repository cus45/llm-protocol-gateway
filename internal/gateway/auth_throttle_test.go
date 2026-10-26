package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// 同一来源对同一账号连续失败到阈值后，后面的尝试在比对口令前就被挡下。
func TestAuthThrottleCountsFailuresWindow(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	throttle := newAuthThrottle()
	throttle.now = func() time.Time { return now }

	for i := 0; i < authMaxFailures; i++ {
		if blocked, _ := throttle.blocked("1.2.3.4", "admin", authMaxFailures); blocked {
			t.Fatalf("blocked after %d failures", i)
		}
		throttle.fail("1.2.3.4", "admin")
	}
	if blocked, wait := throttle.blocked("1.2.3.4", "admin", authMaxFailures); !blocked || wait <= 0 {
		t.Fatalf("expected to be blocked, got blocked=%v wait=%v", blocked, wait)
	}
	// 另一个来源不受影响：失手的是那个 IP，不是账号本身。
	if blocked, _ := throttle.blocked("5.6.7.8", "admin", authMaxFailures); blocked {
		t.Fatalf("a different source must not be blocked")
	}
	// 窗口过期后自动放行，且计数从头开始。
	now = now.Add(authFailureWindow)
	if blocked, _ := throttle.blocked("1.2.3.4", "admin", authMaxFailures); blocked {
		t.Fatalf("window expiry must lift the block")
	}
	if blocked := throttle.fail("1.2.3.4", "admin"); blocked {
		t.Fatalf("a fresh window must not block on its first failure")
	}
}

// 成功登录清空计数：偶尔输错几次不影响正常登录。
func TestAuthThrottleSuccessResets(t *testing.T) {
	throttle := newAuthThrottle()
	for i := 0; i < authMaxFailures-1; i++ {
		throttle.fail("1.2.3.4", "admin")
	}
	throttle.succeeded("1.2.3.4", "admin")
	for i := 0; i < authMaxFailures-1; i++ {
		if blocked := throttle.fail("1.2.3.4", "admin"); blocked {
			t.Fatalf("blocked after reset + %d failures", i)
		}
	}
	if blocked, _ := throttle.blocked("1.2.3.4", "admin", authMaxFailures); blocked {
		t.Fatalf("must still be below the threshold")
	}
}

// 老条目不清理会随用户名无限增长；够多时顺手清扫过期条目。
func TestAuthThrottleSweepsExpired(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	throttle := newAuthThrottle()
	throttle.now = func() time.Time { return now }
	for i := 0; i < authThrottleSweepAt; i++ {
		throttle.fail("1.2.3.4", "user-"+strconv.Itoa(i))
	}
	now = now.Add(authFailureWindow + time.Minute)
	throttle.fail("9.9.9.9", "admin")
	if len(throttle.entries) > 2 {
		t.Fatalf("expired entries were not swept: %d", len(throttle.entries))
	}
}

// 走真实处理器：阈值内的错误口令回 401，之后一律 429 且带 Retry-After。
func TestAuthLoginThrottledAfterFailures(t *testing.T) {
	server, store := newAuthTestServer(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetSetting(settingAdminPassword, string(hash))
	_ = store.SetSetting(settingAdminSession, "test-secret")
	handler := server.Handler()

	attempt := func(password string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"username": "admin", "password": password})
		req := httptest.NewRequest(http.MethodPost, "/__auth/login", bytes.NewReader(body))
		req.Host = "console.lucadesign.uk"
		req.RemoteAddr = "203.0.113.10:443"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	for i := 0; i < authMaxFailures-1; i++ {
		if rec := attempt("wrong-password"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d body = %s", i, rec.Code, rec.Body.String())
		}
	}
	if rec := attempt("wrong-password"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("last allowed failure: status = %d", rec.Code)
	}
	// 即使口令正确，限流期间也不放行（否则限流形同虚设）。
	rec := attempt("password123")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled attempt: status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("429 must carry Retry-After")
	}
}

// 被限流的来源在解析正文之前就被挡下：即使发来的是畸形 JSON，也只会拿到 429，
// 不会被 Decode 的 400 抢先 —— 证明 rejectIfAuthThrottled 跑在 Decode 之前。
func TestAuthLoginThrottledBeforeDecode(t *testing.T) {
	server, store := newAuthTestServer(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetSetting(settingAdminPassword, string(hash))
	_ = store.SetSetting(settingAdminSession, "test-secret")
	for i := 0; i < authMaxFailures; i++ {
		server.authThrottle.fail("203.0.113.11", "admin")
	}
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodPost, "/__auth/login", strings.NewReader(`{"username":"admin","password":`))
	req.Host = "console.lucadesign.uk"
	req.RemoteAddr = "203.0.113.11:443"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled malformed body: status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("429 must carry Retry-After")
	}
}

// 预读账号名后正文必须原样还给 Decode：正常登录照旧成功。
func TestAuthLoginPeekLeavesBodyIntact(t *testing.T) {
	server, store := newAuthTestServer(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetSetting(settingAdminPassword, string(hash))
	_ = store.SetSetting(settingAdminSession, "test-secret")
	handler := server.Handler()

	body, _ := json.Marshal(map[string]string{"password": "password123", "username": "admin"})
	req := httptest.NewRequest(http.MethodPost, "/__auth/login", bytes.NewReader(body))
	req.Host = "console.lucadesign.uk"
	req.RemoteAddr = "203.0.113.12:443"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// peekUsername 只认第一个 username 字符串；取不到时返回空串（按 admin 桶计数）。
func TestPeekUsername(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"username":"alice","password":"x"}`, "alice"},
		{`{"password":"x","username":" bob "}`, "bob"},
		{`{"password":"x"}`, ""},
		{`{"username":42}`, ""},
		{`not json`, ""},
		{``, ""},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/__auth/login", strings.NewReader(c.body))
		if got := peekUsername(req); got != c.want {
			t.Fatalf("peekUsername(%q) = %q, want %q", c.body, got, c.want)
		}
		// 探过之后正文必须完整可读。
		rest, _ := io.ReadAll(req.Body)
		if string(rest) != c.body {
			t.Fatalf("peekUsername(%q) consumed body, left %q", c.body, rest)
		}
	}
}
