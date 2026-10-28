package store

import (
	"database/sql"
	"fmt"
)

// Host metric samples are the machine page's history. One row per sample is a
// few dozen bytes, so a 3 s cadence for a day is ~3 MB — cheap enough to keep
// across restarts, which is the whole point: the in-memory ring resets on
// restart, the database does not.
//
// Columns are the chart's own axes, not the full HostMetrics struct: keeping
// the JSON blob per sample would be ~2 KB × 28 800 rows a day.
type HostMetricSample struct {
	AtMs       int64   `json:"at"`
	CPU        float64 `json:"cpu"`
	Load       float64 `json:"load"`
	MemUsed    uint64  `json:"memUsed"`
	MemTotal   uint64  `json:"memTotal"`
	MemPercent float64 `json:"memPercent"`
	ProcessCPU float64 `json:"processCpu"`
	DiskRead   float64 `json:"diskRead"`
	DiskWrite  float64 `json:"diskWrite"`
	NetRx      float64 `json:"netRx"`
	NetTx      float64 `json:"netTx"`
	Goroutines int     `json:"goroutines"`
	RSS        float64 `json:"rss"`
	FDs        int     `json:"fds"`
}

func ensureHostMetricsTable(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS host_metrics_samples (
		at INTEGER PRIMARY KEY,
		cpu REAL NOT NULL DEFAULT 0,
		load1 REAL NOT NULL DEFAULT 0,
		mem_used INTEGER NOT NULL DEFAULT 0,
		mem_total INTEGER NOT NULL DEFAULT 0,
		mem_percent REAL NOT NULL DEFAULT 0,
		process_cpu REAL NOT NULL DEFAULT 0,
		disk_read REAL NOT NULL DEFAULT 0,
		disk_write REAL NOT NULL DEFAULT 0,
		net_rx REAL NOT NULL DEFAULT 0,
		net_tx REAL NOT NULL DEFAULT 0,
		goroutines INTEGER NOT NULL DEFAULT 0,
		rss REAL NOT NULL DEFAULT 0,
		fds INTEGER NOT NULL DEFAULT 0
	)`)
	return err
}

// AppendHostMetricSamples writes one batch in a single transaction. The
// primary key is the sample timestamp, so a retried batch cannot duplicate a
// point (INSERT OR REPLACE keeps the newer row).
func (s *Store) AppendHostMetricSamples(samples []HostMetricSample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.Prepare(`INSERT OR REPLACE INTO host_metrics_samples (
		at, cpu, load1, mem_used, mem_total, mem_percent, process_cpu,
		disk_read, disk_write, net_rx, net_tx, goroutines, rss, fds
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = statement.Close() }()
	for _, sample := range samples {
		if sample.AtMs <= 0 {
			continue
		}
		if _, err := statement.Exec(sample.AtMs, sample.CPU, sample.Load, sample.MemUsed, sample.MemTotal,
			sample.MemPercent, sample.ProcessCPU, sample.DiskRead, sample.DiskWrite, sample.NetRx, sample.NetTx,
			sample.Goroutines, sample.RSS, sample.FDs); err != nil {
			return fmt.Errorf("append host metric sample: %w", err)
		}
	}
	return tx.Commit()
}

// HostMetricSamplesSince returns every sample newer than fromMs, oldest first.
func (s *Store) HostMetricSamplesSince(fromMs int64) ([]HostMetricSample, error) {
	rows, err := s.reader().Query(`SELECT at, cpu, load1, mem_used, mem_total, mem_percent, process_cpu,
		disk_read, disk_write, net_rx, net_tx, goroutines, rss, fds
		FROM host_metrics_samples WHERE at >= ? ORDER BY at ASC`, fromMs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []HostMetricSample
	for rows.Next() {
		var sample HostMetricSample
		if err := rows.Scan(&sample.AtMs, &sample.CPU, &sample.Load, &sample.MemUsed, &sample.MemTotal,
			&sample.MemPercent, &sample.ProcessCPU, &sample.DiskRead, &sample.DiskWrite, &sample.NetRx,
			&sample.NetTx, &sample.Goroutines, &sample.RSS, &sample.FDs); err != nil {
			return nil, err
		}
		out = append(out, sample)
	}
	return out, rows.Err()
}

// PruneHostMetricSamples drops samples older than beforeMs and returns how
// many rows were removed.
func (s *Store) PruneHostMetricSamples(beforeMs int64) (int64, error) {
	result, err := s.db.Exec(`DELETE FROM host_metrics_samples WHERE at < ?`, beforeMs)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
