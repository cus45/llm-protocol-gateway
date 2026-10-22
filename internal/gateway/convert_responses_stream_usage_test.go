package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func finalResponsesEvent(t *testing.T, body string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			continue
		}
		switch stringValue(event["type"]) {
		case "response.completed", "response.incomplete":
			found, _ = event["response"].(map[string]any)
		}
	}
	if found == nil {
		t.Fatalf("no terminal event in %s", body)
	}
	return found
}

// Claude→Responses 的流式终态 usage 要和非流式一致：带上
// input_tokens_details.cached_tokens。缺了这个字段，客户端（以及网关自己的
// 一致性检查）看到的是"没有缓存"，按缓存计价的账也就对不上。
func TestClaudeToResponsesStreamReportsCachedTokens(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-5","usage":{"input_tokens":10,"cache_read_input_tokens":90,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	rec := httptest.NewRecorder()
	if _, err := streamClaudeToResponsesEventsDirect(rec, strings.NewReader(upstream), "claude-sonnet-5", nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	response := finalResponsesEvent(t, rec.Body.String())
	usage, _ := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(100) || usage["output_tokens"] != float64(3) {
		t.Fatalf("usage = %#v, want input 100 / output 3", usage)
	}
	details, _ := usage["input_tokens_details"].(map[string]any)
	if details == nil || details["cached_tokens"] != float64(90) {
		t.Fatalf("usage.input_tokens_details = %#v, want cached_tokens 90", usage["input_tokens_details"])
	}
}
