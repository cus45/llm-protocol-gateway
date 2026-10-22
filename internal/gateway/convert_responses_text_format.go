package gateway

// Responses 的 text.format 与 Chat 的 response_format 搬运。
//
// 两者只差一层 json_schema 嵌套：Responses 是 {"type":"json_schema","name",
// "schema","strict"}，Chat 是 {"type":"json_schema","json_schema":{…}}。此前
// Chat→Responses 方向不带这个字段，结构化输出承诺被静默丢弃。
// 反向（Responses→Chat）见 convert_responses_fields.go 的
// applyResponsesTextFormatToChat。

// chatResponseFormatToResponsesTextFormat is the reverse: a Responses text
// object carrying Chat response_format's format; nil when none.
func chatResponseFormatToResponsesTextFormat(responseFormat any) map[string]any {
	format, ok := responseFormat.(map[string]any)
	if !ok {
		return nil
	}
	switch stringValue(format["type"]) {
	case "json_object":
		return map[string]any{"format": map[string]any{"type": "json_object"}}
	case "json_schema":
		carrier := format
		if inner, ok := format["json_schema"].(map[string]any); ok {
			carrier = inner
		}
		schema, ok := carrier["schema"].(map[string]any)
		if !ok || len(schema) == 0 {
			return nil
		}
		inner := map[string]any{"type": "json_schema", "schema": schema}
		if name := stringValue(carrier["name"]); name != "" {
			inner["name"] = name
		}
		if strict, ok := carrier["strict"].(bool); ok {
			inner["strict"] = strict
		}
		return map[string]any{"format": inner}
	}
	return nil
}
