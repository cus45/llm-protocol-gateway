package gateway

import (
	"strings"
	"testing"
)

// 一张 1 MiB 的截图按 base64 约 140 万字符，估算器按 4 字符 1 token 会把它算成
// 35 万 token：一个正常额度的 Key 会因此拒绝每一个带图的请求。图片不计入提示，
// 只有文本计。
func TestQuotaPromptEstimateIgnoresMediaPayloads(t *testing.T) {
	image := strings.Repeat("A", 1<<20)
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"what is this?"},{"inlineData":{"mimeType":"image/png","data":"` + image + `"}}]}]}`)
	textOnly := []byte(`{"contents":[{"role":"user","parts":[{"text":"what is this?"}]}]}`)

	withImage := quotaPromptEstimate(body)
	without := quotaPromptEstimate(textOnly)
	if withImage > without*3 {
		t.Fatalf("a 1 MiB inline image was counted as prompt text: estimate %d tokens vs %d without it", withImage, without)
	}
	// 文本本身照旧计入。
	if without <= 0 {
		t.Fatalf("the text part stopped counting: %d", without)
	}

	// OpenAI 风格的 data: URL 图片同样不计。
	dataURL := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + image + `"}}]}]}`)
	if got := quotaPromptEstimate(dataURL); got > without*3 {
		t.Fatalf("a data: URL image was counted as prompt text: %d", got)
	}

	// 没有媒体时估算与原来一致（不被重编码打乱）。
	plain := []byte(`{"messages":[{"role":"user","content":"hello there"}]}`)
	if got, want := quotaPromptEstimate(plain), int64(estimateRTKTokens(string(plain))); got != want {
		t.Fatalf("a body without media changed estimate: %d vs %d", got, want)
	}
}
