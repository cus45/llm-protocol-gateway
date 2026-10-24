package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// 出图也花账号的额度，必须走 lane：并发上限为 1 时，同一账号同时只允许一张
// 在飞，第二张由网关直接 429，而不是一起打到上游。
func TestImageGenerationTakesTheAccountLane(t *testing.T) {
	var hits int32
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":"https://example.com/img.png"}]}`))
	}))
	t.Cleanup(upstream.Close)
	closed := false
	t.Cleanup(func() {
		if !closed {
			close(release)
		}
	})

	provider := imagesImageProvider("img-lane", false)
	provider.BaseURL = upstream.URL
	provider.MaxConcurrentRequests = 1
	provider.QueueWaitSeconds = 0
	server := NewServer(NewRouter(domain.GatewayState{
		Providers: []domain.Provider{provider},
		Routes:    []domain.Route{imagesRoute(provider.ID)},
	}), monitor.NewStore())

	route, err := server.router.RouteByID("img-route")
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if _, err := server.router.AddAPIKey(domain.APIKey{ID: "lane-key", Name: "lane-key", Key: "sk-gw-admin", RouteID: route.ID}); err != nil {
		t.Fatalf("add key: %v", err)
	}
	first := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"a cat"}`))
		status, _, _, _ := server.proxyOpenAIImages(recorder, request, provider, "gpt-image-2", []byte(`{"model":"gpt-image-2","prompt":"a cat"}`), route.OutputProtocol)
		first <- status
	}()
	// Wait until the first render is inside the upstream (holding the slot).
	for atomic.LoadInt32(&hits) == 0 {
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"a dog"}`))
	status, _, _, err := server.proxyOpenAIImages(recorder, request, provider, "gpt-image-2", []byte(`{"model":"gpt-image-2","prompt":"a dog"}`), route.OutputProtocol)
	if status != http.StatusTooManyRequests {
		t.Fatalf("second render = %d (%v), want 429 from the account's lane", status, err)
	}
	// Through the real handler the refusal must also reach the client as 429,
	// not as a 502 "upstream failed".
	handlerRecorder := httptest.NewRecorder()
	handlerRequest := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"a dog"}`))
	handlerRequest.Header.Set("Content-Type", "application/json")
	handlerRequest.Header.Set("Authorization", "Bearer sk-gw-admin")
	server.handleOpenAIImagesGenerations(handlerRecorder, handlerRequest)
	if handlerRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("the handler answered %d, want 429 for a saturated account", handlerRecorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got == "" {
		t.Fatalf("a saturated account must tell the client when to retry")
	}
	closed = true
	close(release)
	sent := <-first
	if sent != http.StatusOK {
		t.Fatalf("the first render should have succeeded, got %d", sent)
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want the second render never to reach it", hits)
	}
}
