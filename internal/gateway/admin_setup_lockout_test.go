package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// /__auth/setup 只在"全新实例"上开放：没有任何管理员口令、也没有任何用户
// 账号、控制台里还没有任何配置。只要实例已经在用（有账号或有配置），公网上
// 谁先到都不能把自己设成管理员；这类实例必须由本机补设口令。
func TestRemoteSetupRefusedOnInstanceInUse(t *testing.T) {
	cases := []struct {
		name  string
		users []domain.User
		state domain.GatewayState
	}{
		{
			name:  "has console users",
			users: []domain.User{{ID: "u1", Username: "luca", Role: domain.UserRoleAdmin, Enabled: true}},
		},
		{
			name:  "has configured providers",
			state: domain.GatewayState{Providers: []domain.Provider{{ID: "p1", Name: "P1", Protocol: domain.ProtocolOpenAIChat}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := NewServer(NewRouter(tc.state), monitor.NewStore())
			store := newMemoryAdminAuthStore()
			server.SetAdminAuthStore(store)
			server.userStore = newMemoryUserStore(tc.users...)
			handler := server.Handler()

			body, _ := json.Marshal(map[string]string{"password": "attacker-pass-1"})
			req := httptest.NewRequest(http.MethodPost, "/__auth/setup", bytes.NewReader(body))
			req.Host = "console.lucadesign.uk"
			req.RemoteAddr = "203.0.113.50:443"
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("remote setup on an in-use instance = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
			}
			if store.Setting(settingAdminPassword) != "" {
				t.Fatal("remote caller changed the admin password of an in-use instance")
			}
			if len(rec.Result().Cookies()) != 0 {
				t.Fatal("refused setup still issued a session cookie")
			}
		})
	}
}

// 本机（桌面端）在不在此列：本机就是管理员，随时可以补设口令。
func TestLoopbackSetupAllowedOnInstanceInUse(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	store := newMemoryAdminAuthStore()
	server.SetAdminAuthStore(store)
	server.userStore = newMemoryUserStore(domain.User{ID: "u1", Username: "luca", Role: domain.UserRoleAdmin, Enabled: true})
	handler := server.Handler()

	body, _ := json.Marshal(map[string]string{"password": "desktop-pass-1"})
	req := httptest.NewRequest(http.MethodPost, "/__auth/setup", bytes.NewReader(body))
	req.Host = "127.0.0.1:18093"
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("loopback setup = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if store.Setting(settingAdminPassword) == "" {
		t.Fatal("loopback setup did not store a password")
	}
}

// 全新实例（无口令、无账号、无配置）仍然允许公网首设，否则远程部署没法初始化。
func TestRemoteSetupAllowedOnFreshInstance(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	store := newMemoryAdminAuthStore()
	server.SetAdminAuthStore(store)
	server.userStore = newMemoryUserStore()
	handler := server.Handler()

	body, _ := json.Marshal(map[string]string{"password": "owner-pass-1"})
	req := httptest.NewRequest(http.MethodPost, "/__auth/setup", bytes.NewReader(body))
	req.Host = "console.lucadesign.uk"
	req.RemoteAddr = "203.0.113.60:443"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh remote setup = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}
