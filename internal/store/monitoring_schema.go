package store

import (
	"database/sql"
	"fmt"
)

func ensureMonitoringSchema(tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS monitor_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time TEXT NOT NULL,
			kind TEXT NOT NULL,
			severity TEXT NOT NULL,
			rule_key TEXT NOT NULL DEFAULT '',
			target_type TEXT NOT NULL,
			target_id TEXT NOT NULL DEFAULT '',
			target_name TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL,
			detail_json TEXT NOT NULL DEFAULT '{}',
			fingerprint TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'firing',
			count INTEGER NOT NULL DEFAULT 1,
			first_seen TEXT NOT NULL,
			last_seen TEXT NOT NULL,
			resolved_at TEXT NOT NULL DEFAULT '',
			resolution_reason TEXT NOT NULL DEFAULT '',
			push_status TEXT NOT NULL DEFAULT '',
			push_error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS monitor_alert_states (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			fingerprint TEXT NOT NULL UNIQUE,
			rule_key TEXT NOT NULL,
			severity TEXT NOT NULL,
			target_type TEXT NOT NULL,
			target_id TEXT NOT NULL DEFAULT '',
			target_name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			disposition TEXT NOT NULL DEFAULT 'open',
			count INTEGER NOT NULL DEFAULT 0,
			first_seen TEXT NOT NULL,
			last_seen TEXT NOT NULL,
			last_notified_at TEXT NOT NULL DEFAULT '',
			acknowledged_at TEXT NOT NULL DEFAULT '',
			ignored_at TEXT NOT NULL DEFAULT '',
			suppressed_until TEXT NOT NULL DEFAULT '',
			unavailable_since TEXT NOT NULL DEFAULT '',
			resolution_reason TEXT NOT NULL DEFAULT '',
			detail_json TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE TABLE IF NOT EXISTS monitor_event_pushes (
			event_id INTEGER NOT NULL,
			channel_id TEXT NOT NULL,
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL,
			PRIMARY KEY (event_id, channel_id)
		)`,
		`CREATE TABLE IF NOT EXISTS checkin_history (
			provider_id TEXT NOT NULL,
			day TEXT NOT NULL,
			at TEXT NOT NULL DEFAULT '',
			outcome TEXT NOT NULL DEFAULT '',
			credit REAL NOT NULL DEFAULT 0,
			streak INTEGER NOT NULL DEFAULT 0,
			msg TEXT NOT NULL DEFAULT '',
			valid_until TEXT NOT NULL DEFAULT '',
			offline INTEGER NOT NULL DEFAULT 0,
			tries INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (provider_id, day)
		)`,
		`CREATE TABLE IF NOT EXISTS provider_metric_samples (
			provider_id TEXT NOT NULL,
			hour TEXT NOT NULL,
			quota_ratio REAL,
			credits REAL,
			credit_valid_until TEXT NOT NULL DEFAULT '',
			auth_expires_at TEXT NOT NULL DEFAULT '',
			health_status TEXT NOT NULL DEFAULT '',
			source_error TEXT NOT NULL DEFAULT '',
			observed_at TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (provider_id, hour)
		)`,
		`CREATE TABLE IF NOT EXISTS monitor_baselines (
			provider_id TEXT NOT NULL,
			hour_of_day INTEGER NOT NULL,
			metric TEXT NOT NULL,
			value REAL NOT NULL DEFAULT 0,
			sample_count INTEGER NOT NULL DEFAULT 0,
			computed_at TEXT NOT NULL,
			PRIMARY KEY (provider_id, hour_of_day, metric)
		)`,
		`CREATE TABLE IF NOT EXISTS monitor_probe_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			provider_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			case_key TEXT NOT NULL,
			ok INTEGER NOT NULL DEFAULT 0,
			status INTEGER NOT NULL DEFAULT 0,
			skipped TEXT NOT NULL DEFAULT '',
			duration_ms INTEGER NOT NULL DEFAULT 0,
			model TEXT NOT NULL DEFAULT '',
			error_code TEXT NOT NULL DEFAULT '',
			day TEXT NOT NULL,
			time TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_monitor_events_time ON monitor_events(time DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_monitor_events_fp ON monitor_events(fingerprint, status)`,
		`CREATE INDEX IF NOT EXISTS idx_monitor_events_target ON monitor_events(target_type, target_id, time DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_monitor_states_status ON monitor_alert_states(status, severity)`,
		`CREATE INDEX IF NOT EXISTS idx_probe_runs_provider_day ON monitor_probe_runs(provider_id, day)`,
		`CREATE INDEX IF NOT EXISTS idx_probe_runs_provider_time ON monitor_probe_runs(provider_id, time DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_provider_time ON request_logs(provider_id, time DESC)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("monitor schema: %w", err)
		}
	}
	return nil
}
