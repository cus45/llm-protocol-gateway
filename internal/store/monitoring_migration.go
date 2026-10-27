package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitoring"
)

const legacyTelegramChannelID = "legacy-telegram"

// MigrateMonitoringV1 copies the pre-monitoring tables into the monitoring
// schema. Check-in history and the Telegram channel migrate once; legacy
// alerts are imported incrementally on every start so rows the old scanner
// writes during phase 1 still land in the event stream.
func (s *Store) MigrateMonitoringV1() error {
	cfg := monitoring.NewConfig(s)
	marker := cfg.MigrationMarker()

	if !marker.Checkins {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO checkin_history
			(provider_id, day, at, outcome, credit, streak, msg, valid_until, offline, tries)
			SELECT provider_id, day, at, outcome, credit, streak, msg, valid_until, offline, tries
			FROM checkins WHERE day != ''`); err != nil {
			return fmt.Errorf("migrate checkin history: %w", err)
		}
		marker.Checkins = true
		if err := cfg.SaveMigrationMarker(marker); err != nil {
			return err
		}
	}

	if !marker.Telegram {
		if err := s.migrateLegacyTelegram(cfg); err != nil {
			return err
		}
		marker.Telegram = true
		if err := cfg.SaveMigrationMarker(marker); err != nil {
			return err
		}
	}

	rows, err := s.reader().Query(`SELECT id, time, rule, severity, api_key_id, api_key_name,
			ips, ip_count, window_minutes, request_count, status, push_status, push_error, concurrent_at
		FROM alerts WHERE id > ? ORDER BY id`, marker.LegacyMaxAlertID)
	if err != nil {
		return fmt.Errorf("read legacy alerts: %w", err)
	}
	legacy := []legacyAlert{}
	for rows.Next() {
		var (
			alert        legacyAlert
			when         string
			ips          sql.NullString
			pushStatus   sql.NullString
			pushError    sql.NullString
			concurrentAt sql.NullString
		)
		if err := rows.Scan(&alert.ID, &when, &alert.Rule, &alert.Severity, &alert.APIKeyID,
			&alert.APIKeyName, &ips, &alert.IPCount, &alert.WindowMinutes, &alert.RequestCount,
			&alert.Status, &pushStatus, &pushError, &concurrentAt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan legacy alert: %w", err)
		}
		alert.Time = parseMonitorTime(when)
		alert.IPs = []string{}
		if raw := strings.TrimSpace(ips.String); raw != "" {
			_ = json.Unmarshal([]byte(raw), &alert.IPs)
			if alert.IPs == nil {
				alert.IPs = []string{}
			}
		}
		alert.PushStatus = pushStatus.String
		alert.PushError = pushError.String
		alert.ConcurrentAt = parseMonitorTime(concurrentAt.String)
		legacy = append(legacy, alert)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("scan legacy alerts: %w", err)
	}
	_ = rows.Close()

	for _, alert := range legacy {
		if err := s.importLegacyAlert(alert); err != nil {
			return err
		}
		if alert.ID > marker.LegacyMaxAlertID {
			marker.LegacyMaxAlertID = alert.ID
		}
	}
	if !marker.Alerts {
		marker.Alerts = true
	}
	return cfg.SaveMigrationMarker(marker)
}

type legacyAlert struct {
	ID            int64
	Time          time.Time
	Rule          string
	Severity      string
	APIKeyID      string
	APIKeyName    string
	IPs           []string
	IPCount       int
	WindowMinutes int
	RequestCount  int
	Status        string
	PushStatus    string
	PushError     string
	ConcurrentAt  time.Time
}

func (s *Store) importLegacyAlert(alert legacyAlert) error {
	var existing int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM monitor_events
		WHERE kind = 'alert' AND target_type = 'api_key'
		  AND json_extract(detail_json, '$.legacyAlertId') = ?`, alert.ID).Scan(&existing); err != nil {
		return fmt.Errorf("check legacy alert %d: %w", alert.ID, err)
	}
	if existing > 0 {
		return nil
	}

	ruleKey := alert.Rule
	switch alert.Rule {
	case "apikey_multi_ip":
		ruleKey = "multi_ip"
	case "apikey_concurrent_ip":
		ruleKey = "concurrent_ip"
	}
	severity := monitoring.SeverityP1
	if alert.Severity == "error" || alert.Severity == "critical" {
		severity = monitoring.SeverityP0
	}
	title := "历史告警 " + alert.Rule
	switch ruleKey {
	case "multi_ip":
		title = "API Key 多 IP 使用"
	case "concurrent_ip":
		title = "API Key 并发 IP"
	}
	detail := map[string]any{
		"legacyAlertId": alert.ID,
		"ips":           alert.IPs,
		"ipCount":       alert.IPCount,
		"windowMinutes": alert.WindowMinutes,
		"requestCount":  alert.RequestCount,
		"legacyStatus":  alert.Status,
		"concurrentAt":  monitorTime(alert.ConcurrentAt),
	}
	if ruleKey == alert.Rule && alert.Rule != "multi_ip" && alert.Rule != "concurrent_ip" {
		detail["legacyRule"] = true
	}
	pushStatus := monitoring.PushSkipped
	switch alert.PushStatus {
	case monitoring.PushSent:
		pushStatus = monitoring.PushSent
	case monitoring.PushFailed:
		pushStatus = monitoring.PushFailed
	}
	at := alert.Time
	if at.IsZero() {
		at = time.Now()
	}
	_, err := s.InsertMonitorEvent(monitoring.Event{
		Time: at, Kind: monitoring.EventKindAlert, Severity: severity, RuleKey: ruleKey,
		TargetType: monitoring.TargetAPIKey, TargetID: alert.APIKeyID, TargetName: alert.APIKeyName,
		Title: title, Detail: detail, Status: monitoring.StatusResolved, Count: 1,
		FirstSeen: at, LastSeen: at, ResolvedAt: at,
		ResolutionReason: monitoring.ResolutionLegacy,
		PushStatus:       pushStatus, PushError: alert.PushError,
	})
	if err != nil {
		return fmt.Errorf("import legacy alert %d: %w", alert.ID, err)
	}
	return nil
}

func (s *Store) migrateLegacyTelegram(cfg *monitoring.Config) error {
	raw := strings.TrimSpace(s.Setting("alerts"))
	if raw == "" {
		return nil
	}
	settings := domain.DefaultAlertSettings()
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return nil
	}
	if strings.TrimSpace(settings.Telegram.BotToken) == "" || strings.TrimSpace(settings.Telegram.ChatID) == "" {
		return nil
	}
	channels := cfg.Channels()
	for _, channel := range channels {
		if channel.ID == legacyTelegramChannelID {
			return nil
		}
	}
	channels = append(channels, monitoring.Channel{
		ID: legacyTelegramChannelID, Type: "telegram", Name: "Telegram（旧配置）",
		Config: monitoring.ChannelConfig{
			BotToken: settings.Telegram.BotToken,
			ChatID:   settings.Telegram.ChatID,
		},
		MinSeverity: monitoring.SeverityP1,
		Enabled:     settings.Telegram.Enabled,
	})
	return cfg.SaveChannels(channels)
}

func (s *Store) PruneMonitoringData(now time.Time) error {
	return s.PruneMonitoringDataContext(context.Background(), now)
}

// PruneMonitoringDataContext drops history past its retention window, in bounded
// batches (see deleteInBatches: one statement wiping a big table would own the
// single writer connection past a shutdown deadline). monitor_events has no
// foreign key to pushes, so orphaned push rows are removed explicitly.
func (s *Store) PruneMonitoringDataContext(ctx context.Context, now time.Time) error {
	passes := []struct {
		statement string
		args      []any
	}{
		{`DELETE FROM monitor_events WHERE id IN (
				SELECT id FROM monitor_events WHERE time < ? ORDER BY time LIMIT ?)`,
			[]any{monitorTime(now.AddDate(0, 0, -90)), pruneBatchRows}},
		{`DELETE FROM monitor_event_pushes WHERE rowid IN (
				SELECT rowid FROM monitor_event_pushes
				WHERE event_id NOT IN (SELECT id FROM monitor_events) LIMIT ?)`,
			[]any{pruneBatchRows}},
		{`DELETE FROM provider_metric_samples WHERE rowid IN (
				SELECT rowid FROM provider_metric_samples WHERE hour < ? ORDER BY hour LIMIT ?)`,
			[]any{monitorTime(now.AddDate(0, 0, -30)), pruneBatchRows}},
		{`DELETE FROM monitor_probe_runs WHERE rowid IN (
				SELECT rowid FROM monitor_probe_runs WHERE time < ? ORDER BY time LIMIT ?)`,
			[]any{monitorTime(now.AddDate(0, 0, -30)), pruneBatchRows}},
		{`DELETE FROM checkin_history WHERE rowid IN (
				SELECT rowid FROM checkin_history WHERE day < ? ORDER BY day LIMIT ?)`,
			[]any{now.AddDate(0, 0, -90).UTC().Format("2006-01-02"), pruneBatchRows}},
		// 探测记录比事件短：它每天最多几条、只用来回答「昨天到底通不通」，
		// 30 天足够复盘，再久就是白占写放大。
		{`DELETE FROM monitor_alert_states WHERE rowid IN (
				SELECT rowid FROM monitor_alert_states
				WHERE status = 'resolved' AND last_seen < ?
				  AND id NOT IN (SELECT id FROM monitor_alert_states
				                 WHERE status = 'resolved' ORDER BY last_seen DESC LIMIT 1000)
				LIMIT ?)`,
			[]any{monitorTime(now.AddDate(0, 0, -90)), pruneBatchRows}},
	}
	for _, step := range passes {
		if _, err := s.deleteInBatches(ctx, step.statement, step.args...); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			// Stopped on purpose (shutdown); the next pass continues. Not a failure.
			return nil
		}
	}
	return nil
}
