package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

func newQuotaTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSaveScheduledTaskRoundTrip(t *testing.T) {
	t.Parallel()
	s := newQuotaTestStore(t)
	task := domain.ScheduledTask{
		ID:             "task-1",
		Name:           "早九点锚定",
		Enabled:        true,
		OwnerUserID:    "user-1",
		Mode:           domain.ScheduleDailyTimes,
		Times:          []string{"09:00", "15:00", "23:00"},
		ProviderIDs:    []string{"claude-a", "claude-b"},
		ModelOverrides: map[string]string{"claude-a": "claude-opus-4-1"},
		QuietMinutes:   45,
		ProbePrompt:    "ping",
		CreatedAt:      "2026-10-16T08:00:00Z",
		NextRunAt:      "2026-10-16T09:00:00Z",
	}
	if err := s.SaveScheduledTask(task); err != nil {
		t.Fatal(err)
	}
	got, err := s.ScheduledTaskByID("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != task.Name || !got.Enabled || got.OwnerUserID != "user-1" {
		t.Fatalf("scalar fields: %+v", got)
	}
	if got.Mode != domain.ScheduleDailyTimes {
		t.Fatalf("mode = %q", got.Mode)
	}
	if len(got.Times) != 3 || got.Times[2] != "23:00" {
		t.Fatalf("times = %v", got.Times)
	}
	if len(got.ProviderIDs) != 2 || got.ProviderIDs[0] != "claude-a" {
		t.Fatalf("providers = %v", got.ProviderIDs)
	}
	if got.ModelOverrides["claude-a"] != "claude-opus-4-1" {
		t.Fatalf("overrides = %v", got.ModelOverrides)
	}
	if got.QuietMinutes != 45 || got.ProbePrompt != "ping" || got.NextRunAt != task.NextRunAt {
		t.Fatalf("tail fields: %+v", got)
	}
}

func TestSaveScheduledTaskReplacesInPlace(t *testing.T) {
	t.Parallel()
	s := newQuotaTestStore(t)
	task := domain.ScheduledTask{ID: "task-1", Name: "第一版", Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"}, ProviderIDs: []string{"a", "b"}}
	if err := s.SaveScheduledTask(task); err != nil {
		t.Fatal(err)
	}
	task.Name = "第二版"
	task.Enabled = true
	task.Mode = domain.ScheduleEveryHours
	task.Interval = 5
	task.Times = nil
	task.ProviderIDs = []string{"a"}
	if err := s.SaveScheduledTask(task); err != nil {
		t.Fatal(err)
	}
	tasks, err := s.ListScheduledTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("want one row after upsert, got %d", len(tasks))
	}
	got := tasks[0]
	if got.Name != "第二版" || !got.Enabled || got.Mode != domain.ScheduleEveryHours || got.Interval != 5 {
		t.Fatalf("upsert kept stale fields: %+v", got)
	}
	if len(got.Times) != 0 {
		t.Fatalf("times not cleared: %v", got.Times)
	}
	if len(got.ProviderIDs) != 1 || got.ProviderIDs[0] != "a" {
		t.Fatalf("providers = %v", got.ProviderIDs)
	}
}

func TestUpdateScheduledTaskScheduleLeavesConfigAlone(t *testing.T) {
	t.Parallel()
	s := newQuotaTestStore(t)
	if err := s.SaveScheduledTask(domain.ScheduledTask{
		ID: "task-1", Name: "保活", ProviderIDs: []string{"a", "b"}, Mode: domain.ScheduleEveryMinutes, Interval: 10,
	}); err != nil {
		t.Fatal(err)
	}
	// The console edits the task while the keeper is mid-batch.
	if err := s.SaveScheduledTask(domain.ScheduledTask{
		ID: "task-1", Name: "保活改名", ProviderIDs: []string{"a"}, Mode: domain.ScheduleEveryMinutes, Interval: 30,
	}); err != nil {
		t.Fatal(err)
	}
	// The keeper then reports the firing it just completed.
	if err := s.UpdateScheduledTaskSchedule("task-1", "2026-10-16T09:00:00Z", "2026-10-16T09:30:00Z"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ScheduledTaskByID("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "保活改名" || got.Interval != 30 || len(got.ProviderIDs) != 1 {
		t.Fatalf("schedule write clobbered the config: %+v", got)
	}
	if got.LastRunAt != "2026-10-16T09:00:00Z" || got.NextRunAt != "2026-10-16T09:30:00Z" {
		t.Fatalf("timestamps not stored: %+v", got)
	}
}

func TestDeleteScheduledTaskRemovesRuns(t *testing.T) {
	t.Parallel()
	s := newQuotaTestStore(t)
	if err := s.SaveScheduledTask(domain.ScheduledTask{ID: "task-1", Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveScheduledTask(domain.ScheduledTask{ID: "task-2", Name: "y"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendScheduledTaskRuns([]domain.ScheduledTaskRun{
		{TaskID: "task-1", ProviderID: "a", BatchID: "b1", FiredAt: "2026-10-16T09:00:00Z", Outcome: domain.QuotaRunFired},
		{TaskID: "task-2", ProviderID: "a", BatchID: "b2", FiredAt: "2026-10-16T09:00:00Z", Outcome: domain.QuotaRunFired},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteScheduledTask("task-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ScheduledTaskByID("task-1"); err == nil {
		t.Fatal("task-1 still readable")
	}
	runs, err := s.ListScheduledTaskRuns("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].TaskID != "task-2" {
		t.Fatalf("orphan runs survived: %+v", runs)
	}
}

func TestListScheduledTaskRunsOrdersAndFilters(t *testing.T) {
	t.Parallel()
	s := newQuotaTestStore(t)
	earlier := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	later := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if err := s.AppendScheduledTaskRuns([]domain.ScheduledTaskRun{
		{TaskID: "task-1", ProviderID: "a", BatchID: "b1", FiredAt: earlier, Outcome: domain.QuotaRunFired, Status: 200, LatencyMs: 812, Model: "claude-sonnet-4-5"},
		{TaskID: "task-1", ProviderID: "b", BatchID: "b1", FiredAt: earlier, Outcome: domain.QuotaRunSkipped, Reason: "quiet window"},
		{TaskID: "task-1", ProviderID: "c", BatchID: "b2", FiredAt: later, Outcome: domain.QuotaRunFailed, Status: 429, Reason: "quota exhausted"},
	}); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListScheduledTaskRuns("", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 rows, got %d", len(all))
	}
	if all[0].BatchID != "b2" {
		t.Fatalf("newest firing not first: %+v", all[0])
	}
	if all[0].ID == 0 {
		t.Fatalf("run id not returned: %+v", all[0])
	}
	if all[1].Reason != "quiet window" || all[1].Outcome != domain.QuotaRunSkipped {
		t.Fatalf("skip row: %+v", all[1])
	}
	if all[2].LatencyMs != 812 || all[2].Model != "claude-sonnet-4-5" {
		t.Fatalf("fired row: %+v", all[2])
	}
	onlyTask2, err := s.ListScheduledTaskRuns("task-2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyTask2) != 0 {
		t.Fatalf("filter leaked other tasks: %+v", onlyTask2)
	}
}

func TestPruneScheduledTaskRuns(t *testing.T) {
	t.Parallel()
	s := newQuotaTestStore(t)
	old := time.Now().UTC().AddDate(0, 0, -60).Format(time.RFC3339Nano)
	fresh := time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.AppendScheduledTaskRuns([]domain.ScheduledTaskRun{
		{TaskID: "task-1", ProviderID: "a", FiredAt: old, Outcome: domain.QuotaRunFired},
		{TaskID: "task-1", ProviderID: "a", FiredAt: fresh, Outcome: domain.QuotaRunFired},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneScheduledTaskRuns(); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListScheduledTaskRuns("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].FiredAt != fresh {
		t.Fatalf("prune kept the wrong rows: %+v", runs)
	}
	// Pruning an empty/short table must stay harmless.
	if err := s.PruneScheduledTaskRuns(); err != nil {
		t.Fatal(err)
	}
}
