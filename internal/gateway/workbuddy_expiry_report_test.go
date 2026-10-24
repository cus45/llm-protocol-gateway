package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func workBuddyProviderExpiringAt(expiresAt time.Time, refreshToken string) domain.Provider {
	return domain.Provider{
		ID:       "wb-expiry",
		Name:     "wb-expiry",
		Protocol: domain.ProtocolOpenAIChat,
		AuthType: domain.AuthTypeWorkBuddy,
		WorkBuddy: &domain.WorkBuddyCredential{
			AccessToken:  "stale-access",
			RefreshToken: refreshToken,
			ExpiresAt:    expiresAt.UTC().Format(time.RFC3339),
		},
	}
}

// 令牌已过期、又没有 refresh token 时只剩一枚死令牌：明确报错，而不是把上游
// 的 401 "invalid token" 原样转给客户端（那看起来像客户端自己配错了）。
func TestWorkBuddyExpiredWithoutRefreshTokenIsReported(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	provider := workBuddyProviderExpiringAt(time.Now().Add(-time.Minute), "")

	_, err := server.ensureFreshWorkBuddyToken(provider)
	if err == nil {
		t.Fatalf("an expired token with nothing to refresh with must fail")
	}
	if !strings.Contains(err.Error(), "过期") {
		t.Fatalf("the error should say the token expired, got %v", err)
	}
}

// 未过期、没有 refresh token：照常放行（上游可能仍然接受）。
func TestWorkBuddyValidWithoutRefreshTokenStillServes(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	provider := workBuddyProviderExpiringAt(time.Now().Add(time.Hour), "")
	if _, err := server.ensureFreshWorkBuddyToken(provider); err != nil {
		t.Fatalf("a still-valid token must not be refused: %v", err)
	}
}

func TestWorkBuddyCredentialExpired(t *testing.T) {
	past := &domain.WorkBuddyCredential{ExpiresAt: time.Now().Add(-time.Minute).Format(time.RFC3339)}
	future := &domain.WorkBuddyCredential{ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)}
	if !workBuddyCredentialExpired(past) {
		t.Fatalf("a past expiry must count as expired")
	}
	if workBuddyCredentialExpired(future) {
		t.Fatalf("a future expiry must not count as expired")
	}
	if workBuddyCredentialExpired(&domain.WorkBuddyCredential{ExpiresAt: "garbage"}) {
		t.Fatalf("an unparsable expiry is not evidence of expiry")
	}
	if !workBuddyCredentialExpired(nil) {
		t.Fatalf("no credential counts as expired")
	}
}
