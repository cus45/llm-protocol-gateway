package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// 同一账号的多条 Provider 行：一个订阅按"每分钟 N 次"卖给账号，不是卖给行。
// 没有 laneGroup 时每行各拿一份预算，N 行就能打出 N 倍的量，账号自己回 429。
func TestProviderLaneGroupsShareOneAccountBudget(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	lanes := newProviderLanes()
	// Two rows of one account: same LaneGroup, so the same key.
	a, b := providerLaneKey(domain.Provider{ID: "a", LaneGroup: "acct"}), providerLaneKey(domain.Provider{ID: "b", LaneGroup: "acct"})
	if a != b {
		t.Fatalf("rows of one account must share a key: %q vs %q", a, b)
	}
	lane := lanes.laneFor(a, 0, 2)
	for i, id := range []string{"a", "b"} {
		if other := lanes.laneFor(providerLaneKey(domain.Provider{ID: id, LaneGroup: "acct"}), 0, 2); other != lane {
			t.Fatalf("row %s got a lane of its own", id)
		}
		release, err := lane.acquireSlot(context.Background(), 0, now)
		if err != nil {
			t.Fatalf("request %d on %s should be admitted, got %v", i+1, id, err)
		}
		release()
	}
	// The account's budget is spent, so the next request is refused whichever
	// row it arrives on.
	if _, err := lane.acquireSlot(context.Background(), 0, now); !errors.Is(err, errProviderSaturated) {
		t.Fatalf("the account's budget is spent, the next request must be refused, got %v", err)
	}
}

// 没分组的 Provider 保持原样：各行一份预算。
func TestProviderLaneWithoutGroupKeepsSeparateBudgets(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	lanes := newProviderLanes()
	for _, id := range []string{"a", "b"} {
		lane := lanes.laneFor(id, 0, 1)
		for i := 0; i < 2; i++ {
			_, err := lane.acquireSlot(context.Background(), 0, now)
			if i == 0 && err != nil {
				t.Fatalf("%s should have a budget of its own, got %v", id, err)
			}
			if i == 1 && !errors.Is(err, errProviderSaturated) {
				t.Fatalf("%s should be limited by its own budget, got %v", id, err)
			}
		}
	}
	if lanes.laneFor(providerLaneKey(domain.Provider{ID: "a"}), 0, 1) == lanes.laneFor(providerLaneKey(domain.Provider{ID: "b"}), 0, 1) {
		t.Fatalf("ungrouped providers must not share a lane")
	}
}

// 并发上限同样是账号级的：两行合起来只能同时有两个请求在飞。
func TestProviderLaneGroupSharesConcurrencyCap(t *testing.T) {
	t.Parallel()
	server := laneGroupServer(t, domain.Provider{ID: "a", LaneGroup: "acct", MaxConcurrentRequests: 2, QueueWaitSeconds: 0},
		domain.Provider{ID: "b", LaneGroup: "acct", MaxConcurrentRequests: 2, QueueWaitSeconds: 0})

	releases := make([]func(), 0, 2)
	for _, id := range []string{"a", "b"} {
		provider, err := server.router.ProviderByID(id)
		if err != nil {
			t.Fatal(err)
		}
		release, err := server.acquireProviderSlot(context.Background(), provider)
		if err != nil {
			t.Fatalf("%s should get one of the account's two slots, got %v", id, err)
		}
		releases = append(releases, release)
	}
	provider, err := server.router.ProviderByID("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.acquireProviderSlot(context.Background(), provider); !errors.Is(err, errProviderSaturated) {
		t.Fatalf("the account's two slots are taken, a third request must be refused, got %v", err)
	}
	for _, release := range releases {
		release()
	}
	release, err := server.acquireProviderSlot(context.Background(), provider)
	if err != nil {
		t.Fatalf("a freed account slot should admit again, got %v", err)
	}
	release()
}

// 排队的请求也按整个账号先到先得：同组另一行放回来的槽，不能插到队尾前面。
func TestProviderLaneGroupQueuesFirstComeFirstServed(t *testing.T) {
	t.Parallel()
	server := laneGroupServer(t, domain.Provider{ID: "a", LaneGroup: "acct", MaxConcurrentRequests: 1, QueueWaitSeconds: 5},
		domain.Provider{ID: "b", LaneGroup: "acct", MaxConcurrentRequests: 1, QueueWaitSeconds: 5})
	holder, err := server.router.ProviderByID("a")
	if err != nil {
		t.Fatal(err)
	}
	release, err := server.acquireProviderSlot(context.Background(), holder)
	if err != nil {
		t.Fatalf("holder should take the only slot: %v", err)
	}

	order := make([]string, 0, 3)
	var mu sync.Mutex
	done := make(chan struct{}, 3)
	for _, id := range []string{"a", "b", "a"} {
		provider, err := server.router.ProviderByID(id)
		if err != nil {
			t.Fatal(err)
		}
		go func(id string, provider domain.Provider) {
			got, err := server.acquireProviderSlot(context.Background(), provider)
			if err == nil {
				mu.Lock()
				order = append(order, id)
				mu.Unlock()
				got()
			}
			done <- struct{}{}
		}(id, provider)
		// Stagger the arrivals so the queue order is deterministic.
		time.Sleep(20 * time.Millisecond)
	}
	release()
	for i := 0; i < 3; i++ {
		<-done
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "a,b,a" {
		t.Fatalf("waiters served out of arrival order: %v", order)
	}
}

// Retry-After 必须读账号的 lane，而不是一行之自己（分组后那行根本没有 lane）。
func TestProviderLaneGroupRetryAfterUsesTheAccountLane(t *testing.T) {
	t.Parallel()
	server := laneGroupServer(t, domain.Provider{ID: "a", LaneGroup: "acct", MaxConcurrentRequests: 1, QueueWaitSeconds: 0},
		domain.Provider{ID: "b", LaneGroup: "acct", MaxConcurrentRequests: 1, QueueWaitSeconds: 0})
	provider, err := server.router.ProviderByID("a")
	if err != nil {
		t.Fatal(err)
	}
	release, err := server.acquireProviderSlot(context.Background(), provider)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got := server.providerLanes.retryAfterSecondsFor(provider, time.Now()); got != providerRetryAfterSeconds {
		t.Fatalf("a full lane with no rate limit nudges by the constant, got %d", got)
	}
	// The account's lane is what a rate refusal reads, too.
	if server.providerLanes.retryAfterSeconds(providerLaneKey(provider), time.Now()) < 1 {
		t.Fatalf("a saturated lane should tell the client to retry after at least a second")
	}
}

// 控制台把 Provider 放回独立预算：传 "laneGroup":"" 时必须真的解除分组，
// 省略字段时保持原分组（PATCH 语义）。
func TestHandleSetProviderConcurrencyLaneGroup(t *testing.T) {
	t.Parallel()
	router := NewRouter(domain.GatewayState{Providers: []domain.Provider{
		{ID: "p1", Name: "p1", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://example.com/v1"},
	}})
	server := &Server{router: router, providerLanes: newProviderLanes()}

	call := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/__providers/p1/concurrency", strings.NewReader(body))
		request.SetPathValue("id", "p1")
		request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, adminIdentity()))
		recorder := httptest.NewRecorder()
		server.handleSetProviderConcurrency(recorder, request)
		return recorder
	}

	if rec := call(`{"maxConcurrentRequests":2,"laneGroup":" acct "}`); rec.Code != http.StatusOK {
		t.Fatalf("set status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := router.State().Providers[0].LaneGroup; got != "acct" {
		t.Fatalf("laneGroup stored as %q, want the trimmed acct", got)
	}

	// Omitting the field keeps the grouping.
	if rec := call(`{"maxConcurrentRequests":3}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := router.State().Providers[0].LaneGroup; got != "acct" {
		t.Fatalf("an omitted laneGroup must keep the grouping, got %q", got)
	}

	// "" puts the provider back on its own budget.
	if rec := call(`{"maxConcurrentRequests":3,"laneGroup":""}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := router.State().Providers[0].LaneGroup; got != "" {
		t.Fatalf(`laneGroup "" should ungroup, got %q`, got)
	}

	long := strings.Repeat("x", providerLaneGroupMaxLen+1)
	if rec := call(`{"maxConcurrentRequests":1,"laneGroup":"` + long + `"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("an over-long laneGroup should be refused, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestProviderLaneKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		provider domain.Provider
		want     string
	}{
		{domain.Provider{ID: "p1"}, "p1"},
		{domain.Provider{ID: "p1", LaneGroup: "  acct "}, "group:acct"},
		{domain.Provider{ID: "p1", LaneGroup: "   "}, "p1"},
	}
	for _, tc := range cases {
		if got := providerLaneKey(tc.provider); got != tc.want {
			t.Fatalf("providerLaneKey(%+v) = %q, want %q", tc.provider, got, tc.want)
		}
	}
	if !providerGrouped(domain.Provider{ID: "p1", LaneGroup: "acct"}) || providerGrouped(domain.Provider{ID: "p1"}) {
		t.Fatalf("providerGrouped must follow the laneGroup field")
	}
}

func laneGroupServer(t *testing.T, providers ...domain.Provider) *Server {
	t.Helper()
	for i := range providers {
		providers[i].Name = providers[i].ID
		providers[i].Protocol = domain.ProtocolOpenAIChat
		if providers[i].BaseURL == "" {
			providers[i].BaseURL = "https://example.com/v1"
		}
	}
	router := NewRouter(domain.GatewayState{Providers: providers})
	for _, provider := range providers {
		if stored, err := router.ProviderByID(provider.ID); err != nil || stored.LaneGroup != provider.LaneGroup {
			t.Fatalf("provider %s was not stored with its lane group: %+v (%v)", provider.ID, stored, err)
		}
	}
	return &Server{router: router, providerLanes: newProviderLanes()}
}

// 分组后的账号被自家的限速拒绝时，failover 写回的 Retry-After 必须是账号
// 那条 lane 的真实窗口剩余秒数。按行号查 lane 在分组后一无所获，只能退回
// 1 秒的常量——客户端于是每隔一秒重试一次，一直撞在同一个分钟窗口上。
func TestFailoverRetryAfterReadsTheAccountLane(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(upstream.Close)

	state := domain.GatewayState{
		Providers: []domain.Provider{{
			ID: "acct-a", Name: "acct-a", Protocol: domain.ProtocolClaude, BaseURL: upstream.URL,
			AuthType: "api_key", APIKeySource: "static", LaneGroup: "acct",
			// Rate-only with a short queue: the request waits, the budget is
			// still spent when the wait runs out, and the refusal then has to
			// name the window it is waiting on.
			RequestsPerMinute: 1, QueueWaitSeconds: 1,
		}},
		Routes: []domain.Route{{
			ID: "acct-route", Name: "acct-route", ProviderID: "acct-a",
			OutputProtocol: domain.ProtocolClaude, Mode: domain.RouteModePassThrough, Enabled: true,
		}},
	}
	server := NewServer(NewRouter(state), monitor.NewStore())
	route, err := server.router.RouteByID("acct-route")
	if err != nil {
		t.Fatalf("route lookup: %v", err)
	}
	provider, err := server.router.ProviderByID("acct-a")
	if err != nil {
		t.Fatalf("provider lookup: %v", err)
	}
	// Spend the account's one request this minute just now, so the rest of the
	// window is long.
	release, err := server.acquireProviderSlot(context.Background(), provider)
	if err != nil {
		t.Fatalf("the account's first request should be admitted: %v", err)
	}
	release()

	key := domain.APIKey{ID: "acct-key", RouteID: route.ID}
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	rec := httptest.NewRecorder()
	status, _, _, _, _, execErr := server.executeProtocolFlowWithFailover(
		rec, req, route, domain.RouteDecision{RouteID: route.ID, ProviderID: route.ProviderID},
		"m", map[string]any{"model": "m", "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		domain.ProtocolClaude, false, key, true,
	)
	if !errors.Is(execErr, errProviderSaturated) {
		t.Fatalf("err=%v, want the account's rate refusal", execErr)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status)
	}
	got := rec.Header().Get("Retry-After")
	wait, convErr := strconv.Atoi(got)
	if convErr != nil || wait <= providerRetryAfterSeconds {
		t.Fatalf("Retry-After = %q, want the ACCOUNT's rate window, not the %ds constant", got, providerRetryAfterSeconds)
	}
}
