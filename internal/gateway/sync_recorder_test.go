package gateway

import (
	"bytes"
	"net/http"
	"sync"
)

// syncRecorder 是并发安全的 ResponseRecorder。
//
// 心跳测试要在后台 goroutine 仍在写的时候轮询已经写出的内容，而
// httptest.ResponseRecorder 内部的 bytes.Buffer 没有锁：写方是心跳或流拷贝
// 的 goroutine，读方是测试主 goroutine，-race 下必然报 data race。
type syncRecorder struct {
	mu      sync.Mutex
	header  http.Header
	body    bytes.Buffer
	flushes int
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{header: http.Header{}}
}

func (r *syncRecorder) Header() http.Header { return r.header }

func (r *syncRecorder) WriteHeader(int) {}

func (r *syncRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(p)
}

func (r *syncRecorder) Flush() {
	r.mu.Lock()
	r.flushes++
	r.mu.Unlock()
}

// snapshot 在同一把锁下取 flush 次数与已写内容，避免读到写了一半的 body。
func (r *syncRecorder) snapshot() (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushes, r.body.String()
}

func (r *syncRecorder) String() string {
	_, body := r.snapshot()
	return body
}
