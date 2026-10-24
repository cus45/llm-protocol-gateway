package monitoring

import "testing"

func TestDefaultRulesCoverPhaseOne(t *testing.T) {
	rules := DefaultRules()
	if len(rules) != 16 {
		t.Fatalf("phase 1 rules = %d, want 16", len(rules))
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
