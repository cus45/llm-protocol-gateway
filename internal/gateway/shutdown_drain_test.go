package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"github.com/luca/llm-protocol-gateway/internal/store"
)

// 优雅退出时 http.Server.Shutdown 会一直等到在飞请求跑完，所以 Stop 必须先排空、
// 再落盘延迟写入（request_log_batcher），最后才关库。顺序反了，排空期间完成的
// 请求行就写进已经关掉的 batcher，直接丢失（gateway 层与 app 层的
// TestStopDrainsHTTPBeforeFlushingDeferredWrites 是端到端的那一半）。
//
// 这里逐步复现 Stop 的顺序：先有若干行攒在批里，排空期间再完成一个请求。
func TestShutdownFlushLandsRowsRecordedDuringDrain(t *testing.T) {
	db, openErr := store.Open(t.TempDir() + "/gateway.db")
	if openErr != nil {
		t.Fatalf("open store: %v", openErr)
	}
	logs := monitor.NewStore()
	server := NewServer(NewRouter(domain.GatewayState{
		Routes: []domain.Route{imagesRoute("img-a")},
		Endpoints: []domain.OutputEndpoint{{
			ID: "openai-chat-output", Name: "OpenAI Chat Output", Protocol: domain.ProtocolOpenAIChat,
		}},
	}), logs, db)
	record := func(path string) {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		server.recordRequestLogFromRequestTTFT(request, time.Now(), domain.APIKey{ID: "k1", Name: "k1"}, true,
			"r1", "p1", "m1", "images", "openai", path, http.StatusOK,
			TokenUsage{InputTokens: 3, OutputTokens: 4}, 0, nil, []byte("{}"))
	}
	// One request already finished and is sitting in the batch when shutdown starts.
	record("/v1/images/generations")

	release := make(chan struct{})
	arrived := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		record("/slow")
		w.WriteHeader(http.StatusOK)
	})
	listener := httptest.NewUnstartedServer(handler)
	listener.Start()
	httpServer := listener.Config
	t.Cleanup(func() {
		go listener.CloseClientConnections()
		listener.Close()
	})
	go func() {
		response, getErr := http.Get(listener.URL + "/slow")
		if getErr == nil {
			_ = response.Body.Close()
		}
	}()
	<-arrived // the in-flight request is parked inside the handler

	// Same order as app.Runtime.Stop: drain, then flush (inside the shutdown
	// budget), then close the DB.
	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		_ = httpServer.Shutdown(context.Background())
		_ = server.FlushDeferredWrites(context.Background())
	}()
	time.Sleep(100 * time.Millisecond) // let Shutdown start waiting on the handler
	close(release)
	<-stopDone

	rows, listErr := db.ListRequestLogs(1000)
	if listErr != nil {
		t.Fatalf("list request logs: %v", listErr)
	}
	if len(rows) != 2 {
		t.Fatalf("persisted request rows = %d, want 2: a row that finished during the drain was dropped", len(rows))
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close store: %v", closeErr)
	}
}

// FlushDeferredWrites 关掉 batcher 之后，排空尾巴上还有请求会记账：那时没有
// 后台 goroutine 再 flush，Append 必须当场写库，而不是把行留在内存里随进程消失。
func TestRequestLogAppendAfterFlushWritesThrough(t *testing.T) {
	store := &fakeRequestLogStore{}
	batcher := newRequestLogBatcher(store, func() int { return 7 }, nil, time.Hour, 1000)
	batcher.Append(monitor.RequestLog{Path: "/batched"}, false)
	batcher.Close()
	batcher.Append(monitor.RequestLog{Path: "/late"}, false)

	singles, batches := store.snapshot()
	if singles != 1 {
		t.Fatalf("single-row writes after Close = %d, want 1: a row appended after the flush would be lost", singles)
	}
	if len(batches) != 1 {
		t.Fatalf("batched writes = %d, want the one flushed by Close", len(batches))
	}
}
