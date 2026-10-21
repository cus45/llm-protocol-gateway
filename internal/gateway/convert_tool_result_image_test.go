package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

const probePNGDataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="

// 工具结果里的截图必须原样交给模型：浏览器类工具每次调用都回一张图，丢掉它
// 模型就只能靠猜选择器和 uid（表现为 No snapshot found / querySelector null）。
func TestClaudeToolResultImagesReachOpenAIHistory(t *testing.T) {
	req := map[string]any{
		"model": "gpt-test",
		"tools": []any{
			map[string]any{"name": "browser_click", "description": "click",
				"input_schema": map[string]any{"type": "object"}},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "看一下页面"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "browser_click",
					"input": map[string]any{"uid": "10_56"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{
					map[string]any{"type": "text", "text": "clicked"},
					map[string]any{"type": "image", "source": map[string]any{
						"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgoAAAANSUhEUg==",
					}},
				}},
			}},
		},
	}

	out, err := claudeRequestToOpenAIChat(req, "gpt-test")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	messages := messageListOf(out["messages"])
	if len(messages) < 4 {
		raw, _ := json.Marshal(out["messages"])
		t.Fatalf("expected tool turn plus image turn, got %d messages: %s", len(messages), raw)
	}

	toolMsg := messages[2]
	if toolMsg["role"] != "tool" {
		t.Fatalf("third message should be the tool result, got %v", toolMsg["role"])
	}
	toolText, _ := toolMsg["content"].(string)
	if !strings.HasPrefix(toolText, "clicked") {
		t.Fatalf("tool text should survive, got %q", toolText)
	}
	if !strings.Contains(toolText, "images; they follow") && !strings.Contains(toolText, "image; it follows") {
		t.Fatalf("tool text should tell the model the screenshot follows, got %q", toolText)
	}

	imageMsg := messages[3]
	if imageMsg["role"] != "user" {
		t.Fatalf("images must follow in a user turn, got role %v", imageMsg["role"])
	}
	parts, ok := imageMsg["content"].([]any)
	if !ok {
		t.Fatalf("image turn should be content parts, got %T", imageMsg["content"])
	}
	// 第一段必须标出这张图来自哪次调用：并行调工具时模型才分得清谁是谁
	head, _ := parts[0].(map[string]any)
	if text, _ := head["text"].(string); !strings.Contains(text, "browser_click") || !strings.Contains(text, "toolu_1") {
		t.Fatalf("image should be attributed to its tool call, got %v", head)
	}
	found := false
	for _, part := range parts {
		block, _ := part.(map[string]any)
		if block["type"] != "image_url" {
			continue
		}
		found = true
		imageURL, _ := block["image_url"].(map[string]any)
		if imageURL["url"] != probePNGDataURL {
			t.Fatalf("image url mangled: %v", imageURL["url"])
		}
	}
	if !found {
		raw, _ := json.Marshal(parts)
		t.Fatalf("no image_url part in the follow-up turn: %s", raw)
	}
}

// 同一个用户回合里既有 tool_result 又有自己的话时：tool 消息必须紧跟 assistant，
// 截图并进那条 user 消息的最前面，而不是另起一条（连着两条 user 有些上游直接拒）。
func TestToolResultsPrecedeUserTextAndImagesMergeIn(t *testing.T) {
	req := map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "点一下按钮"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_9", "name": "browser_click",
					"input": map[string]any{"uid": "10_56"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_9", "content": []any{
					map[string]any{"type": "text", "text": "clicked"},
					map[string]any{"type": "image", "source": map[string]any{
						"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgoAAAANSUhEUg==",
					}},
				}},
				map[string]any{"type": "text", "text": "继续下一步"},
			}},
		},
	}

	out, err := claudeRequestToOpenAIChat(req, "gpt-test")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	messages := messageListOf(out["messages"])
	if len(messages) != 4 {
		raw, _ := json.Marshal(messages)
		t.Fatalf("expected 4 messages (user, assistant, tool, user), got %d: %s", len(messages), raw)
	}
	if messages[2]["role"] != "tool" {
		t.Fatalf("tool message must directly follow the assistant turn, got %v", messages[2]["role"])
	}
	if messages[3]["role"] != "user" {
		t.Fatalf("the user's own turn comes last, got %v", messages[3]["role"])
	}
	parts, ok := messages[3]["content"].([]any)
	if !ok {
		t.Fatalf("merged turn should be content parts, got %T", messages[3]["content"])
	}
	var sawImage, sawText bool
	for _, part := range parts {
		block, _ := part.(map[string]any)
		switch block["type"] {
		case "image_url":
			sawImage = true
		case "text":
			if block["text"] == "继续下一步" {
				sawText = true
			}
		}
	}
	if !sawImage || !sawText {
		raw, _ := json.Marshal(parts)
		t.Fatalf("screenshot should merge into the user's own turn (image=%v text=%v): %s", sawImage, sawText, raw)
	}
}

// 反方向：客户端按 OpenAI 习惯把工具结果拆成 text / image_url 部件时，Claude 的
// tool_result 必须收到 text / image 块，原样透传会被上游判成未知 block 类型。
func TestOpenAIToolResultImagePartsBecomeClaudeBlocks(t *testing.T) {
	req := map[string]any{
		"model": "claude-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "看一下页面"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function",
					"function": map[string]any{"name": "browser_click", "arguments": "{\"uid\":\"10_56\"}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": []any{
				map[string]any{"type": "text", "text": "clicked"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": probePNGDataURL}},
			}},
		},
	}

	system, messages, err := openAIMessagesToClaude(req["messages"].([]any))
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(system) != 0 {
		t.Fatalf("unexpected system blocks: %v", system)
	}

	var toolResult map[string]any
	for _, message := range messages {
		content, _ := message["content"].([]any)
		for _, item := range content {
			block, _ := item.(map[string]any)
			if block["type"] == "tool_result" {
				toolResult = block
			}
		}
	}
	if toolResult == nil {
		t.Fatalf("no tool_result block produced: %v", messages)
	}
	blocks, ok := toolResult["content"].([]any)
	if !ok {
		t.Fatalf("tool_result content should be blocks, got %T", toolResult["content"])
	}
	var image map[string]any
	for _, item := range blocks {
		block, _ := item.(map[string]any)
		if block["type"] == "image" {
			image = block
		}
		if block["type"] == "image_url" {
			t.Fatalf("image_url leaked into a Claude tool_result: %v", block)
		}
	}
	if image == nil {
		t.Fatalf("image part was dropped: %v", blocks)
	}
	source, _ := image["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "image/png" {
		t.Fatalf("image source not converted: %v", source)
	}
}

// messageListOf 把转换结果里的 messages 统一成 map 列表：不同转换器既可能给
// []map[string]any，也可能给 []any。
func messageListOf(value any) []map[string]any {
	switch typed := value.(type) {
	case []map[string]any:
		return typed
	case []any:
		out := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if message, ok := item.(map[string]any); ok {
				out = append(out, message)
			}
		}
		return out
	default:
		return nil
	}
}

// 被中断的回合：assistant 发了两个调用，客户端只回了一个结果。严格的上游会以
// 「tool calls and tool results do not match」整条拒掉，所以没应答的那个要补一条。
func TestMissingToolResultIsFilledBeforeUserTurn(t *testing.T) {
	req := map[string]any{
		"model": "gpt-test",
		"messages": []any{
			map[string]any{"role": "user", "content": "跑两条命令"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_a", "name": "Bash", "input": map[string]any{"command": "ls"}},
				map[string]any{"type": "tool_use", "id": "toolu_b", "name": "Bash", "input": map[string]any{"command": "pwd"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_a", "content": "a.txt"},
				map[string]any{"type": "text", "text": "继续"},
			}},
		},
	}

	out, err := claudeRequestToOpenAIChat(req, "gpt-test")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	messages := messageListOf(out["messages"])
	if len(messages) != 5 {
		raw, _ := json.Marshal(messages)
		t.Fatalf("expected user, assistant, tool a, tool b(filled), user; got %d: %s", len(messages), raw)
	}
	if messages[2]["role"] != "tool" || messages[2]["tool_call_id"] != "toolu_a" {
		t.Fatalf("answered call should keep its own id: %v", messages[2])
	}
	if messages[3]["role"] != "tool" || messages[3]["tool_call_id"] != "toolu_b" {
		t.Fatalf("unanswered call must be filled in place: %v", messages[3])
	}
	if text, _ := messages[3]["content"].(string); !strings.Contains(text, "unavailable") {
		t.Fatalf("filled answer should say the result is unavailable, got %q", text)
	}
	if messages[4]["role"] != "user" {
		t.Fatalf("the user's own turn comes after all tool answers, got %v", messages[4]["role"])
	}
}
