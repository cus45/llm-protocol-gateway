package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// queueWaiter makes a request block in the lane's queue and returns once it is
// counted as waiting, so the test knows the queue order has been recorded.
func queueWaiter(t *testing.T, lane *providerLane, wait time.Duration) <-chan struct{} {
	t.Helper()
	before := lane.waiterCount()
	granted := make(chan struct{})
	go func() {
		release, err := lane.acquire(context.Background(), wait)
		if err != nil {
			t.Errorf("queued request failed: %v", err)
			close(granted)
			return
		}
		close(granted)
		release()
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && lane.waiterCount() <= before {
		time.Sleep(time.Millisecond)
	}
	if got := lane.waiterCount(); got <= before {
		t.Fatalf("the request never queued, waiters=%d", got)
	}
	return granted
}

func waitGranted(t *testing.T, granted <-chan struct{}, within time.Duration) bool {
	t.Helper()
	select {
	case <-granted:
		return true
	case <-time.After(within):
		return false
	}
}

// A freed slot goes to the request that queued first. Without that, a burst of
// clients can starve the oldest waiter indefinitely: every new arrival races for
// the lock and the one holding the longest surviving connection never wins it.
func TestProviderLaneWakesWaitersInArrivalOrder(t *testing.T) {
	t.Parallel()
	lane := &providerLane{max: 1, queueCap: 8, wake: make(chan struct{})}
	held, err := lane.acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	first := queueWaiter(t, lane, 5*time.Second)
	second := queueWaiter(t, lane, 5*time.Second)

	held()
	if !waitGranted(t, first, 2*time.Second) {
		t.Fatal("the first waiter was not granted the freed slot")
	}
	// The first waiter released its slot on its way out: the second must be
	// granted in turn, not left behind.
	if !waitGranted(t, second, 2*time.Second) {
		t.Fatal("the second waiter was never granted after the first one finished")
	}
	if lane.inFlight() != 0 || lane.waiterCount() != 0 {
		t.Fatalf("lane should be empty, inflight=%d waiters=%d", lane.inFlight(), lane.waiterCount())
	}
}

// A waiter that gives up leaves the queue for good: the next released slot must
// not be spent on it.
func TestProviderLaneSkippedWaiterDoesNotHoldTheQueue(t *testing.T) {
	t.Parallel()
	lane := &providerLane{max: 1, queueCap: 8, wake: make(chan struct{})}
	held, err := lane.acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// First waiter runs out of patience before any slot frees.
	_, err = lane.acquire(context.Background(), 30*time.Millisecond)
	if !errors.Is(err, errProviderSaturated) {
		t.Fatalf("expected the short waiter to be refused, got %v", err)
	}
	second := queueWaiter(t, lane, 5*time.Second)
	held()
	if !waitGranted(t, second, 2*time.Second) {
		t.Fatal("the remaining waiter should get the slot the abandoned one no longer wants")
	}
}

// A waiter in the middle of the queue that gives up must not stall the ones
// behind it. The turn lives with the head, so a departure further back cannot
// advance it — but it must not make the queue wait for a turn that is gone
// either: the head keeps its place and, when it is served, the gap is skipped.
func TestProviderLaneMiddleDepartureKeepsTheQueueMoving(t *testing.T) {
	t.Parallel()
	lane := &providerLane{max: 1, queueCap: 8, wake: make(chan struct{})}
	held, err := lane.acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	head := queueWaiter(t, lane, 5*time.Second)
	before := lane.waiterCount()
	go func() {
		// Queued second; times out only because nothing releases for it.
		_, _ = lane.acquire(context.Background(), 40*time.Millisecond)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && lane.waiterCount() <= before {
		time.Sleep(time.Millisecond)
	}
	tail := queueWaiter(t, lane, 5*time.Second)
	// Let the middle one expire and leave while the head still waits.
	time.Sleep(150 * time.Millisecond)
	held()
	if !waitGranted(t, head, 2*time.Second) {
		t.Fatal("the head should be served first")
	}
	if !waitGranted(t, tail, 2*time.Second) {
		t.Fatal("the last waiter must not be stalled by the one that left in the middle")
	}
	if got := lane.waiterCount(); got != 0 {
		t.Fatalf("the queue should be empty, has %d", got)
	}
}

// Many waiters, released one slot at a time, are served in exactly the order
// they queued. This is the property a FIFO ticket buys and the broadcast wake
// does not: with a broadcast every waiter is woken and the winner is whichever
// goroutine the scheduler picks.
func TestProviderLaneServesManyWaitersFirstComeFirstServed(t *testing.T) {
	t.Parallel()
	const waiters = 16
	lane := &providerLane{max: 1, queueCap: waiters + 1, wake: make(chan struct{})}
	held, err := lane.acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []int
	done := make(chan struct{}, waiters)
	for i := 0; i < waiters; i++ {
		i := i
		before := lane.waiterCount()
		go func() {
			release, err := lane.acquire(context.Background(), 10*time.Second)
			if err != nil {
				t.Errorf("waiter %d failed: %v", i, err)
				done <- struct{}{}
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			done <- struct{}{}
			release()
		}()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && lane.waiterCount() <= before {
			time.Sleep(time.Millisecond)
		}
	}
	if got := lane.waiterCount(); got != waiters {
		t.Fatalf("expected %d queued, got %d", waiters, got)
	}
	// Every grant releases its slot as it finishes, so the lane serves the
	// queue one at a time without the test releasing anything else.
	held()
	for i := 0; i < waiters; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d waiters were served", i, waiters)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i, got := range order {
		if got != i {
			t.Fatalf("waiters were served out of order at %d: %v", i, order)
		}
	}
}
