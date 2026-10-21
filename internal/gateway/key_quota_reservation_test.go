package gateway

import (
	"strings"
	"sync"
	"testing"
)

func TestQuotaReservationsAddAndRelease(t *testing.T) {
	ledger := newQuotaReservations()
	if ledger.pending("k1") != 0 {
		t.Fatalf("fresh ledger should reserve nothing, got %d", ledger.pending("k1"))
	}

	releaseA := ledger.hold("k1", 100)
	releaseB := ledger.hold("k1", 250)
	if got := ledger.pending("k1"); got != 350 {
		t.Fatalf("two in-flight requests: got %d, want 350", got)
	}
	if got := ledger.pending("other"); got != 0 {
		t.Fatalf("another key must not inherit the reservation: got %d", got)
	}

	releaseA()
	releaseA() // a deferred cleanup plus an early-return path must subtract once
	if got := ledger.pending("k1"); got != 250 {
		t.Fatalf("double release subtracted twice: got %d, want 250", got)
	}

	releaseB()
	if got := ledger.pending("k1"); got != 0 {
		t.Fatalf("all released, still holding %d", got)
	}
	if len(ledger.tokens) != 0 {
		t.Fatalf("released keys must leave the map, got %v", ledger.tokens)
	}
}

// A burst is exactly what the reservation exists for: N concurrent requests that
// each read "0 used" from a not-yet-flushed aggregate must add up to N reservations.
func TestQuotaReservationsUnderConcurrentHolds(t *testing.T) {
	ledger := newQuotaReservations()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := ledger.hold("k1", 10)
			release()
		}()
	}
	wg.Wait()
	if got := ledger.pending("k1"); got != 0 {
		t.Fatalf("reservations leaked: %d still held", got)
	}
}

func TestQuotaReservationsIgnoreEmptyHolds(t *testing.T) {
	var nilLedger *quotaReservations
	if nilLedger.pending("k1") != 0 {
		t.Fatal("a server built without the ledger must not panic")
	}
	if done := nilLedger.hold("k1", 10); done == nil {
		t.Fatal("hold must always return a callable release")
	} else {
		done()
	}

	ledger := newQuotaReservations()
	ledger.hold("", 10)()
	ledger.hold("k1", 0)()
	if len(ledger.tokens) != 0 {
		t.Fatalf("no-op holds must not create entries: %v", ledger.tokens)
	}
}

// The guard sizes itself on the prompt, so the estimator must not under-count the
// language the operator's clients actually use.
func TestQuotaPromptEstimateCountsCJKAsTokens(t *testing.T) {
	ascii := quotaPromptEstimate([]byte(strings.Repeat("a", 400)))
	if want := int64(100); ascii != want {
		t.Fatalf("ascii estimate: got %d, want %d", ascii, want)
	}
	cjk := quotaPromptEstimate([]byte(strings.Repeat("中", 100)))
	if cjk < 100 {
		t.Fatalf("cjk estimate %d under-counts: each han in the prompt is roughly one token", cjk)
	}
	if quotaPromptEstimate(nil) != 0 {
		t.Fatal("an empty body estimates to zero")
	}
}
