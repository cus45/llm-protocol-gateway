package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// 普通用户看额度不该区分「自己创建的」和「管理员授权的」：只要 Provider
// 对自己可见，额度面板就应该能读到数据；未授权的 Provider 仍然 403。
func TestProviderUsageAllowedForGrantedUsers(t *testing.T) {
	router := NewRouter(domain.GatewayState{Providers: []domain.Provider{
		{ID: "own", Name: "Own", Protocol: domain.ProtocolOpenAIChat, OwnerUserID: "u1", BaseURL: "https://own.example/v1"},
		{ID: "granted", Name: "Granted", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://granted.example/v1"},
		{ID: "other", Name: "Other", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://other.example/v1"},
	}})
	server := NewServer(router, monitor.NewStore())
	server.userStore = newMemoryUserStore(domain.User{
		ID: "u1", Username: "u1", Role: domain.UserRoleUser, Enabled: true,
		AllowedProviderIDs: []string{"granted"},
	})

	handlers := map[string]http.HandlerFunc{
		"qoder-oauth":     server.handleQoderUsage,
		"workbuddy-oauth": server.handleWorkBuddyUsage,
		"zcode-oauth":     server.handleZCodeUsage,
		"trae-oauth":      server.handleTraeUsage,
	}

	for family, handler := range handlers {
		for _, providerID := range []string{"own", "granted"} {
			rec := httptest.NewRecorder()
			handler(rec, quotaRequest(http.MethodGet, "/__providers/"+providerID+"/"+family+"/usage", "", providerID, "u1", domain.UserRoleUser))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s: visible provider quota got %d %s", family, providerID, rec.Code, rec.Body.String())
			}
		}
		rec := httptest.NewRecorder()
		handler(rec, quotaRequest(http.MethodGet, "/__providers/other/"+family+"/usage", "", "other", "u1", domain.UserRoleUser))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s other: un-granted provider got %d, want 403", family, rec.Code)
		}
	}
}
