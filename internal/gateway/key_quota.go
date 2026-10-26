package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// Per-key quota windows.
//
// A key's limit is expressed over a window that resets on a calendar boundary in
// the gateway's own local time. Local (not UTC) is deliberate: this gateway is run
// by one operator for their own clients, and "每天 0 点重新开始" is what they would
// tell a user. A UTC day would silently shift the reset to 08:00 for a +08:00
// deployment, and an operator reading the console would see a day that just started
// at a strange hour.

type quotaWindow string

const (
	// quotaWindowUnlimited is the zero value: a key with no window has no quota,
	// which is the behaviour every existing key had before this feature.
	quotaWindowUnlimited quotaWindow = ""
	quotaWindowDay       quotaWindow = "day"
	quotaWindowMonth     quotaWindow = "month"
)

func normalizeQuotaWindow(raw string) (quotaWindow, error) {
	switch text := strings.ToLower(strings.TrimSpace(raw)); text {
	case "":
		return quotaWindowUnlimited, nil
	case string(quotaWindowDay), string(quotaWindowMonth):
		return quotaWindow(text), nil
	default:
		return quotaWindowUnlimited, fmt.Errorf(`quota window must be "day", "month" or "" (unlimited), got %q`, raw)
	}
}

// quotaLimitCap guards a mistyped limit with a few extra zeros: a number that big
// can never be reached, so keeping it honest ("this is not a limit") beats storing
// it and letting the console render an absurd budget.
const quotaLimitCap int64 = 1_000_000_000_000

// normalizeKeyQuota resolves the three quota fields together, because they only
// mean something as a set:
//
//   - no limit set            → no window either; a lone window would gate nothing
//   - a limit with no window  → rejected: the gateway could refuse a request with
//     no idea when the client may retry, and the operator meant it to reset
//   - negative                → 0 (an int field on the API cannot say "unset")
//
// An empty window with limits is therefore an error rather than "unbounded total":
// silently-never-resetting is the worst of both readings.
func normalizeKeyQuota(rawWindow string, rawTokens, rawCost int64) (quotaWindow, int64, int64, error) {
	window, err := normalizeQuotaWindow(rawWindow)
	if err != nil {
		return quotaWindowUnlimited, 0, 0, err
	}
	tokens, cost := clampQuotaLimit(rawTokens), clampQuotaLimit(rawCost)
	if tokens == 0 && cost == 0 {
		return quotaWindowUnlimited, 0, 0, nil
	}
	if window == quotaWindowUnlimited {
		return quotaWindowUnlimited, 0, 0, fmt.Errorf(`a quota limit needs a window: set quotaWindow to "day" or "month"`)
	}
	return window, tokens, cost, nil
}

func clampQuotaLimit(value int64) int64 {
	if value <= 0 {
		return 0
	}
	if value > quotaLimitCap {
		return quotaLimitCap
	}
	return value
}

// quotaBounds is the half-open interval [from, to) a window covers for `now`.
// The upper bound is exclusive on purpose: the instant a new day starts, the old
// day's usage must already be out of the window, otherwise the reset is a few
// milliseconds late every single time.
func (w quotaWindow) bounds(now time.Time) (from time.Time, to time.Time) {
	local := now.Local()
	switch w {
	case quotaWindowDay:
		start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
		return start, start.AddDate(0, 0, 1)
	case quotaWindowMonth:
		start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, local.Location())
		return start, start.AddDate(0, 1, 0)
	default:
		// Unlimited: an interval wide enough to hold all history and never reset.
		// Callers must check the window first; the bounds only exist so a quota
		// read never has to special-case "no window" in its aggregation.
		return time.Date(1970, time.January, 1, 0, 0, 0, 0, local.Location()), local.AddDate(100, 0, 0)
	}
}

// resetInfo is what a refused client is told: how long until the window restarts,
// and when that is. Retry-After is rounded up so a client that sleeps exactly this
// long never retries into the same exhausted window.
func (w quotaWindow) resetInfo(now time.Time) (retryAfterSeconds int64, resetAt string) {
	if w == quotaWindowUnlimited {
		return 0, ""
	}
	_, to := w.bounds(now)
	seconds := int64(to.Sub(now.Local()).Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return seconds, to.Format(time.RFC3339)
}

// quotaReservations is the in-flight half of a key's quota.
//
// A window's usage is only recorded when a response *finishes* (EnqueueUsage runs
// from the request-log path, after the last byte), so N clients that hit one key at
// the same moment each read "0 used" and all of them pass — with a long stream the
// blind spot lasts as long as the stream. The reservation closes that hole for the
// part of a request the gateway already knows at entry time: its prompt.
//
// Deliberately NOT reserved: the answer's tokens and the request's cost. Both are
// only known when the upstream finishes (and cost only exists if a price row did),
// and reserving a typical-completion guess would start refusing requests that would
// have fit. The consequence is bounded and worth stating: a burst can overshoot a
// limit by what its in-flight requests actually generate, and the *next* request is
// then refused. A quota here is a governance guard, not a prepaid wallet.
type quotaReservations struct {
	mu     sync.Mutex
	tokens map[string]int64
}

func newQuotaReservations() *quotaReservations {
	return &quotaReservations{tokens: map[string]int64{}}
}

// hold reserves `tokens` for one in-flight request and returns the release, which
// is safe to call more than once (deferred cleanup plus an early return path).
func (r *quotaReservations) hold(keyID string, tokens int64) func() {
	if r == nil || keyID == "" || tokens <= 0 {
		return func() {}
	}
	r.mu.Lock()
	r.tokens[keyID] += tokens
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if left := r.tokens[keyID] - tokens; left > 0 {
				r.tokens[keyID] = left
			} else {
				delete(r.tokens, keyID)
			}
		})
	}
}

func (r *quotaReservations) pending(keyID string) int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tokens[keyID]
}

// quotaPromptEstimate is the reservation's size: the prompt part of the request
// body, counted with the same character-class heuristic the RTK counters use
// (ascii/4, one token per CJK rune). Bytes/4 — the fallback used when a stream
// reports no usage — under-counts Chinese prompts by roughly four times, which is
// the wrong direction for a guard meant to refuse early rather than late.
// errKeyQuotaExceeded marks a request its own key's quota refused. Handlers map it
// to 429 through upstreamErrorStatus, so a refusal never reads as an upstream
// failure and never cools a provider down or moves the request to another account:
// the next provider would be refused by the same key anyway.
var errKeyQuotaExceeded = errors.New("api key quota exceeded")

// quotaRefusal is one refused request plus everything a client needs to back off.
type quotaRefusal struct {
	Message           string
	RetryAfterSeconds int64
	ResetAt           string
	Kind              string // "tokens" | "cost"
	Limit             int64
	Used              int64
}

// A refusal *is* the error the flow returns, so the client sees exactly this
// Message and no internal sentinel wording. errors.Is still matches the quota
// sentinel, which is how upstreamErrorStatus knows it is a 429 rather than a 502
// upstream failure.
func (r *quotaRefusal) Error() string { return r.Message }

func (r *quotaRefusal) Is(target error) bool { return target == errKeyQuotaExceeded }

// quotaAdmission is the answer to "may this request go out, and what must it hold
// while it is in flight". release is always callable, even when the request was
// refused (it then does nothing).
type quotaAdmission struct {
	refusal *quotaRefusal
	release func()
}

func (a quotaAdmission) allowed() bool { return a.refusal == nil }

// keyHasQuota is the cheap "does this key carry a budget at all" test. Callers use
// it before estimating the prompt, so the ordinary gateway pays nothing — no
// re-marshal, no character scan — for a feature most keys never touch.
func keyHasQuota(key domain.APIKey) bool {
	return key.QuotaTokens > 0 || key.QuotaCostMicros > 0
}

func quotaWindowLabel(w quotaWindow) string {
	switch w {
	case quotaWindowDay:
		return "本日"
	case quotaWindowMonth:
		return "本月"
	default:
		return "本周期"
	}
}

// admitKeyQuota reads the key's window once and answers both budget questions
// against it. A key with no limits costs nothing: no store read, no reservation.
func (s *Server) admitKeyQuota(key domain.APIKey, promptTokens int64, now time.Time) quotaAdmission {
	window, err := normalizeQuotaWindow(key.QuotaWindow)
	if err != nil || window == quotaWindowUnlimited || (key.QuotaTokens <= 0 && key.QuotaCostMicros <= 0) {
		return quotaAdmission{release: func() {}}
	}
	if s == nil || s.logs == nil {
		// No usage store means no observable spend. Letting the request through is
		// the only honest choice: a gateway that cannot read usage must not pretend
		// to be at its limit. (A running server always has a monitor store.)
		return quotaAdmission{release: func() {}}
	}
	from, to := window.bounds(now)
	used := s.logs.KeyWindowUsage(key.ID, from, to)
	pending := s.keyQuota.pending(key.ID)
	retryAfter, resetAt := window.resetInfo(now)
	tail := fmt.Sprintf("约 %d 秒后（%s）重置", retryAfter, resetAt)

	if limit := key.QuotaTokens; limit > 0 {
		inFlight := used.Tokens + pending
		if inFlight+promptTokens > limit {
			return quotaAdmission{refusal: &quotaRefusal{
				Kind:  "tokens",
				Limit: limit,
				Used:  inFlight,
				Message: fmt.Sprintf("该 API Key 已用尽%s的 token 额度（上限 %d，已占 %d，本次请求预计还需 %d）；%s",
					quotaWindowLabel(window), limit, inFlight, promptTokens, tail),
				RetryAfterSeconds: retryAfter,
				ResetAt:           resetAt,
			}, release: func() {}}
		}
	}
	// Cost is compared against completed usage only: an in-flight request has no
	// price yet, so a burst can end the window slightly over its money cap.
	if limit := key.QuotaCostMicros; limit > 0 && used.CostMicros >= limit {
		return quotaAdmission{refusal: &quotaRefusal{
			Kind:  "cost",
			Limit: limit,
			Used:  used.CostMicros,
			Message: fmt.Sprintf("该 API Key 已用尽%s的成本额度（上限 $%.4f，已用 $%.4f；只统计已设价格行的请求）；%s",
				quotaWindowLabel(window), float64(limit)/1e6, float64(used.CostMicros)/1e6, tail),
			RetryAfterSeconds: retryAfter,
			ResetAt:           resetAt,
		}, release: func() {}}
	}
	return quotaAdmission{release: s.keyQuota.hold(key.ID, promptTokens)}
}

// rejectKeyQuota writes a refusal in the shape the client's protocol expects, the
// same way the per-key stream switch does it: OpenAI Chat and Responses tolerate
// the single error shape, Claude needs its own. Headers go on first because
// nothing has called WriteHeader yet — the handlers write the body themselves for
// failover errors, but a quota refusal is decided before any upstream is touched,
// so it answers here and returns handled.
func rejectKeyQuota(w http.ResponseWriter, clientProtocol domain.Protocol, refusal *quotaRefusal) {
	if refusal == nil {
		return
	}
	w.Header().Set("Retry-After", strconv.FormatInt(refusal.RetryAfterSeconds, 10))
	w.Header().Set("X-Quota-Reset-At", refusal.ResetAt)
	switch clientProtocol {
	case domain.ProtocolClaude:
		writeClaudeError(w, http.StatusTooManyRequests, refusal.Message)
	default:
		writeOpenAIError(w, http.StatusTooManyRequests, refusal.Message)
	}
}

// estimateKeyQuotaPrompt sizes the reservation from the parsed request body. The
// marshal only happens for keys that actually carry a quota, so the common path
// pays nothing for a number that is used once.
func estimateKeyQuotaPrompt(req map[string]any) int64 {
	if len(req) == 0 {
		return 0
	}
	body, err := json.Marshal(req)
	if err != nil {
		return 0
	}
	return quotaPromptEstimate(body)
}
