package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// Admission control per input provider.
//
// Without a bound, one chatty client can open every request it likes against a
// subscription account and the vendor answers 429 — which the gateway then has
// to read back as a provider failure, cool the account down and fail over. A
// lane refuses the request before it leaves, so the account is never punished
// for a burst the gateway could have shaped itself.
const (
	// providerQueueWaitCap bounds the configured wait: past a minute a client
	// has usually given up, and the queue would only be holding memory.
	providerQueueWaitCap = 60 * time.Second
	// providerConcurrencyCap guards a mistyped limit.
	providerConcurrencyCap = 1000
	// providerRetryAfterSeconds is what a refused request is told to wait. The
	// lane does not know when a slot frees, so this is a nudge, not a promise.
	providerRetryAfterSeconds = 1
)

// errProviderSaturated refuses a request at the gateway instead of letting the
// upstream answer 429. It is a distinct error because the failover loop must
// treat it as "this account is busy", never as "this account is broken": a
// saturated provider is healthy and must not be cooled down.
var errProviderSaturated = errors.New("provider is at its concurrent request limit")

// providerLane is one provider's concurrency budget plus the requests waiting
// for a slot. Waiters are woken by a replaced broadcast channel rather than a
// FIFO ticket: on a wake everyone re-checks the counter under the mutex, so a
// released slot always goes to somebody and no ticket can be orphaned.
type providerLane struct {
	mu        sync.Mutex
	inflight  int
	max       int
	waiters   int
	queueCap  int
	broadcast chan struct{}
}

type providerLanes struct {
	mu    sync.Mutex
	lanes map[string]*providerLane
}

func newProviderLanes() *providerLanes {
	return &providerLanes{lanes: map[string]*providerLane{}}
}

// laneFor returns the provider's lane, sized to the current configuration. A
// provider with no limit has no lane at all.
func (l *providerLanes) laneFor(providerID string, max int) *providerLane {
	l.mu.Lock()
	defer l.mu.Unlock()
	lane, ok := l.lanes[providerID]
	if !ok {
		lane = &providerLane{max: max, broadcast: make(chan struct{})}
		l.lanes[providerID] = lane
		return lane
	}
	lane.mu.Lock()
	defer lane.mu.Unlock()
	lane.max = max
	lane.queueCap = providerQueueCapacity(max)
	return lane
}

func providerQueueCapacity(max int) int {
	if max < 1 {
		return 0
	}
	capacity := max * 4
	if capacity < 8 {
		capacity = 8
	}
	return capacity
}

// acquire takes a slot, waiting up to wait for one to free. A zero wait refuses
// immediately, which is the default: a queued request is a request whose client
// is already staring at a spinner.
func (lane *providerLane) acquire(ctx context.Context, wait time.Duration) (func(), error) {
	var deadline time.Time
	if wait > 0 {
		deadline = time.Now().Add(wait)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lane.mu.Lock()
		if lane.inflight < lane.max {
			lane.inflight++
			lane.mu.Unlock()
			return lane.release, nil
		}
		if wait <= 0 || !time.Now().Before(deadline) {
			inflight, max := lane.inflight, lane.max
			lane.mu.Unlock()
			return nil, fmt.Errorf("%w (%d/%d already in flight)", errProviderSaturated, inflight, max)
		}
		if lane.waiters >= lane.queueCap {
			waiting, limit := lane.waiters, lane.queueCap
			lane.mu.Unlock()
			return nil, fmt.Errorf("%w (queue full, %d/%d waiting)", errProviderSaturated, waiting, limit)
		}
		lane.waiters++
		wake := lane.broadcast
		lane.mu.Unlock()

		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-wake:
			timer.Stop()
			lane.doneWaiting()
		case <-ctx.Done():
			timer.Stop()
			lane.doneWaiting()
			return nil, ctx.Err()
		case <-timer.C:
			lane.doneWaiting()
			return nil, fmt.Errorf("%w (waited %s for a free slot)", errProviderSaturated, wait)
		}
	}
}

func (lane *providerLane) doneWaiting() {
	lane.mu.Lock()
	if lane.waiters > 0 {
		lane.waiters--
	}
	lane.mu.Unlock()
}

func (lane *providerLane) release() {
	lane.mu.Lock()
	if lane.inflight > 0 {
		lane.inflight--
	}
	close(lane.broadcast)
	lane.broadcast = make(chan struct{})
	lane.mu.Unlock()
}

func (lane *providerLane) inFlight() int {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	return lane.inflight
}

// waiterCount is how many requests are waiting for a slot. It exists for the
// console's view of a saturated provider and for tests that need to know a
// waiter is actually queued before releasing.
func (lane *providerLane) waiterCount() int {
	lane.mu.Lock()
	defer lane.mu.Unlock()
	return lane.waiters
}

// providerConcurrencyConfig reads the two knobs off a provider with their
// guards applied.
func providerConcurrencyConfig(provider domain.Provider) (max int, wait time.Duration) {
	max = provider.MaxConcurrentRequests
	if max < 0 {
		max = 0
	}
	if max > providerConcurrencyCap {
		max = providerConcurrencyCap
	}
	seconds := provider.QueueWaitSeconds
	if seconds < 0 {
		seconds = 0
	}
	if seconds > int(providerQueueWaitCap/time.Second) {
		seconds = int(providerQueueWaitCap / time.Second)
	}
	return max, time.Duration(seconds) * time.Second
}

// acquireProviderSlot reserves one upstream slot for the whole exchange. The
// release is safe to call more than once per request path because it is only
// ever called by the deferred closure the caller got back.
func (s *Server) acquireProviderSlot(ctx context.Context, provider domain.Provider) (func(), error) {
	max, wait := providerConcurrencyConfig(provider)
	if max <= 0 || s.providerLanes == nil {
		return func() {}, nil
	}
	lane := s.providerLanes.laneFor(provider.ID, max)
	return lane.acquire(ctx, wait)
}

func (s *Server) providerInFlight(providerID string) int {
	if s == nil || s.providerLanes == nil {
		return 0
	}
	s.providerLanes.mu.Lock()
	lane, ok := s.providerLanes.lanes[providerID]
	s.providerLanes.mu.Unlock()
	if !ok {
		return 0
	}
	return lane.inFlight()
}

// handleSetProviderConcurrency is the admin-only per-provider admission knob, on
// its own endpoint for the same reason the first-packet timeout is: the provider
// PATCH is partial, and an int there cannot tell "not sent" from "cleared".
func (s *Server) handleSetProviderConcurrency(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var payload struct {
		MaxConcurrentRequests *int `json:"maxConcurrentRequests"`
		QueueWaitSeconds      *int `json:"queueWaitSeconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.MaxConcurrentRequests == nil {
		writeOpenAIError(w, http.StatusBadRequest, `body must be {"maxConcurrentRequests": N, "queueWaitSeconds": N?}`)
		return
	}
	wait := 0
	if payload.QueueWaitSeconds != nil {
		wait = *payload.QueueWaitSeconds
	}
	updated, err := s.router.SetProviderConcurrency(r.PathValue("id"), *payload.MaxConcurrentRequests, wait)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := s.saveState(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to save configuration: "+err.Error())
		return
	}
	if s.logs != nil {
		if updated.MaxConcurrentRequests == 0 {
			s.logs.AddApp("info", "provider concurrency limit cleared", updated.ID)
		} else {
			s.logs.AddApp("info", "provider concurrency limit set", fmt.Sprintf("%s: %d concurrent, wait %ds", updated.ID, updated.MaxConcurrentRequests, updated.QueueWaitSeconds))
		}
	}
	writeJSON(w, http.StatusOK, redactProviderForClient(updated))
}
