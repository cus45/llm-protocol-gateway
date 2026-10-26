package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/gateway"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"github.com/luca/llm-protocol-gateway/internal/store"
)

// 优雅退出时 http.Server.Shutdown 会一直等到在飞请求跑完。Stop 先 flush 再
// Shutdown 的话，排空期间完成的请求会写进已经关掉的 batcher：真实进程里它的
// 行留在内存里随进程消失，用量也只剩库里那一行。先排空、再落盘、最后关库，
// 排空中的请求才能落库。
//
// 这里用真实的 Runtime.Stop 和真实的 http.Server：/slow 在 Shutdown 等待期间
// 才结束并记账，正是「排空中完成的请求」。
func TestStopDrainsHTTPBeforeFlushingDeferredWrites(t *testing.T) {
	db, openErr := store.Open(t.TempDir() + "/gateway.db")
	if openErr != nil {
		t.Fatalf("open store: %v", openErr)
	}
	logs := monitor.NewStore()
	router := gateway.NewRouter(domain.GatewayState{Routes: []domain.Route{{
		ID: "r1", ProviderID: "p1", OutputEndpointID: "openai-chat-output",
		OutputProtocol: domain.ProtocolOpenAIChat, Enabled: true,
	}}})
	server := gateway.NewServer(router, logs, db)

	record := func() {
		request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader("{}"))
		server.RecordRequestLogForTest(request, time.Now(), domain.APIKey{ID: "k1", Name: "k1"},
			"r1", "p1", "m1", "/v1/images/generations", http.StatusOK,
			gateway.TokenUsage{InputTokens: 3, OutputTokens: 4})
	}

	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	listener := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		record()
		w.WriteHeader(http.StatusOK)
	}))
	listener.Start()
	httpServer := listener.Config
	t.Cleanup(func() {
		httpServer.Close()
		listener.Close()
	})
	go func() {
		response, getErr := http.Get(listener.URL + "/slow")
		if getErr == nil {
			_ = response.Body.Close()
		}
	}()
	<-arrived // the in-flight request is parked inside the handler

	rt := &Runtime{db: db, logs: logs, router: router, server: server, httpServer: httpServer, started: true}
	stopDone := make(chan error, 1)
	go func() { stopDone <- rt.Stop(context.Background()) }()
	time.Sleep(100 * time.Millisecond) // let Shutdown start waiting on the handler
	close(release)
	if stopErr := <-stopDone; stopErr != nil {
		t.Fatalf("stop: %v", stopErr)
	}

	// Stop already closed the DB; reopen the same file to read what landed.
	reopened, reopenErr := store.Open(db.Path())
	if reopenErr != nil {
		t.Fatalf("reopen store: %v", reopenErr)
	}
	defer func() { _ = reopened.Close() }()
	rows, listErr := reopened.ListRequestLogs(1000)
	if listErr != nil {
		t.Fatalf("list request logs: %v", listErr)
	}
	if len(rows) != 1 {
		t.Fatalf("persisted request rows = %d, want 1: the row of the request that finished during the drain was dropped", len(rows))
	}
}
