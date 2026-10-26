package gateway

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// priceStoreSpy is an in-memory PriceStore whose load can be held open. With
// staleRead set it returns the rows as they were when the load started (and then
// waits for holdOpen): that is what a read that raced a write actually returns.
type priceStoreSpy struct {
	mu        sync.Mutex
	rows      []domain.ModelPrice
	staleRead bool
	holdOpen  chan struct{}
	loads     atomic.Int32
	failWith  error
}

func (s *priceStoreSpy) LoadModelPrices() ([]domain.ModelPrice, error) {
	s.loads.Add(1)
	s.mu.Lock()
	snapshot := append([]domain.ModelPrice(nil), s.rows...)
	failWith := s.failWith
	hold := s.holdOpen
	if s.staleRead {
		// Only the first read is the raced one.
		s.holdOpen = nil
	}
	s.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if failWith != nil {
		return nil, failWith
	}
	return snapshot, nil
}

func (s *priceStoreSpy) SaveModelPrice(price domain.ModelPrice) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rows {
		if s.rows[i].ProviderID == price.ProviderID && s.rows[i].ModelID == price.ModelID {
			s.rows[i] = price
			return nil
		}
	}
	s.rows = append(s.rows, price)
	return nil
}

func (s *priceStoreSpy) DeleteModelPrice(providerID, modelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.rows[:0]
	for _, row := range s.rows {
		if row.ProviderID == providerID && row.ModelID == modelID {
			continue
		}
		kept = append(kept, row)
	}
	s.rows = kept
	return nil
}

func newPricingTestServer(store PriceStore) *Server {
	server := NewServer(NewRouter(domain.GatewayState{Providers: []domain.Provider{{ID: "p1", Name: "P"}}}), monitor.NewStore())
	server.SetPriceStore(store)
	return server
}

// 管理员保存单价时如果正好有一次冷加载在飞，那次加载读到的是旧表；它回来后如果
// 照常写回缓存并标成已加载，这次保存就被悄悄撤销了：之后每个请求都按旧单价计费
// （或新加的单价一直显示未计价），当天的成本是错的，且直到进程重启都不会自愈。
func TestPriceCacheDoesNotUndoASaveThatRacedItsLoad(t *testing.T) {
	hold := make(chan struct{})
	// 第一次读表拿到的是 rows=1 的快照，且卡住不返回；保存就发生在它返回之前。
	spy := &priceStoreSpy{rows: []domain.ModelPrice{{ProviderID: "*", ModelID: "m1", InputUSDPerMTok: 1}}, staleRead: true, holdOpen: hold}
	server := newPricingTestServer(spy)

	loaded := make(chan struct{})
	go func() {
		defer close(loaded)
		server.lookupPrice("p1", "m1") // 冷加载开始，卡在 store 里
	}()
	time.Sleep(50 * time.Millisecond)

	// 管理员在加载在飞的时候改了单价，随后 handler 会 invalidatePrices。
	if err := spy.SaveModelPrice(domain.ModelPrice{ProviderID: "*", ModelID: "m1", InputUSDPerMTok: 100}); err != nil {
		t.Fatal(err)
	}
	server.invalidatePrices()

	close(hold) // 在飞的加载带着旧行返回
	<-loaded

	price, ok := server.lookupPrice("p1", "m1")
	if !ok {
		t.Fatal("m1 should be priced")
	}
	if price.InputUSDPerMTok != 100 {
		t.Fatalf("after saving 100 the ledger still prices m1 at %v: the in-flight load overwrote the invalidation", price.InputUSDPerMTok)
	}
}

// 同时到达的多个请求只应查一次库：其余请求等那一次加载的结果。这也是「等结果
// 而不是各自再查」这条实现的理由。
func TestPriceCacheLoadsOnceForConcurrentCallers(t *testing.T) {
	hold := make(chan struct{})
	spy := &priceStoreSpy{rows: []domain.ModelPrice{{ProviderID: "*", ModelID: "m1", InputUSDPerMTok: 2}}, holdOpen: hold}
	server := newPricingTestServer(spy)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if price, ok := server.lookupPrice("p1", "m1"); !ok || price.InputUSDPerMTok != 2 {
				t.Errorf("lookup got (%+v, %v), want the stored row", price, ok)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(hold)
	wg.Wait()
	if got := spy.loads.Load(); got != 1 {
		t.Fatalf("the table was loaded %d time(s) for 32 concurrent callers, want 1", got)
	}
}

// 读表失败不能被缓存成「空表已加载」：下一次请求要能重新读到。
func TestPriceCacheDoesNotCacheAFailedLoad(t *testing.T) {
	spy := &priceStoreSpy{rows: []domain.ModelPrice{{ProviderID: "*", ModelID: "m1", InputUSDPerMTok: 3}}, failWith: errors.New("database is locked")}
	server := newPricingTestServer(spy)
	if _, ok := server.lookupPrice("p1", "m1"); ok {
		t.Fatal("a failed load must not price anything")
	}
	spy.mu.Lock()
	spy.failWith = nil
	spy.mu.Unlock()
	if price, ok := server.lookupPrice("p1", "m1"); !ok || price.InputUSDPerMTok != 3 {
		t.Fatalf("the second lookup got (%+v, %v) after the store recovered, want the stored row", price, ok)
	}
	if spy.loads.Load() != 2 {
		t.Fatalf("loads = %d, want 2 (the failure must not be cached)", spy.loads.Load())
	}
}
