package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Responses 的流式终态事件必须和 status 一致：被截断的 turn 发
// response.incomplete，不能发 response.completed —— 严格客户端（Codex 等）
// 只看事件名，会把截断当成正常完成并丢掉后半段。
func TestChatToResponsesStreamEmitsIncompleteEvent(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"}}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-5","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"

	rec := httptest.NewRecorder()
	if _, err := streamOpenAIChatToResponsesEvents(rec, strings.NewReader(upstream), "gpt-5"); err != nil {
		t.Fatalf("stream: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "event: response.completed") {
		t.Fatalf("truncated turn emitted response.completed:\n%s", body)
	}
	if !strings.Contains(body, "event: response.incomplete") {
		t.Fatalf("truncated turn did not emit response.incomplete:\n%s", body)
	}
	if !strings.Contains(body, `"reason":"max_output_tokens"`) {
		t.Fatalf("incomplete event lacks reason:\n%s", body)
	}
}

func TestClaudeToResponsesStreamEmitsIncompleteEvent(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-5","usage":{"input_tokens":4,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":16}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	rec := httptest.NewRecorder()
	if _, err := streamClaudeToResponsesEventsDirect(rec, strings.NewReader(upstream), "claude-sonnet-5", nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "event: response.completed") {
		t.Fatalf("truncated turn emitted response.completed:\n%s", body)
	}
	if !strings.Contains(body, "event: response.incomplete") || !strings.Contains(body, `"reason":"max_output_tokens"`) {
		t.Fatalf("truncated turn did not emit response.incomplete with reason:\n%s", body)
	}
}

// 正常结束仍然是 response.completed。
func TestNormalStreamStillEmitsCompleted(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-5","usage":{"input_tokens":4,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
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
	if body := rec.Body.String(); !strings.Contains(body, "event: response.completed") {
		t.Fatalf("normal turn did not emit response.completed:\n%s", body)
	}
}
