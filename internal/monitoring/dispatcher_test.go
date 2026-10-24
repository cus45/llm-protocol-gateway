package monitoring

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeChannelAdapter struct {
	calls int
	err   error
}

func (f *fakeChannelAdapter) Send(ctx context.Context, ch Channel, n Notification) error {
	f.calls++
	return f.err
}

func testChannel(min Severity) Channel {
	return Channel{
		ID: "ch1", Type: "feishu", Name: "飞书", Enabled: true, MinSeverity: min,
		Config: ChannelConfig{Webhook: "https://example.invalid/hook", Secret: "supersecret"},
	}
}

func testFiringTransition(now time.Time, severity Severity, duration time.Duration) Transition {
	state := AlertState{
		ID: 1, Fingerprint: "provider_down|provider:p1", RuleKey: "provider_down",
		Severity: severity, TargetType: TargetProvider, TargetID: "p1", TargetName: "P1",
		Status: StatusFiring, Disposition: DispositionOpen, Count: 1,
		FirstSeen: now.Add(-duration), LastSeen: now,
	}
	event := Event{
		ID: 10, Time: now, Kind: EventKindAlert, Severity: severity, RuleKey: state.RuleKey,
		TargetType: state.TargetType, TargetID: state.TargetID, TargetName: state.TargetName,
		Title: "P1 成功率跌破 80%", Status: StatusFiring, Fingerprint: state.Fingerprint,
		Detail: map[string]any{"successRatio": 0.5, "total": 20},
	}
	return Transition{State: state, Event: event, Firing: true}
}

func seedTransition(t *testing.T, st *fakeStore, tr Transition) Transition {
	t.Helper()
	id, err := st.InsertMonitorEvent(tr.Event)
	if err != nil {
		t.Fatal(err)
	}
	tr.Event.ID = id
	return tr
}

func newDispatcherForTest(st *fakeStore, channels []Channel, cfg DispatchConfig, rules []RuleConfig) *Dispatcher {
	return NewDispatcher(st, channels, cfg, rules)
}

func quietDisabled() DispatchConfig {
	cfg := DefaultDispatchConfig()
	cfg.QuietEnabled = false
	return cfg
}

// 阶段 4 起 P1 不再即时单发：Dispatch 只入摘要队列，Flush 到点才发一条摘要。
func TestP1QueuedIntoDigest(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	st := newFakeStore()
	adapter := &fakeChannelAdapter{}
	d := newDispatcherForTest(st, []Channel{testChannel(SeverityP1)}, quietDisabled(), DefaultRules())
	current := now
	d.now = func() time.Time { return current }
	d.adapters["feishu"] = adapter

	tr := seedTransition(t, st, testFiringTransition(now, SeverityP1, time.Minute))
	if err := d.Dispatch(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 0 {
		t.Fatalf("P1 must not go out inline, calls = %d", adapter.calls)
	}
	if got := st.events[tr.Event.ID-1].PushStatus; got != PushPending {
		t.Fatalf("event push status = %q", got)
	}
	// 窗口（默认 15 分钟）没到不发；到点的那一 tick 发一份摘要。
	current = now.Add(15 * time.Minute)
	if err := d.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 1 {
		t.Fatalf("digest calls = %d", adapter.calls)
	}
	if len(st.pushes) != 1 || st.pushes[0].Status != PushSent || st.pushes[0].Attempts != 1 {
		t.Fatalf("pushes = %+v", st.pushes)
	}
	if got := st.events[tr.Event.ID-1].PushStatus; got != PushSent {
		t.Fatalf("event push status = %q", got)
	}
	state := st.states[tr.State.Fingerprint]
	// LastNotifiedAt 记的是摘要真正发出的时刻（窗口边界），不是入队时刻。
	if !state.LastNotifiedAt.Equal(now.Add(15 * time.Minute)) {
		t.Fatalf("last notified = %v", state.LastNotifiedAt)
	}
}

func TestP2NeverPushed(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	st := newFakeStore()
	adapter := &fakeChannelAdapter{}
	d := newDispatcherForTest(st, []Channel{testChannel(SeverityP2)}, quietDisabled(), DefaultRules())
	d.now = func() time.Time { return now }
	d.adapters["feishu"] = adapter

	tr := seedTransition(t, st, testFiringTransition(now, SeverityP2, time.Minute))
	if err := d.Dispatch(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 0 {
		t.Fatalf("P2 should never push, calls = %d", adapter.calls)
	}
	if got := st.events[tr.Event.ID-1].PushStatus; got != PushSkipped {
		t.Fatalf("event push status = %q", got)
	}
}

func TestCooldownSuppressesRepeat(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	st := newFakeStore()
	adapter := &fakeChannelAdapter{}
	d := newDispatcherForTest(st, []Channel{testChannel(SeverityP1)}, quietDisabled(), DefaultRules())
	d.now = func() time.Time { return now }
	d.adapters["feishu"] = adapter

	tr := testFiringTransition(now, SeverityP1, time.Minute)
	tr.State.LastNotifiedAt = now.Add(-10 * time.Minute)
	tr = seedTransition(t, st, tr)
	if err := d.Dispatch(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 0 {
		t.Fatalf("cooldown did not suppress, calls = %d", adapter.calls)
	}
	if got := st.events[tr.Event.ID-1].PushStatus; got != PushSkipped {
		t.Fatalf("event push status = %q", got)
	}
}

func TestUpgradeBypassesCooldown(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	st := newFakeStore()
	adapter := &fakeChannelAdapter{}
	d := newDispatcherForTest(st, []Channel{testChannel(SeverityP1)}, quietDisabled(), DefaultRules())
	d.now = func() time.Time { return now }
	d.adapters["feishu"] = adapter

	tr := testFiringTransition(now, SeverityP0, time.Minute)
	tr.State.LastNotifiedAt = now.Add(-10 * time.Minute)
	tr.Upgraded = true
	tr = seedTransition(t, st, tr)
	if err := d.Dispatch(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 1 {
		t.Fatalf("upgrade should bypass cooldown, calls = %d", adapter.calls)
	}
}

func TestQuietHoursDeferP1ButNotP0(t *testing.T) {
	cfg := DefaultDispatchConfig()
	cfg.Timezone = "UTC"
	cfg.QuietEnabled = true
	cfg.QuietStart = "23:00"
	cfg.QuietEnd = "08:00"
	now := time.Date(2026, 10, 23, 23, 30, 0, 0, time.UTC)
	current := now
	st := newFakeStore()
	adapter := &fakeChannelAdapter{}
	// 渠道收 P1：P1 进摘要队列（静默期压住），P0 按 minSeverity 也命中，仍即时。
	d := newDispatcherForTest(st, []Channel{testChannel(SeverityP1)}, cfg, DefaultRules())
	d.now = func() time.Time { return current }
	d.adapters["feishu"] = adapter

	p1 := seedTransition(t, st, testFiringTransition(now, SeverityP1, time.Minute))
	if err := d.Dispatch(context.Background(), p1); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 0 || d.digestQueued() != 1 {
		t.Fatalf("P1 not queued: calls=%d digest=%d", adapter.calls, d.digestQueued())
	}
	// 静默期内 Flush 不许发出摘要。
	if err := d.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 0 {
		t.Fatalf("digest sent inside quiet hours, calls = %d", adapter.calls)
	}
	if got := st.events[p1.Event.ID-1].PushStatus; got != PushPending {
		t.Fatalf("deferred event push status = %q", got)
	}

	// 换一条告警（不同指纹）并推进 10 分钟，避免撞上第一条的冷却。
	current = now.Add(10 * time.Minute)
	p0State := testFiringTransition(current, SeverityP0, time.Minute)
	p0State.State.Fingerprint = "auth_invalid|provider:p0"
	p0State.Event.Fingerprint = p0State.State.Fingerprint
	p0 := seedTransition(t, st, p0State)
	if err := d.Dispatch(context.Background(), p0); err != nil {
		t.Fatal(err)
	}
	// P0 永远即时：静默期内也当次发出，不进 deferred（计划 §4 红线）。
	if adapter.calls != 1 || len(d.deferred) != 0 {
		t.Fatalf("P0 must bypass quiet hours: calls=%d deferred=%d", adapter.calls, len(d.deferred))
	}
	if got := st.events[p0.Event.ID-1].PushStatus; got != PushSent {
		t.Fatalf("P0 push status in quiet hours = %q", got)
	}

	current = time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	if err := d.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	// calls = P0 即时 1 次 + P1 静默结束后补发摘要 1 次。
	if adapter.calls != 2 || d.digestQueued() != 0 {
		t.Fatalf("flush after quiet hours: calls=%d digest=%d", adapter.calls, d.digestQueued())
	}
	if got := st.events[p1.Event.ID-1].PushStatus; got != PushSent {
		t.Fatalf("P1 push status after quiet hours = %q", got)
	}
}

func TestChannelMinSeverityFilter(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	st := newFakeStore()
	adapter := &fakeChannelAdapter{}
	d := newDispatcherForTest(st, []Channel{testChannel(SeverityP0)}, quietDisabled(), DefaultRules())
	d.now = func() time.Time { return now }
	d.adapters["feishu"] = adapter

	tr := seedTransition(t, st, testFiringTransition(now, SeverityP1, time.Minute))
	if err := d.Dispatch(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 0 {
		t.Fatalf("channel filter ignored, calls = %d", adapter.calls)
	}
	if got := st.events[tr.Event.ID-1].PushStatus; got != PushSkipped {
		t.Fatalf("event push status = %q", got)
	}
}

func TestRecoveryShortLivedSkipped(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	current := now
	st := newFakeStore()
	adapter := &fakeChannelAdapter{}
	d := newDispatcherForTest(st, []Channel{testChannel(SeverityP1)}, quietDisabled(), DefaultRules())
	d.now = func() time.Time { return current }
	d.adapters["feishu"] = adapter

	short := testFiringTransition(now, SeverityP1, 3*time.Minute)
	short.Firing = false
	short.Resolved = true
	short.State.Status = StatusResolved
	short.Event.Status = StatusResolved
	short = seedTransition(t, st, short)
	if err := d.Dispatch(context.Background(), short); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 0 {
		t.Fatalf("short-lived recovery should be skipped, calls = %d", adapter.calls)
	}

	long := testFiringTransition(now, SeverityP1, 10*time.Minute)
	long.Firing = false
	long.Resolved = true
	long.State.Status = StatusResolved
	long.Event.Status = StatusResolved
	long = seedTransition(t, st, long)
	if err := d.Dispatch(context.Background(), long); err != nil {
		t.Fatal(err)
	}
	// 恢复本身不发单条，只进摘要队列；窗口到点后以摘要形式发出。
	if adapter.calls != 0 || d.digestQueued() != 1 {
		t.Fatalf("long-lived recovery should ride a digest, calls = %d digest = %d", adapter.calls, d.digestQueued())
	}
	current = now.Add(15 * time.Minute)
	if err := d.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 1 || d.digestQueued() != 0 {
		t.Fatalf("recovery digest not sent: calls = %d digest = %d", adapter.calls, d.digestQueued())
	}
}

func TestDigestRetryThenFailedRecorded(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	st := newFakeStore()
	adapter := &fakeChannelAdapter{err: errors.New("send failed for supersecret")}
	d := newDispatcherForTest(st, []Channel{testChannel(SeverityP1)}, quietDisabled(), DefaultRules())
	current := now
	d.now = func() time.Time { return current }
	d.adapters["feishu"] = adapter
	d.retryWait = []time.Duration{time.Millisecond, time.Millisecond}

	tr := seedTransition(t, st, testFiringTransition(now, SeverityP1, time.Minute))
	if err := d.Dispatch(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	current = now.Add(15 * time.Minute)
	if err := d.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 3 {
		t.Fatalf("attempts = %d want 3", adapter.calls)
	}
	if len(st.pushes) != 1 || st.pushes[0].Status != PushFailed || st.pushes[0].Attempts != 3 {
		t.Fatalf("pushes = %+v", st.pushes)
	}
	if strings.Contains(st.pushes[0].LastError, "supersecret") {
		t.Fatalf("push error leaked secret: %q", st.pushes[0].LastError)
	}
	if got := st.events[tr.Event.ID-1].PushStatus; got != PushFailed {
		t.Fatalf("event push status = %q", got)
	}
	if d.digestQueued() != 0 {
		t.Fatalf("a failed digest must not be re-packed into the next one, digest = %d", d.digestQueued())
	}
}
