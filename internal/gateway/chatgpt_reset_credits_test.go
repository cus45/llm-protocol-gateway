package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// The reset-credit endpoint shape mirrors sub2api's openai_quota_reset_credits
// parsing: credits[] with id / status / expires_at, plus available_count.
func TestFetchChatGPTOAuthResetCredits(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{
			"credits": [
				{"id":"rc_1","reset_type":"codex_rate_limits","status":"available","title":"Full reset","granted_at":"2026-09-04T01:02:06Z","expires_at":"2026-10-04T01:02:06Z"},
				{"id":"rc_2","reset_type":"codex_rate_limits","status":"redeemed","expires_at":"2026-10-04T22:38:55Z"},
				{"id":"rc_3","reset_type":"other_kind","status":"available","expires_at":"2026-10-04T22:38:55Z"}
			],
			"available_count": 2
		}`))
	}))
	defer server.Close()

	provider := domain.Provider{
		AuthType: domain.AuthTypeChatGPTOAuth,
		ChatGPTOAuth: &domain.ChatGPTOAuthCredential{
			AccessToken: "token",
		},
	}
	credits := fetchChatGPTOAuthResetCreditsAt(context.Background(), provider, server.URL)
	if gotPath != "/" {
		t.Fatalf("path = %q", gotPath)
	}
	if credits == nil {
		t.Fatal("credits must parse")
	}
	if credits.AvailableCount != 2 {
		t.Fatalf("availableCount = %d, want 2", credits.AvailableCount)
	}
	if len(credits.Credits) != 1 {
		t.Fatalf("only the available codex card should be kept, got %#v", credits.Credits)
	}
	if credits.Credits[0].ID != "rc_1" || credits.Credits[0].ExpiresAt != "2026-10-04T01:02:06Z" {
		t.Fatalf("unexpected card: %#v", credits.Credits[0])
	}
}

func TestChatGPTResetCreditsJSONShape(t *testing.T) {
	var report ChatGPTOAuthUsageReport
	body := `{"available":true,"resetCredits":{"availableCount":1,"credits":[{"id":"rc_1","expiresAt":"2026-10-04T01:02:06Z"}]}}`
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatal(err)
	}
	if report.ResetCredits == nil || report.ResetCredits.AvailableCount != 1 || len(report.ResetCredits.Credits) != 1 {
		t.Fatalf("unexpected report: %#v", report)
	}
}
