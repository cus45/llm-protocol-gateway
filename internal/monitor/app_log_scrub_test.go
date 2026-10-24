package monitor

import (
	"strings"
	"testing"
)

// 应用日志里上游错误原文、OAuth 续期失败都会带上凭据：写入前必须抹掉。
func TestAddAppScrubsCredentials(t *testing.T) {
	store := NewStore()
	store.SetLevel("info")

	store.AddApp("error", "refresh failed: Incorrect API key provided: sk-live-abcdefghijklmnop1234", "Authorization: Bearer sk-ant-oat01-abcdefghijklmnopqrstuv")
	store.AddApp("warn", "jwt expired eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "tool output ghp_1234567890abcdefghijklmnop")

	entries := store.ListApp(0)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	for _, entry := range entries {
		for _, field := range []string{entry.Message, entry.Context} {
			for _, secret := range []string{"sk-live-", "sk-ant-oat01-", "ghp_1234567890", "eyJhbGciOiJIUzI1NiJ9."} {
				if strings.Contains(field, secret) {
					t.Fatalf("app log still carries a credential (%s): %q", secret, field)
				}
			}
		}
	}
	// The readable part of the message must survive: scrubbing is not deletion.
	if !strings.Contains(entries[1].Message, "refresh failed") {
		t.Fatalf("the message lost its meaning: %q", entries[1].Message)
	}
	if !strings.Contains(entries[1].Message, appLogScrubbed) {
		t.Fatalf("the credential should be replaced, not removed silently: %q", entries[1].Message)
	}
}

func TestScrubAppLogTextLeavesOrdinaryTextAlone(t *testing.T) {
	text := "provider marked unavailable reason=usage_limit retry_in=2m0s"
	if got := scrubAppLogText(text); got != text {
		t.Fatalf("ordinary text was rewritten: %q", got)
	}
	if got := scrubAppLogText(""); got != "" {
		t.Fatalf("empty text changed: %q", got)
	}
}
