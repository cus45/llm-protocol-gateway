package gateway

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// TestFailoverHonoursUpstreamRetryAfter drives a real failover attempt against an
// upstream that answers 429 with Retry-After: the header must reach the cooldown,
// not just the body. Before this, every 429 rested the soft default minute.
func TestFailoverHonoursUpstreamRetryAfter(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"Number of requests has exceeded your rate limit"}}`))
	}))
	defer upstream.Close()

	state := domain.GatewayState{
		Providers: []domain.Provider{{
			ID: "cooldown-probe", Name: "cooldown-probe", Protocol: domain.ProtocolOpenAIChat,
			BaseURL: upstream.URL, AuthType: "api_key", APIKeySource: "static",
		}, {
			ID: "cooldown-backup", Name: "cooldown-backup", Protocol: domain.ProtocolClaude,
			BaseURL: upstream.URL, AuthType: "api_key", APIKeySource: "static",
		}},
		Routes: []domain.Route{
			{
				ID: "cooldown-probe-route", Name: "cooldown-probe-route", ProviderID: "cooldown-probe",
				OutputProtocol: domain.ProtocolClaude, Enabled: true,
			},
			// A chain of two so the attempt can be deferred: without a backup the
			// gateway has nowhere to fail over and commits the error at once.
			{
				ID: "cooldown-backup-route", Name: "cooldown-backup-route", ProviderID: "cooldown-backup",
				OutputProtocol: domain.ProtocolClaude, Enabled: true,
			},
		},
	}
	server := NewServer(NewRouter(state), monitor.NewStore())
	route, err := server.router.RouteByID("cooldown-probe-route")
	if err != nil {
		t.Fatalf("route lookup: %v", err)
	}
	key := domain.APIKey{ID: "cooldown-key", RouteID: route.ID, FallbackProviderIDs: []string{"cooldown-backup"}}

	for i := 0; i < providerSoftFailureThreshold; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		status, _, _, _, _, execErr := server.executeProtocolFlowWithFailover(
			rec, req, route, domain.RouteDecision{RouteID: route.ID, ProviderID: route.ProviderID},
			"m", map[string]any{"model": "m", "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
			domain.ProtocolClaude, true, key, true,
		)
		if status != http.StatusTooManyRequests {
			t.Fatalf("attempt %d status = %d err=%v, want 429", i+1, status, execErr)
		}
	}
	if atomic.LoadInt32(&hits) != providerSoftFailureThreshold {
		t.Fatalf("upstream hits = %d, want %d", hits, providerSoftFailureThreshold)
	}
	st, ok := server.providerUnavailableSnapshot("cooldown-probe")
	if !ok {
		t.Fatal("expected provider marked unavailable after repeated 429")
	}
	if left := time.Until(st.nextRetry); left < 4*time.Minute || left > 5*time.Minute {
		t.Fatalf("cooldown = %v left, want ~5m from the upstream Retry-After", left)
	}
}
