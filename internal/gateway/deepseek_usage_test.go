package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestIsDeepSeekBaseURL(t *testing.T) {
	for _, url := range []string{
		"https://api.deepseek.com",
		"https://api.deepseek.com/v1",
		"HTTPS://API.DEEPSEEK.COM/v1",
	} {
		if !isDeepSeekBaseURL(url) {
			t.Fatalf("expected %q to be recognized as DeepSeek", url)
		}
	}
	// Relays that merely serve deepseek-* models do not implement /user/balance.
	for _, url := range []string{
		"", "https://api.anthropic.com", "https://api.z.ai/api/coding/paas/v4",
		"https://my-relay.example.com/v1",
	} {
		if isDeepSeekBaseURL(url) {
			t.Fatalf("did not expect %q to be recognized as DeepSeek", url)
		}
	}
}

// A provider configured with a versioned path must still resolve to the bare
// host, because /user/balance lives at the root.
func TestDeepSeekBalanceBaseStripsPath(t *testing.T) {
	cases := map[string]string{
		"https://api.deepseek.com":         "https://api.deepseek.com",
		"https://api.deepseek.com/":        "https://api.deepseek.com",
		"https://api.deepseek.com/v1":      "https://api.deepseek.com",
		"https://api.deepseek.com/v1/chat": "https://api.deepseek.com",
		"":                                 "https://api.deepseek.com",
	}
	for input, want := range cases {
		if got := deepSeekBalanceBase(input); got != want {
			t.Fatalf("deepSeekBalanceBase(%q) = %q, want %q", input, got, want)
		}
	}
}

// The query goes to the literal vendor host whatever the provider's BaseURL
// says; the transport stands in for the network so the test can see it.
func TestFetchDeepSeekBalanceParsesResponse(t *testing.T) {
	var gotHost, gotPath, gotAuth string
	probe := useProbeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[
			{"currency":"CNY","total_balance":"110.00","granted_balance":"10.00","topped_up_balance":"100.00"}]}`))
	}))

	report, err := fetchDeepSeekBalance(context.Background(), "https://api.deepseek.com/v1", "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	gotHost = probe.lastHost
	if gotHost != "api.deepseek.com" || gotPath != "/user/balance" {
		t.Fatalf("queried %q %q, want api.deepseek.com /user/balance", gotHost, gotPath)
	}
	// Unlike Zhipu, DeepSeek requires the Bearer prefix.
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("unexpected auth header %q", gotAuth)
	}
	if !report.Available || !report.IsAvailable {
		t.Fatalf("expected an available report, got %+v", report)
	}
	if len(report.BalanceInfos) != 1 {
		t.Fatalf("expected 1 balance info, got %d", len(report.BalanceInfos))
	}
	info := report.BalanceInfos[0]
	// Amounts must stay verbatim strings so money keeps its exact decimals.
	if info.Currency != "CNY" || info.TotalBalance != "110.00" ||
		info.GrantedBalance != "10.00" || info.ToppedUpBalance != "100.00" {
		t.Fatalf("unexpected balance info: %+v", info)
	}
	if report.FetchedAt == "" {
		t.Fatal("expected fetchedAt to be set")
	}
}

// is_available:false means the account is out of credit — still a successful
// read, so Available stays true while IsAvailable goes false.
func TestFetchDeepSeekBalanceExhaustedAccount(t *testing.T) {
	useProbeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"0.00"}]}`))
	}))

	report, err := fetchDeepSeekBalance(context.Background(), "https://api.deepseek.com", "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Available {
		t.Fatal("reading the balance succeeded, so Available must be true")
	}
	if report.IsAvailable {
		t.Fatal("expected IsAvailable to be false for an exhausted account")
	}
}

func TestFetchDeepSeekBalanceAuthFailure(t *testing.T) {
	useProbeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))

	report, err := fetchDeepSeekBalance(context.Background(), "https://api.deepseek.com", "sk-bad")
	if err != nil {
		t.Fatalf("auth failures are reported in-band, not as errors: %v", err)
	}
	if report.Available || report.Unsupported {
		t.Fatalf("expected an unavailable report, got %+v", report)
	}
	if !strings.Contains(report.Error, "401") {
		t.Fatalf("expected the status code in the error, got %q", report.Error)
	}
}

// A relay without the endpoint returns 404: mark Unsupported so the console
// stops polling and shows an explanatory note instead of an error.
func TestFetchDeepSeekBalanceUnsupportedOn404(t *testing.T) {
	useProbeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	report, err := fetchDeepSeekBalance(context.Background(), "https://api.deepseek.com", "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Unsupported || report.Available {
		t.Fatalf("expected unsupported, got %+v", report)
	}
}

func TestFetchDeepSeekBalanceRequiresAPIKey(t *testing.T) {
	report, err := fetchDeepSeekBalance(context.Background(), "https://api.deepseek.com", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if report.Available || report.Error == "" {
		t.Fatalf("expected a missing-key report, got %+v", report)
	}
}

func TestFetchDeepSeekBalanceMalformedBody(t *testing.T) {
	useProbeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json at all`))
	}))

	report, err := fetchDeepSeekBalance(context.Background(), "https://api.deepseek.com", "sk-test")
	if err != nil {
		t.Fatal(err)
	}
	if report.Available {
		t.Fatalf("expected parse failure to be unavailable, got %+v", report)
	}
}

// Non-admin owners may read their own provider's balance, matching Zhipu.
func TestDeepSeekUsagePathAllowedForUsers(t *testing.T) {
	if !isUserProviderUsagePath("/__providers/p1/deepseek/usage") {
		t.Fatal("expected the DeepSeek usage path to be readable by owners")
	}
	if isUserProviderUsagePath("/__providers/p1/deepseek/other") {
		t.Fatal("only the usage sub-path should be allowed")
	}
}

// useProbeServer points the Zhipu / DeepSeek usage probes at a local handler
// for the duration of one test, leaving their vendor-pinned URLs untouched.
func useProbeServer(t *testing.T, handler http.Handler) *hostDropTransport {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse probe server url: %v", err)
	}
	probe := &hostDropTransport{target: target}
	previous := usageProbeTransport
	usageProbeTransport = probe
	t.Cleanup(func() { usageProbeTransport = previous })
	return probe
}

// hostDropTransport sends every request to the test server while keeping the
// original Host visible to the handler, so a test can still assert which host
// the production code meant to reach.
type hostDropTransport struct {
	target *url.URL
	// lastHost is the host the production code addressed, before the rewrite.
	lastHost string
}

func (t *hostDropTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.lastHost = req.URL.Host
	rewritten := req.Clone(req.Context())
	rewritten.URL.Scheme = t.target.Scheme
	rewritten.URL.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(rewritten)
}
