package monitoring

import (
	"context"
	"sort"
	"testing"
	"time"
)

type fakeStore struct {
	states   map[string]AlertState
	events   []Event
	nextID   int64
	pushes   []EventPush
	settings map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{states: map[string]AlertState{}, settings: map[string]string{}}
}

func (f *fakeStore) Setting(key string) string { return f.settings[key] }

func (f *fakeStore) SetSetting(key, value string) error {
	if f.settings == nil {
		f.settings = map[string]string{}
	}
	f.settings[key] = value
	return nil
}

func (f *fakeStore) LoadMonitorStates() ([]AlertState, error) {
	out := make([]AlertState, 0, len(f.states))
	for _, state := range f.states {
		out = append(out, state)
	}
	return out, nil
}

func (f *fakeStore) UpsertMonitorState(state AlertState) (AlertState, error) {
	if state.ID == 0 {
		var maxID int64
		for _, existing := range f.states {
			if existing.ID > maxID {
				maxID = existing.ID
			}
		}
		state.ID = maxID + 1
	}
	f.states[state.Fingerprint] = state
	return state, nil
}

func (f *fakeStore) InsertMonitorEvent(event Event) (int64, error) {
	f.nextID++
	event.ID = f.nextID
	f.events = append(f.events, event)
	return event.ID, nil
}

func (f *fakeStore) LatestMonitorEventID(fingerprint string) (int64, error) {
	var latest int64
	for _, event := range f.events {
		if event.Fingerprint == fingerprint && event.ID > latest {
			latest = event.ID
		}
	}
	return latest, nil
}

func (f *fakeStore) UpdateMonitorEventPush(eventID int64, status, message string) error {
	for i := range f.events {
		if f.events[i].ID == eventID {
			f.events[i].PushStatus = status
			f.events[i].PushError = message
		}
	}
	return nil
}

func (f *fakeStore) UpsertMonitorEventPush(push EventPush) error {
	for i := range f.pushes {
		if f.pushes[i].EventID == push.EventID && f.pushes[i].ChannelID == push.ChannelID {
			f.pushes[i] = push
			return nil
		}
	}
	f.pushes = append(f.pushes, push)
	return nil
}

func (f *fakeStore) ListMonitorEventPushes(eventID int64) ([]EventPush, error) {
	out := []EventPush{}
	for _, push := range f.pushes {
		if push.EventID == eventID {
			out = append(out, push)
		}
	}
	return out, nil
}

func (f *fakeStore) SetMonitorDisposition(id int64, disposition string, at time.Time) error {
	for fingerprint, state := range f.states {
		if state.ID == id {
			state.Disposition = disposition
			if disposition == DispositionAcknowledged {
				state.AcknowledgedAt = at
			}
			if disposition == DispositionIgnored {
				state.IgnoredAt = at
			}
			f.states[fingerprint] = state
		}
	}
	return nil
}

func (f *fakeStore) MonitorAlertByID(id int64) (AlertState, bool, error) {
	for _, state := range f.states {
		if state.ID == id {
			return state, true, nil
		}
	}
	return AlertState{}, false, nil
}

// snapshotAt 必须把 CollectorLastSuccess 设为同一模拟时刻，否则 collector_stale
// 在 5 分钟后会额外返回一条 transition，污染断言的条数。
func snapshotAt(now time.Time, win map[string]map[int]WindowStats) Snapshot {
	return Snapshot{Now: now, Windows: win, CollectorLastSuccess: now}
}

func providerSnap(now time.Time, provider ProviderObservation, win map[int]WindowStats) Snapshot {
	windows := map[string]map[int]WindowStats{}
	if win != nil {
		windows[provider.ID] = win
	}
	return Snapshot{
		Now: now, Providers: []ProviderObservation{provider}, Windows: windows,
		CollectorLastSuccess: now,
	}
}

func transitionKeys(transitions []Transition) []string {
	keys := make([]string, 0, len(transitions))
	for _, tr := range transitions {
		keys = append(keys, tr.State.RuleKey)
	}
	sort.Strings(keys)
	return keys
}

func TestAuthInvalidNeedsSamplesAndRatio(t *testing.T) {
	base := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	store := newFakeStore()
	engine := NewEngine(store, DefaultRules())
	provider := ProviderObservation{ID: "p1", Name: "P1"}
	var transitions []Transition
	for i := 0; i < 5; i++ {
		tr, err := engine.Evaluate(context.Background(), providerSnap(base.Add(time.Duration(i)*time.Minute), provider,
			map[int]WindowStats{5: {Total: 9, Auth401403: 9}}))
		if err != nil {
			t.Fatal(err)
		}
		transitions = append(transitions, tr...)
	}
	if len(transitions) != 0 {
		t.Fatalf("fired with too few samples: %+v", transitions)
	}
	for i := 5; i <= 8; i++ {
		tr, err := engine.Evaluate(context.Background(), providerSnap(base.Add(time.Duration(i)*time.Minute), provider,
			map[int]WindowStats{5: {Total: 10, Auth401403: 5}}))
		if err != nil {
			t.Fatal(err)
		}
		transitions = append(transitions, tr...)
	}
	if len(transitions) != 1 || transitions[0].State.RuleKey != "auth_invalid" || !transitions[0].Firing {
		t.Fatalf("expected auth_invalid firing, got %+v", transitions)
	}
}

func TestProviderDownNeedsTenMinutes(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	st := newFakeStore()
	engine := NewEngine(st, DefaultRules())
	win := map[string]map[int]WindowStats{
		"p1": {5: {Total: 20, Success: 10}},
	}
	var transitions []Transition
	for i := 0; i < 9; i++ {
		tr, err := engine.Evaluate(context.Background(), snapshotAt(now.Add(time.Duration(i)*time.Minute), win))
		if err != nil {
			t.Fatal(err)
		}
		transitions = append(transitions, tr...)
	}
	if len(transitions) != 0 {
		t.Fatalf("fired before 10 minutes: %+v", transitions)
	}
	tr, err := engine.Evaluate(context.Background(), snapshotAt(now.Add(10*time.Minute), win))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr) != 1 || tr[0].State.RuleKey != "provider_down" || !tr[0].Firing {
		t.Fatalf("expected provider_down firing, got %+v", tr)
	}
}

func TestRecoveryNeedsTwoCleanTicks(t *testing.T) {
	base := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	engine := NewEngine(newFakeStore(), DefaultRules())
	bad := map[string]map[int]WindowStats{"p1": {5: {Total: 20, Success: 10}}}
	for i := 0; i <= 10; i++ {
		if _, err := engine.Evaluate(context.Background(), snapshotAt(base.Add(time.Duration(i)*time.Minute), bad)); err != nil {
			t.Fatal(err)
		}
	}
	good := map[string]map[int]WindowStats{"p1": {5: {Total: 20, Success: 20}}}
	first, err := engine.Evaluate(context.Background(), snapshotAt(base.Add(11*time.Minute), good))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 0 {
		t.Fatalf("resolved after one clean tick: %+v", first)
	}
	second, err := engine.Evaluate(context.Background(), snapshotAt(base.Add(12*time.Minute), good))
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || !second[0].Resolved || second[0].State.ResolutionReason != ResolutionRecovered {
		t.Fatalf("expected recovered resolution, got %+v", second)
	}
}

// 恢复推送的详情必须是证明恢复那一次窗口的数字。沿用触发时（或宽限期无数据时）
// 的旧详情，会让运维读到一条 "total=0" 的恢复通知。
func TestRecoveredEventCarriesCleanWindow(t *testing.T) {
	base := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	engine := NewEngine(newFakeStore(), DefaultRules())
	provider := ProviderObservation{ID: "p1", Name: "P1"}
	failing := map[int]WindowStats{5: {Total: 12, Success: 0, Auth401403: 12}}
	for i := 0; i <= 4; i++ {
		if _, err := engine.Evaluate(context.Background(), providerSnap(base.Add(time.Duration(i)*time.Minute), provider, failing)); err != nil {
			t.Fatal(err)
		}
	}
	clean := map[int]WindowStats{5: {Total: 40, Success: 40}}
	var resolved *Transition
	for i := 5; i <= 7; i++ {
		tr, err := engine.Evaluate(context.Background(), providerSnap(base.Add(time.Duration(i)*time.Minute), provider, clean))
		if err != nil {
			t.Fatal(err)
		}
		for index := range tr {
			if tr[index].Resolved && tr[index].State.RuleKey == "auth_invalid" {
				resolved = &tr[index]
			}
		}
	}
	if resolved == nil {
		t.Fatal("auth_invalid never recovered on a clean window")
	}
	total, _ := resolved.Event.Detail["total"].(int)
	auth, _ := resolved.Event.Detail["authCount"].(int)
	if total != 40 || auth != 0 {
		t.Fatalf("recovery detail = %+v, want the clean window (total 40, authCount 0)", resolved.Event.Detail)
	}
}

func TestCooldownAndFlapSuppression(t *testing.T) {
	base := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	engine := NewEngine(newFakeStore(), DefaultRules())
	key := KeyObservation{ID: "k1", Name: "K1", MultiIPCount: 5, MultiIPs: []string{"1.1.1.1"}}
	clean := KeyObservation{ID: "k1", Name: "K1"}
	snap := func(at time.Time, hit bool) Snapshot {
		keys := []KeyObservation{clean}
		if hit {
			keys = []KeyObservation{key}
		}
		return Snapshot{Now: at, Keys: keys, CollectorLastSuccess: at}
	}
	if _, err := engine.Evaluate(context.Background(), snap(base, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Evaluate(context.Background(), snap(base.Add(time.Minute), false)); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Evaluate(context.Background(), snap(base.Add(2*time.Minute), false)); err != nil {
		t.Fatal(err)
	}
	third, err := engine.Evaluate(context.Background(), snap(base.Add(3*time.Minute), true))
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 1 || !third[0].Firing {
		t.Fatalf("expected third firing, got %+v", third)
	}
	if third[0].State.SuppressedUntil.Before(base.Add(3*time.Minute + time.Hour - time.Second)) {
		t.Fatalf("flap suppression not set: %+v", third[0].State.SuppressedUntil)
	}
}

func TestUnavailableGraceResolvesDataUnavailable(t *testing.T) {
	base := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	engine := NewEngine(newFakeStore(), DefaultRules())
	provider := ProviderObservation{ID: "p1", Name: "P1"}
	bad := map[int]WindowStats{5: {Total: 20, Success: 10}}
	for i := 0; i <= 10; i++ {
		if _, err := engine.Evaluate(context.Background(), providerSnap(base.Add(time.Duration(i)*time.Minute), provider, bad)); err != nil {
			t.Fatal(err)
		}
	}
	missing := Snapshot{Now: base.Add(11 * time.Minute), Providers: []ProviderObservation{provider}, CollectorLastSuccess: base.Add(11 * time.Minute)}
	if tr, err := engine.Evaluate(context.Background(), missing); err != nil || len(tr) != 0 {
		t.Fatalf("unexpected transition at grace start: %+v %v", tr, err)
	}
	missing.Now = base.Add(12 * time.Minute)
	missing.CollectorLastSuccess = missing.Now
	if tr, err := engine.Evaluate(context.Background(), missing); err != nil || len(tr) != 0 {
		t.Fatalf("resolved within grace: %+v %v", tr, err)
	}
	missing.Now = base.Add(27 * time.Minute)
	missing.CollectorLastSuccess = missing.Now
	tr, err := engine.Evaluate(context.Background(), missing)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, transition := range tr {
		if transition.Resolved && transition.State.ResolutionReason == ResolutionDataUnavailable {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected data_unavailable resolution, got %+v", tr)
	}
}

func TestQuotaLowIgnoresZeroLimit(t *testing.T) {
	base := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	engine := NewEngine(newFakeStore(), DefaultRules())
	unsupported := ProviderObservation{ID: "p1", Name: "P1", Quota: &QuotaSnapshot{
		Remaining: -1, Limit: 0, Ratio: -1, ObservedAt: base,
	}}
	for i := 0; i < 3; i++ {
		tr, err := engine.Evaluate(context.Background(), providerSnap(base.Add(time.Duration(i)*time.Minute), unsupported, nil))
		if err != nil {
			t.Fatal(err)
		}
		if len(tr) != 0 {
			t.Fatalf("quota_low fired without a limit: %+v", tr)
		}
	}
	low := ProviderObservation{ID: "p1", Name: "P1", Quota: &QuotaSnapshot{
		Remaining: 5, Limit: 100, Ratio: 0.05, ObservedAt: base,
	}}
	tr, err := engine.Evaluate(context.Background(), providerSnap(base.Add(4*time.Minute), low, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(tr) != 1 || tr[0].State.RuleKey != "quota_low" || !tr[0].Firing {
		t.Fatalf("expected quota_low firing, got %+v", tr)
	}
}

func TestOAuthExpiringRequiresFailedRenewal(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	engine := NewEngine(newFakeStore(), DefaultRules())
	failing := ProviderObservation{ID: "p1", Name: "P1", ExpiresAt: now.Add(time.Hour),
		Renewal: RenewalOutcome{Known: true, LastOK: false}}
	healthy := ProviderObservation{ID: "p2", Name: "P2", ExpiresAt: now.Add(time.Hour),
		Renewal: RenewalOutcome{Known: true, LastOK: true}}
	snap := Snapshot{Now: now, Providers: []ProviderObservation{failing, healthy}, CollectorLastSuccess: now}
	tr, err := engine.Evaluate(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr) != 1 || tr[0].State.TargetID != "p1" || tr[0].State.RuleKey != "oauth_expiring" {
		t.Fatalf("expected oauth_expiring for p1, got %+v", tr)
	}
}

func TestKeyBudgetPicksTokenOrCostRatio(t *testing.T) {
	base := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	engine := NewEngine(newFakeStore(), DefaultRules())
	keys := []KeyObservation{
		{ID: "k1", Name: "K1", TokenLimit: 100, TokenUsed: 85, CostLimitMicros: 1000, CostUsedMicros: 990},
		{ID: "k2", Name: "K2", TokenLimit: 100, TokenUsed: 100},
	}
	snap := func(at time.Time) Snapshot {
		return Snapshot{Now: at, Keys: keys, CollectorLastSuccess: at}
	}
	var transitions []Transition
	for i := 0; i <= 5; i++ {
		tr, err := engine.Evaluate(context.Background(), snap(base.Add(time.Duration(i)*time.Minute)))
		if err != nil {
			t.Fatal(err)
		}
		transitions = append(transitions, tr...)
	}
	seen := map[string]bool{}
	for _, tr := range transitions {
		seen[tr.State.Fingerprint] = true
	}
	if !seen["key_budget_warn|api_key:k1"] {
		t.Fatalf("cost ratio 0.99 should warn: %+v", transitions)
	}
	if !seen["key_budget_exhausted|api_key:k2"] {
		t.Fatalf("token ratio 1.0 should exhaust: %+v", transitions)
	}
	if seen["key_budget_exhausted|api_key:k1"] {
		t.Fatalf("k1 must not be exhausted: %+v", transitions)
	}
}

func TestMultiIPAndConcurrentIP(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	engine := NewEngine(newFakeStore(), DefaultRules())
	key := KeyObservation{
		ID: "k1", Name: "K1", MultiIPCount: 5,
		MultiIPs:      []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5"},
		ConcurrentIPs: 4, ConcurrentAt: now,
	}
	snap := Snapshot{Now: now, Keys: []KeyObservation{key}, CollectorLastSuccess: now}
	tr, err := engine.Evaluate(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	keys := transitionKeys(tr)
	if len(tr) != 2 || keys[0] != "concurrent_ip" || keys[1] != "multi_ip" {
		t.Fatalf("expected multi_ip and concurrent_ip, got %+v", tr)
	}
}

func TestCollectorStale(t *testing.T) {
	base := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	engine := NewEngine(newFakeStore(), DefaultRules())
	var transitions []Transition
	for i := 0; i < 5; i++ {
		tr, err := engine.Evaluate(context.Background(), Snapshot{Now: base.Add(time.Duration(i) * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		transitions = append(transitions, tr...)
	}
	if len(transitions) != 0 {
		t.Fatalf("collector_stale fired early: %+v", transitions)
	}
	tr, err := engine.Evaluate(context.Background(), Snapshot{Now: base.Add(5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(tr) != 1 || tr[0].State.RuleKey != "collector_stale" {
		t.Fatalf("expected collector_stale, got %+v", tr)
	}
}

func TestTTFTRegressionRequiresBaseline(t *testing.T) {
	base := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	engine := NewEngine(newFakeStore(), DefaultRules())
	provider := ProviderObservation{ID: "p1", Name: "P1"}
	slow := map[int]WindowStats{15: {Total: 20, TTFTp95: 9000}}
	for i := 0; i <= 16; i++ {
		tr, err := engine.Evaluate(context.Background(), providerSnap(base.Add(time.Duration(i)*time.Minute), provider, slow))
		if err != nil {
			t.Fatal(err)
		}
		if len(tr) != 0 {
			t.Fatalf("ttft_regression fired without baseline: %+v", tr)
		}
	}
	provider.BaselineTTFTP95 = 1000
	var transitions []Transition
	for i := 18; i <= 33; i++ {
		tr, err := engine.Evaluate(context.Background(), providerSnap(base.Add(time.Duration(i)*time.Minute), provider, slow))
		if err != nil {
			t.Fatal(err)
		}
		transitions = append(transitions, tr...)
	}
	if len(transitions) != 1 || transitions[0].State.RuleKey != "ttft_regression" {
		t.Fatalf("expected ttft_regression firing, got %+v", transitions)
	}
}

func TestModelUnavailableOnlyForSyncingProviders(t *testing.T) {
	base := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	store := newFakeStore()
	engine := NewEngine(store, DefaultRules())
	syncing := ProviderObservation{ID: "p1", Name: "P1", Capabilities: Capabilities{Models: true}}
	plain := ProviderObservation{ID: "p2", Name: "P2", ModelFailures: 5}
	var fired []Transition
	for i := 0; i < 5; i++ {
		syncing.ModelFailures = i + 1
		at := base.Add(time.Duration(i) * 15 * time.Minute)
		tr, err := engine.Evaluate(context.Background(), Snapshot{
			Now: at, Providers: []ProviderObservation{syncing, plain}, CollectorLastSuccess: at,
		})
		if err != nil {
			t.Fatal(err)
		}
		fired = append(fired, tr...)
	}
	if len(fired) != 1 || fired[0].State.RuleKey != "model_unavailable" || fired[0].State.TargetID != "p1" {
		t.Fatalf("expected model_unavailable for p1 only, got %+v", fired)
	}
}
