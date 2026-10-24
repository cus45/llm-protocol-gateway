package monitoring

import (
	"testing"
	"time"
)

func TestSeverityMeets(t *testing.T) {
	cases := []struct {
		have, min Severity
		want      bool
	}{
		{SeverityP0, SeverityP0, true},
		{SeverityP0, SeverityP1, true},
		{SeverityP1, SeverityP0, false},
		{SeverityP2, SeverityP2, true},
		{SeverityInfo, SeverityP2, false},
	}
	for _, tc := range cases {
		if got := tc.have.Meets(tc.min); got != tc.want {
			t.Fatalf("%s meets %s = %v, want %v", tc.have, tc.min, got, tc.want)
		}
	}
}

func TestWindowStatsRatios(t *testing.T) {
	w := WindowStats{Total: 100, Success: 80, Auth401403: 10, RateLimited: 5, ServerError: 5, Empty: 3}
	if got := w.SuccessRatio(); got != 0.8 {
		t.Fatalf("success ratio = %v", got)
	}
	if got := w.ErrorRatio(); got != 0.05 {
		t.Fatalf("error ratio = %v", got)
	}
	if got := w.AuthRatio(); got != 0.1 {
		t.Fatalf("auth ratio = %v", got)
	}
	if (WindowStats{}).SuccessRatio() != 0 {
		t.Fatal("empty window must report 0, callers gate on Total")
	}
}

func TestQuotaRatioUnavailable(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	q := QuotaSnapshot{Remaining: -1, Limit: 0, Ratio: -1, ObservedAt: now}
	if q.Available() {
		t.Fatal("negative ratio must be unavailable")
	}
	q = QuotaSnapshot{Remaining: 5, Limit: 100, Ratio: 0.05, ObservedAt: now}
	if !q.Available() {
		t.Fatal("positive limit must be available")
	}
	if q.IsStale(now.Add(29*time.Minute), 30*time.Minute) {
		t.Fatal("fresh snapshot must not be stale")
	}
	if !q.IsStale(now.Add(31*time.Minute), 30*time.Minute) {
		t.Fatal("snapshot older than max age must be stale")
	}
	if !(QuotaSnapshot{}).IsStale(now, 30*time.Minute) {
		t.Fatal("zero ObservedAt must be stale")
	}
}
