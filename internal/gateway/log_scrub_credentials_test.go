package gateway

import (
	"strings"
	"testing"
)

// 请求日志、应用日志、UI 诊断日志都会把上游错误原文、工具结果、URL 落库或展示。
// 带前缀的密钥（sk-…、ghp_…）有专门规则，但常见凭据里还有一批「没有前缀」的：
// Authorization: Bearer 后面的一串不透明 token、URL 查询串里的 ?api_key=…、
// URL 里的 user:pass@、cloudflared service install 的 tunnel token。这些原来原样
// 落进 request_logs 和 /__app/logs，管理员在控制台就能看到。
var credentialShapeSamples = []string{
	"https://api.example.com/v1/chat?api_key=3f9a1c2b4d5e6f708192a3b4&x=1",
	"https://example.com/v1/messages?token=abcdef0123456789abcdef",
	"connect to https://admin:hunter2@internal.example.com/v1 failed",
	"Authorization: Bearer 0123456789abcdef0123456789abcdef",
	"cloudflared service install eyJhIjoiYWNjb3VudC1pZCIsInQiOiJ0dW5uZWwtdG9rZW4ifQ==",
	"sk-01234567-89ab-cdef-0123-456789abcdef", // Zhipu / Qwen 的 UUID 形
}

// 留下的可读部分也别切太多：URL 的 scheme/主机、查询参数名、Bearer 关键字都还在，
// 否则人就不知道是哪个上游报的错。
var credentialShapeSurvivors = []string{
	"api.example.com",
	"@internal.example.com",
	"Bearer",
	"api_key=",
}

func assertScrubbed(t *testing.T, label, text string) {
	t.Helper()
	for _, sample := range credentialShapeSamples {
		secret := sample
		// 形状里可读的壳（scheme、查询名）不算秘密，只比对真正的凭据部分。
		for _, prefix := range []string{
			"https://api.example.com/v1/chat?api_key=",
			"https://example.com/v1/messages?token=",
			"https://admin:",
			"Authorization: Bearer ",
			"cloudflared service install ",
		} {
			secret = strings.TrimPrefix(secret, prefix)
		}
		if secret == "" {
			continue
		}
		if strings.Contains(text, secret) {
			t.Fatalf("%s: credential %q survived the scrub: %q", label, secret, text)
		}
	}
}

func TestRequestLogScrubsCredentialShapes(t *testing.T) {
	for _, sample := range credentialShapeSamples {
		// 作为错误描述（纯文本）
		assertScrubbed(t, "error description", scrubSensitiveMessage("upstream said: "+sample+" (retry)"))
		// 作为 JSON 字符串值（错误回包、工具结果）
		body := []byte(`{"error":{"message":"failed calling ` + sample + `"}}`)
		assertScrubbed(t, "json body", string(scrubSensitiveRequestBody(body)))
		// 作为 SSE data 行（流式错误事件）
		sse := "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"" + sample + "\"}}\n\n"
		assertScrubbed(t, "sse body", string(scrubSensitiveRequestBody([]byte(sse))))
	}
	for _, survivor := range credentialShapeSurvivors {
		scrubbed := string(scrubSensitiveRequestBody([]byte(`{"m":"` + credentialShapeSamples[0] + ` ` + credentialShapeSamples[3] + `"}`)))
		if !strings.Contains(scrubbed+" "+scrubSensitiveMessage(credentialShapeSamples[2]), survivor) {
			t.Fatalf("the scrub also removed %q, leaving nothing to read: %q", survivor, scrubbed)
		}
	}
}

// UI 诊断日志与上面两条用同一套判定。
func TestUILogScrubsCredentialShapes(t *testing.T) {
	for _, sample := range credentialShapeSamples {
		got := uiLogSanitize("connect error: " + sample)
		assertScrubbed(t, "ui log", got)
	}
}
