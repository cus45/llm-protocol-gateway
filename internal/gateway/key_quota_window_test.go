package gateway

import (
	"testing"
	"time"
)

func TestNormalizeQuotaWindow(t *testing.T) {
	cases := []struct {
		in      string
		want    quotaWindow
		wantErr bool
	}{
		{"", quotaWindowUnlimited, false},
		{"  ", quotaWindowUnlimited, false},
		{"day", quotaWindowDay, false},
		{"month", quotaWindowMonth, false},
		{" Month ", quotaWindowMonth, false},
		{"week", quotaWindowUnlimited, true},
		{"hour", quotaWindowUnlimited, true},
	}
	for _, tc := range cases {
		got, err := normalizeQuotaWindow(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("%q: expected rejection, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: unexpected error %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("%q: got %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The window must start on the local calendar boundary and end exactly when the
// next one begins: an inclusive upper bound would make every reset a few
// milliseconds late.
func TestQuotaWindowBoundsUseLocalCalendarDays(t *testing.T) {
	local := time.Local
	mid := time.Date(2026, 3, 14, 12, 34, 56, 0, local)

	from, to := quotaWindowDay.bounds(mid)
	if want := time.Date(2026, 3, 14, 0, 0, 0, 0, local); !from.Equal(want) {
		t.Fatalf("day start: got %v, want %v", from, want)
	}
	if want := time.Date(2026, 3, 15, 0, 0, 0, 0, local); !to.Equal(want) {
		t.Fatalf("day end must be the next midnight (exclusive): got %v, want %v", to, want)
	}
	// One nanosecond before midnight still belongs to the old day.
	edgeFrom, edgeTo := quotaWindowDay.bounds(to.Add(-time.Nanosecond))
	if !edgeFrom.Equal(from) || !edgeTo.Equal(to) {
		t.Fatalf("edge of the day shifted: %v ~ %v, want %v ~ %v", edgeFrom, edgeTo, from, to)
	}

	// Month across a 31-day month, a 28/29-day February and a year rollover.
	for _, tc := range []struct {
		at   time.Time
		want time.Time
	}{
		{time.Date(2026, 1, 31, 23, 59, 59, 0, local), time.Date(2026, 2, 1, 0, 0, 0, 0, local)},
		{time.Date(2028, 2, 10, 8, 0, 0, 0, local), time.Date(2028, 3, 1, 0, 0, 0, 0, local)}, // leap year
		{time.Date(2026, 12, 25, 0, 0, 0, 0, local), time.Date(2027, 1, 1, 0, 0, 0, 0, local)},
	} {
		_, monthTo := quotaWindowMonth.bounds(tc.at)
		if !monthTo.Equal(tc.want) {
			t.Fatalf("month end for %v: got %v, want %v", tc.at, monthTo, tc.want)
		}
		fromMonth, _ := quotaWindowMonth.bounds(tc.at)
		if wantStart := time.Date(tc.at.Year(), tc.at.Month(), 1, 0, 0, 0, 0, local); !fromMonth.Equal(wantStart) {
			t.Fatalf("month start: got %v, want %v", fromMonth, wantStart)
		}
	}
}

func TestQuotaWindowResetInfo(t *testing.T) {
	local := time.Local
	now := time.Date(2026, 3, 14, 23, 0, 0, 0, local)
	seconds, resetAt := quotaWindowDay.resetInfo(now)
	if want := int64(3600); seconds != want {
		t.Fatalf("retry-after: got %d, want %d", seconds, want)
	}
	if parsed, err := time.Parse(time.RFC3339, resetAt); err != nil || !parsed.Equal(now.Add(time.Hour)) {
		t.Fatalf("reset-at: got %q (err %v), want the next midnight", resetAt, err)
	}

	// The final second of a window never yields a zero (or negative) wait: a client
	// told to retry after 0s would retry into the same exhausted window.
	late := time.Date(2026, 3, 14, 23, 59, 59, 999_000_000, local)
	if seconds, _ := quotaWindowDay.resetInfo(late); seconds < 1 {
		t.Fatalf("retry-after must round up to at least a second, got %d", seconds)
	}

	if seconds, resetAt := quotaWindowUnlimited.resetInfo(now); seconds != 0 || resetAt != "" {
		t.Fatalf("an unlimited key has no reset: %d %q", seconds, resetAt)
	}
}
