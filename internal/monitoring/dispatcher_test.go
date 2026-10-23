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

func TestP1ImmediateInPhaseOne(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	st := newFakeStore()
	adapter := &fakeChannelAdapter{}
	d := newDispatcherForTest(st, []Channel{testChannel(SeverityP1)}, quietDisabled(), DefaultRules())
	d.now = func() time.Time { return now }
	d.adapters["feishu"] = adapter

	tr := seedTransition(t, st, testFiringTransition(now, SeverityP1, time.Minute))
	if err := d.Dispatch(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 1 {
		t.Fatalf("calls = %d", adapter.calls)
	}
	if len(st.pushes) != 1 || st.pushes[0].Status != PushSent || st.pushes[0].Attempts != 1 {
		t.Fatalf("pushes = %+v", st.pushes)
	}
	if got := st.events[tr.Event.ID-1].PushStatus; got != PushSent {
		t.Fatalf("event push status = %q", got)
	}
	state := st.states[tr.State.Fingerprint]
	if !state.LastNotifiedAt.Equal(now) {
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
	d := newDispatcherForTest(st, []Channel{testChannel(SeverityP1)}, cfg, DefaultRules())
	d.now = func() time.Time { return current }
	d.adapters["feishu"] = adapter

	p1 := seedTransition(t, st, testFiringTransition(now, SeverityP1, time.Minute))
	if err := d.Dispatch(context.Background(), p1); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 0 || len(d.deferred) != 1 {
		t.Fatalf("P1 not deferred: calls=%d deferred=%d", adapter.calls, len(d.deferred))
	}
	if got := st.events[p1.Event.ID-1].PushStatus; got != PushPending {
		t.Fatalf("deferred event push status = %q", got)
	}

	p0 := seedTransition(t, st, testFiringTransition(now, SeverityP0, time.Minute))
	if err := d.Dispatch(context.Background(), p0); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 1 {
		t.Fatalf("P0 should bypass quiet hours, calls = %d", adapter.calls)
	}

	current = time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	if err := d.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 2 || len(d.deferred) != 0 {
		t.Fatalf("flush did not send: calls=%d deferred=%d", adapter.calls, len(d.deferred))
	}
	if got := st.events[p1.Event.ID-1].PushStatus; got != PushSent {
		t.Fatalf("flushed event push status = %q", got)
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
	st := newFakeStore()
	adapter := &fakeChannelAdapter{}
	d := newDispatcherForTest(st, []Channel{testChannel(SeverityP1)}, quietDisabled(), DefaultRules())
	d.now = func() time.Time { return now }
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
	if adapter.calls != 1 {
		t.Fatalf("long-lived recovery should notify, calls = %d", adapter.calls)
	}
}

func TestPushRetryThenFailedRecorded(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	st := newFakeStore()
	adapter := &fakeChannelAdapter{err: errors.New("send failed for supersecret")}
	d := newDispatcherForTest(st, []Channel{testChannel(SeverityP1)}, quietDisabled(), DefaultRules())
	d.now = func() time.Time { return now }
	d.adapters["feishu"] = adapter
	d.retryWait = []time.Duration{time.Millisecond, time.Millisecond}

	tr := seedTransition(t, st, testFiringTransition(now, SeverityP1, time.Minute))
	if err := d.Dispatch(context.Background(), tr); err != nil {
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
}
