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

func TestEarliestChatGPTResetCreditPicksSoonestExpiry(t *testing.T) {
	credits := []ChatGPTResetCredit{
		{ID: "never", ExpiresAt: ""},
		{ID: "late", ExpiresAt: "2026-11-01T00:00:00Z"},
		{ID: "soon", ExpiresAt: "2026-10-04T01:02:06Z"},
	}
	credit, ok := earliestChatGPTResetCredit(credits)
	if !ok || credit.ID != "soon" {
		t.Fatalf("got %#v ok=%v", credit, ok)
	}
	if _, ok := earliestChatGPTResetCredit(nil); ok {
		t.Fatal("no credits must report not found")
	}
}

func TestConsumeChatGPTOAuthResetCredit(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotAccount string
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("ChatGPT-Account-ID")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"code":"reset","windows_reset":2}`))
	}))
	defer server.Close()

	provider := domain.Provider{
		AuthType: domain.AuthTypeChatGPTOAuth,
		ChatGPTOAuth: &domain.ChatGPTOAuthCredential{
			AccessToken:      "token",
			ChatGPTAccountID: "acct_1",
		},
	}
	out, err := consumeChatGPTOAuthResetCreditAt(context.Background(), provider, server.URL+"/consume", "cr-1", "redeem-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/consume" {
		t.Fatalf("method=%q path=%q", gotMethod, gotPath)
	}
	if gotAuth != "Bearer token" || gotAccount != "acct_1" {
		t.Fatalf("auth=%q account=%q", gotAuth, gotAccount)
	}
	if gotBody["credit_id"] != "cr-1" || gotBody["redeem_request_id"] != "redeem-1" {
		t.Fatalf("body=%#v", gotBody)
	}
	if out.Code != "reset" || out.WindowsReset != 2 {
		t.Fatalf("out=%#v", out)
	}
}

func TestConsumeChatGPTOAuthResetCreditErrors(t *testing.T) {
	provider := domain.Provider{
		ChatGPTOAuth: &domain.ChatGPTOAuthCredential{AccessToken: "token"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"detail":"slow down"}`))
	}))
	defer server.Close()
	if _, err := consumeChatGPTOAuthResetCreditAt(context.Background(), provider, server.URL, "cr-1", "redeem-1"); err == nil {
		t.Fatal("non-2xx must be an error")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer empty.Close()
	if _, err := consumeChatGPTOAuthResetCreditAt(context.Background(), provider, empty.URL, "cr-1", "redeem-1"); err == nil {
		t.Fatal("a response without a code must be an error")
	}
}

func TestNewChatGPTResetRedeemIDIsUUIDv4(t *testing.T) {
	id, err := newChatGPTResetRedeemID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 36 || id[14] != '4' || (id[19] != '8' && id[19] != '9' && id[19] != 'a' && id[19] != 'b') {
		t.Fatalf("not a v4 uuid: %q", id)
	}
}
