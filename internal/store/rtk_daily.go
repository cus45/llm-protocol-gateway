package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func ensureRTKDailyTable(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS rtk_daily_buckets (
		day TEXT NOT NULL,
		provider_id TEXT NOT NULL DEFAULT '',
		commands INTEGER NOT NULL DEFAULT 0,
		original_bytes INTEGER NOT NULL DEFAULT 0,
		compressed_bytes INTEGER NOT NULL DEFAULT 0,
		original_tokens INTEGER NOT NULL DEFAULT 0,
		compressed_tokens INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (day, provider_id)
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_rtk_daily_buckets_day ON rtk_daily_buckets(day)`); err != nil {
		return err
	}
	return nil
}

// ApplyRTKDelta adds one request's savings to its day/provider bucket.
func (s *Store) ApplyRTKDelta(delta monitor.RTKPersistDelta) error {
	if delta.Commands <= 0 {
		return nil
	}
	day := strings.TrimSpace(delta.Day)
	if day == "" {
		day = time.Now().Local().Format("2006-01-02")
	}
	providerID := strings.TrimSpace(delta.ProviderID)
	if providerID == "" {
		providerID = "_unknown"
	}
	_, err := s.db.Exec(`INSERT INTO rtk_daily_buckets (
		day, provider_id, commands, original_bytes, compressed_bytes,
		original_tokens, compressed_tokens
	) VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(day, provider_id) DO UPDATE SET
		commands = rtk_daily_buckets.commands + excluded.commands,
		original_bytes = rtk_daily_buckets.original_bytes + excluded.original_bytes,
		compressed_bytes = rtk_daily_buckets.compressed_bytes + excluded.compressed_bytes,
		original_tokens = rtk_daily_buckets.original_tokens + excluded.original_tokens,
		compressed_tokens = rtk_daily_buckets.compressed_tokens + excluded.compressed_tokens`,
		day, providerID, delta.Commands, delta.OriginalBytes, delta.CompressedBytes,
		delta.OriginalTokens, delta.CompressedTokens)
	if err != nil {
		return fmt.Errorf("persist rtk daily bucket: %w", err)
	}
	return nil
}

// LoadRTKStats reads per-provider daily buckets in [from, to). A zero from
// loads all history; a zero to is treated as no upper bound.
func (s *Store) LoadRTKStats(from, to time.Time) ([]monitor.RTKDayStats, error) {
	query := `SELECT day, provider_id, commands, original_bytes, compressed_bytes,
		original_tokens, compressed_tokens FROM rtk_daily_buckets`
	var (
		rows *sql.Rows
		err  error
	)
	switch {
	case from.IsZero() && to.IsZero():
		rows, err = s.reader().Query(query + ` ORDER BY day, provider_id`)
	case from.IsZero():
		rows, err = s.reader().Query(query+` WHERE day < ? ORDER BY day, provider_id`, to.Local().Format("2006-01-02"))
	case to.IsZero():
		rows, err = s.reader().Query(query+` WHERE day >= ? ORDER BY day, provider_id`, from.Local().Format("2006-01-02"))
	default:
		rows, err = s.reader().Query(query+` WHERE day >= ? AND day < ? ORDER BY day, provider_id`,
			from.Local().Format("2006-01-02"), to.Local().Format("2006-01-02"))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]monitor.RTKDayStats, 0)
	for rows.Next() {
		var stats monitor.RTKDayStats
		if err := rows.Scan(&stats.Date, &stats.ProviderID, &stats.Commands,
			&stats.OriginalBytes, &stats.CompressedBytes,
			&stats.OriginalTokens, &stats.CompressedTokens); err != nil {
			return nil, err
		}
		stats.Finalize()
		out = append(out, stats)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date == out[j].Date {
			return out[i].ProviderID < out[j].ProviderID
		}
		return out[i].Date < out[j].Date
	})
	return out, nil
}
