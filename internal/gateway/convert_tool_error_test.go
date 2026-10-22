package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// Claude 的 tool_result.is_error 在 Chat 里没有字段可放，必须写进文本，
// 否则失败的工具有结果被当成成功。
func TestClaudeToolResultIsErrorToChat(t *testing.T) {
	var req map[string]any
	if err := json.Unmarshal([]byte(`{
		"model":"claude-sonnet-4.5",
		"max_tokens":100,
		"messages":[
			{"role":"user","content":"read the file"},
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read","input":{"path":"a.txt"}}]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1","is_error":true,"content":[{"type":"text","text":"ENOENT: a.txt"}]}
			]}
		]
	}`), &req); err != nil {
		t.Fatalf("request is not json: %v", err)
	}
	chatReq, err := claudeRequestToOpenAIChat(req, "gpt-4o")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var messages []map[string]any
	switch list := chatReq["messages"].(type) {
	case []map[string]any:
		messages = list
	case []any:
		for _, item := range list {
			if message, ok := item.(map[string]any); ok {
				messages = append(messages, message)
			}
		}
	}
	if len(messages) == 0 {
		t.Fatalf("no messages: %#v", chatReq["messages"])
	}
	var tool map[string]any
	for _, message := range messages {
		if message["role"] == "tool" {
			tool = message
		}
	}
	if tool == nil {
		t.Fatalf("tool message missing: %#v", messages)
	}
	content, _ := tool["content"].(string)
	if !strings.HasPrefix(content, toolResultFailedPrefix) || !strings.Contains(content, "ENOENT") {
		t.Fatalf("failed tool result = %q", content)
	}
}

// Responses 的 function_call_output.is_error 要变成 Claude 的 tool_result.is_error。
func TestResponsesToolOutputIsErrorToClaude(t *testing.T) {
	var req map[string]any
	if err := json.Unmarshal([]byte(`{
		"model":"claude-sonnet-4.5",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"read"}]},
			{"type":"function_call","call_id":"call_1","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ENOENT","is_error":true}
		]
	}`), &req); err != nil {
		t.Fatalf("request is not json: %v", err)
	}
	claudeReq, _, err := responsesToClaudeRequestDirect(req, "claude-sonnet-4.5", 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	messages, _ := claudeReq["messages"].([]any)
	found := false
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, blockItem := range blocks {
			block, ok := blockItem.(map[string]any)
			if !ok || stringValue(block["type"]) != "tool_result" {
				continue
			}
			found = true
			if block["is_error"] != true {
				t.Fatalf("tool_result is_error = %#v", block["is_error"])
			}
		}
	}
	if !found {
		t.Fatalf("no tool_result block: %#v", claudeReq["messages"])
	}
}
