package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// 出图是最慢的请求：没有上限时，卡住的上游会一直占着请求和它的账号槽位，
// 直到客户端放弃。出图必须带上一个有限的截止时间；嵌入与音频是快往返，
// 保持调用方自己的 context。
//
// 这里断言的是"网关发出的请求带没有带超时"（由 RoundTripper 观察），而不是
// 上游 handler 看到的 ctx：Go 的 httptest 服务端不会继承客户端的 deadline。
func TestImageUpstreamRequestsAreBounded(t *testing.T) {
	seen := make(chan string, 4)
	previous := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if deadline, ok := r.Context().Deadline(); ok {
			seen <- deadline.String()
		} else {
			seen <- "none"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
			Request:    r,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })

	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	started := time.Now()
	if _, _, _, err := server.proxyOpenAIImagesUpstream(httptest.NewRecorder(), context.Background(), "https://vendor.example/v1/images/generations", "sk-x", "application/json", []byte(`{}`)); err != nil {
		t.Fatalf("images request: %v", err)
	}
	got := <-seen
	if got == "none" {
		t.Fatalf("an image render reached the upstream without any deadline")
	}
	deadline, err := time.Parse(time.RFC3339Nano, got)
	if err == nil {
		if window := deadline.Sub(started); window < time.Minute || window > imageGenerationTimeout+time.Minute {
			t.Fatalf("image deadline is %v away, want about %v", window, imageGenerationTimeout)
		}
	}

	// Embeddings are not renders: no gateway-imposed deadline.
	if _, _, _, err := server.proxyOpenAIImagesUpstream(httptest.NewRecorder(), context.Background(), "https://vendor.example/v1/embeddings", "sk-x", "application/json", []byte(`{}`)); err != nil {
		t.Fatalf("embeddings request: %v", err)
	}
	if got := <-seen; got != "none" {
		t.Fatalf("embeddings must keep the caller's context, got a deadline")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
