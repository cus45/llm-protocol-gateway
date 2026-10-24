package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// traeProviderWithExpiry builds a Trae provider whose token expires at the
// given time, pointing at an auth host that refuses the exchange (a closed
// local port), so the renewal always fails.
func traeProviderWithExpiry(expiresAt time.Time) domain.Provider {
	provider := domain.Provider{
		ID:       "trae-expiry",
		Name:     "trae-expiry",
		Protocol: domain.ProtocolOpenAIChat,
		AuthType: domain.AuthTypeTraeCN,
		Trae: &domain.TraeCredential{
			AccessToken:  "stale-access",
			RefreshToken: "refresh-" + fmt.Sprint(expiresAt.UnixNano()),
			ExpiresAt:    expiresAt.UTC().Format(time.RFC3339),
			// An unroutable auth host: the exchange fails as a network error, not
			// as "this refresh token is invalid" — that is the case where a stale
			// token used to be sent upstream as if nothing had happened.
			APIHost:  "https://127.0.0.1:1",
			AuthHost: "https://127.0.0.1:1",
		},
	}
	return provider
}

// 续期失败但手上的令牌还没到期：照常服务，别把一次网络抖动升级成故障。
func TestTraeRenewalFailureKeepsServingWhileTokenIsStillValid(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	provider := traeProviderWithExpiry(time.Now().Add(30 * time.Minute))

	refreshed, err := server.ensureFreshTraeToken(provider)
	if err != nil {
		t.Fatalf("a renewal failure before expiry must not fail the request: %v", err)
	}
	if strings.TrimSpace(refreshed.Trae.AccessToken) != "stale-access" {
		t.Fatalf("the stored token should be carried through, got %q", refreshed.Trae.AccessToken)
	}
}

// 令牌已经过期、续期又失败：必须明确报错。返回 nil 会让请求带着一枚死令牌
// 打到上游，客户端看到的是 401 "invalid api key"，像是密钥配错了。
//
// 失败按"不是登录失效"的那一类注入（网络错误），否则走不到这个分支：供应商
// 拒绝 refresh token 时，上面已经按"需要重新登录"报错了。
func TestTraeRenewalFailureOnExpiredTokenIsReported(t *testing.T) {
	previous := traeRenewOverride
	traeRenewOverride = func(context.Context, traeSite, domain.TraeCredential, string) (domain.TraeCredential, error) {
		return domain.TraeCredential{}, errors.New("dial tcp: connection refused")
	}
	t.Cleanup(func() { traeRenewOverride = previous })

	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	provider := traeProviderWithExpiry(time.Now().Add(-5 * time.Minute))

	_, err := server.ensureFreshTraeToken(provider)
	if err == nil {
		t.Fatalf("an expired token with a failed renewal must fail the request")
	}
	if !strings.Contains(err.Error(), "过期") {
		t.Fatalf("the error should say the token expired, got %v", err)
	}
}

// 同一类续期失败，但令牌还没过期：照常服务。
func TestTraeNetworkRenewalFailureKeepsValidToken(t *testing.T) {
	previous := traeRenewOverride
	traeRenewOverride = func(context.Context, traeSite, domain.TraeCredential, string) (domain.TraeCredential, error) {
		return domain.TraeCredential{}, errors.New("dial tcp: connection refused")
	}
	t.Cleanup(func() { traeRenewOverride = previous })

	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	provider := traeProviderWithExpiry(time.Now().Add(30 * time.Minute))
	if _, err := server.ensureFreshTraeToken(provider); err != nil {
		t.Fatalf("a transient renewal failure must not fail a still-valid token: %v", err)
	}
}

func TestTraeCredentialExpired(t *testing.T) {
	past := &domain.TraeCredential{ExpiresAt: time.Now().Add(-time.Minute).Format(time.RFC3339)}
	future := &domain.TraeCredential{ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339)}
	// Inside the renew lead but not yet expired: still servable if the renewal fails.
	lead := &domain.TraeCredential{ExpiresAt: time.Now().Add(traeRenewLead / 2).Format(time.RFC3339)}
	unparsable := &domain.TraeCredential{ExpiresAt: "not a time"}

	if !traeCredentialExpired(past) {
		t.Fatalf("a past expiry must count as expired")
	}
	if traeCredentialExpired(future) || traeCredentialExpired(lead) {
		t.Fatalf("a token that has not expired must not count as expired")
	}
	if traeCredentialExpired(unparsable) {
		t.Fatalf("an unparsable expiry is not evidence of expiry")
	}
	if !traeCredentialExpired(nil) {
		t.Fatalf("no credential at all counts as expired")
	}
}
