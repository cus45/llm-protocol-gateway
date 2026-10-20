package gateway

import (
	"sync/atomic"
	"testing"
)

// Claude's callback goes to the gateway's own port on localhost, which a browser
// on another machine can never reach. The pasted address is the supported way
// out, so it must be accepted in the shape the address bar actually offers.
func TestParseClaudeOAuthInputTakesTheCallbackURL(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantCode  string
		wantState string
	}{
		{"whole callback URL", "http://localhost:18093/callback?code=abc%2F123&state=st-9", "abc/123", "st-9"},
		{"URL without a state", "http://localhost:18093/callback?code=abc", "abc", ""},
		{"code#state form", "abc#st-9", "abc", "st-9"},
		{"bare code", "abc", "abc", ""},
		{"empty", "   ", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, state := parseClaudeOAuthInput(tc.raw)
			if code != tc.wantCode || state != tc.wantState {
				t.Fatalf("parseClaudeOAuthInput(%q) = (%q, %q), want (%q, %q)", tc.raw, code, state, tc.wantCode, tc.wantState)
			}
		})
	}
}

// The code is one-time: whichever door arrives first spends it, and the other
// must be told so rather than sending a second exchange that Anthropic rejects
// (which reads to the user as a failed login).
func TestClaimClaudeCallbackIsSingleUse(t *testing.T) {
	pending := claudeOAuthPending{claim: &atomic.Bool{}}
	if !claimClaudeCallback(pending) {
		t.Fatal("the first arrival must be allowed to exchange the code")
	}
	if claimClaudeCallback(pending) {
		t.Fatal("the second arrival must not spend the same code again")
	}
	if !claimClaudeCallback(claudeOAuthPending{}) {
		t.Fatal("a flow without a claim must not be locked out")
	}
}

// The callback hint belongs to the flow, not to one status transition: the
// console reads it while polling, after the flow has already moved on.
func TestClaudeOAuthStatusKeepsCallbackHint(t *testing.T) {
	store := newClaudeOAuthPendingStore()
	store.setStatus("f1", "pending", "waiting for browser callback")
	store.setCallbackHint("f1", claudeOAuthFlowStatus{
		PasteCallback: true, CallbackURL: "http://localhost:18093/callback", Instructions: "把地址粘回来",
	})
	store.setStatus("f1", "pending", "still waiting")
	status, ok := store.status("f1")
	if !ok {
		t.Fatal("status missing")
	}
	if !status.PasteCallback || status.CallbackURL == "" || status.Instructions == "" {
		t.Fatalf("callback hint lost: %+v", status)
	}
	if status.Status != "pending" || status.Message != "still waiting" {
		t.Fatalf("status transition lost: %+v", status)
	}
}
