package gateway

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// The RBAC work is only correct if the whole stack agrees: the route table maps
// every console call to a permission, the middleware enforces it for a real
// non-loopback session, resource scopes decide which rows a principal sees, and
// owner attribution labels new rows with the creator's account instead of "".
// These tests drive the real Handler() through all four layers.

const rbacTestSecret = "rbac-test-secret"

// memoryRoleStore mirrors store.roles.go: built-ins live in code and are never
// written back, custom roles are keyed by id.
type memoryRoleStore struct {
	roles  map[string]domain.Role
	grants map[string][]string
}

func newMemoryRoleStore() *memoryRoleStore {
	return &memoryRoleStore{roles: map[string]domain.Role{}, grants: map[string][]string{}}
}

func (m *memoryRoleStore) ListRoles() ([]domain.Role, error) {
	out := make([]domain.Role, 0, len(m.roles))
	for _, role := range m.roles {
		out = append(out, role)
	}
	return out, nil
}

func (m *memoryRoleStore) RoleByID(id string) (domain.Role, error) {
	role, ok := m.roles[id]
	if !ok {
		return domain.Role{}, sql.ErrNoRows
	}
	return role, nil
}

func (m *memoryRoleStore) SaveRole(role domain.Role) error {
	m.roles[role.ID] = role
	return nil
}

func (m *memoryRoleStore) DeleteRole(id string) error {
	delete(m.roles, id)
	return nil
}

// Grants are owned by the user store in tests; the production sqlite store
// implements both interfaces.
func (m *memoryRoleStore) ListResourceGrants() (map[string][]string, error) {
	out := make(map[string][]string, len(m.grants))
	for userID, ids := range m.grants {
		out[userID] = append([]string{}, ids...)
	}
	return out, nil
}

func (m *memoryRoleStore) SetResourceGrants(userID string, resourceIDs []string) error {
	m.grants[userID] = append([]string{}, resourceIDs...)
	return nil
}

type rbacFixture struct {
	server *Server
	users  *memoryUserStore
	roles  *memoryRoleStore
	state  *domain.GatewayState
}

func newRBACFixture(t *testing.T) *rbacFixture {
	t.Helper()
	state := domain.GatewayState{Providers: []domain.Provider{
		{ID: "shared", Name: "Shared", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://shared.example/v1", OwnerUserID: "admin"},
		{ID: "alice-private", Name: "Alice Private", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://alice.example/v1", OwnerUserID: "alice"},
	}, APIKeys: []domain.APIKey{
		{ID: "k-admin", Name: "admin key", Enabled: true, OwnerUserID: "admin", RouteID: "route-admin"},
		{ID: "k-alice", Name: "alice key", Enabled: true, OwnerUserID: "alice", RouteID: "route-alice"},
	}}
	state.Routes = []domain.Route{
		{ID: "route-admin", Name: "admin route", ProviderID: "shared"},
		{ID: "route-alice", Name: "alice route", ProviderID: "alice-private"},
	}
	router := NewRouter(state)
	server := NewServer(router, monitor.NewStore())
	auth := newMemoryAdminAuthStore()
	_ = auth.SetSetting(settingAdminPassword, "x")
	_ = auth.SetSetting(settingAdminSession, rbacTestSecret)
	server.SetAdminAuthStore(auth)

	users := newMemoryUserStore(
		domain.User{ID: "admin", Username: "admin", DisplayName: "王管理", Role: domain.UserRoleAdmin, RoleID: domain.RoleIDAdmin, Enabled: true},
		domain.User{ID: "alice", Username: "alice", Role: domain.UserRoleUser, RoleID: domain.RoleIDUser, Enabled: true},
	)
	server.userStore = users
	// 生产里同一个 *store.Store 同时实现 UserStore 与 RoleStore；测试里让两个
	// 假实现共享同一张授权表，否则写进 users.grants 的授权读不出来。
	roles := newMemoryRoleStore()
	users.grants = roles.grants
	server.roles = roles
	return &rbacFixture{server: server, users: users, roles: roles, state: &router.state}
}

func (f *rbacFixture) call(t *testing.T, userID, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	token, err := mintSessionToken(rbacTestSecret, userID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	request := httptest.NewRequest(method, path, reader)
	request.Host = "console.example.test"
	request.RemoteAddr = "203.0.113.10:443"
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: adminSessionCookieName, Value: token})
	recorder := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func (f *rbacFixture) provider(t *testing.T, id string) domain.Provider {
	t.Helper()
	for _, provider := range f.state.Providers {
		if provider.ID == id {
			return provider
		}
	}
	t.Fatalf("provider %q not found", id)
	return domain.Provider{}
}

func decodeSlice[T any](t *testing.T, body []byte) []T {
	t.Helper()
	var out []T
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %T: %v (body %.200s)", out, err, body)
	}
	return out
}

// 用户的自述痛点：普通用户能建自己的 Key，却加不了 Provider。内置「user」
// 角色必须同时持有 keys.create 与 providers.create，且两边的资源范围都是 own。
func TestNormalUserCanCreateOwnProviderAndKey(t *testing.T) {
	f := newRBACFixture(t)

	created := f.call(t, "alice", http.MethodPost, "/__providers", `{"name":"Alice New","protocol":"openai_chat","baseUrl":"https://alice-new.example/v1"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create provider status=%d body=%s", created.Code, created.Body.String())
	}
	var provider domain.Provider
	if err := json.Unmarshal(created.Body.Bytes(), &provider); err != nil {
		t.Fatal(err)
	}
	if provider.OwnerUserID != "alice" {
		t.Fatalf("ownerUserID=%q, want the creator's own id", provider.OwnerUserID)
	}

	key := f.call(t, "alice", http.MethodPost, "/__apikeys", `{"name":"alice own","providerId":"`+provider.ID+`"}`)
	if key.Code != http.StatusCreated {
		t.Fatalf("create key status=%d body=%s", key.Code, key.Body.String())
	}

	// 自己新建的 Provider 立刻可用：创建 Key 时后端会按可见集合校验绑定。
	if got := f.provider(t, provider.ID); got.OwnerUserID != "alice" {
		t.Fatalf("stored owner=%q", got.OwnerUserID)
	}
}

// 痛点的原话是「自己生成的 Key 加不了 Provider」：普通用户编辑自己的 Key，
// 把自己的 Provider 加进备选链要放行，把别人的加进来要拒绝。
func TestOwnKeyAcceptsOwnProviderInFallbackChain(t *testing.T) {
	f := newRBACFixture(t)
	f.state.Routes = append(f.state.Routes, domain.Route{ID: "route-alice-2", Name: "alice second", ProviderID: "alice-private"})

	own := f.call(t, "alice", http.MethodPatch, "/__apikeys/k-alice", `{"name":"alice key","fallbackProviderIds":["alice-private"]}`)
	if own.Code != http.StatusOK {
		t.Fatalf("attach own provider status=%d body=%s", own.Code, own.Body.String())
	}

	foreign := f.call(t, "alice", http.MethodPatch, "/__apikeys/k-alice", `{"name":"alice key","fallbackProviderIds":["alice-private","shared"]}`)
	if foreign.Code != http.StatusForbidden {
		t.Fatalf("attach admin provider status=%d, want 403 body=%s", foreign.Code, foreign.Body.String())
	}

	// 别人的 Key 连编辑入口都进不去。
	other := f.call(t, "alice", http.MethodPatch, "/__apikeys/k-admin", `{"name":"hijack"}`)
	if other.Code != http.StatusForbidden {
		t.Fatalf("edit admin key status=%d, want 403", other.Code)
	}
}

// 管理员建的 Provider / Key 不能再落成「无主」，否则控制台一律显示「管理员」。
// 账号名 admin 存在时归属真实账号 id；只有隐式管理密码时才是 legacy admin。
func TestAdminCreatedRowsCarryAdminAccount(t *testing.T) {
	f := newRBACFixture(t)
	created := f.call(t, "admin", http.MethodPost, "/__providers", `{"name":"Admin Owned","protocol":"openai_chat","baseUrl":"https://admin.example/v1"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", created.Code, created.Body.String())
	}
	var provider domain.Provider
	_ = json.Unmarshal(created.Body.Bytes(), &provider)
	if provider.OwnerUserID != "admin" {
		t.Fatalf("provider owner=%q, want admin", provider.OwnerUserID)
	}

	key := f.call(t, "admin", http.MethodPost, "/__apikeys", `{"name":"admin key 2","routeId":"route-admin"}`)
	if key.Code != http.StatusCreated {
		t.Fatalf("key status=%d body=%s", key.Code, key.Body.String())
	}
	var apiKey domain.APIKey
	_ = json.Unmarshal(key.Body.Bytes(), &apiKey)
	if apiKey.OwnerUserID != "admin" {
		t.Fatalf("key owner=%q, want admin", apiKey.OwnerUserID)
	}
}

// 每个控制台路由都必须落到权限点上：普通用户拿到的是 403 而不是能顺手改配置。
func TestRoutePermissionMatrix(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{"unknown console route fails closed", http.MethodPost, "/__not-a-route", `{}`, http.StatusForbidden},
		{"user list denied", http.MethodGet, "/__users", "", http.StatusForbidden},
		{"role list denied", http.MethodGet, "/__roles", "", http.StatusForbidden},
		{"role create denied", http.MethodPost, "/__roles", `{"name":"x"}`, http.StatusForbidden},
		{"role catalog allowed", http.MethodGet, "/__roles/catalog", "", http.StatusOK},
		{"role options allowed", http.MethodGet, "/__roles/options", "", http.StatusOK},
		{"settings write denied", http.MethodPatch, "/__settings", `{}`, http.StatusForbidden},
		{"machine denied", http.MethodGet, "/__host-metrics", "", http.StatusForbidden},
		{"machine snapshot denied", http.MethodGet, "/__host-metrics/snapshot", "", http.StatusForbidden},
		{"machine diagnostics denied", http.MethodGet, "/__host-diagnostics", "", http.StatusForbidden},
		{"prices denied", http.MethodGet, "/__prices", "", http.StatusForbidden},
		{"monitor denied", http.MethodGet, "/__monitor/stream", "", http.StatusForbidden},
		{"user cannot create routes to unowned providers", http.MethodPost, "/__routes", `{"name":"x","providerId":"shared"}`, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRBACFixture(t)
			// 用一个最小的自定义角色把基线算清楚：普通用户连 /__state、/__logs 都允许，
			// 所以这里只给 keys.view，其余全部应从路由表里被拒。
			if err := f.roles.SaveRole(domain.Role{
				ID: "keys-only", Name: "仅密钥", Scope: domain.ScopeOwn,
				Permissions: []domain.Permission{domain.PermKeysView},
			}); err != nil {
				t.Fatal(err)
			}
			alice := f.users.users["alice"]
			alice.RoleID = "keys-only"
			f.users.users["alice"] = alice
			recorder := f.call(t, "alice", tc.method, tc.path, tc.body)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("%s %s status=%d want %d body=%s", tc.method, tc.path, recorder.Code, tc.wantStatus, recorder.Body.String())
			}
		})
	}
}

// 自定义角色：只给「查看 Provider + 创建密钥」，且范围是 own。
// 角色一变，会话不用重新登录就按新权限判定。
func TestCustomRoleDrivesAccess(t *testing.T) {
	f := newRBACFixture(t)
	if err := f.roles.SaveRole(domain.Role{
		ID: "key-smith", Name: "密钥工匠", Scope: domain.ScopeOwn,
		Permissions: []domain.Permission{domain.PermProvidersView, domain.PermKeysView, domain.PermKeysCreate},
	}); err != nil {
		t.Fatal(err)
	}
	alice := f.users.users["alice"]
	alice.RoleID = "key-smith"
	f.users.users["alice"] = alice

	if recorder := f.call(t, "alice", http.MethodGet, "/__apikeys", ""); recorder.Code != http.StatusOK {
		t.Fatalf("keys view status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := f.call(t, "alice", http.MethodPost, "/__providers", `{"name":"nope","protocol":"openai_chat","baseUrl":"https://x.example/v1"}`); recorder.Code != http.StatusForbidden {
		t.Fatalf("provider create should be denied, status=%d", recorder.Code)
	}
	if recorder := f.call(t, "alice", http.MethodGet, "/__users", ""); recorder.Code != http.StatusForbidden {
		t.Fatalf("users should be denied, status=%d", recorder.Code)
	}

	// 范围 own：只看得见自己的密钥。
	recorder := f.call(t, "alice", http.MethodGet, "/__apikeys", "")
	keys := decodeSlice[domain.APIKey](t, recorder.Body.Bytes())
	if len(keys) != 1 || keys[0].ID != "k-alice" {
		t.Fatalf("alice sees %d keys, want only k-alice", len(keys))
	}
}

// 范围 all 的普通角色（只读管理员）：看得见全部密钥，但写操作仍按权限点拒绝。
func TestScopeAllReadOnlyRole(t *testing.T) {
	f := newRBACFixture(t)
	if err := f.roles.SaveRole(domain.Role{
		ID: "auditor", Name: "审计员", Scope: domain.ScopeAll,
		Permissions: []domain.Permission{domain.PermDashboardView, domain.PermKeysView, domain.PermProvidersView, domain.PermLogsView, domain.PermUsageView},
	}); err != nil {
		t.Fatal(err)
	}
	alice := f.users.users["alice"]
	alice.RoleID = "auditor"
	f.users.users["alice"] = alice

	recorder := f.call(t, "alice", http.MethodGet, "/__apikeys", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if keys := decodeSlice[domain.APIKey](t, recorder.Body.Bytes()); len(keys) != 2 {
		t.Fatalf("auditor sees %d keys, want 2", len(keys))
	}
	if recorder := f.call(t, "alice", http.MethodDelete, "/__apikeys/k-admin", ""); recorder.Code != http.StatusForbidden {
		t.Fatalf("auditor delete status=%d, want 403", recorder.Code)
	}
	// /__state 下发的视图也必须按范围收敛：all 才能看到全部 Provider。
	state := f.call(t, "alice", http.MethodGet, "/__state", "")
	if state.Code != http.StatusOK {
		t.Fatalf("auditor state status=%d body=%s", state.Code, state.Body.String())
	}
	var snapshot domain.GatewayState
	if err := json.Unmarshal(state.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.APIKeys) != 2 || len(snapshot.Providers) != 2 {
		t.Fatalf("scope=all snapshot: %d keys / %d providers, want 2/2", len(snapshot.APIKeys), len(snapshot.Providers))
	}
}

// 被授权范围：管理员把 shared 授给 alice，alice 就能在自己的密钥里绑定它，
// 但仍看不见、也改不了别人的行。旧角色里的 "granted" 范围按 own 读回。
func TestGrantedScopeControlsKeyBinding(t *testing.T) {
	f := newRBACFixture(t)
	if err := f.roles.SaveRole(domain.Role{
		ID: "restricted", Name: "受限用户", Scope: domain.ScopeGranted,
		Permissions: []domain.Permission{domain.PermProvidersView, domain.PermKeysView, domain.PermKeysCreate},
	}); err != nil {
		t.Fatal(err)
	}
	alice := f.users.users["alice"]
	alice.RoleID = "restricted"
	f.users.users["alice"] = alice
	f.roles.grants["alice"] = []string{"shared"}

	allowed := f.call(t, "alice", http.MethodPost, "/__apikeys", `{"name":"granted key","routeId":"route-admin"}`)
	if allowed.Code != http.StatusCreated {
		t.Fatalf("granted create status=%d body=%s", allowed.Code, allowed.Body.String())
	}

	// 收回 shared 之后，同一个路由不能再绑：范围按「当前」授权判定，不是建 Key 那一刻。
	f.roles.grants["alice"] = nil
	revoked := f.call(t, "alice", http.MethodPost, "/__apikeys", `{"name":"stale grant","routeId":"route-admin"}`)
	if revoked.Code != http.StatusForbidden {
		t.Fatalf("revoked grant status=%d body=%s", revoked.Code, revoked.Body.String())
	}

	// 自有资源不在授权列表里也始终可用。
	own := f.call(t, "alice", http.MethodPost, "/__apikeys", `{"name":"own route key","routeId":"route-alice"}`)
	if own.Code != http.StatusCreated {
		t.Fatalf("own route status=%d body=%s", own.Code, own.Body.String())
	}
}

// 用户管理里分配的 Provider 是叠加的：普通用户的角色和权限都不变，自己的
// 资源照旧可用，被分配的 Provider 也能绑进自己的密钥。
func TestUserGrantsAddToOwnScope(t *testing.T) {
	f := newRBACFixture(t)

	before := f.call(t, "alice", http.MethodPost, "/__apikeys", `{"name":"before","routeId":"route-admin"}`)
	if before.Code != http.StatusForbidden {
		t.Fatalf("ungranted bind status=%d, want 403 body=%s", before.Code, before.Body.String())
	}

	f.roles.grants["alice"] = []string{"shared"}

	granted := f.call(t, "alice", http.MethodPost, "/__apikeys", `{"name":"granted key","routeId":"route-admin"}`)
	if granted.Code != http.StatusCreated {
		t.Fatalf("granted bind status=%d body=%s", granted.Code, granted.Body.String())
	}
	own := f.call(t, "alice", http.MethodPost, "/__apikeys", `{"name":"own key","routeId":"route-alice"}`)
	if own.Code != http.StatusCreated {
		t.Fatalf("own bind status=%d body=%s", own.Code, own.Body.String())
	}
	if alice := f.users.users["alice"]; alice.RoleID != domain.RoleIDUser {
		t.Fatalf("grant changed alice role to %q, want user", alice.RoleID)
	}

	// 授权只扩大「可用」，不扩大「可改」：改别人的 Provider 仍然 403。
	hijack := f.call(t, "alice", http.MethodPatch, "/__providers/shared", `{"name":"hijack"}`)
	if hijack.Code != http.StatusForbidden {
		t.Fatalf("granted user edit status=%d, want 403 body=%s", hijack.Code, hijack.Body.String())
	}

	state := f.call(t, "alice", http.MethodGet, "/__state", "")
	if state.Code != http.StatusOK {
		t.Fatalf("state status=%d body=%s", state.Code, state.Body.String())
	}
	var snapshot domain.GatewayState
	if err := json.Unmarshal(state.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, provider := range snapshot.Providers {
		seen[provider.ID] = true
	}
	if !seen["shared"] || !seen["alice-private"] || len(snapshot.Providers) != 2 {
		t.Fatalf("alice providers=%v, want own + granted", seen)
	}
}

// 用户报告的原文：「给了角色查看权限，额度保活显示 permission denied:
// missing quota.manage」。只读角色必须能打开页面（列表 + 明细），
// 但新建、改、删、立即执行仍然要求 quota.manage。
func TestQuotaViewOnlyRoleCanOpenPage(t *testing.T) {
	f := newRBACFixture(t)
	f.server.scheduledTaskStore = newFakeScheduledTaskStore()
	tasks := f.server.scheduledTaskStore
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tasks.SaveScheduledTask(domain.ScheduledTask{
		ID: "q-alice", Name: "alice 的保活", OwnerUserID: "alice", Enabled: true,
		Mode: domain.ScheduleEveryHours, Interval: 5, ProviderIDs: []string{"alice-private"},
	}))
	must(tasks.SaveScheduledTask(domain.ScheduledTask{
		ID: "q-admin", Name: "admin 的保活", OwnerUserID: "admin", Enabled: true,
		Mode: domain.ScheduleEveryHours, Interval: 5, ProviderIDs: []string{"shared"},
	}))
	must(f.roles.SaveRole(domain.Role{
		ID: "quota-auditor", Name: "保活只读", Scope: domain.ScopeOwn,
		Permissions: []domain.Permission{domain.PermQuotaKeeperView},
	}))
	alice := f.users.users["alice"]
	alice.RoleID = "quota-auditor"
	f.users.users["alice"] = alice

	list := f.call(t, "alice", http.MethodGet, "/__quota-tasks", "")
	if list.Code != http.StatusOK {
		t.Fatalf("view-only list status=%d body=%s", list.Code, list.Body.String())
	}
	var payload struct {
		Tasks []domain.ScheduledTask `json:"tasks"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Tasks) != 1 || payload.Tasks[0].ID != "q-alice" {
		t.Fatalf("view-only list = %+v, want only own task", payload.Tasks)
	}

	denied := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/__quota-tasks", `{"name":"x","providerIds":["alice-private"]}`},
		{http.MethodPost, "/__quota-tasks/preview", `{"providerIds":["alice-private"]}`},
		{http.MethodPatch, "/__quota-tasks/q-alice", `{"name":"renamed"}`},
		{http.MethodDelete, "/__quota-tasks/q-alice", ""},
		{http.MethodPost, "/__quota-tasks/q-alice/run", ""},
	}
	for _, tc := range denied {
		if recorder := f.call(t, "alice", tc.method, tc.path, tc.body); recorder.Code != http.StatusForbidden {
			t.Fatalf("%s %s status=%d, want 403 body=%s", tc.method, tc.path, recorder.Code, recorder.Body.String())
		}
	}
}

// 「全部资源」范围对定时任务与 Provider/路由一致：只读能看到所有任务并拿到
// 归属名，拿到维护权限后也能改别人建的任务、用别人的 Provider 建任务。
func TestScopeAllQuotaRoleCoversEveryTask(t *testing.T) {
	f := newRBACFixture(t)
	f.server.scheduledTaskStore = newFakeScheduledTaskStore()
	if err := f.server.scheduledTaskStore.SaveScheduledTask(domain.ScheduledTask{
		ID: "q-alice", Name: "alice 的保活", OwnerUserID: "alice", Enabled: true,
		Mode: domain.ScheduleEveryHours, Interval: 5, ProviderIDs: []string{"alice-private"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.server.scheduledTaskStore.SaveScheduledTask(domain.ScheduledTask{
		ID: "q-admin", Name: "admin 的保活", OwnerUserID: "admin", Enabled: true,
		Mode: domain.ScheduleEveryHours, Interval: 5, ProviderIDs: []string{"shared"},
	}); err != nil {
		t.Fatal(err)
	}
	setAliceRole := func(id string, perms ...domain.Permission) {
		t.Helper()
		if err := f.roles.SaveRole(domain.Role{ID: id, Name: id, Scope: domain.ScopeAll, Permissions: perms}); err != nil {
			t.Fatal(err)
		}
		alice := f.users.users["alice"]
		alice.RoleID = id
		f.users.users["alice"] = alice
	}

	setAliceRole("quota-all-view", domain.PermQuotaKeeperView)
	list := f.call(t, "alice", http.MethodGet, "/__quota-tasks", "")
	if list.Code != http.StatusOK {
		t.Fatalf("scope=all view list status=%d body=%s", list.Code, list.Body.String())
	}
	var payload struct {
		Tasks      []domain.ScheduledTask `json:"tasks"`
		OwnerNames map[string]string      `json:"ownerNames"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Tasks) != 2 {
		t.Fatalf("scope=all view sees %d tasks, want 2", len(payload.Tasks))
	}
	if len(payload.OwnerNames) != 2 {
		t.Fatalf("scope=all ownerNames=%v, want both owners", payload.OwnerNames)
	}
	if recorder := f.call(t, "alice", http.MethodPatch, "/__quota-tasks/q-admin", `{"name":"x"}`); recorder.Code != http.StatusForbidden {
		t.Fatalf("view-only patch status=%d, want 403", recorder.Code)
	}

	setAliceRole("quota-all-manage", domain.PermQuotaKeeperView, domain.PermQuotaKeeperManage)
	if recorder := f.call(t, "alice", http.MethodPatch, "/__quota-tasks/q-admin", `{"name":"改过的","mode":"every_hours","interval":6,"providerIds":["shared"]}`); recorder.Code != http.StatusOK {
		t.Fatalf("scope=all manage patch status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := f.call(t, "alice", http.MethodPost, "/__quota-tasks", `{"name":"监管任务","mode":"every_hours","interval":5,"providerIds":["shared"]}`); recorder.Code != http.StatusOK {
		t.Fatalf("scope=all create on shared provider status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// 角色接口：内置不可改不可删、改角色名冲突由存储层兜底、权限点必须存在。
func TestRoleAPIValidation(t *testing.T) {
	f := newRBACFixture(t)

	if recorder := f.call(t, "admin", http.MethodPatch, "/__roles/admin", `{"name":"x","permissions":["providers.view"]}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("builtin update status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := f.call(t, "admin", http.MethodDelete, "/__roles/user", ""); recorder.Code != http.StatusBadRequest {
		t.Fatalf("builtin delete status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := f.call(t, "admin", http.MethodPost, "/__roles", `{"name":"bad","permissions":["not.a.permission"]}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown permission status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := f.call(t, "admin", http.MethodPost, "/__roles", `{"name":"wildcard","permissions":["*"]}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("wildcard must not be assignable, status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	created := f.call(t, "admin", http.MethodPost, "/__roles", `{"name":"运维","description":"d","scope":"all","permissions":["usage.view","logs.view"]}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var role domain.Role
	_ = json.Unmarshal(created.Body.Bytes(), &role)
	if role.ID == "" || role.Builtin {
		t.Fatalf("created role=%+v", role)
	}

	// 有用户占用时不能删。
	alice := f.users.users["alice"]
	alice.RoleID = role.ID
	f.users.users["alice"] = alice
	if recorder := f.call(t, "admin", http.MethodDelete, "/__roles/"+role.ID, ""); recorder.Code != http.StatusConflict {
		t.Fatalf("delete in-use role status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// 复制出来的是独立的自定义角色。
	copied := f.call(t, "admin", http.MethodPost, "/__roles/"+role.ID+"/copy", "")
	if copied.Code != http.StatusCreated {
		t.Fatalf("copy status=%d body=%s", copied.Code, copied.Body.String())
	}
	var clone domain.Role
	_ = json.Unmarshal(copied.Body.Bytes(), &clone)
	if clone.ID == role.ID || clone.Builtin {
		t.Fatalf("clone=%+v", clone)
	}

	// 授权目标列表带归属人名字，前端据此区分同名 Provider。
	targets := f.call(t, "admin", http.MethodGet, "/__roles/grant-targets", "")
	if targets.Code != http.StatusOK {
		t.Fatalf("grant targets status=%d body=%s", targets.Code, targets.Body.String())
	}
	if !strings.Contains(targets.Body.String(), `"ownerName":"王管理"`) {
		t.Fatalf("grant targets missing owner display name: %s", targets.Body.String())
	}
}

// 用户接口：角色 id 校验、保留名、授权列表落库、被删用户的 Key 归还管理员。
func TestUserAPIRoleAndGrants(t *testing.T) {
	f := newRBACFixture(t)

	if recorder := f.call(t, "admin", http.MethodPost, "/__users", `{"username":"admin","password":"password123"}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("reserved username status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder := f.call(t, "admin", http.MethodPost, "/__users", `{"username":"bob","password":"password123","roleId":"ghost"}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown role status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	created := f.call(t, "admin", http.MethodPost, "/__users", `{"username":"bob","password":"password123","roleId":"grantee","displayName":"鲍勃","grantedProviderIds":["shared"]}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var bob domain.User
	_ = json.Unmarshal(created.Body.Bytes(), &bob)
	if bob.RoleID != domain.RoleIDGrantee || bob.DisplayName != "鲍勃" {
		t.Fatalf("created user=%+v", bob)
	}
	if got := f.roles.grants[bob.ID]; len(got) != 1 || got[0] != "shared" {
		t.Fatalf("grants=%v, want [shared]", got)
	}

	// 列表要带角色名与范围，用户管理页靠它显示徽章。
	list := f.call(t, "admin", http.MethodGet, "/__users", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	if !strings.Contains(list.Body.String(), `"roleName":"受限用户"`) || !strings.Contains(list.Body.String(), `"grantedProviderIds":["shared"]`) {
		t.Fatalf("user list missing role/grant fields: %s", list.Body.String())
	}

	// 删除用户：其密钥归还真实管理员账号，而不是留空。
	if recorder := f.call(t, "admin", http.MethodDelete, "/__users/alice", ""); recorder.Code != http.StatusOK && recorder.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for _, key := range f.state.APIKeys {
		if key.ID == "k-alice" && key.OwnerUserID != "admin" {
			t.Fatalf("orphan key owner=%q, want admin", key.OwnerUserID)
		}
	}
}

// 头像与显示名称：只接受本机上送的位图 data URL，SVG/脚本一律拒绝。
func TestProfileUpdateAcceptsRasterAvatarOnly(t *testing.T) {
	f := newRBACFixture(t)
	png := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	ok := f.call(t, "alice", http.MethodPatch, "/__profile", `{"displayName":"小艾","avatarUrl":"`+png+`"}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("profile status=%d body=%s", ok.Code, ok.Body.String())
	}
	if !strings.Contains(ok.Body.String(), `"displayName":"小艾"`) || !strings.Contains(ok.Body.String(), `"roleId":"user"`) {
		t.Fatalf("status payload missing profile fields: %s", ok.Body.String())
	}
	if stored := f.users.users["alice"]; stored.DisplayName != "小艾" || !strings.HasPrefix(stored.AvatarURL, "data:image/png;base64,") {
		t.Fatalf("stored user=%+v", stored)
	}

	svg := f.call(t, "alice", http.MethodPatch, "/__profile", `{"avatarUrl":"data:image/svg+xml;base64,PHN2Zz48L3N2Zz4="}`)
	if svg.Code != http.StatusBadRequest {
		t.Fatalf("svg avatar status=%d, want 400", svg.Code)
	}

	// 隐式管理员（只有本地管理密码）没有账号行，不能在个人资料里改。
	if recorder := f.call(t, legacyAdminUserID, http.MethodPatch, "/__profile", `{"displayName":"x"}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("implicit admin profile status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
