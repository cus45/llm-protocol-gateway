package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	// Embedded zone database: a slim container image without
	// /usr/share/zoneinfo would otherwise silently drop every named zone a
	// user picks, and "09:00 Asia/Shanghai" would fire on the server's clock.
	_ "time/tzdata"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// ScheduledTaskStore persists quota-keeper tasks and their run history.
type ScheduledTaskStore interface {
	ListScheduledTasks() ([]domain.ScheduledTask, error)
	ScheduledTaskByID(id string) (domain.ScheduledTask, error)
	SaveScheduledTask(task domain.ScheduledTask) error
	UpdateScheduledTaskSchedule(id, lastRunAt, nextRunAt string) error
	DeleteScheduledTask(id string) error
	AppendScheduledTaskRuns(runs []domain.ScheduledTaskRun) error
	ListScheduledTaskRuns(taskID string, limit int) ([]domain.ScheduledTaskRun, error)
	PruneScheduledTaskRuns() error
}

const (
	// quotaKeeperTick is deliberately much finer than the shortest schedule the
	// console offers: a slot named 09:00 should fire at 09:00, not "09:00 plus
	// however long the loop sleeps".
	quotaKeeperTick       = 20 * time.Second
	quotaKeeperStartDelay = 8 * time.Second
	// quotaProbeTimeout bounds one provider's probe so one hung upstream cannot
	// hold the task's in-flight claim (and therefore the next slot) open.
	quotaProbeTimeout     = 3 * time.Minute
	maxScheduledTaskTimes = 24
)

// serverTimeZoneLabel names the zone the server itself runs in, for the console
// to label tasks that left TimeZone empty. `time.Local.String()` is literally
// "Local" and useless to a human, so this resolves the usual sources: the TZ
// environment variable, the /etc/localtime symlink, and finally the numeric
// offset.
func serverTimeZoneLabel() string {
	if name := strings.TrimSpace(os.Getenv("TZ")); name != "" {
		if _, err := time.LoadLocation(name); err == nil {
			return name
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if index := strings.Index(target, "zoneinfo/"); index >= 0 {
			return target[index+len("zoneinfo/"):]
		}
	}
	_, offset := time.Now().Zone()
	sign := "+"
	minutes := offset / 60
	if minutes < 0 {
		sign = "-"
		minutes = -minutes
	}
	return fmt.Sprintf("UTC%s%02d:%02d", sign, minutes/60, minutes%60)
}

// quotaLocation is the zone a task's "HH:MM" list is read against. The gateway
// often runs somewhere the operator does not (a US VPS serving a Beijing user),
// so an empty zone is the only case that falls back to the server's own clock.
// An unresolvable name falls back to the server too, but the API refuses such a
// value on save, so this cannot silently shift a schedule.
func quotaLocation(task domain.ScheduledTask) *time.Location {
	name := strings.TrimSpace(task.TimeZone)
	if name == "" {
		return time.Local
	}
	if location, err := time.LoadLocation(name); err == nil {
		return location
	}
	if location, err := quotaFixedZone(name); err == nil {
		return location
	}
	return time.Local
}

// quotaFixedZone accepts the numeric forms a client may send ("+08:00",
// "UTC+8", "-7") so a zone can still be honoured on a host without a tz
// database.
func quotaFixedZone(name string) (*time.Location, error) {
	value := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(name), " ", ""))
	value = strings.TrimPrefix(value, "UTC")
	if value == "" || value == "Z" {
		return time.UTC, nil
	}
	sign := 1
	switch value[0] {
	case '+':
	case '-':
		sign = -1
		value = value[1:]
	default:
		return nil, fmt.Errorf("not a UTC offset: %q", name)
	}
	hours, minutes, err := parseHourMinute(value)
	if err != nil {
		return nil, err
	}
	if hours > 14 || minutes > 59 {
		return nil, fmt.Errorf("UTC offset out of range: %q", name)
	}
	label := "UTC+" + value
	if sign < 0 {
		label = "UTC-" + value
	}
	return time.FixedZone(label, sign*(hours*3600+minutes*60)), nil
}

func parseHourMinute(value string) (int, int, error) {
	parts := strings.Split(value, ":")
	if len(parts) > 2 {
		return 0, 0, fmt.Errorf("not a UTC offset: %q", value)
	}
	hours, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	minutes := 0
	if len(parts) == 2 {
		minutes, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, err
		}
	}
	return hours, minutes, nil
}

// quotaClockMinute parses "HH:MM" into minutes past local midnight.
func quotaClockMinute(spec string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(spec), ":")
	if len(parts) != 2 {
		return 0, false
	}
	hour, hourErr := strconv.Atoi(strings.TrimSpace(parts[0]))
	minute, minuteErr := strconv.Atoi(strings.TrimSpace(parts[1]))
	if hourErr != nil || minuteErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

// normalizeQuotaClocks canonicalises a task's time list: valid, deduplicated,
// ascending, in "HH:MM" form. Anything unparsable is dropped rather than
// rejecting the whole task, so a half-edited schedule still fires on what is
// left.
func normalizeQuotaClocks(times []string) []string {
	seen := map[int]struct{}{}
	unique := make([]int, 0, len(times))
	for _, spec := range times {
		minutes, ok := quotaClockMinute(spec)
		if !ok {
			continue
		}
		if _, dup := seen[minutes]; dup {
			continue
		}
		seen[minutes] = struct{}{}
		unique = append(unique, minutes)
	}
	sort.Ints(unique)
	out := make([]string, 0, len(unique))
	for _, minutes := range unique {
		out = append(out, fmt.Sprintf("%02d:%02d", minutes/60, minutes%60))
	}
	if len(out) > maxScheduledTaskTimes {
		out = out[:maxScheduledTaskTimes]
	}
	return out
}

// nextScheduledFire returns the first slot strictly after `from`.
//
// daily_times reads its clock list against the machine's local timezone, which
// is what "每天早上九点" means to the user. The rolling modes count fixed steps
// from AnchorAt, so anchoring at 09:00 with a 5-hour interval reproduces the
// upstream window chain (09:00 → 14:00 → 19:00) without the user doing the
// arithmetic.
func nextScheduledFire(task domain.ScheduledTask, from time.Time) (time.Time, bool) {
	switch task.Mode {
	case domain.ScheduleEveryHours, domain.ScheduleEveryMinutes:
		if task.Interval <= 0 {
			return time.Time{}, false
		}
		step := time.Duration(task.Interval) * time.Hour
		if task.Mode == domain.ScheduleEveryMinutes {
			step = time.Duration(task.Interval) * time.Minute
		}
		anchor, ok := quotaAnchor(task, from)
		if !ok {
			return time.Time{}, false
		}
		if anchor.After(from) {
			return anchor, true
		}
		steps := int64(from.Sub(anchor)/step) + 1
		return anchor.Add(time.Duration(steps) * step), true
	default:
		clocks := normalizeQuotaClocks(task.Times)
		if len(clocks) == 0 {
			return time.Time{}, false
		}
		zone := quotaLocation(task)
		local := from.In(zone)
		dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
		for _, spec := range clocks {
			minutes, _ := quotaClockMinute(spec)
			candidate := dayStart.Add(time.Duration(minutes) * time.Minute)
			if candidate.After(local) {
				return candidate, true
			}
		}
		firstMinutes, _ := quotaClockMinute(clocks[0])
		return dayStart.AddDate(0, 0, 1).Add(time.Duration(firstMinutes) * time.Minute), true
	}
}

// quotaAnchor is where the rolling modes count from: an explicit anchor, else
// the task's creation time, else `from` itself (a task with no recorded
// creation stamp starts its chain now).
func quotaAnchor(task domain.ScheduledTask, from time.Time) (time.Time, bool) {
	if anchor, ok := parseScheduleStamp(task.AnchorAt); ok {
		return anchor, true
	}
	if created, ok := parseScheduleStamp(task.CreatedAt); ok {
		return created, true
	}
	return from, true
}

func parseScheduleStamp(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if stamp, err := time.Parse(layout, value); err == nil {
			return stamp, true
		}
	}
	return time.Time{}, false
}

func formatScheduleStamp(at time.Time) string {
	return at.Format(time.RFC3339Nano)
}

// quotaSameSlot compares two stamps to the second. Stored values keep the
// sub-second fraction of the anchor they were computed from, so exact equality
// would flag a perfectly good slot as inconsistent after a timezone or
// monotonic-clock rounding hiccup.
func quotaSameSlot(left, right time.Time) bool {
	return left.Truncate(time.Second).Equal(right.Truncate(time.Second))
}

// quotaUpcomingFires lists the next `count` slots; the console shows these so a
// schedule can be checked before it is saved.
func quotaUpcomingFires(task domain.ScheduledTask, from time.Time, count int) []time.Time {
	out := []time.Time{}
	cursor := from
	for len(out) < count {
		next, ok := nextScheduledFire(task, cursor)
		if !ok {
			break
		}
		out = append(out, next)
		cursor = next
	}
	return out
}

// quotaQuietWindow is how recently real traffic must have touched a provider
// for the probe to stand down. Firing inside that window would push the
// upstream reset further out instead of anchoring it, which is the one way this
// feature can hurt.
func quotaQuietWindow(task domain.ScheduledTask) (time.Duration, bool) {
	if task.QuietMinutes < 0 {
		return 0, false
	}
	minutes := task.QuietMinutes
	if minutes == 0 {
		minutes = domain.DefaultQuotaQuietMinutes
	}
	if minutes > 24*60 {
		minutes = 24 * 60
	}
	return time.Duration(minutes) * time.Minute, true
}

func (s *Server) scheduledTaskStoreReady() bool {
	return s.scheduledTaskStore != nil
}

// StartQuotaKeeper runs the due slots of every enabled task. It ticks far
// finer than any schedule it serves, so a task created or edited at any moment
// starts firing on its own grid without a restart.
func (s *Server) StartQuotaKeeper(ctx context.Context) {
	if !s.scheduledTaskStoreReady() {
		return
	}
	go func() {
		timer := time.NewTimer(quotaKeeperStartDelay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				s.runDueScheduledTasks(context.Background())
				s.pruneScheduledTaskRunsOnceADay()
				timer.Reset(quotaKeeperTick)
			}
		}
	}()
}

func (s *Server) runDueScheduledTasks(ctx context.Context) {
	tasks, err := s.scheduledTaskStore.ListScheduledTasks()
	if err != nil {
		slog.Warn("quota keeper: list tasks failed", "error", err)
		return
	}
	now := time.Now()
	for _, task := range tasks {
		if !task.Enabled || len(task.ProviderIDs) == 0 {
			continue
		}
		slot, ok := parseScheduleStamp(task.NextRunAt)
		if !ok {
			// First sight of this task (or its schedule changed underneath us):
			// adopt the next slot and wait for it rather than firing now.
			s.advanceScheduledTask(task, now, task.LastRunAt)
			continue
		}
		if slot.After(now) {
			// A future slot must still be one the current schedule produces. A
			// batch or an advance that read the task before the console edited it
			// can write back a stamp computed from the old times, and the console
			// would then show "下次触发 05:00" on a task whose list is 09:00 —
			// a time that cannot exist. Self-heal instead of trusting the stamp.
			if expected, ok := nextScheduledFire(task, now); ok && !quotaSameSlot(expected, slot) {
				s.advanceScheduledTask(task, now, task.LastRunAt)
			}
			continue
		}
		if now.Sub(slot) > time.Duration(domain.QuotaMissGraceMinutes)*time.Minute {
			// The gateway was down through the slot. Firing hours late would
			// re-anchor the whole day at the wrong hour, so skip to the next one.
			s.advanceScheduledTask(task, now, task.LastRunAt)
			continue
		}
		if !s.claimScheduledTaskRun(task.ID) {
			continue
		}
		go s.runScheduledTaskBatch(ctx, task, slot, false)
	}
}

// advanceScheduledTask stores the next slot (and an unchanged last-run stamp)
// for a task that did not fire in this tick.
func (s *Server) advanceScheduledTask(task domain.ScheduledTask, from time.Time, lastRunAt string) {
	next, ok := nextScheduledFire(task, from)
	if !ok {
		return
	}
	if err := s.scheduledTaskStore.UpdateScheduledTaskSchedule(task.ID, lastRunAt, formatScheduleStamp(next)); err != nil {
		slog.Warn("quota keeper: advance schedule failed", "task", task.ID, "error", err)
	}
}

// claimScheduledTaskRun keeps one firing per task: a batch of several providers
// can outlive a tick, and a manual run must not overlap the scheduled one.
func (s *Server) claimScheduledTaskRun(taskID string) bool {
	s.quotaKeeperMu.Lock()
	defer s.quotaKeeperMu.Unlock()
	if s.quotaTaskInFlight == nil {
		s.quotaTaskInFlight = map[string]bool{}
	}
	if s.quotaTaskInFlight[taskID] {
		return false
	}
	s.quotaTaskInFlight[taskID] = true
	return true
}

func (s *Server) releaseScheduledTaskRun(taskID string) {
	s.quotaKeeperMu.Lock()
	defer s.quotaKeeperMu.Unlock()
	delete(s.quotaTaskInFlight, taskID)
}

// runScheduledTaskBatch pokes every provider of the task in order. force skips
// the quiet-window check, which is what the "立即执行" button means; the
// scheduler itself never forces.
func (s *Server) runScheduledTaskBatch(ctx context.Context, task domain.ScheduledTask, slot time.Time, force bool) {
	defer s.releaseScheduledTaskRun(task.ID)
	batchID := randomHex(8)
	started := time.Now()
	var fired, skipped, failed int
	for _, providerID := range task.ProviderIDs {
		run := s.probeProviderForQuota(ctx, task, providerID, force)
		run.TaskID = task.ID
		run.BatchID = batchID
		if run.FiredAt == "" {
			run.FiredAt = formatScheduleStamp(time.Now())
		}
		if err := s.scheduledTaskStore.AppendScheduledTaskRuns([]domain.ScheduledTaskRun{run}); err != nil {
			slog.Warn("quota keeper: append run failed", "task", task.ID, "provider", providerID, "error", err)
		}
		switch run.Outcome {
		case domain.QuotaRunFired:
			fired++
		case domain.QuotaRunSkipped:
			skipped++
		default:
			failed++
		}
	}
	lastRunAt := formatScheduleStamp(started)
	nextRunAt := task.NextRunAt
	if !force {
		if next, ok := nextScheduledFire(task, slot); ok {
			nextRunAt = formatScheduleStamp(next)
		}
	}
	if err := s.scheduledTaskStore.UpdateScheduledTaskSchedule(task.ID, lastRunAt, nextRunAt); err != nil {
		slog.Warn("quota keeper: persist schedule failed", "task", task.ID, "error", err)
	}
	summary := fmt.Sprintf("task=%s batch=%s fired=%d skipped=%d failed=%d providers=%d", task.ID, batchID, fired, skipped, failed, len(task.ProviderIDs))
	if failed > 0 {
		s.logs.AddApp("warn", "quota keeper fired with failures", summary)
		return
	}
	s.logs.AddApp("info", "quota keeper fired", summary)
}

// probeProviderForQuota sends one throwaway chat through the console's own
// chat-test path, so every auth type (OAuth, subscriptions, plain keys) keeps
// working here without a second forwarding implementation.
func (s *Server) probeProviderForQuota(ctx context.Context, task domain.ScheduledTask, providerID string, force bool) domain.ScheduledTaskRun {
	run := domain.ScheduledTaskRun{ProviderID: providerID}
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		run.Outcome = domain.QuotaRunFailed
		run.Reason = "Provider 不存在或已删除"
		return run
	}
	if provider.Disabled {
		run.Outcome = domain.QuotaRunSkipped
		run.Reason = "Provider 已禁用"
		return run
	}
	// A task belongs to whoever made it, so the grant that let them schedule it
	// still governs it: revoking the provider in 用户管理 must stop the
	// scheduled traffic too, otherwise a revoked or deleted user's task keeps
	// poking that provider forever.
	if s.quotaOwnerLostAccess(task.OwnerUserID, providerID) {
		run.Outcome = domain.QuotaRunSkipped
		run.Reason = "该 Provider 已不再授权给任务创建者"
		return run
	}
	if !s.claimQuotaProbe(providerID) {
		run.Outcome = domain.QuotaRunSkipped
		run.Reason = "同一 Provider 正在探测中"
		return run
	}
	defer s.releaseQuotaProbe(providerID)

	window, quietEnabled := quotaQuietWindow(task)
	if quietEnabled && !force {
		if usedAt, source, busy := s.providerRecentlyUsed(providerID, window); busy {
			run.Outcome = domain.QuotaRunSkipped
			run.Reason = fmt.Sprintf("%d 分钟内已有%s（%s）", int(window.Minutes()), source, usedAt.In(time.Local).Format("15:04:05"))
			return run
		}
	}

	model := quotaProbeModel(task, provider)
	if model == "" {
		run.Outcome = domain.QuotaRunFailed
		run.Reason = "Provider 未配置模型，请在任务里指定"
		return run
	}
	prompt := strings.TrimSpace(task.ProbePrompt)
	if prompt == "" {
		prompt = domain.DefaultQuotaProbePrompt
	}
	probeCtx, cancel := context.WithTimeout(ctx, quotaProbeTimeout)
	defer cancel()
	req, err := quotaProbeHTTPRequest(probeCtx, providerID, providerChatTestRequest{Model: model, UserPrompt: prompt, Message: prompt})
	if err != nil {
		run.Outcome = domain.QuotaRunFailed
		run.Reason = err.Error()
		return run
	}
	started := time.Now()
	result, _ := s.testProviderChat(req, providerID, providerChatTestRequest{Model: model, UserPrompt: prompt, Message: prompt}, started)
	s.recordQuotaProbe(providerID, started)

	run.Model = mapStringValue(result, "model")
	if run.Model == "" {
		run.Model = model
	}
	run.Status = mapIntValue(result, "status")
	run.LatencyMs = mapInt64Value(result, "latencyMs")
	success, _ := result["success"].(bool)
	if success {
		run.Outcome = domain.QuotaRunFired
		return run
	}
	run.Outcome = domain.QuotaRunFailed
	if message := mapStringValue(result, "error"); message != "" {
		run.Reason = truncateRunReason(message)
	} else {
		run.Reason = fmt.Sprintf("上游返回 HTTP %d", run.Status)
	}
	return run
}

func truncateRunReason(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\n", " "))
	if len(value) > 300 {
		return value[:300]
	}
	return value
}

// quotaProbeModel prefers the task's pin, then the provider default, then the
// first model in the menu. An empty result means there is nothing sane to ask
// for, and the caller records that instead of sending a bogus model name.
func quotaProbeModel(task domain.ScheduledTask, provider domain.Provider) string {
	if model := strings.TrimSpace(task.ModelOverrides[provider.ID]); model != "" {
		return model
	}
	if model := strings.TrimSpace(provider.DefaultModel); model != "" {
		return model
	}
	for _, model := range provider.Models {
		if model.InMenu && strings.TrimSpace(model.ID) != "" {
			return strings.TrimSpace(model.ID)
		}
	}
	return ""
}

func quotaProbeHTTPRequest(ctx context.Context, providerID string, req providerChatTestRequest) (*http.Request, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "/__providers/"+providerID+"/chat-test", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	return request, nil
}

// claimQuotaProbe serialises probes per provider across tasks: two tasks that
// both list the same provider at the same minute would otherwise send two
// requests and move that provider's reset twice.
func (s *Server) claimQuotaProbe(providerID string) bool {
	s.quotaKeeperMu.Lock()
	defer s.quotaKeeperMu.Unlock()
	if s.quotaProbeInFlight == nil {
		s.quotaProbeInFlight = map[string]bool{}
	}
	if s.quotaProbeInFlight[providerID] {
		return false
	}
	s.quotaProbeInFlight[providerID] = true
	return true
}

func (s *Server) releaseQuotaProbe(providerID string) {
	s.quotaKeeperMu.Lock()
	defer s.quotaKeeperMu.Unlock()
	delete(s.quotaProbeInFlight, providerID)
}

func (s *Server) recordQuotaProbe(providerID string, at time.Time) {
	s.quotaKeeperMu.Lock()
	defer s.quotaKeeperMu.Unlock()
	if s.quotaLastProbeAt == nil {
		s.quotaLastProbeAt = map[string]time.Time{}
	}
	s.quotaLastProbeAt[providerID] = at
}

// providerRecentlyUsed answers whether anything touched this provider inside
// the window, and what that was. Console chat tests never reach request_logs,
// so the table holds real traffic only; the probes this loop sent are tracked
// in memory instead. Naming the source matters in the run history: "上一发探测
// 才 40 秒" and "用户正在用这个 Provider" are different reasons to stand down.
func (s *Server) providerRecentlyUsed(providerID string, window time.Duration) (time.Time, string, bool) {
	if window <= 0 {
		return time.Time{}, "", false
	}
	cutoff := time.Now().Add(-window)
	s.quotaKeeperMu.Lock()
	probeAt := s.quotaLastProbeAt[providerID]
	s.quotaKeeperMu.Unlock()
	if !probeAt.IsZero() && probeAt.After(cutoff) {
		return probeAt, "保活探测", true
	}
	if s.requestLogStore == nil {
		return time.Time{}, "", false
	}
	page, err := s.requestLogStore.QueryRequestLogs(monitor.RequestLogQuery{
		ProviderID: providerID,
		From:       cutoff,
		Page:       1,
		PageSize:   1,
	})
	if err != nil {
		slog.Warn("quota keeper: quiet-window query failed", "provider", providerID, "error", err)
		return time.Time{}, "", false
	}
	if len(page.Items) == 0 {
		return time.Time{}, "", false
	}
	return page.Items[0].Time, "真实流量", true
}

func (s *Server) pruneScheduledTaskRunsOnceADay() {
	s.quotaKeeperMu.Lock()
	defer s.quotaKeeperMu.Unlock()
	if !s.quotaLastPruneAt.IsZero() && time.Since(s.quotaLastPruneAt) < 24*time.Hour {
		return
	}
	s.quotaLastPruneAt = time.Now()
	if err := s.scheduledTaskStore.PruneScheduledTaskRuns(); err != nil {
		slog.Warn("quota keeper: prune run history failed", "error", err)
	}
}

// quotaOwnerLostAccess reports whether the task's owner is a normal console
// user who may no longer use the provider. Admin-owned tasks (including the
// legacy "admin" id and ownerless tasks that predate multi-user) are never
// restricted; a disabled or deleted owner's schedules go quiet instead of
// outliving the account.
func (s *Server) quotaOwnerLostAccess(ownerUserID, providerID string) bool {
	owner := strings.TrimSpace(ownerUserID)
	if owner == "" || owner == legacyAdminUserID || s.userStore == nil {
		return false
	}
	user, err := s.userStore.UserByID(owner)
	if err != nil {
		return true
	}
	if user.Role == domain.UserRoleAdmin {
		return false
	}
	if !user.Enabled {
		return true
	}
	return !s.allowedProviderIDsForUser(owner)[strings.TrimSpace(providerID)]
}

func mapStringValue(result map[string]any, key string) string {
	value, _ := result[key].(string)
	return strings.TrimSpace(value)
}

func mapIntValue(result map[string]any, key string) int {
	switch value := result[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	}
	return 0
}

func mapInt64Value(result map[string]any, key string) int64 {
	switch value := result[key].(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	}
	return 0
}
