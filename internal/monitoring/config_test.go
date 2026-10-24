package monitoring

import (
	"testing"
	"time"
)

func TestDefaultRulesCoverPhaseOne(t *testing.T) {
	rules := DefaultRules()
	// 阶段 1 的 16 条 + 阶段 3 上线的 probe_failed / checkin_failed。
	if len(rules) != 18 {
		t.Fatalf("active rules = %d, want 18", len(rules))
	}
	byKey := map[string]RuleConfig{}
	for _, r := range rules {
		byKey[r.Key] = r
	}
	for _, key := range []string{"oauth_expiring", "auth_invalid", "key_budget_exhausted", "multi_ip", "collector_stale"} {
		if _, ok := byKey[key]; !ok {
			t.Fatalf("missing rule %s", key)
		}
	}
	if byKey["oauth_expiring"].Severity != SeverityP0 {
		t.Fatal("oauth_expiring must default to p0")
	}
	if byKey["probe_failed"].Severity != SeverityP1 {
		t.Fatal("probe_failed must default to p1; authSeverity only escalates a hit")
	}
	if byKey["probe_failed"].Params["authSeverity"] != float64(SeverityP0.Rank()) {
		t.Fatal("probe_failed must carry the authSeverity escalation target")
	}
	if byKey["checkin_failed"].Severity != SeverityP2 {
		t.Fatal("checkin_failed is a reminder, not a page-worthy alert")
	}
}

func TestRuleValidationRejectsWindowAndCooldown(t *testing.T) {
	rules := DefaultRules()
	if err := ValidateRules(rules); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	// 每个子用例重新取 DefaultRules：RuleConfig 里的 Params 是 map，
	// append 浅拷贝会共享 map，前一个子用例的改动会污染后一个。
	bad := DefaultRules()
	for i := range bad {
		if bad[i].Key == "auth_invalid" {
			bad[i].Params["windowMinutes"] = 7
		}
	}
	if err := ValidateRules(bad); err == nil {
		t.Fatal("windowMinutes=7 must be rejected")
	}
	bad = DefaultRules()
	for i := range bad {
		if bad[i].Key == "provider_down" {
			bad[i].CooldownMinutes = 1
		}
	}
	if err := ValidateRules(bad); err == nil {
		t.Fatal("cooldown=1 must be rejected")
	}
}

// P1 摘要间隔：5–120 合法；存量的越界值读取时夹回默认 15 分钟，不因坏值停发。
func TestDigestIntervalBoundsAndFallback(t *testing.T) {
	cases := []struct {
		minutes int
		want    time.Duration
	}{
		{minutes: 5, want: 5 * time.Minute},
		{minutes: 15, want: 15 * time.Minute},
		{minutes: 120, want: 120 * time.Minute},
		{minutes: 0, want: 15 * time.Minute},
		{minutes: 4, want: 15 * time.Minute},
		{minutes: 121, want: 15 * time.Minute},
	}
	for _, tc := range cases {
		cfg := DispatchConfig{DigestMinutes: tc.minutes}
		if got := cfg.DigestInterval(); got != tc.want {
			t.Fatalf("DigestInterval(%d) = %s, want %s", tc.minutes, got, tc.want)
		}
	}
	st := newFakeStore()
	_ = st.SetSetting("monitor_dispatch", `{"digestMinutes":9999}`)
	if got := NewConfig(st).Dispatch().DigestMinutes; got != 15 {
		t.Fatalf("stored out-of-range digestMinutes read back as %d, want 15", got)
	}
}
