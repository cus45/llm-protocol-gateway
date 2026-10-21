package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func traceOfRequest(t *testing.T, f *failoverFixture) monitor.RequestAnalysis {
	t.Helper()
	logs := f.server.logs.List(10)
	for _, entry := range logs {
		if entry.APIKeyID == "k-sched" && entry.ProviderID != "" {
			return entry.Analysis
		}
	}
	t.Fatalf("no request log for k-sched in %d entries", len(logs))
	return monitor.RequestAnalysis{}
}

// withExtraFallback appends a chain entry that resolves to no provider, which is
// how a deleted-but-still-referenced account behaves at request time.
func withExtraFallback(providerID string) fixtureOption {
	return func(key *domain.APIKey) {
		key.FallbackProviderIDs = append(key.FallbackProviderIDs, providerID)
	}
}

func withoutFallbacks() fixtureOption {
	return func(key *domain.APIKey) { key.FallbackProviderIDs = nil }
}

// The trace is what makes a failed-over request explainable after the fact:
// who was chosen first, what they answered, and who actually served it.
func TestScheduleTraceRecordsTheWholeChain(t *testing.T) {
	f := newFailoverFixture(t, http.StatusServiceUnavailable, 0, "")
	if rec := f.request(t); rec.Code != http.StatusOK {
		t.Fatalf("expected the backup to serve, got %d: %s", rec.Code, rec.Body.String())
	}
	analysis := traceOfRequest(t, f)
	if got := len(analysis.Candidates); got != 2 {
		t.Fatalf("both providers should be listed as candidates, got %d", got)
	}
	if analysis.Candidates[0] != "p-pref" || analysis.Candidates[1] != "p-back" {
		t.Fatalf("unexpected candidate order: %v", analysis.Candidates)
	}
	if len(analysis.Attempts) != 2 {
		t.Fatalf("expected two attempts, got %#v", analysis.Attempts)
	}
	first, second := analysis.Attempts[0], analysis.Attempts[1]
	if first.ProviderID != "p-pref" || first.Status != http.StatusServiceUnavailable || first.Reason != attemptReasonUpstreamError || !first.Deferred {
		t.Fatalf("the failed attempt should say what happened and that it was discarded: %#v", first)
	}
	if first.Ms <= 0 {
		t.Fatalf("the attempt should carry its duration, got %d", first.Ms)
	}
	if second.ProviderID != "p-back" || second.Reason != attemptReasonServed || second.Status != http.StatusOK || second.Deferred {
		t.Fatalf("the serving attempt should be last and not deferred: %#v", second)
	}
	if analysis.ScheduleMode != "" {
		t.Fatalf("a key with no mode set should report the default, got %q", analysis.ScheduleMode)
	}
	if analysis.SessionPinned != "" {
		t.Fatalf("an anonymous request has no pin, got %q", analysis.SessionPinned)
	}
}

// The pin that chose the provider is part of the answer to "why this account".
func TestScheduleTraceRecordsTheSessionPin(t *testing.T) {
	f := newFailoverFixture(t, 0, 0, "")
	if rec := f.requestWithSession(t, "chat-a"); rec.Code != http.StatusOK {
		t.Fatalf("request failed: %d", rec.Code)
	}
	f.setPreferredStatus(http.StatusServiceUnavailable)
	if rec := f.requestWithSession(t, "chat-a"); rec.Code != http.StatusOK {
		t.Fatalf("failover request failed: %d", rec.Code)
	}
	f.setPreferredStatus(0)
	if rec := f.requestWithSession(t, "chat-a"); rec.Code != http.StatusOK {
		t.Fatalf("third request failed: %d", rec.Code)
	}
	logs := f.server.logs.List(3)
	if len(logs) == 0 {
		t.Fatal("no request logs")
	}
	analysis := logs[0].Analysis
	if analysis.SessionPinned != "p-back" {
		t.Fatalf("the third turn was chosen by its session pin, got %q (attempts %#v)", analysis.SessionPinned, analysis.Attempts)
	}
	if len(analysis.Candidates) == 0 || analysis.Candidates[0] != "p-back" {
		t.Fatalf("the pinned provider should come first in the candidate list, got %v", analysis.Candidates)
	}
	if len(analysis.Attempts) != 1 || analysis.Attempts[0].Reason != attemptReasonServed {
		t.Fatalf("the pinned account served directly, got %#v", analysis.Attempts)
	}
}

// A candidate that was never tried still shows up, with the reason it was
// skipped — otherwise "why didn't it use that account?" has no answer.
func TestScheduleTraceRecordsSkippedCandidates(t *testing.T) {
	f := newFailoverFixture(t, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "", withExtraFallback("ghost"))
	if rec := f.request(t); rec.Code == http.StatusOK {
		t.Fatal("expected the chain to run out and surface the failure")
	}
	analysis := traceOfRequest(t, f)
	var skipped *monitor.RequestAttempt
	for i := range analysis.Attempts {
		if analysis.Attempts[i].ProviderID == "ghost" {
			skipped = &analysis.Attempts[i]
		}
	}
	if skipped == nil {
		t.Fatalf("the unresolvable candidate should be recorded, got %#v", analysis.Attempts)
	}
	if !skipped.Skipped || skipped.Reason != attemptReasonUndecidable {
		t.Fatalf("it should be marked skipped with its reason, got %#v", *skipped)
	}
	if len(analysis.Attempts) != 3 {
		t.Fatalf("expected two failures plus the skipped candidate, got %#v", analysis.Attempts)
	}
}

// A request with no chain still gets one attempt recorded, so the panel is not
// blank for the common single-provider key.
func TestScheduleTraceRecordsSingleProviderRequests(t *testing.T) {
	f := newFailoverFixture(t, 0, 0, "", withoutFallbacks())
	if rec := f.request(t); rec.Code != http.StatusOK {
		t.Fatalf("request failed: %d", rec.Code)
	}
	analysis := traceOfRequest(t, f)
	if len(analysis.Attempts) != 1 || analysis.Attempts[0].Reason != attemptReasonServed {
		t.Fatalf("expected exactly one served attempt, got %#v", analysis.Attempts)
	}
}

// A timeout is a different story from a refusal, and the trace must say which:
// an operator reading the log needs to know the account was given up on, not
// that it answered with an error.
func TestScheduleTraceRecordsFirstPacketTimeout(t *testing.T) {
	f := newTimeoutFixture(t, 1, 5*time.Second, false)
	if rec := f.do(t, false); rec.Code != http.StatusOK {
		t.Fatalf("expected the backup to serve, got %d: %s", rec.Code, rec.Body.String())
	}
	var analysis monitor.RequestAnalysis
	for _, entry := range f.server.logs.List(10) {
		if entry.APIKeyID == "k-to" {
			analysis = entry.Analysis
			break
		}
	}
	if len(analysis.Attempts) != 2 {
		t.Fatalf("expected the timed-out attempt and the serving one, got %#v", analysis.Attempts)
	}
	if analysis.Attempts[0].Reason != attemptReasonFirstPacketTimeout || !analysis.Attempts[0].Deferred {
		t.Fatalf("the first attempt should be the timed-out one, got %#v", analysis.Attempts[0])
	}
	if analysis.Attempts[1].Reason != attemptReasonServed {
		t.Fatalf("the backup should be recorded as the one that served, got %#v", analysis.Attempts[1])
	}
}
