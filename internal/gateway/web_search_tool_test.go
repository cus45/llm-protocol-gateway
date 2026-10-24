package gateway

import (
	"strings"
	"testing"
)

// Claude 原生的服务端工具（web_search_* / bash_* / computer_* …）必须原样到达
// 上游：它们用 type 作判别字段、没有 input_schema，补一个就会被上游 400
// （"…input_schema: Extra inputs are not permitted"）。Claude Code 的 WebSearch
// 走的就是这条路。
func TestClaudeServerToolsReachTheUpstreamUnchanged(t *testing.T) {
	payload := map[string]any{
		"model": "claude-sonnet-4-5",
		"messages": []any{map[string]any{
			"role":    "user",
			"content": "search the web for today's news",
		}},
		"tools": []any{
			map[string]any{
				"type":            "web_search_20250305",
				"name":            "web_search",
				"max_uses":        5,
				"allowed_domains": []any{"example.com"},
				// 客户端缓存标记必须丢掉：上游对内置工具不接受 input_schema，
				// 也不保证接受 cache_control。
				"cache_control": map[string]any{"type": "ephemeral"},
			},
			map[string]any{
				"type": "custom",
				"name": "lookup",
				"input_schema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"q": map[string]any{"type": "string"}},
				},
			},
		},
	}
	normalizeClaudePassThroughPayload(payload)

	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %#v, want both tools kept", payload["tools"])
	}
	search, _ := tools[0].(map[string]any)
	if got := stringValue(search["type"]); got != "web_search_20250305" {
		t.Fatalf("server tool type = %q, want it preserved", got)
	}
	if got := stringValue(search["name"]); got != "web_search" {
		t.Fatalf("server tool name = %q, want web_search", got)
	}
	if maxUses, _ := intFromAny(search["max_uses"]); maxUses != 5 {
		t.Fatalf("max_uses = %v, want 5", search["max_uses"])
	}
	if _, has := search["input_schema"]; has {
		t.Fatalf("a server tool must not gain an input_schema: %#v", search)
	}
	if _, has := search["cache_control"]; has {
		t.Fatalf("cache_control on a server tool must be dropped: %#v", search)
	}
	custom, _ := tools[1].(map[string]any)
	if _, has := custom["input_schema"]; !has {
		t.Fatalf("a custom tool must keep its input_schema: %#v", custom)
	}
}

// Responses 的内置 web_search 没有客户端可调用的名字，不能摊平成 function 工具；
// Claude 那头没有对应实现，硬发过去只会让上游拒绝整个请求。
func TestResponsesWebSearchIsNotTurnedIntoAClientTool(t *testing.T) {
	req := map[string]any{
		"model": "gpt-5",
		"input": "what happened today",
		"tools": []any{
			map[string]any{"type": "web_search"},
			map[string]any{
				"type":        "function",
				"name":        "lookup",
				"description": "look something up",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
	}
	ctx := buildCodexToolContextFromRequest(req)
	tools := ctx.ClaudeTools()
	if len(tools) != 1 {
		t.Fatalf("only the function tool may be flattened, got %d: %#v", len(tools), tools)
	}
	flat, _ := tools[0].(map[string]any)
	if name := stringValue(flat["name"]); name != "lookup" {
		t.Fatalf("flattened tool = %q, want lookup", name)
	}
	for _, tool := range tools {
		if strings.Contains(strings.ToLower(stringValue(tool.(map[string]any)["name"])), "web_search") {
			t.Fatalf("web_search was flattened into a client tool: %#v", tool)
		}
	}
}
