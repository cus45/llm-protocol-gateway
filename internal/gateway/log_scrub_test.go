package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSensitiveKeyMatches(t *testing.T) {
	sensitive := []string{"api_key", "API_KEY", "apiKey", "x-api-key", "Authorization", "access_token",
		"refreshToken", "client_secret", "PASSWORD", "openai_api_key", "github_token", "db_password"}
	for _, key := range sensitive {
		if !sensitiveKeyMatches(key) {
			t.Fatalf("%q must be sensitive", key)
		}
	}
	for _, key := range []string{"model", "messages", "content", "max_tokens", "temperature", "token_count", "stop"} {
		if sensitiveKeyMatches(key) {
			t.Fatalf("%q must not be sensitive", key)
		}
	}
}

func TestScrubSensitiveRequestBody(t *testing.T) {
	body := []byte(`{
		"model":"claude-sonnet-4.5",
		"max_tokens":1024,
		"metadata":{"user_id":"u1","api_key":"sk-live-secret"},
		"messages":[{"role":"user","content":[{"type":"text","text":"use sk-ant-abcdefghijklmnopqrstuvwxyz please"}]}],
		"mcp_servers":[{"name":"github","env":{"GITHUB_TOKEN":"ghp_abcdefghijklmnopqrstuvwxyz0123","LOG_LEVEL":"debug"}}],
		"access_token":{"nested":"value"}
	}`)
	out := scrubSensitiveRequestBody(body)
	text := string(out)
	for _, leaked := range []string{"sk-live-secret", "sk-ant-abcdefghijklmnopqrstuvwxyz", "ghp_abcdefghijklmnopqrstuvwxyz0123", `"value"`} {
		if strings.Contains(text, leaked) {
			t.Fatalf("%q leaked: %s", leaked, text)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("not json: %v (%s)", err, out)
	}
	// 非敏感字段逐字保留。
	if payload["model"] != "claude-sonnet-4.5" || payload["max_tokens"] != float64(1024) {
		t.Fatalf("non-sensitive fields changed: %s", out)
	}
	metadata, _ := payload["metadata"].(map[string]any)
	if metadata["user_id"] != "u1" {
		t.Fatalf("metadata.user_id changed: %s", out)
	}
	servers, _ := payload["mcp_servers"].([]any)
	server, _ := servers[0].(map[string]any)
	env, _ := server["env"].(map[string]any)
	if env["LOG_LEVEL"] != "debug" {
		t.Fatalf("unsensitive env changed: %s", out)
	}
	if !strings.Contains(strings.ToLower(metadata["api_key"].(string)), "redacted") {
		t.Fatalf("api_key not redacted: %s", out)
	}
	if !strings.Contains(text, "use [redacted] please") {
		t.Fatalf("prompt secret not scrubbed: %s", text)
	}
}

func TestScrubSensitiveRequestBodyLeavesCleanBodiesAlone(t *testing.T) {
	clean := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	if got := scrubSensitiveRequestBody(clean); string(got) != string(clean) {
		t.Fatalf("clean body must stay byte-identical: %s", got)
	}
	plain := []byte("not json at all")
	if got := scrubSensitiveRequestBody(plain); string(got) != string(plain) {
		t.Fatalf("plain text must stay byte-identical: %s", got)
	}
	withKey := []byte("token sk-abcdefghijklmnop0123 in text")
	if got := string(scrubSensitiveRequestBody(withKey)); strings.Contains(got, "sk-abcdefghijklmnop0123") {
		t.Fatalf("text secret not scrubbed: %s", got)
	}
}

// 上游把凭据回显在响应里（错误文案、工具结果的嵌套 JSON、SSE 错误事件）时，
// 响应正文落库前同样要脱敏，否则管理界面会把 key 原样展示出去。
func TestScrubSensitiveResponseBody(t *testing.T) {
	cases := []struct{ name, body string }{
		{
			name: "nested json inside a tool result",
			body: `{"content":[{"type":"text","text":"{\"refreshToken\":\"abc\",\"secretKey\":\"def\",\"accessKey\":\"ghi\",\"headers\":{\"Cookie\":\"sid=1\"},\"jwt\":\"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl\"}"}]}`,
		},
		{
			name: "sse error event",
			body: "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"auth failed\",\"refresh_token\":\"abc\",\"detail\":\"{\\\"secretAccessKey\\\":\\\"def\\\",\\\"token\\\":\\\"ghi\\\"}\"}}\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(scrubSensitiveRequestBody([]byte(tc.body)))
			for _, leaked := range []string{"\"abc\"", "\"def\"", "\"ghi\"", "sid=1", "eyJhbGciOiJIUzI1NiJ9"} {
				if strings.Contains(out, leaked) {
					t.Fatalf("%s leaked: %s", leaked, out)
				}
			}
		})
	}
}
