package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// qoderSignInHandler answers what a device flow needs after the browser said
// yes: the job-token trade, and the account endpoint that names the account.
func qoderSignInHandler(jobToken string, jobStatus int, userInfo string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/me/jobToken":
			if jobStatus != 0 && jobStatus != http.StatusOK {
				w.WriteHeader(jobStatus)
			}
			_, _ = io.WriteString(w, jobToken)
		case "/api/v1/userinfo":
			if userInfo == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, userInfo)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestQoderAuthorizationURLCarriesThePKCEChallenge(t *testing.T) {
	authURL, err := qoderAuthorizationURL(qoderGlobalSite, "the-verifier", "nonce-1", "machine-1")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "qoder.com" || parsed.Path != "/device/selectAccounts" {
		t.Fatalf("unexpected page: %s", authURL)
	}
	query := parsed.Query()
	if query.Get("challenge_method") != "S256" || query.Get("nonce") != "nonce-1" || query.Get("machine_id") != "machine-1" {
		t.Fatalf("device-flow parameters wrong: %v", query)
	}
	if query.Get("client_id") != qoderGlobalSite.ClientID {
		t.Fatalf("client id = %q", query.Get("client_id"))
	}
	// The verifier itself never goes to the page — only its digest.
	if query.Get("challenge") == "the-verifier" || strings.Contains(authURL, "the-verifier") {
		t.Fatal("the PKCE verifier leaked into the authorization URL")
	}
	if query.Get("redirect_uri") != "qoder-app://" {
		t.Fatalf("Qoder's desktop client sends its app scheme, got %q", query.Get("redirect_uri"))
	}

	cnURL, err := qoderAuthorizationURL(qoderCNSite, "the-verifier", "nonce-2", "machine-2")
	if err != nil {
		t.Fatal(err)
	}
	// Qoder CN's CLI sends no redirect at all, and signs in as its own client.
	if strings.Contains(cnURL, "redirect_uri") {
		t.Fatalf("Qoder CN must not send a redirect_uri: %s", cnURL)
	}
	if !strings.Contains(cnURL, "client_id=e883ade2") || !strings.HasPrefix(cnURL, "https://qoder.cn/") {
		t.Fatalf("Qoder CN signed in as the wrong client or host: %s", cnURL)
	}
}

func TestQoderCompleteSignInTradesTheDeviceTokenForAJobToken(t *testing.T) {
	server := httptest.NewServer(qoderSignInHandler(
		`{"token":"jt-1","refresh_token":"jrt-1","expires_in":7200000}`,
		http.StatusOK,
		`{"id":"uid-1","name":"Example User","email":"user@example.com"}`,
	))
	defer server.Close()

	credential, err := qoderCompleteSignIn(context.Background(), qoderTestSite(server), qoderDeviceToken{
		Token: "dt-1", RefreshToken: "drt-1", UserID: "uid-1", UserName: "Fallback Name",
	}, "machine-1")
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "jt-1" || credential.RefreshToken != "jrt-1" {
		t.Fatalf("job token pair wrong: %+v", credential)
	}
	if credential.DeviceToken != "dt-1" || credential.DeviceRefresh != "drt-1" {
		t.Fatalf("the device token reads usage and check-in later, so it is kept: %+v", credential)
	}
	if credential.MachineID != "machine-1" || credential.UID != "uid-1" {
		t.Fatalf("identity wrong: %+v", credential)
	}
	if credential.Email != "user@example.com" || credential.Name != "Example User" {
		t.Fatalf("the account endpoint should have named it: %+v", credential)
	}
	if credential.DeviceChat || credential.SignInMode != qoderSignInModeDevice {
		t.Fatalf("mode wrong: %+v", credential)
	}
	if until, err := time.Parse(time.RFC3339, credential.ExpiresAt); err != nil || time.Until(until) < time.Hour {
		t.Fatalf("expiry = %q, want about two hours out", credential.ExpiresAt)
	}
}

// Qoder CN's CLI never makes a job token: its device token is what signs the
// model calls. An account qoder.cn refuses to trade is served that way rather
// than failed.
func TestQoderCompleteSignInFallsBackToDeviceChat(t *testing.T) {
	server := httptest.NewServer(qoderSignInHandler(`{"error":"no job token for you"}`, http.StatusForbidden, `{"id":"uid-2","email":"cn@example.com"}`))
	defer server.Close()

	site := qoderTestSite(server)
	site.DeviceChatFallback = true
	credential, err := qoderCompleteSignIn(context.Background(), site, qoderDeviceToken{
		Token: "dt-2", RefreshToken: "drt-2", UserID: "uid-2", ExpiresIn: 3600,
	}, "machine-2")
	if err != nil {
		t.Fatal(err)
	}
	if !credential.DeviceChat {
		t.Fatalf("expected the device-chat shape, got %+v", credential)
	}
	if credential.AccessToken != "dt-2" || credential.RefreshToken != "drt-2" {
		t.Fatalf("the device token must sign the calls: %+v", credential)
	}
	if until, err := time.Parse(time.RFC3339, credential.ExpiresAt); err != nil || time.Until(until) <= 0 {
		t.Fatalf("expiry = %q, want the device token's own hour", credential.ExpiresAt)
	}
}

// The global site has no such fallback: a job token it refuses means the
// sign-in did not happen.
func TestQoderCompleteSignInReportsARefusedJobToken(t *testing.T) {
	server := httptest.NewServer(qoderSignInHandler(`{"message":"nope"}`, http.StatusForbidden, ""))
	defer server.Close()

	if _, err := qoderCompleteSignIn(context.Background(), qoderTestSite(server),
		qoderDeviceToken{Token: "dt-1", UserID: "uid-1"}, "machine-1"); err == nil {
		t.Fatal("expected the refusal to fail the sign-in")
	}
}

func TestRenewQoderCredentialSpendsTheRefreshToken(t *testing.T) {
	var seenBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/jobToken/refresh" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		seenBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"jt-2","refresh_token":"jrt-2","expires_in":86400000}`)
	}))
	defer server.Close()

	renewed, err := renewQoderCredential(context.Background(), qoderTestSite(server), domain.QoderCredential{
		SignInMode: qoderSignInModeDevice, AccessToken: "jt-1", RefreshToken: "jrt-1", UID: "uid-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Qoder's refresh token is spent on use: the pair it gave is the only thing
	// that can renew again, so the body must carry the old one and the result
	// must carry the new.
	if !strings.Contains(seenBody, `"refresh_token":"jrt-1"`) {
		t.Fatalf("refresh request body = %q", seenBody)
	}
	if renewed.AccessToken != "jt-2" || renewed.RefreshToken != "jrt-2" {
		t.Fatalf("new pair not kept: %+v", renewed)
	}
}

// Only Qoder refusing the refresh means the sign-in is gone. A refresh that
// timed out, or never reached Qoder, must not push the user through a browser.
func TestRenewQoderCredentialMarksALapsedSignIn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := renewQoderCredential(context.Background(), qoderTestSite(server), domain.QoderCredential{
		SignInMode: qoderSignInModeDevice, AccessToken: "jt-1", RefreshToken: "jrt-spent", UID: "uid-1",
	})
	if err == nil {
		t.Fatal("expected a lapsed sign-in")
	}
	if !qoderErrorMeansSignIn(err) {
		t.Fatalf("a refused refresh must read as a sign-in that is gone, got %v", err)
	}

	// A server-side failure is not one.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	_, err = renewQoderCredential(context.Background(), qoderTestSite(upstream), domain.QoderCredential{
		SignInMode: qoderSignInModeDevice, AccessToken: "jt-1", RefreshToken: "jrt-1", UID: "uid-1",
	})
	if err == nil {
		t.Fatal("expected the refresh failure to surface")
	}
	if qoderErrorMeansSignIn(err) {
		t.Fatalf("a 500 is not a lapsed sign-in: %v", err)
	}
}

// A personal access token outlives its job token's refresh pair by months: when
// the pair is gone, the token mints a new one instead of asking for a sign-in.
func TestRenewQoderCredentialReExchangesAPersonalToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/jobToken/refresh":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"refresh spent"}`)
		case "/api/v1/jobToken/exchange":
			_, _ = io.WriteString(w, `{"token":"jt-from-pat","refresh_token":"jrt-from-pat","expires_in":3600000}`)
		case "/api/v1/userinfo":
			_, _ = io.WriteString(w, `{"id":"uid-1","email":"user@example.com"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	renewed, err := renewQoderCredential(context.Background(), qoderTestSite(server), domain.QoderCredential{
		SignInMode: qoderSignInModePAT, PAT: "pt-live", AccessToken: "jt-old", RefreshToken: "jrt-old", UID: "uid-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if renewed.AccessToken != "jt-from-pat" {
		t.Fatalf("expected a re-exchanged token, got %+v", renewed)
	}
	if renewed.PAT != "pt-live" {
		t.Fatalf("the personal access token is carried forward, got %q", renewed.PAT)
	}
}

func TestRenewQoderCredentialRotatesADeviceChatAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/deviceToken/refresh" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"device_token":"dt-2","refresh_token":"drt-2","expires_at":"2027-01-02T00:00:00Z"}`)
	}))
	defer server.Close()

	renewed, err := renewQoderCredential(context.Background(), qoderTestSite(server), domain.QoderCredential{
		DeviceChat: true, AccessToken: "dt-1", RefreshToken: "drt-1", DeviceToken: "dt-1", DeviceRefresh: "drt-1", UID: "uid-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// On this shape the chat pair and the account pair are the one pair, so both
	// halves move together.
	if renewed.AccessToken != "dt-2" || renewed.DeviceToken != "dt-2" || renewed.DeviceRefresh != "drt-2" {
		t.Fatalf("device pair not rotated: %+v", renewed)
	}
	if until, err := time.Parse(time.RFC3339, renewed.ExpiresAt); err != nil || until.Year() != 2027 {
		t.Fatalf("expiry = %q", renewed.ExpiresAt)
	}
}

func TestQoderPollDeviceTokenWaitsForTheAuthorization(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/deviceToken/poll" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		query := r.URL.Query()
		if query.Get("challenge_method") != "S256" || query.Get("nonce") != "n-1" || query.Get("verifier") != "v-1" {
			t.Errorf("poll query wrong: %v", query)
		}
		attempts++
		if attempts < 3 {
			// Not authorized yet: Qoder answers 404 and the client asks again.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"dt-1","refresh_token":"drt-1","user_id":"uid-1","user_name":"Example"}`)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	token, err := qoderPollDeviceToken(ctx, qoderTestSite(server), "n-1", "v-1")
	if err != nil {
		t.Fatal(err)
	}
	if token.accessToken() != "dt-1" || token.UserID != "uid-1" {
		t.Fatalf("device token wrong: %+v", token)
	}
	if attempts < 3 {
		t.Fatalf("expected the poll to wait for the user, took %d tries", attempts)
	}
}

func TestQoderPollDeviceTokenStopsWhenTheWaitEnds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := qoderPollDeviceToken(ctx, qoderTestSite(server), "n", "v"); err == nil {
		t.Fatal("expected the ended wait to surface")
	}
}

func TestQoderAuthorizationURLErrorOnUnknownSite(t *testing.T) {
	if _, err := qoderAuthorizationURL(qoderSite{}, "v", "n", "m"); err == nil {
		t.Fatal("an empty site must not produce a sign-in URL")
	}
}

func TestQoderStatusOfReportsModeNotTokens(t *testing.T) {
	status := qoderStatusOf(domain.Provider{
		AuthType: domain.AuthTypeQoderCN,
		Models:   []domain.Model{{ID: "ultimate"}},
		QoderPAT: &domain.QoderCredential{
			PAT: "pt-secret", RefreshToken: "jrt-secret", DeviceToken: "dt-secret",
			ExpiresAt: "2026-01-01T00:00:00Z", AccountLabel: "user@example.com", Email: "user@example.com",
		},
	})
	encoded, _ := json.Marshal(status)
	for _, secret := range []string{"pt-secret", "jrt-secret", "dt-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("status leaked %q: %s", secret, encoded)
		}
	}
	if status["connected"] != true || status["signInMode"] != qoderSignInModePAT || status["models"] != 1 {
		t.Fatalf("status wrong: %+v", status)
	}
	if qoderStatusOf(domain.Provider{})["connected"] != false {
		t.Fatal("an unsigned-in provider must not read as connected")
	}
}
