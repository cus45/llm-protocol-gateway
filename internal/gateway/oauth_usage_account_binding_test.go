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

// 账号 A 失败一次后切到 B：A 的冷却不得冻住 B 的刷新。线上的后果是新账号的
// 报告最多 10 分钟不更新，而旧值已经被删掉——控制台与调度在这段时间里"无数据"。
func TestOauthUsageFailureDoesNotFreezeTheNextAccount(t *testing.T) {
	cache := newOAuthUsageCache()
	cache.setForAccount("claude:p", "account-a", ClaudeOAuthUsageReport{Available: true})
	cache.setFailureForAccount("claude:p", "account-a", errUsageProbeFailed)

	if cache.needsRefresh("claude:p") {
		t.Fatalf("the first account's failure must hold its entry in cooldown")
	}
	// 切到账号 B：B 从没失败过，必须立刻可刷新。
	cache.bindAccount("claude:p", "account-b")
	if !cache.needsRefresh("claude:p") {
		t.Fatalf("the next account inherited the previous account's cooldown")
	}
	// 直接以 B 的身份上报失败：仍然不能被 A 的冷却吞掉。
	cache.setFailureForAccount("claude:p", "account-a", errUsageProbeFailed)
	cache.setFailureForAccount("claude:p", "account-b", errUsageProbeFailed)
	cache.bindAccount("claude:p", "account-c")
	if !cache.needsRefresh("claude:p") {
		t.Fatalf("a fresh account must never inherit another account's failure")
	}
}
