package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// 采样参数整流器：当上游是 Claude 系模型、且它明确拒绝 temperature / top_p /
// top_k（"`temperature` is deprecated for this model"）时，去掉这些字段重发一次。
//
// 新一代 Claude（Opus 4.7+、Sonnet 5+、Fable 5 等）在任意取值下都不接受采样
// 参数，Sonnet 5 只接受默认值，Haiku 5.5 只接受 temperature=1 与 top_p=0.99 且
// 不能同时给。客户端（Claude Code 的 auto 模式分类器、各类 SDK）仍习惯带
// temperature=0，直连会直接 400。这些值对模型没有意义（留空即按默认采样），
// 因此丢弃后重试对结果零损失。

// samplingRefusalMatcher 识别上游拒绝采样参数的措辞。两种来源都要覆盖：
//   - Anthropic 官方："`temperature` is deprecated for this model."
//     "`top_p` is not supported for this model." / "temperature: unsupported parameter"
//   - 第三方中转把整句塞进 message 字符串时的类似措辞
var samplingRefusalPhrases = []string{
	"deprecated for this model",
	"not supported for this model",
	"unsupported parameter",
	"does not support temperature",
	"does not support top_p",
	"does not support top_k",
}

// samplingRefusalFields 是这些字段的名字，用于确认错误确实指向采样参数。
var samplingRefusalFields = []string{"temperature", "top_p", "top_k"}

// shouldRectifySamplingRefusal 判断上游响应体是否是"拒绝采样参数"这一类错误。
func shouldRectifySamplingRefusal(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	namesField := false
	for _, field := range samplingRefusalFields {
		if strings.Contains(lower, field) {
			namesField = true
			break
		}
	}
	if !namesField {
		return false
	}
	for _, phrase := range samplingRefusalPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// stripSamplingFields 从请求体中删除 temperature / top_p / top_k，返回改写后的
// 请求体与是否真的删掉了字段（没有字段时调用方不必重试）。
func stripSamplingFields(body []byte) ([]byte, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return body, false
	}
	changed := false
	for _, key := range samplingRefusalFields {
		if _, exists := payload[key]; exists {
			delete(payload, key)
			changed = true
		}
	}
	if !changed {
		return body, false
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return rewritten, true
}

// maybeRectifySamplingRefusal 在 Claude 上游 400 且命中采样参数拒绝时，去掉
// temperature/top_p/top_k 重发一次。返回重试响应与是否真的重试过；未重试时
// 原响应体已恢复，调用方可原样转发。
func (s *Server) maybeRectifySamplingRefusal(r *http.Request, provider domain.Provider, body []byte, response *http.Response, resend func([]byte) (*http.Response, error)) (*http.Response, bool) {
	if response == nil || response.StatusCode != http.StatusBadRequest {
		return response, false
	}
	errBody, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	response.Body = io.NopCloser(strings.NewReader(string(errBody)))
	if readErr != nil || !shouldRectifySamplingRefusal(errBody) {
		return response, false
	}
	stripped, changed := stripSamplingFields(body)
	if !changed {
		return response, false
	}
	s.logs.AddApp("info", "sampling rectifier retrying", "provider="+provider.ID)
	retryResp, retryErr := resend(stripped)
	if retryErr != nil || retryResp == nil {
		detail := "no response"
		if retryErr != nil {
			detail = retryErr.Error()
		}
		s.logs.AddApp("warn", "sampling rectifier retry failed", detail)
		return response, false
	}
	return retryResp, true
}
