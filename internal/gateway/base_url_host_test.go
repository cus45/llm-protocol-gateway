package gateway

import "testing"

func TestBaseURLHostIn(t *testing.T) {
	cases := []struct {
		base    string
		domains []string
		want    bool
	}{
		{"https://open.bigmodel.cn/api/paas/v4", []string{"bigmodel.cn"}, true},
		{"https://bigmodel.cn", []string{"bigmodel.cn"}, true},
		{"https://API.BIGMODEL.CN/v4", []string{"bigmodel.cn"}, true},
		{"https://open.bigmodel.cn./v4", []string{"bigmodel.cn"}, true},
		{"open.bigmodel.cn/v4", []string{"bigmodel.cn"}, true},
		{"https://api.z.ai/api/paas/v4", []string{"bigmodel.cn", "z.ai"}, true},
		{"https://api.deepseek.com/v1", []string{"deepseek.com"}, true},
		{"https://evil.example/v1", []string{"deepseek.com"}, false},
		{"https://evil.example/?u=bigmodel.cn", []string{"bigmodel.cn"}, false},
		{"https://bigmodel.cn.evil.example/v4", []string{"bigmodel.cn"}, false},
		{"https://evilbigmodel.cn/v4", []string{"bigmodel.cn"}, false},
		{"https://notz.ai/v1", []string{"z.ai"}, false},
		{"https://z.ai.evil.example/v1", []string{"z.ai"}, false},
		{"https://user:pass@bigmodel.cn/x", []string{"bigmodel.cn"}, true},
		{"", []string{"bigmodel.cn"}, false},
		{"   ", []string{"bigmodel.cn"}, false},
	}
	for _, c := range cases {
		if got := baseURLHostIn(c.base, c.domains...); got != c.want {
			t.Errorf("baseURLHostIn(%q, %v) = %v, want %v", c.base, c.domains, got, c.want)
		}
	}
}

// 地址里只是「出现」了厂商域名不算该厂商：额度查询会带上 API key，不能把它
// 发到别的主机去。
func TestZhipuUsageRejectsSpoofedHosts(t *testing.T) {
	for _, base := range []string{
		"https://evil.example/?u=bigmodel.cn",
		"https://bigmodel.cn.evil.example/v4",
		"https://notz.ai/v1",
		"https://z.ai.evil.example/v1",
	} {
		if isZhipuBaseURL(base) {
			t.Errorf("isZhipuBaseURL(%q) = true, want false", base)
		}
	}
	for _, base := range []string{"https://open.bigmodel.cn/api/paas/v4", "https://api.z.ai/api/paas/v4"} {
		if !isZhipuBaseURL(base) {
			t.Errorf("isZhipuBaseURL(%q) = false, want true", base)
		}
	}
}

func TestDeepSeekBalanceRejectsSpoofedHosts(t *testing.T) {
	for _, base := range []string{
		"https://evil.example/?u=deepseek.com",
		"https://deepseek.com.evil.example/v1",
		"https://notdeepseek.com/v1",
	} {
		if isDeepSeekBaseURL(base) {
			t.Errorf("isDeepSeekBaseURL(%q) = true, want false", base)
		}
	}
	if !isDeepSeekBaseURL("https://api.deepseek.com/v1") {
		t.Error("isDeepSeekBaseURL(api.deepseek.com) = false, want true")
	}
}

// 查询地址本身也必须落在厂商主机上；解析不出主机时回落到官方端点，而不是
// 回落成空字符串或照搬原地址。
func TestUsageBaseHostsPinnedToVendor(t *testing.T) {
	if got := zhipuQuotaBase("https://api.z.ai/api/paas/v4"); got != "https://api.z.ai" {
		t.Errorf("zhipuQuotaBase(z.ai) = %q", got)
	}
	if got := zhipuQuotaBase("https://bigmodel.cn.evil.example/docs?x=bigmodel.cn"); got != "https://api.z.ai" {
		t.Errorf("zhipuQuotaBase(spoofed) = %q, want the official fallback", got)
	}
	if got := deepSeekBalanceBase("https://evil.example/?u=deepseek.com"); got != "https://api.deepseek.com" {
		t.Errorf("deepSeekBalanceBase(spoofed) = %q, want the official fallback", got)
	}
	if got := deepSeekBalanceBase("https://relay.deepseek.com/v1"); got != "https://relay.deepseek.com" {
		t.Errorf("deepSeekBalanceBase(subdomain) = %q", got)
	}
}
