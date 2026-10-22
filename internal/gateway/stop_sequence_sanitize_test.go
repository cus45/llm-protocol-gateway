package gateway

import (
	"encoding/json"
	"testing"
)

func TestSanitizeClaudeStopSequences(t *testing.T) {
	cases := []struct {
		name   string
		raw    any
		want   []any
		absent bool
	}{
		// GLM 等上游把 stop_sequences 反序列化成 List<String>：对象项会让整个请求
		// 400（Cannot construct instance of java.util.ArrayList），必须剔除。
		{name: "drops non-strings", raw: []any{"STOP", map[string]any{"a": 1}, 5, nil}, want: []any{"STOP"}},
		// 空串在上游表示"立即停止"，流会在第一个 token 前结束。
		{name: "drops empty strings", raw: []any{"", "END", ""}, want: []any{"END"}},
		{name: "caps at four", raw: []any{"a", "b", "c", "d", "e"}, want: []any{"a", "b", "c", "d"}},
		{name: "string stays list", raw: "END", want: []any{"END"}},
		{name: "all dropped", raw: []any{"", map[string]any{}}, absent: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"model": "glm-5.3", "stop_sequences": tc.raw}
			sanitizeClaudeStopSequences(payload)
			got, exists := payload["stop_sequences"]
			if tc.absent {
				if exists {
					t.Fatalf("stop_sequences = %#v, want absent", got)
				}
				return
			}
			list, ok := got.([]any)
			if !ok {
				t.Fatalf("stop_sequences = %#v, want a list", got)
			}
			if len(list) != len(tc.want) {
				t.Fatalf("stop_sequences = %#v, want %#v", list, tc.want)
			}
			for i := range tc.want {
				if list[i] != tc.want[i] {
					t.Fatalf("stop_sequences = %#v, want %#v", list, tc.want)
				}
			}
			if payload["model"] != "glm-5.3" {
				t.Fatalf("other fields must survive: %#v", payload)
			}
		})
	}

	// Absent field stays absent.
	payload := map[string]any{"model": "glm-5.3"}
	sanitizeClaudeStopSequences(payload)
	if _, exists := payload["stop_sequences"]; exists {
		t.Fatalf("stop_sequences must not be invented")
	}
}

func TestSanitizeClaudeStopSequencesBody(t *testing.T) {
	clean := []byte(`{"model":"glm-5.3","stop_sequences":["END"],"messages":[]}`)
	if got := sanitizeClaudeStopSequencesBody(clean); string(got) != string(clean) {
		t.Fatalf("clean body must stay byte-identical, got %s", got)
	}
	untouched := []byte(`{"model":"glm-5.3","messages":[]}`)
	if got := sanitizeClaudeStopSequencesBody(untouched); string(got) != string(untouched) {
		t.Fatalf("body without stop_sequences must stay byte-identical, got %s", got)
	}
	bad := []byte(`{"model":"glm-5.3","stop_sequences":[{"bad":1},"","END"],"messages":[]}`)
	var payload map[string]any
	if err := json.Unmarshal(sanitizeClaudeStopSequencesBody(bad), &payload); err != nil {
		t.Fatalf("rewritten body is not json: %v", err)
	}
	list, _ := payload["stop_sequences"].([]any)
	if len(list) != 1 || list[0] != "END" {
		t.Fatalf("stop_sequences = %#v, want [END]", payload["stop_sequences"])
	}
}
