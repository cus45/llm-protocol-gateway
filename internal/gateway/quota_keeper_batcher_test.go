package gateway

import (
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// The quiet window must see a success that is still queued in the request-log
// batcher: a manual request a minute ago can otherwise be invisible for up to
// one flush interval, and the keeper fires into the window it exists to protect.
func TestQuietWindowSeesBatchedSuccess(t *testing.T) {
	upstream := chatUpstream(t, nil)
	server, _ := newQuotaTestServer(t, upstream.URL)

	// A long flush interval keeps the background loop out of the window.
	committed := &fakeRequestLogStore{}
	server.requestLogStore = committed
	batcher := newRequestLogBatcher(committed, nil, nil, time.Hour, 0)
	t.Cleanup(batcher.Close)
	server.requestLogBatcher = batcher

	// A successful manual request lands in the batcher, not in the store yet.
	batcher.Append(monitor.RequestLog{
		Time:       time.Now().Add(-time.Minute),
		ProviderID: "p1",
		Model:      "claude-sonnet-4-5",
		Status:     200,
	}, false)
	if len(committed.singles) != 0 {
		t.Fatalf("precondition: success row must still be pending, store has %d", len(committed.singles))
	}
	// The store answers like SQLite: only flushed batches are queryable.
	server.requestLogStore = &fakeQueryLogStore{fake: committed}

	usedAt, source, busy := server.providerRecentlyUsed("p1", time.Hour)
	if !busy || source != quotaUsageSourceTraffic || usedAt.IsZero() {
		t.Fatalf("a batched success from a minute ago must hold the quiet window: busy=%v source=%q at=%v", busy, source, usedAt)
	}
}

// fakeQueryLogStore delegates appends to the same fake the batcher writes into
// and answers queries like the real store — from what was actually appended,
// not from what is still pending.
type fakeQueryLogStore struct {
	fakeRequestLogStore
	fake *fakeRequestLogStore
}

func (f *fakeQueryLogStore) AppendRequestLog(entry monitor.RequestLog) error {
	return f.fake.AppendRequestLog(entry)
}

func (f *fakeQueryLogStore) AppendRequestLogWithRetention(entry monitor.RequestLog, days int) error {
	return f.fake.AppendRequestLogWithRetention(entry, days)
}

func (f *fakeQueryLogStore) AppendRequestLogs(entries []monitor.RequestLog) error {
	return f.fake.AppendRequestLogs(entries)
}

func (f *fakeQueryLogStore) QueryRequestLogs(query monitor.RequestLogQuery) (monitor.RequestLogPage, error) {
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	for _, batch := range f.fake.batches {
		for _, entry := range batch {
			if query.ProviderID != "" && entry.ProviderID != query.ProviderID {
				continue
			}
			if query.Status == "2xx" && (entry.Status < 200 || entry.Status > 299) {
				continue
			}
			if !query.From.IsZero() && entry.Time.Before(query.From) {
				continue
			}
			return monitor.RequestLogPage{Items: []monitor.RequestLog{entry}}, nil
		}
	}
	return monitor.RequestLogPage{}, nil
}
