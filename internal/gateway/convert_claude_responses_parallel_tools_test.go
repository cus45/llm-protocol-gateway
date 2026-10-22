package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// 并行工具调用：上游（Responses）会依次开两个 function_call，参数增量只带
// item_id / output_index。转成 Claude 流时每个 tool_use 必须是独立的 block
// index，增量要落到各自的块上，两个块都要被 content_block_stop 关掉；否则
// 客户端看到的两个调用参数串台、或第二个块永远没关。
func TestClaudeStreamParallelToolCallsGetDistinctBlocks(t *testing.T) {
	upstream := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"get_weather","arguments":""}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_a","output_index":0,"delta":"{\"city\":\"Pa"}`,
		``,
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_a","output_index":0,"arguments":"{\"city\":\"Paris\"}"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_b","type":"function_call","call_id":"call_b","name":"get_weather","arguments":""}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_b","output_index":1,"delta":"{\"city\":\"Ro"}`,
		``,
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_b","output_index":1,"arguments":"{\"city\":\"Rome\"}"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_b","type":"function_call","call_id":"call_b","name":"get_weather","arguments":"{\"city\":\"Rome\"}"}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}}}`,
		``,
	}, "\n")

	rec := httptest.NewRecorder()
	if _, err := streamResponsesToClaudeEventsDirect(rec, strings.NewReader(upstream), "claude-sonnet-5"); err != nil {
		t.Fatalf("stream: %v", err)
	}
	for _, logged := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(logged, "event:") || strings.HasPrefix(logged, "data:") {
			t.Logf("SSE %s", logged)
		}
	}

	// writeClaudeSSE puts "event: <name>" on its own line and the payload on the
	// following "data:" line; the payload itself carries no type field.
	type block struct {
		id  string
		arg strings.Builder
		// stop is set when the block's stop was seen, keyed by index.
	}
	blocks := map[int]*block{}
	stopped := map[int]bool{}
	eventType := ""
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			continue
		}
		index := int(int64FromAny(event["index"]))
		switch eventType {
		case "content_block_start":
			contentBlock, _ := event["content_block"].(map[string]any)
			if stringValue(contentBlock["type"]) != "tool_use" {
				continue
			}
			if blocks[index] != nil {
				t.Fatalf("two tool_use blocks share index %d", index)
			}
			blocks[index] = &block{id: stringValue(contentBlock["id"])}
		case "content_block_delta":
			if current := blocks[index]; current != nil {
				if delta, ok := event["delta"].(map[string]any); ok {
					current.arg.WriteString(stringValue(delta["partial_json"]))
				}
			}
		case "content_block_stop":
			stopped[index] = true
		}
	}
	if len(blocks) != 2 {
		t.Fatalf("tool blocks = %d, want 2 (body=%s)", len(blocks), rec.Body.String())
	}
	byID := map[string]int{}
	for index, current := range blocks {
		byID[current.id] = index
	}
	for id, wantArgs := range map[string]string{"call_a": `{"city":"Paris"}`, "call_b": `{"city":"Rome"}`} {
		index, ok := byID[id]
		if !ok {
			t.Fatalf("missing tool block %s (body=%s)", id, rec.Body.String())
		}
		if got := blocks[index].arg.String(); got != wantArgs {
			t.Fatalf("%s args = %q, want %q", id, got, wantArgs)
		}
		if !stopped[index] {
			t.Fatalf("%s block never received content_block_stop", id)
		}
	}
}
