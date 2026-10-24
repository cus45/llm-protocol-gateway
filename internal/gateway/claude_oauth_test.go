package gateway

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// The sweep rotates tokens earlier than the request path does: a quiet account
// must not leave its refresh token idle long enough for Anthropic to retire it.
func TestClaudeTokenRefreshLead(t *testing.T) {
	at := func(d time.Duration) *domain.ClaudeOAuthCredential {
		return &domain.ClaudeOAuthCredential{AccessToken: "at", ExpiresAt: time.Now().Add(d).UTC().Format(time.RFC3339)}
	}
	if !claudeTokenNeedsRefreshWithin(at(20*time.Minute), claudeTokenProactiveRefreshLead) {
		t.Fatal("a token 20 minutes out is due under the sweep's lead")
	}
	if claudeTokenNeedsRefresh(at(20 * time.Minute)) {
		t.Fatal("the request path must leave a token 20 minutes out alone")
	}
	if claudeTokenNeedsRefreshWithin(at(2*time.Hour), claudeTokenProactiveRefreshLead) {
		t.Fatal("a token two hours out is not due")
	}
}

func TestBuildClaudeLocalAuthorizeURLUsesCAIEndpoint(t *testing.T) {
	authURL := buildClaudeLocalAuthorizeURL("http://localhost:18093/callback", "challenge-value", "state-value")
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	if parsed.Scheme+"://"+parsed.Host+parsed.Path != claudeOAuthCAIAuthorizeURL {
		t.Fatalf("authorize url = %q, want base %q", parsed.Scheme+"://"+parsed.Host+parsed.Path, claudeOAuthCAIAuthorizeURL)
	}
	query := parsed.Query()
	if query.Get("code") != "true" {
		t.Fatalf("local flow should include code=true, got %q", query.Get("code"))
	}
	if query.Get("redirect_uri") != "http://localhost:18093/callback" {
		t.Fatalf("redirect_uri = %q", query.Get("redirect_uri"))
	}
	if query.Get("code_challenge") != "challenge-value" {
		t.Fatalf("missing code_challenge")
	}
	if query.Get("state") != "state-value" {
		t.Fatalf("missing state")
	}
}

func TestClaudeOAuthCallbackURLUsesLocalhostCallback(t *testing.T) {
	server := &Server{}
	server.SetListenAddr("0.0.0.0:18093")
	got := server.claudeOAuthCallbackURL()
	want := "http://localhost:18093/callback"
	if got != want {
		t.Fatalf("callback url = %q, want %q", got, want)
	}
}

func TestBuildClaudeAuthorizeURLMatchesSub2API(t *testing.T) {
	authURL := buildClaudeAuthorizeURL("challenge-value", "state-value")
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	if parsed.Scheme+"://"+parsed.Host+parsed.Path != claudeOAuthAuthorizeURL {
		t.Fatalf("authorize url = %q, want base %q", parsed.Scheme+"://"+parsed.Host+parsed.Path, claudeOAuthAuthorizeURL)
	}
	query := parsed.RawQuery
	if !strings.HasPrefix(query, "code=true&client_id=") {
		t.Fatalf("query should start with code=true&client_id=, got %q", query)
	}
	if !strings.Contains(query, "code_challenge=challenge-value") {
		t.Fatalf("query missing code_challenge: %q", query)
	}
	if !strings.Contains(query, "code_challenge_method=S256") {
		t.Fatalf("query missing code_challenge_method: %q", query)
	}
	if !strings.Contains(query, "state=state-value") {
		t.Fatalf("query missing state: %q", query)
	}
	scope := parsed.Query().Get("scope")
	for _, required := range []string{
		"org:create_api_key",
		"user:profile",
		"user:inference",
		"user:sessions:claude_code",
		"user:mcp_servers",
		"user:file_upload",
	} {
		if !strings.Contains(scope, required) {
			t.Fatalf("scope %q missing %q", scope, required)
		}
	}
}
