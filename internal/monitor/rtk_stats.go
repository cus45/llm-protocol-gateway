package monitor

import "time"

// RTKPersistDelta is one increment of gateway-side RTK compression savings.
// The gateway records it when it actually replaces a tool result, so the
// numbers describe work that reached the upstream payload.
type RTKPersistDelta struct {
	Day              string
	ProviderID       string
	Commands         int64
	OriginalBytes    int64
	CompressedBytes  int64
	OriginalTokens   int64
	CompressedTokens int64
}

// RTKDayStats is one daily savings bucket. Day is YYYY-MM-DD; ProviderID is
// empty for the aggregated total returned by the HTTP layer.
type RTKDayStats struct {
	Date             string  `json:"date"`
	ProviderID       string  `json:"providerId,omitempty"`
	Commands         int64   `json:"commands"`
	OriginalBytes    int64   `json:"originalBytes"`
	CompressedBytes  int64   `json:"compressedBytes"`
	OriginalTokens   int64   `json:"originalTokens"`
	CompressedTokens int64   `json:"compressedTokens"`
	SavedTokens      int64   `json:"savedTokens"`
	SavedPercent     float64 `json:"savedPercent"`
}

func (s *RTKDayStats) Finalize() {
	s.SavedTokens = s.OriginalTokens - s.CompressedTokens
	if s.SavedTokens < 0 {
		s.SavedTokens = 0
	}
	if s.OriginalTokens > 0 {
		s.SavedPercent = float64(s.SavedTokens) / float64(s.OriginalTokens) * 100
	}
}

// RTKStatsStore persists and reads RTK savings. It lives apart from the usage
// aggregates because usage days are rebuilt from request logs while compression
// savings cannot be reconstructed from a log row.
type RTKStatsStore interface {
	ApplyRTKDelta(delta RTKPersistDelta) error
	// LoadRTKStats loads buckets in [from, to). A zero from loads all history.
	LoadRTKStats(from, to time.Time) ([]RTKDayStats, error)
}
