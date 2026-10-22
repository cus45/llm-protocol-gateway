package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func TestReadGatewayRequestBodyCap(t *testing.T) {
	// 远大于上限的假正文由 io.Pipe 持续供给，不真的分配 256MB。
	pr, pw := io.Pipe()
	go func() {
		chunk := make([]byte, 1<<20)
		for i := 0; i < 300; i++ {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
		pw.Close()
	}()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", pr)
	if _, tooLarge, _ := readGatewayRequestBody(r); !tooLarge {
		t.Fatalf("oversized body must report tooLarge")
	}

	r = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"a":1}`))
	body, tooLarge, err := readGatewayRequestBody(r)
	if tooLarge || err != nil || string(body) != `{"a":1}` {
		t.Fatalf("small body: tooLarge=%v err=%v body=%s", tooLarge, err, body)
	}
}

// TestChatHandlerAnswers413: 客户端正文超过网关上限时，聊天端点回 413 而不是
// 一直缓冲到进程 OOM，也不是含糊的 400 invalid json。
func TestChatHandlerAnswers413(t *testing.T) {
	router := NewRouter(domain.GatewayState{})
	server := NewServer(router, monitor.NewStore())
	handler := server.Handler()

	pr, pw := io.Pipe()
	go func() {
		chunk := make([]byte, 1<<20)
		for i := 0; i < 300; i++ {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
		pw.Close()
	}()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", pr)
	// 明确声明长度：超大正文不该被当成"长度未知"。
	req.ContentLength = gatewayRequestBodyLimit + 1
	req.Host = "127.0.0.1:18093"
	req.RemoteAddr = "127.0.0.1:4321"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "exceeds the gateway limit") {
		t.Fatalf("413 body should explain the limit: %s", rec.Body.String())
	}
}
