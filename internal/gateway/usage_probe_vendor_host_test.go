package gateway

import (
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// 用量/余额探针会把 Provider 的 API Key 发到厂商的固定主机上（Zhipu 是
// open.bigmodel.cn，DeepSeek 是 api.deepseek.com）。哪些 Provider 值得探，取决于
// BaseURL 到底是不是厂商自己——按「后缀匹配」判断会把挂在该域名下的第三方中转也算
// 成厂商，于是那家中转的 key 被送到了厂商主机。
func TestVendorProbeOnlyForTheVendorsOwnHosts(t *testing.T) {
	deepSeekOfficial := []string{"https://api.deepseek.com", "https://api.deepseek.com/v1", "api.deepseek.com"}
	deepSeekRelays := []string{
		"https://proxy.deepseek.com/v1",   // 别人家的中转
		"https://deepseek.com.example/v1", // 只是长得像
		"https://evil.example/?u=deepseek.com",
	}
	for _, base := range deepSeekOfficial {
		if !isDeepSeekBaseURL(base) {
			t.Errorf("isDeepSeekBaseURL(%q) = false, want true", base)
		}
		if got := deepSeekBalanceBase(base); got != base && got != "https://"+base {
			// 只保留 scheme+主机，路径被去掉。
			if got != "https://api.deepseek.com" {
				t.Errorf("deepSeekBalanceBase(%q) = %q", base, got)
			}
		}
	}
	for _, base := range deepSeekRelays {
		if isDeepSeekBaseURL(base) {
			t.Errorf("isDeepSeekBaseURL(%q) = true: a relay's key would be sent to api.deepseek.com", base)
		}
	}

	// 同一份判定驱动「定时刷新哪些 Provider 的额度/余额」：中转不该被排进来。
	official := domain.Provider{ID: "ds", Name: "ds", BaseURL: "https://api.deepseek.com/v1", AuthType: domain.AuthTypeAPIKey, APIKeySource: "sk-ds"}
	relay := domain.Provider{ID: "relay", Name: "relay", BaseURL: "https://proxy.deepseek.com/v1", AuthType: domain.AuthTypeAPIKey, APIKeySource: "sk-relay"}
	if !isDeepSeekBaseURL(official.BaseURL) {
		t.Error("the official provider should be swept")
	}
	if isDeepSeekBaseURL(relay.BaseURL) {
		t.Error("the relay should not be swept: the sweep pins api.deepseek.com")
	}
}

// zhipu 的判断同样只认官方主机（bigmodel.cn / z.ai 及其子域），而探针地址按官方
// 主机决定：一个 z.ai.evil 之类的伪装不能把 key 引到别处。
func TestZhipuProbeStaysOnOfficialHosts(t *testing.T) {
	for _, base := range []string{"https://open.bigmodel.cn/api/paas/v4", "https://api.z.ai/v1", "bigmodel.cn"} {
		if !isZhipuBaseURL(base) {
			t.Errorf("isZhipuBaseURL(%q) = false, want true", base)
		}
	}
	for _, base := range []string{"https://z.ai.evil.example/v1", "https://evil.example/?u=bigmodel.cn", "https://bigmodel.cn.evil.example/v1"} {
		if isZhipuBaseURL(base) {
			t.Errorf("isZhipuBaseURL(%q) = true", base)
		}
	}
}
