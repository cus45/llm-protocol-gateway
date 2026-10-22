package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// A subscription account is sold with a requests-per-minute budget. Without a
// budget at the gateway, the first the gateway hears of a spent budget is the
// vendor's 429 — already counted against the account. These tests pin the
// budget, its window, and the fact that a rate-only lane still lets requests
// through concurrently.

func TestProviderLaneRequestsPerMinuteBudget(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	lane := &providerLane{rpm: 3, broadcast: make(chan struct{})}

	for i := 0; i < 3; i++ {
		release, err := lane.acquireSlot(context.Background(), 0, now)
		if err != nil {
			t.Fatalf("request %d refused: %v", i+1, err)
		}
		release()
		now = now.Add(time.Second)
	}
	_, err := lane.acquireSlot(context.Background(), 0, now)
	if !errors.Is(err, errProviderSaturated) {
		t.Fatalf("fourth request in the minute should be refused, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "requests this minute") {
		t.Fatalf("error should name the rate budget: %v", err)
	}

	// The window slides off after a minute of wall clock.
	now = now.Add(time.Minute)
	release, err := lane.acquireSlot(context.Background(), 0, now)
	if err != nil {
		t.Fatalf("new minute should have a fresh budget: %v", err)
	}
	release()
}

func TestProviderLaneRateOnlyDoesNotBoundConcurrency(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	lane := &providerLane{rpm: 5, broadcast: make(chan struct{})}
	releases := make([]func(), 0, 5)
	for i := 0; i < 5; i++ {
		release, err := lane.acquireSlot(context.Background(), 0, now)
		if err != nil {
			t.Fatalf("request %d refused: %v", i+1, err)
		}
		releases = append(releases, release) // held, not released
	}
	for _, release := range releases {
		release()
	}
}

// Concurrency and rate together: a request inside the concurrency cap but over
// the rate is refused, not admitted.
func TestProviderLaneRateBlocksEvenWithFreeSlots(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	lane := &providerLane{max: 5, rpm: 1, queueCap: 8, broadcast: make(chan struct{})}
	release, err := lane.acquireSlot(context.Background(), 0, now)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := lane.acquireSlot(context.Background(), 0, now); !errors.Is(err, errProviderSaturated) {
		t.Fatalf("second request should be refused by the rate, got %v", err)
	}
}

func TestProviderConcurrencyConfigReadsRateLimit(t *testing.T) {
	t.Parallel()
	provider := domain.Provider{RequestsPerMinute: 42}
	if got := providerRequestsPerMinute(provider); got != 42 {
		t.Fatalf("providerRequestsPerMinute = %d, want 42", got)
	}
	// A mistyped value is clamped rather than allowing a runaway.
	if got := providerRequestsPerMinute(domain.Provider{RequestsPerMinute: providerRequestsPerMinuteCap + 1}); got != providerRequestsPerMinuteCap {
		t.Fatalf("clamped = %d", got)
	}
	if got := providerRequestsPerMinute(domain.Provider{RequestsPerMinute: -1}); got != 0 {
		t.Fatalf("negative = %d, want 0", got)
	}
}

// A provider with only a rate configured must still get a lane; previously a
// zero concurrency cap meant "no lane at all" and the rate was never checked.
func TestAcquireProviderSlotBuildsRateOnlyLane(t *testing.T) {
	t.Parallel()
	server := &Server{providerLanes: newProviderLanes()}
	provider := domain.Provider{ID: "p-rate", RequestsPerMinute: 1}
	release, err := server.acquireProviderSlot(context.Background(), provider)
	if err != nil {
		t.Fatalf("first request refused: %v", err)
	}
	release()
	if _, err := server.acquireProviderSlot(context.Background(), provider); !errors.Is(err, errProviderSaturated) {
		t.Fatalf("second request should be refused, got %v", err)
	}
}
