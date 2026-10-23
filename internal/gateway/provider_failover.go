package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// failoverClass distinguishes transient upstream issues from hard quota/auth
// failures. Both still advance the backup chain; only hard (or repeated soft)
// failures mark the provider UI as "unavailable".
type failoverClass int

const (
	failoverNone failoverClass = iota
	failoverSoft
	failoverHard
	// failoverPromptRefusal advances the backup chain but rests nobody: the
	// vendor turned the request's own prompt away, as WorkBuddy's 11128 does to
	// another agent's system prompt. Every account of it answers the same, so
	// none of them is at fault.
	failoverPromptRefusal
)

const (
	// Soft failures must repeat within this window before the provider is
	// marked unavailable (avoids single 429/529/5xx blips painting "异常").
	providerSoftFailureWindow    = 60 * time.Second
	providerSoftFailureThreshold = 3
	providerSoftUnavailableTTL   = 1 * time.Minute
	claudeUsageHardThresholdPct  = 95.0
)

// hardQuotaHints are narrow subscription/billing exhaustion signals. Broad
// tokens like bare "quota" / "exceeded" / "rate limit" are intentionally
// omitted — those usually mean short-lived throttling, not a dead provider.
//
// Bare "billing" is deliberately absent: "billing not enabled on this
// workspace", "billing address missing" and the like are configuration
// mistakes that the next account on the chain answers fine, and treating the
// word as quota death cooled a healthy provider down for a whole cycle.
var hardQuotaHints = []string{
	"insufficient_quota",
	"usage limit",
	"you've hit your",
	"you have hit your",
	"credit balance",
	"tokens exhausted",
	"out of credits",
	"quota exceeded",
	"exceeded your current quota",
	"plan limit",
}

// hardAuthHints mark credential death (not a transient token-refresh race).
var hardAuthHints = []string{
	"invalid_grant",
	"revoked",
	"invalid_api_key",
	"invalid x-api-key",
	"invalid api key",
	"api key not valid",
}

// softThrottleHints classify 400 bodies that are failover-worthy but not a
// hard account/quota kill.
var softThrottleHints = []string{
	"rate_limit",
	"rate limit",
	"overloaded",
	"capacity",
}

type providerUnavailableState struct {
	nextRetry time.Time
	hard      bool
	reason    string
}

type providerSoftFailureState struct {
	count       int
	windowStart time.Time
}

// shouldFailoverProvider reports whether an upstream failure should advance to
// the next backup provider for this API key.
func shouldFailoverProvider(status int, body []byte, transportErr error) bool {
	class, _ := classifyProviderFailover(status, body, transportErr, nil)
	return class != failoverNone
}

// isRequestContextCanceled reports client-gone / deadline errors. These must
// NOT trigger provider failover: the backup attempt reuses r.Context(), which
// is already done, so it would only produce a misleading "context canceled"
// against the fallback URL (and leave Claude Code looking "stuck").
func isRequestContextCanceled(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// classifyProviderFailover returns soft/hard/none plus a short reason string.
// fiveHourUtil, when non-nil, is the cached Claude OAuth 5h utilization
// (0-100). Hard "usage limit" signals are downgraded to soft when utilization
// is clearly under the hard threshold — matching the quota panel reality.
func classifyProviderFailover(status int, body []byte, transportErr error, fiveHourUtil *float64) (failoverClass, string) {
	if transportErr != nil {
		if isRequestContextCanceled(transportErr) {
			return failoverNone, "client_canceled"
		}
		return failoverSoft, "transport: " + truncateReason(transportErr.Error())
	}

	msg := strings.ToLower(summarizeUpstreamHTTPError(status, body))
	hardQuota := containsAnyHint(msg, hardQuotaHints)
	hardAuth := containsAnyHint(msg, hardAuthHints)
	softThrottle := containsAnyHint(msg, softThrottleHints)

	switch {
	case status == http.StatusTooManyRequests || status == 529:
		if hardQuota {
			return maybeDowngradeHardUsage(failoverHard, fmt.Sprintf("http_%d_usage", status), fiveHourUtil)
		}
		return failoverSoft, fmt.Sprintf("http_%d", status)
	case status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout:
		return failoverSoft, fmt.Sprintf("http_%d", status)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if hardQuota {
			return maybeDowngradeHardUsage(failoverHard, fmt.Sprintf("http_%d_usage", status), fiveHourUtil)
		}
		if hardAuth {
			return failoverHard, fmt.Sprintf("http_%d_auth", status)
		}
		// Transient auth (token refresh race, brief 403): failover yes, mark no
		// until consecutive soft failures accumulate.
		return failoverSoft, fmt.Sprintf("http_%d", status)
	case status == http.StatusBadRequest:
		if hardQuota {
			return maybeDowngradeHardUsage(failoverHard, "http_400_usage", fiveHourUtil)
		}
		if softThrottle {
			return failoverSoft, "http_400_throttle"
		}
		if workBuddyPromptRefused(status, body) {
			return failoverPromptRefusal, "prompt_refused"
		}
		return failoverNone, ""
	default:
		return failoverNone, ""
	}
}

func maybeDowngradeHardUsage(class failoverClass, reason string, fiveHourUtil *float64) (failoverClass, string) {
	if class != failoverHard || fiveHourUtil == nil {
		return class, reason
	}
	if *fiveHourUtil < claudeUsageHardThresholdPct {
		return failoverSoft, fmt.Sprintf("%s (five_hour=%.1f<%.0f, softed)", reason, *fiveHourUtil, claudeUsageHardThresholdPct)
	}
	return class, fmt.Sprintf("%s (five_hour=%.1f)", reason, *fiveHourUtil)
}

func containsAnyHint(msg string, hints []string) bool {
	for _, hint := range hints {
		if hint != "" && strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

func truncateReason(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 160 {
		return s
	}
	return s[:160] + "…"
}

// failoverResponseWriter buffers error responses so the gateway can try the
// next provider. Successful (2xx) responses flush through immediately so SSE
// streaming still works.
type failoverResponseWriter struct {
	base        http.ResponseWriter
	header      http.Header
	status      int
	buf         bytes.Buffer
	canDefer    bool
	passthrough bool
	discarded   bool
	wroteHeader bool
	// body is the whole turn this attempt produced: the bytes before its commit
	// followed by everything written after it. delivered is how much of that
	// turn the client already has — a commit hands over its bytes, a write after
	// the commit goes straight out — and is all a cut turn may be closed on top
	// of without sending anything twice.
	body      bytes.Buffer
	delivered int
}

func newFailoverResponseWriter(base http.ResponseWriter, canDefer bool) *failoverResponseWriter {
	return &failoverResponseWriter{
		base:     base,
		header:   make(http.Header),
		canDefer: canDefer,
	}
}

// UpstreamHeader returns a header the upstream sent with the buffered error
// attempt (Retry-After), empty once the writer took over the real response.
func (w *failoverResponseWriter) UpstreamHeader(name string) string {
	if w.passthrough || w.discarded {
		return ""
	}
	return strings.TrimSpace(w.header.Get(name))
}

// deferring reports whether this attempt's response can still be thrown away
// for a backup: nothing has been committed to the real client yet.
func (w *failoverResponseWriter) deferring() bool {
	return !w.passthrough && !w.discarded
}

// failoverUnavailable reports that this attempt is the last one: the loop has
// no backup left, so whatever the attempt wrote is the client's answer.
func (w *failoverResponseWriter) failoverUnavailable() bool {
	return !w.canDefer
}

// streamClosed reports that the buffered stream ends with its protocol's
// closing frame, i.e. the turn was closed (by the upstream or by a converter
// that closed a cut stream itself) and only needs to be delivered.
func (w *failoverResponseWriter) streamClosed() bool {
	return closesUpstreamStream(w.stream())
}

// Prefix returns what the attempt handed to the client when it committed its
// headers: the start of its turn. A cut turn is completed on top of it, since
// the stream after it never arrived. Empty when the commit carried no body.
func (w *failoverResponseWriter) Prefix() []byte {
	if w.delivered == 0 {
		return nil
	}
	return append([]byte(nil), w.body.Bytes()[:w.delivered]...)
}

// stream is the stream this attempt took, prefix excluded.
func (w *failoverResponseWriter) stream() []byte {
	if w.delivered > w.body.Len() {
		return nil
	}
	return w.body.Bytes()[w.delivered:]
}

// StreamLen is the size of the stream the attempt took, prefix excluded.
func (w *failoverResponseWriter) StreamLen() int {
	return w.body.Len() - w.delivered
}

// streamStart is how much of the turn body the client already has, i.e. how
// much of a replayed copy of the body is already sent. A commit that carried
// bytes wrote all of them, so the whole prefix counts; a commit that carried
// nothing sent none of them, so the body starts over.
func (w *failoverResponseWriter) streamStart() int {
	return w.delivered
}

// deliver sends the rest of a cut turn — the stream that was read but, because
// this attempt is the last one, never went out — and returns the attempt with
// it recorded, so the turn can be closed on top of everything sent so far.
func (w *failoverResponseWriter) deliver(streamBody []byte, statusCode int) *failoverResponseWriter {
	hw := newSSEHeartbeatResponseWriter(w)
	hw.WriteHeader(statusCode)
	start := w.streamStart()
	if start > len(streamBody) {
		// The commit delivered more than this body holds; nothing is left over.
		start = len(streamBody)
	}
	_, _ = hw.Write(streamBody[start:])
	return w
}

func (w *failoverResponseWriter) recordsForRetry() {}

func (w *failoverResponseWriter) Header() http.Header {
	if w.passthrough {
		return w.base.Header()
	}
	return w.header
}

func (w *failoverResponseWriter) WriteHeader(statusCode int) {
	if w.discarded {
		return
	}
	if w.passthrough {
		if !w.wroteHeader {
			w.base.WriteHeader(statusCode)
			w.wroteHeader = true
		}
		return
	}
	if !w.canDefer || (statusCode > 0 && statusCode < 400) {
		w.commitHeaders(statusCode)
		return
	}
	w.status = statusCode
	w.wroteHeader = true
}

func (w *failoverResponseWriter) Write(p []byte) (int, error) {
	if w.discarded {
		return len(p), nil
	}
	if w.passthrough {
		// Committed already: everything from here on goes straight to the
		// client, and is kept so a cut turn can be reported as delivered.
		// A partial write is not a write: the bytes the client did not take are
		// reported as unwritten so the caller stops copying and the client-gone
		// path is reached instead of the stream looking fully delivered.
		n, err := w.base.Write(p)
		w.body.Write(p[:n])
		w.delivered = w.body.Len()
		return n, err
	}
	// A deferred attempt keeps its stream to itself until the loop settles on
	// it: a Write here must not commit the headers (writeBufferedStream calls
	// FlushError for that), or a discarded attempt would already be on the wire.
	return w.buf.Write(p)
}

func (w *failoverResponseWriter) Flush() {
	if w.passthrough {
		if f, ok := w.base.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// commitHeaders records this attempt's status; the bytes and the real
// WriteHeader wait for FlushError, once the failover loop has settled on this
// attempt. Error statuses never wait here: the failover decision (and the
// Retry-After it reads back) needs the status as soon as the upstream says it.
func (w *failoverResponseWriter) commitHeaders(statusCode int) {
	if w.discarded || w.passthrough {
		return
	}
	dst := w.base.Header()
	for key, values := range w.header {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
	if statusCode != 0 {
		w.base.WriteHeader(statusCode)
		w.status = statusCode
	} else if w.status != 0 {
		w.base.WriteHeader(w.status)
	}
	w.wroteHeader = true
	w.passthrough = true
	if w.buf.Len() > 0 {
		// These bytes leave with the headers, ahead of anything streamed after
		// them; the attempt keeps them as the start of its recorded turn, so a
		// cut turn is completed on top of what the client already has.
		_, _ = w.base.Write(w.buf.Bytes())
		_, _ = w.body.Write(w.buf.Bytes())
		w.delivered = w.body.Len()
		w.buf.Reset()
	}
}

func (w *failoverResponseWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *failoverResponseWriter) BufferedBody() []byte {
	return append([]byte(nil), w.buf.Bytes()...)
}

func (w *failoverResponseWriter) Discard() {
	w.discarded = true
	w.buf.Reset()
	w.body.Reset()
	w.delivered = 0
	w.header = make(http.Header)
	w.status = 0
	w.wroteHeader = false
	w.passthrough = false
}

func (w *failoverResponseWriter) FlushError() {
	if w.passthrough || w.discarded {
		return
	}
	status := w.Status()
	w.commitHeaders(status)
}

// markProviderUnavailable records that providerID should show as
// "unavailable"/"异常" until the background recovery loop re-probes it.
// hard failures use the longer recovery interval; soft (repeated transient)
// failures use a shorter TTL. Safe for concurrent use.
func (s *Server) markProviderUnavailable(providerID string, hard bool, reason, retryAfter string, body []byte) {
	s.setProviderUnavailable(providerID, hard, reason, retryAfter, body, true)
}

// refreshProviderUnavailableCountdown extends nextRetryAt without re-logging
// a fresh "marked unavailable" event (used by the recovery loop).
func (s *Server) refreshProviderUnavailableCountdown(providerID string, hard bool, reason, retryAfter string, body []byte) {
	s.setProviderUnavailable(providerID, hard, reason, retryAfter, body, false)
}

// markProviderAvailable clears any tracked "unavailable" cooldown for
// providerID, e.g. once a live request or background probe succeeds again.
func (s *Server) markProviderAvailable(providerID string) {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return
	}
	s.providerAvailabilityMu.Lock()
	if s.providerAvailability != nil {
		delete(s.providerAvailability, providerID)
	}
	if s.providerSoftFailures != nil {
		delete(s.providerSoftFailures, providerID)
	}
	s.providerAvailabilityMu.Unlock()
}

// providerNextRetry reports the next scheduled recovery-probe time for
// providerID and whether it is currently tracked as unavailable.
func (s *Server) providerNextRetry(providerID string) (time.Time, bool) {
	s.providerAvailabilityMu.Lock()
	defer s.providerAvailabilityMu.Unlock()
	st, ok := s.providerAvailability[providerID]
	return st.nextRetry, ok
}

// providerUnavailableIDs snapshots the currently-tracked unavailable provider
// IDs, for the background recovery loop to re-probe each cycle.
func (s *Server) providerUnavailableIDs() []string {
	s.providerAvailabilityMu.Lock()
	defer s.providerAvailabilityMu.Unlock()
	ids := make([]string, 0, len(s.providerAvailability))
	for id := range s.providerAvailability {
		ids = append(ids, id)
	}
	return ids
}

func (s *Server) providerUnavailableSnapshot(providerID string) (providerUnavailableState, bool) {
	s.providerAvailabilityMu.Lock()
	defer s.providerAvailabilityMu.Unlock()
	st, ok := s.providerAvailability[providerID]
	return st, ok
}

func (s *Server) bumpSoftFailure(providerID string) int {
	s.providerAvailabilityMu.Lock()
	defer s.providerAvailabilityMu.Unlock()
	if s.providerSoftFailures == nil {
		s.providerSoftFailures = make(map[string]providerSoftFailureState)
	}
	now := time.Now()
	st := s.providerSoftFailures[providerID]
	if st.count == 0 || now.Sub(st.windowStart) > providerSoftFailureWindow {
		st = providerSoftFailureState{count: 1, windowStart: now}
	} else {
		st.count++
	}
	s.providerSoftFailures[providerID] = st
	return st.count
}

func (s *Server) clearSoftFailures(providerID string) {
	s.providerAvailabilityMu.Lock()
	defer s.providerAvailabilityMu.Unlock()
	if s.providerSoftFailures != nil {
		delete(s.providerSoftFailures, providerID)
	}
}

// cachedClaudeFiveHourUtilization returns the (possibly stale) Claude OAuth
// 5h utilization for providerID when a usage report is cached.
func (s *Server) cachedClaudeFiveHourUtilization(providerID string) (float64, bool) {
	if s == nil || s.oauthUsageCache == nil {
		return 0, false
	}
	cached, ok := s.oauthUsageCache.getAllowStale("claude:" + providerID)
	if !ok {
		return 0, false
	}
	report, ok := cached.(ClaudeOAuthUsageReport)
	if !ok || !report.Available || report.FiveHour == nil {
		return 0, false
	}
	return report.FiveHour.Utilization, true
}

func (s *Server) classifyProviderFailover(providerID string, status int, body []byte, err error) (failoverClass, string) {
	var utilPtr *float64
	if util, ok := s.cachedClaudeFiveHourUtilization(providerID); ok {
		u := util
		utilPtr = &u
	}
	return classifyProviderFailover(status, body, err, utilPtr)
}

const (
	// providerCooldownFloor is the shortest rest any cooldown lasts, even when
	// the upstream says "retry now".
	providerCooldownFloor = 5 * time.Second
	// providerCooldownCap bounds what an upstream may talk the gateway into: a
	// vendor that answers "reset in three days" must not take an account out of
	// rotation until then, because the recovery loop is what proves it back.
	providerCooldownCap = 60 * time.Minute
)

// providerCooldownHint is what an upstream says about when to come back.
type providerCooldownHint struct {
	retryAfter time.Duration
	// resetsAt is an absolute moment (ms epoch) when the account's window
	// reopens; zero when the vendor did not say.
	resetsAt time.Time
}

func (h providerCooldownHint) ok() bool {
	return h.retryAfter > 0 || !h.resetsAt.IsZero()
}

// cooldownHint reads the upstream's own retry timing: the Retry-After header,
// plus what the vendor already told the usage panel — Claude's unified 5h/7d
// window and ChatGPT's primary/secondary rate-limit window both carry the exact
// moment the account reopens, which beats any guess when the 429 body is terse.
func (s *Server) cooldownHint(providerID, retryAfter string, body []byte, now time.Time) providerCooldownHint {
	hint := parseProviderCooldownHint(retryAfter, body, now)
	if s == nil || s.oauthUsageCache == nil {
		return hint
	}
	if cached, ok := s.oauthUsageCache.getAllowStale("claude:" + providerID); ok {
		if report, ok := cached.(ClaudeOAuthUsageReport); ok && report.Available {
			for _, bucket := range []*ClaudeOAuthUsageBucket{report.FiveHour, report.SevenDay} {
				if bucket == nil {
					continue
				}
				if at, ok := parseResetsAt(bucket.ResetsAt); ok && at.After(now) && at.After(hint.resetsAt) {
					hint.resetsAt = at
				}
			}
		}
	}
	if cached, ok := s.oauthUsageCache.getAllowStale("chatgpt:" + providerID); ok {
		if report, ok := cached.(ChatGPTOAuthUsageReport); ok && report.Available {
			for _, bucket := range report.Buckets {
				if at, ok := parseResetsAt(bucket.ResetsAt); ok && at.After(now) && at.After(hint.resetsAt) {
					hint.resetsAt = at
				}
			}
		}
	}
	return hint
}

// cooldownTTL is how long providerID rests: the upstream's own hint when it
// names a time, the class default otherwise. Only "how long" comes from the
// upstream — whether the mark shows as hard still comes from the classifier,
// so a vendor phrase alone never paints an account 异常.
func (s *Server) cooldownTTL(providerID, retryAfter string, body []byte, hard bool, now time.Time) time.Duration {
	ttl := providerFailoverRecoveryInterval
	if !hard {
		ttl = providerSoftUnavailableTTL
	}
	hint := s.cooldownHint(providerID, retryAfter, body, now)
	want := hint.retryAfter
	if !hint.resetsAt.IsZero() {
		if until := hint.resetsAt.Sub(now); until > want {
			want = until
		}
	}
	if !hint.ok() || want <= 0 {
		return ttl
	}
	if hard {
		// Spent until the vendor says otherwise: between the soft default and a
		// quarter hour there is no reason to probe a dead quota.
		if ttl < providerCooldownFloor*3 {
			ttl = providerCooldownFloor * 3
		}
	}
	if want < ttl {
		want = ttl
	}
	if want > providerCooldownCap {
		want = providerCooldownCap
	}
	return want
}

// parseProviderCooldownHint extracts the upstream's retry timing from a
// Retry-After header (seconds or HTTP date) and from the body's reset hints:
// Anthropic's unified `resets_at` (RFC3339 or ms epoch), Codex's
// `resets_in_seconds` / `resets_at`, and the same fields under `error` or
// `error.details`. Bodies that name no time fall back to the class default.
func parseProviderCooldownHint(retryAfter string, body []byte, now time.Time) providerCooldownHint {
	var hint providerCooldownHint
	if d, ok := parseRetryAfterHeader(retryAfter, now); ok {
		hint.retryAfter = d
	}
	payload := decodeErrorPayload(body)
	resetCandidates := []string{
		payloadString(payload, "resets_at"),
		payloadString(payload, "error", "details", "resets_at"),
		payloadString(payload, "error", "resets_at"),
	}
	for _, raw := range resetCandidates {
		if at, ok := parseResetsAt(raw); ok && at.After(now) {
			hint.resetsAt = at
			break
		}
	}
	// Resets are usually carried beside the error code at the top level, but
	// some envelopes nest them one level down.
	for _, src := range []map[string]any{
		payload,
		payloadMap(payload, "error"),
		payloadMap(payload, "error", "details"),
	} {
		if secs, ok := numericSeconds(src["resets_in_seconds"]); ok && secs > 0 {
			if d := secondsDuration(secs); d > hint.retryAfter {
				hint.retryAfter = d
			}
			break
		}
	}
	return hint
}

// decodeErrorPayload unwraps the handful of envelope shapes upstreams use
// ({"error":{...}}, {"error":{"details":{...}}}, a bare object, an SSE
// "data:" line) down to the innermost error object worth reading hints from.
func decodeErrorPayload(body []byte) map[string]any {
	text := strings.TrimSpace(string(body))
	if idx := strings.Index(text, "data:"); idx >= 0 && !strings.HasPrefix(text, "{") {
		text = strings.TrimSpace(text[idx+len("data:"):])
	}
	var top map[string]any
	if err := json.Unmarshal([]byte(text), &top); err != nil {
		// Some vendors wrap the JSON in prose; read what round-trips.
		start := strings.Index(text, "{")
		end := strings.LastIndex(text, "}")
		if start < 0 || end <= start {
			return map[string]any{}
		}
		if err := json.Unmarshal([]byte(text[start:end+1]), &top); err != nil {
			return map[string]any{}
		}
	}
	return top
}

func payloadMap(payload map[string]any, keys ...string) map[string]any {
	current := payload
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			return map[string]any{}
		}
		current = next
	}
	return current
}

// payloadString walks keys as objects and returns the first string leaf found.
func payloadString(payload map[string]any, keys ...string) string {
	current := payload
	for i, key := range keys {
		value, ok := current[key]
		if !ok {
			return ""
		}
		if i == len(keys)-1 {
			return strings.TrimSpace(stringValue(value))
		}
		nested, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		current = nested
	}
	return ""
}

// parseResetsAt accepts Anthropic's RFC3339 timestamp and the ms-epoch number
// (or numeric string) Codex and the Claude usage endpoint use.
func parseResetsAt(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if at, err := time.Parse(time.RFC3339, raw); err == nil {
		return at, true
	}
	if ms, ok := numericSeconds(raw); ok && ms > 0 {
		return time.UnixMilli(int64(ms)), true
	}
	return time.Time{}, false
}

// parseRetryAfterHeader reads the seconds or HTTP-date form of Retry-After.
func parseRetryAfterHeader(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if secs, ok := numericSeconds(raw); ok {
		if secs <= 0 {
			return 0, false
		}
		return secondsDuration(secs), true
	}
	if at, err := http.ParseTime(raw); err == nil {
		if d := at.Sub(now); d > 0 {
			return d, true
		}
		return 0, false
	}
	return 0, false
}

func numericSeconds(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}

func secondsDuration(secs float64) time.Duration {
	if secs > float64(providerCooldownCap/time.Second) {
		return providerCooldownCap
	}
	return time.Duration(secs * float64(time.Second))
}

// setProviderUnavailable cools providerID down until the recovery loop proves
// it back. retryAfter/body carry the upstream's own timing hint; hard failures
// rest at least providerFailoverRecoveryInterval (see cooldownTTL).
func (s *Server) setProviderUnavailable(providerID string, hard bool, reason, retryAfter string, body []byte, logMark bool) {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return
	}
	now := time.Now()
	ttl := s.cooldownTTL(providerID, retryAfter, body, hard, now)
	reason = truncateReason(reason)
	s.providerAvailabilityMu.Lock()
	if s.providerAvailability == nil {
		s.providerAvailability = make(map[string]providerUnavailableState)
	}
	s.providerAvailability[providerID] = providerUnavailableState{
		nextRetry: now.Add(ttl),
		hard:      hard,
		reason:    reason,
	}
	s.providerAvailabilityMu.Unlock()
	if !logMark {
		return
	}
	kind := "soft"
	if hard {
		kind = "hard"
	}
	msg := fmt.Sprintf("provider=%s kind=%s reason=%s retry_in=%s", providerID, kind, reason, ttl.Round(time.Second))
	if s.logs != nil {
		s.logs.AddApp("warn", "provider marked unavailable", msg)
	}
	slog.Warn("provider marked unavailable", "provider", providerID, "hard", hard, "reason", reason, "retry_in", ttl)
}

// applyProviderAvailability overlays the live-request availability tracker
// onto a provider destined for the client: a provider that just failed a real
// API call shows healthStatus "unavailable" (rendered "异常" in the console)
// with nextRetryAt, even if its persisted HealthStatus still reflects an
// earlier manual "获取模型" check.
func (s *Server) applyProviderAvailability(provider *domain.Provider) {
	if provider == nil {
		return
	}
	if provider.Disabled {
		// 禁用后不展示「异常 / N秒后重试」覆盖层，也不诱导后台重探。
		provider.NextRetryAt = ""
		return
	}
	nextRetry, unavailable := s.providerNextRetry(provider.ID)
	if !unavailable {
		return
	}
	provider.HealthStatus = "unavailable"
	provider.NextRetryAt = nextRetry.Format(time.RFC3339)
}

// recordProviderRequestOutcome updates the runtime availability tracker for
// providerID based on one completed upstream attempt. Failover-worthy soft
// failures only paint "异常" after consecutive hits; hard quota/auth failures
// mark immediately. Independent of whether a fallback chain exists.
func (s *Server) recordProviderRequestOutcome(providerID string, status int, body []byte, err error) {
	_ = s.applyProviderFailoverOutcome(providerID, status, body, "", err)
}

// applyProviderFailoverOutcome classifies the attempt, updates soft/hard
// availability tracking, and returns whether the caller should advance to the
// next backup provider. retryAfter is the attempt's upstream Retry-After header,
// which (with the body's reset hints) decides how long a cooldown lasts.
func (s *Server) applyProviderFailoverOutcome(providerID string, status int, body []byte, retryAfter string, err error) bool {

	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return false
	}
	if s.router != nil {
		if provider, lookupErr := s.router.ProviderByID(providerID); lookupErr == nil && provider.Disabled {
			// 已禁用：不再写入 unavailable，也不参与后续重探刷日志。
			s.markProviderAvailable(providerID)
			class, _ := s.classifyProviderFailover(providerID, status, body, err)
			return class != failoverNone
		}
	}
	class, reason := s.classifyProviderFailover(providerID, status, body, err)
	if class == failoverNone {
		// The vendor's own quota wording outlives the classifier: a 400 that
		// spells out insufficient_quota advances the chain but is not worth a
		// cooldown mark, while a 429 that does is already soft here.
		if err == nil {
			s.markProviderAvailable(providerID)
		}
		return false
	}
	switch class {
	case failoverHard:
		s.clearSoftFailures(providerID)
		s.markProviderUnavailable(providerID, true, reason, retryAfter, body)
	case failoverSoft:
		count := s.bumpSoftFailure(providerID)
		if count >= providerSoftFailureThreshold {
			s.markProviderUnavailable(providerID, false, fmt.Sprintf("%s consecutive=%d", reason, count), retryAfter, body)
		} else if s.logs != nil {
			s.logs.AddApp("info", "provider soft failure", fmt.Sprintf(
				"provider=%s consecutive=%d/%d reason=%s",
				providerID, count, providerSoftFailureThreshold, reason,
			))
		}
	case failoverPromptRefusal:
		// The account is not the problem: ask the next provider, mark nothing.
	}
	return true
}

func (s *Server) persistAPIKeyActiveProvider(keyID, providerID string) {
	updated, err := s.router.SetAPIKeyActiveProvider(keyID, providerID)
	if err != nil {
		return
	}
	if s.apiKeyStore != nil {
		_ = s.apiKeyStore.UpdateAPIKey(updated)
	}
}

// errProviderNotAssigned marks a request refused because the API key's owner is
// no longer granted any provider on the key's chain. Handlers map it to 403 so
// the client and the request log show a permission denial instead of a
// misleading 502 Bad Gateway.
var errProviderNotAssigned = errors.New("permission denied")

// upstreamErrorStatus maps an execute-flow error to the client-facing status.
// Permission denials are 403, a provider at its own concurrent limit or a key at
// its own quota is 429 (both with Retry-After), everything else stays a 502
// upstream failure.
func upstreamErrorStatus(err error) int {
	if errors.Is(err, errProviderNotAssigned) {
		return http.StatusForbidden
	}
	if errors.Is(err, errProviderSaturated) {
		return http.StatusTooManyRequests
	}
	if errors.Is(err, errKeyQuotaExceeded) {
		return http.StatusTooManyRequests
	}
	return http.StatusBadGateway
}

// executeProtocolFlowWithFailover works through the key's whole provider chain
// (see failoverAttemptOrder) when an upstream failure looks like
// quota/auth/availability, instead of stopping at the end of the chain behind
// the sticky provider.
func (s *Server) executeProtocolFlowWithFailover(
	w http.ResponseWriter,
	r *http.Request,
	route domain.Route,
	decision domain.RouteDecision,
	model string,
	req map[string]any,
	clientProtocol domain.Protocol,
	skipIncomingAuth bool,
	matchedKey domain.APIKey,
	gatewayKeyMatched bool,
) (int, TokenUsage, []byte, domain.RouteDecision, string, error) {
	// The key's own quota is decided before any provider is touched: switching
	// accounts would not help, since every provider on the chain is refused by the
	// same key, and a refusal that reaches an upstream spends that account's
	// quota for nothing. Headers go on `w` here because the handlers still own the
	// body — they write it in their own protocol shape once WriteHeader is called.
	if gatewayKeyMatched && keyHasQuota(matchedKey) {
		admission := s.admitKeyQuota(matchedKey, estimateKeyQuotaPrompt(req), time.Now())
		if refusal := admission.refusal; refusal != nil {
			w.Header().Set("Retry-After", strconv.FormatInt(refusal.RetryAfterSeconds, 10))
			w.Header().Set("X-Quota-Reset-At", refusal.ResetAt)
			return http.StatusTooManyRequests, TokenUsage{}, nil, decision, model, refusal
		}
		defer admission.release()
	}

	if !gatewayKeyMatched {
		timer := startAttempt()
		status, usage, body, err := s.executeProtocolFlow(w, r, route, decision, model, req, clientProtocol, skipIncomingAuth)
		s.recordProviderRequestOutcome(decision.ProviderID, status, body, err)
		s.noteProviderUsage(decision.ProviderID, usage)
		noteSingleAttemptTrace(r.Context(), decision.ProviderID, model, status, usage, err, timer.millis())
		return status, usage, body, decision, model, err
	}

	chain := apiKeyProviderChain(route.ProviderID, matchedKey.FallbackProviderIDs)
	if len(chain) == 0 {
		timer := startAttempt()
		status, usage, body, err := s.executeProtocolFlow(w, r, route, decision, model, req, clientProtocol, skipIncomingAuth)
		s.recordProviderRequestOutcome(decision.ProviderID, status, body, err)
		s.noteProviderUsage(decision.ProviderID, usage)
		noteSingleAttemptTrace(r.Context(), decision.ProviderID, model, status, usage, err, timer.millis())
		return status, usage, body, decision, model, err
	}
	// 普通用户的 Key 只能使用管理员当前授权的 Provider。授权是在“用户”页面随时可
	// 改的，而 Key 的路由/兜底链是创建时写死的，所以必须在每次请求时重新校验，
	// 不能只依赖创建 Key 时的校验（否则撤销授权后旧 Key 会一直可用）。
	// 先过滤整条链再排尝试顺序，这样即使 ActiveProviderID 指向已被撤销的 Provider，
	// 也不会跳过链上仍被授权的首选 Provider。
	if allowedChain := s.allowedProviderChainForKey(matchedKey, chain); len(allowedChain) != len(chain) {
		for _, providerID := range chain {
			if !s.providerAllowedForKeyOwner(matchedKey, providerID) {
				s.logs.AddApp("warn", "provider not assigned to key owner, skipped", fmt.Sprintf("key=%s owner=%s provider=%s", matchedKey.ID, matchedKey.OwnerUserID, providerID))
			}
		}
		chain = allowedChain
		if len(chain) == 0 {
			return http.StatusForbidden, TokenUsage{}, nil, decision, model,
				fmt.Errorf("%w: provider %q is not assigned to your account", errProviderNotAssigned, strings.TrimSpace(route.ProviderID))
		}
	}
	attempts := s.failoverAttemptOrder(scheduleInput{
		chain:   chain,
		active:  matchedKey.ActiveProviderID,
		mode:    matchedKey.ScheduleMode,
		keyID:   matchedKey.ID,
		advance: true,
	})
	// A conversation stays on the account that has its prompt cache, even when
	// another conversation on this same key has moved the key-level sticky
	// provider somewhere else.
	sessionKey := sessionAffinityKey(r, req, matchedKey.ID)
	attempts, pinnedProviderID := s.applySessionPin(sessionKey, attempts)
	noteScheduleDecision(r.Context(), matchedKey.ScheduleMode, attempts, pinnedProviderID)

	var lastStatus int
	var lastUsage TokenUsage
	var lastBody []byte
	var lastErr error
	var lastDecision domain.RouteDecision = decision
	lastModel := model
	requestModel, _ := req["model"].(string)
	// 普通用户的 Key（有归属用户）不能使用被管理员禁用的 Provider；
	// 管理员自己的 Key（无归属）不受限制。
	ownerRestricted := strings.TrimSpace(matchedKey.OwnerUserID) != ""

	for i, providerID := range attempts {
		if ownerRestricted {
			if provider, err := s.router.ProviderByID(providerID); err == nil && provider.Disabled {
				lastErr = fmt.Errorf("provider %q is disabled by administrator", providerID)
				s.logs.AddApp("warn", "provider disabled, skipped for user key", fmt.Sprintf("key=%s provider=%s", matchedKey.ID, providerID))
				noteScheduleAttempt(r.Context(), monitor.RequestAttempt{ProviderID: providerID, Reason: attemptReasonDisabled, Skipped: true})
				continue
			}
		}
		attemptDecision, err := s.router.DecideForProvider(route.ID, providerID)
		if err != nil {
			lastErr = err
			noteScheduleAttempt(r.Context(), monitor.RequestAttempt{ProviderID: providerID, Reason: attemptReasonUndecidable, Error: truncateReason(err.Error()), Skipped: true})
			continue
		}
		lastDecision = attemptDecision

		attemptModel := model
		attemptKey := matchedKey
		if providerID != strings.TrimSpace(route.ProviderID) {
			override := ""
			if matchedKey.FallbackModelOverrides != nil {
				override = strings.TrimSpace(matchedKey.FallbackModelOverrides[providerID])
			}
			// 同协议备选沿用请求模型；跨协议备选必须锁定自己的模型，否则把
			// 上游 A 的模型名发给协议不同的上游 B 只会换来一个 400。
			if override == "" && s.router.fallbackModelRequired(route.ProviderID, providerID) {
				s.logs.AddApp("warn", "provider failover skipped missing model override", fmt.Sprintf("key=%s provider=%s", matchedKey.ID, providerID))
				noteScheduleAttempt(r.Context(), monitor.RequestAttempt{ProviderID: providerID, Reason: attemptReasonMissingModel, Skipped: true})
				continue
			}
			if override != "" {
				attemptKey.ModelOverride = override
			}
		}
		attemptProvider, _ := s.router.ProviderByID(providerID)
		if resolved, _ := resolveConsumerModelOnProvider(attemptProvider, attemptKey, true, requestModel); strings.TrimSpace(resolved) != "" {
			attemptModel = resolved
		}
		attemptReq := cloneRequestMap(req)
		attemptReq["model"] = attemptModel

		canDefer := i < len(attempts)-1
		writer := newFailoverResponseWriter(w, canDefer)
		timer := startAttempt()
		status, usage, body, execErr := s.executeProtocolFlow(writer, r, route, attemptDecision, attemptModel, attemptReq, clientProtocol, skipIncomingAuth)
		elapsed := timer.millis()
		if execErr != nil {
			lastStatus, lastUsage, lastBody, lastErr, lastModel = status, usage, nil, execErr, attemptModel
			// Client already gone: never advance the chain (backup would reuse the
			// same canceled context and surface a fake GLM/fallback cancel).
			if isRequestContextCanceled(execErr) || (r.Context() != nil && r.Context().Err() != nil) {
				if !writer.passthrough && writer.buf.Len() == 0 && writer.status == 0 {
					writer.Discard()
				} else {
					writer.FlushError()
				}
				noteScheduleAttempt(r.Context(), monitor.RequestAttempt{ProviderID: providerID, Model: attemptModel, Status: status, Ms: elapsed, Reason: attemptReasonClientGone, Error: truncateReason(execErr.Error())})
				return status, usage, body, attemptDecision, attemptModel, execErr
			}
			// Busy, not broken: an admission refusal moves to the next account
			// and must never be counted as a provider failure, or a busy but
			// healthy account would be painted 异常 and cooled down.
			if errors.Is(execErr, errProviderSaturated) {
				lastStatus = http.StatusTooManyRequests
				noteScheduleAttempt(r.Context(), monitor.RequestAttempt{ProviderID: providerID, Model: attemptModel, Status: http.StatusTooManyRequests, Ms: elapsed, Reason: attemptReasonSaturated, Error: truncateReason(execErr.Error()), Deferred: canDefer})
				if canDefer {
					writer.Discard()
					markTimingFlag(r.Context(), timingFlagFailoverRetry)
					if t := requestTimingFrom(r.Context()); t != nil {
						t.resetUpstreamMarks()
					}
					s.logs.AddApp("info", "provider skipped at concurrent limit", fmt.Sprintf("key=%s provider=%s err=%s", matchedKey.ID, providerID, execErr.Error()))
					continue
				}
				writer.Discard()
				// The account's lane, not the row's: a grouped provider's
				// budget lives under its LaneGroup key.
				laneWait := providerRetryAfterSeconds
				if provider, lookupErr := s.router.ProviderByID(providerID); lookupErr == nil {
					laneWait = s.providerLanes.retryAfterSecondsFor(provider, time.Now())
				}
				w.Header().Set("Retry-After", strconv.Itoa(laneWait))
				return http.StatusTooManyRequests, TokenUsage{}, nil, attemptDecision, attemptModel, execErr
			}
			transportFailoverWorthy := s.applyProviderFailoverOutcome(providerID, 0, nil, "", execErr)
			// Transport errors are worth another account only while nothing of
			// this turn has reached the client. Once the first byte is out —
			// a streamed request, or a deferred attempt whose turn was
			// committed — the client's time-to-first-token has been paid, so a
			// cut is the answer: it stays with the account that started it.
			if !writer.deferring() {
				transportFailoverWorthy = false
			}
			transportReason := attemptReasonTransportError
			if errors.Is(execErr, errUpstreamFirstPacketTimeout) {
				transportReason = attemptReasonFirstPacketTimeout
			}
			noteScheduleAttempt(r.Context(), monitor.RequestAttempt{ProviderID: providerID, Model: attemptModel, Ms: elapsed, Reason: transportReason, Error: truncateReason(execErr.Error()), Deferred: canDefer && transportFailoverWorthy})
			if canDefer && transportFailoverWorthy {
				writer.Discard()
				markTimingFlag(r.Context(), timingFlagFailoverRetry)
				if t := requestTimingFrom(r.Context()); t != nil {
					t.resetUpstreamMarks()
				}
				s.logs.AddApp("warn", "provider failover after transport error", fmt.Sprintf("key=%s from=%s err=%s", matchedKey.ID, providerID, execErr.Error()))
				s.releaseSessionPin(sessionKey, pinnedProviderID, providerID)
				continue
			}
			// 尚未向客户端提交任何字节时交给上层写错误响应（例如空流重试耗尽）。
			if !writer.passthrough && writer.buf.Len() == 0 && writer.status == 0 {
				writer.Discard()
			} else {
				writer.FlushError()
			}
			return status, usage, body, attemptDecision, attemptModel, execErr
		}

		respBody := body
		if len(respBody) == 0 {
			respBody = writer.BufferedBody()
		}
		lastStatus, lastUsage, lastBody, lastErr, lastModel = status, usage, respBody, nil, attemptModel

		if r.Context() != nil && r.Context().Err() != nil {
			// Upstream may have returned an error status, but the client is
			// already gone — do not burn the fallback chain.
			if !writer.passthrough {
				writer.FlushError()
			}
			noteScheduleAttempt(r.Context(), monitor.RequestAttempt{ProviderID: providerID, Model: attemptModel, Status: status, Ms: elapsed, Reason: attemptReasonClientGone})
			return status, usage, respBody, attemptDecision, attemptModel, r.Context().Err()
		}

		attemptFailoverWorthy := s.applyProviderFailoverOutcome(providerID, status, respBody, writer.UpstreamHeader("Retry-After"), nil)
		if canDefer && attemptFailoverWorthy {
			noteScheduleAttempt(r.Context(), monitor.RequestAttempt{ProviderID: providerID, Model: attemptModel, Status: status, Ms: elapsed, Reason: attemptFailoverReason(respBody), Error: summarizeUpstreamHTTPError(status, respBody), Deferred: true})
			writer.Discard()
			markTimingFlag(r.Context(), timingFlagFailoverRetry)
			if t := requestTimingFrom(r.Context()); t != nil {
				t.resetUpstreamMarks()
			}
			s.logs.AddApp("warn", "provider failover after upstream error", fmt.Sprintf("key=%s from=%s status=%d", matchedKey.ID, providerID, status))
			s.releaseSessionPin(sessionKey, pinnedProviderID, providerID)
			continue
		}

		// Stick to the provider that served this request — unless a session pin
		// is what chose it. Two conversations pinned to two different accounts
		// would otherwise rewrite ActiveProviderID on every turn, and the
		// key-level stickiness would flap between them without either
		// conversation having moved.
		desiredActive := ""
		if providerID != strings.TrimSpace(route.ProviderID) {
			desiredActive = providerID
		}
		pinnedThisTurn := pinnedProviderID != "" && providerID == pinnedProviderID
		if !pinnedThisTurn && strings.TrimSpace(matchedKey.ActiveProviderID) != desiredActive {
			s.persistAPIKeyActiveProvider(matchedKey.ID, desiredActive)
			if desiredActive != "" {
				s.logs.AddApp("info", "api key active provider updated", fmt.Sprintf("key=%s provider=%s", matchedKey.ID, desiredActive))
			} else {
				s.logs.AddApp("info", "api key restored preferred provider", fmt.Sprintf("key=%s provider=%s", matchedKey.ID, route.ProviderID))
			}
		}
		// The conversation now belongs to the account that answered it, and that
		// account has now carried this request's work.
		s.setSessionPin(sessionKey, providerID)
		s.noteProviderUsage(providerID, usage)
		noteScheduleAttempt(r.Context(), monitor.RequestAttempt{ProviderID: providerID, Model: attemptModel, Status: status, Ms: elapsed, Reason: attemptReasonServed})
		if !writer.passthrough {
			writer.FlushError()
		}
		return status, usage, respBody, attemptDecision, attemptModel, nil
	}

	if lastErr != nil {
		return lastStatus, lastUsage, lastBody, lastDecision, lastModel, lastErr
	}
	return lastStatus, lastUsage, lastBody, lastDecision, lastModel, nil
}

// attemptFailoverReason names why an otherwise successful-looking attempt went
// to the next account: a stream the upstream cut mid-answer is not an HTTP
// error, so it must not be reported as one.
func attemptFailoverReason(body []byte) string {
	if isUpstreamStreamInterruption(errors.New(string(body))) {
		return attemptReasonStreamInterrupted
	}
	return attemptReasonUpstreamError
}

func cloneRequestMap(req map[string]any) map[string]any {
	if req == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(req))
	for key, value := range req {
		out[key] = value
	}
	return out
}

// providerFailoverRecoveryInterval 控制回切探测周期：前置 Provider 恢复后，
// 最多等待一个周期即可自动切回，避免长时间滞留在备用 Provider。
const providerFailoverRecoveryInterval = 2 * time.Minute

// StartProviderFailoverRecovery periodically probes higher-priority providers
// for keys stuck on a fallback, and switches back when one becomes available.
func (s *Server) StartProviderFailoverRecovery(ctx context.Context) {
	go func() {
		// 启动后先等一小段，避开冷启动阶段的探测抖动。
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				s.recoverAPIKeyPreferredProviders(context.Background())
				s.reprobeUnavailableProviders(context.Background())
				timer.Reset(providerFailoverRecoveryInterval)
			}
		}
	}()
}

func (s *Server) recoverAPIKeyPreferredProviders(ctx context.Context) {
	keys := s.router.State().APIKeys
	// 同一周期内每个 Provider 只探测一次，多个 Key 共用同一前置 Provider 时复用结果。
	probeCache := make(map[string]bool)
	for _, key := range keys {
		if !key.Enabled {
			continue
		}
		route, err := s.router.RouteByID(key.RouteID)
		if err != nil {
			continue
		}
		chain := apiKeyProviderChain(route.ProviderID, key.FallbackProviderIDs)
		if len(chain) <= 1 {
			continue
		}
		activeIdx := apiKeyEffectiveProviderIndex(key.ActiveProviderID, chain)
		if activeIdx <= 0 {
			// 未处于备用状态：无需探测，零开销跳过。
			continue
		}
		for i := 0; i < activeIdx; i++ {
			providerID := chain[i]
			available, cached := probeCache[providerID]
			if !cached {
				provider, err := s.router.ProviderByID(providerID)
				if err != nil || provider.Disabled {
					// 已删除或管理员禁用：不当作可回切目标，也不发探测流量。
					probeCache[providerID] = false
					continue
				}
				available = s.probeProviderAvailable(ctx, provider)
				probeCache[providerID] = available
			}
			if !available {
				continue
			}
			desiredActive := ""
			if providerID != route.ProviderID {
				desiredActive = providerID
			}
			s.persistAPIKeyActiveProvider(key.ID, desiredActive)
			s.logs.AddApp("info", "api key recovered higher-priority provider", fmt.Sprintf("key=%s provider=%s", key.ID, providerID))
			slog.Info("api key recovered higher-priority provider", "key", key.ID, "provider", providerID)
			break
		}
	}
}

// reprobeUnavailableProviders re-checks every provider currently marked
// unavailable by a live request failure (recordProviderRequestOutcome) and
// clears the mark once a probe succeeds. Providers still down get their
// retry countdown pushed to the next cycle, matching the console's
// "N秒后重试" hint. Runs on the same cadence as recoverAPIKeyPreferredProviders.
func (s *Server) reprobeUnavailableProviders(ctx context.Context) {
	for _, id := range s.providerUnavailableIDs() {
		provider, err := s.router.ProviderByID(id)
		if err != nil {
			// Provider no longer exists: stop tracking it.
			s.markProviderAvailable(id)
			continue
		}
		if provider.Disabled {
			// 管理员已禁用：停止可用性重探，避免无效 /models 刷日志。
			s.markProviderAvailable(id)
			if s.logs != nil {
				s.logs.AddApp("info", "provider unavailable cleared (disabled)", fmt.Sprintf("provider=%s", id))
			}
			continue
		}
		if s.probeProviderAvailable(ctx, provider) {
			s.markProviderAvailable(id)
			if s.logs != nil {
				s.logs.AddApp("info", "provider recovered", fmt.Sprintf("provider=%s", id))
			}
			slog.Info("provider recovered", "provider", id)
			continue
		}
		// Still unavailable: refresh the countdown for the next cycle,
		// preserving hard/soft classification from the original mark.
		prev, _ := s.providerUnavailableSnapshot(id)
		reason := prev.reason
		if reason == "" {
			reason = "reprobe_failed"
		}
		// A vendor-named reset time is kept: it survives cycles of failed
		// probes, whose own bodies carry no hint at all.
		s.refreshProviderUnavailableCountdown(id, prev.hard, reason, "", nil)
	}
}

func (s *Server) probeProviderAvailable(ctx context.Context, provider domain.Provider) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/__probe", nil)
	if err != nil {
		return false
	}
	result := s.fetchProviderModels(req, provider, time.Now())
	return result.Success
}

// decisionForAPIKey is the provider the request is attributed to before it is
// sent: the conversation's pinned account when there is one, otherwise the
// first provider the failover loop would try.
func (s *Server) decisionForAPIKey(route domain.Route, key domain.APIKey, fallback domain.RouteDecision, sessionKey string) domain.RouteDecision {
	chain := apiKeyProviderChain(route.ProviderID, key.FallbackProviderIDs)
	// Mirror the request-time authorization filter in
	// executeProtocolFlowWithFailover so the pre-flight decision (used for
	// request-log attribution) never names a provider the owner lost access to.
	if allowed := s.allowedProviderChainForKey(key, chain); len(allowed) > 0 {
		chain = allowed
	}
	if len(chain) == 0 {
		return fallback
	}
	// Attribute the request to the provider the failover loop will actually try
	// first, so the log line matches the traffic.
	attempts := s.failoverAttemptOrder(scheduleInput{
		chain:  chain,
		active: key.ActiveProviderID,
		mode:   key.ScheduleMode,
		keyID:  key.ID,
	})
	attempts, _ = s.applySessionPin(sessionKey, attempts)
	decision, err := s.router.DecideForProvider(route.ID, attempts[0])
	if err != nil {
		return fallback
	}
	return decision
}
