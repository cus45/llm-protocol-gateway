package gateway

import (
	"strings"
	"testing"
)

// Anthropic rejects any request where an assistant tool_use has no matching
// tool_result immediately after it. A Responses client replays its history the
// same way Chat does, so an interrupted call — or an agent that sends only the
// call — used to fail the whole turn with a 400 instead of reaching the model.

func claudeToolResultIDs(messages []map[string]any) []string {
	ids := []string{}
	for _, msg := range messages {
		if stringValue(msg["role"]) != "user" {
			continue
		}
		blocks, _ := msg["content"].([]any)
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if ok && stringValue(block["type"]) == "tool_result" {
				ids = append(ids, stringValue(block["tool_use_id"]))
			}
		}
	}
	return ids
}

func TestResponsesToClaudeSynthesizesMissingToolResult(t *testing.T) {
	req := map[string]any{
		"model": "claude-sonnet-5",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "read a.go"}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": `{"path":"a.go"}`},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "never mind, do something else"}}},
		},
	}
	claudeReq, _, err := responsesToClaudeRequestDirect(req, "claude-sonnet-5", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	messages := claudeMessagesFromAny(claudeReq["messages"])

	ids := claudeToolResultIDs(messages)
	if len(ids) != 1 || ids[0] != "call_1" {
		t.Fatalf("tool_result ids = %v, want [call_1]", ids)
	}
	// The result must sit in the user message directly after the assistant call.
	found := false
	for i, msg := range messages {
		blocks, _ := msg["content"].([]any)
		hasUse := false
		for _, raw := range blocks {
			if block, ok := raw.(map[string]any); ok && stringValue(block["type"]) == "tool_use" {
				hasUse = true
			}
		}
		if !hasUse {
			continue
		}
		if i+1 >= len(messages) || stringValue(messages[i+1]["role"]) != "user" {
			t.Fatalf("tool_use at %d is not followed by a user message: %#v", i, messages)
		}
		nextBlocks, _ := messages[i+1]["content"].([]any)
		first, _ := nextBlocks[0].(map[string]any)
		if stringValue(first["type"]) != "tool_result" || stringValue(first["tool_use_id"]) != "call_1" {
			t.Fatalf("first block after tool_use = %#v, want the synthesized tool_result", first)
		}
		if text := stringValue(first["content"]); !strings.Contains(text, "unavailable") {
			t.Fatalf("synthetic tool_result text = %q", text)
		}
		found = true
	}
	if !found {
		t.Fatalf("no tool_use in %#v", messages)
	}
	// The user's own text must still be there, after the result.
	lastUser, _ := messages[len(messages)-1]["content"].([]any)
	if len(lastUser) < 2 {
		t.Fatalf("user message lost its text: %#v", lastUser)
	}
}

func TestResponsesToClaudeKeepsRealToolResult(t *testing.T) {
	req := map[string]any{
		"model": "claude-sonnet-5",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "read a.go"}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": `{"path":"a.go"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "package main"},
		},
	}
	claudeReq, _, err := responsesToClaudeRequestDirect(req, "claude-sonnet-5", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	messages := claudeMessagesFromAny(claudeReq["messages"])
	results := 0
	for _, msg := range messages {
		blocks, _ := msg["content"].([]any)
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok || stringValue(block["type"]) != "tool_result" {
				continue
			}
			results++
			if text := stringValue(block["content"]); text != "package main" {
				t.Fatalf("real tool_result was replaced: %q", text)
			}
		}
	}
	if results != 1 {
		t.Fatalf("tool_result blocks = %d, want exactly the real one", results)
	}
}

// A call at the very end of the history (the interrupted-turn shape) has no
// following user message to receive the result.
func TestResponsesToClaudeSynthesizesToolResultAtEnd(t *testing.T) {
	req := map[string]any{
		"model": "claude-sonnet-5",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "read a.go"}}},
			map[string]any{"type": "function_call", "call_id": "call_2", "name": "read_file", "arguments": `{"path":"a.go"}`},
		},
	}
	claudeReq, _, err := responsesToClaudeRequestDirect(req, "claude-sonnet-5", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	messages := claudeMessagesFromAny(claudeReq["messages"])
	ids := claudeToolResultIDs(messages)
	if len(ids) != 1 || ids[0] != "call_2" {
		t.Fatalf("tool_result ids = %v, want [call_2]", ids)
	}
	if role := stringValue(messages[len(messages)-1]["role"]); role != "user" {
		t.Fatalf("last message role = %q, want user carrying the result", role)
	}
}

func claudeMessagesFromAny(value any) []map[string]any {
	out := []map[string]any{}
	items, _ := value.([]any)
	for _, raw := range items {
		if msg, ok := raw.(map[string]any); ok {
			out = append(out, msg)
		}
	}
	return out
}
