package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// The OpenCode Zen headers must describe the account this attempt actually
// sends with: in a multi-account chain the send provider is swapped per
// attempt, and a header built from the outer provider would name another
// account (round26 D26-12).
func TestOpenCodeZenHeadersFollowTheSendProvider(t *testing.T) {
	var mu sync.Mutex
	var seen []http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer upstream.Close()

	body := []byte(`{"model":"kimi-k2.5-free","messages":[{"role":"user","content":"hi"}]}`)
	zen := domain.Provider{ID: "zen-1", Name: "zen", AuthType: domain.AuthTypeOpenCodeZenFree, BaseURL: upstream.URL, APIKeySource: "literal:zen-key"}
	relay := domain.Provider{ID: "relay-1", Name: "relaxed", AuthType: domain.AuthTypeAPIKey, BaseURL: upstream.URL, APIKeySource: "literal:relay-key"}
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())

	for _, provider := range []domain.Provider{zen, relay} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
		if _, _, _, err := server.proxyOpenAIChat(recorder, request, provider, chatBodyModel(body), body, true); err != nil {
			t.Fatalf("%s: proxyOpenAIChat: %v", provider.ID, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(seen))
	}
	// The two attempts name different accounts, so whatever the Zen path adds
	// must differ between them; an identical header set would mean it was
	// built from the same provider both times.
	same := true
	for key := range seen[0] {
		if seen[0].Get(key) != seen[1].Get(key) {
			same = false
		}
	}
	if same {
		t.Fatalf("both accounts sent identical headers: %v", seen[0])
	}
}
