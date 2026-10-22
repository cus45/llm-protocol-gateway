package gateway

import (
	"encoding/json"
	"testing"
)

func decodeClaudeCacheTest(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("request is not json: %v (%s)", err, raw)
	}
	return req
}

func cacheMarkAt(t *testing.T, content any) map[string]any {
	t.Helper()
	blocks, ok := content.([]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("content is not a block array: %#v", content)
	}
	last, ok := blocks[len(blocks)-1].(map[string]any)
	if !ok {
		t.Fatalf("last block is not an object: %#v", blocks[len(blocks)-1])
	}
	mark, _ := last["cache_control"].(map[string]any)
	return mark
}

// Chat 的顶层 cache_control（Codex 风格）落到 system 末尾，并在对话末尾补断点：
// 否则每轮都要全价重算工具和系统提示。
func TestClaudeCacheMarksFromChatTopLevel(t *testing.T) {
	req := decodeClaudeCacheTest(t, []byte(`{
		"model":"claude-sonnet-4.5",
		"cache_control":{"type":"ephemeral"},
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":"hi"}
		]
	}`))
	claudeReq, err := openAIChatToClaudeRequest(req, "claude-sonnet-4.5", 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	system, ok := claudeReq["system"].([]any)
	if !ok || len(system) != 1 {
		t.Fatalf("system must become a block array with the mark: %#v", claudeReq["system"])
	}
	block := system[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "be brief" {
		t.Fatalf("system text changed: %#v", block)
	}
	if mark := cacheMarkAt(t, claudeReq["system"]); mark["type"] != "ephemeral" {
		t.Fatalf("system cache_control = %#v", mark)
	}
	messages := claudeReq["messages"].([]map[string]any)
	user := messages[len(messages)-1]
	if user["role"] != "user" {
		t.Fatalf("last message = %#v", user)
	}
	if mark := cacheMarkAt(t, user["content"]); mark["type"] != "ephemeral" {
		t.Fatalf("conversation tail cache_control = %#v", mark)
	}
}

// 客户端没要求缓存也要补对话断点：多轮 agent 每轮都在同一前缀上追加。
func TestClaudeCacheMarksConversationTailOnly(t *testing.T) {
	req := decodeClaudeCacheTest(t, []byte(`{
		"model":"claude-sonnet-4.5",
		"messages":[{"role":"user","content":"hi"}]
	}`))
	claudeReq, err := openAIChatToClaudeRequest(req, "claude-sonnet-4.5", 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if _, exists := claudeReq["system"]; exists {
		t.Fatalf("no system must be invented: %#v", claudeReq["system"])
	}
	messages := claudeReq["messages"].([]map[string]any)
	if mark := cacheMarkAt(t, messages[0]["content"]); mark["type"] != "ephemeral" {
		t.Fatalf("conversation tail cache_control = %#v", mark)
	}
}

// 客户端自己给的断点就算数，不再叠加。
func TestClaudeCacheMarksRespectClientMarks(t *testing.T) {
	req := decodeClaudeCacheTest(t, []byte(`{
		"model":"claude-sonnet-4.5",
		"messages":[
			{"role":"system","content":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral","ttl":"1h"}}]},
			{"role":"user","content":"hi"}
		]
	}`))
	claudeReq, err := openAIChatToClaudeRequest(req, "claude-sonnet-4.5", 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if mark := cacheMarkAt(t, claudeReq["system"]); mark["ttl"] != "1h" {
		t.Fatalf("client ttl must be kept: %#v", mark)
	}
	messages := claudeReq["messages"].([]map[string]any)
	if blocks, ok := messages[0]["content"].([]any); ok {
		if block := blocks[len(blocks)-1].(map[string]any); block["cache_control"] != nil {
			t.Fatalf("a client-marked prefix must not gain another mark: %#v", block)
		}
	}
}

// thinking 块不能当断点，要往前找最后一个可缓存的块。
func TestClaudeCacheMarksSkipThinkingTail(t *testing.T) {
	req := decodeClaudeCacheTest(t, []byte(`{
		"model":"claude-opus-4-6",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"hmm"},
				{"type":"text","text":"hello"}
			]}
		]
	}`))
	claudeReq, err := openAIChatToClaudeRequest(req, "claude-opus-4-6", 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	messages := claudeReq["messages"].([]map[string]any)
	if mark := cacheMarkAt(t, messages[len(messages)-1]["content"]); mark["type"] != "ephemeral" {
		t.Fatalf("text block before thinking must carry the mark: %#v", mark)
	}
}

// Responses 路径同样补断点，并以对话最后一个块为准。
func TestClaudeCacheMarksFromResponses(t *testing.T) {
	req := decodeClaudeCacheTest(t, []byte(`{
		"model":"claude-sonnet-4.5",
		"instructions":"be brief",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]
	}`))
	claudeReq, _, err := responsesToClaudeRequestDirect(req, "claude-sonnet-4.5", 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	messages, ok := claudeReq["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("messages = %#v", claudeReq["messages"])
	}
	last := messages[len(messages)-1].(map[string]any)
	if mark := cacheMarkAt(t, last["content"]); mark["type"] != "ephemeral" {
		t.Fatalf("responses tail cache_control = %#v", mark)
	}
}
