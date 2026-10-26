package gateway

import (
	"testing"
)

// 一个被中途打断的轮次（Ctrl-C、崩溃）会在历史里留下一个没有结果的 function_call。
// OpenAI 一系的上游对之后每一轮都报「No tool output found for function call …」，
// 直到用户换一个新会话；这里给它补一条说明性的输出。
func TestInterruptedToolCallGetsASyntheticOutput(t *testing.T) {
	items, _, err := inputItemsFromChatMessages([]any{
		map[string]any{"role": "user", "content": "read a.txt"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"path":"a.txt"}`}},
		}},
		map[string]any{"role": "user", "content": "never mind"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls, outputs int
	for _, raw := range items {
		entry, _ := raw.(map[string]any)
		switch stringValue(entry["type"]) {
		case "function_call":
			calls++
		case "function_call_output":
			if stringValue(entry["call_id"]) == "call_1" && stringValue(entry["output"]) == "" {
				t.Fatalf("synthetic output is empty: %v", entry)
			}
			outputs++
		}
	}
	if calls != 1 || outputs != 1 {
		t.Fatalf("calls=%d outputs=%d, want the interrupted call paired with one output", calls, outputs)
	}

	// 已经有结果的不重复补。
	items, _, err = inputItemsFromChatMessages([]any{
		map[string]any{"role": "user", "content": "read a.txt"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read", "arguments": `{}`}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	outputs = 0
	for _, raw := range items {
		entry, _ := raw.(map[string]any)
		if stringValue(entry["type"]) == "function_call_output" {
			outputs++
		}
	}
	if outputs != 1 {
		t.Fatalf("a call that already had its result got %d outputs", outputs)
	}
}
