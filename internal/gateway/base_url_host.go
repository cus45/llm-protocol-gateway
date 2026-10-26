package gateway

import (
	"net/http"
	"net/url"
	"strings"
)

// baseURLHostIn says whether a provider's BaseURL points at one of `domains`
// (or a subdomain of one). It parses the URL rather than looking for the domain
// as a substring: `https://evil.example/?u=bigmodel.cn` and `https://z.ai.evil`
// both contain a vendor's name yet send the request — and the provider's API
// key — somewhere else entirely.
func baseURLHostIn(baseURL string, domains ...string) bool {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return false
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	for _, domain := range domains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if domain == "" {
			continue
		}
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// baseURLHostIs says whether a provider's BaseURL points at exactly one of
// `hosts` — no subdomain, no path, no query. It is baseURLHostIn's stricter
// sibling for the vendors whose whole domain they do not own: DeepSeek's balance
// endpoint is api.deepseek.com, and proxy.deepseek.com is somebody else's relay.
func baseURLHostIs(baseURL string, hosts ...string) bool {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return false
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	for _, want := range hosts {
		if want = strings.ToLower(strings.TrimSpace(want)); want != "" && host == want {
			return true
		}
	}
	return false
}

// usageProbeTransport carries the Zhipu / DeepSeek usage probes. Those queries
// are pinned to a literal vendor host, and the host is the point: it is where
// the provider's API key is sent. Tests therefore reach a local server by
// swapping the transport, never by loosening the host check.
var usageProbeTransport http.RoundTripper = http.DefaultTransport
