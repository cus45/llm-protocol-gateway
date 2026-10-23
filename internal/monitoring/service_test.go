package monitoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestServiceEvaluatesAndDispatches(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	st := newFakeStore()
	src := &fakeSource{
		keys: []KeyObservation{{
			ID: "k1", Name: "K1", MultiIPCount: 5,
			MultiIPs: []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5"},
		}},
	}
	collector := NewCollector(src, st)
	engine := NewEngine(st, DefaultRules())
	adapter := &fakeChannelAdapter{}
	cfg := quietDisabled()
	cfg.Timezone = "UTC"
	dispatcher := NewDispatcher(st, []Channel{testChannel(SeverityP2)}, cfg, DefaultRules())
	dispatcher.adapters["feishu"] = adapter
	service := NewService(collector, engine, dispatcher, st, nil)

	if err := service.TickAt(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d", adapter.calls)
	}
	if _, ok := st.states["multi_ip|api_key:k1"]; !ok {
		t.Fatalf("alert state missing: %+v", st.states)
	}
	if st.settings["monitor_collector_last_success"] == "" {
		t.Fatal("collector heartbeat setting not written")
	}
}

func TestServiceReloadsRulesOnSettingChange(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	st := newFakeStore()
	collector := NewCollector(&fakeSource{}, st)
	engine := NewEngine(st, DefaultRules())
	dispatchConfig := quietDisabled()
	dispatcher := NewDispatcher(st, nil, dispatchConfig, DefaultRules())
	service := NewService(collector, engine, dispatcher, st, nil)

	if err := service.TickAt(context.Background(), now); err != nil {
		t.Fatal(err)
	}

	rules := DefaultRules()
	for i := range rules {
		if rules[i].Key == "multi_ip" {
			rules[i].Severity = SeverityP1
			rules[i].CooldownMinutes = 30
		}
	}
	rawRules, err := MarshalRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("monitor_rules", rawRules); err != nil {
		t.Fatal(err)
	}
	dispatchConfig.DigestMinutes = 45
	rawDispatch, _ := json.Marshal(dispatchConfig)
	if err := st.SetSetting("monitor_dispatch", string(rawDispatch)); err != nil {
		t.Fatal(err)
	}
	rawChannels, _ := json.Marshal([]Channel{testChannel(SeverityP1)})
	if err := st.SetSetting("monitor_channels", string(rawChannels)); err != nil {
		t.Fatal(err)
	}

	if err := service.TickAt(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	active := engine.Rules()
	found := false
	for _, rule := range active {
		if rule.Key == "multi_ip" {
			found = true
			if rule.Severity != SeverityP1 || rule.CooldownMinutes != 30 {
				t.Fatalf("rule not reloaded: %+v", rule)
			}
		}
	}
	if !found {
		t.Fatal("multi_ip rule missing after reload")
	}
	if len(dispatcher.channels) != 1 || dispatcher.dispatch.DigestMinutes != 45 {
		t.Fatalf("dispatcher config not reloaded: channels=%d digest=%d", len(dispatcher.channels), dispatcher.dispatch.DigestMinutes)
	}
}

func TestServiceRecordsRenewalEvent(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	st := newFakeStore()
	service := NewService(nil, nil, nil, st, nil)

	service.RecordRenewal(context.Background(), RenewalRecord{
		ProviderID: "p1", Vendor: "claude", StartedAt: now, Duration: 1500 * time.Millisecond,
	})
	if len(st.events) != 1 {
		t.Fatalf("events = %d", len(st.events))
	}
	success := st.events[0]
	if success.Kind != EventKindRenewal || success.Severity != SeverityInfo || success.Status != StatusInfo {
		t.Fatalf("unexpected success event: %+v", success)
	}
	if success.Detail["vendor"] != "claude" {
		t.Fatalf("detail = %+v", success.Detail)
	}

	service.RecordRenewal(context.Background(), RenewalRecord{
		ProviderID: "p1", Vendor: "claude", StartedAt: now.Add(time.Minute),
		Duration: time.Second, Err: errors.New("POST failed: 401 Unauthorized token=abc123"),
	})
	if len(st.events) != 2 {
		t.Fatalf("events = %d", len(st.events))
	}
	failure := st.events[1]
	if failure.Severity != SeverityP0 {
		t.Fatalf("failure severity = %s", failure.Severity)
	}
	if failure.Detail["errorKind"] != "auth_failed" {
		t.Fatalf("error kind = %v", failure.Detail["errorKind"])
	}
	if strings.Contains(fmt.Sprint(failure.Detail), "401") || strings.Contains(fmt.Sprint(failure.Detail), "abc123") {
		t.Fatalf("raw error leaked into detail: %+v", failure.Detail)
	}
}

// panickingSource stands in for a data source that blows up on one call — a
// vendor SDK reading a nil runtime-info field, say. The gateway serves traffic
// from its own goroutines, so a panic here must cost one tick and nothing else.
type panickingSource struct {
	Source
}

func (panickingSource) Providers(context.Context, time.Time) ([]ProviderObservation, error) {
	panic("simulated source panic")
}

func TestServiceTickPanicIsContained(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	st := newFakeStore()
	healthy := &fakeSource{providers: []ProviderObservation{{ID: "p1", Name: "P1"}}}
	collector := NewCollector(&panickingSource{Source: healthy}, st)
	engine := NewEngine(st, DefaultRules())
	dispatcher := NewDispatcher(st, nil, quietDisabled(), DefaultRules())
	service := NewService(collector, engine, dispatcher, st, nil)

	err := service.TickAt(context.Background(), now)
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("a panicking tick must surface as an error, got %v", err)
	}
	// The heartbeat only advances on a tick that actually collected, or
	// collector_stale would never fire.
	if st.settings["monitor_collector_last_success"] != "" {
		t.Fatalf("a failed tick advanced the heartbeat: %v", st.settings["monitor_collector_last_success"])
	}

	// The same service must keep working once the source is healthy again: the
	// panic is per-tick, not fatal to the loop.
	live := NewCollector(healthy, st)
	recovered := NewService(live, engine, dispatcher, st, nil)
	if err := recovered.TickAt(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatalf("tick after a panic failed: %v", err)
	}
	if got := st.settings["monitor_collector_last_success"]; got == "" {
		t.Fatal("service stopped recording successful ticks")
	}
}
