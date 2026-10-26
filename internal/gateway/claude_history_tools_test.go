package gateway

import (
	"testing"
)

// 历史里调用过、这次却没声明的工具：Anthropic 会整条拒绝（"must define tools"），
// 连 tools: [] 也不行。tool_search 类客户端每个请求只声明它当下认为需要的工具，
// 历史却是全的，正好撞上这条规则。
func TestClaudePassthroughStubsUndeclaredHistoryTools(t *testing.T) {
	payload := map[string]any{
		"model": "claude-x",
		"messages": []any{
			map[string]any{"role": "user", "content": "look around"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "search_files", "input": map[string]any{"q": "x"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "found"},
			}},
		},
		"tools": []any{},
	}
	normalizeClaudePassThroughPayload(payload)
	tools := asMapSlice(payload["tools"])
	if len(tools) != 1 {
		t.Fatalf("the history's tool was not declared: %v", payload["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "search_files" {
		t.Fatalf("stub names %v", tool["name"])
	}
	if schema, ok := tool["input_schema"].(map[string]any); !ok || schema["type"] != "object" {
		t.Fatalf("stub needs a schema Anthropic accepts: %v", tool["input_schema"])
	}
	if choice, _ := payload["tool_choice"].(map[string]any); choice["type"] != "none" {
		t.Fatalf("a stubbed request must not offer the tools for calling: %v", payload["tool_choice"])
	}

	// 全部声明过的请求原样保留（不补桩、不改 tool_choice）。
	declared := map[string]any{
		"model": "claude-x",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "read", "input": map[string]any{}},
			}},
		},
		"tools": []any{map[string]any{"name": "read", "input_schema": map[string]any{"type": "object"}}},
	}
	normalizeClaudePassThroughPayload(declared)
	if got := len(asMapSlice(declared["tools"])); got != 1 {
		t.Fatalf("a fully declared request grew to %d tools", got)
	}
	if _, has := declared["tool_choice"]; has {
		t.Fatalf("a fully declared request must keep its own tool_choice (none given): %v", declared["tool_choice"])
	}
}
