package gateway

import (
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// D31-4: a usage report cached for one account must never be served once the
// provider is re-pointed at another account, and the cached snapshot that the
// scheduler reads must follow the same rule.
func TestOAuthUsageCacheDropsReportAfterAccountSwap(t *testing.T) {
	server := imagesTestServer(t)
	provider := domain.Provider{
		ID:       "claude-1",
		Name:     "claude-1",
		Protocol: domain.ProtocolClaude,
		AuthType: domain.AuthTypeClaudeOAuth,
		ClaudeOAuth: &domain.ClaudeOAuthCredential{
			AccessToken: "access-a", RefreshToken: "refresh-a",
		},
	}
	if _, err := server.router.AddProvider(provider); err != nil {
		t.Fatalf("add provider: %v", err)
	}
	key := server.oauthUsageCacheKeyFor("claude", provider.ID)
	server.oauthUsageCache.setForAccount(key, oauthUsageAccountID(provider), ClaudeOAuthUsageReport{
		Available: true,
		FiveHour:  &ClaudeOAuthUsageBucket{Utilization: 100},
	})
	if _, ok := server.cachedClaudeFiveHourUtilization(provider.ID); !ok {
		t.Fatal("report for the current account must be visible")
	}

	// Swap the account behind the provider (what a re-login does).
	if _, err := server.router.SetProviderClaudeOAuth(provider.ID, domain.ClaudeOAuthCredential{AccessToken: "access-b", RefreshToken: "refresh-b"}); err != nil {
		t.Fatalf("swap account: %v", err)
	}

	if _, ok := server.oauthUsageCache.getAllowStale(server.oauthUsageCacheKeyFor("claude", provider.ID)); ok {
		t.Fatal("previous account's report still served after the account was swapped")
	}
	if util, ok := server.cachedClaudeFiveHourUtilization(provider.ID); ok {
		t.Fatalf("scheduler still reads the previous account's usage (%v)", util)
	}
}

// Token rotation of the same account (refresh token unchanged) must keep the
// report: the background refresh rotates access tokens every few minutes.
func TestOAuthUsageCacheKeepsReportAcrossTokenRotation(t *testing.T) {
	server := imagesTestServer(t)
	provider := domain.Provider{
		ID:           "chatgpt-1",
		AuthType:     domain.AuthTypeChatGPTOAuth,
		ChatGPTOAuth: &domain.ChatGPTOAuthCredential{AccessToken: "access-a", RefreshToken: "refresh-a"},
	}
	key := server.oauthUsageCacheKeyFor("chatgpt", provider.ID)
	server.oauthUsageCache.setForAccount(key, oauthUsageAccountID(provider), ChatGPTOAuthUsageReport{Available: true})
	rotated := provider
	// Same account: a token rotation reissues the access token only.
	rotated.ChatGPTOAuth = &domain.ChatGPTOAuthCredential{AccessToken: "access-b", RefreshToken: "refresh-a"}
	server.oauthUsageCache.bindAccount(key, oauthUsageAccountID(rotated))
	if _, ok := server.oauthUsageCache.getAllowStale(key); !ok {
		t.Fatal("same-account report must survive token rotation")
	}
}

// The identity must be a fingerprint, never the credential itself.
func TestOAuthUsageAccountIDIsNotTheToken(t *testing.T) {
	provider := domain.Provider{
		AuthType:     domain.AuthTypeChatGPTOAuth,
		ChatGPTOAuth: &domain.ChatGPTOAuthCredential{AccessToken: "access-secret", RefreshToken: "refresh-secret"},
	}
	id := oauthUsageAccountID(provider)
	if id == "" || strings.Contains(id, "secret") {
		t.Fatalf("account id %q leaks or misses the credential", id)
	}
	other := provider
	other.ChatGPTOAuth = &domain.ChatGPTOAuthCredential{AccessToken: "x", RefreshToken: "refresh-other"}
	if oauthUsageAccountID(other) == id {
		t.Fatal("different accounts must get different ids")
	}
}
