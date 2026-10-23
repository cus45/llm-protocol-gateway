package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// localClock builds a stamp in the machine's local zone, which is the zone the
// daily_times mode reads its "HH:MM" list against.
func localClock(t *testing.T, date, clock string) time.Time {
	t.Helper()
	stamp, err := time.ParseInLocation("2006-01-02 15:04:05", date+" "+clock+":00", time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return stamp
}

func TestNextScheduledFireDailyTimes(t *testing.T) {
	task := domain.ScheduledTask{
		Mode:  domain.ScheduleDailyTimes,
		Times: []string{"23:00", "09:00", "14:00", "09:00", "junk"},
	}
	cases := []struct {
		name string
		from string
		want string
	}{
		{"before first slot", "08:00", "2026-10-16 09:00"},
		{"exactly on a slot counts the next", "09:00", "2026-10-16 14:00"},
		{"mid day", "15:30", "2026-10-16 23:00"},
		{"after last slot rolls to tomorrow", "23:30", "2026-10-17 09:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from := localClock(t, "2026-10-16", tc.from)
			next, ok := nextScheduledFire(task, from)
			if !ok {
				t.Fatal("no next fire")
			}
			if got := next.In(time.Local).Format("2006-01-02 15:04"); got != tc.want {
				t.Fatalf("next = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestNextScheduledFireDailyTimesInvalidList(t *testing.T) {
	if _, ok := nextScheduledFire(domain.ScheduledTask{Mode: domain.ScheduleDailyTimes}, time.Now()); ok {
		t.Fatal("empty time list must not schedule")
	}
	task := domain.ScheduledTask{Mode: domain.ScheduleDailyTimes, Times: []string{"25:00", "", "12:60"}}
	if _, ok := nextScheduledFire(task, time.Now()); ok {
		t.Fatal("unparsable time list must not schedule")
	}
}

func TestNextScheduledFireEveryHoursReproducesTheWindowChain(t *testing.T) {
	anchor := localClock(t, "2026-10-16", "09:00")
	task := domain.ScheduledTask{
		Mode:     domain.ScheduleEveryHours,
		Interval: 5,
		AnchorAt: anchor.Format(time.RFC3339Nano),
	}
	got := []string{}
	cursor := anchor.Add(-time.Hour)
	for index := 0; index < 4; index++ {
		next, ok := nextScheduledFire(task, cursor)
		if !ok {
			t.Fatal("no next fire")
		}
		got = append(got, next.In(time.Local).Format("15:04"))
		cursor = next
	}
	// Anchored at 09:00, the chain is 09:00, 14:00, 19:00, 00:00 — the five-hour
	// reset the upstream meter would compute from the same first request.
	want := []string{"09:00", "14:00", "19:00", "00:00"}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("chain = %v, want %v", got, want)
		}
	}
	// One minute before the boundary still resolves to the boundary, and a slot
	// that has just passed is skipped rather than re-fired.
	justBefore := localClock(t, "2026-10-16", "13:59")
	if next, _ := nextScheduledFire(task, justBefore); next.In(time.Local).Format("15:04") != "14:00" {
		t.Fatalf("13:59 → %s, want 14:00", next)
	}
	justAfter := localClock(t, "2026-10-16", "14:01")
	if next, _ := nextScheduledFire(task, justAfter); next.In(time.Local).Format("15:04") != "19:00" {
		t.Fatalf("14:01 → %s, want 19:00", next)
	}
}

func TestNextScheduledFireEveryMinutesAndInvalidIntervals(t *testing.T) {
	created := localClock(t, "2026-10-16", "10:00")
	task := domain.ScheduledTask{Mode: domain.ScheduleEveryMinutes, Interval: 10, CreatedAt: created.Format(time.RFC3339Nano)}
	next, ok := nextScheduledFire(task, created)
	if !ok {
		t.Fatal("no next fire")
	}
	if got := next.In(time.Local).Format("15:04"); got != "10:10" {
		t.Fatalf("every 10 minutes from 10:00 = %s, want 10:10", got)
	}
	if _, ok := nextScheduledFire(domain.ScheduledTask{Mode: domain.ScheduleEveryHours, Interval: 0}, created); ok {
		t.Fatal("zero interval must not schedule")
	}
	if _, ok := nextScheduledFire(domain.ScheduledTask{Mode: domain.ScheduleMode("nonsense")}, created); ok {
		t.Fatal("unknown mode must fall through to the daily list, which is empty here")
	}
}

func TestQuotaUpcomingFiresWalksTheWholeDay(t *testing.T) {
	task := domain.ScheduledTask{Mode: domain.ScheduleDailyTimes, Times: []string{"09:00", "15:00", "23:00"}}
	from := localClock(t, "2026-10-16", "10:00")
	fires := quotaUpcomingFires(task, from, 4)
	if len(fires) != 4 {
		t.Fatalf("got %d fires", len(fires))
	}
	want := []string{"15:00", "23:00", "09:00", "15:00"}
	for index, fire := range fires {
		if got := fire.In(time.Local).Format("15:04"); got != want[index] {
			t.Fatalf("fire %d = %s, want %s", index, got, want[index])
		}
	}
	// A day later than the first fire means the list rolled over midnight.
	if fires[2].In(time.Local).Day() != 17 {
		t.Fatalf("third fire should be tomorrow: %s", fires[2])
	}
}

func TestQuotaQuietWindow(t *testing.T) {
	cases := []struct {
		minutes    int
		wantWindow time.Duration
		wantOn     bool
	}{
		{-1, 0, false},
		{0, time.Hour, true},
		{30, 30 * time.Minute, true},
		{99999, 24 * time.Hour, true},
	}
	for _, tc := range cases {
		window, enabled := quotaQuietWindow(domain.ScheduledTask{QuietMinutes: tc.minutes})
		if window != tc.wantWindow || enabled != tc.wantOn {
			t.Fatalf("quiet %d → %v/%v, want %v/%v", tc.minutes, window, enabled, tc.wantWindow, tc.wantOn)
		}
	}
}

func TestQuotaProbeModelPrefersPinThenDefaultThenMenu(t *testing.T) {
	provider := domain.Provider{
		ID:           "p1",
		DefaultModel: "default-model",
		Models:       []domain.Model{{ID: "menu-1", InMenu: true}, {ID: "hidden", InMenu: false}},
	}
	if got := quotaProbeModel(domain.ScheduledTask{ModelOverrides: map[string]string{"p1": "pinned"}}, provider); got != "pinned" {
		t.Fatalf("pinned = %q", got)
	}
	if got := quotaProbeModel(domain.ScheduledTask{}, provider); got != "default-model" {
		t.Fatalf("default = %q", got)
	}
	provider.DefaultModel = ""
	if got := quotaProbeModel(domain.ScheduledTask{}, provider); got != "menu-1" {
		t.Fatalf("menu = %q", got)
	}
	provider.Models = []domain.Model{{ID: "hidden", InMenu: false}}
	if got := quotaProbeModel(domain.ScheduledTask{}, provider); got != "" {
		t.Fatalf("nothing selectable, got %q", got)
	}
}

func TestNormalizeScheduledTask(t *testing.T) {
	t.Run("valid daily list is canonicalised", func(t *testing.T) {
		task := &domain.ScheduledTask{
			Name:           "  早九点  ",
			Mode:           domain.ScheduleDailyTimes,
			Times:          []string{"9:00", "09:00", " 14:30 ", "bad"},
			ProviderIDs:    []string{"p1", " p1 ", "", "p2"},
			ModelOverrides: map[string]string{"p1": " m1 ", "": "skip"},
			QuietMinutes:   3000,
			ProbePrompt:    strings.Repeat("长", maxQuotaPromptChars+10),
		}
		if err := normalizeScheduledTask(task); err != nil {
			t.Fatal(err)
		}
		if task.Name != "早九点" {
			t.Fatalf("name = %q", task.Name)
		}
		if strings.Join(task.Times, ",") != "09:00,14:30" {
			t.Fatalf("times = %v", task.Times)
		}
		if strings.Join(task.ProviderIDs, ",") != "p1,p2" {
			t.Fatalf("providers = %v", task.ProviderIDs)
		}
		if task.ModelOverrides["p1"] != "m1" || len(task.ModelOverrides) != 1 {
			t.Fatalf("overrides = %v", task.ModelOverrides)
		}
		if task.QuietMinutes != 24*60 {
			t.Fatalf("quiet = %d", task.QuietMinutes)
		}
		if len([]rune(task.ProbePrompt)) != maxQuotaPromptChars {
			t.Fatalf("prompt length = %d", len([]rune(task.ProbePrompt)))
		}
		if task.Interval != 0 {
			t.Fatalf("daily mode must clear the interval, got %d", task.Interval)
		}
	})

	cases := []struct {
		name string
		task domain.ScheduledTask
	}{
		{"no times", domain.ScheduledTask{Mode: domain.ScheduleDailyTimes, ProviderIDs: []string{"p1"}}},
		{"no providers", domain.ScheduledTask{Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"}}},
		{"interval out of range", domain.ScheduledTask{Mode: domain.ScheduleEveryHours, Interval: 0, ProviderIDs: []string{"p1"}}},
		{"minute interval out of range", domain.ScheduledTask{Mode: domain.ScheduleEveryMinutes, Interval: 2000, ProviderIDs: []string{"p1"}}},
		{"unknown mode", domain.ScheduledTask{Mode: domain.ScheduleMode("weekly"), Times: []string{"09:00"}, ProviderIDs: []string{"p1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := tc.task
			if err := normalizeScheduledTask(&task); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestQuotaNextStampHonoursEnabled(t *testing.T) {
	task := domain.ScheduledTask{Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"}, Enabled: true}
	if stamp := quotaNextStamp(task, localClock(t, "2026-10-16", "08:00")); stamp == "" {
		t.Fatal("enabled task must get a next stamp")
	}
	task.Enabled = false
	if stamp := quotaNextStamp(task, time.Now()); stamp != "" {
		t.Fatalf("disabled task must store nothing, got %q", stamp)
	}
}

// fakeScheduledTaskStore is the keeper's view of the database: it records what
// the loop wrote and can release a batch signal to the test.
type fakeScheduledTaskStore struct {
	mu      sync.Mutex
	tasks   map[string]domain.ScheduledTask
	runs    []domain.ScheduledTaskRun
	persist chan string
}

func newFakeScheduledTaskStore() *fakeScheduledTaskStore {
	return &fakeScheduledTaskStore{tasks: map[string]domain.ScheduledTask{}, persist: make(chan string, 8)}
}

func (f *fakeScheduledTaskStore) ListScheduledTasks() ([]domain.ScheduledTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []domain.ScheduledTask{}
	for _, task := range f.tasks {
		out = append(out, task)
	}
	return out, nil
}

func (f *fakeScheduledTaskStore) ScheduledTaskByID(id string) (domain.ScheduledTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	task, ok := f.tasks[id]
	if !ok {
		return domain.ScheduledTask{}, errQuotaTaskNotFound
	}
	return task, nil
}

// errQuotaTaskNotFound mirrors the store's wording so a test failure reads the
// same way it would against the real database.
var errQuotaTaskNotFound = errors.New("scheduled task not found")

func (f *fakeScheduledTaskStore) SaveScheduledTask(task domain.ScheduledTask) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[task.ID] = task
	return nil
}

func (f *fakeScheduledTaskStore) UpdateScheduledTaskSchedule(id, lastRunAt, nextRunAt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	task, ok := f.tasks[id]
	if !ok {
		return nil
	}
	task.LastRunAt, task.NextRunAt = lastRunAt, nextRunAt
	f.tasks[id] = task
	f.persist <- id
	return nil
}

func (f *fakeScheduledTaskStore) UpdateScheduledTaskProviders(id string, providerIDs []string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	task, ok := f.tasks[id]
	if !ok {
		return nil
	}
	task.ProviderIDs = append([]string{}, providerIDs...)
	task.Enabled = enabled
	f.tasks[id] = task
	return nil
}

func (f *fakeScheduledTaskStore) DeleteScheduledTask(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tasks, id)
	return nil
}

func (f *fakeScheduledTaskStore) AppendScheduledTaskRuns(runs []domain.ScheduledTaskRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, runs...)
	return nil
}

func (f *fakeScheduledTaskStore) ListScheduledTaskRuns(taskID string, limit int) ([]domain.ScheduledTaskRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []domain.ScheduledTaskRun{}
	for _, run := range f.runs {
		if taskID == "" || run.TaskID == taskID {
			out = append(out, run)
		}
	}
	return out, nil
}

func (f *fakeScheduledTaskStore) PruneScheduledTaskRuns() error { return nil }

func (f *fakeScheduledTaskStore) snapshotRuns() []domain.ScheduledTaskRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.ScheduledTaskRun{}, f.runs...)
}

func (f *fakeScheduledTaskStore) task(id string) domain.ScheduledTask {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tasks[id]
}

// quotaStubLogStore answers the quiet-window query only; everything else is a
// no-op, so the keeper sees exactly the "real traffic" the test wants.
type quotaStubLogStore struct {
	fakeRequestLogStore
	recent bool
	query  func(monitor.RequestLogQuery) (monitor.RequestLogPage, error)
}

func (q *quotaStubLogStore) QueryRequestLogs(query monitor.RequestLogQuery) (monitor.RequestLogPage, error) {
	if q.query != nil {
		return q.query(query)
	}
	if q.recent {
		return monitor.RequestLogPage{Items: []monitor.RequestLog{{Time: time.Now().Add(-5 * time.Minute)}}}, nil
	}
	return monitor.RequestLogPage{}, nil
}

func newQuotaTestServer(t *testing.T, upstreamURL string, providers ...domain.Provider) (*Server, *fakeScheduledTaskStore) {
	t.Helper()
	if len(providers) == 0 {
		providers = []domain.Provider{{
			ID: "p1", Name: "Claude", Protocol: domain.ProtocolClaude,
			AuthType: domain.AuthTypeAPIKey, BaseURL: upstreamURL + "/v1",
			APIKeySource: "literal:sk-test", AuthHeader: "x-api-key",
			DefaultModel: "claude-sonnet-4-5",
		}}
	}
	router := NewRouter(domain.GatewayState{Providers: providers})
	store := newFakeScheduledTaskStore()
	server := NewServer(router, monitor.NewStore())
	server.scheduledTaskStore = store
	return server, store
}

func chatUpstream(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	defaultBody := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}],"model":"claude-sonnet-4-5","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handler != nil {
			handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, defaultBody)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestProbeProviderForQuotaFiresThroughTheChatTestPath(t *testing.T) {
	var gotBody string
	upstream := chatUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		gotBody = string(got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}],"model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	server, _ := newQuotaTestServer(t, upstream.URL)
	task := domain.ScheduledTask{ID: "task-1", ProbePrompt: "你好", ModelOverrides: map[string]string{"p1": "claude-opus-5"}}

	run := server.probeProviderForQuota(context.Background(), task, "p1", false)
	if run.Outcome != domain.QuotaRunFired {
		t.Fatalf("outcome = %s reason = %q", run.Outcome, run.Reason)
	}
	if run.Status != http.StatusOK || run.Model != "claude-opus-5" {
		t.Fatalf("run = %+v", run)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("upstream body %q: %v", gotBody, err)
	}
	if payload["model"] != "claude-opus-5" {
		t.Fatalf("model sent = %v", payload["model"])
	}
	// The probe costs one short message: the prompt is what the user typed, not
	// the console's default "x+5等于几".
	messages, _ := payload["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %v", payload["messages"])
	}
	if content, _ := messages[0].(map[string]any); content["content"] != "你好" {
		t.Fatalf("probe prompt = %v", content)
	}
}

func TestProbeProviderForQuotaRecordsFailuresAndMissingProviders(t *testing.T) {
	upstream := chatUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"quota exhausted"}}`)
	})
	server, _ := newQuotaTestServer(t, upstream.URL)
	task := domain.ScheduledTask{ID: "task-1"}

	run := server.probeProviderForQuota(context.Background(), task, "p1", false)
	if run.Outcome != domain.QuotaRunFailed || run.Status != http.StatusTooManyRequests {
		t.Fatalf("run = %+v", run)
	}

	missing := server.probeProviderForQuota(context.Background(), task, "gone", false)
	if missing.Outcome != domain.QuotaRunFailed || !strings.Contains(missing.Reason, "不存在") {
		t.Fatalf("missing provider run = %+v", missing)
	}

	disabled := domain.Provider{ID: "p2", Name: "Off", Protocol: domain.ProtocolClaude, AuthType: domain.AuthTypeAPIKey, BaseURL: upstream.URL, Disabled: true, DefaultModel: "m"}
	if _, err := server.router.AddProvider(disabled); err != nil {
		t.Fatal(err)
	}
	skipped := server.probeProviderForQuota(context.Background(), task, "p2", false)
	if skipped.Outcome != domain.QuotaRunSkipped || !strings.Contains(skipped.Reason, "禁用") {
		t.Fatalf("disabled provider run = %+v", skipped)
	}
}

func TestProbeProviderForQuotaQuietWindowSkips(t *testing.T) {
	calls := 0
	upstream := chatUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[],"model":"m","usage":{}}`)
	})
	server, _ := newQuotaTestServer(t, upstream.URL)
	server.requestLogStore = &quotaStubLogStore{recent: true}
	task := domain.ScheduledTask{ID: "task-1", QuietMinutes: 60}

	run := server.probeProviderForQuota(context.Background(), task, "p1", false)
	if run.Outcome != domain.QuotaRunSkipped || !strings.Contains(run.Reason, "已有成功请求") {
		t.Fatalf("quiet run = %+v", run)
	}
	if calls != 0 {
		t.Fatalf("quiet window must not send traffic, got %d requests", calls)
	}

	// force is what "立即执行" means: the window is ignored.
	if forced := server.probeProviderForQuota(context.Background(), task, "p1", true); forced.Outcome != domain.QuotaRunFired {
		t.Fatalf("forced run = %+v", forced)
	}
	// A negative QuietMinutes turns the guard off entirely.
	noGuard := server.probeProviderForQuota(context.Background(), domain.ScheduledTask{ID: "task-2", QuietMinutes: -1}, "p1", false)
	if noGuard.Outcome != domain.QuotaRunFired {
		t.Fatalf("guard off run = %+v", noGuard)
	}
}

func TestDailyClockIsNotSuppressedByRealTraffic(t *testing.T) {
	// 用户反馈的现场：卡点 20:13，19:58 有一笔真实请求，20:13 被静默窗口跳过，
	// 于是下一次激活要等到第二天 05:10。定点任务的意义就是把窗口钉在 20:13，
	// 真实流量不应该让它整格消失。
	calls := 0
	upstream := chatUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[],"model":"m","usage":{}}`)
	})
	server, _ := newQuotaTestServer(t, upstream.URL)
	server.requestLogStore = &quotaStubLogStore{recent: true}

	daily := server.probeProviderForQuota(context.Background(), domain.ScheduledTask{
		ID: "task-daily", Mode: domain.ScheduleDailyTimes, QuietMinutes: 60,
	}, "p1", false)
	if daily.Outcome != domain.QuotaRunFired || calls != 1 {
		t.Fatalf("daily clock must still fire after real traffic: %+v calls=%d", daily, calls)
	}

	// 但自己刚刚探测过时仍要跳过：定点列表排得密时，没必要同一分钟打两发。
	server.recordQuotaProbe("p1", time.Now())
	again := server.probeProviderForQuota(context.Background(), domain.ScheduledTask{
		ID: "task-daily-2", Mode: domain.ScheduleDailyTimes, QuietMinutes: 60,
	}, "p1", false)
	if again.Outcome != domain.QuotaRunSkipped || !strings.Contains(again.Reason, "保活探测") {
		t.Fatalf("a recent probe must still defer the next clock: %+v", again)
	}
	if calls != 1 {
		t.Fatalf("skipped probe sent traffic: calls=%d", calls)
	}
}

func TestFailedProbeDoesNotBlockTheNextSlot(t *testing.T) {
	upstream := chatUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream down"}}`)
	})
	server, _ := newQuotaTestServer(t, upstream.URL)
	run := server.probeProviderForQuota(context.Background(), domain.ScheduledTask{
		ID: "task-1", Mode: domain.ScheduleEveryHours, QuietMinutes: 60,
	}, "p1", false)
	if run.Outcome != domain.QuotaRunFailed {
		t.Fatalf("run = %+v", run)
	}
	// 失败没有启动窗口，不能让它挡住下一格。
	if _, _, busy := server.providerRecentlyUsed("p1", time.Hour); busy {
		t.Fatal("a failed probe must not count as a recent use")
	}
}

func TestQuietWindowOnlyCountsSuccessfulTraffic(t *testing.T) {
	server, _ := newQuotaTestServer(t, "http://127.0.0.1:1")
	var got monitor.RequestLogQuery
	server.requestLogStore = &quotaStubLogStore{query: func(query monitor.RequestLogQuery) (monitor.RequestLogPage, error) {
		got = query
		return monitor.RequestLogPage{}, nil
	}}
	server.providerRecentlyUsed("p1", time.Hour)
	if got.Status != "2xx" {
		t.Fatalf("quiet window must only count successful traffic, got status=%q", got.Status)
	}
}

func TestProviderRecentlyUsesInMemoryProbeStamps(t *testing.T) {
	server, _ := newQuotaTestServer(t, "http://127.0.0.1:1")
	if _, _, busy := server.providerRecentlyUsed("p1", time.Hour); busy {
		t.Fatal("fresh server must be quiet")
	}
	server.recordQuotaProbe("p1", time.Now().Add(-10*time.Minute))
	usedAt, source, busy := server.providerRecentlyUsed("p1", time.Hour)
	if !busy || usedAt.IsZero() {
		t.Fatalf("probe stamp ignored: %v %v", usedAt, busy)
	}
	if source != "保活探测" {
		t.Fatalf("a probe must be reported as a probe, not as user traffic: %q", source)
	}
	if _, _, busy := server.providerRecentlyUsed("p1", 5*time.Minute); busy {
		t.Fatal("probe older than the window must not count")
	}
}

func TestRunScheduledTaskBatchWritesOneRowPerProviderAndAdvances(t *testing.T) {
	upstream := chatUpstream(t, nil)
	server, store := newQuotaTestServer(t, upstream.URL,
		domain.Provider{ID: "p1", Name: "A", Protocol: domain.ProtocolClaude, AuthType: domain.AuthTypeAPIKey, BaseURL: upstream.URL + "/v1", APIKeySource: "literal:k", AuthHeader: "x-api-key", DefaultModel: "m1"},
		domain.Provider{ID: "p2", Name: "B", Protocol: domain.ProtocolClaude, AuthType: domain.AuthTypeAPIKey, BaseURL: upstream.URL + "/v1", APIKeySource: "literal:k", AuthHeader: "x-api-key", DefaultModel: "m2"},
		domain.Provider{ID: "p3", Name: "Gone", Protocol: domain.ProtocolClaude, AuthType: domain.AuthTypeAPIKey, BaseURL: upstream.URL, DefaultModel: "m3", Disabled: true},
	)
	slot := localClock(t, "2026-10-16", "09:00")
	task := domain.ScheduledTask{
		ID:          "task-1",
		Mode:        domain.ScheduleDailyTimes,
		Times:       []string{"09:00", "14:00"},
		ProviderIDs: []string{"p1", "p2", "p3", "missing"},
		NextRunAt:   formatScheduleStamp(slot),
	}
	if err := store.SaveScheduledTask(task); err != nil {
		t.Fatal(err)
	}
	if !server.claimScheduledTaskRun(task.ID) {
		t.Fatal("claim failed")
	}
	server.runScheduledTaskBatch(context.Background(), task, slot, false)

	runs := store.snapshotRuns()
	if len(runs) != 3 {
		t.Fatalf("want one row per live provider (the deleted one must not be pinged), got %d: %+v", len(runs), runs)
	}
	batchIDs := map[string]struct{}{}
	for _, run := range runs {
		if run.TaskID != task.ID || run.FiredAt == "" {
			t.Fatalf("row missing identity: %+v", run)
		}
		batchIDs[run.BatchID] = struct{}{}
	}
	if len(batchIDs) != 1 {
		t.Fatalf("rows of one firing must share a batch: %v", batchIDs)
	}
	byProvider := map[string]domain.ScheduledTaskRun{}
	for _, run := range runs {
		byProvider[run.ProviderID] = run
	}
	if byProvider["p1"].Outcome != domain.QuotaRunFired || byProvider["p2"].Outcome != domain.QuotaRunFired {
		t.Fatalf("live providers: %+v %+v", byProvider["p1"], byProvider["p2"])
	}
	if byProvider["p3"].Outcome != domain.QuotaRunSkipped {
		t.Fatalf("disabled provider: %+v", byProvider["p3"])
	}
	if _, ok := byProvider["missing"]; ok {
		t.Fatalf("a deleted provider must not produce a run row: %+v", byProvider["missing"])
	}
	// A batch never rewrites membership: an id that did not resolve is skipped
	// for this round only, and the next keeper tick decides whether it was a soft
	// delete (keep the id, pause if nothing is left) or a purge (drop it).
	stored := store.task("task-1")
	if got := strings.Join(stored.ProviderIDs, ","); got != "p1,p2,p3,missing" {
		t.Fatalf("a batch must leave membership alone: %q", got)
	}
	if stored.NextRunAt == "" {
		t.Fatal("next slot not stored")
	}
	next, err := time.Parse(time.RFC3339Nano, stored.NextRunAt)
	if err != nil {
		t.Fatalf("next stamp %q: %v", stored.NextRunAt, err)
	}
	if got := next.In(time.Local).Format("15:04"); got != "14:00" {
		t.Fatalf("advanced to %s, want 14:00", got)
	}
	if stored.LastRunAt == "" {
		t.Fatal("last run stamp missing")
	}
	if !server.claimScheduledTaskRun(task.ID) {
		t.Fatal("batch must release its claim when it finishes")
	}
	server.releaseScheduledTaskRun(task.ID)
}

func testClaudeProvider(id, name, upstreamURL string) domain.Provider {
	return domain.Provider{
		ID: id, Name: name, Protocol: domain.ProtocolClaude,
		AuthType: domain.AuthTypeAPIKey, BaseURL: upstreamURL + "/v1",
		APIKeySource: "literal:sk-test", AuthHeader: "x-api-key",
		DefaultModel: "claude-sonnet-4-5",
	}
}

func TestRunDueScheduledTasksPrunesPurgedProviders(t *testing.T) {
	upstream := chatUpstream(t, nil)
	server, store := newQuotaTestServer(t, upstream.URL)
	store.SaveScheduledTask(domain.ScheduledTask{
		ID: "task-1", Enabled: true, Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"},
		ProviderIDs: []string{"p1", "gone"}, NextRunAt: formatScheduleStamp(time.Now().Add(2 * time.Hour)),
	})
	server.runDueScheduledTasks(context.Background())
	stored := store.task("task-1")
	if got := strings.Join(stored.ProviderIDs, ","); got != "p1" {
		t.Fatalf("purged provider not pruned: %q", got)
	}
	if !stored.Enabled {
		t.Fatal("a task with one live provider must stay enabled")
	}
	if runs := store.snapshotRuns(); len(runs) != 0 {
		t.Fatalf("a future slot must not fire: %+v", runs)
	}
}

// A provider delete is a soft delete that an admin can undo from the trash. The
// keeper must pause the task without losing the membership, and a restore must
// bring the task back — otherwise an accidental delete silently rewrites a
// schedule that has nothing to do with it.
func TestSoftDeletedProviderKeepsTaskMembershipAndRestoreRevivesIt(t *testing.T) {
	upstream := chatUpstream(t, nil)
	server, store := newQuotaTestServer(t, upstream.URL,
		testClaudeProvider("p1", "Claude 1", upstream.URL),
		testClaudeProvider("p2", "Claude 2", upstream.URL))
	store.SaveScheduledTask(domain.ScheduledTask{
		ID: "task-1", Enabled: true, Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"},
		ProviderIDs: []string{"p1", "p2"}, NextRunAt: formatScheduleStamp(time.Now().Add(2 * time.Hour)),
	})
	if err := server.router.DeleteProvider("p2"); err != nil {
		t.Fatal(err)
	}
	server.runDueScheduledTasks(context.Background())
	stored := store.task("task-1")
	if got := strings.Join(stored.ProviderIDs, ","); got != "p1,p2" {
		t.Fatalf("soft-deleted provider must stay on the task, providers = %q", got)
	}
	if !stored.Enabled {
		t.Fatal("one live member is enough to keep the task enabled")
	}

	// Every member soft-deleted: the task pauses but keeps its list, so a restore
	// has something to bring back.
	if err := server.router.DeleteProvider("p1"); err != nil {
		t.Fatal(err)
	}
	server.runDueScheduledTasks(context.Background())
	stored = store.task("task-1")
	if stored.Enabled {
		t.Fatal("a task with no schedulable member must be paused")
	}
	if got := strings.Join(stored.ProviderIDs, ","); got != "p1,p2" {
		t.Fatalf("pausing must preserve the ids, providers = %q", got)
	}
	server.runDueScheduledTasks(context.Background())
	if runs := store.snapshotRuns(); len(runs) != 0 {
		t.Fatalf("a paused task must not fire: %+v", runs)
	}

	// Undoing one delete revives the task on its own schedule, not immediately.
	if _, err := server.router.RestoreProvider("p1"); err != nil {
		t.Fatal(err)
	}
	server.runDueScheduledTasks(context.Background())
	stored = store.task("task-1")
	if !stored.Enabled {
		t.Fatal("restoring a member must re-enable the task")
	}
	if stored.NextRunAt == "" {
		t.Fatal("a revived task needs a fresh next slot")
	}
	if got := strings.Join(stored.ProviderIDs, ","); got != "p1,p2" {
		t.Fatalf("revive must keep the membership, providers = %q", got)
	}
}

func TestResumeTasksForProviderRevivesOnlyMatchingPausedTasks(t *testing.T) {
	upstream := chatUpstream(t, nil)
	server, store := newQuotaTestServer(t, upstream.URL)
	slot := formatScheduleStamp(localClock(t, "2026-10-16", "09:00"))
	store.SaveScheduledTask(domain.ScheduledTask{
		ID: "paused-p1", Enabled: false, Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"},
		ProviderIDs: []string{"p1", "p2"},
	})
	store.SaveScheduledTask(domain.ScheduledTask{
		ID: "paused-elsewhere", Enabled: false, Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"},
		ProviderIDs: []string{"px"},
	})
	server.resumeTasksForProvider("p1")
	if !store.task("paused-p1").Enabled {
		t.Fatal("a paused task naming the restored provider must be re-enabled")
	}
	if store.task("paused-elsewhere").Enabled {
		t.Fatal("a task naming another provider must stay paused")
	}
	// A task that is already enabled is never rewritten by a restore.
	store.SaveScheduledTask(domain.ScheduledTask{
		ID: "running", Enabled: true, Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"},
		ProviderIDs: []string{"p1"}, NextRunAt: slot,
	})
	server.resumeTasksForProvider("p1")
	if store.task("running").NextRunAt != slot {
		t.Fatal("an enabled task must keep its slot")
	}
}

func TestRunDueScheduledTasksPausesWhenEveryProviderWasDeleted(t *testing.T) {
	upstream := chatUpstream(t, nil)
	server, store := newQuotaTestServer(t, upstream.URL)
	store.SaveScheduledTask(domain.ScheduledTask{
		ID: "task-1", Enabled: true, Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"},
		ProviderIDs: []string{"gone"}, NextRunAt: formatScheduleStamp(time.Now().Add(-time.Minute)),
	})
	server.runDueScheduledTasks(context.Background())
	stored := store.task("task-1")
	if stored.Enabled {
		t.Fatal("a task with no live provider must be paused instead of firing a no-op forever")
	}
	if len(stored.ProviderIDs) != 0 {
		t.Fatalf("providers = %v, want empty", stored.ProviderIDs)
	}
	if runs := store.snapshotRuns(); len(runs) != 0 {
		t.Fatalf("a fully deleted task must not write failure rows: %+v", runs)
	}
}

func TestManualBatchKeepsTheScheduleItWasNotDueFor(t *testing.T) {
	upstream := chatUpstream(t, nil)
	server, store := newQuotaTestServer(t, upstream.URL)
	slot := localClock(t, "2026-10-16", "10:30")
	task := domain.ScheduledTask{
		ID:          "task-1",
		Mode:        domain.ScheduleDailyTimes,
		Times:       []string{"09:00", "14:00"},
		ProviderIDs: []string{"p1"},
		NextRunAt:   formatScheduleStamp(localClock(t, "2026-10-16", "14:00")),
	}
	if err := store.SaveScheduledTask(task); err != nil {
		t.Fatal(err)
	}
	server.runScheduledTaskBatch(context.Background(), task, slot, true)
	stored := store.task("task-1")
	if stored.NextRunAt != task.NextRunAt {
		t.Fatalf("manual run moved the schedule: %q → %q", task.NextRunAt, stored.NextRunAt)
	}
	if stored.LastRunAt == "" {
		t.Fatal("manual run should still record when it ran")
	}
}

func TestQuotaClaimsBlockOverlappingRuns(t *testing.T) {
	server, _ := newQuotaTestServer(t, "http://127.0.0.1:1")
	if !server.claimScheduledTaskRun("task-1") {
		t.Fatal("first task claim must succeed")
	}
	if server.claimScheduledTaskRun("task-1") {
		t.Fatal("a batch already in flight must hold the task")
	}
	if !server.claimScheduledTaskRun("task-2") {
		t.Fatal("claims are per task")
	}
	server.releaseScheduledTaskRun("task-1")
	server.releaseScheduledTaskRun("task-2")

	if !server.claimQuotaProbe("p1") {
		t.Fatal("first probe claim must succeed")
	}
	if server.claimQuotaProbe("p1") {
		t.Fatal("two tasks must not probe one provider at the same time")
	}
	if !server.claimQuotaProbe("p2") {
		t.Fatal("probes are per provider")
	}
	server.releaseQuotaProbe("p1")
	server.releaseQuotaProbe("p2")
}

// 服务器在洛杉矶、人在北京：同一个 "09:00" 必须按任务自带的时区解释，
// 否则用户看到的就是"9 点的任务在凌晨跑"。
func TestNextScheduledFireHonoursTaskZone(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("zone database unavailable: %v", err)
	}
	losAngeles, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("zone database unavailable: %v", err)
	}
	from := time.Date(2026, 10, 16, 9, 30, 0, 0, shanghai) // 09:30 北京 = 前一天 18:30 洛杉矶
	task := domain.ScheduledTask{Mode: domain.ScheduleDailyTimes, Times: []string{"09:00", "14:00", "19:00"}}

	beijing := task
	beijing.TimeZone = "Asia/Shanghai"
	next, ok := nextScheduledFire(beijing, from)
	if !ok {
		t.Fatal("no next fire")
	}
	if got := next.In(shanghai).Format("2006-01-02 15:04"); got != "2026-10-16 14:00" {
		t.Fatalf("Beijing schedule = %s, want 14:00 the same day", got)
	}

	pacific := task
	pacific.TimeZone = "America/Los_Angeles"
	next, ok = nextScheduledFire(pacific, from)
	if !ok {
		t.Fatal("no next fire")
	}
	// In Los Angeles it is still Oct 15 evening (18:30 PDT), so the only slot of
	// that day's list still ahead of it is 19:00 — 15 hours off the Beijing answer.
	if got := next.In(losAngeles).Format("2006-01-02 15:04"); got != "2026-10-15 19:00" {
		t.Fatalf("Pacific schedule = %s, want 19:00 Oct 15 PDT", got)
	}
	if !next.After(from) {
		t.Fatalf("a next fire must always be in the future: %s vs %s", next, from)
	}

	// A fixed offset must land on the same instant as the named zone, so a host
	// without a zone database still schedules the right hour.
	fixed := task
	fixed.TimeZone = "+08:00"
	offsetNext, ok := nextScheduledFire(fixed, from)
	beijingNext, _ := nextScheduledFire(beijing, from)
	if !ok || !offsetNext.Equal(beijingNext) {
		t.Fatalf("fixed offset = %v, want the same instant as Asia/Shanghai (%v)", offsetNext, beijingNext)
	}
}

func TestNormalizeScheduledTaskZone(t *testing.T) {
	for _, name := range []string{"", "Asia/Shanghai", "UTC", "America/Los_Angeles", "+08:00", "-7", "UTC+8"} {
		task := domain.ScheduledTask{Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"}, ProviderIDs: []string{"p"}, TimeZone: name}
		if err := normalizeScheduledTask(&task); err != nil {
			t.Fatalf("zone %q rejected: %v", name, err)
		}
	}
	task := domain.ScheduledTask{Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"}, ProviderIDs: []string{"p"}, TimeZone: "Asia/Shangai"}
	if err := normalizeScheduledTask(&task); err == nil {
		t.Fatal("a typo'd zone must be refused, not silently run on the server clock")
	}
}

func TestRunDueScheduledTasksAdoptsTheRealSlot(t *testing.T) {
	// 用户反馈的现场：时间表已经改成 09:00/14:00/19:00，存着的 nextRunAt 却还是
	// 05:00 —— 一个不在表里的时刻。调度器不能信这个戳，必须自己纠正过来，
	// 否则页面永远显示一个不可能的触发时间。
	upstream := chatUpstream(t, nil)
	server, store := newQuotaTestServer(t, upstream.URL)
	bogus := time.Now().Add(4 * time.Hour)
	store.SaveScheduledTask(domain.ScheduledTask{
		ID: "task-1", Enabled: true, Mode: domain.ScheduleDailyTimes, Times: []string{"09:00", "14:00", "19:00"},
		ProviderIDs: []string{"p1"}, NextRunAt: formatScheduleStamp(bogus),
	})
	server.runDueScheduledTasks(context.Background())
	if runs := store.snapshotRuns(); len(runs) != 0 {
		t.Fatalf("a bogus future slot must not fire: %+v", runs)
	}
	stored := store.task("task-1")
	if stored.NextRunAt == formatScheduleStamp(bogus) {
		t.Fatalf("the inconsistent slot was left in place: %q", stored.NextRunAt)
	}
	next, ok := parseScheduleStamp(stored.NextRunAt)
	if !ok {
		t.Fatalf("healed stamp unparsable: %q", stored.NextRunAt)
	}
	if got := next.In(time.Local).Format("15:04"); got != "09:00" && got != "14:00" && got != "19:00" {
		t.Fatalf("healed to %s, want one of the task's own times", got)
	}
	// A second tick must be a no-op: the healed value is consistent now, so the
	// loop cannot keep rewriting the row every 20 seconds.
	before := store.task("task-1").NextRunAt
	server.runDueScheduledTasks(context.Background())
	if after := store.task("task-1").NextRunAt; after != before {
		t.Fatalf("consistent slot rewritten again: %q → %q", before, after)
	}
}

func TestRunDueScheduledTasksFiresAndRespectsGrace(t *testing.T) {
	upstream := chatUpstream(t, nil)

	t.Run("due slot fires", func(t *testing.T) {
		server, store := newQuotaTestServer(t, upstream.URL)
		now := time.Now()
		store.SaveScheduledTask(domain.ScheduledTask{
			ID: "task-1", Enabled: true, Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"},
			ProviderIDs: []string{"p1"}, NextRunAt: formatScheduleStamp(now.Add(-time.Minute)),
		})
		server.runDueScheduledTasks(context.Background())
		select {
		case id := <-store.persist:
			if id != "task-1" {
				t.Fatalf("persisted %q", id)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("batch never reported its schedule")
		}
		if runs := store.snapshotRuns(); len(runs) != 1 || runs[0].Outcome != domain.QuotaRunFired {
			t.Fatalf("runs = %+v", runs)
		}
	})

	t.Run("missed beyond grace is skipped not fired", func(t *testing.T) {
		server, store := newQuotaTestServer(t, upstream.URL)
		stale := time.Now().Add(-3 * time.Hour)
		store.SaveScheduledTask(domain.ScheduledTask{
			ID: "task-1", Enabled: true, Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"},
			ProviderIDs: []string{"p1"}, NextRunAt: formatScheduleStamp(stale),
		})
		server.runDueScheduledTasks(context.Background())
		if runs := store.snapshotRuns(); len(runs) != 0 {
			t.Fatalf("stale slot fired: %+v", runs)
		}
		next, ok := parseScheduleStamp(store.task("task-1").NextRunAt)
		if !ok || !next.After(time.Now()) {
			t.Fatalf("stale slot must advance to a future time, got %q", store.task("task-1").NextRunAt)
		}
	})

	t.Run("unknown next stamp adopts the following slot", func(t *testing.T) {
		server, store := newQuotaTestServer(t, upstream.URL)
		store.SaveScheduledTask(domain.ScheduledTask{
			ID: "task-1", Enabled: true, Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"},
			ProviderIDs: []string{"p1"},
		})
		server.runDueScheduledTasks(context.Background())
		if runs := store.snapshotRuns(); len(runs) != 0 {
			t.Fatalf("a task with no stamp must not fire immediately: %+v", runs)
		}
		if next, ok := parseScheduleStamp(store.task("task-1").NextRunAt); !ok || !next.After(time.Now()) {
			t.Fatalf("no future stamp adopted: %q", store.task("task-1").NextRunAt)
		}
	})

	t.Run("disabled and empty tasks are left alone", func(t *testing.T) {
		server, store := newQuotaTestServer(t, upstream.URL)
		stamp := formatScheduleStamp(time.Now().Add(-time.Minute))
		store.SaveScheduledTask(domain.ScheduledTask{
			ID: "off", Enabled: false, Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"},
			ProviderIDs: []string{"p1"}, NextRunAt: stamp,
		})
		store.SaveScheduledTask(domain.ScheduledTask{
			ID: "empty", Enabled: true, Mode: domain.ScheduleDailyTimes, Times: []string{"09:00"},
			NextRunAt: stamp,
		})
		server.runDueScheduledTasks(context.Background())
		if runs := store.snapshotRuns(); len(runs) != 0 {
			t.Fatalf("disabled/empty tasks fired: %+v", runs)
		}
		if got := store.task("off").NextRunAt; got != stamp {
			t.Fatalf("disabled task's schedule rewritten: %q", got)
		}
	})
}
