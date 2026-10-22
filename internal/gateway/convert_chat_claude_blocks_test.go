package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

type claudeStreamBlock struct {
	IndexStart int
	IndexStop  int
	Type       string
	Text       string // text_delta text or thinking_delta thinking
}

func parseClaudeStreamBlocks(t *testing.T, body string) []claudeStreamBlock {
	t.Helper()
	blocks := []claudeStreamBlock{}
	openByIndex := map[int]int{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		index := int(int64FromAny(event["index"]))
		switch stringValue(event["type"]) {
		case "content_block_start":
			block, _ := event["content_block"].(map[string]any)
			blocks = append(blocks, claudeStreamBlock{IndexStart: index, Type: stringValue(block["type"])})
			openByIndex[index] = len(blocks) - 1
		case "content_block_stop":
			if pos, ok := openByIndex[index]; ok {
				blocks[pos].IndexStop = index
			} else {
				t.Fatalf("content_block_stop for index %d with no matching start: %s", index, body)
			}
		case "content_block_delta":
			delta, _ := event["delta"].(map[string]any)
			pos, ok := openByIndex[index]
			if !ok {
				t.Fatalf("delta for unopened index %d", index)
			}
			blocks[pos].Text += stringValue(delta["text"]) + stringValue(delta["thinking"])
		}
	}
	return blocks
}

// Two parallel tool calls must close in the order they opened. Closing from a
// map iteration randomized that order, and a client tracking block indices
// sees stops out of sequence.
func TestChatStreamToClaudeClosesToolBlocksInOrder(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"write","arguments":"{}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	for attempt := 0; attempt < 20; attempt++ {
		recorder := httptest.NewRecorder()
		if _, err := streamOpenAIChatToClaudeEvents(recorder, strings.NewReader(input), "m", nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		body := recorder.Body.String()
		blocks := parseClaudeStreamBlocks(t, body)
		lastStop := -1
		for _, line := range strings.Split(body, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "data:") {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))), &event); err != nil {
				continue
			}
			if stringValue(event["type"]) == "content_block_stop" {
				index := int(int64FromAny(event["index"]))
				if index <= lastStop {
					t.Fatalf("content_block_stop out of order: %d after %d\n%s", index, lastStop, body)
				}
				lastStop = index
			}
		}
		if len(blocks) != 2 {
			t.Fatalf("blocks = %+v, want two tool_use blocks", blocks)
		}
		for _, block := range blocks {
			if block.IndexStart != block.IndexStop {
				t.Fatalf("start/stop index mismatch: %+v", block)
			}
		}
	}
}

// Reasoning arrives before the answer; Anthropic requires the thinking block
// to precede text, and Claude Code refuses the stream if it does not.
func TestChatStreamToClaudeEmitsThinkingBeforeText(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Let me think "},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"about it."},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"Answer."},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	recorder := httptest.NewRecorder()
	if _, err := streamOpenAIChatToClaudeEvents(recorder, strings.NewReader(input), "m", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	blocks := parseClaudeStreamBlocks(t, recorder.Body.String())
	if len(blocks) != 2 {
		t.Fatalf("blocks = %+v, want thinking + text", blocks)
	}
	if blocks[0].Type != "thinking" || blocks[0].Text != "Let me think about it." {
		t.Fatalf("first block = %+v, want thinking with full text", blocks[0])
	}
	if blocks[0].IndexStart != 0 {
		t.Fatalf("thinking block index = %d, want 0", blocks[0].IndexStart)
	}
	if blocks[1].Type != "text" || blocks[1].Text != "Answer." {
		t.Fatalf("second block = %+v, want text Answer.", blocks[1])
	}
	if blocks[1].IndexStart != 1 {
		t.Fatalf("text block index = %d, want 1", blocks[1].IndexStart)
	}
}

// Thinking → text → tool call: three blocks, strictly increasing indices, and
// the thinking block closes before the text block opens.
func TestChatStreamToClaudeThinkingTextToolOrder(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"Running it."},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"run","arguments":"{}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	recorder := httptest.NewRecorder()
	if _, err := streamOpenAIChatToClaudeEvents(recorder, strings.NewReader(input), "m", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := recorder.Body.String()
	blocks := parseClaudeStreamBlocks(t, body)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %+v, want thinking + text + tool_use\n%s", blocks, body)
	}
	wantTypes := []string{"thinking", "text", "tool_use"}
	for i, want := range wantTypes {
		if blocks[i].Type != want {
			t.Fatalf("block %d type = %q, want %q", i, blocks[i].Type, want)
		}
		if blocks[i].IndexStart != i || blocks[i].IndexStop != i {
			t.Fatalf("block %d index start/stop = %d/%d, want %d", i, blocks[i].IndexStart, blocks[i].IndexStop, i)
		}
	}
}

// Text then reasoning: the late reasoning cannot become a thinking block —
// nothing valid can follow text — so it must be dropped, and the stream must
// still be well-formed.
func TestChatStreamToClaudeDropsReasoningAfterText(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"Answer first."},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"late thoughts"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":" More."},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	recorder := httptest.NewRecorder()
	if _, err := streamOpenAIChatToClaudeEvents(recorder, strings.NewReader(input), "m", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	blocks := parseClaudeStreamBlocks(t, recorder.Body.String())
	if len(blocks) != 1 || blocks[0].Type != "text" {
		t.Fatalf("blocks = %+v, want a single text block", blocks)
	}
	if blocks[0].Text != "Answer first. More." {
		t.Fatalf("text = %q", blocks[0].Text)
	}
}
