package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 系统时钟被回拨时，预算窗口不能跟着倒流：否则已计入的准入永远出不了窗口，
// 账号会被一直拒绝，直到时钟追回来。
func TestProviderLaneSurvivesClockGoingBackwards(t *testing.T) {
	lane := &providerLane{rpm: 1, wake: make(chan struct{})}
	now := time.Unix(1_700_000_000, 0)

	release, err := lane.acquireSlot(context.Background(), 0, now)
	if err != nil {
		t.Fatalf("first admission: %v", err)
	}
	release()

	// A small backwards jump keeps the minute it already served: the budget is
	// still spent, so the next request is refused rather than doubled.
	if _, err := lane.acquireSlot(context.Background(), 0, now.Add(-5*time.Second)); !errors.Is(err, errProviderSaturated) {
		t.Fatalf("a small rollback must not refill the budget, got %v", err)
	}

	// A jump back by more than a window means the counted requests belong to a
	// past the clock no longer reaches: the window is empty again.
	release, err = lane.acquireSlot(context.Background(), 0, now.Add(-10*time.Minute))
	if err != nil {
		t.Fatalf("a large rollback must not keep the account refused: %v", err)
	}
	release()

	// And once the clock catches up, ordinary sliding behaviour resumes.
	if _, err := lane.acquireSlot(context.Background(), 0, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("the window should slide after the clock recovers: %v", err)
	}
}
