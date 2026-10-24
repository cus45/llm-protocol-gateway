package gateway

import (
	"fmt"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// 账号自己的并发/限速闸门拒绝，是网关的准入控制，不是上游故障：什么都没发出去。
// 若按"传输错误"记账，一个健康账号被连拒三次就会被冷却几分钟——正好与闸门
// 的目的相反（闸门是为了保护账号不被上游 429）。
func TestLaneSaturationIsNotAProviderFailure(t *testing.T) {
	class, reason := classifyProviderFailover(429, nil, fmt.Errorf("%w (1/1 already in flight)", errProviderSaturated), nil)
	if class != failoverNone {
		t.Fatalf("a lane refusal was classified as %v (%s), want no failover and no cooldown", class, reason)
	}
	if reason != "lane_saturated" {
		t.Fatalf("reason = %q, want lane_saturated", reason)
	}

	// 真正的传输错误仍然要算失败：账号卡死必须能被换掉。
	class, _ = classifyProviderFailover(0, nil, fmt.Errorf("dial tcp 1.2.3.4:443: connection refused"), nil)
	if class != failoverSoft {
		t.Fatalf("a real transport error must still be a soft failure, got %v", class)
	}
}

// 走完整判定：lane 拒绝不能让 Provider 进入不可用。
func TestLaneSaturationLeavesProviderAvailable(t *testing.T) {
	provider := domain.Provider{ID: "lane-sat", Name: "lane-sat", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://example.com/v1"}
	server := NewServer(NewRouter(domain.GatewayState{Providers: []domain.Provider{provider}}), monitor.NewStore())
	for i := 0; i < providerSoftFailureThreshold+2; i++ {
		server.recordProviderRequestOutcome(provider.ID, 429, nil, fmt.Errorf("%w", errProviderSaturated))
	}
	if state, ok := server.providerUnavailableSnapshot(provider.ID); ok {
		t.Fatalf("a healthy account was cooled down by its own lane refusals: %#v", state)
	}
}
