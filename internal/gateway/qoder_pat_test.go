package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// qoderTestSite is a Qoder site whose account and inference hosts are the
// caller's stand-in server.
func qoderTestSite(server *httptest.Server) qoderSite {
	return qoderSite{
		AuthType: domain.AuthTypeQoderPAT,
		Name:     "Qoder",
		Web:      server.URL,
		OpenAPI:  server.URL,
		API:      server.URL,
		ClientID: "test-client",
	}
}

// qoderTestHandler answers the two endpoints a personal-access-token sign-in
// asks: the exchange, and the account endpoint that says whose account it is.
func qoderTestHandler(exchange string, userInfo string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/jobToken/exchange":
			_, _ = io.WriteString(w, exchange)
		case "/api/v1/userinfo":
			if userInfo == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, userInfo)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"unreachable in this test"}`)
		}
	}
}

func TestExchangeQoderJobTokenReadsTheAccountItBelongsTo(t *testing.T) {
	server := httptest.NewServer(qoderTestHandler(
		`{"token":"jt-flat","refresh_token":"jrt-1","expires_in":86400000}`,
		`{"id":"uid-1","name":"Example User","email":"user@example.com"}`,
	))
	defer server.Close()

	credential, err := exchangeQoderJobToken(context.Background(), qoderTestSite(server), domain.QoderCredential{}, "pt-secret")
	if err != nil {
		t.Fatalf("exchange failed: %v", err)
	}
	if credential.AccessToken != "jt-flat" {
		t.Errorf("job token = %q", credential.AccessToken)
	}
	// The refresh token Qoder returns is the job token's own, spent on use; the
	// personal access token is carried in its own field, because that is what
	// mints a new pair once the refresh is gone.
	if credential.RefreshToken != "jrt-1" {
		t.Errorf("refresh token = %q", credential.RefreshToken)
	}
	if credential.PAT != "pt-secret" {
		t.Errorf("personal access token = %q", credential.PAT)
	}
	if credential.SignInMode != qoderSignInModePAT {
		t.Errorf("sign-in mode = %q", credential.SignInMode)
	}
	if credential.UID != "uid-1" || credential.Email != "user@example.com" {
		t.Errorf("account identity = %+v", credential)
	}
	if credential.AccountLabel != "user@example.com" {
		t.Errorf("label = %q, want the account Qoder reported", credential.AccountLabel)
	}
	if credential.MachineID == "" {
		t.Error("the envelope needs a machine id, minted once per account")
	}
	expiresAt, err := time.Parse(time.RFC3339, credential.ExpiresAt)
	if err != nil {
		t.Fatalf("expiry %q is not RFC3339: %v", credential.ExpiresAt, err)
	}
	if until := time.Until(expiresAt); until < 23*time.Hour || until > 24*time.Hour {
		t.Fatalf("expiry should sit about a day out (expires_in is milliseconds), got %v", until)
	}
}

// Qoder answers the exchange with the account endpoint's identity only if it is
// asked, and the uid is what the COSY envelope signs with: an account that
// cannot be named cannot be sent a request.
func TestExchangeQoderJobTokenRefusesAnUnnameableAccount(t *testing.T) {
	server := httptest.NewServer(qoderTestHandler(`{"token":"jt-flat","expires_in":1000}`, ""))
	defer server.Close()

	_, err := exchangeQoderJobToken(context.Background(), qoderTestSite(server), domain.QoderCredential{}, "pt-secret")
	if err == nil {
		t.Fatal("expected an error when the account endpoint refuses")
	}
	if !strings.Contains(err.Error(), "无法确认") {
		t.Fatalf("error should say the account could not be identified, got %v", err)
	}
}

func TestExchangeQoderJobTokenErrorsOmitTheToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad credentials"}`)
	}))
	defer server.Close()

	_, err := exchangeQoderJobToken(context.Background(), qoderTestSite(server), domain.QoderCredential{}, "pt-super-secret")
	if err == nil {
		t.Fatal("expected an error for a non-2xx response")
	}
	// Exchange errors flow into request logs, so the token must never appear.
	if strings.Contains(err.Error(), "pt-super-secret") {
		t.Fatalf("error message leaked the token: %v", err)
	}
}

func TestExchangeQoderJobTokenRejectsAnEmptyOrTokenlessAnswer(t *testing.T) {
	if _, err := exchangeQoderJobToken(context.Background(), qoderSite{}, domain.QoderCredential{}, "  "); err == nil {
		t.Fatal("expected an error for an empty token")
	}
	server := httptest.NewServer(qoderTestHandler(`{"success":true}`, `{"id":"uid-1"}`))
	defer server.Close()
	if _, err := exchangeQoderJobToken(context.Background(), qoderTestSite(server), domain.QoderCredential{}, "pt-x"); err == nil {
		t.Fatal("expected an error when the answer carries no token")
	}
}

func TestQoderJobExpiryPrefersWhatQoderSaid(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	cases := []struct {
		name string
		job  qoderJobToken
		want time.Duration
	}{
		{"milliseconds", qoderJobToken{ExpiresIn: 3_600_000}, time.Hour},
		{"absolute date", qoderJobToken{ExpireTime: now.Add(2 * time.Hour).Format(time.RFC3339)}, 2 * time.Hour},
		{"no lifetime at all", qoderJobToken{}, qoderTokenFallbackTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := time.Parse(time.RFC3339, qoderJobExpiry(tc.job, now))
			if err != nil {
				t.Fatal(err)
			}
			// Every branch keeps the skew, so a renewal always happens ahead of
			// the real end.
			if delta := got.Sub(now); delta < tc.want-qoderTokenExpirySkew-time.Second || delta > tc.want {
				t.Fatalf("expiry sits %v out, want about %v minus the skew", delta, tc.want)
			}
		})
	}
}

func TestQoderTokenNeedsRefresh(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	almost := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	cases := []struct {
		name       string
		credential *domain.QoderCredential
		want       bool
	}{
		{"nil credential", nil, true},
		{"missing token", &domain.QoderCredential{RefreshToken: "jrt-1"}, true},
		{"no expiry recorded", &domain.QoderCredential{AccessToken: "jt-1"}, true},
		{"unparseable expiry", &domain.QoderCredential{AccessToken: "jt-1", ExpiresAt: "someday"}, true},
		{"inside the renewal lead", &domain.QoderCredential{AccessToken: "jt-1", ExpiresAt: almost}, true},
		{"comfortably fresh", &domain.QoderCredential{AccessToken: "jt-1", ExpiresAt: future}, false},
		{"already spent", &domain.QoderCredential{AccessToken: "jt-1", ExpiresAt: past}, true},
	}
	for _, tc := range cases {
		if got := qoderTokenNeedsRefresh(tc.credential); got != tc.want {
			t.Errorf("%s: qoderTokenNeedsRefresh = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// An account Qoder named is shown by name; only a token whose account endpoint
// refused falls back to the last four, and never to more of the credential.
func TestQoderAccountLabelFor(t *testing.T) {
	site := qoderGlobalSite
	if got := qoderAccountLabelFor(site, &domain.QoderCredential{Email: "a@b.co", Name: "ignored"}); got != "a@b.co" {
		t.Errorf("email should win, got %q", got)
	}
	if got := qoderAccountLabelFor(site, &domain.QoderCredential{UID: "uid-9"}); got != "uid-9" {
		t.Errorf("uid should be used when nothing else is known, got %q", got)
	}
	const pat = "pt-abcdefghijklmnop9xyz"
	got := qoderAccountLabelFor(site, &domain.QoderCredential{PAT: pat})
	if !strings.HasSuffix(got, "****9xyz") {
		t.Fatalf("expected the last four chars, got %q", got)
	}
	if strings.Contains(got, pat) || strings.Contains(got, pat[:len(pat)-4]) {
		t.Fatalf("label leaked the token: %q", got)
	}
	if label := qoderAccountLabelFor(site, nil); label != "Qoder" {
		t.Fatalf("an unknown account should read as the site, got %q", label)
	}
	if label := qoderAccountLabelFor(qoderCNSite, &domain.QoderCredential{PAT: "abc"}); label != "Qoder CN" {
		t.Fatalf("a too-short token must not be sliced, got %q", label)
	}
}

// Rows written before the browser sign-in existed kept the pasted token in the
// refresh-token column; losing track of it would strand a working connection.
func TestQoderPersonalTokenReadsLegacyRows(t *testing.T) {
	if got := qoderPersonalToken(&domain.QoderCredential{PAT: "pt-modern"}); got != "pt-modern" {
		t.Fatalf("modern row should read its own field, got %q", got)
	}
	if got := qoderPersonalToken(&domain.QoderCredential{RefreshToken: "pt-legacy"}); got != "pt-legacy" {
		t.Fatalf("legacy row should fall back to the refresh column, got %q", got)
	}
	// A device-flow account's refresh token is a job refresh token, not a
	// personal access token: it must never be exchanged as one.
	if got := qoderPersonalToken(&domain.QoderCredential{SignInMode: qoderSignInModeDevice, RefreshToken: "jrt-1"}); got != "" {
		t.Fatalf("device account should report no personal token, got %q", got)
	}
}

// Guard against the redaction trap: without a Qoder branch in
// redactProviderForClient, every token and the raw model listing serialize to
// the browser via /__state.
func TestRedactProviderForClientHidesQoderTokens(t *testing.T) {
	provider := domain.Provider{
		ID:       "qoder",
		Name:     "Qoder",
		AuthType: domain.AuthTypeQoderPAT,
		Protocol: domain.ProtocolOpenAIChat,
		BaseURL:  qoderGlobalSite.API,
		QoderPAT: &domain.QoderCredential{
			AccessToken:    "jt-should-never-be-sent",
			RefreshToken:   "jrt-should-never-be-sent",
			PAT:            "pt-should-never-be-sent",
			DeviceToken:    "dt-should-never-be-sent",
			MachineID:      "machine-not-secret",
			Email:          "user@example.com",
			Models:         json.RawMessage(`{"chat":[{"key":"ultimate"}]}`),
			ExpiresAt:      "2026-01-01T00:00:00Z",
			AccountLabel:   "user@example.com",
			SignInMode:     qoderSignInModeDevice,
			DeviceChat:     true,
			HasStoredToken: true,
		},
	}
	encoded, err := json.Marshal(redactProviderForClient(provider))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"jt-should-never-be-sent", "jrt-should-never-be-sent",
		"pt-should-never-be-sent", "dt-should-never-be-sent", "ultimate",
	} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("redacted provider leaked %q: %s", secret, encoded)
		}
	}
	redacted := redactProviderForClient(provider)
	if !redacted.QoderPAT.Connected {
		t.Error("a signed-in account must read as connected")
	}
	if redacted.QoderPAT.SignInMode != qoderSignInModeDevice {
		t.Errorf("sign-in mode should survive redaction, got %q", redacted.QoderPAT.SignInMode)
	}
	if redacted.QoderPAT.ExpiresAt != "2026-01-01T00:00:00Z" {
		t.Errorf("expiry should survive redaction, got %q", redacted.QoderPAT.ExpiresAt)
	}
	if redacted.QoderPAT.Email != "user@example.com" || !redacted.QoderPAT.DeviceChat {
		t.Errorf("the console needs the account identity, got %+v", redacted.QoderPAT)
	}
}

func TestRedactProviderQoderPATNeverConnected(t *testing.T) {
	redacted := redactProviderForClient(domain.Provider{
		ID: "p1", AuthType: domain.AuthTypeQoderPAT, Protocol: domain.ProtocolOpenAIChat,
		QoderPAT: &domain.QoderCredential{},
	})
	if redacted.QoderPAT.Connected || redacted.QoderPAT.HasStoredToken {
		t.Fatalf("expected no connection state without a credential, got %+v", redacted.QoderPAT)
	}
}

// Disconnect must not wipe the credential — only pause forwarding. Losing it
// forces a trip back to the browser, or to the token page, just to reconnect.
func TestDisconnectProviderQoderPATKeepsToken(t *testing.T) {
	router := NewRouter(domain.GatewayState{Providers: []domain.Provider{{
		ID: "p1", AuthType: domain.AuthTypeQoderPAT, Protocol: domain.ProtocolOpenAIChat,
		BaseURL: qoderGlobalSite.API,
		QoderPAT: &domain.QoderCredential{
			AccessToken: "jt-live", RefreshToken: "jrt-live", PAT: "pt-keepme1234",
			ExpiresAt: "2026-01-01T00:00:00Z", AccountLabel: "user@example.com",
		},
	}}})

	updated, err := router.DisconnectProviderQoderPAT("p1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.QoderPAT == nil {
		t.Fatal("the credential must not be nil after disconnect")
	}
	if updated.QoderPAT.PAT != "pt-keepme1234" || updated.QoderPAT.RefreshToken != "jrt-live" {
		t.Fatalf("credential must survive disconnect, got %+v", updated.QoderPAT)
	}
	if !updated.QoderPAT.Disconnected {
		t.Fatal("expected Disconnected=true")
	}
	if updated.QoderPAT.AccessToken != "" {
		t.Fatalf("the live token should be cleared, got %q", updated.QoderPAT.AccessToken)
	}

	redacted := redactProviderForClient(updated)
	if redacted.QoderPAT.Connected {
		t.Error("a disconnected provider must not read as Connected")
	}
	if !redacted.QoderPAT.HasStoredToken || !redacted.QoderPAT.Disconnected {
		t.Errorf("the console must be able to offer one-click reconnect, got %+v", redacted.QoderPAT)
	}
}

// A paused connection must not silently keep forwarding through the lazy renew
// path — otherwise "断开连接" would do nothing observable.
func TestEnsureFreshQoderTokenRefusesWhenDisconnected(t *testing.T) {
	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	provider := domain.Provider{
		ID: "p1", AuthType: domain.AuthTypeQoderPAT, Protocol: domain.ProtocolOpenAIChat,
		QoderPAT: &domain.QoderCredential{PAT: "pt-x", Disconnected: true},
	}
	if _, err := server.ensureFreshQoderToken(provider); err == nil {
		t.Fatal("expected an error for a disconnected provider")
	} else if !strings.Contains(err.Error(), "登录") {
		t.Fatalf("expected a not-signed-in error, got %v", err)
	}
}

func TestEnsureFreshQoderTokenLeavesAFreshAccountAlone(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	router := NewRouter(domain.GatewayState{Providers: []domain.Provider{{
		ID: "p1", AuthType: domain.AuthTypeQoderPAT, Protocol: domain.ProtocolOpenAIChat,
		QoderPAT: &domain.QoderCredential{
			AccessToken: "jt-live", RefreshToken: "jrt-live",
			ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
	}}})
	gateway := NewServer(router, monitor.NewStore())
	if _, err := gateway.ensureFreshQoderToken(router.State().Providers[0]); err != nil {
		t.Fatalf("a fresh account must pass through untouched: %v", err)
	}
	// The unexpired path performs no network I/O at all: it runs on every
	// proxied request.
	if calls != 0 {
		t.Fatalf("expected no upstream calls, got %d", calls)
	}
}

func TestQoderLiteralPAT(t *testing.T) {
	cases := []struct {
		name         string
		apiKeySource string
		want         string
	}{
		{"empty", "", ""},
		{"plain pt-", "pt-abc123", "pt-abc123"},
		{"literal prefix", "literal:pt-abc123", "pt-abc123"},
		{"literal prefix with space", " literal:  pt-abc123 ", "pt-abc123"},
		{"env source is dynamic", "env:QODER_PAT", ""},
		{"keychain source is dynamic", "keychain:qoder", ""},
		{"non-token plain source", "sk-not-a-qoder-token", ""},
	}
	for _, tc := range cases {
		got := qoderLiteralPAT(domain.Provider{APIKeySource: tc.apiKeySource})
		if got != tc.want {
			t.Fatalf("%s: qoderLiteralPAT(%q) = %q, want %q", tc.name, tc.apiKeySource, got, tc.want)
		}
	}
}

func TestNormalizeReasoningEffortKeepsMax(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		if got := normalizeReasoningEffort(effort); got != effort {
			t.Errorf("normalizeReasoningEffort(%q) = %q, want %q", effort, got, effort)
		}
	}
	for _, off := range []string{"", "none", "off", "disabled"} {
		if got := normalizeReasoningEffort(off); got != "" {
			t.Errorf("normalizeReasoningEffort(%q) = %q, want empty", off, got)
		}
	}
}

// Qoder reports failures as an `event: error` frame whose data payload is a
// bare error object with no "error" wrapper. Without event-name tracking the
// frame is skipped as an ordinary chunk and the real message ("Unsupported
// model …") is replaced by the useless "stream ended without any chunks".
func TestChatToResponsesStreamSurfacesBareErrorEvent(t *testing.T) {
	sse := "event: error\n" +
		`data: {"code":"invalid_model_error","message":"Unsupported model \"qwen3.8-max-preview\"","type":"invalid_model_error"}` + "\n\n"

	recorder := httptest.NewRecorder()
	_, err := streamOpenAIChatToResponsesEvents(recorder, strings.NewReader(sse), "qwen3.8-max-preview")
	if err == nil {
		t.Fatal("expected an error for an error-event stream")
	}
	if !strings.Contains(err.Error(), "Unsupported model") {
		t.Fatalf("error should carry the upstream message, got: %v", err)
	}
	if strings.Contains(err.Error(), "without any chunks") {
		t.Fatalf("upstream message was masked by the empty-stream error: %v", err)
	}
}

func TestChatToClaudeStreamSurfacesBareErrorEvent(t *testing.T) {
	sse := "event: error\n" +
		`data: {"code":"invalid_model_error","message":"Unsupported model \"nope\"","type":"invalid_model_error"}` + "\n\n"

	recorder := httptest.NewRecorder()
	_, err := streamOpenAIChatToClaudeEvents(recorder, strings.NewReader(sse), "nope")
	if err == nil {
		t.Fatal("expected an error for an error-event stream")
	}
	if !strings.Contains(err.Error(), "Unsupported model") {
		t.Fatalf("error should carry the upstream message, got: %v", err)
	}
}
