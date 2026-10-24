package gateway

import (
	"net/http"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// 出图落在 Key 的兜底 Provider 上时，校验的必须是真正收请求的那个 Provider，
// 而不是路由默认值：路由默认值健康、兜底已被停用时必须当场 503，不发请求。
func TestImagesHandlerRefusesDisabledEffectiveProvider(t *testing.T) {
	upstream, gotPath, _ := imagesUpstream(t)
	live := imagesImageProvider("img-a", false)
	paused := imagesImageProvider("img-b", false)
	paused.BaseURL = upstream.URL + "/v1"
	server := imagesTestServer(t, live, paused)
	if disabled, err := server.router.SetProviderDisabled("img-b", true); err != nil || !disabled.Disabled {
		t.Fatalf("disable img-b: %+v %v", disabled, err)
	}
	key, err := server.router.AddAPIKey(domain.APIKey{
		Key: "sk-gw-admin", Name: "k1", RouteID: "img-route",
		FallbackProviderIDs: []string{"img-b"},
	})
	if err != nil {
		t.Fatalf("add key: %v", err)
	}
	// 调度把 Key 粘到兜底 Provider 上（切换就是这个形状）。
	if _, err := server.router.SetAPIKeyActiveProvider(key.ID, "img-b"); err != nil {
		t.Fatalf("pin active provider: %v", err)
	}
	stored, _ := server.router.APIKeyByID(key.ID)
	if stored.ActiveProviderID != "img-b" {
		t.Fatalf("active provider = %q, want img-b", stored.ActiveProviderID)
	}

	recorder := imagesRequest(t, server, "sk-gw-admin", `{"model":"gpt-image-2","prompt":"x"}`)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", recorder.Code, recorder.Body.String())
	}
	if *gotPath != "" {
		t.Fatalf("disabled provider still reached the upstream at %q", *gotPath)
	}
}

// Provider 级校验本身：禁用、已删除、协议不对的落点都要拒绝。
func TestImagesProviderRefusal(t *testing.T) {
	paused := imagesImageProvider("img-b", true)
	claude := imagesImageProvider("img-c", false)
	claude.Protocol = domain.ProtocolClaude
	server := imagesTestServer(t, imagesImageProvider("img-a", false))
	for _, provider := range []domain.Provider{paused, claude} {
		if _, err := server.router.AddProvider(provider); err != nil {
			t.Fatalf("add provider %s: %v", provider.ID, err)
		}
	}
	if _, err := server.router.UpdateProvider("img-a", domain.Provider{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if refusal, ok := imagesProviderRefusal(server.router, "img-b"); !ok || refusal.status != http.StatusServiceUnavailable {
		t.Fatalf("disabled provider refusal = %+v ok=%v, want 503", refusal, ok)
	}
	if refusal, ok := imagesProviderRefusal(server.router, "img-c"); !ok || refusal.status != http.StatusBadGateway {
		t.Fatalf("claude provider refusal = %+v ok=%v, want 502", refusal, ok)
	}
	if _, ok := imagesProviderRefusal(server.router, "img-a"); ok {
		t.Fatal("live OpenAI provider must not be refused")
	}
}
