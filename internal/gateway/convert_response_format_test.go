package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
)

func marshalForTest(t *testing.T, value any) (map[string]any, string) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return decoded, string(raw)
}

func TestChatResponseFormatToOutputConfigFormat(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"city": map[string]any{"type": "string"}},
		"required":   []any{"city"},
	}
	cases := []struct {
		name string
		in   any
		want map[string]any
	}{
		{
			// Claude 没有 json_object 模式：放宽成空对象 schema。
			name: "json_object",
			in:   map[string]any{"type": "json_object"},
			want: map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}},
		},
		{
			name: "json_schema nested",
			in: map[string]any{"type": "json_schema", "json_schema": map[string]any{
				"name": "weather", "schema": schema, "strict": true,
			}},
			want: map[string]any{"type": "json_schema", "schema": schema},
		},
		{
			name: "json_schema flat",
			in:   map[string]any{"type": "json_schema", "name": "weather", "schema": schema},
			want: map[string]any{"type": "json_schema", "schema": schema},
		},
		{name: "text means no constraint", in: map[string]any{"type": "text"}},
		{name: "schema without body", in: map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "x"}}},
		{name: "not a map", in: "json"},
		{name: "absent", in: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chatResponseFormatToOutputConfigFormat(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestOutputConfigFormatToChatResponseFormat(t *testing.T) {
	schema := map[string]any{"type": "object"}
	got := outputConfigFormatToChatResponseFormat(map[string]any{
		"effort": "high",
		"format": map[string]any{"type": "json_schema", "name": "weather", "schema": schema},
	})
	want := map[string]any{
		"type":        "json_schema",
		"json_schema": map[string]any{"name": "weather", "schema": schema},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	if got := outputConfigFormatToChatResponseFormat(map[string]any{"effort": "high"}); got != nil {
		t.Fatalf("effort only must not invent a format: %#v", got)
	}
	if got := outputConfigFormatToChatResponseFormat(nil); got != nil {
		t.Fatalf("nil config: %#v", got)
	}
}

func TestSetClaudeOutputConfigFormatKeepsEffort(t *testing.T) {
	claudeReq := map[string]any{"output_config": map[string]any{"effort": "low"}}
	setClaudeOutputConfigFormat(claudeReq, map[string]any{"type": "json_object"})
	config, _ := claudeReq["output_config"].(map[string]any)
	if config["effort"] != "low" {
		t.Fatalf("effort lost: %#v", config)
	}
	if !reflect.DeepEqual(config["format"], map[string]any{"type": "json_object"}) {
		t.Fatalf("format = %#v", config["format"])
	}

	// 无 output_config 时新建。
	fresh := map[string]any{}
	setClaudeOutputConfigFormat(fresh, map[string]any{"type": "json_object"})
	if _, ok := fresh["output_config"].(map[string]any); !ok {
		t.Fatalf("output_config missing: %#v", fresh)
	}
}

// TestChatToClaudeRequestCarriesResponseFormat: ◄ 请求转换把 response_format
// 落到 output_config.format，且不影响其它字段。
func TestChatToClaudeRequestCarriesResponseFormat(t *testing.T) {
	chatReq := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "weather in Paris?"}},
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{
			"name":   "weather",
			"schema": map[string]any{"type": "object"},
		}},
	}
	claudeReq, err := openAIChatToClaudeRequest(chatReq, "claude-sonnet-4.5", 0)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	config, _ := claudeReq["output_config"].(map[string]any)
	format, _ := config["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("output_config.format = %#v", config["format"])
	}
	if _, has := format["name"]; has {
		t.Fatalf("format must not carry name: %#v", config["format"])
	}
	if _, ok := format["schema"].(map[string]any); !ok {
		t.Fatalf("schema missing: %#v", format)
	}

	// 没有 response_format 时不凭空产生 output_config。
	plain, _ := openAIChatToClaudeRequest(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, "claude-sonnet-4.5", 0)
	if _, exists := plain["output_config"]; exists {
		t.Fatalf("output_config invented: %#v", plain["output_config"])
	}
}

// TestClaudeToChatRequestCarriesResponseFormat: 反向转换把 output_config.format
// 放回 response_format。
func TestClaudeToChatRequestCarriesResponseFormat(t *testing.T) {
	claudeReq := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "weather?"}},
		"output_config": map[string]any{
			"effort": "high",
			"format": map[string]any{"type": "json_schema", "name": "weather", "schema": map[string]any{"type": "object"}},
		},
	}
	chatReq, err := claudeRequestToOpenAIChat(claudeReq, "gpt-5.5")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	format, _ := chatReq["response_format"].(map[string]any)
	inner, _ := format["json_schema"].(map[string]any)
	if format["type"] != "json_schema" || inner["name"] != "weather" {
		t.Fatalf("response_format = %#v", chatReq["response_format"])
	}

	// 只有 effort 的 output_config 不得变成 response_format。
	effortOnly, _ := claudeRequestToOpenAIChat(map[string]any{
		"messages":      []any{map[string]any{"role": "user", "content": "hi"}},
		"output_config": map[string]any{"effort": "low"},
	}, "claude-opus-4.5")
	if _, exists := effortOnly["response_format"]; exists {
		t.Fatalf("response_format invented: %#v", effortOnly["response_format"])
	}
}

// H-1/H-2: 同一份 schema 经 Chat response_format 与 Responses text.format
// 两条路径，必须产出逐字节相同的 Claude output_config.format：只带
// {type, schema}，不带 name，不带 Claude 拒收的关键字。
func TestStructuredFormatSameForChatAndResponses(t *testing.T) {
	schema := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object",
		"properties": map[string]any{
			"tags": map[string]any{"type": "array", "maxItems": 3, "items": map[string]any{"type": "string", "minLength": 1}},
		},
	}
	chatReq, err := openAIChatToClaudeRequest(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{
			"name": "tags", "schema": schema, "strict": true,
		}},
	}, "claude-sonnet-4.5", 0)
	if err != nil {
		t.Fatalf("chat convert: %v", err)
	}
	responsesReq, _, err := responsesToClaudeRequestDirect(map[string]any{
		"model": "claude-sonnet-4.5",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}},
		"text":  map[string]any{"format": map[string]any{"type": "json_schema", "name": "tags", "schema": schema, "strict": true}},
	}, "claude-sonnet-4.5", 0)
	if err != nil {
		t.Fatalf("responses convert: %v", err)
	}
	chatCfg, _ := chatReq["output_config"].(map[string]any)
	respCfg, _ := responsesReq["output_config"].(map[string]any)
	chatFormat, chatJSON := marshalForTest(t, chatCfg["format"])
	respFormat, respJSON := marshalForTest(t, respCfg["format"])
	if chatJSON != respJSON {
		t.Fatalf("chat format %s != responses format %s", chatJSON, respJSON)
	}
	if _, has := chatFormat["name"]; has {
		t.Fatalf("Claude GA rejects format.name: chat %s / responses %s", chatJSON, respJSON)
	}
	chatSchema, _ := chatFormat["schema"].(map[string]any)
	respSchema, _ := respFormat["schema"].(map[string]any)
	if _, has := chatSchema["$schema"]; has {
		t.Fatalf("$schema must be sanitized: chat %s / responses %s", chatJSON, respJSON)
	}
	if _, has := respSchema["$schema"]; has {
		t.Fatalf("$schema must be sanitized: chat %s / responses %s", chatJSON, respJSON)
	}

	// json_object 没有 Claude 模式：两条路径都放宽成空对象 schema。
	objChat, _ := openAIChatToClaudeRequest(map[string]any{
		"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
		"response_format": map[string]any{"type": "json_object"},
	}, "claude-sonnet-4.5", 0)
	_, objChatJSON := marshalForTest(t, objChat["output_config"])
	if want := `{"format":{"schema":{"type":"object"},"type":"json_schema"}}`; objChatJSON != want {
		t.Fatalf("json_object output_config = %s, want %s", objChatJSON, want)
	}
}

// H-3: response_format 与 thinking effort 同时出现时，两者都保留。
func TestChatToClaudeKeepsFormatWithThinkingEffort(t *testing.T) {
	cases := []struct {
		name string
		req  map[string]any
	}{
		{
			name: "reasoning_effort adaptive",
			req: map[string]any{
				"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
				"reasoning_effort": "high",
				"response_format":  map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "x", "schema": map[string]any{"type": "object"}}},
			},
		},
		{
			name: "explicit adaptive thinking",
			req: map[string]any{
				"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
				"thinking":        map[string]any{"type": "adaptive", "effort": "low"},
				"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "x", "schema": map[string]any{"type": "object"}}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claudeReq, err := openAIChatToClaudeRequest(tc.req, "claude-sonnet-5", 0)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			config, _ := claudeReq["output_config"].(map[string]any)
			if config["effort"] == nil {
				t.Fatalf("effort lost: %#v", config)
			}
			format, _ := config["format"].(map[string]any)
			if format == nil || format["type"] != "json_schema" {
				t.Fatalf("format lost: %#v", config)
			}
			if thinking, _ := claudeReq["thinking"].(map[string]any); thinking["type"] != "adaptive" {
				t.Fatalf("thinking = %#v", claudeReq["thinking"])
			}
		})
	}
}

// D32-1: Chat 的 stop 必须落到 Claude stop_sequences（字符串与数组都接受）。
func TestChatToClaudeCarriesStop(t *testing.T) {
	cases := []struct {
		name string
		stop any
		want []string
	}{
		{name: "string", stop: "END", want: []string{"END"}},
		{name: "array", stop: []any{"END", "STOP"}, want: []string{"END", "STOP"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claudeReq, err := openAIChatToClaudeRequest(map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
				"stop":     tc.stop,
			}, "claude-sonnet-4.5", 0)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			got, _ := claudeReq["stop_sequences"].([]any)
			if len(got) != len(tc.want) {
				t.Fatalf("stop_sequences = %#v, want %#v", claudeReq["stop_sequences"], tc.want)
			}
			for i, want := range tc.want {
				if got[i] != want {
					t.Fatalf("stop_sequences = %#v, want %#v", got, tc.want)
				}
			}
		})
	}
	// 没有 stop 时不凭空产生字段。
	plain, _ := openAIChatToClaudeRequest(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, "claude-sonnet-4.5", 0)
	if _, exists := plain["stop_sequences"]; exists {
		t.Fatalf("stop_sequences invented: %#v", plain["stop_sequences"])
	}
}
