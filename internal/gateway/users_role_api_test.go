package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"golang.org/x/crypto/bcrypt"
)

// 角色本身早有完整的权限分支，缺的是 /__users 的写入路径。
// 这里把「能授予 / 能撤销 / 不能乱填 / 普通用户管不了」四件事钉住。
func TestUserRoles(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		body       string
		callerRole domain.UserRole
		wantStatus int
		wantRole   domain.UserRole
	}{
		{
			name: "create grants admin", method: http.MethodPost,
			body:       `{"username":"ops","password":"password123","role":"admin"}`,
			callerRole: domain.UserRoleAdmin, wantStatus: http.StatusCreated, wantRole: domain.UserRoleAdmin,
		},
		{
			name: "create without role stays normal", method: http.MethodPost,
			body:       `{"username":"legacy-client","password":"password123"}`,
			callerRole: domain.UserRoleAdmin, wantStatus: http.StatusCreated, wantRole: domain.UserRoleUser,
		},
		{
			name: "create rejects unknown role", method: http.MethodPost,
			body:       `{"username":"sneaky","password":"password123","role":"superuser"}`,
			callerRole: domain.UserRoleAdmin, wantStatus: http.StatusBadRequest,
		},
		{
			name: "promote existing user", method: http.MethodPatch,
			body:       `{"role":"ADMIN"}`,
			callerRole: domain.UserRoleAdmin, wantStatus: http.StatusOK, wantRole: domain.UserRoleAdmin,
		},
		{
			name: "demote back to normal", method: http.MethodPatch,
			body:       `{"role":"user"}`,
			callerRole: domain.UserRoleAdmin, wantStatus: http.StatusOK, wantRole: domain.UserRoleUser,
		},
		{
			name: "patch rejects unknown role", method: http.MethodPatch,
			body:       `{"role":"owner"}`,
			callerRole: domain.UserRoleAdmin, wantStatus: http.StatusBadRequest,
		},
		{
			name: "normal user cannot manage roles", method: http.MethodPatch,
			body:       `{"role":"admin"}`,
			callerRole: domain.UserRoleUser, wantStatus: http.StatusForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newUsersAPITestServer()
			target, pathID := "/__users", ""
			if tc.method == http.MethodPatch {
				target, pathID = "/__users/u1", "u1"
			}
			recorder := httptest.NewRecorder()
			request := quotaRequest(tc.method, target, tc.body, pathID, "caller", tc.callerRole)
			if tc.method == http.MethodPost {
				server.handleCreateUser(recorder, request)
			} else {
				server.handleUpdateUser(recorder, request)
			}
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if tc.wantStatus != http.StatusOK && tc.wantStatus != http.StatusCreated {
				return
			}
			var updated domain.User
			if err := json.Unmarshal(recorder.Body.Bytes(), &updated); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if updated.Role != tc.wantRole {
				t.Fatalf("response role = %q, want %q", updated.Role, tc.wantRole)
			}
			stored, err := server.userStore.UserByID(updated.ID)
			if err != nil {
				t.Fatalf("stored user %q missing: %v", updated.ID, err)
			}
			if stored.Role != tc.wantRole {
				t.Fatalf("persisted role = %q, want %q", stored.Role, tc.wantRole)
			}
		})
	}
}

// 提权后的用户改密码必须只动自己那一条 hash：!isAdmin() 判分支会把他推进「重写内置
// admin 密码」的路径，既改坏了别人的凭证，也让他自己的密码改不掉。
func TestPromotedAdminChangesOwnPassword(t *testing.T) {
	server, settings := newAuthTestServer(t)
	legacyHash, err := bcrypt.GenerateFromPassword([]byte("adminpassword"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	_ = settings.SetSetting(settingAdminPassword, string(legacyHash))
	_ = settings.SetSetting(settingAdminSession, "test-secret")
	ownHash, err := bcrypt.GenerateFromPassword([]byte("ownpass123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	server.userStore = newMemoryUserStore(domain.User{
		ID: "u1", Username: "u1", Role: domain.UserRoleAdmin, Enabled: true, PasswordHash: string(ownHash),
	})

	session := publicRequest(t, server, http.MethodPost, "/__auth/login",
		`{"username":"u1","password":"ownpass123"}`, "")
	if session.Code != http.StatusOK {
		t.Fatalf("login status = %d body = %s", session.Code, session.Body.String())
	}
	var status adminAuthStatus
	if err := json.Unmarshal(session.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Role != string(domain.UserRoleAdmin) || status.UserID != "u1" {
		t.Fatalf("login returned role=%q userId=%q, want admin/u1", status.Role, status.UserID)
	}
	cookie := session.Result().Cookies()[0].Value

	changed := publicRequest(t, server, http.MethodPost, "/__auth/password",
		`{"currentPassword":"ownpass123","newPassword":"brandnewpass"}`, cookie)
	if changed.Code != http.StatusOK {
		t.Fatalf("password status = %d body = %s", changed.Code, changed.Body.String())
	}

	stored, err := server.userStore.UserByID("u1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.PasswordHash == string(ownHash) {
		t.Fatal("own password hash was not rotated")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(stored.PasswordHash), []byte("brandnewpass")); err != nil {
		t.Fatalf("new password does not verify: %v", err)
	}
	if got := settings.Setting(settingAdminPassword); got != string(legacyHash) {
		t.Fatal("legacy admin password was overwritten by a database-backed admin")
	}
}

func newUsersAPITestServer() *Server {
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	server.userStore = newMemoryUserStore(
		domain.User{ID: "u1", Username: "u1", Role: domain.UserRoleUser, Enabled: true},
	)
	return server
}

// publicRequest 走完整中间件：非 loopback 来源 + 指定会话 cookie，本地旁路会绕过
// sessionIdentity，测不出真实身份分支。
func publicRequest(t *testing.T, server *Server, method, target, body, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	request := httptest.NewRequest(method, target, reader)
	request.Host = "console.example.test"
	request.RemoteAddr = "203.0.113.10:443"
	request.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: cookie})
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}
