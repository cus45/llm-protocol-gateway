package monitoring

import (
	"context"
	"testing"
	"time"
)

// ruleTicks advances one engine by a minute and returns only the transitions of
// one rule, so a table test can assert a single rule's lifecycle without being
// disturbed by neighbours (provider_down fires alongside most bad windows).
func ruleTicks(t *testing.T, engine *Engine, now time.Time, target string, win map[int]WindowStats, ruleKey string) []Transition {
	t.Helper()
	tr, err := engine.Evaluate(context.Background(), snapshotAt(now, map[string]map[int]WindowStats{target: win}))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]Transition, 0, len(tr))
	for _, item := range tr {
		if item.State.RuleKey == ruleKey {
			out = append(out, item)
		}
	}
	return out
}

// 阶段 1 的窗口类规则逐条钉住三件事：达标触发、阈值边缘与样本不足不触发、
// 连续两个干净 tick 恢复。边缘值故意写成「恰好等于阈值」，因为规则用的是严格
// 大于——等于时不触发正是它该有的语义；样本不足必须沉默，既不能报警也不能被
// 当成恢复。
func TestWindowRulesTriggerThresholdAndRecover(t *testing.T) {
	cases := []struct {
		ruleKey        string
		windowMinutes  int
		sustainedTicks int
		bad            WindowStats
		atThreshold    WindowStats
		belowSamples   WindowStats
		clean          WindowStats
	}{
		{
			ruleKey: "server_error_spike", windowMinutes: 5, sustainedTicks: 5,
			bad:          WindowStats{Total: 20, Success: 17, ServerError: 3},
			atThreshold:  WindowStats{Total: 30, Success: 27, ServerError: 3},
			belowSamples: WindowStats{Total: 19, Success: 0, ServerError: 19},
			clean:        WindowStats{Total: 20, Success: 20},
		},
		{
			ruleKey: "rate_limit_spike", windowMinutes: 5, sustainedTicks: 5,
			bad:          WindowStats{Total: 20, Success: 15, RateLimited: 5},
			atThreshold:  WindowStats{Total: 20, Success: 16, RateLimited: 4},
			belowSamples: WindowStats{Total: 19, Success: 0, RateLimited: 19},
			clean:        WindowStats{Total: 20, Success: 20},
		},
		{
			ruleKey: "empty_response", windowMinutes: 10, sustainedTicks: 10,
			bad:          WindowStats{Total: 10, Success: 9, Empty: 1},
			atThreshold:  WindowStats{Total: 20, Success: 19, Empty: 1},
			belowSamples: WindowStats{Total: 9, Success: 0, Empty: 9},
			clean:        WindowStats{Total: 10, Success: 10},
		},
	}
	base := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	at := func(offset int) time.Time { return base.Add(time.Duration(offset) * time.Minute) }

	for _, tc := range cases {
		tc := tc
		t.Run(tc.ruleKey+" 达标触发", func(t *testing.T) {
			engine := NewEngine(newFakeStore(), DefaultRules())
			win := map[int]WindowStats{tc.windowMinutes: tc.bad}
			var fired []Transition
			for offset := 0; offset <= tc.sustainedTicks; offset++ {
				for _, item := range ruleTicks(t, engine, at(offset), "p1", win, tc.ruleKey) {
					if item.Firing {
						fired = append(fired, item)
					}
				}
				if offset < tc.sustainedTicks && len(fired) != 0 {
					t.Fatalf("%s fired at tick %d before its %d-minute sustain", tc.ruleKey, offset, tc.sustainedTicks)
				}
			}
			if len(fired) != 1 {
				t.Fatalf("%s firing transitions = %d, want exactly 1", tc.ruleKey, len(fired))
			}
			if fired[0].Event.Severity != SeverityP1 {
				t.Fatalf("%s severity = %s, want p1", tc.ruleKey, fired[0].Event.Severity)
			}
		})

		t.Run(tc.ruleKey+" 边缘与缺样本不触发", func(t *testing.T) {
			for _, win := range []map[int]WindowStats{
				{tc.windowMinutes: tc.atThreshold},
				{tc.windowMinutes: tc.belowSamples},
			} {
				engine := NewEngine(newFakeStore(), DefaultRules())
				for offset := 0; offset <= tc.sustainedTicks+3; offset++ {
					if got := ruleTicks(t, engine, at(offset), "p1", win, tc.ruleKey); len(got) != 0 {
						t.Fatalf("%s produced %+v on window %+v", tc.ruleKey, got, win)
					}
				}
			}
		})

		t.Run(tc.ruleKey+" 干净两 tick 恢复", func(t *testing.T) {
			engine := NewEngine(newFakeStore(), DefaultRules())
			win := map[int]WindowStats{tc.windowMinutes: tc.bad}
			for offset := 0; offset <= tc.sustainedTicks; offset++ {
				ruleTicks(t, engine, at(offset), "p1", win, tc.ruleKey)
			}
			clean := map[int]WindowStats{tc.windowMinutes: tc.clean}
			if got := ruleTicks(t, engine, at(tc.sustainedTicks+1), "p1", clean, tc.ruleKey); len(got) != 0 {
				t.Fatalf("%s resolved after one clean tick: %+v", tc.ruleKey, got)
			}
			resolved := ruleTicks(t, engine, at(tc.sustainedTicks+2), "p1", clean, tc.ruleKey)
			if len(resolved) != 1 || !resolved[0].Resolved ||
				resolved[0].State.ResolutionReason != ResolutionRecovered {
				t.Fatalf("%s did not recover: %+v", tc.ruleKey, resolved)
			}
		})
	}
}

// 限流规则有两条独立入口：占比超标，或绝对条数触顶（maxCount）。后者专门用来
// 抓住「低流量但连续 429」的账号，所以两条都要钉住。
func TestRateLimitSpikeFiresOnCountAlone(t *testing.T) {
	base := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	engine := NewEngine(newFakeStore(), DefaultRules())
	// 10/100 = 0.1 低于 maxRatio 0.2，但条数已达 maxCount 10。
	win := map[int]WindowStats{5: {Total: 100, Success: 90, RateLimited: 10}}
	var fired int
	for offset := 0; offset <= 5; offset++ {
		for _, item := range ruleTicks(t, engine, base.Add(time.Duration(offset)*time.Minute), "p1", win, "rate_limit_spike") {
			if item.Firing {
				fired++
			}
		}
	}
	if fired != 1 {
		t.Fatalf("rate_limit_spike firing = %d, want 1", fired)
	}
}

// 凭证类规则读的是账号状态：连续续期失败按「连续命中的 tick 数」收敛，积分到期
// 只在额度读数本身可用时才报，缺数据一律沉默。
func TestCredentialRulesNeedData(t *testing.T) {
	base := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	engine := NewEngine(newFakeStore(), DefaultRules())

	single := ProviderObservation{
		ID: "p1", Name: "P1", ExpiresAt: base.Add(24 * time.Hour),
		Renewal: RenewalOutcome{Known: true, Consecutive: 1, LastOK: false},
	}
	for offset := 0; offset <= 3; offset++ {
		got := ruleTicksProvider(t, engine, base.Add(time.Duration(offset)*time.Minute), single, "oauth_refresh_failed")
		if len(got) != 0 {
			t.Fatalf("one failed renewal must not alert, got %+v", got)
		}
	}

	pair := single
	pair.Renewal = RenewalOutcome{Known: true, Consecutive: 2, LastOK: false}
	var fired []Transition
	for offset := 4; offset <= 5; offset++ {
		for _, item := range ruleTicksProvider(t, engine, base.Add(time.Duration(offset)*time.Minute), pair, "oauth_refresh_failed") {
			if item.Firing {
				fired = append(fired, item)
			}
		}
	}
	if len(fired) != 1 {
		t.Fatalf("oauth_refresh_failed firing = %d, want 1", len(fired))
	}
	if fired[0].Event.Severity != SeverityP0 {
		t.Fatalf("a dead credential must be P0, got %s", fired[0].Event.Severity)
	}

	// 续期成功后计数归零 → 两个干净 tick 解决。
	renewed := pair
	renewed.Renewal = RenewalOutcome{Known: true, Consecutive: 0, LastOK: true}
	renewed.ExpiresAt = base.Add(48 * time.Hour)
	var resolved []Transition
	for offset := 6; offset <= 8; offset++ {
		for _, item := range ruleTicksProvider(t, engine, base.Add(time.Duration(offset)*time.Minute), renewed, "oauth_refresh_failed") {
			if item.Resolved {
				resolved = append(resolved, item)
			}
		}
	}
	if len(resolved) != 1 {
		t.Fatalf("oauth_refresh_failed did not resolve after renewals succeeded: %d", len(resolved))
	}

	// 积分到期：lead 72 小时以内报，以外不报；额度不可用（Remaining<=0）时不报。
	credits := NewEngine(newFakeStore(), DefaultRules())
	soon := ProviderObservation{
		ID: "p2", Name: "P2",
		Quota: &QuotaSnapshot{Source: "workbuddy", Remaining: 500, Limit: 1000, Ratio: 0.5,
			ExpiresAt: base.Add(24 * time.Hour), ObservedAt: base},
	}
	if got := ruleTicksProvider(t, credits, base, soon, "credit_expiring"); len(got) != 1 || !got[0].Firing {
		t.Fatalf("credit_expiring should fire inside the lead window: %+v", got)
	}

	later := soon
	later.Quota = &QuotaSnapshot{Source: "workbuddy", Remaining: 500, Limit: 1000, Ratio: 0.5,
		ExpiresAt: base.Add(100 * time.Hour), ObservedAt: base}
	if got := ruleTicksProvider(t, credits, base.Add(time.Minute), later, "credit_expiring"); len(got) != 0 {
		t.Fatalf("credit_expiring fired outside the lead window: %+v", got)
	}

	// 额度读数不可用：不能拿一个到期的 0 余额编造「积分即将到期」。
	unusable := soon
	unusable.Quota = &QuotaSnapshot{Source: "workbuddy", Remaining: 0, Limit: 0, Ratio: -1,
		ExpiresAt: base.Add(time.Hour), ObservedAt: base}
	if got := ruleTicksProvider(t, credits, base.Add(2*time.Minute), unusable, "credit_expiring"); len(got) != 0 {
		t.Fatalf("credit_expiring fired on an unusable quota reading: %+v", got)
	}
}

func ruleTicksProvider(t *testing.T, engine *Engine, now time.Time, provider ProviderObservation, ruleKey string) []Transition {
	t.Helper()
	tr, err := engine.Evaluate(context.Background(), providerSnap(now, provider, nil))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]Transition, 0, len(tr))
	for _, item := range tr {
		if item.State.RuleKey == ruleKey {
			out = append(out, item)
		}
	}
	return out
}
