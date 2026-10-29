package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// 管理员首页第一次拉 /__host-metrics 时懒建采样器。并发首访曾经各建一个：
// 被覆盖的那个采样器的协程没人再读，等于泄漏。sync.Once 保证只建一个。
func TestHostMetricsSamplerIsCreatedOnce(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	const callers = 16
	var wg sync.WaitGroup
	codes := make([]int, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			request := httptest.NewRequest(http.MethodGet, "/__host-metrics", nil)
			request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, adminIdentity()))
			recorder := httptest.NewRecorder()
			server.handleHostMetrics(recorder, request)
			codes[index] = recorder.Code
		}(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("caller %d = %d, want 200", i, code)
		}
	}
	if server.hostMetrics == nil {
		t.Fatalf("the sampler was never created")
	}
}

// The sampler must run for the life of the process, not from the first console
// visit: before StartHostMetrics existed, a restart left the curve blank until
// someone opened the page, and SQLite had nothing to restore because nothing
// had been sampled at all.
func TestStartHostMetricsSamplesWithoutARequest(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), nil)
	server.StartHostMetrics()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(server.hostSampler().History()) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no sample was taken without a console request")
}
