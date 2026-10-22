package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// Claude 的 stop_sequences 走到 Responses 上游时不能丢：上游看不见停止序列，
// 网关要在回包时按客户端序列本地匹配、报 stop_sequence，并且只有 turn 正常
// 结束时才改写状态 —— 被截断（incomplete）的响应不能被说成 completed。
func TestClaudeStopSequencesMatchOnResponsesUpstream(t *testing.T) {
	cutOff := []byte(`{"id":"msg_1","model":"gpt-5","stop_reason":"max_tokens",` +
		`"content":[{"type":"text","text":"partial END"}],` +
		`"usage":{"input_tokens":5,"output_tokens":7}}`)
	body, _, err := claudeToResponsesResponseDirect(cutOff, "gpt-5", nil, "END")
	if err != nil {
		t.Fatalf("claudeToResponsesResponseDirect: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["status"] != "incomplete" {
		t.Fatalf("truncated turn reported status %#v, want incomplete", payload["status"])
	}
	if details, _ := payload["incomplete_details"].(map[string]any); details["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details = %#v, want max_output_tokens", payload["incomplete_details"])
	}

	ended := []byte(`{"id":"msg_2","model":"gpt-5","stop_reason":"end_turn",` +
		`"content":[{"type":"text","text":"answer END"}],` +
		`"usage":{"input_tokens":5,"output_tokens":7}}`)
	body, _, err = claudeToResponsesResponseDirect(ended, "gpt-5", nil, "END")
	if err != nil {
		t.Fatalf("claudeToResponsesResponseDirect: %v", err)
	}
	payload = map[string]any{}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload["status"] != "completed" || payload["stop_sequence"] != "END" {
		t.Fatalf("completed turn = status %#v / stop_sequence %#v, want completed / END",
			payload["status"], payload["stop_sequence"])
	}

	// 没有停在任何序列上的正常结束：不该凭空报 stop_sequence。
	if body, _, err = claudeToResponsesResponseDirect(ended, "gpt-5", nil, "STOP"); err != nil {
		t.Fatalf("claudeToResponsesResponseDirect: %v", err)
	}
	payload = map[string]any{}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, exists := payload["stop_sequence"]; exists {
		t.Fatalf("unmatched sequence still reported: %#v", payload["stop_sequence"])
	}
}

// Claude→Responses 的 proxy 全链路：上游收到的 Responses 请求体里没有 stop，
// 但回给客户端的正文仍按客户的 stop_sequences 报 stop_sequence。
func TestResponsesToClaudeProxyHonoursClientStopSequences(t *testing.T) {
	var sent []byte
	// The probe upstream answers in Claude shape (what a Claude-protocol provider
	// returns); the gateway's direct converter turns it into the Responses answer.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5",` +
			`"content":[{"type":"text","text":"answer END"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":5,"output_tokens":7}}`))
	}))
	defer upstream.Close()

	provider := domain.Provider{
		ID: "p_resp", Name: "resp", Protocol: domain.ProtocolOpenAIResponses,
		BaseURL: upstream.URL, APIKeySource: "literal:test-secret",
	}
	server := NewServer(NewRouter(domain.GatewayState{Providers: []domain.Provider{provider}}), monitor.NewStore())

	claudeReq := map[string]any{
		"model":          "claude-sonnet-4-5",
		"max_tokens":     64,
		"stop_sequences": []any{"END"},
		"messages":       []any{map[string]any{"role": "user", "content": "hi"}},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	status, _, body, err := server.proxyResponsesToClaudeMessages(rec, req, provider, "gpt-5", claudeReq, false)
	if err != nil {
		t.Fatalf("proxyResponsesToClaudeMessages: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 (%s)", status, body)
	}
	var upstreamReq map[string]any
	if err := json.Unmarshal(sent, &upstreamReq); err != nil {
		t.Fatalf("upstream body: %v (%s)", err, sent)
	}
	if stop, exists := upstreamReq["stop"]; exists {
		t.Fatalf("upstream Responses body carried stop: %#v", stop)
	}
	var answer map[string]any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("answer body: %v (%s)", err, body)
	}
	if answer["status"] != "completed" || answer["stop_sequence"] != "END" {
		t.Fatalf("answer status/stop_sequence = %#v / %#v, want completed / END", answer["status"], answer["stop_sequence"])
	}
}
