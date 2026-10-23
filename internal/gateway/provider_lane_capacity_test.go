package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// A rate-only provider (maxConcurrentRequests=0, requestsPerMinute>0) has a lane
// but no concurrency cap. Its queue capacity used to be 0, so a rate-only
// provider configured with a queue wait refused every arrival with "queue full"
// instead of letting it wait for the minute window to restart.
func TestRateOnlyLaneQueueCapacityAllowsWaiting(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	lanes := newProviderLanes()
	if capacity := providerQueueCapacity(0, 1); capacity < 1 {
		t.Fatalf("a rate-only lane must be able to queue, capacity = %d", capacity)
	}
	lane := lanes.laneFor("p-rate", 0, 1)
	if lane.queueCap < 1 {
		t.Fatalf("lane built for a rate-only provider has no queue: %d", lane.queueCap)
	}
	release, err := lane.acquireSlot(context.Background(), 0, now)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	done := make(chan error, 1)
	go func() {
		release, err := lanes.laneFor("p-rate", 0, 1).acquireSlot(context.Background(), 300*time.Millisecond, now)
		if err == nil {
			release()
		}
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && lane.waiterCount() != 1 {
		time.Sleep(2 * time.Millisecond)
	}
	if got := lane.waiterCount(); got != 1 {
		t.Fatalf("expected the rate-limited request to queue, waiting = %d", got)
	}
	err = <-done
	if !errors.Is(err, errProviderSaturated) {
		t.Fatalf("a rate wait that runs out must report saturation, got %v", err)
	}
	if strings.Contains(err.Error(), "queue full") {
		t.Fatalf("a rate-only lane must not report a full queue: %v", err)
	}
}

// A lane with no budgets admits without counting anything in flight.
func TestLaneWithoutBudgetsIsFree(t *testing.T) {
	t.Parallel()
	lane := newProviderLanes().laneFor("p-free", 0, 0)
	release, err := lane.acquire(context.Background(), 0)
	if err != nil {
		t.Fatalf("an unbudgeted lane must admit, got %v", err)
	}
	release()
	if got := lane.inFlight(); got != 0 {
		t.Fatalf("a free lane must not count in flight, got %d", got)
	}
}

// A refusal at admission says how long to wait. The rate budget knows exactly
// when its window restarts, so Retry-After must point at that, not a fixed 1s.
func TestRetryAfterAdmissionRefusalUsesRateWindow(t *testing.T) {
	t.Parallel()
	lanes := newProviderLanes()
	lane := lanes.laneFor("p1", 0, 1)
	release, err := lane.acquireSlot(context.Background(), 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	release()
	// Half a minute into the window leaves roughly half a minute to wait.
	now := time.Now()
	lane.mu.Lock()
	lane.admits = []int64{now.Add(-30 * time.Second).UnixNano()}
	lane.mu.Unlock()
	if _, err := lane.acquireSlot(context.Background(), 0, now); !errors.Is(err, errProviderSaturated) {
		t.Fatalf("expected saturation, got %v", err)
	}
	if seconds := lane.retryAfterSeconds(now); seconds < 29 || seconds > 31 {
		t.Fatalf("rate refusal should wait out the window (~30s), got %ds", seconds)
	}

	// A full concurrency cap with no rate keeps the short nudge but reports how
	// many slots are expected to free.
	busy := lanes.laneFor("p2", 2, 0)
	holdA, err := busy.acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holdA()
	holdB, err := busy.acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer holdB()
	if seconds := busy.retryAfterSeconds(now); seconds != providerBusyRetryAfterSeconds {
		t.Fatalf("a concurrency-only refusal should stay a short nudge, got %ds", seconds)
	}
}

func TestRetryAfterSecondsNilLane(t *testing.T) {
	t.Parallel()
	var lane *providerLane
	if got := lane.retryAfterSeconds(time.Now()); got != providerRetryAfterSeconds {
		t.Fatalf("nil lane = %d, want %d", got, providerRetryAfterSeconds)
	}
}

// The protocol proxy's admission refusal must carry the lane's Retry-After on the
// wire: a client that never sees it retries into the same wall.
func TestAdmissionRefusalSetsRateRetryAfterHeader(t *testing.T) {
	t.Parallel()
	state := domain.GatewayState{
		Providers: []domain.Provider{{
			ID: "lane-rate", Name: "lane-rate", Protocol: domain.ProtocolOpenAIChat,
			BaseURL: "http://127.0.0.1:1", RequestsPerMinute: 1,
		}},
		Routes: []domain.Route{{
			ID: "lane-rate-route", ProviderID: "lane-rate",
			OutputProtocol: domain.ProtocolOpenAIChat, Enabled: true,
		}},
	}
	server := NewServer(NewRouter(state), monitor.NewStore())
	route, err := server.router.RouteByID("lane-rate-route")
	if err != nil {
		t.Fatalf("route lookup: %v", err)
	}
	// Spend the minute's one request, then place the window far enough in that
	// the admission wait cannot outlast it: the request is refused rather than
	// queued for the window to slide out.
	lane := server.providerLanes.laneFor("lane-rate", 0, 1)
	release, err := lane.acquire(context.Background(), 0)
	if err != nil {
		t.Fatalf("first admission: %v", err)
	}
	release()
	lane.mu.Lock()
	lane.admits = []int64{time.Now().UnixNano()}
	lane.mu.Unlock()

	// The admission wait is measured on the request's own clock; stopping it at
	// a deadline refuses at once, which is what a saturated lane answers.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	status, _, _, execErr := server.executeProtocolFlow(
		rec, req, route, domain.RouteDecision{RouteID: route.ID, ProviderID: route.ProviderID},
		"m", map[string]any{"model": "m", "messages": []any{}}, domain.ProtocolOpenAIChat, true,
	)
	if !errors.Is(execErr, errProviderSaturated) {
		t.Fatalf("expected a saturation refusal, got status=%d err=%v", status, execErr)
	}
	got, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After missing or not a number: %q", rec.Header().Get("Retry-After"))
	}
	if got < 59 || got > 61 {
		t.Fatalf("Retry-After = %d, want the ~60s left in the rate window", got)
	}
}

// A lane refuses on its own budget — the caller's clock is not taken as proof:
// waiting runs until the budget frees a slot or the lane's wait is spent.
func TestRateOnlyLaneWaitsOutItsBudgetBeforeRefusing(t *testing.T) {
	t.Parallel()
	state := domain.GatewayState{
		Providers: []domain.Provider{{
			ID: "lane-wait", Name: "lane-wait", Protocol: domain.ProtocolOpenAIChat,
			BaseURL: "http://127.0.0.1:1", RequestsPerMinute: 1,
		}},
		Routes: []domain.Route{{
			ID: "lane-wait-route", ProviderID: "lane-wait",
			OutputProtocol: domain.ProtocolOpenAIChat, Enabled: true,
		}},
	}
	server := NewServer(NewRouter(state), monitor.NewStore())
	route, err := server.router.RouteByID("lane-wait-route")
	if err != nil {
		t.Fatalf("route lookup: %v", err)
	}
	lane := server.providerLanes.laneFor("lane-wait", 0, 1)
	release, err := lane.acquire(context.Background(), 0)
	if err != nil {
		t.Fatalf("first admission: %v", err)
	}
	release()
	// Age the admission so the wait is a short, bounded one rather than the
	// budget's cap.
	lane.mu.Lock()
	lane.admits = []int64{time.Now().Add(-time.Minute + 120*time.Millisecond).UnixNano()}
	lane.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	rec := httptest.NewRecorder()
	started := time.Now()
	status, _, _, execErr := server.executeProtocolFlow(
		rec, req, route, domain.RouteDecision{RouteID: route.ID, ProviderID: route.ProviderID},
		"m", map[string]any{"model": "m", "messages": []any{}}, domain.ProtocolOpenAIChat, true,
	)
	if waited := time.Since(started); waited < 100*time.Millisecond {
		t.Fatalf("the lane refused without waiting out its 120ms window: %v", waited)
	}
	// Past the wait the window has freed the slot, so the flow goes on to the
	// upstream (which is unreachable here): anything but a saturation refusal
	// means the admission succeeded.
	if errors.Is(execErr, errProviderSaturated) {
		t.Fatalf("a window that slid out must admit the request, status=%d err=%v", status, execErr)
	}
}
