package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitoring"
)

const (
	maxScheduledProviders = 100
	maxQuotaPromptChars   = 200
	quotaPreviewCount     = 6
	quotaRunHistoryLimit  = 50
)

// requireQuotaUser gates the keeper endpoints for any signed-in console user.
// The page lives in 核心配置 and normal users configure their own tasks, so
// this is deliberately not requireAdmin; what a caller may touch is decided by
// task ownership below, and which providers they may schedule by the grant
// check in validateScheduledTaskProviders.
func requireQuotaUser(w http.ResponseWriter, r *http.Request) (sessionIdentity, bool) {
	identity, ok := identityFromRequest(r)
	if !ok {
		writeOpenAIError(w, http.StatusUnauthorized, "authentication required")
		return sessionIdentity{}, false
	}
	return identity, true
}

// tasksVisibleTo filters tasks per role: admins see every task, a normal user
// sees only the ones they created.
func tasksVisibleTo(identity sessionIdentity, tasks []domain.ScheduledTask) []domain.ScheduledTask {
	if identity.isAdmin() {
		return tasks
	}
	out := make([]domain.ScheduledTask, 0, len(tasks))
	for _, task := range tasks {
		if strings.TrimSpace(task.OwnerUserID) == identity.UserID {
			out = append(out, task)
		}
	}
	return out
}

// scheduledTaskForCaller loads a task and refuses one owned by somebody else.
// A foreign task answers 404, not 403: confirming it exists would leak which
// schedules other users run.
func (s *Server) scheduledTaskForCaller(w http.ResponseWriter, r *http.Request, id string) (domain.ScheduledTask, bool) {
	identity := s.requestIdentity(r)
	task, err := s.scheduledTaskStore.ScheduledTaskByID(id)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return domain.ScheduledTask{}, false
	}
	if !identity.isAdmin() && strings.TrimSpace(task.OwnerUserID) != identity.UserID {
		writeOpenAIError(w, http.StatusNotFound, "scheduled task not found")
		return domain.ScheduledTask{}, false
	}
	return task, true
}

// validateScheduledTaskProviders rejects providers the caller has not been
// granted. Admins are unrestricted; a normal user may only schedule against the
// same providers they can see and test on the Providers page.
func (s *Server) validateScheduledTaskProviders(identity sessionIdentity, task domain.ScheduledTask) error {
	if identity.isAdmin() {
		return nil
	}
	allowed := s.allowedProviderIDsForUser(identity.UserID)
	for _, providerID := range task.ProviderIDs {
		if !allowed[strings.TrimSpace(providerID)] {
			return fmt.Errorf("provider %q 未授权给你的账号", providerID)
		}
	}
	return nil
}

// handleListScheduledTasks returns the caller's tasks plus the most recent
// firings, so the page renders in one request. Admins additionally get the
// owner-name table the card needs to show whose task it is.
func (s *Server) handleListScheduledTasks(w http.ResponseWriter, r *http.Request) {
	identity, ok := requireQuotaUser(w, r)
	if !ok {
		return
	}
	if !s.scheduledTaskStoreReady() {
		writeOpenAIError(w, http.StatusServiceUnavailable, "quota keeper store is not configured")
		return
	}
	all, err := s.scheduledTaskStore.ListScheduledTasks()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tasks := tasksVisibleTo(identity, all)
	for i := range tasks {
		// A task the keeper paused after every provider was deleted stores an
		// empty list; the console maps providerIds directly, so keep the field
		// an array instead of letting the nil slice serialize as null.
		if tasks[i].ProviderIDs == nil {
			tasks[i].ProviderIDs = []string{}
		}
	}
	runs, err := s.quotaVisibleRuns(identity, tasks)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	response := map[string]any{"tasks": tasks, "recentRuns": runs, "serverTimeZone": serverTimeZoneLabel()}
	response["providerQuota"] = s.quotaTaskProviderQuota(identity, tasks)
	if identity.isAdmin() {
		response["ownerNames"] = s.quotaOwnerNames(tasks)
	}
	writeJSON(w, http.StatusOK, response)
}

// quotaTaskProviderQuota hands the task cards the last real quota read for the
// providers they schedule. The card used to derive "窗口重置" from nextRunAt +
// 5h, which is a schedule guess; Claude reports a real resets_at per window and
// that is what the operator is looking at when they wonder why a fire did not
// move the number.
func (s *Server) quotaTaskProviderQuota(identity sessionIdentity, tasks []domain.ScheduledTask) map[string]monitoring.QuotaSnapshot {
	out := map[string]monitoring.QuotaSnapshot{}
	if s.monitoringStore == nil {
		return out
	}
	// Only providers the caller can already see: a revoked grant must not leak
	// that account's usage through a task they made before the revocation.
	visible := map[string]bool{}
	granted := map[string]bool{}
	if !identity.isAdmin() {
		granted = s.allowedProviderIDsForUser(identity.UserID)
	}
	for _, task := range tasks {
		for _, providerID := range task.ProviderIDs {
			if identity.isAdmin() || granted[providerID] {
				visible[providerID] = true
			}
		}
	}
	if len(visible) == 0 {
		return out
	}
	samples, err := s.monitoringStore.LatestProviderMetricSamples()
	if err != nil {
		return out
	}
	now := time.Now().UTC()
	staleAfter := time.Duration(monitoring.NewConfig(s.monitoringStore).Probe().QuotaStaleMinutes) * time.Minute
	for providerID := range visible {
		sample, found := samples[providerID]
		if !found {
			continue
		}
		snapshot := monitorQuotaFromSample(sample, staleAfter, now)
		// Only a sample that names its windows is useful here; the card falls
		// back to its own estimate for vendors without per-window usage.
		if snapshot == nil || len(snapshot.Windows) == 0 {
			continue
		}
		out[providerID] = *snapshot
	}
	return out
}

// quotaVisibleRuns returns recent firings for the tasks the caller may see.
// Admins get the global page straight from the store; a normal user's rows are
// gathered per task, so another tenant's traffic can neither be read nor
// crowd out their own history by filling the shared page.
func (s *Server) quotaVisibleRuns(identity sessionIdentity, tasks []domain.ScheduledTask) ([]domain.ScheduledTaskRun, error) {
	if identity.isAdmin() {
		return s.scheduledTaskStore.ListScheduledTaskRuns("", quotaRunHistoryLimit)
	}
	merged := []domain.ScheduledTaskRun{}
	for _, task := range tasks {
		runs, err := s.scheduledTaskStore.ListScheduledTaskRuns(task.ID, quotaRunHistoryLimit)
		if err != nil {
			return nil, err
		}
		merged = append(merged, runs...)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		left, leftErr := time.Parse(time.RFC3339Nano, merged[i].FiredAt)
		right, rightErr := time.Parse(time.RFC3339Nano, merged[j].FiredAt)
		if leftErr != nil || rightErr != nil {
			return merged[i].ID > merged[j].ID
		}
		if !left.Equal(right) {
			return left.After(right)
		}
		return merged[i].ID > merged[j].ID
	})
	if len(merged) > quotaRunHistoryLimit {
		merged = merged[:quotaRunHistoryLimit]
	}
	return merged, nil
}

// quotaOwnerNames maps the distinct task owners to display names. Only admins
// ever receive it, so a normal user's payload carries no other identities.
func (s *Server) quotaOwnerNames(tasks []domain.ScheduledTask) map[string]string {
	names := map[string]string{}
	for _, task := range tasks {
		owner := strings.TrimSpace(task.OwnerUserID)
		if owner == "" {
			continue
		}
		if _, seen := names[owner]; !seen {
			names[owner] = s.resolveUserName(owner)
		}
	}
	return names
}

// scheduledTaskPayload lets an omitted "enabled" mean "true" on create instead
// of quietly storing a paused task. The pointer shadows the embedded bool for
// decoding; responses still marshal the domain type.
type scheduledTaskPayload struct {
	domain.ScheduledTask
	Enabled *bool `json:"enabled"`
}

func (p scheduledTaskPayload) task(defaultEnabled bool) domain.ScheduledTask {
	task := p.ScheduledTask
	if p.Enabled != nil {
		task.Enabled = *p.Enabled
	} else {
		task.Enabled = defaultEnabled
	}
	return task
}

func (s *Server) handleCreateScheduledTask(w http.ResponseWriter, r *http.Request) {
	identity, ok := requireQuotaUser(w, r)
	if !ok {
		return
	}
	if !s.scheduledTaskStoreReady() {
		writeOpenAIError(w, http.StatusServiceUnavailable, "quota keeper store is not configured")
		return
	}
	var payload scheduledTaskPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid task json: "+err.Error())
		return
	}
	task := payload.task(true)
	if id := strings.TrimSpace(task.ID); id != "" {
		if _, err := s.scheduledTaskStore.ScheduledTaskByID(id); err == nil {
			writeOpenAIError(w, http.StatusConflict, "已存在同名 id 的任务")
			return
		}
	} else {
		task.ID = "quota-" + randomHex(5)
	}
	now := time.Now()
	if strings.TrimSpace(task.CreatedAt) == "" {
		task.CreatedAt = formatScheduleStamp(now)
	}
	// The task belongs to whoever created it, admin included: that is what makes
	// a normal user's list contain only their own schedules.
	task.OwnerUserID = strings.TrimSpace(identity.UserID)
	if err := normalizeScheduledTask(&task); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.validateScheduledTaskProviders(identity, task); err != nil {
		writeOpenAIError(w, http.StatusForbidden, err.Error())
		return
	}
	// 已删除的 Provider 只影响「现在能不能探测」，不把它从列表里删掉：软删除可以
	// 恢复，恢复后成员还在，任务自己会复活。彻底清除的 id 留着就是永久死 id，
	// 新建时直接剪掉。
	if live, purged := s.liveScheduledProviderIDs(task.ProviderIDs); len(purged) > 0 {
		if len(live) == 0 {
			writeOpenAIError(w, http.StatusBadRequest, "所选 Provider 都已删除，请重新选择")
			return
		}
		task.ProviderIDs = live
	}
	if _, schedulable := s.schedulableProviderIDs(task); len(schedulable) == len(task.ProviderIDs) {
		writeOpenAIError(w, http.StatusBadRequest, "所选 Provider 都已删除，请重新选择")
		return
	}
	task.PausedBy = scheduledTaskPauseMark(task.Enabled)
	task.NextRunAt = quotaNextStamp(task, now)
	if err := s.scheduledTaskStore.SaveScheduledTask(task); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// handleUpdateScheduledTask replaces a task's configuration. ID, owner and
// created stamp are taken from the stored row, never from the body, so a stale
// editor tab cannot rewrite identity fields.
func (s *Server) handleUpdateScheduledTask(w http.ResponseWriter, r *http.Request) {
	identity, ok := requireQuotaUser(w, r)
	if !ok {
		return
	}
	if !s.scheduledTaskStoreReady() {
		writeOpenAIError(w, http.StatusServiceUnavailable, "quota keeper store is not configured")
		return
	}
	existing, allowed := s.scheduledTaskForCaller(w, r, strings.TrimSpace(r.PathValue("id")))
	if !allowed {
		return
	}
	var payload scheduledTaskPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid task json: "+err.Error())
		return
	}
	// An edit that omits "enabled" keeps whatever the stored row says; a body
	// never rewrites identity or history fields.
	task := payload.task(existing.Enabled)
	task.ID = existing.ID
	task.OwnerUserID = existing.OwnerUserID
	task.CreatedAt = existing.CreatedAt
	if strings.TrimSpace(task.AnchorAt) == "" {
		task.AnchorAt = existing.AnchorAt
	}
	if err := normalizeScheduledTask(&task); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.validateScheduledTaskProviders(identity, task); err != nil {
		writeOpenAIError(w, http.StatusForbidden, err.Error())
		return
	}
	// 编辑页可能还列着已删除的 Provider。软删除的保留在列表里（恢复后自动复活），
	// 彻底清除的静默剪掉而不是让这次保存失败；但如果一个能调度的都不剩，就必须
	// 让用户重新选。
	if live, purged := s.liveScheduledProviderIDs(task.ProviderIDs); len(purged) > 0 {
		if len(live) == 0 {
			writeOpenAIError(w, http.StatusBadRequest, "所选 Provider 都已删除，请重新选择")
			return
		}
		task.ProviderIDs = live
	}
	if _, schedulable := s.schedulableProviderIDs(task); len(schedulable) == len(task.ProviderIDs) {
		writeOpenAIError(w, http.StatusBadRequest, "所选 Provider 都已删除，请重新选择")
		return
	}
	// Re-anchoring the schedule is the point of an edit: "改成 10 点" should not
	// keep waiting on a next-run stamp computed for the old times. A disabled
	// task stores nothing, so re-enabling recomputes from that moment.
	// A switch-off here is the user's: mark it so the keeper does not revive the
	// task on its next tick. Leaving the flag alone keeps the keeper's own pause
	// mark, so a restored provider still brings its task back.
	if task.Enabled != existing.Enabled {
		task.PausedBy = scheduledTaskPauseMark(task.Enabled)
	} else {
		task.PausedBy = existing.PausedBy
	}
	task.NextRunAt = quotaNextStamp(task, time.Now())
	task.LastRunAt = existing.LastRunAt
	if err := s.scheduledTaskStore.SaveScheduledTask(task); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (s *Server) handleDeleteScheduledTask(w http.ResponseWriter, r *http.Request) {
	_, ok := requireQuotaUser(w, r)
	if !ok {
		return
	}
	if !s.scheduledTaskStoreReady() {
		writeOpenAIError(w, http.StatusServiceUnavailable, "quota keeper store is not configured")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if _, allowed := s.scheduledTaskForCaller(w, r, id); !allowed {
		return
	}
	if err := s.scheduledTaskStore.DeleteScheduledTask(id); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleRunScheduledTask fires a task immediately, ignoring the quiet window —
// that is what an explicit press means — and answers before the batch runs so
// the console can watch the rows arrive.
func (s *Server) handleRunScheduledTask(w http.ResponseWriter, r *http.Request) {
	_, ok := requireQuotaUser(w, r)
	if !ok {
		return
	}
	if !s.scheduledTaskStoreReady() {
		writeOpenAIError(w, http.StatusServiceUnavailable, "quota keeper store is not configured")
		return
	}
	task, allowed := s.scheduledTaskForCaller(w, r, strings.TrimSpace(r.PathValue("id")))
	if !allowed {
		return
	}
	// 手动执行只读判断，不改库：软删除的成员留着（恢复后自动复活），这里只决定
	// 这一轮能不能发；没有任何可调度的成员才报错。
	if live, purged := s.liveScheduledProviderIDs(task.ProviderIDs); len(purged) > 0 {
		if len(live) == 0 {
			writeOpenAIError(w, http.StatusBadRequest, "任务里的 Provider 都已删除，请先从回收站恢复或编辑任务重新选择")
			return
		}
		task.ProviderIDs = live
	}
	schedulable, _ := s.schedulableProviderIDs(task)
	if len(schedulable) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "任务里的 Provider 都已删除，请先从回收站恢复或编辑任务重新选择")
		return
	}
	task.ProviderIDs = schedulable
	if !s.claimScheduledTaskRun(task.ID) {
		writeOpenAIError(w, http.StatusConflict, "该任务正在执行中，请等待本轮结束")
		return
	}
	go s.runScheduledTaskBatch(context.Background(), task, time.Now(), true)
	s.logs.AddApp("info", "quota keeper manual run", fmt.Sprintf("task=%s providers=%d", task.ID, len(task.ProviderIDs)))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"started":       true,
		"taskId":        task.ID,
		"providerCount": len(task.ProviderIDs),
	})
}

func (s *Server) handleListScheduledTaskRuns(w http.ResponseWriter, r *http.Request) {
	_, ok := requireQuotaUser(w, r)
	if !ok {
		return
	}
	if !s.scheduledTaskStoreReady() {
		writeOpenAIError(w, http.StatusServiceUnavailable, "quota keeper store is not configured")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	limit := quotaRunHistoryLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if _, allowed := s.scheduledTaskForCaller(w, r, id); !allowed {
		return
	}
	runs, err := s.scheduledTaskStore.ListScheduledTaskRuns(id, limit)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// handlePreviewScheduledTask answers "when would this fire?" before anything is
// saved, which is the only way a user can tell a 5-hour rolling chain from a
// daily list without waiting a day for it.
func (s *Server) handlePreviewScheduledTask(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireQuotaUser(w, r); !ok {
		return
	}
	var task domain.ScheduledTask
	if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid task json: "+err.Error())
		return
	}
	if err := normalizeScheduledTaskSchedule(&task); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := time.Now()
	fires := quotaUpcomingFires(task, now, quotaPreviewCount)
	zone := quotaLocation(task)
	out := make([]map[string]any, 0, len(fires))
	for _, fire := range fires {
		local := fire.In(zone)
		out = append(out, map[string]any{
			"at":    formatScheduleStamp(fire),
			"daily": local.Format("2006-01-02"),
			"clock": local.Format("15:04"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"fires": out, "timeZone": task.TimeZone})
}

// normalizeScheduledTask canonicalises what the console sent and rejects the
// configurations that could not fire: no times, no interval, no providers.
func normalizeScheduledTask(task *domain.ScheduledTask) error {
	if err := normalizeScheduledTaskSchedule(task); err != nil {
		return err
	}
	task.ProviderIDs = dedupeNonEmptyStrings(task.ProviderIDs)
	if len(task.ProviderIDs) == 0 {
		return fmt.Errorf("请至少选择一个 Provider")
	}
	if len(task.ProviderIDs) > maxScheduledProviders {
		task.ProviderIDs = task.ProviderIDs[:maxScheduledProviders]
	}
	overrides := map[string]string{}
	for providerID, model := range task.ModelOverrides {
		providerID = strings.TrimSpace(providerID)
		model = strings.TrimSpace(model)
		if providerID == "" || model == "" {
			continue
		}
		overrides[providerID] = model
	}
	if len(overrides) == 0 {
		overrides = nil
	}
	task.ModelOverrides = overrides
	if task.QuietMinutes > 24*60 {
		task.QuietMinutes = 24 * 60
	}
	prompt := []rune(strings.TrimSpace(task.ProbePrompt))
	if len(prompt) > maxQuotaPromptChars {
		prompt = prompt[:maxQuotaPromptChars]
	}
	task.ProbePrompt = string(prompt)
	task.AnchorAt = strings.TrimSpace(task.AnchorAt)
	return nil
}

// normalizeScheduledTaskSchedule checks only what the firing times depend on.
// The preview endpoint runs on this alone, so the console can show "接下来会在
// 09:00 / 14:00 / 19:00 触发" before a single provider has been picked.
func normalizeScheduledTaskSchedule(task *domain.ScheduledTask) error {
	task.Name = strings.TrimSpace(task.Name)
	if task.Name == "" {
		task.Name = "未命名保活任务"
	}
	if task.Mode == "" {
		task.Mode = domain.ScheduleDailyTimes
	}
	switch task.Mode {
	case domain.ScheduleDailyTimes:
		times := normalizeQuotaClocks(task.Times)
		if len(times) == 0 {
			return fmt.Errorf("每天定点模式需要至少一个 HH:MM 时间")
		}
		task.Times = times
		task.Interval = 0
	case domain.ScheduleEveryHours:
		if task.Interval < 1 || task.Interval > 24*30 {
			return fmt.Errorf("滚动小时数需要在 1 到 720 之间")
		}
		task.Times = nil
	case domain.ScheduleEveryMinutes:
		if task.Interval < 1 || task.Interval > 24*60 {
			return fmt.Errorf("滚动分钟数需要在 1 到 1440 之间")
		}
		task.Times = nil
	default:
		return fmt.Errorf("未知的调度策略：%s", task.Mode)
	}
	task.AnchorAt = strings.TrimSpace(task.AnchorAt)
	return normalizeScheduledTaskZone(task)
}

// normalizeScheduledTaskZone validates the zone name instead of letting
// quotaLocation quietly fall back to the server's clock: a typo like
// "Asia/Shangai" would otherwise fire the task hours off with no signal.
func normalizeScheduledTaskZone(task *domain.ScheduledTask) error {
	name := strings.TrimSpace(task.TimeZone)
	if name == "" {
		task.TimeZone = ""
		return nil
	}
	if _, err := time.LoadLocation(name); err == nil {
		task.TimeZone = name
		return nil
	}
	if _, err := quotaFixedZone(name); err == nil {
		task.TimeZone = name
		return nil
	}
	return fmt.Errorf("无法识别的时区 %q（请用 Asia/Shanghai 这类名称，或 +08:00）", name)
}

// quotaNextStamp is the schedule the task will actually follow; a disabled task
// stores nothing so re-enabling it recomputes from that moment.
func quotaNextStamp(task domain.ScheduledTask, from time.Time) string {
	if !task.Enabled {
		return ""
	}
	next, ok := nextScheduledFire(task, from)
	if !ok {
		return ""
	}
	return formatScheduleStamp(next)
}

func dedupeNonEmptyStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, dup := seen[value]; dup {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
