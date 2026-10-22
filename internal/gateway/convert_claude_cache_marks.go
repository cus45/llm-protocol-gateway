package gateway

import "strings"

// Claude 只把带 cache_control 的位置当缓存断点，而别的协议的客户端从不写它：
// Chat 客户端最多在顶层给一个 cache_control（Codex 风格），Responses 客户端
// 什么都不给。直接转换过去等于每轮都全价重算整个前缀。

var claudeEphemeralMark = map[string]any{"type": "ephemeral"}

// claudeCacheMark 归一化客户端给的 cache_control：值原样保留（可能带 ttl），
// 非对象则按 ephemeral 处理。nil 表示没要求缓存。
func claudeCacheMark(value any) map[string]any {
	mark, ok := value.(map[string]any)
	if !ok || len(mark) == 0 {
		return nil
	}
	return mark
}

// setClaudeSystemCacheMark 把顶层 cache_control 落到 system 末尾的文本块上：
// 工具与系统提示每轮都一样，单独做成一个断点，和对话分开缓存。
func setClaudeSystemCacheMark(system any, mark map[string]any) (any, bool) {
	if mark == nil || system == nil {
		return system, false
	}
	blocks, ok := system.([]any)
	if !ok {
		text, isText := system.(string)
		if !isText || strings.TrimSpace(text) == "" {
			return system, false
		}
		return []any{map[string]any{"type": "text", "text": text, "cache_control": mark}}, true
	}
	for i := len(blocks) - 1; i >= 0; i-- {
		block, isBlock := blocks[i].(map[string]any)
		if !isBlock || block["type"] != "text" {
			continue
		}
		if _, exists := block["cache_control"]; exists {
			return system, false
		}
		block["cache_control"] = mark
		return system, true
	}
	return system, false
}

// markClaudeConversationTail 在最后一条消息的最后一个可缓存块上打点：下一轮
// 只会往后追加，这段前缀就能直接命中缓存。thinking 块不能打点。
func markClaudeConversationTail(claudeReq map[string]any) bool {
	switch messages := claudeReq["messages"].(type) {
	case []map[string]any:
		if len(messages) == 0 {
			return false
		}
		return markClaudeMessageTail(messages[len(messages)-1])
	case []any:
		if len(messages) == 0 {
			return false
		}
		last, ok := messages[len(messages)-1].(map[string]any)
		return ok && markClaudeMessageTail(last)
	}
	return false
}

func markClaudeMessageTail(last map[string]any) bool {
	switch content := last["content"].(type) {
	case string:
		if strings.TrimSpace(content) == "" {
			return false
		}
		last["content"] = []any{map[string]any{"type": "text", "text": content, "cache_control": claudeEphemeralMark}}
		return true
	case []any:
		for i := len(content) - 1; i >= 0; i-- {
			block, isBlock := content[i].(map[string]any)
			if !isBlock || !claudeCacheableBlock(block) {
				continue
			}
			if _, exists := block["cache_control"]; exists {
				return false
			}
			block["cache_control"] = claudeEphemeralMark
			return true
		}
	}
	return false
}

// claudeCacheableBlock 判断这个内容块能不能当缓存断点。
func claudeCacheableBlock(block map[string]any) bool {
	switch block["type"] {
	case "text":
		text, _ := block["text"].(string)
		return strings.TrimSpace(text) != ""
	case "image", "document":
		return true
	}
	return false
}

// applyClaudeCacheMarks 给从其他协议转来的 Messages 请求补上缓存断点：
// 顶层 cache_control 落到 system 末尾，已显式给断点的内容（整段转换里来的
// 块）保持原样，对话末尾再补一个，最多两处，在 Anthropic 的四个上限之内。
func applyClaudeCacheMarks(claudeReq map[string]any, topLevelCacheControl any) {
	if claudeReq == nil {
		return
	}
	if mark := claudeCacheMark(topLevelCacheControl); mark != nil {
		if system, ok := setClaudeSystemCacheMark(claudeReq["system"], mark); ok {
			claudeReq["system"] = system
		}
		markClaudeConversationTail(claudeReq)
		return
	}
	if !claudeRequestHasCacheMark(claudeReq) {
		markClaudeConversationTail(claudeReq)
	}
}

// claudeRequestHasCacheMark 判断请求里是否已经有任何断点（system、工具或消息）。
func claudeRequestHasCacheMark(claudeReq map[string]any) bool {
	if claudeContentHasCacheMark(claudeReq["system"]) {
		return true
	}
	if tools, ok := claudeReq["tools"].([]any); ok {
		for _, item := range tools {
			if block, isBlock := item.(map[string]any); isBlock {
				if _, exists := block["cache_control"]; exists {
					return true
				}
			}
		}
	}
	switch messages := claudeReq["messages"].(type) {
	case []map[string]any:
		for _, message := range messages {
			if claudeContentHasCacheMark(message["content"]) {
				return true
			}
		}
	case []any:
		for _, item := range messages {
			if message, ok := item.(map[string]any); ok && claudeContentHasCacheMark(message["content"]) {
				return true
			}
		}
	}
	return false
}

func claudeContentHasCacheMark(content any) bool {
	blocks, ok := content.([]any)
	if !ok {
		return false
	}
	return contentBlockArrayHasCacheControl(blocks)
}
