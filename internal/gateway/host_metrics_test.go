package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/store"
)

// 采样器现在常驻：无人看时降频而不是退出，否则页面关着的时段在图上会是空白。
func TestHostMetricsSamplerRunsAndSlowsWhenIdle(t *testing.T) {
	h := newHostMetricsSampler()
	snap := h.Snapshot()
	if snap.CPUCount <= 0 {
		t.Fatalf("cpuCount=%d", snap.CPUCount)
	}
	if snap.CollectedAt.IsZero() {
		t.Fatal("expected collectedAt")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !h.collecting.Load() {
		time.Sleep(20 * time.Millisecond)
	}
	if !h.collecting.Load() {
		t.Fatal("expected background sampler to be running after Snapshot")
	}
	if !h.active() {
		t.Fatal("a fresh Snapshot must count as an active viewer")
	}

	// A subscriber keeps it at full rate even after the request window expires.
	unsubscribe := h.Subscribe()
	h.mu.Lock()
	h.lastRequest = time.Now().Add(-hostMetricsActiveWindow - time.Second)
	h.mu.Unlock()
	if !h.active() {
		t.Fatal("a live subscriber must keep full-rate sampling")
	}
	unsubscribe()
	// Unsubscribing itself counts as activity for the grace window, so age it
	// out before asserting the idle cadence.
	h.mu.Lock()
	h.lastRequest = time.Now().Add(-hostMetricsActiveWindow - time.Second)
	h.mu.Unlock()

	// Nobody watching: the loop stays alive but samples slower.
	if h.active() {
		t.Fatal("expected idle cadence once the window expired and nobody subscribed")
	}
	if !h.collecting.Load() {
		t.Fatal("the sampler must not stop when idle — the chart needs the history")
	}
}

// 泵每拍都要读最新样本，但它不是"观众"：如果读一次就续一次活跃窗口，
// 降频永远不会发生，无人看时也按满速采。
func TestHostMetricsLatestDoesNotCountAsAViewer(t *testing.T) {
	h := newHostMetricsSampler()
	h.Start()
	h.mu.Lock()
	h.lastRequest = time.Now().Add(-hostMetricsActiveWindow - time.Second)
	h.mu.Unlock()

	for i := 0; i < 5; i++ {
		_ = h.Latest()
	}
	if h.active() {
		t.Fatal("reading the latest sample must not extend the active window")
	}
	// Snapshot does count: a page asking for data is a real viewer.
	_ = h.Snapshot()
	if !h.active() {
		t.Fatal("Snapshot must mark an active viewer")
	}
}

// 冷启动时两个控制台页签同时连上来：谁先跑 Start 都要等环形缓冲就绪，
// 后到的那个不能拿到空历史（曾经会画出空白曲线）。
func TestHostMetricsConcurrentStartWaitsForHistory(t *testing.T) {
	backing := &fakeHostMetricStore{}
	seeded := storePoint(time.Now().Add(-30*time.Second).UnixMilli(), 11)
	backing.samples = append(backing.samples, seeded)

	h := newHostMetricsSampler(backing)
	var wg sync.WaitGroup
	empty := make([]bool, 8)
	for i := range empty {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			empty[index] = len(h.History()) == 0
		}(i)
	}
	wg.Wait()
	for i, wasEmpty := range empty {
		if wasEmpty {
			t.Fatalf("caller %d saw an empty history after Start", i)
		}
	}
}

func TestHostMetricsHistoryRingKeepsWindowAndDedupes(t *testing.T) {
	h := newHostMetricsSampler()
	// 时间戳故意放在未来：History() 会先 Start 并追加一个真实采样点，
	// 若种子点和它落在同一毫秒就是「替换」（1 条），否则被当作乱序丢弃
	// （仍是 1 条）。不做这个偏移，断言结果就取决于两次调用是否同毫秒，
	// Windows 的计时精度约 15ms，几乎必跨。
	now := time.Now().Add(time.Minute)
	h.mu.Lock()
	// A point from outside the window is dropped; an in-window one is kept.
	h.appendLocked(storePoint(now.Add(-hostMetricsHistoryWindow-time.Minute).UnixMilli(), 1))
	h.appendLocked(storePoint(now.UnixMilli(), 2))
	h.appendLocked(storePoint(now.UnixMilli(), 3)) // same ms: replaces, not duplicates
	points := make([]store.HostMetricSample, len(h.samples))
	copy(points, h.samples)
	h.mu.Unlock()

	if len(points) != 1 {
		t.Fatalf("ring kept %d points: %+v", len(points), points)
	}
	if points[0].CPU != 3 {
		t.Fatalf("same-millisecond sample must replace the old one, cpu=%v", points[0].CPU)
	}
	if got := len(h.History()); got != 1 {
		t.Fatalf("History()=%d", got)
	}
}

func TestHostMetricsSamplerFlushesAndRestoresPersistedHistory(t *testing.T) {
	backing := &fakeHostMetricStore{}
	first := newHostMetricsSampler(backing)
	// Seed the buffers directly: appendLocked rejects points older than the
	// live sample Start() just took, and this test is about flush + restore.
	seeded := storePoint(time.Now().Add(-time.Minute).UnixMilli(), 42)
	first.mu.Lock()
	first.samples = []store.HostMetricSample{seeded}
	first.pending = []store.HostMetricSample{seeded}
	first.mu.Unlock()
	first.Stop()

	if len(backing.samples) == 0 {
		t.Fatal("Stop must flush buffered samples")
	}
	if backing.prunedAt == 0 {
		t.Fatal("the first flush of a run must prune rows past the retention window")
	}

	// A restart seeds the ring from the store, so the chart does not start empty.
	second := newHostMetricsSampler(backing)
	second.Start()
	found := false
	for _, point := range second.History() {
		if point.CPU == 42 {
			found = true
		}
	}
	if !found {
		t.Fatalf("restored history missing the persisted point: %+v", second.History())
	}
}

// 流连上时先发一整窗快照；历史绝不能再逐条重发一遍，否则客户端要先吞掉
// 上千个重复帧才看到 snapshot，而且后面的 sample 会和快照里的点重复。
func TestHostStreamSendsSnapshotWithoutReplayingHistory(t *testing.T) {
	server := &Server{hostMetrics: newHostMetricsSampler()}
	recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/__host-metrics/stream", nil).WithContext(ctx)
	req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, adminIdentity()))

	done := make(chan struct{})
	go func() {
		server.handleHostMetricsStream(recorder, req)
		close(done)
	}()
	<-done

	body := recorder.Body.String()
	if !strings.Contains(body, "event: snapshot") {
		t.Fatalf("snapshot frame missing: %q", body)
	}
	if strings.Contains(body, "event: sample") {
		t.Fatalf("connect must not replay every historical point as a sample frame: %q", body)
	}
	if !strings.Contains(body, `"points":[`) {
		t.Fatalf("snapshot must carry the window: %q", body)
	}
}

func TestHandleHostMetricsRequiresAdmin(t *testing.T) {
	s := &Server{hostMetrics: newHostMetricsSampler()}
	rec := httptest.NewRecorder()
	s.handleHostMetrics(rec, httptest.NewRequest(http.MethodGet, "/__host-metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/__host-metrics", nil)
	req = req.WithContext(context.WithValue(req.Context(), authContextKey{},
		sessionIdentity{UserID: "u1", Username: "bob", Role: domain.UserRoleUser}))
	s.handleHostMetrics(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("normal user status=%d", rec.Code)
	}
}

func TestHandleHostMetricsJSON(t *testing.T) {
	s := &Server{hostMetrics: newHostMetricsSampler()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/__host-metrics", nil)
	req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, adminIdentity()))
	s.handleHostMetrics(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var m HostMetrics
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.CPUCount <= 0 {
		t.Fatalf("cpuCount=%d", m.CPUCount)
	}
}

func TestParsePowermetricsCPUTemp(t *testing.T) {
	in := []byte("Something\nCPU die temperature: 52.25 C\nOther\n")
	v, ok := parsePowermetricsCPUTemp(in)
	if !ok || v < 52 || v > 53 {
		t.Fatalf("got %v ok=%v", v, ok)
	}
}

func TestParsePowermetricsThermalPressure(t *testing.T) {
	in := []byte("**** Thermal pressure ****\n\nCurrent pressure level: Nominal\n")
	level, ok := parsePowermetricsThermalPressure(in)
	if !ok || level != "Nominal" {
		t.Fatalf("got %q ok=%v", level, ok)
	}
	if thermalPressureLabelZH("Nominal") != "正常" {
		t.Fatalf("label=%q", thermalPressureLabelZH("Nominal"))
	}
}

func TestSumInterfaceCountersSkipsVirtual(t *testing.T) {
	got := sumInterfaceCounters([]interfaceCounter{
		{Name: "lo0", RX: 100, TX: 100},
		{Name: "utun3", RX: 500, TX: 500},
		{Name: "en0", RX: 1000, TX: 200},
		{Name: "en1", RX: 0, TX: 0}, // link down, ignored
		{Name: "eth0", RX: 7, TX: 3},
	})
	if !got.ok || got.interfaces != 2 || got.rx != 1007 || got.tx != 203 {
		t.Fatalf("got %+v", got)
	}
}

func TestNetRates(t *testing.T) {
	base := time.Now()
	prev := netCounters{rx: 1000, tx: 500, at: base, ok: true}
	cur := netCounters{rx: 3000, tx: 1500, at: base.Add(2 * time.Second), ok: true}
	rx, tx, ok := netRates(prev, cur)
	if !ok || rx != 1000 || tx != 500 {
		t.Fatalf("rx=%v tx=%v ok=%v", rx, tx, ok)
	}
	// Counter reset must not produce a spike.
	if _, _, ok := netRates(cur, netCounters{rx: 10, tx: 10, at: cur.at.Add(time.Second), ok: true}); ok {
		t.Fatal("expected reset to be rejected")
	}
	// First sample has no baseline.
	if _, _, ok := netRates(netCounters{}, cur); ok {
		t.Fatal("expected no rate without baseline")
	}
}

func TestApplyMemPercent(t *testing.T) {
	var m HostMetrics
	m.applyMem(8<<30, 2<<30)
	if !m.MemAvailable || m.MemPercent < 24.9 || m.MemPercent > 25.1 {
		t.Fatalf("got %+v", m)
	}
}

// fakeHostMetricStore is an in-memory stand-in for *store.Store.
type fakeHostMetricStore struct {
	samples  []store.HostMetricSample
	prunedAt int64
}

func (f *fakeHostMetricStore) AppendHostMetricSamples(samples []store.HostMetricSample) error {
	f.samples = append(f.samples, samples...)
	return nil
}

func (f *fakeHostMetricStore) HostMetricSamplesSince(fromMs int64) ([]store.HostMetricSample, error) {
	var out []store.HostMetricSample
	for _, sample := range f.samples {
		if sample.AtMs >= fromMs {
			out = append(out, sample)
		}
	}
	return out, nil
}

func (f *fakeHostMetricStore) PruneHostMetricSamples(beforeMs int64) (int64, error) {
	f.prunedAt = beforeMs
	return 0, nil
}

func storePoint(atMs int64, cpu float64) store.HostMetricSample {
	return store.HostMetricSample{AtMs: atMs, CPU: cpu}
}

// flushRecorder is an httptest.ResponseRecorder that also implements
// http.Flusher, which the SSE handler requires.
type flushRecorder struct {
	*httptest.ResponseRecorder
}

func (f *flushRecorder) Flush() {}
