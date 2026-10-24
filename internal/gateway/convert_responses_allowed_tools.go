package gateway

import "strings"

// Responses 允许 tool_choice={"type":"allowed_tools","mode":"auto|required",
// "tools":[…]}：模型只能从中挑。Claude 的 tool_choice.allowed_tools 是同构的，
// 但一些兼容上游不认，所以同时把 tools 列表收窄过去。

// responsesAllowedToolsChoice 解析 Responses 的 allowed_tools 选择，返回
// Claude allowed_tools.tools 的内容（工具名 → 可选 input_schema）与模式。
// 非该类型返回 ok=false。
func responsesAllowedToolsChoice(choice any) (approved map[string]any, mode string, ok bool) {
	typed, isMap := choice.(map[string]any)
	if !isMap || stringValue(typed["type"]) != "allowed_tools" {
		return nil, "", false
	}
	mode = strings.TrimSpace(strings.ToLower(stringValue(typed["mode"])))
	if mode != "required" {
		mode = "auto"
	}
	approved = map[string]any{}
	items, _ := typed["tools"].([]any)
	for _, item := range items {
		tool, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		name := stringValue(tool["name"])
		switch toolType := stringValue(tool["type"]); toolType {
		case "function", "custom":
			if namespace := stringValue(tool["namespace"]); namespace != "" && name != "" {
				name = flattenNamespaceToolName(namespace, name)
			}
		case "":
		default:
			// 内置工具（web_search 等）不在 Claude 的可选列表里，跳过。
			continue
		}
		if name == "" {
			continue
		}
		var params map[string]any
		if custom, isCustom := tool["custom"].(map[string]any); isCustom {
			params, _ = custom["parameters"].(map[string]any)
		}
		if params == nil {
			params, _ = tool["parameters"].(map[string]any)
		}
		entry := map[string]any{}
		if params != nil {
			entry["input_schema"] = params
		}
		// The approved list is read by compatible upstreams as Responses'
		// shape ({"type":"function"|"custom","name":…[,"namespace":…]}), and by
		// Claude as {name: {input_schema}}. Keep both halves: an entry without
		// its type/name is a tool no one can identify.
		switch toolType := stringValue(tool["type"]); toolType {
		case "function", "custom":
			entry["type"] = toolType
			entry["name"] = stringValue(tool["name"])
			if namespace := stringValue(tool["namespace"]); namespace != "" {
				entry["namespace"] = namespace
			}
		}
		approved[name] = entry
	}
	if len(approved) == 0 {
		return nil, "", false
	}
	return approved, mode, true
}

// narrowClaudeTools 只留下白名单里的工具；Claude 的列表是有序的，顺序不变。
func narrowClaudeTools(tools []any, keep map[string]any) []any {
	out := make([]any, 0, len(keep))
	for _, item := range tools {
		tool, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		name := stringValue(tool["name"])
		if _, ok := keep[name]; !ok {
			continue
		}
		out = append(out, item)
	}
	return out
}
