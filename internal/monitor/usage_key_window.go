package monitor

import "time"

// KeyWindowUsage is one key's spend inside a quota window, read straight from the
// day buckets.
//
// This exists for the request path: UsageStatsRangeForKeys builds a whole console
// snapshot (today, month, range, daily series, every dimension, status histogram)
// and clones the day maps to do it. A quota check asks one question — how much has
// this key already used — so it walks only the day labels the window covers.
type KeyWindowUsage struct {
	// Tokens is prompt-plus-completion, the same total the console shows as
	// "区间总 Token": InputTokens is already stored normalized (cache included),
	// so CacheTokens must never be added again here.
	Tokens int64
	// CostMicros is the priced part of that spend. Requests with no price row
	// contribute zero, which is why PricedCount travels with it.
	CostMicros   int64
	RequestCount int64
	PricedCount  int64
}

// KeyWindowUsage returns the key's totals over the half-open interval [from, to).
// A window with no day in it (the common case for a fresh day) reports zero.
func (s *Store) KeyWindowUsage(keyID string, from, to time.Time) KeyWindowUsage {
	var out KeyWindowUsage
	if s == nil || keyID == "" || !to.After(from) {
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Enumerate the calendar days covered instead of scanning every stored day:
	// buckets are permanent, so a scan would grow with the age of the install.
	start := from.Local()
	cursor := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	for cursor.Before(to) {
		if day, ok := s.usageByDay[cursor.Format("2006-01-02")]; ok {
			if stats, ok := day.byAPIKey[keyID]; ok {
				out.Tokens += stats.InputTokens + stats.OutputTokens
				out.CostMicros += stats.CostMicros
				out.RequestCount += stats.RequestCount
				out.PricedCount += stats.PricedCount
			}
		}
		cursor = cursor.AddDate(0, 0, 1)
	}
	return out
}
