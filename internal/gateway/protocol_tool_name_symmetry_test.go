package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// Tool names must survive every protocol direction unchanged.
//
// The rule this file enforces: whatever a request converter does to a tool name,
// the matching response converter undoes, so the client dispatches a name it
// actually registered. A name that goes out renamed and comes back renamed
// reaches the client as an unknown tool — the failure Claude Code reports as
// "No such tool available: bash. Tool names are case-sensitive: call Bash
// instead." — which is exactly what happened when Claude→Chat rewrote "Bash" to
// "bash" in the conversation history.
//
// Each case below takes the client's declared names, converts the request,
// builds the upstream's answer **from the names the request ended up carrying**
// (so a rename on the way out is fed back on the way in), then asserts the
// client sees its own names again.

// clutterNames are the shapes that real clients send: Claude Code's TitleCase,
// lowercase third-party tools, snake_case ones, and MCP namespaced ones.
var clutterNames = []string{"Bash", "Read", "get_weather", "MyCamelTool", "mcp__srv__do"}

const clutterHistoryName = "Bash"

// mapsOf is asMapSlice with the assertion folded in, so the tables below can be
// ranged over as maps.
func mapsOf(value any) []map[string]any {
	switch typed := value.(type) {
	case []any:
		out := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case []map[string]any:
		return typed
	default:
		return nil
	}
}

// ---- fixtures ----------------------------------------------------------------

func claudeClientRequest() map[string]any {
	tools := make([]any, 0, len(clutterNames))
	for _, name := range clutterNames {
		tools = append(tools, map[string]any{
			"name":        name,
			"description": name + " tool",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"q": map[string]any{"type": "string"}},
			},
		})
	}
	return map[string]any{
		"model":      "m",
		"max_tokens": 256,
		"messages": []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": clutterHistoryName, "input": map[string]any{"q": "x"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"},
			}},
			map[string]any{"role": "user", "content": "again"},
		},
		"tools": tools,
	}
}

func chatClientRequest() map[string]any {
	tools := make([]any, 0, len(clutterNames))
	for _, name := range clutterNames {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": name + " tool",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		})
	}
	return map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
					"name": clutterHistoryName, "arguments": `{"q":"x"}`,
				}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
			map[string]any{"role": "user", "content": "again"},
		},
		"tools": tools,
	}
}

func responsesClientRequest() map[string]any {
	tools := make([]any, 0, len(clutterNames))
	for _, name := range clutterNames {
		tools = append(tools, map[string]any{
			"type":        "function",
			"name":        name,
			"description": name + " tool",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		})
	}
	return map[string]any{
		"model":             "m",
		"max_output_tokens": 256,
		"instructions":      "go",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "go"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": clutterHistoryName, "arguments": `{"q":"x"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "again"},
			}},
		},
		"tools": tools,
	}
}

// ---- name extraction & upstream answers --------------------------------------

// outboundNames reads the tool names a converted request carries: the declared
// tools, and the names its history calls them by.
func outboundNames(body map[string]any) (declared []string, history []string) {
	for _, item := range mapsOf(body["tools"]) {
		name := stringValue(item["name"])
		if name == "" {
			if fn, ok := item["function"].(map[string]any); ok {
				name = stringValue(fn["name"])
			}
		}
		if name != "" {
			declared = append(declared, name)
		}
	}
	for _, item := range mapsOf(body["messages"]) {
		for _, call := range mapsOf(item["tool_calls"]) {
			if fn, ok := call["function"].(map[string]any); ok {
				if name := stringValue(fn["name"]); name != "" {
					history = append(history, name)
				}
			}
		}
		for _, block := range mapsOf(item["content"]) {
			if stringValue(block["type"]) == "tool_use" {
				if name := stringValue(block["name"]); name != "" {
					history = append(history, name)
				}
			}
		}
	}
	for _, item := range mapsOf(body["input"]) {
		switch stringValue(item["type"]) {
		case "function_call", "custom_tool_call":
			if name := stringValue(item["name"]); name != "" {
				history = append(history, name)
			}
		}
	}
	return declared, history
}

func chatAnswer(names []string) []byte {
	calls := make([]any, 0, len(names))
	for i, name := range names {
		calls = append(calls, map[string]any{
			"id": "call_r" + string(rune('a'+i)), "type": "function",
			"function": map[string]any{"name": name, "arguments": `{}`},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion", "model": "m",
		"choices": []any{map[string]any{
			"index":         0,
			"finish_reason": "tool_calls",
			"message":       map[string]any{"role": "assistant", "content": nil, "tool_calls": calls},
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
	return body
}

func claudeAnswer(names []string) []byte {
	blocks := make([]any, 0, len(names))
	for i, name := range names {
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": "toolu_r" + string(rune('a'+i)), "name": name, "input": map[string]any{},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "m",
		"content":     blocks,
		"stop_reason": "tool_use",
		"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
	})
	return body
}

func responsesAnswer(names []string) []byte {
	items := make([]any, 0, len(names))
	for i, name := range names {
		items = append(items, map[string]any{
			"type": "function_call", "id": "fc_" + string(rune('a'+i)),
			"call_id": "call_r" + string(rune('a'+i)), "name": name,
			"arguments": `{}`, "status": "completed",
		})
	}
	body, _ := json.Marshal(map[string]any{
		"id": "resp_1", "object": "response", "status": "completed", "model": "m",
		"output": items,
		"usage":  map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
	})
	return body
}

// clientVisibleNames reads the tool names out of the body handed back to the
// client, whichever protocol that client speaks.
func clientVisibleNames(t *testing.T, body []byte) []string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("client body is not JSON: %v (%s)", err, body)
	}
	var names []string
	for _, item := range mapsOf(payload["content"]) {
		if stringValue(item["type"]) == "tool_use" {
			names = append(names, stringValue(item["name"]))
		}
	}
	for _, choice := range mapsOf(payload["choices"]) {
		message, _ := choice["message"].(map[string]any)
		for _, call := range mapsOf(message["tool_calls"]) {
			fn, _ := call["function"].(map[string]any)
			names = append(names, stringValue(fn["name"]))
		}
	}
	for _, item := range mapsOf(payload["output"]) {
		if stringValue(item["type"]) == "function_call" {
			names = append(names, stringValue(item["name"]))
		}
	}
	return names
}

func namesWithPrefix(prefix string, count int) []string {
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, prefix+string(rune('a'+i)))
	}
	return out
}

func assertNames(t *testing.T, label string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

// ---- the matrix --------------------------------------------------------------

func TestProtocolToolNameSymmetry(t *testing.T) {
	type direction struct {
		name    string
		convert func(t *testing.T) (outbound map[string]any, client func([]string) []byte)
	}

	// Every case: convert the client's request, hand its own names back as the
	// upstream's answer, convert that answer for the client, and compare.
	run := func(d direction) {
		t.Run(d.name, func(t *testing.T) {
			outbound, answerFor := d.convert(t)
			sent, history := outboundNames(outbound)
			assertNames(t, "outbound tools", sent, clutterNames)
			assertNames(t, "outbound history call", history, []string{clutterHistoryName})

			clientBody := answerFor(sent)
			assertNames(t, "client-visible tools", clientVisibleNames(t, clientBody), clutterNames)
		})
	}

	run(direction{
		name: "claude client -> chat upstream",
		convert: func(t *testing.T) (map[string]any, func([]string) []byte) {
			out, err := claudeRequestToOpenAIChat(claudeClientRequest(), "m")
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			return out, func(names []string) []byte {
				body, _, err := openAIChatResponseToClaude(chatAnswer(names), "m", claudeDeclaredToolNames(claudeClientRequest()["tools"]))
				if err != nil {
					t.Fatalf("response: %v", err)
				}
				return body
			}
		},
	})

	run(direction{
		name: "chat client -> claude upstream",
		convert: func(t *testing.T) (map[string]any, func([]string) []byte) {
			out, err := openAIChatToClaudeRequest(chatClientRequest(), "m", 0)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			return out, func(names []string) []byte {
				body, _, err := claudeResponseToOpenAIChat(claudeAnswer(names), "m", extractOpenAIToolNames(chatClientRequest()))
				if err != nil {
					t.Fatalf("response: %v", err)
				}
				return body
			}
		},
	})

	run(direction{
		name: "claude client -> responses upstream",
		convert: func(t *testing.T) (map[string]any, func([]string) []byte) {
			out, err := claudeToResponsesRequestDirect(claudeClientRequest(), "m")
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			return out, func(names []string) []byte {
				body, _, err := responsesToClaudeResponseDirect(responsesAnswer(names), "m")
				if err != nil {
					t.Fatalf("response: %v", err)
				}
				return body
			}
		},
	})

	run(direction{
		name: "chat client -> responses upstream",
		convert: func(t *testing.T) (map[string]any, func([]string) []byte) {
			out, err := openAIChatToResponsesRequest(chatClientRequest(), "m")
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			return out, func(names []string) []byte {
				body, _, err := responsesToOpenAIChatResponse(responsesAnswer(names), "m")
				if err != nil {
					t.Fatalf("response: %v", err)
				}
				return body
			}
		},
	})

	run(direction{
		name: "responses client -> chat upstream",
		convert: func(t *testing.T) (map[string]any, func([]string) []byte) {
			out, toolCtx, err := responsesToOpenAIChatRequest(responsesClientRequest(), "m")
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			return out, func(names []string) []byte {
				body, _, err := openAIChatToResponsesResponseWithTools(chatAnswer(names), "m", toolCtx)
				if err != nil {
					t.Fatalf("response: %v", err)
				}
				return body
			}
		},
	})

	run(direction{
		name: "responses client -> claude upstream",
		convert: func(t *testing.T) (map[string]any, func([]string) []byte) {
			out, toolCtx, err := responsesToClaudeRequestDirect(responsesClientRequest(), "m", 0)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			return out, func(names []string) []byte {
				body, _, err := claudeToResponsesResponseDirect(claudeAnswer(names), "m", toolCtx)
				if err != nil {
					t.Fatalf("response: %v", err)
				}
				return body
			}
		},
	})
}

// The Claude OAuth cloak renames third-party tool names on the way out. The
// answer must come back wearing the names the client declared — Claude Code's
// own names are already TitleCase, so the alias map is empty for them and the
// pass-through costs nothing.
func TestClaudeOAuthCloakNamesRoundTrip(t *testing.T) {
	req := claudeClientRequest()
	aliases := claudeOAuthToolNameAliases(req)
	if len(aliases) == 0 {
		t.Fatalf("expected the cloak to rename lowercase tools, got no aliases")
	}
	applyClaudeOAuthCloaking(req)
	cloaked, _ := outboundNames(req)
	// bash → Bash is the cloak's own work, so "Bash" and "Read" stay put while
	// the lowercase tools get TitleCased.
	assertNames(t, "cloaked tools", cloaked, []string{"Bash", "Read", "GetWeather", "MyCamelTool", "mcp__srv__do"})

	clientBody := renameClaudeToolUseNamesJSON(claudeAnswer(cloaked), aliases)
	assertNames(t, "client-visible tools", clientVisibleNames(t, clientBody), clutterNames)
}

// A Claude Code request wears names the cloak never touches, so nothing is
// rewritten on either hop.
func TestClaudeOAuthCloakLeavesClaudeCodeAlone(t *testing.T) {
	req := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			map[string]any{"name": "Bash", "input_schema": map[string]any{"type": "object"}},
			map[string]any{"name": "mcp__server__tool", "input_schema": map[string]any{"type": "object"}},
		},
	}
	if aliases := claudeOAuthToolNameAliases(req); len(aliases) != 0 {
		t.Fatalf("expected no aliases for Claude Code names, got %v", aliases)
	}
}

// Parallel tool use must survive Responses→Chat in the non-streaming path as
// well as the streaming one: the non-stream converter used to keep only the
// first call, so the client never ran the others.
func TestResponsesToChatKeepsParallelToolCalls(t *testing.T) {
	body, _, err := responsesToOpenAIChatResponse(responsesAnswer([]string{"Bash", "Read", "get_weather"}), "m")
	if err != nil {
		t.Fatal(err)
	}
	assertNames(t, "tool_calls", clientVisibleNames(t, body), []string{"Bash", "Read", "get_weather"})
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	choice := mapsOf(payload["choices"])[0]
	if reason := stringValue(choice["finish_reason"]); reason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", reason)
	}
}

// A Codex namespace tool travels as "ns__tool" (that is the only rename left in
// the codebase, and the upstream validates it), so the answer must come back as
// the namespaced call the client registered — not as the flattened proxy name.
func TestCodexNamespaceToolNamesRoundTrip(t *testing.T) {
	req := responsesClientRequest()
	req["tools"] = []any{
		map[string]any{
			"type": "namespace",
			"name": "repo",
			"tools": []any{
				map[string]any{"type": "function", "name": "status", "parameters": map[string]any{"type": "object"}},
				map[string]any{"type": "function", "name": "log", "parameters": map[string]any{"type": "object"}},
			},
		},
	}
	claudeReq, toolCtx, err := responsesToClaudeRequestDirect(req, "m", 0)
	if err != nil {
		t.Fatal(err)
	}
	sent := make([]string, 0, 2)
	for _, tool := range mapsOf(claudeReq["tools"]) {
		sent = append(sent, stringValue(tool["name"]))
	}
	assertNames(t, "claude-side tools", sent, []string{"repo__status", "repo__log"})

	body, _, err := claudeToResponsesResponseDirect(claudeAnswer(sent), "m", toolCtx)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, 2)
	for _, item := range mapsOf(payload["output"]) {
		if stringValue(item["type"]) != "function_call" {
			continue
		}
		got = append(got, stringValue(item["namespace"])+"/"+stringValue(item["name"]))
	}
	assertNames(t, "client-visible calls", got, []string{"repo/status", "repo/log"})
}
