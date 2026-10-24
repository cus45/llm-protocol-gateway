package gateway

import "strings"

// Chat 与 Responses 之间只搬两边同义的字段：prompt_cache_key 让上游按对话复用
// KV 缓存，service_tier 决定 fast 档，safety_identifier 是上游做安全审计的终端
// 用户标识（Responses 客户端如 Codex 每轮都带，丢了上游就看不到说话的人）。
// 两边都没有对应字段的（metadata、store、include……）不搬，免得让不认识它们的
// 上游 400。

// openAIServiceTiers 是 Chat 与 Responses 共有的 service_tier 取值。
var openAIServiceTiers = map[string]struct{}{
	"auto":     {},
	"default":  {},
	"flex":     {},
	"priority": {},
}

// normalizedOpenAIServiceTier 归一小写并校验取值；None 表示不该搬运
// （空值或上游不认识的档位都按未指定处理，避免整轮 400）。
func normalizedOpenAIServiceTier(value any) (string, bool) {
	text := strings.ToLower(strings.TrimSpace(stringValue(value)))
	if _, ok := openAIServiceTiers[text]; !ok {
		return "", false
	}
	return text, true
}

// forwardChatConversationFields 把 Chat 的 prompt_cache_key / service_tier /
// safety_identifier 带到 Responses 请求里。
func forwardChatConversationFields(chatReq, responsesReq map[string]any) {
	if key := strings.TrimSpace(stringValue(chatReq["prompt_cache_key"])); key != "" {
		responsesReq["prompt_cache_key"] = key
	}
	if tier, ok := normalizedOpenAIServiceTier(chatReq["service_tier"]); ok {
		responsesReq["service_tier"] = tier
	}
	if id := strings.TrimSpace(stringValue(chatReq["safety_identifier"])); id != "" {
		responsesReq["safety_identifier"] = id
	}
}

// forwardResponsesConversationFields 把 Responses 的 prompt_cache_key /
// service_tier / safety_identifier 带到 Chat 请求里。
func forwardResponsesConversationFields(responsesReq, chatReq map[string]any) {
	if key := strings.TrimSpace(stringValue(responsesReq["prompt_cache_key"])); key != "" {
		chatReq["prompt_cache_key"] = key
	}
	if tier, ok := normalizedOpenAIServiceTier(responsesReq["service_tier"]); ok {
		chatReq["service_tier"] = tier
	}
	if id := strings.TrimSpace(stringValue(responsesReq["safety_identifier"])); id != "" {
		chatReq["safety_identifier"] = id
	}
}
