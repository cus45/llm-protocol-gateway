package gateway

import (
	"reflect"
	"testing"
)

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
			name: "json_object",
			in:   map[string]any{"type": "json_object"},
			want: map[string]any{"type": "json_object"},
		},
		{
			name: "json_schema nested",
			in: map[string]any{"type": "json_schema", "json_schema": map[string]any{
				"name": "weather", "schema": schema, "strict": true,
			}},
			want: map[string]any{"type": "json_schema", "name": "weather", "schema": schema},
		},
		{
			name: "json_schema flat",
			in:   map[string]any{"type": "json_schema", "name": "weather", "schema": schema},
			want: map[string]any{"type": "json_schema", "name": "weather", "schema": schema},
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
	if format["type"] != "json_schema" || format["name"] != "weather" {
		t.Fatalf("output_config.format = %#v", config["format"])
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
