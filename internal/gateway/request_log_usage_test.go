package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func recordUsageCase(t *testing.T, status int, usage TokenUsage, requestBody, responseBody string) monitor.RequestLog {
	t.Helper()
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	putPriceOn(t, server, domain.ModelPrice{
		ProviderID: "p1", ModelID: "*",
		InputUSDPerMTok: 5, OutputUSDPerMTok: 25,
	})
	server.recordRequestLog(
		time.Now(), domain.APIKey{ID: "k1", Name: "main"}, true,
		"r1", "p1", "model-x", "chat", "openai_chat->openai_chat", "/v1/chat/completions",
		status, usage, []byte(requestBody), []byte(responseBody),
	)
	logs := server.logs.List(1)
	if len(logs) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(logs))
	}
	return logs[0]
}

// An error reply carries no usage, so the body-length fallback used to invent
// one: a 429 to a 200 KB prompt was logged as ~280k input tokens and priced at
// $1.12. The vendor billed nothing, so neither tokens nor cost may be recorded.
func TestFailedRequestDoesNotEstimateUsage(t *testing.T) {
	requestBody := `{"messages":[{"role":"user","content":"` + strings.Repeat("x", 40000) + `"}]}`
	entry := recordUsageCase(t, 429, TokenUsage{}, requestBody, `{"error":{"message":"rate limited"}}`)
	if entry.InputTokens != 0 || entry.OutputTokens != 0 {
		t.Fatalf("failure borrowed an estimated token count: in=%d out=%d", entry.InputTokens, entry.OutputTokens)
	}
	if entry.CostMicros != 0 {
		t.Fatalf("failure was charged from an estimate: %d micros", entry.CostMicros)
	}
}

// A failed attempt that did reach the model (a stream the client aborted
// after the prompt was read) keeps the vendor-reported usage and is priced on
// it: only the made-up estimate is wrong, not metering a real partial call.
func TestFailedRequestKeepsVendorReportedUsage(t *testing.T) {
	entry := recordUsageCase(t, 499, TokenUsage{InputTokens: 100, OutputTokens: 2}, `{}`, `{}`)
	if !entry.Priced {
		t.Fatal("a model with a price row must stay priced")
	}
	if want := int64(550); entry.CostMicros != want {
		t.Fatalf("cost = %d, want %d (100 in at $5/M, 2 out at $25/M)", entry.CostMicros, want)
	}
}

// Success without a usage block still falls back to the body-length estimate —
// that fallback is what keeps relays that omit usage visible in the token view.
func TestSuccessfulRequestStillEstimatesUsage(t *testing.T) {
	requestBody := strings.Repeat("a", 400)
	responseBody := `{"choices":[{"message":{"content":"` + strings.Repeat("b", 80) + `"}}]}`
	entry := recordUsageCase(t, 200, TokenUsage{}, requestBody, responseBody)
	if want := int64(len(requestBody) / 4); entry.InputTokens != want {
		t.Fatalf("estimated input = %d, want %d", entry.InputTokens, want)
	}
	if want := int64(len(responseBody) / 4); entry.OutputTokens != want {
		t.Fatalf("estimated output = %d, want %d", entry.OutputTokens, want)
	}
	if entry.CostMicros <= 0 {
		t.Fatalf("estimated usage was not priced: %d", entry.CostMicros)
	}
}
