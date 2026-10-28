package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// routeOwnershipTestServer builds a console with one granted provider, one
// provider of u1's own, and one provider u1 has no business seeing, each behind
// a route. Another user (u2) owns the third provider's route and the
// administrator owns a route of its own, so "route of a provider I may not use"
// has both a peer and an admin as its victim.
func routeOwnershipTestServer() *Server {
	router := NewRouter(domain.GatewayState{
		Providers: []domain.Provider{
			{ID: "granted", Name: "Granted", Protocol: domain.ProtocolClaude, BaseURL: "https://a.example/v1"},
			{ID: "mine", Name: "Mine", Protocol: domain.ProtocolClaude, OwnerUserID: "u1", BaseURL: "https://b.example/v1"},
			{ID: "theirs", Name: "Theirs", Protocol: domain.ProtocolClaude, OwnerUserID: "u2", BaseURL: "https://c.example/v1"},
			{ID: "admin-owned", Name: "Admin", Protocol: domain.ProtocolClaude, BaseURL: "https://d.example/v1"},
			{ID: "paused", Name: "Paused", Protocol: domain.ProtocolClaude, BaseURL: "https://e.example/v1", Disabled: true},
		},
		Routes: []domain.Route{
			{ID: "route-granted", Name: "Granted", ProviderID: "granted", OutputProtocol: domain.ProtocolClaude},
			{ID: "route-mine", Name: "Mine", ProviderID: "mine", OutputProtocol: domain.ProtocolClaude},
			{ID: "route-theirs", Name: "Theirs", ProviderID: "theirs", OutputProtocol: domain.ProtocolClaude},
			{ID: "route-admin", Name: "Admin", ProviderID: "admin-owned", OutputProtocol: domain.ProtocolClaude},
			{ID: "route-paused", Name: "Paused", ProviderID: "paused", OutputProtocol: domain.ProtocolClaude},
		},
	})
	server := NewServer(router, monitor.NewStore())
	server.userStore = newMemoryUserStore(
		grantedUser(domain.User{ID: "u1", Username: "u1", Role: domain.UserRoleUser, Enabled: true}, []string{"granted"}),
		domain.User{ID: "u2", Username: "u2", Role: domain.UserRoleUser, Enabled: true},
		domain.User{ID: "boss", Username: "boss", Role: domain.UserRoleAdmin, Enabled: true},
	)
	return server
}

// 路由删除/修改/测试三个 handler 早先只靠中间件，而中间件对普通用户放行的是
// 路径形状，不是归属：任何 role=user 都能 DELETE 别人的路由，连带把那条路由上
// 别人的 key 弄成悬空。这里把「只能用自己够得着的 Provider 的路由」钉死，
// 也钉住中间件不再把每一个 /__routes/* 都放进来。
func TestRouteHandlersRefuseRoutesOfUngrantedProviders(t *testing.T) {
	cases := []struct {
		name     string
		role     domain.UserRole
		method   string
		routeID  string
		body     string
		wantCode int
	}{
		{name: "user deletes a peer's route", role: domain.UserRoleUser, method: http.MethodDelete, routeID: "route-theirs", wantCode: http.StatusForbidden},
		{name: "user deletes the admin's route", role: domain.UserRoleUser, method: http.MethodDelete, routeID: "route-admin", wantCode: http.StatusForbidden},
		{name: "user deletes the route of a provider the admin paused", role: domain.UserRoleUser, method: http.MethodDelete, routeID: "route-paused", wantCode: http.StatusForbidden},
		{name: "user edits a peer's route", role: domain.UserRoleUser, method: http.MethodPatch, routeID: "route-theirs", body: `{"name":"taken"}`, wantCode: http.StatusForbidden},
		{name: "user tests a peer's route", role: domain.UserRoleUser, method: http.MethodPost, routeID: "route-theirs", body: `{}`, wantCode: http.StatusForbidden},
		{name: "user deletes the route of a provider granted to them", role: domain.UserRoleUser, method: http.MethodDelete, routeID: "route-granted", wantCode: http.StatusOK},
		{name: "user deletes the route of a provider they created", role: domain.UserRoleUser, method: http.MethodDelete, routeID: "route-mine", wantCode: http.StatusOK},
		{name: "admin deletes any route", role: domain.UserRoleAdmin, method: http.MethodDelete, routeID: "route-theirs", wantCode: http.StatusOK},
		{name: "user deleting a route that does not exist still learns it is missing", role: domain.UserRoleUser, method: http.MethodDelete, routeID: "route-missing", wantCode: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := routeOwnershipTestServer()
			request := quotaRequest(tc.method, "/__routes/"+tc.routeID, tc.body, tc.routeID, "u1", tc.role)
			if tc.method == http.MethodPost {
				// The test route must not reach a provider in this test; the
				// ownership answer is written before any upstream is tried, and a
				// 403 here must never turn into a 502 from an attempted call.
				request.SetPathValue("id", tc.routeID)
			}
			recorder := httptest.NewRecorder()
			switch tc.method {
			case http.MethodDelete:
				server.handleDeleteRoute(recorder, request)
			case http.MethodPatch:
				server.handleUpdateRoute(recorder, request)
			case http.MethodPost:
				server.handleTestRoute(recorder, request)
			}
			if recorder.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tc.wantCode, recorder.Body.String())
			}
			_, err := server.router.RouteByID(tc.routeID)
			deleted := err != nil
			if tc.wantCode == http.StatusOK && tc.method == http.MethodDelete && !deleted {
				t.Fatalf("route %s should have been deleted", tc.routeID)
			}
			if tc.wantCode == http.StatusForbidden && deleted {
				t.Fatalf("route %s was deleted by a caller who may not use its provider", tc.routeID)
			}
		})
	}
}

// 被授权路由也不能被重新指向一个调用者够不着的 Provider：否则用户把自己够得着
// 的路由改到别人的账号上，就能用别人的额度（同为 handleCreateRoute 的规则）。
func TestUserCannotRetargetRouteOntoUngrantedProvider(t *testing.T) {
	server := routeOwnershipTestServer()
	request := quotaRequest(http.MethodPatch, "/__routes/route-granted", `{"providerId":"theirs"}`, "route-granted", "u1", domain.UserRoleUser)
	recorder := httptest.NewRecorder()
	server.handleUpdateRoute(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", recorder.Code, recorder.Body.String())
	}
	route, err := server.router.RouteByID("route-granted")
	if err != nil {
		t.Fatalf("route gone: %v", err)
	}
	if route.ProviderID != "granted" {
		t.Fatalf("route was re-pointed at %q", route.ProviderID)
	}

	// The same edit onto a provider they may use is allowed.
	request = quotaRequest(http.MethodPatch, "/__routes/route-granted", `{"providerId":"mine"}`, "route-granted", "u1", domain.UserRoleUser)
	recorder = httptest.NewRecorder()
	server.handleUpdateRoute(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
}

// 中间件这一层只放行「路由动作」这一种形状，其它 /__routes/* 仍走默认拒绝；
// 真正的归属判断在 handler 里（上面两个测试）。这里防止有人把前缀放行回来。
func TestMiddlewareDoesNotOpenEveryRoutePathToUsers(t *testing.T) {
	allowed := []struct{ method, path string }{
		{http.MethodPatch, "/__routes/r1"},
		{http.MethodDelete, "/__routes/r1"},
		{http.MethodPost, "/__routes/r1/test"},
	}
	for _, tc := range allowed {
		if !isUserAllowedPath(tc.method, tc.path) {
			t.Fatalf("%s %s should reach its handler (which then checks ownership)", tc.method, tc.path)
		}
	}
	refused := []struct{ method, path string }{
		{http.MethodDelete, "/__routes"},
		{http.MethodDelete, "/__routes/"},
		{http.MethodGet, "/__routes/r1"},
		{http.MethodPut, "/__routes/r1"},
		{http.MethodPost, "/__routes/r1/anything"},
		{http.MethodDelete, "/__routes/r1/test"},
	}
	for _, tc := range refused {
		if isUserAllowedPath(tc.method, tc.path) {
			t.Fatalf("%s %s must stay refused for role=user", tc.method, tc.path)
		}
	}
}
