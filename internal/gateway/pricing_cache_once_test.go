package gateway

import (
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// memPriceStore 是只读的价格表，给计费口径的测试用。
type memPriceStore struct{ rows []domain.ModelPrice }

func (m memPriceStore) LoadModelPrices() ([]domain.ModelPrice, error) { return m.rows, nil }
func (m memPriceStore) SaveModelPrice(domain.ModelPrice) error        { return nil }
func (m memPriceStore) DeleteModelPrice(string, string) error         { return nil }

// 缓存的 token 已经含在 InputTokens 里：计费时必须按缓存价单独算，不能再用
// 全价算一遍输入，否则每一轮 agent 对话都被多收几倍（缓存读便宜十倍）。
func TestPricingCountsCachedTokensOnce(t *testing.T) {
	server := &Server{}
	server.SetPriceStore(memPriceStore{rows: []domain.ModelPrice{{
		ProviderID: "*", ModelID: "*",
		InputUSDPerMTok: 3, OutputUSDPerMTok: 15, CacheReadUSDPerMTok: 0.3, CacheWriteUSDPerMTok: 3.75,
	}}})

	// Claude 口径：1000 未缓存输入 + 900k 缓存读 + 100k 缓存写 = InputTokens 1,000,000。
	usage := normalizeClaudeUsageFields(1_000_000-900_000-100_000, 1000, 900_000, 100_000)
	cost, priced := server.costOf("p1", "m", usage)
	if !priced {
		t.Fatalf("the wildcard price row should price every model")
	}
	// 1000*15 + 900000*0.3 + 100000*3.75 = 660,000 微美元。
	// 若把 1M 输入再按全价算一遍会多出 3,000,000。
	if cost != 660_000 {
		t.Fatalf("cost = %d micros, want 660000 (cached tokens billed once)", cost)
	}

	// 全部未缓存的 1M 输入只值 3,000,000。
	if plain, _ := server.costOf("p1", "m", TokenUsage{InputTokens: 1_000_000}); plain != 3_000_000 {
		t.Fatalf("plain input cost = %d, want 3000000", plain)
	}

	// 缓存读大于输入（上游口径错乱）时必须夹紧，不能算出负的输入量。
	if clamped, _ := server.costOf("p1", "m", TokenUsage{InputTokens: 1_000_000, CacheTokens: 2_000_000, CacheWriteTokens: 500}); clamped != 300_000 {
		t.Fatalf("overlapping cache cost = %d, want the read price applied to the whole input", clamped)
	}
}

// 没有价格行的模型是"未计价"，不是 $0：把未计价当零会把账单读成免费。
func TestPricingUnpricedIsNotZero(t *testing.T) {
	server := &Server{}
	server.SetPriceStore(memPriceStore{rows: []domain.ModelPrice{{ProviderID: "p1", ModelID: "priced", InputUSDPerMTok: 1}}})
	if cost, priced := server.costOf("p1", "other", TokenUsage{InputTokens: 1000}); priced || cost != 0 {
		t.Fatalf("an unpriced model came back priced=%v cost=%d", priced, cost)
	}
}
