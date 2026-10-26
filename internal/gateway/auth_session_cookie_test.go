package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// 登录页的「信任此安全设备（30 天内免登录）」曾经只是个装饰：勾不勾服务器都发同一枚
// 7 天持久 cookie，在共用电脑上关掉浏览器也依然登录着。现在信任与否由服务端决定
// cookie 是否持久；不信任（或旧客户端不带这个字段）时是浏览器会话 cookie。
func TestLoginCookieLifetimeFollowsTrustDevice(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		wantMaxAge  bool
		wantExpires bool
	}{
		{name: "trusted device keeps a persistent cookie", body: `{"username":"admin","password":"password123","trustDevice":true}`, wantMaxAge: true, wantExpires: true},
		{name: "untrusted device gets a browser-session cookie", body: `{"username":"admin","password":"password123","trustDevice":false}`},
		{name: "a client that never heard of the field gets the safe default", body: `{"username":"admin","password":"password123"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, store := newAuthTestServer(t)
			hash, err := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.MinCost)
			if err != nil {
				t.Fatal(err)
			}
			_ = store.SetSetting(settingAdminPassword, string(hash))
			_ = store.SetSetting(settingAdminSession, "test-secret")

			request := httptest.NewRequest(http.MethodPost, "/__auth/login", strings.NewReader(tc.body))
			request.Host = "console.example.com"
			request.RemoteAddr = "203.0.113.11:443"
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("login status = %d (%s)", recorder.Code, recorder.Body.String())
			}
			cookie := sessionCookieFrom(t, recorder.Result().Cookies())
			if tc.wantMaxAge != (cookie.MaxAge > 0) {
				t.Fatalf("Max-Age = %d, want persistent=%v", cookie.MaxAge, tc.wantMaxAge)
			}
			if tc.wantExpires != !cookie.Expires.IsZero() {
				t.Fatalf("Expires = %v, want expires=%v", cookie.Expires, tc.wantExpires)
			}
			if cookie.Value == "" {
				t.Fatal("the login issued no session token")
			}
		})
	}
}

func sessionCookieFrom(t *testing.T, cookies []*http.Cookie) *http.Cookie {
	t.Helper()
	for _, cookie := range cookies {
		if cookie.Name == adminSessionCookieName {
			return cookie
		}
	}
	t.Fatalf("no %s cookie in the login response", adminSessionCookieName)
	return nil
}
