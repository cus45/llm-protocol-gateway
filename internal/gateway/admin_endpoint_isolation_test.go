package gateway

import (
	"net/http"
	"testing"
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
		{http.MethodGet, "/__host-metrics/snapshot"},
		{http.MethodGet, "/__host-metrics/stream"},
		{http.MethodGet, "/__host-diagnostics"},
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

// TestProviderSelfCheckRoutesStay 守住三条 Provider 级自检路由（Provider 编辑弹窗的
// 连通性测试）。它们与被撤下的控制台自检页无关，是独立实现（provider_self_register.go），
// 清理 selfcheck.go 时最容易连坐删错的地方，所以单独钉住。
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
