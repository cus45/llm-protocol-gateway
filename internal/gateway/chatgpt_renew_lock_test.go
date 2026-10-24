package gateway

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// D31-5: once the renew lock is taken the refresh must spend the refresh_token
// the store holds right now. Spending the caller's stale snapshot gets
// refresh_token_reused and kills the rotating chain.
func TestEnsureFreshChatGPTTokenSpendsLiveRefreshToken(t *testing.T) {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"a@example.com","https://api.openai.com/auth":{"chatgpt_plan_type":"plus","chatgpt_account_id":"acct-1"}}`))
	jwt := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + claims + "."
	var spent []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		spent = append(spent, r.PostForm.Get("refresh_token"))
		if r.PostForm.Get("refresh_token") == "rt-old" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"refresh_token_reused"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": jwt, "refresh_token": "rt-new-2", "expires_in": 3600, "id_token": jwt,
		})
	}))
	defer upstream.Close()
	previousURL := chatgptOAuthTokenURLVar
	chatgptOAuthTokenURLVar = func() string { return upstream.URL }
	t.Cleanup(func() { chatgptOAuthTokenURLVar = previousURL })

	router := NewRouter(domain.GatewayState{Providers: []domain.Provider{{
		ID:       "codex-1",
		AuthType: domain.AuthTypeChatGPTOAuth,
		ChatGPTOAuth: &domain.ChatGPTOAuthCredential{
			AccessToken: "at", RefreshToken: "rt-new-1", ExpiresAt: "2000-01-01T00:00:00Z",
		},
	}}})
	server := &Server{router: router}
	stale := domain.Provider{
		ID:       "codex-1",
		AuthType: domain.AuthTypeChatGPTOAuth,
		ChatGPTOAuth: &domain.ChatGPTOAuthCredential{
			AccessToken: "at", RefreshToken: "rt-old", ExpiresAt: "2000-01-01T00:00:00Z",
		},
	}
	// The mutex that serializes renewals is registered lazily; taking it once
	// proves the refresh below runs on its slow path.
	server.lockOAuthUsageFetch("chatgpt-token:codex-1")()

	updated, err := server.ensureFreshChatGPTToken(stale)
	if err != nil {
		t.Fatalf("renew failed: %v (spent %v)", err, spent)
	}
	if len(spent) != 1 || spent[0] != "rt-new-1" {
		t.Fatalf("refresh spent %v, want the stored rt-new-1", spent)
	}
	if updated.ChatGPTOAuth.RefreshToken != "rt-new-2" || strings.TrimSpace(updated.ChatGPTOAuth.AccountLabel) == "" {
		t.Fatalf("updated credential = %+v", updated.ChatGPTOAuth)
	}
}
