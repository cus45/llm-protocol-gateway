package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// quotaRequest builds a console request as if ServeMux had routed it: the
// {id} path value has to be set by hand because these tests call the handler
// directly, and identity is normally injected by withAdminAuth.
func quotaRequest(method, target, body, pathID, userID string, role domain.UserRole) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if pathID != "" {
		request.SetPathValue("id", pathID)
	}
	if userID == "" {
		return request
	}
	return request.WithContext(context.WithValue(request.Context(), authContextKey{},
		sessionIdentity{UserID: userID, Username: userID, Role: role}))
}

// 额度保活对普通用户开放，所以任务与 Provider 两层权限都要钉住：只能看见并
// 操作自己的任务，只能调度已授权给自己的 Provider。
func TestQuotaOwnerLostAccess(t *testing.T) {
	router := NewRouter(domain.GatewayState{Providers: []domain.Provider{
		{ID: "granted", Name: "Granted", Protocol: domain.ProtocolClaude, BaseURL: "https://x.example/v1"},
		{ID: "other", Name: "Other", Protocol: domain.ProtocolClaude, BaseURL: "https://y.example/v1"},
		{ID: "mine", Name: "Mine", Protocol: domain.ProtocolClaude, OwnerUserID: "u1", BaseURL: "https://z.example/v1"},
	}})
	server := NewServer(router, monitor.NewStore())
	server.userStore = newMemoryUserStore(
		domain.User{ID: "u1", Username: "u1", Role: domain.UserRoleUser, RoleID: domain.RoleIDGrantee, Enabled: true, AllowedResourceIDs: []string{"granted"}},
		domain.User{ID: "u2", Username: "u2", Role: domain.UserRoleUser, RoleID: domain.RoleIDGrantee, Enabled: false, AllowedResourceIDs: []string{"granted"}},
		domain.User{ID: "boss", Username: "boss", Role: domain.UserRoleAdmin, Enabled: true},
	)

	cases := []struct {
		name       string
		owner      string
		providerID string
		wantLost   bool
	}{
		{"no owner (pre multi-user task)", "", "granted", false},
		{"legacy admin owner", legacyAdminUserID, "other", false},
		{"granted provider", "u1", "granted", false},
		{"own provider implies access", "u1", "mine", false},
		{"provider of another user", "u1", "other", true},
		{"admin-role owner is unrestricted", "boss", "other", false},
		{"disabled owner goes quiet", "u2", "granted", true},
		{"deleted owner goes quiet", "ghost", "granted", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := server.quotaOwnerLostAccess(tc.owner, tc.providerID); got != tc.wantLost {
				t.Fatalf("lost access = %v, want %v", got, tc.wantLost)
			}
		})
	}
}

func TestProbeProviderForQuotaSkipsRevokedProvider(t *testing.T) {
	upstream := chatUpstream(t, nil)
	server, _ := newQuotaTestServer(t, upstream.URL)
	server.userStore = newMemoryUserStore(domain.User{
		ID: "u1", Username: "u1", Role: domain.UserRoleUser, Enabled: true, AllowedResourceIDs: []string{"some-other"},
	})
	run := server.probeProviderForQuota(context.Background(), domain.ScheduledTask{ID: "task-1", OwnerUserID: "u1"}, "p1", false)
	if run.Outcome != domain.QuotaRunSkipped || !strings.Contains(run.Reason, "不再授权") {
		t.Fatalf("revoked owner run = %+v", run)
	}
	// The admin's own task is never restricted by grants.
	adminRun := server.probeProviderForQuota(context.Background(), domain.ScheduledTask{ID: "task-1", OwnerUserID: legacyAdminUserID}, "p1", false)
	if adminRun.Outcome != domain.QuotaRunFired {
		t.Fatalf("admin task run = %+v", adminRun)
	}
}

func TestScheduledTaskVisibilityAndProviderGrants(t *testing.T) {
	server, store := newQuotaTestServer(t, "http://127.0.0.1:1")
	server.userStore = newMemoryUserStore(
		domain.User{ID: "u1", Username: "alice", Role: domain.UserRoleUser, Enabled: true, AllowedResourceIDs: []string{"p1"}},
		domain.User{ID: "u2", Username: "bob", Role: domain.UserRoleUser, Enabled: true},
	)
	store.SaveScheduledTask(domain.ScheduledTask{ID: "t-admin", Name: "管理员的任务", OwnerUserID: legacyAdminUserID, ProviderIDs: []string{"p1"}, Enabled: true})
	store.SaveScheduledTask(domain.ScheduledTask{ID: "t-u1", Name: "爱丽丝的任务", OwnerUserID: "u1", ProviderIDs: []string{"p1"}, Enabled: true})
	store.SaveScheduledTask(domain.ScheduledTask{ID: "t-u2", Name: "鲍勃的任务", OwnerUserID: "u2", ProviderIDs: []string{"p1"}, Enabled: true})
	store.AppendScheduledTaskRuns([]domain.ScheduledTaskRun{
		{TaskID: "t-admin", ProviderID: "p1", BatchID: "b1", FiredAt: formatScheduleStamp(time.Now()), Outcome: domain.QuotaRunFired},
		{TaskID: "t-u1", ProviderID: "p1", BatchID: "b2", FiredAt: formatScheduleStamp(time.Now()), Outcome: domain.QuotaRunFired},
		{TaskID: "t-u2", ProviderID: "p1", BatchID: "b3", FiredAt: formatScheduleStamp(time.Now()), Outcome: domain.QuotaRunFired},
	})

	t.Run("admin sees every task and the owner names", func(t *testing.T) {
		rec := httptest.NewRecorder()
		server.handleListScheduledTasks(rec, quotaRequest(http.MethodGet, "/__quota-tasks", "", "", legacyAdminUserID, domain.UserRoleAdmin))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
		}
		var payload struct {
			Tasks      []domain.ScheduledTask    `json:"tasks"`
			RecentRuns []domain.ScheduledTaskRun `json:"recentRuns"`
			OwnerNames map[string]string         `json:"ownerNames"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Tasks) != 3 || len(payload.RecentRuns) != 3 {
			t.Fatalf("admin view = %d tasks / %d runs", len(payload.Tasks), len(payload.RecentRuns))
		}
		if payload.OwnerNames["u1"] != "alice" || payload.OwnerNames[legacyAdminUserID] != "内置管理员" {
			t.Fatalf("owner names = %v", payload.OwnerNames)
		}
	})

	t.Run("a user sees only their own task and runs", func(t *testing.T) {
		rec := httptest.NewRecorder()
		server.handleListScheduledTasks(rec, quotaRequest(http.MethodGet, "/__quota-tasks", "", "", "u1", domain.UserRoleUser))
		var payload struct {
			Tasks      []domain.ScheduledTask    `json:"tasks"`
			RecentRuns []domain.ScheduledTaskRun `json:"recentRuns"`
			OwnerNames map[string]string         `json:"ownerNames"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Tasks) != 1 || payload.Tasks[0].ID != "t-u1" {
			t.Fatalf("user tasks = %+v", payload.Tasks)
		}
		if len(payload.RecentRuns) != 1 || payload.RecentRuns[0].TaskID != "t-u1" {
			t.Fatalf("user runs = %+v", payload.RecentRuns)
		}
		if _, leaked := payload.OwnerNames["u2"]; leaked {
			t.Fatalf("owner table leaked to a normal user: %v", payload.OwnerNames)
		}
	})

	t.Run("foreign task is 404 not 403", func(t *testing.T) {
		// 403 would confirm the id exists; the run-history and delete paths share
		// the same guard.
		rec := httptest.NewRecorder()
		server.handleListScheduledTaskRuns(rec, quotaRequest(http.MethodGet, "/__quota-tasks/t-admin/runs", "", "t-admin", "u1", domain.UserRoleUser))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("runs of a foreign task status = %d, want 404 (%s)", rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		server.handleDeleteScheduledTask(rec, quotaRequest(http.MethodDelete, "/__quota-tasks/t-admin", "", "t-admin", "u1", domain.UserRoleUser))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("delete of a foreign task status = %d, want 404 (%s)", rec.Code, rec.Body.String())
		}
		if _, err := store.ScheduledTaskByID("t-admin"); err != nil {
			t.Fatalf("the admin task must survive the refused delete: %v", err)
		}
	})

	t.Run("own task is deletable", func(t *testing.T) {
		rec := httptest.NewRecorder()
		server.handleDeleteScheduledTask(rec, quotaRequest(http.MethodDelete, "/__quota-tasks/t-u1", "", "t-u1", "u1", domain.UserRoleUser))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
		}
		if _, err := store.ScheduledTaskByID("t-u1"); err == nil {
			t.Fatal("task not deleted")
		}
	})

	t.Run("creating against an un-granted provider is refused", func(t *testing.T) {
		body := `{"name":"越权","mode":"daily_times","times":["09:00"],"providerIds":["p-other"]}`
		rec := httptest.NewRecorder()
		request := quotaRequest(http.MethodPost, "/__quota-tasks", body, "", "u2", domain.UserRoleUser)
		server.handleCreateScheduledTask(rec, request)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("an unauthenticated caller gets 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		server.handleListScheduledTasks(rec, quotaRequest(http.MethodGet, "/__quota-tasks", "", "", "", domain.UserRoleUser))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}

func TestPausedTaskSerializesProviderIDsAsAnArray(t *testing.T) {
	server, store := newQuotaTestServer(t, "http://127.0.0.1:1")
	// The keeper pauses a task once every provider was deleted and stores an
	// empty list; a nil slice would serialize as null and the console maps the
	// field directly, blanking the whole page.
	store.SaveScheduledTask(domain.ScheduledTask{ID: "t-paused", Name: "已暂停", OwnerUserID: legacyAdminUserID, Enabled: false})
	rec := httptest.NewRecorder()
	server.handleListScheduledTasks(rec, quotaRequest(http.MethodGet, "/__quota-tasks", "", "", legacyAdminUserID, domain.UserRoleAdmin))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Tasks []struct {
			ProviderIDs json.RawMessage `json:"providerIds"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Tasks) != 1 || string(payload.Tasks[0].ProviderIDs) != "[]" {
		t.Fatalf("providerIds = %s, want []", payload.Tasks[0].ProviderIDs)
	}
}
