package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// blockingRequestLogStore never returns from a batch write. It stands in for the
// single SQLite writer connection being held by a long maintenance statement —
// which is exactly what used to keep the process alive until systemd killed it.
type blockingRequestLogStore struct {
	fakeRequestLogStore
	release chan struct{}
}

func (f *blockingRequestLogStore) AppendRequestLogs([]monitor.RequestLog) error {
	<-f.release
	return nil
}

// 关停时 flush 必须让路，不能等一个卡住的写者。
//
// 生产上发生过的事：启动剪枝一条语句独占写连接几分钟，优雅退出的 batcher.Close()
// 无期限等它，进程 90 秒不退，systemd SIGKILL——丢的是整个内存里的写批次，还让每次
// 发布看起来卡死。现在每一步都吃 ctx 的预算：超时就报告并放手。
func TestFlushDeferredWritesGivesUpInsteadOfWaitingForAWedgedWriter(t *testing.T) {
	blocked := &blockingRequestLogStore{release: make(chan struct{})}
	t.Cleanup(func() { close(blocked.release) })

	batcher := newRequestLogBatcher(blocked, func() int { return 7 }, nil, time.Hour, 100)
	server := &Server{requestLogBatcher: batcher, logs: monitor.NewStore()}
	batcher.Append(monitor.RequestLog{Time: time.Now(), Status: 200}, false)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := server.FlushDeferredWrites(ctx)
	elapsed := time.Since(started)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("flush returned %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("flush waited %s: the shutdown budget is the whole point", elapsed)
	}
}

// 写者没被卡住时，同一批数据要照常落库——有预算不等于放弃落盘。
func TestFlushDeferredWritesLandsRowsWhenTheWriterIsFree(t *testing.T) {
	store := &fakeRequestLogStore{}
	batcher := newRequestLogBatcher(store, func() int { return 7 }, nil, time.Hour, 100)
	server := &Server{requestLogBatcher: batcher, logs: monitor.NewStore()}
	batcher.Append(monitor.RequestLog{Time: time.Now(), Status: 200}, false)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.FlushDeferredWrites(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	_, batches := store.snapshot()
	if len(batches) == 0 {
		t.Fatal("queued rows were dropped instead of persisted on shutdown")
	}
}
