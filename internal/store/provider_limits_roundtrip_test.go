package store

import (
	"path/filepath"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// 账号分组的三个限额必须真的落库：漏写一列，重启后管理员设的额度就没了，
// 而且界面上还显示着旧值——比没设更糟。
func TestProviderLimitsRoundTrip(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	provider := domain.Provider{
		ID: "lane-p", Name: "lane-p", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://example.com/v1",
		LaneGroup: "acct", MaxConcurrentRequests: 3, RequestsPerMinute: 42, QueueWaitSeconds: 7,
	}
	if err := store.Save(domain.GatewayState{Providers: []domain.Provider{provider}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := store.Load(domain.GatewayState{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(loaded.Providers))
	}
	got := loaded.Providers[0]
	if got.LaneGroup != "acct" {
		t.Fatalf("lane group = %q, want acct", got.LaneGroup)
	}
	if got.MaxConcurrentRequests != 3 || got.RequestsPerMinute != 42 || got.QueueWaitSeconds != 7 {
		t.Fatalf("limits did not round-trip: max=%d rpm=%d wait=%d",
			got.MaxConcurrentRequests, got.RequestsPerMinute, got.QueueWaitSeconds)
	}
}
