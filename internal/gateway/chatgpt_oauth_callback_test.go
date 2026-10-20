package gateway

import (
	"errors"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
)

// ChatGPT only ever sends the browser back to localhost:1455, so a gateway on
// another machine can never receive the callback. The paste-in finish is the
// supported way out, which means the flow must stay alive when the port cannot
// be taken — and the code must be spent exactly once, whichever door arrives
// first (a second exchange is rejected as invalid_grant and reads as a failed
// login).
func TestChatGPTCallbackClaimIsSingleUse(t *testing.T) {
	pending := chatgptOAuthPending{claim: &atomic.Bool{}}
	if !claimChatGPTCallback(pending) {
		t.Fatal("the first arrival must be allowed to exchange the code")
	}
	if claimChatGPTCallback(pending) {
		t.Fatal("the second arrival must not spend the same code again")
	}
	// A flow made before the claim existed still has to work rather than stall.
	if !claimChatGPTCallback(chatgptOAuthPending{}) {
		t.Fatal("a pending flow without a claim must not be locked out")
	}
}

func TestChatGPTCallbackBlockedReasonNamesBothCases(t *testing.T) {
	if got := chatgptCallbackBlockedReason(errors.New("bind: address already in use")); got != "busy" {
		t.Fatalf("a port held by another program should read busy, got %q", got)
	}
	if runtime.GOOS == "windows" {
		// Windows reserves whole port ranges; 10013 is not fixable by retrying.
		if got := chatgptCallbackBlockedReason(syscall.Errno(10013)); got != "reserved" {
			t.Fatalf("a reserved port range should read reserved, got %q", got)
		}
	}
}

// The callback hint belongs to the flow: every later status transition must
// carry it, or the console loses the paste box exactly when it needs it.
func TestChatGPTOAuthStatusKeepsCallbackHint(t *testing.T) {
	store := newChatGPTOAuthPendingStore()
	store.setStatus("f1", "pending", "等待浏览器授权…")
	store.setCallbackHint("f1", chatgptOAuthFlowStatus{
		PasteCallback: true, CallbackPort: 1455, CallbackBlocked: "busy", Instructions: "把地址粘回来",
	})
	store.setStatus("f1", "pending", "仍在等待")
	status, ok := store.status("f1")
	if !ok {
		t.Fatal("status missing")
	}
	if !status.PasteCallback || status.CallbackPort != 1455 || status.CallbackBlocked != "busy" || status.Instructions == "" {
		t.Fatalf("callback hint lost: %+v", status)
	}
	if status.Status != "pending" || status.Message != "仍在等待" {
		t.Fatalf("status transition lost: %+v", status)
	}
}

// The pasted address is what a remote gateway's user has to work with, so the
// parser must take the whole URL, not only a bare code.
func TestParseChatGPTOAuthInputTakesTheCallbackURL(t *testing.T) {
	code, state := parseChatGPTOAuthInput("http://localhost:1455/auth/callback?code=abc%2F123&state=st-9", "")
	if code != "abc/123" {
		t.Fatalf("code = %q", code)
	}
	if state != "st-9" {
		t.Fatalf("state = %q, want the one carried by the URL", state)
	}
	code, state = parseChatGPTOAuthInput("bare-code", "fallback-state")
	if code != "bare-code" || state != "fallback-state" {
		t.Fatalf("bare code path broken: %q %q", code, state)
	}
}
