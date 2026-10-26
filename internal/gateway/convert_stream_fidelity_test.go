package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// claudeStreamToolArguments runs one Anthropic SSE through the Claude→Chat stream
// converter and reassembles the arguments the client would run the tool with.
func claudeStreamToolArguments(t *testing.T, sse string) map[string]string {
	t.Helper()
	recorder := httptest.NewRecorder()
	if _, err := streamClaudeToOpenAIChatEvents(recorder, strings.NewReader(sse), "m", nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	names := map[int]string{}
	arguments := map[int]*strings.Builder{}
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if line == "" || line == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		choices := asMapSlice(chunk["choices"])
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		for _, raw := range asMapSlice(delta["tool_calls"]) {
			call, _ := raw.(map[string]any)
			index := int(int64FromAny(call["index"]))
			function, _ := call["function"].(map[string]any)
			if name := stringValue(function["name"]); name != "" {
				names[index] = name
			}
			if args := stringValue(function["arguments"]); args != "" {
				if arguments[index] == nil {
					arguments[index] = &strings.Builder{}
				}
				arguments[index].WriteString(args)
			}
		}
	}
	out := map[string]string{}
	for index, name := range names {
		if arguments[index] != nil {
			out[name] = arguments[index].String()
		} else {
			out[name] = ""
		}
	}
	return out
}

// 一个 tool_use 的起始帧就带着完整 input（中转、合流实现会这么写）时，客户端拿到
// 的必须是那份参数，而不是空对象：空参数等于让工具静默地按默认值跑一次。
func TestClaudeStreamToolInputOnStartFrameReachesTheClient(t *testing.T) {
	got := claudeStreamToolArguments(t, `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"a.txt"}}}
data: {"type":"content_block_stop","index":0}
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}
data: {"type":"message_stop"}
`)
	if got["read"] != `{"path":"a.txt"}` {
		t.Fatalf("read was called with %q, want {\"path\":\"a.txt\"}", got["read"])
	}
}

// 常规的 input_json_delta 拼装不受影响（起始帧 input 为空对象）。
func TestClaudeStreamToolInputFromDeltasStillReassembles(t *testing.T) {
	got := claudeStreamToolArguments(t, `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"bash","input":{}}}
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"git status"}}
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":" --short\"}"}}
data: {"type":"content_block_stop","index":0}
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}
data: {"type":"message_stop"}
`)
	if got["bash"] != `{"command":"git status --short"}` {
		t.Fatalf("bash was called with %q", got["bash"])
	}
}

// Chat → Claude 的请求侧：采样与停止串不能丢也不能超限，思考强度不能被放大。
func TestChatToClaudeRequestKeepsSamplingStopsAndEffort(t *testing.T) {
	req, err := openAIChatToClaudeRequest(map[string]any{
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"temperature":      0.5,
		"top_k":            5,
		"stop":             []any{"a", "b", "c", "d", "e"},
		"reasoning_effort": "minimal",
	}, "claude-opus-5", 0)
	if err != nil {
		t.Fatal(err)
	}
	if req["top_k"] == nil {
		t.Fatal("top_k was dropped on the way to Claude")
	}
	stops := stopSequencesFromAny(req["stop_sequences"])
	if len(stops) != 4 || stops[0] != "a" || stops[3] != "d" {
		t.Fatalf("stop_sequences = %v, want Anthropic's maximum of four in order", stops)
	}
	config, _ := req["output_config"].(map[string]any)
	if config != nil && config["effort"] == "high" {
		t.Fatalf("reasoning_effort=minimal became %v: the weakest level must not turn into the strongest", config["effort"])
	}
	if got := mapReasoningEffortToClaudeEffort("minimal", "claude-opus-5"); got != "low" {
		t.Fatalf("minimal → %q, want low", got)
	}
	// 未知等级沿用预算表的 medium，而不是高档。
	if got := mapReasoningEffortToClaudeEffort("turbo", "claude-opus-5"); got != "medium" {
		t.Fatalf("unknown effort → %q, want medium (the budget table's default)", got)
	}
}

// 内容过滤的一轮（content_filter、没有正文）在 Claude 里是 refusal。但 output 是
// 一个空的 text 块时，所有 Anthropic 客户端都渲染成「什么都没发生」，和
// stop_reason=refusal 自相矛盾；refusal 必须有一句能显示的文字。
func TestContentFilteredChatTurnBecomesAVisibleClaudeRefusal(t *testing.T) {
	body, _, err := openAIChatResponseToClaude([]byte(`{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"content_filter"}]}`), "claude-x", nil)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if response["stop_reason"] != "refusal" {
		t.Fatalf("stop_reason = %v, want refusal", response["stop_reason"])
	}
	blocks := asMapSlice(response["content"])
	if len(blocks) == 0 {
		t.Fatal("refusal with no content blocks")
	}
	block, _ := blocks[0].(map[string]any)
	if strings.TrimSpace(stringValue(block["text"])) == "" {
		t.Fatalf("refusal carries an empty text block: clients render nothing (%v)", blocks)
	}
}

// 两张图各带 detail（low / original→high），转给 Claude 时必须带着：Anthropic 的
// image 块有 detail 字段，客户端要的低分辨率不能被悄悄换成默认高分辨率。
func TestChatImageDetailReachesClaude(t *testing.T) {
	blocks := openAIContentToClaudeBlocks([]any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png", "detail": "low"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/b.png", "detail": "original"}},
	})
	if len(blocks) != 2 {
		t.Fatalf("blocks = %v", blocks)
	}
	first, _ := blocks[0].(map[string]any)
	second, _ := blocks[1].(map[string]any)
	if first["detail"] != "low" {
		t.Fatalf("low detail lost: %v", first)
	}
	if second["detail"] != "high" {
		t.Fatalf("original should map to high: %v", second)
	}
}

// 控制台的推理强度一列要能读出自适应思考的档位。
func TestReasoningLabelReadsAdaptiveEffort(t *testing.T) {
	if got := reasoningLabelFromMap(map[string]any{"thinking": map[string]any{"type": "adaptive", "effort": "low"}}); got != "adaptive·low" {
		t.Fatalf("label = %q, want adaptive·low", got)
	}
}
