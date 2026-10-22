package gateway

// structuredOutputFormat 在 Chat 的 response_format、Responses 的 text.format
// 与 Claude 的 output_config.format 之间搬运结构化输出声明。
//
// 三家的形状只差一个 key：{"type":"json_schema","json_schema":{"name","schema",
// "strict"}} ⇄ {"type":"json_schema","name","schema"}。此前三个方向的转换都会
// 丢掉它：客户端以为拿到严格 JSON，实际拿到一段自由文本，解析直接失败。只做
// 自己认得的形状，其它一律不猜、原样不带（不可表达时由调用方决定是否删除）。

// chatResponseFormatToOutputConfigFormat converts a Chat response_format
// (text / json_object / json_schema) into Claude output_config.format.
func chatResponseFormatToOutputConfigFormat(responseFormat any) map[string]any {
	format, ok := responseFormat.(map[string]any)
	if !ok {
		return nil
	}
	switch stringValue(format["type"]) {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		if schema, ok := format["json_schema"].(map[string]any); ok {
			if converted := claudeStructuredFormat(schema); converted != nil {
				return converted
			}
		}
		// 个别客户端写成 {"type":"json_schema","schema":{…}}。
		return claudeStructuredFormat(format)
	}
	return nil
}

// claudeStructuredFormat builds {"type":"json_schema",name,schema} from a
// schema carrier that has name/schema at its top level.
func claudeStructuredFormat(carrier map[string]any) map[string]any {
	schema, ok := carrier["schema"].(map[string]any)
	if !ok || len(schema) == 0 {
		return nil
	}
	format := map[string]any{"type": "json_schema", "schema": schema}
	if name := stringValue(carrier["name"]); name != "" {
		format["name"] = name
	}
	return format
}

// outputConfigFormatToChatResponseFormat converts Claude output_config.format
// into a Chat response_format.
func outputConfigFormatToChatResponseFormat(outputConfig any) map[string]any {
	config, ok := outputConfig.(map[string]any)
	if !ok {
		return nil
	}
	format, ok := config["format"].(map[string]any)
	if !ok {
		return nil
	}
	switch stringValue(format["type"]) {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		schema, ok := format["schema"].(map[string]any)
		if !ok || len(schema) == 0 {
			return nil
		}
		inner := map[string]any{"schema": schema}
		if name := stringValue(format["name"]); name != "" {
			inner["name"] = name
		}
		return map[string]any{"type": "json_schema", "json_schema": inner}
	}
	return nil
}

// setClaudeOutputConfigFormat puts format into claudeReq's output_config,
// keeping any effort already there.
func setClaudeOutputConfigFormat(claudeReq map[string]any, format map[string]any) {
	if format == nil {
		return
	}
	config, _ := claudeReq["output_config"].(map[string]any)
	if config == nil {
		config = map[string]any{}
	}
	// effort 可用的模型才发 output_config；纯 format 不会改变思考行为。
	config["format"] = format
	claudeReq["output_config"] = config
}
