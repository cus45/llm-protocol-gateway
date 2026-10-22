package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// forwarded 头只有来自本机/内网代理（Cloudflare tunnel、本机反代）时才可信：
// 公网直连的客户端随手带上 X-Forwarded-For / X-Real-IP / CF-Connecting-IP 就能
// 换一个"来源 IP"，登录限流、自注册配额、审计都按它记账，等于可以任意绕过。
func TestRequestClientIPIgnoresForwardedHeadersFromPublicPeers(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       string
	}{
		{
			name:       "public peer spoofs every header",
			remoteAddr: "203.0.113.9:4444",
			headers: map[string]string{
				"CF-Connecting-IP": "10.9.9.9",
				"X-Forwarded-For":  "198.51.100.7",
				"X-Real-IP":        "192.0.2.5",
			},
			want: "203.0.113.9",
		},
		{
			name:       "loopback proxy trusts CF-Connecting-IP",
			remoteAddr: "127.0.0.1:5555",
			headers:    map[string]string{"CF-Connecting-IP": "198.51.100.7"},
			want:       "198.51.100.7",
		},
		{
			name:       "private proxy trusts first X-Forwarded-For hop",
			remoteAddr: "10.0.0.4:5555",
			headers:    map[string]string{"X-Forwarded-For": " 198.51.100.8 , 10.0.0.4"},
			want:       "198.51.100.8",
		},
		{
			name:       "trusted proxy with garbage header falls back to peer",
			remoteAddr: "127.0.0.1:5555",
			headers:    map[string]string{"X-Real-IP": "not-an-ip"},
			want:       "127.0.0.1",
		},
		{
			name:       "IPv6 loopback proxy",
			remoteAddr: "[::1]:5555",
			headers:    map[string]string{"X-Real-IP": "198.51.100.9"},
			want:       "198.51.100.9",
		},
		{
			name:       "no headers uses peer",
			remoteAddr: "198.51.100.20:1234",
			want:       "198.51.100.20",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			req.RemoteAddr = tc.remoteAddr
			for key, value := range tc.headers {
				req.Header.Set(key, value)
			}
			if got := requestClientIP(req); got != tc.want {
				t.Fatalf("requestClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
