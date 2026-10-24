package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitoring"
)

const (
	defaultMonitorPageSize = 50
	maxMonitorPageSize     = 200
)

func monitorTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseMonitorTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed.UTC()
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed.UTC()
	}
	return time.Time{}
}

func monitorDetailJSON(detail map[string]any) string {
	if detail == nil {
		return "{}"
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func parseMonitorDetail(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}
	}
	detail := map[string]any{}
	_ = json.Unmarshal([]byte(raw), &detail)
	if detail == nil {
		detail = map[string]any{}
	}
	return detail
}

const monitorStateColumns = `id, fingerprint, rule_key, severity, target_type, target_id, target_name,
	status, disposition, count, first_seen, last_seen, last_notified_at, acknowledged_at,
	ignored_at, suppressed_until, unavailable_since, resolution_reason, detail_json`

func scanMonitorState(scanner rowScanner) (monitoring.AlertState, error) {
	var (
		state                                         monitoring.AlertState
		firstSeen, lastSeen, lastNotified             string
		acknowledgedAt, ignoredAt, suppressedUntil    string
		unavailableSince, resolutionReason, detailRaw string
	)
	if err := scanner.Scan(&state.ID, &state.Fingerprint, &state.RuleKey, &state.Severity,
		&state.TargetType, &state.TargetID, &state.TargetName, &state.Status, &state.Disposition,
		&state.Count, &firstSeen, &lastSeen, &lastNotified, &acknowledgedAt, &ignoredAt,
		&suppressedUntil, &unavailableSince, &resolutionReason, &detailRaw); err != nil {
		return monitoring.AlertState{}, err
	}
	state.FirstSeen = parseMonitorTime(firstSeen)
	state.LastSeen = parseMonitorTime(lastSeen)
	state.LastNotifiedAt = parseMonitorTime(lastNotified)
	state.AcknowledgedAt = parseMonitorTime(acknowledgedAt)
	state.IgnoredAt = parseMonitorTime(ignoredAt)
	state.SuppressedUntil = parseMonitorTime(suppressedUntil)
	state.UnavailableSince = parseMonitorTime(unavailableSince)
	state.ResolutionReason = resolutionReason
	state.Detail = parseMonitorDetail(detailRaw)
	return state, nil
}

func (s *Store) LoadMonitorStates() ([]monitoring.AlertState, error) {
	rows, err := s.reader().Query(`SELECT ` + monitorStateColumns + ` FROM monitor_alert_states ORDER BY last_seen DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := []monitoring.AlertState{}
	for rows.Next() {
		state, err := scanMonitorState(rows)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, rows.Err()
}

func (s *Store) UpsertMonitorState(state monitoring.AlertState) (monitoring.AlertState, error) {
	row := s.db.QueryRow(`INSERT INTO monitor_alert_states (
			fingerprint, rule_key, severity, target_type, target_id, target_name, status,
			disposition, count, first_seen, last_seen, last_notified_at, acknowledged_at,
			ignored_at, suppressed_until, unavailable_since, resolution_reason, detail_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(fingerprint) DO UPDATE SET
			rule_key = excluded.rule_key,
			severity = excluded.severity,
			target_type = excluded.target_type,
			target_id = excluded.target_id,
			target_name = excluded.target_name,
			status = excluded.status,
			disposition = excluded.disposition,
			count = excluded.count,
			first_seen = excluded.first_seen,
			last_seen = excluded.last_seen,
			last_notified_at = excluded.last_notified_at,
			acknowledged_at = excluded.acknowledged_at,
			ignored_at = excluded.ignored_at,
			suppressed_until = excluded.suppressed_until,
			unavailable_since = excluded.unavailable_since,
			resolution_reason = excluded.resolution_reason,
			detail_json = excluded.detail_json
		RETURNING id`,
		state.Fingerprint, state.RuleKey, state.Severity, state.TargetType, state.TargetID,
		state.TargetName, state.Status, state.Disposition, state.Count,
		monitorTime(state.FirstSeen), monitorTime(state.LastSeen), monitorTime(state.LastNotifiedAt),
		monitorTime(state.AcknowledgedAt), monitorTime(state.IgnoredAt), monitorTime(state.SuppressedUntil),
		monitorTime(state.UnavailableSince), state.ResolutionReason, monitorDetailJSON(state.Detail))
	if err := row.Scan(&state.ID); err != nil {
		return state, err
	}
	return state, nil
}

func (s *Store) MonitorAlertByID(id int64) (monitoring.AlertState, bool, error) {
	row := s.reader().QueryRow(`SELECT `+monitorStateColumns+` FROM monitor_alert_states WHERE id = ?`, id)
	state, err := scanMonitorState(row)
	if err == sql.ErrNoRows {
		return monitoring.AlertState{}, false, nil
	}
	if err != nil {
		return monitoring.AlertState{}, false, err
	}
	return state, true, nil
}

func (s *Store) SetMonitorDisposition(id int64, disposition string, at time.Time) error {
	switch disposition {
	case monitoring.DispositionAcknowledged:
		_, err := s.db.Exec(`UPDATE monitor_alert_states SET disposition = ?, acknowledged_at = ? WHERE id = ?`,
			disposition, monitorTime(at), id)
		return err
	case monitoring.DispositionIgnored:
		_, err := s.db.Exec(`UPDATE monitor_alert_states SET disposition = ?, ignored_at = ? WHERE id = ?`,
			disposition, monitorTime(at), id)
		return err
	case monitoring.DispositionOpen:
		_, err := s.db.Exec(`UPDATE monitor_alert_states SET disposition = ?, acknowledged_at = '', ignored_at = '' WHERE id = ?`,
			disposition, id)
		return err
	default:
		return fmt.Errorf("invalid alert disposition %q", disposition)
	}
}

// AppendMonitorTimeline attaches an action entry to the newest event of the
// fingerprint, which is where the alert detail handler reads the history from.
func (s *Store) AppendMonitorTimeline(fingerprint string, entry monitoring.TimelineEntry) error {
	id, err := s.LatestMonitorEventID(fingerprint)
	if err != nil || id == 0 {
		return err
	}
	var raw string
	if err := s.db.QueryRow(`SELECT detail_json FROM monitor_events WHERE id = ?`, id).Scan(&raw); err != nil {
		return err
	}
	detail := parseMonitorDetail(raw)
	timeline := []monitoring.TimelineEntry{}
	if existing, ok := detail["timeline"]; ok {
		encoded, err := json.Marshal(existing)
		if err == nil {
			_ = json.Unmarshal(encoded, &timeline)
		}
	}
	timeline = append(timeline, entry)
	detail["timeline"] = timeline
	_, err = s.db.Exec(`UPDATE monitor_events SET detail_json = ? WHERE id = ?`, monitorDetailJSON(detail), id)
	return err
}

const monitorEventColumns = `id, time, kind, severity, rule_key, target_type, target_id, target_name,
	title, detail_json, fingerprint, status, count, first_seen, last_seen, resolved_at,
	resolution_reason, push_status, push_error`

func scanMonitorEvent(scanner rowScanner) (monitoring.Event, error) {
	var (
		event                                   monitoring.Event
		at, firstSeen, lastSeen, resolvedAt     string
		resolutionReason, pushStatus, pushError string
		detailRaw                               string
	)
	if err := scanner.Scan(&event.ID, &at, &event.Kind, &event.Severity, &event.RuleKey,
		&event.TargetType, &event.TargetID, &event.TargetName, &event.Title, &detailRaw,
		&event.Fingerprint, &event.Status, &event.Count, &firstSeen, &lastSeen, &resolvedAt,
		&resolutionReason, &pushStatus, &pushError); err != nil {
		return monitoring.Event{}, err
	}
	event.Time = parseMonitorTime(at)
	event.FirstSeen = parseMonitorTime(firstSeen)
	event.LastSeen = parseMonitorTime(lastSeen)
	event.ResolvedAt = parseMonitorTime(resolvedAt)
	event.ResolutionReason = resolutionReason
	event.PushStatus = pushStatus
	event.PushError = pushError
	event.Detail = parseMonitorDetail(detailRaw)
	return event, nil
}

func (s *Store) InsertMonitorEvent(event monitoring.Event) (int64, error) {
	at := event.Time
	if at.IsZero() {
		at = time.Now()
	}
	firstSeen := event.FirstSeen
	if firstSeen.IsZero() {
		firstSeen = at
	}
	lastSeen := event.LastSeen
	if lastSeen.IsZero() {
		lastSeen = at
	}
	result, err := s.db.Exec(`INSERT INTO monitor_events (
			time, kind, severity, rule_key, target_type, target_id, target_name, title,
			detail_json, fingerprint, status, count, first_seen, last_seen, resolved_at,
			resolution_reason, push_status, push_error
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		monitorTime(at), event.Kind, event.Severity, event.RuleKey, event.TargetType,
		event.TargetID, event.TargetName, event.Title, monitorDetailJSON(event.Detail),
		event.Fingerprint, event.Status, event.Count, monitorTime(firstSeen), monitorTime(lastSeen),
		monitorTime(event.ResolvedAt), event.ResolutionReason, event.PushStatus, event.PushError)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) MonitorEventByID(id int64) (monitoring.Event, bool, error) {
	row := s.reader().QueryRow(`SELECT `+monitorEventColumns+` FROM monitor_events WHERE id = ?`, id)
	event, err := scanMonitorEvent(row)
	if err == sql.ErrNoRows {
		return monitoring.Event{}, false, nil
	}
	if err != nil {
		return monitoring.Event{}, false, err
	}
	return event, true, nil
}

func (s *Store) LatestMonitorEventID(fingerprint string) (int64, error) {
	var id int64
	err := s.reader().QueryRow(`SELECT COALESCE(MAX(id), 0) FROM monitor_events WHERE fingerprint = ?`, fingerprint).Scan(&id)
	return id, err
}

func (s *Store) MonitorEventIDsByFingerprint(fingerprint string, limit int) ([]int64, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.reader().Query(`SELECT id FROM monitor_events WHERE fingerprint = ? ORDER BY id DESC LIMIT ?`,
		fingerprint, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) UpdateMonitorEventPush(eventID int64, status, message string) error {
	_, err := s.db.Exec(`UPDATE monitor_events SET push_status = ?, push_error = ? WHERE id = ?`,
		status, message, eventID)
	return err
}

func monitorPageBounds(page, pageSize int) (int, int) {
	if pageSize <= 0 {
		pageSize = defaultMonitorPageSize
	}
	if pageSize > maxMonitorPageSize {
		pageSize = maxMonitorPageSize
	}
	if page <= 0 {
		page = 1
	}
	return page, pageSize
}

func (s *Store) QueryMonitorEvents(q monitoring.EventQuery) (monitoring.EventPage, error) {
	page, pageSize := monitorPageBounds(q.Page, q.PageSize)
	where := []string{"1=1"}
	args := []any{}
	if v := strings.TrimSpace(q.Kind); v != "" {
		where = append(where, "kind = ?")
		args = append(args, v)
	}
	if v := strings.TrimSpace(q.Severity); v != "" {
		where = append(where, "severity = ?")
		args = append(args, v)
	}
	if v := strings.TrimSpace(q.TargetType); v != "" {
		where = append(where, "target_type = ?")
		args = append(args, v)
	}
	if v := strings.TrimSpace(q.TargetID); v != "" {
		where = append(where, "target_id = ?")
		args = append(args, v)
	}
	if v := strings.TrimSpace(q.Status); v != "" {
		where = append(where, "status = ?")
		args = append(args, v)
	}
	if !q.From.IsZero() {
		where = append(where, "time >= ?")
		args = append(args, monitorTime(q.From))
	}
	if !q.To.IsZero() {
		where = append(where, "time <= ?")
		args = append(args, monitorTime(q.To))
	}
	clause := strings.Join(where, " AND ")

	result := monitoring.EventPage{Items: []monitoring.Event{}, Page: page, PageSize: pageSize}
	if err := s.reader().QueryRow(`SELECT COUNT(*) FROM monitor_events WHERE `+clause, args...).Scan(&result.Total); err != nil {
		return result, err
	}
	listArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	rows, err := s.reader().Query(`SELECT `+monitorEventColumns+` FROM monitor_events WHERE `+clause+`
		ORDER BY time DESC, id DESC LIMIT ? OFFSET ?`, listArgs...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		event, err := scanMonitorEvent(rows)
		if err != nil {
			return result, err
		}
		result.Items = append(result.Items, event)
	}
	return result, rows.Err()
}

func (s *Store) QueryMonitorAlerts(q monitoring.AlertQuery) (monitoring.AlertPage, error) {
	page, pageSize := monitorPageBounds(q.Page, q.PageSize)
	where := []string{"1=1"}
	args := []any{}
	for _, filter := range []struct {
		column string
		value  string
	}{
		{"status", q.Status},
		{"disposition", q.Disposition},
		{"severity", q.Severity},
		{"rule_key", q.Rule},
		{"target_type", q.TargetType},
		{"target_id", q.TargetID},
	} {
		if v := strings.TrimSpace(filter.value); v != "" {
			where = append(where, "st."+filter.column+" = ?")
			args = append(args, v)
		}
	}
	if !q.From.IsZero() {
		where = append(where, "st.last_seen >= ?")
		args = append(args, monitorTime(q.From))
	}
	if !q.To.IsZero() {
		where = append(where, "st.last_seen <= ?")
		args = append(args, monitorTime(q.To))
	}
	clause := strings.Join(where, " AND ")

	result := monitoring.AlertPage{Items: []monitoring.AlertView{}, Page: page, PageSize: pageSize}
	if err := s.reader().QueryRow(`SELECT COUNT(*) FROM monitor_alert_states st WHERE `+clause, args...).Scan(&result.Total); err != nil {
		return result, err
	}
	listArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	rows, err := s.reader().Query(`SELECT st.id, st.fingerprint, st.rule_key, st.severity, st.target_type,
			st.target_id, st.target_name, st.status, st.disposition, st.count, st.first_seen, st.last_seen,
			st.last_notified_at, st.acknowledged_at, st.ignored_at, st.suppressed_until,
			st.unavailable_since, st.resolution_reason, st.detail_json,
			COALESCE((SELECT e.id FROM monitor_events e WHERE e.fingerprint = st.fingerprint ORDER BY e.id DESC LIMIT 1), 0),
			COALESCE((SELECT e.push_status FROM monitor_events e WHERE e.fingerprint = st.fingerprint ORDER BY e.id DESC LIMIT 1), ''),
			COALESCE((SELECT e.push_error FROM monitor_events e WHERE e.fingerprint = st.fingerprint ORDER BY e.id DESC LIMIT 1), '')
		FROM monitor_alert_states st WHERE `+clause+`
		ORDER BY st.last_seen DESC, st.id DESC LIMIT ? OFFSET ?`, listArgs...)
	if err != nil {
		return result, err
	}
	fingerprints := make([]string, 0, pageSize)
	for rows.Next() {
		var (
			view                                          monitoring.AlertView
			firstSeen, lastSeen, lastNotified             string
			acknowledgedAt, ignoredAt, suppressedUntil    string
			unavailableSince, resolutionReason, detailRaw string
		)
		if err := rows.Scan(&view.ID, &view.Fingerprint, &view.RuleKey, &view.Severity,
			&view.TargetType, &view.TargetID, &view.TargetName, &view.Status, &view.Disposition,
			&view.Count, &firstSeen, &lastSeen, &lastNotified, &acknowledgedAt, &ignoredAt,
			&suppressedUntil, &unavailableSince, &resolutionReason, &detailRaw,
			&view.LatestEventID, &view.PushStatus, &view.PushError); err != nil {
			_ = rows.Close()
			return result, err
		}
		view.FirstSeen = parseMonitorTime(firstSeen)
		view.LastSeen = parseMonitorTime(lastSeen)
		view.LastNotifiedAt = parseMonitorTime(lastNotified)
		view.AcknowledgedAt = parseMonitorTime(acknowledgedAt)
		view.IgnoredAt = parseMonitorTime(ignoredAt)
		view.SuppressedUntil = parseMonitorTime(suppressedUntil)
		view.UnavailableSince = parseMonitorTime(unavailableSince)
		view.ResolutionReason = resolutionReason
		view.Detail = parseMonitorDetail(detailRaw)
		view.RelatedEventIDs = []int64{}
		result.Items = append(result.Items, view)
		fingerprints = append(fingerprints, view.Fingerprint)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return result, err
	}
	_ = rows.Close()
	if len(fingerprints) == 0 {
		return result, nil
	}

	related, err := s.monitorEventIDsByFingerprints(fingerprints, 10)
	if err != nil {
		return result, err
	}
	for i := range result.Items {
		if ids, ok := related[result.Items[i].Fingerprint]; ok {
			result.Items[i].RelatedEventIDs = ids
		}
	}
	return result, nil
}

func (s *Store) monitorEventIDsByFingerprints(fingerprints []string, limit int) (map[string][]int64, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(fingerprints)), ",")
	args := make([]any, 0, len(fingerprints)+1)
	for _, fingerprint := range fingerprints {
		args = append(args, fingerprint)
	}
	args = append(args, limit)
	rows, err := s.reader().Query(`SELECT fingerprint, id FROM (
			SELECT fingerprint, id, ROW_NUMBER() OVER (PARTITION BY fingerprint ORDER BY id DESC) AS rn
			FROM monitor_events WHERE fingerprint IN (`+placeholders+`)
		) WHERE rn <= ? ORDER BY id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var fingerprint string
		var id int64
		if err := rows.Scan(&fingerprint, &id); err != nil {
			return nil, err
		}
		out[fingerprint] = append(out[fingerprint], id)
	}
	return out, rows.Err()
}

func (s *Store) UpsertMonitorEventPush(push monitoring.EventPush) error {
	_, err := s.db.Exec(`INSERT INTO monitor_event_pushes (event_id, channel_id, status, attempts, last_error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(event_id, channel_id) DO UPDATE SET
			status = excluded.status, attempts = excluded.attempts,
			last_error = excluded.last_error, updated_at = excluded.updated_at`,
		push.EventID, push.ChannelID, push.Status, push.Attempts, push.LastError, monitorTime(push.UpdatedAt))
	return err
}

func (s *Store) ListMonitorEventPushes(eventID int64) ([]monitoring.EventPush, error) {
	rows, err := s.reader().Query(`SELECT event_id, channel_id, status, attempts, last_error, updated_at
		FROM monitor_event_pushes WHERE event_id = ? ORDER BY channel_id`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pushes := []monitoring.EventPush{}
	for rows.Next() {
		var push monitoring.EventPush
		var updatedAt string
		if err := rows.Scan(&push.EventID, &push.ChannelID, &push.Status, &push.Attempts, &push.LastError, &updatedAt); err != nil {
			return nil, err
		}
		push.UpdatedAt = parseMonitorTime(updatedAt)
		pushes = append(pushes, push)
	}
	return pushes, rows.Err()
}

func (s *Store) SettingsJSON(keys ...string) (map[string]string, error) {
	out := map[string]string{}
	if len(keys) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys))
	for _, key := range keys {
		args = append(args, key)
	}
	rows, err := s.reader().Query(`SELECT key, value FROM settings WHERE key IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, rows.Err()
}

func (s *Store) UpsertProviderMetricSample(sample monitoring.MetricSample) error {
	hour := strings.TrimSpace(sample.Hour)
	if hour == "" {
		hour = sample.ObservedAt.UTC().Truncate(time.Hour).Format(time.RFC3339Nano)
	}
	var quotaRatio, credits any
	if sample.QuotaRatio != nil {
		quotaRatio = *sample.QuotaRatio
	}
	if sample.Credits != nil {
		credits = *sample.Credits
	}
	_, err := s.db.Exec(`INSERT INTO provider_metric_samples (
			provider_id, hour, quota_ratio, credits, credit_valid_until, auth_expires_at,
			health_status, source_error, observed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id, hour) DO UPDATE SET
			quota_ratio = excluded.quota_ratio,
			credits = excluded.credits,
			credit_valid_until = excluded.credit_valid_until,
			auth_expires_at = excluded.auth_expires_at,
			health_status = excluded.health_status,
			source_error = excluded.source_error,
			observed_at = excluded.observed_at`,
		sample.ProviderID, hour, quotaRatio, credits, sample.CreditValidUntil,
		sample.AuthExpiresAt, sample.HealthStatus, sample.SourceError, monitorTime(sample.ObservedAt))
	return err
}

func (s *Store) LatestProviderMetricSamples() (map[string]monitoring.MetricSample, error) {
	rows, err := s.reader().Query(`SELECT provider_id, hour, quota_ratio, credits, credit_valid_until,
			auth_expires_at, health_status, source_error, observed_at
		FROM provider_metric_samples ORDER BY provider_id, hour DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]monitoring.MetricSample{}
	for rows.Next() {
		var (
			sample     monitoring.MetricSample
			quotaRatio sql.NullFloat64
			credits    sql.NullFloat64
			observedAt string
		)
		if err := rows.Scan(&sample.ProviderID, &sample.Hour, &quotaRatio, &credits,
			&sample.CreditValidUntil, &sample.AuthExpiresAt, &sample.HealthStatus,
			&sample.SourceError, &observedAt); err != nil {
			return nil, err
		}
		if _, seen := out[sample.ProviderID]; seen {
			continue
		}
		if quotaRatio.Valid {
			value := quotaRatio.Float64
			sample.QuotaRatio = &value
		}
		if credits.Valid {
			value := credits.Float64
			sample.Credits = &value
		}
		sample.ObservedAt = parseMonitorTime(observedAt)
		out[sample.ProviderID] = sample
	}
	return out, rows.Err()
}

const monitorTrafficFilter = `action IN ('convert', 'pass_through')
	AND status <> 499
	AND COALESCE(protocol_flow, '') <> 'embeddings'
	AND COALESCE(access_source, '') <> 'monitor_probe'`

func (s *Store) RecomputeTTFTBaselines(now time.Time, loc *time.Location) error {
	if loc == nil {
		loc = time.UTC
	}
	targetHour := now.In(loc).Hour()
	from := now.Add(-7 * 24 * time.Hour)
	rows, err := s.reader().Query(`SELECT provider_id, time, ttft_ms FROM request_logs
		WHERE time >= ? AND time <= ? AND ttft_ms > 0 AND `+monitorTrafficFilter,
		monitorTime(from), monitorTime(now))
	if err != nil {
		return err
	}
	defer rows.Close()
	samples := map[string][]int64{}
	for rows.Next() {
		var providerID, at string
		var ttft int64
		if err := rows.Scan(&providerID, &at, &ttft); err != nil {
			return err
		}
		if parseMonitorTime(at).In(loc).Hour() != targetHour {
			continue
		}
		samples[providerID] = append(samples[providerID], ttft)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	computedAt := monitorTime(now)
	for providerID, values := range samples {
		value := int64(0)
		if len(values) >= 20 {
			value = monitorPercentile(values, 0.95)
		}
		if _, err := s.db.Exec(`INSERT INTO monitor_baselines
			(provider_id, hour_of_day, metric, value, sample_count, computed_at)
			VALUES (?, ?, 'ttft_p95', ?, ?, ?)
			ON CONFLICT(provider_id, hour_of_day, metric) DO UPDATE SET
				value = excluded.value, sample_count = excluded.sample_count, computed_at = excluded.computed_at`,
			providerID, targetHour, float64(value), len(values), computedAt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) TTFTBaselines(loc *time.Location) (map[string]int64, error) {
	if loc == nil {
		loc = time.UTC
	}
	rows, err := s.reader().Query(`SELECT provider_id, value FROM monitor_baselines
		WHERE hour_of_day = ? AND metric = 'ttft_p95' AND value > 0`, time.Now().In(loc).Hour())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var providerID string
		var value float64
		if err := rows.Scan(&providerID, &value); err != nil {
			return nil, err
		}
		out[providerID] = int64(value)
	}
	return out, rows.Err()
}

type monitorWindowAccum struct {
	stats     monitoring.WindowStats
	ttfts     []int64
	latencies []int64
}

func (s *Store) WindowStatsByProvider(from, to time.Time) (map[string]map[int]monitoring.WindowStats, error) {
	rows, err := s.reader().Query(`SELECT provider_id, time, status, output_tokens, ttft_ms,
			latency_ms, cost_micros, error_description
		FROM request_logs
		WHERE time >= ? AND time <= ? AND `+monitorTrafficFilter,
		monitorTime(from), monitorTime(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	accums := map[string]map[int]*monitorWindowAccum{}
	for rows.Next() {
		var (
			providerID, at, errorDescription string
			status                           int
			outputTokens, ttft, latency      int64
			costMicros                       int64
		)
		if err := rows.Scan(&providerID, &at, &status, &outputTokens, &ttft, &latency,
			&costMicros, &errorDescription); err != nil {
			return nil, err
		}
		age := to.Sub(parseMonitorTime(at))
		if age < 0 {
			continue
		}
		for _, minutes := range []int{5, 10, 15} {
			if age > time.Duration(minutes)*time.Minute {
				continue
			}
			byWindow := accums[providerID]
			if byWindow == nil {
				byWindow = map[int]*monitorWindowAccum{}
				accums[providerID] = byWindow
			}
			accum := byWindow[minutes]
			if accum == nil {
				accum = &monitorWindowAccum{}
				byWindow[minutes] = accum
			}
			accum.stats.Total++
			accum.stats.CostMicros += costMicros
			if status >= 200 && status < 300 {
				if errorDescription == "" {
					accum.stats.Success++
					if outputTokens == 0 {
						accum.stats.Empty++
					}
				} else {
					accum.stats.Pseudo200++
				}
			}
			switch {
			case status == 401 || status == 403:
				accum.stats.Auth401403++
			case status == 429:
				accum.stats.RateLimited++
			case status >= 500:
				accum.stats.ServerError++
			case status == 0:
				lower := strings.ToLower(errorDescription)
				if strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline") {
					accum.stats.ServerError++
				}
			}
			if ttft > 0 {
				accum.ttfts = append(accum.ttfts, ttft)
			}
			if latency > 0 {
				accum.latencies = append(accum.latencies, latency)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := map[string]map[int]monitoring.WindowStats{}
	for providerID, byWindow := range accums {
		out[providerID] = map[int]monitoring.WindowStats{}
		for minutes, accum := range byWindow {
			stats := accum.stats
			stats.TTFTp50 = monitorPercentile(accum.ttfts, 0.50)
			stats.TTFTp95 = monitorPercentile(accum.ttfts, 0.95)
			stats.LatencyP95 = monitorPercentile(accum.latencies, 0.95)
			out[providerID][minutes] = stats
		}
	}
	return out, nil
}

func (s *Store) MonitorRequestSummary(from, to time.Time) (monitoring.RequestSummary, error) {
	rows, err := s.reader().Query(`SELECT status, error_description, latency_ms, cost_micros, priced
		FROM request_logs WHERE time >= ? AND time <= ? AND `+monitorTrafficFilter,
		monitorTime(from), monitorTime(to))
	if err != nil {
		return monitoring.RequestSummary{}, err
	}
	defer rows.Close()
	summary := monitoring.RequestSummary{}
	latencies := []int64{}
	for rows.Next() {
		var (
			status           int
			errorDescription string
			latency, cost    int64
			priced           int
		)
		if err := rows.Scan(&status, &errorDescription, &latency, &cost, &priced); err != nil {
			return summary, err
		}
		summary.Total++
		if status >= 200 && status < 300 && errorDescription == "" {
			summary.Success++
		}
		if latency > 0 {
			latencies = append(latencies, latency)
		}
		if priced != 0 {
			summary.Priced++
		}
		summary.CostMicros += cost
	}
	if err := rows.Err(); err != nil {
		return summary, err
	}
	summary.P95LatencyMs = monitorPercentile(latencies, 0.95)
	return summary, nil
}

// SaveCheckinHistory upserts one day's check-in history row.
//
// It is the standalone form of what checkin.go's SaveCheckin does inside the
// same transaction as the current-state update; production writes through that
// path. Kept as the documented single-row API (used by the migration tests) —
// a caller that uses it must also update `checkins`, or the two disagree.
func (s *Store) SaveCheckinHistory(record domain.CheckinRecord) error {
	offline := 0
	if record.Offline {
		offline = 1
	}
	_, err := s.db.Exec(`INSERT INTO checkin_history
		(provider_id, day, at, outcome, credit, streak, msg, valid_until, offline, tries)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id, day) DO UPDATE SET
			at = excluded.at, outcome = excluded.outcome, credit = excluded.credit,
			streak = excluded.streak, msg = excluded.msg, valid_until = excluded.valid_until,
			offline = excluded.offline, tries = excluded.tries`,
		record.ProviderID, record.Day, record.At, record.Outcome, record.Credit,
		record.Streak, record.Message, record.Until, offline, record.Tries)
	return err
}

// ListCheckinHistory reads the last days of check-in history for one provider,
// newest first. The console shows today's state from `checkins`; this is the
// history view behind it.
func (s *Store) ListCheckinHistory(providerID string, days int) ([]domain.CheckinRecord, error) {
	if days <= 0 {
		days = 30
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := s.reader().Query(`SELECT provider_id, day, at, outcome, credit, streak, msg,
			valid_until, offline, tries
		FROM checkin_history WHERE provider_id = ? AND day >= ? ORDER BY day DESC`, providerID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []domain.CheckinRecord{}
	for rows.Next() {
		var (
			record  domain.CheckinRecord
			offline int
		)
		if err := rows.Scan(&record.ProviderID, &record.Day, &record.At, &record.Outcome,
			&record.Credit, &record.Streak, &record.Message, &record.Until, &offline,
			&record.Tries); err != nil {
			return nil, err
		}
		record.Offline = offline != 0
		records = append(records, record)
	}
	return records, rows.Err()
}

// RecentRenewalOutcomes counts consecutive p0 renewal events walking backwards
// from the newest one; a success event resets the streak.
func (s *Store) RecentRenewalOutcomes(providerID string, limit int) (monitoring.RenewalOutcome, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.reader().Query(`SELECT severity, time FROM monitor_events
		WHERE kind = 'renewal' AND target_id = ? ORDER BY time DESC, id DESC LIMIT ?`,
		providerID, limit)
	if err != nil {
		return monitoring.RenewalOutcome{}, err
	}
	defer rows.Close()
	outcome := monitoring.RenewalOutcome{}
	for rows.Next() {
		var severity, at string
		if err := rows.Scan(&severity, &at); err != nil {
			return monitoring.RenewalOutcome{}, err
		}
		if !outcome.Known {
			outcome.Known = true
			outcome.LastAt = parseMonitorTime(at)
			outcome.LastOK = monitoring.Severity(severity) != monitoring.SeverityP0
		}
		if monitoring.Severity(severity) == monitoring.SeverityP0 {
			outcome.Consecutive++
		} else {
			break
		}
	}
	return outcome, rows.Err()
}

func monitorPercentile(values []int64, p float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(math.Ceil(p*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

// InsertMonitorProbeRun stores one probe case. Rows that were skipped are kept
// too: the daily budget and answers like "why is there no weekly deep check"
// both have to be explainable from the database alone.
func (s *Store) InsertMonitorProbeRun(record monitoring.ProbeRunRecord) error {
	return s.InsertMonitorProbeRuns([]monitoring.ProbeRunRecord{record})
}

// InsertMonitorProbeRuns writes one round in a single transaction. A round is
// several rows sharing one timestamp, and the "did this round already run"
// checks read them as a unit: a half-written round must not be visible.
func (s *Store) InsertMonitorProbeRuns(records []monitoring.ProbeRunRecord) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("insert probe runs: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, record := range records {
		if _, err := tx.Exec(`INSERT INTO monitor_probe_runs
			(provider_id, kind, case_key, ok, status, skipped, duration_ms, model, error_code, day, time)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			record.ProviderID, record.Kind, record.Case, boolToInt(record.OK), record.Status,
			record.Skipped, record.DurationMs, record.Model, record.ErrorCode, record.Day,
			record.Time.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("insert probe run: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("insert probe runs: %w", err)
	}
	return nil
}

// MonitorProbeRunningCase 是 CLI 深检「本轮已在跑」的占位行 case_key。占位行
// 不是一次探测：它不延长失败连续数、不占当日配额，探测页也不当作结果展示；
// 它唯一的用途是让周检的「本周已跑」在执行器返回之前就从库里成立（重启安全）。
const MonitorProbeRunningCasePrefix = "round:running:"

// MonitorProbeOutcome summarises one provider's probe history for the rule
// engine, plus how many probes were already spent on `day`. A skipped round
// (no model configured, CLI missing) is not a verdict, so it neither extends
// the failure streak nor resets it — only a run that actually reached the
// upstream does.
//
// 一轮轻探测落多行（chat / tools 各一行，同一轮的时间戳相同）。连续失败与当日
// 计数都按「轮」算：maxPerDayPerAccount 的语义是一次探测，不是一次上游请求，
// 否则默认 4 次会悄悄变成 2 轮。轮判定失败时，轮状态与鉴权标记跟失败 case 走，
// 否则「对话 200、tools 401」会被显示成正常。
func (s *Store) MonitorProbeOutcome(providerID string, day string) (monitoring.ProbeOutcome, int, error) {
	outcome := monitoring.ProbeOutcome{}
	rows, err := s.reader().Query(`SELECT kind, case_key, ok, status, skipped, time
		FROM monitor_probe_runs WHERE provider_id = ? AND case_key NOT LIKE '`+MonitorProbeRunningCasePrefix+`%'
		ORDER BY time DESC, id DESC LIMIT 50`, providerID)
	if err != nil {
		return outcome, 0, fmt.Errorf("read probe runs: %w", err)
	}
	defer rows.Close()
	streakBroken := false
	lastRound := ""
	for rows.Next() {
		var (
			kind, caseKey, skipped, stamp string
			ok                            int
			status                        int
		)
		if err := rows.Scan(&kind, &caseKey, &ok, &status, &skipped, &stamp); err != nil {
			return outcome, 0, fmt.Errorf("scan probe run: %w", err)
		}
		at := parseMonitorTime(stamp)
		if at.IsZero() {
			continue
		}
		if skipped != "" {
			continue
		}
		firstOfRound := stamp != lastRound
		inLatestRound := lastRound == ""
		lastRound = stamp
		if inLatestRound {
			outcome.Known = true
			outcome.LastAt = at
			outcome.LastStatus = status
			outcome.Skipped = ""
			outcome.AuthFailure = status == 401 || status == 403
		}
		if inLatestRound && ok == 0 {
			// 最近一轮里失败的 case 说了算：chat 过了、tools 401 时，轮状态是 401
			// 而不是开头的 200。更早的轮次不覆写最近结果。
			outcome.LastStatus = status
			if status == 401 || status == 403 {
				outcome.AuthFailure = true
			}
		}
		if !firstOfRound {
			continue
		}
		if streakBroken {
			continue
		}
		if ok != 0 {
			streakBroken = true
			continue
		}
		outcome.Consecutive++
	}
	if err := rows.Err(); err != nil {
		return outcome, 0, fmt.Errorf("iterate probe runs: %w", err)
	}
	// 当日配额按轮计，不按行。
	var used int
	if err := s.reader().QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM monitor_probe_runs
		WHERE provider_id = ? AND day = ? AND skipped = '' GROUP BY time)`,
		providerID, day).Scan(&used); err != nil {
		return outcome, 0, fmt.Errorf("count probe runs: %w", err)
	}
	return outcome, used, nil
}

// ReplaceMonitorProbeRun 在一段事务里删掉某账号某一轮时刻的占位行（以及本轮
// 可能残留的行），再写入正式结果。深检收尾用它把「running」占位换成真结果：
// 要么全换上，要么一行不动。
func (s *Store) ReplaceMonitorProbeRun(providerID, runningCase string, records []monitoring.ProbeRunRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("replace probe run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM monitor_probe_runs
		WHERE provider_id = ? AND case_key = ?`,
		providerID, runningCase); err != nil {
		return fmt.Errorf("replace probe run: %w", err)
	}
	for _, record := range records {
		if _, err := tx.Exec(`INSERT INTO monitor_probe_runs
			(provider_id, kind, case_key, ok, status, skipped, duration_ms, model, error_code, day, time)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			record.ProviderID, record.Kind, record.Case, boolToInt(record.OK), record.Status,
			record.Skipped, record.DurationMs, record.Model, record.ErrorCode, record.Day,
			record.Time.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("replace probe run: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace probe run: %w", err)
	}
	return nil
}

// LatestMonitorProbeRuns returns the most recent probe rows for one provider,
// newest first, for the probe page.
//
// 原先的按账号过滤把 providerID 填进两个占位、再配合空串判断，结果过滤条件恒不
// 成立（探测页最近结果一直是全量）。这里改成按需拼 WHERE 子句。
func (s *Store) LatestMonitorProbeRuns(providerID string, limit int) ([]monitoring.ProbeRunRecord, error) {
	return s.LatestMonitorProbeRunsOfKind("", providerID, limit)
}

// LatestMonitorProbeRunsOfKind narrows the listing to one kind (light | cli).
// The weekly deep check asks for its own kind only: a light run stamped with a
// later clock would otherwise push the CLI rows out of a limit-sized window and
// make "did this week run" answer wrongly.
func (s *Store) LatestMonitorProbeRunsOfKind(kind, providerID string, limit int) ([]monitoring.ProbeRunRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	// 用拼接而不是 (? = '' OR x = ?)：参数按位置绑定，同一个值填两个占位
	// 会让其中一个条件恒不成立（见 LatestMonitorProbeRuns 的说明）。kind /
	// providerID 只是加不加 WHERE 子句的选择。
	where := "1=1"
	args := make([]any, 0, 3)
	if providerID != "" {
		where += " AND provider_id = ?"
		args = append(args, providerID)
	}
	if kind != "" {
		where += " AND kind = ?"
		args = append(args, kind)
	}
	args = append(args, limit)
	rows, err := s.reader().Query(`SELECT provider_id, kind, case_key, ok, status, skipped, duration_ms, model, error_code, day, time
		FROM monitor_probe_runs WHERE `+where+`
		ORDER BY time DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("read latest probe runs: %w", err)
	}
	defer rows.Close()
	out := make([]monitoring.ProbeRunRecord, 0, limit)
	for rows.Next() {
		var (
			record monitoring.ProbeRunRecord
			ok     int
			stamp  string
		)
		if err := rows.Scan(&record.ProviderID, &record.Kind, &record.Case, &ok, &record.Status,
			&record.Skipped, &record.DurationMs, &record.Model, &record.ErrorCode, &record.Day, &stamp); err != nil {
			return nil, fmt.Errorf("scan latest probe runs: %w", err)
		}
		record.OK = ok != 0
		record.Time = parseMonitorTime(stamp)
		out = append(out, record)
	}
	return out, rows.Err()
}
