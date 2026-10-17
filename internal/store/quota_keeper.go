package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

const (
	// quotaRunRetentionDays and quotaRunSafetyCap bound the run history the
	// keeper writes. The rows are tiny (one per provider per firing) but the
	// keeper fires on a schedule forever, so the table still needs a ceiling.
	quotaRunRetentionDays = 30
	quotaRunSafetyCap     = 5000
)

func ensureQuotaKeeperTables(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS scheduled_tasks (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL DEFAULT '',
		enabled INTEGER NOT NULL DEFAULT 1,
		owner_user_id TEXT NOT NULL DEFAULT '',
		mode TEXT NOT NULL DEFAULT 'daily_times',
		times TEXT NOT NULL DEFAULT '',
		interval INTEGER NOT NULL DEFAULT 0,
		anchor_at TEXT NOT NULL DEFAULT '',
		provider_ids TEXT NOT NULL DEFAULT '',
		model_overrides TEXT NOT NULL DEFAULT '',
		quiet_minutes INTEGER NOT NULL DEFAULT 0,
		probe_prompt TEXT NOT NULL DEFAULT '',
		last_run_at TEXT NOT NULL DEFAULT '',
		next_run_at TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return err
	}
	// time_zone arrived after the first release, and installs that already have
	// the table never see the CREATE above, so it goes through ALTER.
	if err := addColumnIfMissing(tx, "scheduled_tasks", "time_zone", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS scheduled_task_runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id TEXT NOT NULL,
		provider_id TEXT NOT NULL DEFAULT '',
		batch_id TEXT NOT NULL DEFAULT '',
		fired_at TEXT NOT NULL DEFAULT '',
		outcome TEXT NOT NULL DEFAULT '',
		status INTEGER NOT NULL DEFAULT 0,
		latency_ms INTEGER NOT NULL DEFAULT 0,
		model TEXT NOT NULL DEFAULT '',
		reason TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_scheduled_task_runs_time ON scheduled_task_runs(fired_at DESC, id DESC)`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_scheduled_task_runs_task ON scheduled_task_runs(task_id, fired_at DESC, id DESC)`)
	return err
}

const scheduledTaskSelectColumns = `id, name, enabled, owner_user_id, mode, times, interval, anchor_at, provider_ids, model_overrides, quiet_minutes, probe_prompt, last_run_at, next_run_at, created_at, time_zone`

func scanScheduledTask(rows *sql.Rows) (domain.ScheduledTask, error) {
	var task domain.ScheduledTask
	var enabled int
	var mode, times, providerIDs, overrides string
	if err := rows.Scan(&task.ID, &task.Name, &enabled, &task.OwnerUserID, &mode, &times, &task.Interval,
		&task.AnchorAt, &providerIDs, &overrides, &task.QuietMinutes, &task.ProbePrompt,
		&task.LastRunAt, &task.NextRunAt, &task.CreatedAt, &task.TimeZone); err != nil {
		return domain.ScheduledTask{}, err
	}
	task.Enabled = enabled != 0
	task.Mode = domain.ScheduleMode(strings.TrimSpace(mode))
	if task.Mode == "" {
		task.Mode = domain.ScheduleDailyTimes
	}
	task.Times = decodeProviderIDList(times)
	task.ProviderIDs = decodeProviderIDList(providerIDs)
	task.ModelOverrides = decodeFallbackModelOverrides(overrides)
	return task, nil
}

// ListScheduledTasks returns every keep-alive task, oldest first.
func (s *Store) ListScheduledTasks() ([]domain.ScheduledTask, error) {
	rows, err := s.reader().Query(`SELECT ` + scheduledTaskSelectColumns + ` FROM scheduled_tasks ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []domain.ScheduledTask{}
	for rows.Next() {
		task, err := scanScheduledTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// ScheduledTaskByID returns one task; a missing row is reported as an error so
// handlers can answer 404 without inspecting sql.ErrNoRows.
func (s *Store) ScheduledTaskByID(id string) (domain.ScheduledTask, error) {
	rows, err := s.reader().Query(`SELECT `+scheduledTaskSelectColumns+` FROM scheduled_tasks WHERE id = ?`, strings.TrimSpace(id))
	if err != nil {
		return domain.ScheduledTask{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return domain.ScheduledTask{}, err
		}
		return domain.ScheduledTask{}, fmt.Errorf("scheduled task not found")
	}
	return scanScheduledTask(rows)
}

// SaveScheduledTask inserts or replaces a task. Position ordering is not part
// of this table: the keeper runs every task in the same tick, so the console
// order is simply creation order.
func (s *Store) SaveScheduledTask(task domain.ScheduledTask) error {
	enabled := 0
	if task.Enabled {
		enabled = 1
	}
	mode := strings.TrimSpace(string(task.Mode))
	if mode == "" {
		mode = string(domain.ScheduleDailyTimes)
	}
	_, err := s.db.Exec(`INSERT INTO scheduled_tasks
		(id, name, enabled, owner_user_id, mode, times, interval, anchor_at, provider_ids, model_overrides,
		 quiet_minutes, probe_prompt, last_run_at, next_run_at, created_at, time_zone)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name, enabled = excluded.enabled, owner_user_id = excluded.owner_user_id,
			mode = excluded.mode, times = excluded.times, interval = excluded.interval,
			anchor_at = excluded.anchor_at, provider_ids = excluded.provider_ids,
			model_overrides = excluded.model_overrides, quiet_minutes = excluded.quiet_minutes,
			probe_prompt = excluded.probe_prompt, last_run_at = excluded.last_run_at,
			next_run_at = excluded.next_run_at, created_at = excluded.created_at,
			time_zone = excluded.time_zone`,
		task.ID, strings.TrimSpace(task.Name), enabled, strings.TrimSpace(task.OwnerUserID), mode,
		encodeProviderIDList(task.Times), task.Interval, strings.TrimSpace(task.AnchorAt),
		encodeProviderIDList(task.ProviderIDs), encodeFallbackModelOverrides(task.ModelOverrides),
		task.QuietMinutes, strings.TrimSpace(task.ProbePrompt),
		task.LastRunAt, task.NextRunAt, task.CreatedAt, strings.TrimSpace(task.TimeZone))
	return err
}

// UpdateScheduledTaskSchedule writes only the two bookkeeping timestamps. The
// keeper recomputes NextRunAt after every firing and after every console edit;
// going through SaveScheduledTask for that would race with an edit the user
// made while the batch was in flight.
func (s *Store) UpdateScheduledTaskSchedule(id, lastRunAt, nextRunAt string) error {
	_, err := s.db.Exec(`UPDATE scheduled_tasks SET last_run_at = ?, next_run_at = ? WHERE id = ?`,
		lastRunAt, nextRunAt, strings.TrimSpace(id))
	return err
}

// DeleteScheduledTask drops the task together with its run history.
func (s *Store) DeleteScheduledTask(id string) error {
	id = strings.TrimSpace(id)
	if _, err := s.db.Exec(`DELETE FROM scheduled_task_runs WHERE task_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM scheduled_tasks WHERE id = ?`, id)
	return err
}

// AppendScheduledTaskRuns records one firing's per-provider outcomes.
func (s *Store) AppendScheduledTaskRuns(runs []domain.ScheduledTaskRun) error {
	if len(runs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`INSERT INTO scheduled_task_runs
		(task_id, provider_id, batch_id, fired_at, outcome, status, latency_ms, model, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, run := range runs {
		if _, err := stmt.Exec(run.TaskID, run.ProviderID, run.BatchID, run.FiredAt, run.Outcome,
			run.Status, run.LatencyMs, run.Model, run.Reason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListScheduledTaskRuns returns the newest firings. Empty taskID asks for the
// global history; limit falls back to a page default and is capped.
func (s *Store) ListScheduledTaskRuns(taskID string, limit int) ([]domain.ScheduledTaskRun, error) {
	if limit <= 0 {
		limit = defaultScheduledTaskRunPage
	}
	if limit > maxScheduledTaskRunPage {
		limit = maxScheduledTaskRunPage
	}
	query := `SELECT id, task_id, provider_id, batch_id, fired_at, outcome, status, latency_ms, model, reason
		FROM scheduled_task_runs`
	args := []any{}
	if id := strings.TrimSpace(taskID); id != "" {
		query += ` WHERE task_id = ?`
		args = append(args, id)
	}
	query += ` ORDER BY fired_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.reader().Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []domain.ScheduledTaskRun{}
	for rows.Next() {
		var run domain.ScheduledTaskRun
		if err := rows.Scan(&run.ID, &run.TaskID, &run.ProviderID, &run.BatchID, &run.FiredAt,
			&run.Outcome, &run.Status, &run.LatencyMs, &run.Model, &run.Reason); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

const (
	defaultScheduledTaskRunPage = 100
	maxScheduledTaskRunPage     = 500
)

// PruneScheduledTaskRuns trims run history by age and by a total-row ceiling.
// The keeper calls it on its own day boundary rather than on every append:
// pruning a table this small on each write would only add lock traffic.
func (s *Store) PruneScheduledTaskRuns() error {
	cutoff := time.Now().UTC().AddDate(0, 0, -quotaRunRetentionDays).Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`DELETE FROM scheduled_task_runs WHERE fired_at < ?`, cutoff); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM scheduled_task_runs WHERE id NOT IN (
		SELECT id FROM scheduled_task_runs ORDER BY fired_at DESC, id DESC LIMIT ?
	)`, quotaRunSafetyCap)
	return err
}
