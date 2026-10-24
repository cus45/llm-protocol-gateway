package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
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

// 端到端：经真实落库路径写入的失败请求，响应正文里的 key 必须已被抹掉。
func TestRequestLogScrubsResponseCredentials(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	entry := recordAndFetch(t, server, 502, `{"model":"m"}`,
		`{"error":{"message":"Incorrect API key provided: sk-proj-abcdefghijklmnop","token":"tok-xyz","detail":"{\"secretKey\":\"abc123\",\"jwt\":\"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl\"}"}}`)
	for _, leaked := range []string{"abcdefghijklmnop", "tok-xyz", "abc123", "c2lnbmF0dXJl"} {
		if strings.Contains(entry.ResponseBody, leaked) {
			t.Fatalf("stored response leaks %s: %q", leaked, entry.ResponseBody)
		}
	}
	if !strings.Contains(entry.ResponseBody, "Incorrect API key") {
		t.Fatalf("scrubbing ate the error message: %q", entry.ResponseBody)
	}
}

// ErrorDescription 是落库的错误摘要，管理界面直接展示：上游回显的 key 也不能原样进去。
func TestRequestLogScrubsErrorDescription(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	entry := recordAndFetch(t, server, 401, `{"model":"m"}`,
		`{"error":{"message":"Incorrect API key provided: sk-abcdefghijklmnopqrstuvwx","type":"invalid_request_error"}}`)
	if strings.Contains(entry.ErrorDescription, "sk-abcdefghijklmnopqrstuvwx") {
		t.Fatalf("ErrorDescription leaks the key: %q", entry.ErrorDescription)
	}
	if !strings.Contains(entry.ErrorDescription, "Incorrect API key") || !strings.Contains(entry.ErrorDescription, "[redacted]") {
		t.Fatalf("ErrorDescription lost its shape: %q", entry.ErrorDescription)
	}
	// 错在正文不是 JSON 时走 summarizeUpstreamHTTPError 的兜底摘要，同样要抹。
	plain := recordAndFetch(t, server, 500, `{"model":"m"}`, "upstream said: bad key ghp_abcdefghijklmnopqrstuvwxyz0123")
	if strings.Contains(plain.ErrorDescription, "ghp_abcdefghijklmnopqrstuvwxyz0123") {
		t.Fatalf("fallback ErrorDescription leaks the token: %q", plain.ErrorDescription)
	}
}
