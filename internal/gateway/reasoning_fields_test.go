package gateway

import (
	"encoding/json"
	"testing"
)

// DeepSeek 一系（以及它前面的中转）把 reasoning 与 reasoning_content 当成同一个
// 字段，一条消息同时带两个会被整条请求拒绝（duplicate field reasoning）。转发前
// 统一成一个名字，且只留一次。
func TestDedupeReasoningFields(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":"a","reasoning":"old-name","reasoning_content":"kept"},
		{"role":"assistant","content":"b","reasoning":"only-old"},
		{"role":"user","content":"hi"}
	]}`)
	var payload map[string]any
	if err := json.Unmarshal(dedupeReasoningFields(body), &payload); err != nil {
		t.Fatal(err)
	}
	messages := asMapSlice(payload["messages"])
	first, _ := messages[0].(map[string]any)
	if _, has := first["reasoning"]; has {
		t.Fatalf("both names still on one message: %v", first)
	}
	if first["reasoning_content"] != "kept" {
		t.Fatalf("reasoning_content should win: %v", first)
	}
	second, _ := messages[1].(map[string]any)
	if _, has := second["reasoning"]; has || second["reasoning_content"] != "only-old" {
		t.Fatalf("a lone reasoning was not folded into reasoning_content: %v", second)
	}
	third, _ := messages[2].(map[string]any)
	if len(third) != 2 {
		t.Fatalf("a message without thinking was rewritten: %v", third)
	}

	// 没有可改的内容时原样返回（不重新编码）。
	clean := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	if string(dedupeReasoningFields(clean)) != string(clean) {
		t.Fatal("a body with nothing to change must come back byte for byte")
	}
}
