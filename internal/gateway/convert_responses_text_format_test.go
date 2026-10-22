package gateway

import (
	"reflect"
	"testing"
)

// TestChatToResponsesTextFormat: Chat 的 response_format 必须落到 Responses 的
// text.format（此前被丢弃，客户端按结构化输出解析会失败）。
func TestChatToResponsesTextFormat(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}}
	chatReq := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "weather?"}},
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{
			"name": "weather", "schema": schema, "strict": true,
		}},
	}
	responsesReq, err := openAIChatToResponsesRequest(chatReq, "gpt-5.5")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	text, _ := responsesReq["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	want := map[string]any{"type": "json_schema", "name": "weather", "schema": schema, "strict": true}
	if !reflect.DeepEqual(format, want) {
		t.Fatalf("text.format = %#v, want %#v", format, want)
	}

	// json_object 直通。
	object, _ := openAIChatToResponsesRequest(map[string]any{
		"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
		"response_format": map[string]any{"type": "json_object"},
	}, "gpt-5.5")
	text, _ = object["text"].(map[string]any)
	format, _ = text["format"].(map[string]any)
	if format["type"] != "json_object" {
		t.Fatalf("json_object not carried: %#v", object["text"])
	}

	// 没有 response_format 时不制造 text。
	plain, _ := openAIChatToResponsesRequest(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, "gpt-5.5")
	if _, exists := plain["text"]; exists {
		t.Fatalf("text invented: %#v", plain["text"])
	}
}

// TestResponsesToChatTextFormat: 反向转换（Responses text.format → Chat
// response_format）已由 applyResponsesTextFormatToChat 覆盖，这里钉住整条
// 请求转换路径。
func TestResponsesToChatTextFormat(t *testing.T) {
	responsesReq := map[string]any{
		"input": "weather?",
		"text": map[string]any{"format": map[string]any{
			"type": "json_schema", "name": "weather", "strict": true,
			"schema": map[string]any{"type": "object"},
		}},
	}
	chatReq, _, err := responsesToOpenAIChatRequest(responsesReq, "glm-5.3")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	format, _ := chatReq["response_format"].(map[string]any)
	inner, _ := format["json_schema"].(map[string]any)
	if format["type"] != "json_schema" || inner["name"] != "weather" || inner["strict"] != true {
		t.Fatalf("response_format = %#v", chatReq["response_format"])
	}
}
