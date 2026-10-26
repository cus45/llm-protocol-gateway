package gateway

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// 流式转发的复制循环（Write）和心跳 goroutine（Flush）会同时碰同一个
// http.ResponseWriter，而 net/http 不允许两个 goroutine 并发读写它：Flush 插进
// 一帧写到一半的地方，服务器会把 chunk 缓冲里的字节再吐一遍，客户端收到重复的
// SSE。passThroughOpeningWriter 是两者唯一交汇的地方，Write/Flush/WriteHeader
// 必须共用一把锁。
func TestPassThroughOpeningWriterSerialisesWriteAndFlush(t *testing.T) {
	recorder := &concurrencyCheckingWriter{header: http.Header{}, body: &strings.Builder{}}
	opening := &passThroughOpeningWriter{w: recorder, limit: passThroughLogBufferMax}
	opening.tee = &streamTee{limitedBuffer: &limitedBuffer{max: passThroughLogBufferMax}}
	opening.end = &streamEndReader{}

	const writers = 8
	const perWriter = 200
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				_, _ = opening.Write([]byte("event: chunk\n\n"))
			}
		}()
	}
	// The heartbeat goroutine: flushing while the body is still being copied.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < perWriter; j++ {
			opening.Flush()
		}
	}()
	wg.Wait()

	if recorder.overlapped.Load() {
		t.Fatal("Write and Flush reached the response writer concurrently: net/http does not allow that, and a flush mid-frame duplicates SSE bytes")
	}
	want := strings.Repeat("event: chunk\n\n", writers*perWriter)
	if got := recorder.body.String(); got != want {
		t.Fatalf("client received %d bytes, want %d: output was duplicated or dropped", len(got), len(want))
	}
}

// concurrencyCheckingWriter fails if anything calls Write while another call is
// still inside Write or Flush on it.
type concurrencyCheckingWriter struct {
	header     http.Header
	body       *strings.Builder
	busy       atomic.Bool
	overlapped atomic.Bool
}

func (w *concurrencyCheckingWriter) Header() http.Header { return w.header }

func (w *concurrencyCheckingWriter) WriteHeader(int) {}

func (w *concurrencyCheckingWriter) Write(p []byte) (int, error) {
	w.enter()
	defer w.leave()
	// Write the bytes in two steps so a concurrent Flush has a window to land
	// in the middle of the frame.
	half := len(p) / 2
	_, _ = w.body.Write(p[:half])
	_, _ = w.body.Write(p[half:])
	return len(p), nil
}

func (w *concurrencyCheckingWriter) Flush() {
	w.enter()
	defer w.leave()
}

func (w *concurrencyCheckingWriter) enter() {
	if !w.busy.CompareAndSwap(false, true) {
		w.overlapped.Store(true)
	}
}

func (w *concurrencyCheckingWriter) leave() { w.busy.Store(false) }
