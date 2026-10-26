package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// Gemini 客户端与网关之间失真的几处：Gemini 是一个完整的前端协议，从 Gemini 进来的
// 请求、发回 Gemini 的回复都必须保住 Gemini 自己的语义（思考、图片、缓存计数、
// 结束原因），否则 Gemini CLI / google-genai 用户看到的是一条没有思考、看不见图片、
// 把截断当正常结束的回答。

func geminiStreamChunks(t *testing.T, sse string) []map[string]any {
	t.Helper()
	var out []map[string]any
	recorder := httptest.NewRecorder()
	if _, err := streamOpenAIChatToGeminiEvents(recorder, strings.NewReader(sse), "gemini-x"); err != nil {
		t.Fatalf("stream: %v", err)
	}
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if line == "" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			t.Fatalf("chunk %q is not JSON: %v", line, err)
		}
		out = append(out, chunk)
	}
	return out
}

func firstCandidate(chunk map[string]any) map[string]any {
	candidates := asMapSlice(chunk["candidates"])
	if len(candidates) == 0 {
		return nil
	}
	candidate, _ := candidates[0].(map[string]any)
	return candidate
}

// Gemini 的流以「带 finishReason 的 candidate」收尾；一个只有 finish_reason、
// 没有正文的结束帧（max_tokens 截断、内容过滤）原来被当作空帧丢掉，客户端把截断
// 的半句话当成完整回答。
func TestGeminiStreamKeepsFinishReasonWithoutContent(t *testing.T) {
	for _, tc := range []struct{ finish, want string }{
		{"length", "MAX_TOKENS"},
		{"content_filter", "SAFETY"},
		{"tool_calls", "STOP"},
	} {
		chunks := geminiStreamChunks(t, `data: {"choices":[{"delta":{"content":"partial"}}]}
data: {"choices":[{"delta":{},"finish_reason":"`+tc.finish+`"}]}
data: [DONE]
`)
		last := firstCandidate(chunks[len(chunks)-1])
		if last == nil || last["finishReason"] != tc.want {
			t.Fatalf("finish_reason=%q: the stream ended without finishReason %s (last chunk %v)", tc.finish, tc.want, chunks[len(chunks)-1])
		}
	}
}

// Chat 上游的 reasoning_content 在 Gemini 里是 thought 部件：丢掉它等于把整段
// 思考（以及 Gemini CLI 的「思考」展示）抹掉。
func TestGeminiKeepsThinkingAsThoughtParts(t *testing.T) {
	chunks := geminiStreamChunks(t, `data: {"choices":[{"delta":{"reasoning_content":"Let me think. "}}]}
data: {"choices":[{"delta":{"reasoning_content":"Step 2. "}}]}
data: {"choices":[{"delta":{"content":"Answer"},"finish_reason":"stop"}]}
data: [DONE]
`)
	var thought strings.Builder
	var answer strings.Builder
	for _, chunk := range chunks {
		candidate := firstCandidate(chunk)
		if candidate == nil {
			continue
		}
		content, _ := candidate["content"].(map[string]any)
		for _, raw := range asMapSlice(content["parts"]) {
			part, _ := raw.(map[string]any)
			if thoughtFlag, _ := part["thought"].(bool); thoughtFlag {
				thought.WriteString(stringValue(part["text"]))
				continue
			}
			answer.WriteString(stringValue(part["text"]))
		}
	}
	if thought.String() != "Let me think. Step 2. " {
		t.Fatalf("thinking reached the Gemini client as %q, want it in thought parts", thought.String())
	}
	if answer.String() != "Answer" {
		t.Fatalf("answer = %q", answer.String())
	}

	// 非流式同样要有 thought 部件。
	body, err := openAIChatToGeminiResponse(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "Answer", "reasoning_content": "Because."},
			"finish_reason": "stop",
		}},
	}, "gemini-x")
	if err != nil {
		t.Fatal(err)
	}
	candidate := firstCandidate(body)
	content, _ := candidate["content"].(map[string]any)
	foundThought := false
	for _, raw := range asMapSlice(content["parts"]) {
		part, _ := raw.(map[string]any)
		if flag, _ := part["thought"].(bool); flag && stringValue(part["text"]) == "Because." {
			foundThought = true
		}
	}
	if !foundThought {
		t.Fatalf("non-stream body lost the thinking: %v", content["parts"])
	}
}

// usageMetadata 要有 cachedContentTokenCount：Gemini 客户端据此算缓存命中。
func TestGeminiUsageReportsCachedTokens(t *testing.T) {
	body, err := openAIChatToGeminiResponse(geminiUsageChatBody(), "gemini-x")
	if err != nil {
		t.Fatal(err)
	}
	usage, _ := body["usageMetadata"].(map[string]any)
	if got := int64FromAny(usage["cachedContentTokenCount"]); got != 800 {
		t.Fatalf("cachedContentTokenCount = %v, want 800 (%v)", usage["cachedContentTokenCount"], usage)
	}
	chunks := geminiStreamChunks(t, `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":800}}}
data: [DONE]
`)
	usage, _ = chunks[len(chunks)-1]["usageMetadata"].(map[string]any)
	if got := int64FromAny(usage["cachedContentTokenCount"]); got != 800 {
		t.Fatalf("stream cachedContentTokenCount = %v, want 800 (%v)", usage["cachedContentTokenCount"], usage)
	}
}

func geminiUsageChatBody() map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": "ok"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1000, "completion_tokens": 50,
			"prompt_tokens_details": map[string]any{"cached_tokens": 800}},
	}
}

// Gemini 请求里的 inlineData（截图）必须变成 Chat 上游认识的 image_url，否则模型
// 看不到图，只看到一句「[inline data: image/png]」，然后对着它编答案。
func TestGeminiInlineImageReachesChatUpstream(t *testing.T) {
	req, err := geminiToOpenAIChatRequest(map[string]any{
		"contents": []any{map[string]any{
			"role": "user",
			"parts": []any{
				map[string]any{"text": "what is this?"},
				map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "iVBORw0KGgo="}},
			},
		}},
	}, "gpt-x")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(req)
	text := string(encoded)
	if !strings.Contains(text, `"image_url"`) || !strings.Contains(text, "data:image/png;base64,iVBORw0KGgo=") {
		t.Fatalf("the inline image never reached the Chat upstream: %s", text)
	}
}

// 一个 user 轮里既有图片又有 functionCall（Gemini 的多模态工具结果）时，图片不能丢；
// 非图片的 inlineData（PDF、音频）不该被塞进 image_url，留给模型一句说明。
func TestGeminiInlineMediaWithToolCallsAndNonImages(t *testing.T) {
	req, err := geminiToOpenAIChatRequest(map[string]any{
		"contents": []any{
			map[string]any{
				"role": "user",
				"parts": []any{
					map[string]any{"functionCall": map[string]any{"name": "look", "args": map[string]any{}}},
					map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "iVBORw0KGgo="}},
				},
			},
			map[string]any{
				"role": "user",
				"parts": []any{
					map[string]any{"inlineData": map[string]any{"mimeType": "application/pdf", "data": "JVBERi0xLjQ="}},
				},
			},
		},
	}, "gpt-x")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(req)
	text := string(encoded)
	if !strings.Contains(text, "data:image/png;base64,iVBORw0KGgo=") {
		t.Fatalf("the image was dropped from a turn that also carries a tool call: %s", text)
	}
	if strings.Contains(text, `"url":"data:application/pdf`) {
		t.Fatalf("a PDF was handed to the upstream as an image_url: %s", text)
	}
	if !strings.Contains(text, "application/pdf") {
		t.Fatalf("the non-image attachment lost its description: %s", text)
	}
}
