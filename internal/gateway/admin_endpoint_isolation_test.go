package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// 控制台管理端点的访问控制有一层默认拒绝：不在用户白名单里的 /__* 路径对
// role=user 一律 403（admin_auth.go 的 withAdminAuth）。这个测试把那层防线
// 变成可执行契约——将来谁想把某个管理端点加进白名单，必须先让这里继续通过，
// 也就必须同时补 handler 级的数据隔离。
func TestNormalUserIsRefusedOnAdminEndpoints(t *testing.T) {
	adminRoutes := []struct{ method, path string }{
		{http.MethodGet, "/__app/logs"},
		{http.MethodGet, "/__app/version"},
		{http.MethodGet, "/__settings/paths"},
		{http.MethodGet, "/__providers/export"},
		{http.MethodPost, "/__providers/import"},
		{http.MethodGet, "/__providers/deleted"},
		{http.MethodGet, "/__prices"},
		{http.MethodGet, "/__users"},
		{http.MethodGet, "/__monitor/overview"},
		{http.MethodGet, "/__monitor/alerts"},
		{http.MethodGet, "/__monitor/channels"},
		{http.MethodGet, "/__monitor/events"},
		{http.MethodGet, "/__monitor/accounts"},
		{http.MethodGet, "/__public"},
		{http.MethodGet, "/__public/control-token"},
		{http.MethodGet, "/__host-metrics"},
		{http.MethodGet, "/__rtk/stats"},
		{http.MethodPatch, "/__app/log-level"},
		{http.MethodPatch, "/__settings/web-exposed"},
		{http.MethodPatch, "/__settings/usage-batch"},
		{http.MethodPatch, "/__settings/request-log-retention"},
		{http.MethodPatch, "/__public-access"},
		{http.MethodPost, "/__monitor/accounts/x/test"},
		{http.MethodDelete, "/__monitor/channels/x"},
		{http.MethodPost, "/__users"},
		{http.MethodDelete, "/__users/u1"},
	}
	for _, route := range adminRoutes {
		if isUserAllowedPath(route.method, route.path) {
			t.Fatalf("%s %s is reachable by role=user; if that is intended the handler needs its own data isolation and this list must change deliberately", route.method, route.path)
		}
	}
}

// 反过来：用户白名单里的端点不能被误伤（每个 handler 自己按身份收窄数据）。
func TestNormalUserReachesOwnDataEndpoints(t *testing.T) {
	allowed := []struct{ method, path string }{
		{http.MethodGet, "/__state"},
		{http.MethodGet, "/__logs"},
		{http.MethodGet, "/__request-stats"},
		{http.MethodGet, "/__usage-analysis"},
		{http.MethodGet, "/__apikeys"},
		{http.MethodPost, "/__apikeys"},
		{http.MethodPatch, "/__apikeys/k1"},
		{http.MethodPost, "/__providers"},
		{http.MethodPost, "/__routes"},
	}
	for _, route := range allowed {
		if !isUserAllowedPath(route.method, route.path) {
			t.Fatalf("%s %s should stay reachable for role=user", route.method, route.path)
		}
	}
}

// 自检页已从控制台移除（监控中心阶段 3，任务 7）：页面入口、本机偏好与整批
// /__selfcheck* 接口都不该再回来。CLI 深检走 /__monitor/probes/run，不经这些
// 路由；Provider 级连通性测试走 /__providers/{id}/self-check/*（由
// TestProviderSelfCheckRoutesStay 守住），也不受影响。
type selfCheckAPIRoute struct{ method, path string }

var retiredSelfCheckAPIRoutes = []selfCheckAPIRoute{
	{http.MethodGet, "/__selfcheck/tools"},
	{http.MethodPost, "/__selfcheck"},
	{http.MethodGet, "/__selfcheck/sc-x"},
	{http.MethodPost, "/__selfcheck/sc-x/retry/c1"},
}

func TestRetiredSelfCheckPageAndAPIRoutesAreGone(t *testing.T) {
	if shouldServeUIFallback("/self-check") {
		t.Fatal("/self-check must not fall back to the SPA entry page; the self-check page was retired")
	}
	for _, route := range retiredSelfCheckAPIRoutes {
		if isUserAllowedPath(route.method, route.path) {
			t.Fatalf("%s %s is still on the user allowlist", route.method, route.path)
		}
	}

	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	// 只测「路由 + SPA 兜底」这一层：本机管理员旁路（与桌面端/本机访问一致），
	// 已不存在的 /__ 路径必须落到 mux 的 404，而不是被当成页面返回 index.html。
	// 角色白名单由上面两个测试负责。
	handler := withHostSeparatedServing(server.Handler(), "", nil)
	for _, route := range retiredSelfCheckAPIRoutes {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(route.method, route.path, nil)
		req.Host = "127.0.0.1:18779"
		req.RemoteAddr = "127.0.0.1:50000"
		handler.ServeHTTP(rec, req)
		body := rec.Body.String()
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404", route.method, route.path, rec.Code)
		}
		if strings.Contains(body, "<html") || strings.Contains(body, "<div id=") {
			t.Fatalf("%s %s returned the SPA entry page (%d), want a 404", route.method, route.path, rec.Code)
		}
	}
}

// 自检页入口删除时，保留的 Provider 级连通性测试不能被误删。
func TestProviderSelfCheckRoutesStay(t *testing.T) {
	for _, path := range []string{
		"/__providers/p1/self-check/health",
		"/__providers/p1/self-check/chat",
		"/__providers/p1/self-check/conformance",
	} {
		if !isSelfCheckPath(http.MethodPost, path) {
			t.Fatalf("%s must stay a provider self-check path", path)
		}
	}
}
