package gateway

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClaudeStopSequenceToChatFinishStream: Claude 报 stop_sequence 时，流式
// Chat 输出的 finish_reason 必须是 stop（Chat 只认 stop），否则客户端会当成
// 异常结束重试。正文以客户端停止序列结尾（含跨 delta 拼出的序列）同样成立。
func TestClaudeStopSequenceToChatFinishStream(t *testing.T) {
	stream := func(reason string, texts ...string) string {
		events := []string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"glm","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0}}}`,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		}
		for _, text := range texts {
			events = append(events,
				`event: content_block_delta`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+text+`"}}`)
		}
		events = append(events,
			`event: content_block_stop`,
			`data: {"type":"content_block_stop","index":0}`,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"`+reason+`","stop_sequence":"END"},"usage":{"output_tokens":7}}`,
			`event: message_stop`,
			`data: {"type":"message_stop"}`)
		return strings.Join(events, "\n\n") + "\n\n"
	}

	cases := []struct {
		name      string
		reason    string
		texts     []string
		sequences []string
		want      string
	}{
		{name: "upstream stop_sequence", reason: "stop_sequence", texts: []string{"hello END"}, want: "stop"},
		// Upstream cut the turn on its own stop sequence; the client's own
		// sequence never appeared, so keep upstream's reason.
		{name: "other stop sequence", reason: "stop_sequence", texts: []string{"hello"}, sequences: []string{"NOPE"}, want: "stop"},
		{name: "client sequence across deltas", reason: "end_turn", texts: []string{"hello EN", "D"}, sequences: []string{"END"}, want: "stop"},
		{name: "max_tokens stays", reason: "max_tokens", texts: []string{"hello"}, sequences: []string{"END"}, want: "length"},
		{name: "tool_use stays", reason: "tool_use", texts: []string{"hello"}, sequences: []string{"END"}, want: "tool_calls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if _, err := streamClaudeToOpenAIChatEvents(rec, strings.NewReader(stream(tc.reason, tc.texts...)), "glm", nil, tc.sequences...); err != nil {
				t.Fatalf("stream: %v", err)
			}
			out := rec.Body.String()
			if !strings.Contains(out, `"finish_reason":"`+tc.want+`"`) {
				t.Fatalf("finish_reason must be %s:\n%s", tc.want, out)
			}
		})
	}
}
