package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// 请求的角色（API / UI）只看 Host，所以绑定的域名必须是干净的裸主机名；
// 带路径、端口、通配符会让判定含糊，两个角色同名则根本分不开。
func TestPublicHostValidation(t *testing.T) {
	ok := [][2]string{
		{"", ""},
		{"api.example.com", "ui.example.com"},
		{"API.Example.com", "ui.example.com"}, // 大小写归一后不同即可
		{"a-b.example.co.uk", ""},
	}
	for _, pair := range ok {
		if msg := PublicHostError(pair[0], pair[1]); msg != "" {
			t.Fatalf("PublicHostError(%q,%q) = %q, want accepted", pair[0], pair[1], msg)
		}
	}
	bad := [][2]string{
		{"api.example.com/ui", ""},             // 带路径
		{"api.example.com:443", ""},            // 带端口
		{"*.example.com", ""},                  // 通配符
		{"localhost", ""},                      // 单标签
		{"api.example.com", "api.example.com"}, // 两个角色同名
		{"api..example.com", ""},               // 空标签
		{"-bad.example.com", ""},               // 标签首字符是连字符
	}
	for _, pair := range bad {
		if msg := PublicHostError(pair[0], pair[1]); msg == "" {
			t.Fatalf("PublicHostError(%q,%q) accepted an invalid pair", pair[0], pair[1])
		}
	}
}

// 处理器必须当场拒绝，而不是存下一个分不清角色的域名。
func TestPublicAccessRejectsInvalidHostnames(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	request := httptest.NewRequest(http.MethodPatch, "/__public-access",
		strings.NewReader(`{"customDomain":"api.example.com/ui","uiDomain":"ui.example.com","exposeApi":true,"exposeUi":true}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, adminIdentity()))
	recorder := httptest.NewRecorder()
	server.handleUpdatePublicAccess(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", recorder.Code, recorder.Body.String())
	}
	if got := server.router.State().PublicAccess.CustomDomain; got != "" {
		t.Fatalf("an invalid hostname was stored: %q", got)
	}
}
