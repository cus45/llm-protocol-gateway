package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// "admin" 这个用户 ID 是内置管理员：会话里看到它就直接当管理员。若普通用户
// 能注册出这个 ID（或用户名），他登录后就拿到了管理员身份——越权。
// 两道防线都必须真的挡住：用户 ID 生成时排除，用户名保留字拒绝。
func TestUsersCannotClaimTheAdminIdentity(t *testing.T) {
	for _, username := range []string{"admin", "Admin", " ADMIN "} {
		server := newUsersAPITestServer()
		recorder := httptest.NewRecorder()
		request := quotaRequest(http.MethodPost, "/__users",
			`{"username":"`+strings.TrimSpace(username)+`","password":"a-long-enough-password","role":"user"}`, "", "caller", domain.UserRoleAdmin)
		server.handleCreateUser(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("creating a user named %q = %d, want 400 (%s)", username, recorder.Code, recorder.Body.String())
		}
	}
}

// 即使用户名不叫 admin，生成的稳定 ID 也不能是 admin（slug 之后可能撞上）。
func TestUserIDsNeverCollideWithTheAdminID(t *testing.T) {
	server := newUsersAPITestServer()
	for _, username := range []string{"admin-2", "a-dmin", "administrator"} {
		recorder := httptest.NewRecorder()
		request := quotaRequest(http.MethodPost, "/__users",
			`{"username":"`+username+`","password":"a-long-enough-password","role":"user"}`, "", "caller", domain.UserRoleAdmin)
		server.handleCreateUser(recorder, request)
		if recorder.Code != http.StatusCreated {
			continue // 该名字被拒也无妨，只要没创建出 ID=admin 的用户
		}
		var created domain.User
		if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if created.ID == legacyAdminUserID {
			t.Fatalf("user %q got the built-in admin id", username)
		}
	}
	// 直连来源：任何已存在用户的 ID 都不是 legacyAdminUserID。
	if users, err := server.userStore.ListUsers(); err == nil {
		for _, user := range users {
			if user.ID == legacyAdminUserID {
				t.Fatalf("a stored user carries the built-in admin id")
			}
		}
	}
}
