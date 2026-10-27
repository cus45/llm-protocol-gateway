package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// requestLogBatchInterval / requestLogBatchMax bound the batching of successful
// request logs.
//
// Why batch at all: a bodiless success row is only a few hundred bytes, but every
// commit dirties at least one 4 KiB WAL page, so committing per request wastes
// most of each page. Batching a minute's worth of rows into one transaction cuts
// that amplification by roughly an order of magnitude.
//
// The window also bounds what an ungraceful kill can lose. Graceful shutdown
// flushes (see Server.FlushRequestLogs), and reading the log page flushes too,
// so the console never shows stale data.
const (
	requestLogBatchInterval = time.Minute
	requestLogBatchMax      = 200
)

// requestLogBatcher persists successful request logs in batches while writing
// failures through immediately — failures are the rows someone needs when
// debugging an incident, which is exactly when the process may be about to die.
type requestLogBatcher struct {
	store         RequestLogStore
	retentionDays func() int
	onError       func(message, context string)
	flushInterval time.Duration
	maxBatch      int

	mu      sync.Mutex
	pending []monitor.RequestLog
	closed  bool

	wake    chan struct{}
	stop    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

// newRequestLogBatcher starts the background flusher. flushInterval<=0 defaults
// to requestLogBatchInterval and maxBatch<=0 to requestLogBatchMax; both are
// parameters (rather than fields tweaked afterwards) because the goroutine
// starts here and would race with any later mutation.
func newRequestLogBatcher(store RequestLogStore, retentionDays func() int, onError func(message, context string), flushInterval time.Duration, maxBatch int) *requestLogBatcher {
	if store == nil {
		return nil
	}
	if flushInterval <= 0 {
		flushInterval = requestLogBatchInterval
	}
	if maxBatch <= 0 {
		maxBatch = requestLogBatchMax
	}
	b := &requestLogBatcher{
		store:         store,
		retentionDays: retentionDays,
		onError:       onError,
		flushInterval: flushInterval,
		maxBatch:      maxBatch,
		wake:          make(chan struct{}, 1),
		stop:          make(chan struct{}),
		stopped:       make(chan struct{}),
	}
	go b.loop()
	return b
}

// Append queues entry for batched persistence, or writes it through right away
// when failure is true.
//
// After Close there is no goroutine left to flush a queued row, so a late
// Append (a request finishing while the server drains) writes through instead
// of parking the row in memory until the process exits.
func (b *requestLogBatcher) Append(entry monitor.RequestLog, failure bool) {
	if b == nil {
		return
	}
	if failure {
		b.writeOne(entry)
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.writeOne(entry)
		return
	}
	b.pending = append(b.pending, entry)
	full := len(b.pending) >= b.maxBatch
	b.mu.Unlock()
	if full {
		// Non-blocking wake; loop coalesces multiple wakes.
		select {
		case b.wake <- struct{}{}:
		default:
		}
	}
}

func (b *requestLogBatcher) loop() {
	defer close(b.stopped)
	ticker := time.NewTicker(b.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-b.stop:
			b.Flush()
			return
		case <-ticker.C:
			b.Flush()
		case <-b.wake:
			b.Flush()
		}
	}
}

// Flush persists everything queued so far. Safe to call from any goroutine; the
// log-reading handler calls it so the console never misses recent requests.
func (b *requestLogBatcher) Flush() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if len(b.pending) == 0 {
		b.mu.Unlock()
		return
	}
	batch := b.pending
	b.pending = nil
	b.mu.Unlock()

	if err := b.store.AppendRequestLogs(batch); err != nil && b.onError != nil {
		b.onError("failed to persist request log batch", err.Error())
	}
}

func (b *requestLogBatcher) writeOne(entry monitor.RequestLog) {
	retention := 0
	if b.retentionDays != nil {
		retention = b.retentionDays()
	}
	if err := b.store.AppendRequestLogWithRetention(entry, retention); err != nil && b.onError != nil {
		b.onError("failed to persist request log", err.Error())
	}
}

// Close flushes pending logs and stops the background goroutine, waiting as long
// as the flush takes. Shutdown uses CloseContext instead.
func (b *requestLogBatcher) Close() {
	_ = b.CloseContext(context.Background())
}

// CloseContext signals the flusher to land what it holds and waits for it only up
// to ctx's deadline.
//
// The wait is bounded on purpose: landing the batch is a SQLite write queued
// behind the single writer connection, and a long statement (startup pruning, a
// vacuum) can hold that connection for minutes. Unflushed rows cost dashboard
// numbers; an unbounded wait costs the whole restart, because systemd turns
// TimeoutStopSec into a SIGKILL that also discards everything still in flight.
func (b *requestLogBatcher) CloseContext(ctx context.Context) error {
	if b == nil {
		return nil
	}
	b.once.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
		close(b.stop)
	})
	select {
	case <-b.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
