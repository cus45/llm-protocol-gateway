package gateway

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestParseProviderCooldownHint(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(20 * time.Minute)
	cases := []struct {
		name       string
		retryAfter string
		body       string
		wantAfter  time.Duration
		wantReset  time.Time
	}{
		{
			name:       "retry-after seconds cap a short hint",
			retryAfter: "2",
			wantAfter:  2 * time.Second,
		},
		{
			name:       "retry-after http date",
			retryAfter: now.Add(40 * time.Second).Format(http.TimeFormat),
			wantAfter:  40 * time.Second,
		},
		{
			name:       "retry-after past date ignored",
			retryAfter: now.Add(-time.Minute).Format(http.TimeFormat),
		},
		{
			name:      "anthropic resets_at rfc3339",
			body:      `{"type":"error","error":{"type":"rate_limit_error","message":"limit"},"resets_at":"` + resetAt.Format(time.RFC3339) + `"}`,
			wantReset: resetAt,
		},
		{
			name:      "codex resets_at ms epoch",
			body:      `{"error":{"type":"usage_limit_reached","resets_at":` + strconv.FormatInt(resetAt.UnixMilli(), 10) + `}}`,
			wantReset: resetAt,
		},
		{
			name:      "codex resets_in_seconds",
			body:      `{"error":{"code":"rate_limit_reached","resets_in_seconds":90}}`,
			wantAfter: 90 * time.Second,
		},
		{
			name:      "resets_in_seconds nested under details",
			body:      `{"error":{"details":{"resets_in_seconds":30}}}`,
			wantAfter: 30 * time.Second,
		},
		{
			name:      "past resets_at dropped",
			body:      `{"resets_at":"` + now.Add(-time.Hour).Format(time.RFC3339) + `"}`,
			wantReset: time.Time{},
		},
		{
			name:      "prose wrapped json",
			body:      `upstream said: {"resets_at":"` + resetAt.Format(time.RFC3339) + `"} sorry`,
			wantReset: resetAt,
		},
		{
			name: "no hints",
			body: `{"error":{"message":"boom"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hint := parseProviderCooldownHint(tc.retryAfter, []byte(tc.body), now)
			if hint.retryAfter != tc.wantAfter {
				t.Fatalf("retryAfter = %v, want %v", hint.retryAfter, tc.wantAfter)
			}
			if !hint.resetsAt.Equal(tc.wantReset) {
				t.Fatalf("resetsAt = %v, want %v", hint.resetsAt, tc.wantReset)
			}
		})
	}
}

// A "billing" word alone is a configuration complaint, not a spent account:
// it used to take a healthy provider out of rotation as a hard quota death.
func TestBillingWordIsNotHardQuota(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"error":{"message":"billing not enabled on this workspace"}}`,
		`{"error":{"message":"Please add a billing method in the console"}}`,
	} {
		if class, reason := classifyProviderFailover(http.StatusBadRequest, []byte(body), nil, nil); class != failoverNone {
			t.Fatalf("%s classified %v (%s), want none", body, class, reason)
		}
	}
	// A spent OpenAI account still dies by its real code.
	if class, _ := classifyProviderFailover(http.StatusTooManyRequests, []byte(`{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`), nil, nil); class != failoverHard {
		t.Fatalf("insufficient_quota classified %v, want hard", class)
	}
}

// OpenAI's own wording and code both mean a spent account; relays echo either.
func TestExhaustedQuotaHints(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{"error":{"code":"insufficient_quota","message":"You exceeded your current quota, please check your plan and billing details"}}`,
		`{"error":{"message":"You exceeded your current quota","type":"invalid_request_error"}}`,
		`{"error":{"message":"insufficient_quota"}}`,
	}
	for _, body := range cases {
		if class, reason := classifyProviderFailover(http.StatusBadRequest, []byte(body), nil, nil); class != failoverHard {
			t.Fatalf("%s classified %v (%s), want hard", body, class, reason)
		}
	}
}

func TestCooldownTTLFollowsUpstreamHint(t *testing.T) {
	t.Parallel()
	now := time.Now()
	s := &Server{}
	const soft = providerSoftUnavailableTTL
	const hard = providerQuotaRestFloor
	cases := []struct {
		name       string
		hard       bool
		retryAfter string
		body       string
		want       time.Duration
	}{
		{name: "soft without hint", hard: false, want: soft},
		{name: "hard without hint", hard: true, want: hard},
		// A vendor saying "come back in 1s" is telling the truth about its own
		// throttle; the ladder sets the default rest, not a floor over the vendor.
		{name: "soft retry-after shorter than the default wins", hard: false, retryAfter: "1", want: 1 * time.Second},
		{name: "soft retry-after above the soft default wins", hard: false, retryAfter: "90", want: 90 * time.Second},
		{name: "soft retry-after capped at the rate cap", hard: false, retryAfter: "3000", want: providerRateRestCap},
		{name: "hard retry-after below the floor keeps the floor", hard: true, retryAfter: "10", want: hard},
		{name: "hard retry-after longer wins", hard: true, retryAfter: "900", want: 15 * time.Minute},
		{name: "absurd hint capped", hard: true, retryAfter: "86400", want: providerCooldownCap},
		{
			name: "resets_at wins over the default",
			hard: true,
			body: `{"resets_at":"` + now.Add(30*time.Minute).Format(time.RFC3339) + `"}`,
			want: providerCooldownCap,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.cooldownTTL("p", tc.retryAfter, []byte(tc.body), tc.hard, now)
			if tc.want == providerCooldownCap {
				if got < 29*time.Minute || got > providerCooldownCap {
					t.Fatalf("ttl = %v, want ~30m (capped at %v)", got, providerCooldownCap)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("ttl = %v, want %v", got, tc.want)
			}
		})
	}
}

// The usage panel already knows exactly when the Claude window reopens; a terse
// 429 body must not make the gateway guess one minute instead.
func TestCooldownTTLUsesCachedClaudeWindow(t *testing.T) {
	t.Parallel()
	now := time.Now()
	s := &Server{oauthUsageCache: newOAuthUsageCache()}
	s.oauthUsageCache.set("claude:c1", ClaudeOAuthUsageReport{
		Available: true,
		FiveHour: &ClaudeOAuthUsageBucket{
			Utilization: 100,
			ResetsAt:    now.Add(17 * time.Minute).UTC().Format(time.RFC3339),
		},
	})
	ttl := s.cooldownTTL("c1", "", []byte(`{"error":{"message":"usage limit"}}`), false, now)
	if ttl < 16*time.Minute || ttl > 18*time.Minute {
		t.Fatalf("ttl = %v, want ~17m from the cached window", ttl)
	}
}

func TestCooldownTTLUsesCachedChatGPTWindow(t *testing.T) {
	t.Parallel()
	now := time.Now()
	s := &Server{oauthUsageCache: newOAuthUsageCache()}
	s.oauthUsageCache.set("chatgpt:k1", ChatGPTOAuthUsageReport{
		Available: true,
		Buckets: []ChatGPTOAuthUsageBucket{{
			Label:       "5 小时额度",
			Utilization: 100,
			ResetsAt:    now.Add(9 * time.Minute).UTC().Format(time.RFC3339),
		}},
	})
	ttl := s.cooldownTTL("k1", "", []byte(`{"error":{"type":"usage_limit_reached"}}`), true, now)
	if ttl < 8*time.Minute || ttl > 10*time.Minute {
		t.Fatalf("ttl = %v, want ~9m from the cached ChatGPT window", ttl)
	}
}

func TestApplyOutcomeRetryAfterExtendsCooldown(t *testing.T) {
	t.Parallel()
	s := &Server{}
	for i := 0; i < providerSoftFailureThreshold; i++ {
		s.applyProviderFailoverOutcome("p1", http.StatusTooManyRequests, []byte(`{"error":{"code":"rate_limit_reached","resets_in_seconds":120}}`), "5", nil)
	}
	st, ok := s.providerUnavailableSnapshot("p1")
	if !ok {
		t.Fatal("expected unavailable")
	}
	if left := time.Until(st.nextRetry); left < 100*time.Second {
		t.Fatalf("cooldown = %v left, want ~2m from resets_in_seconds", left)
	}
}

func TestApplyOutcomeHardKeepsDefaultWithoutHint(t *testing.T) {
	t.Parallel()
	s := &Server{}
	s.applyProviderFailoverOutcome("p2", http.StatusUnauthorized, []byte(`{"error":{"message":"invalid_grant"}}`), "", nil)
	st, ok := s.providerUnavailableSnapshot("p2")
	if !ok || !st.hard {
		t.Fatalf("expected hard unavailable, got %#v ok=%v", st, ok)
	}
	if left := time.Until(st.nextRetry); left < providerFailoverRecoveryInterval-time.Second {
		t.Fatalf("hard cooldown = %v, want >= %v", left, providerFailoverRecoveryInterval)
	}
}

// The Retry-After the upstream sent must survive the failover writer and reach
// the cooldown; a passthrough writer must not leak the previous attempt's.
func TestFailoverWriterUpstreamHeader(t *testing.T) {
	t.Parallel()
	w := newFailoverResponseWriter(httptest.NewRecorder(), true)
	w.Header().Set("Retry-After", "42")
	if got := w.UpstreamHeader("Retry-After"); got != "42" {
		t.Fatalf("buffered header = %q, want 42", got)
	}
	w.passthrough = true
	if got := w.UpstreamHeader("Retry-After"); got != "" {
		t.Fatalf("passthrough writer leaked %q", got)
	}
}

// The rest follows the window that is actually spent: a weekly plan window at
// 100% with the 5h window nearly empty must not be read as "reopens in 9m".
func TestCooldownTTLUsesTheExhaustedClaudeWindow(t *testing.T) {
	t.Parallel()
	now := time.Now()
	s := &Server{oauthUsageCache: newOAuthUsageCache()}
	s.oauthUsageCache.set("claude:c2", ClaudeOAuthUsageReport{
		Available: true,
		FiveHour: &ClaudeOAuthUsageBucket{
			Utilization: 5,
			ResetsAt:    now.Add(9 * time.Minute).UTC().Format(time.RFC3339),
		},
		SevenDay: &ClaudeOAuthUsageBucket{
			Utilization: 100,
			ResetsAt:    now.Add(6 * 24 * time.Hour).UTC().Format(time.RFC3339),
		},
	})
	ttl := s.cooldownTTL("c2", "", []byte(`{"error":{"message":"usage limit"}}`), true, now)
	if ttl != providerCooldownCap {
		t.Fatalf("ttl = %v, want the capped wait for the exhausted weekly window", ttl)
	}
}

// A vendor's own Retry-After is the truth about its throttle: a short one is
// neither stretched to the ladder's default nor left to replace a longer rest.
func TestCooldownTTLHonoursAShortRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Now()
	s := &Server{}
	if ttl := s.cooldownTTL("short", "2", nil, false, now); ttl != 2*time.Second {
		t.Fatalf("ttl = %v, want the vendor's 2s", ttl)
	}
}
