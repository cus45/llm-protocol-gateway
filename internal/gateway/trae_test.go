package gateway

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

func TestNewTraeDeviceKeepsItsShape(t *testing.T) {
	deviceID, machineID, publicKey, privateKey, err := newTraeDevice()
	if err != nil {
		t.Fatal(err)
	}
	// Trae's own clients send a 19-digit device id and a 32-character hex
	// machine id; anything else reads to Trae as a new device, and Trae has been
	// seen to drop requests whose device keeps changing.
	if !regexp.MustCompile(`^[1-9][0-9]{18}$`).MatchString(deviceID) {
		t.Errorf("device id %q is not 19 digits that do not start with zero", deviceID)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(machineID) {
		t.Errorf("machine id %q is not 32 hex characters", machineID)
	}
	block, _ := pem.Decode([]byte(publicKey))
	if block == nil || block.Type != "PUBLIC KEY" {
		t.Fatal("public key is not a PEM PUBLIC KEY block")
	}
	raw, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("public key does not parse: %v", err)
	}
	if _, ok := raw.(*ecdsa.PublicKey); !ok {
		t.Errorf("public key is %T, want ECDSA", raw)
	}
	if _, err := traePrivateKey(privateKey); err != nil {
		t.Errorf("private key does not round-trip: %v", err)
	}
}

// The renewal's proof is a signature over an exact string. If the joining or
// the encoding drifts, Trae answers with nothing useful, so the pair is checked
// against the public key the account holds.
func TestTraeProofVerifiesAgainstTheDeviceKey(t *testing.T) {
	_, _, publicKey, privateKey, err := newTraeDevice()
	if err != nil {
		t.Fatal(err)
	}
	key, err := traePrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	const refresh = "refresh-token"
	const clientID = "en1oxy7wnw8j9n"
	at := int64(1792000000)
	nonce := "0123456789abcdef0123456789abcdef"
	proof, err := traeProofOf(key, traeBoundTokenPath, clientID, refresh, at, nonce)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(publicKey))
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	public := parsed.(*ecdsa.PublicKey)
	sign := func(parts ...string) []byte {
		digest := sha256.Sum256([]byte(strings.Join(parts, "\n")))
		return digest[:]
	}
	der, err := base64.StdEncoding.DecodeString(proof)
	if err != nil {
		t.Fatalf("proof is not base64: %v", err)
	}
	if !ecdsa.VerifyASN1(public, sign("POST", traeBoundTokenPath, clientID, refresh, "1792000000", nonce), der) {
		t.Error("the proof does not verify over the string the renewal documents")
	}
	if ecdsa.VerifyASN1(public, sign("POST", traeBoundTokenPath, "other-client", refresh, "1792000000", nonce), der) {
		t.Error("one proof passed for another client")
	}
	if ecdsa.VerifyASN1(public, sign("POST", "/trae/api/v4/oauth/ExchangeToken", clientID, refresh, "1792000000", nonce), der) {
		t.Error("one proof passed for another path")
	}
}

func TestTraeAuthorizeURLCarriesWhatThePageAsks(t *testing.T) {
	called := "http://127.0.0.1:53211/authorize"
	raw := traeAuthorizeURL(traeSites["trae-cn"], called, "1234567890123456789", strings.Repeat("ab", 16))
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "www.trae.cn" || parsed.Path != "/authorization" {
		t.Errorf("authorize page = %q", raw)
	}
	query := parsed.Query()
	for _, key := range []string{"login_version", "auth_from", "login_channel", "plugin_version", "auth_type", "client_id", "auth_callback_url", "machine_id", "device_id", "x_device_id", "x_machine_id", "x_app_version", "x_app_type"} {
		if query.Get(key) == "" {
			t.Errorf("the page was asked without %s", key)
		}
	}
	if query.Get("auth_callback_url") != called {
		t.Errorf("callback = %q, want the loopback address being listened on", query.Get("auth_callback_url"))
	}
	if query.Get("client_id") != traeClientID {
		t.Errorf("client_id = %q", query.Get("client_id"))
	}
	// The page's app version is the IDE's, not the client version a request
	// header carries.
	if query.Get("x_app_version") != traeSites["trae-cn"].ideVer {
		t.Errorf("x_app_version = %q, want the realm's IDE version", query.Get("x_app_version"))
	}
	if !strings.Contains(traeAuthorizeURL(traeSites["trae-global"], called, "1", "2"), "www.trae.ai") {
		t.Error("Trae Global was sent to the China page")
	}
}

// The tokens arrive as two JSON documents inside the query string, with field
// names that differ between page versions.
func TestTraeCredentialFromCallback(t *testing.T) {
	jwt := `{"Token":"cloud-ide-jwt","RefreshToken":"refresh-1","TokenExpireAt":1792000000,"ClientID":"ono9krqynydwx5"}`
	info := `{"UserID":"u-42","ScreenName":"郭庆","Host":"https://coresg-normal.trae.ai","AIRegion":"US"}`
	credential, err := traeCredentialFromCallback(url.Values{"userJwt": {jwt}, "userInfo": {info}}, domain.TraeCredential{DeviceID: "123", MachineID: "456"})
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "cloud-ide-jwt" || credential.RefreshToken != "refresh-1" {
		t.Errorf("tokens = %q / %q", credential.AccessToken, credential.RefreshToken)
	}
	if credential.UserID != "u-42" || credential.AccountLabel != "郭庆" {
		t.Errorf("account = %+v", credential)
	}
	if credential.DeviceID != "123" {
		t.Error("the device made for the sign-in was dropped")
	}
	if credential.APIHost != "https://coresg-normal.trae.ai" {
		t.Errorf("api host = %q", credential.APIHost)
	}
	if credential.Region != "US" {
		t.Errorf("region = %q, want the deployment the account is in", credential.Region)
	}
	if expires, err := time.Parse(time.RFC3339, credential.ExpiresAt); err != nil || expires.Unix() != 1792000000 {
		t.Errorf("expiry = %q", credential.ExpiresAt)
	}

	// Older pages spell the fields in camelCase and hand the refresh token in
	// the query instead.
	camel, err := traeCredentialFromCallback(url.Values{
		"userJwt":      {`{"token":"jwt-2","refreshToken":"r-2","tokenExpireAt":1792000000}`},
		"userInfo":     {`{"userId":"u-2"}`},
		"refreshToken": {""},
	}, domain.TraeCredential{})
	if err != nil {
		t.Fatal(err)
	}
	if camel.AccessToken != "jwt-2" || camel.RefreshToken != "r-2" || camel.UserID != "u-2" {
		t.Errorf("camelCase answer parsed as %+v", camel)
	}

	for name, values := range map[string]url.Values{
		"no token":   {"userInfo": {`{"UserID":"u"}`}},
		"bad json":   {"userJwt": {`{`}},
		"no refresh": {"userJwt": {`{"Token":"only-a-token"}`}},
	} {
		if _, err := traeCredentialFromCallback(values, domain.TraeCredential{}); err == nil {
			t.Errorf("%s: the callback was accepted", name)
		}
	}
}

func TestTraeJWTExpiryAndRenewalTiming(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1792000000}`))
	at, ok := traeJWTExpiry("eyJhbGciOiJIUzI1NiJ9." + payload + ".sig")
	if !ok || at.Unix() != 1792000000 {
		t.Fatalf("exp = %v (%v), want 1792000000", at, ok)
	}
	for _, bad := range []string{"", "a.b", "a.b.c.d", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".c"} {
		if _, ok := traeJWTExpiry(bad); ok {
			t.Errorf("accepted %q as a token with an expiry", bad)
		}
	}
	sooner := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	if !traeTokenNeedsRefresh(&domain.TraeCredential{ExpiresAt: sooner}) {
		t.Error("a token ending in five minutes was not renewed; the lead is ten")
	}
	later := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	if traeTokenNeedsRefresh(&domain.TraeCredential{ExpiresAt: later}) {
		t.Error("a fresh token was renewed on every request")
	}
	// No recorded expiry at all: asking with a dead token is the more expensive
	// mistake, so it renews.
	if !traeTokenNeedsRefresh(&domain.TraeCredential{}) {
		t.Error("a credential with no expiry was left alone")
	}
}

func TestApplyTraeHeaders(t *testing.T) {
	provider := domain.Provider{AuthType: domain.AuthTypeTraeCN, Trae: &domain.TraeCredential{
		AccessToken: "jwt", UserID: "u-1", DeviceID: "123", MachineID: "456",
	}}
	request, _ := http.NewRequest(http.MethodPost, traeSites["trae-cn"].api+traeChatPath, nil)
	applyTraeHeaders(request, provider)
	if got := request.Header.Get("Authorization"); got != "Cloud-IDE-JWT jwt" {
		t.Errorf("Authorization = %q, want the IDE's own scheme", got)
	}
	for _, key := range []string{"X-Cloudide-Token", "x-ide-token", "x-uid", "x-app-id", "x-device-id", "x-machine-id", "x-ide-version", "x-app-version", "x-ide-version-code"} {
		if request.Header.Get(key) == "" {
			t.Errorf("%s was not sent", key)
		}
	}
	// A model is offered only to a client new enough for it, so the version
	// headers are load-bearing rather than decorative.
	if request.Header.Get("x-ide-version") != traeClientVersion {
		t.Errorf("x-ide-version = %q, want the pinned client release", request.Header.Get("x-ide-version"))
	}
	if request.Header.Get("x-device-id") != "123" || request.Header.Get("x-machine-id") != "456" {
		t.Errorf("device = %s/%s, want the account's own", request.Header.Get("x-device-id"), request.Header.Get("x-machine-id"))
	}
	paused := domain.Provider{AuthType: domain.AuthTypeTraeCN, Trae: &domain.TraeCredential{AccessToken: "jwt", Disconnected: true}}
	quiet, _ := http.NewRequest(http.MethodPost, traeSites["trae-cn"].api+traeChatPath, nil)
	applyTraeHeaders(quiet, paused)
	if quiet.Header.Get("Authorization") != "" {
		t.Error("a paused account signed its request")
	}
}

// Trae's agent protocol has no turn for a tool call or its result, so both go
// in as text and the conversation stays one the model can read.
func TestTraeMessages(t *testing.T) {
	raw := json.RawMessage(`[
	  {"role":"developer","content":"be terse"},
	  {"role":"user","content":[{"type":"text","text":"look at src"},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]},
	  {"role":"assistant","content":"reading","tool_calls":[{"id":"call-1","function":{"name":"read","arguments":"{\"filePath\":\"a\"}"}}]},
	  {"role":"tool","tool_call_id":"call-1","content":"file body"}
	]`)
	messages := traeMessages(raw)
	if len(messages) != 4 {
		t.Fatalf("got %d messages: %v", len(messages), messages)
	}
	text := func(index int) string {
		parts, _ := messages[index]["content"].([]map[string]any)
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			out = append(out, part["text"].(string))
		}
		return strings.Join(out, "\n")
	}
	if messages[0]["role"] != "system" {
		t.Errorf("first role = %v, a developer turn is sent as the system one", messages[0]["role"])
	}
	if !strings.Contains(text(1), "[an image, which this model can't see]") {
		t.Errorf("the image was not marked in text: %q", text(1))
	}
	if !strings.Contains(text(2), "<tool_call>") || !strings.Contains(text(2), "\"name\":\"read\"") {
		t.Errorf("the assistant's call was not carried in its turn: %q", text(2))
	}
	if messages[3]["role"] != "user" || !strings.Contains(text(3), "file body") {
		t.Errorf("the tool result went in as %v / %q, want a user turn carrying it", messages[3]["role"], text(3))
	}
}

func TestTraeChatBodyShapesWhatTraeRequires(t *testing.T) {
	provider := domain.Provider{
		AuthType:     domain.AuthTypeTraeCN,
		Trae:         &domain.TraeCredential{UserID: "u-1"},
		DefaultModel: "glm-5.2",
		Models:       []domain.Model{{ID: "glm-5.2", MaxOutputTokens: 32000}},
	}
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"max_tokens":9000,"tools":[{"type":"function","function":{"name":"bash","description":"run","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}]}`)
	encoded, err := traeChatBody(provider, body, "chat_v3")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"messages", "function", "config_name", "model", "stream", "request_id", "session_id"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("%s was not sent; Trae requires it on every request", key)
		}
	}
	if payload["function"] != "chat_v3" || payload["model"] != "glm-5.2" || payload["config_name"] != "glm-5.2" {
		t.Errorf("function/model = %v / %v / %v", payload["function"], payload["model"], payload["config_name"])
	}
	if stream, _ := payload["stream"].(bool); !stream {
		t.Error("Trae only answers a streaming request")
	}
	if limit, _ := payload["max_tokens"].(float64); limit != 9000 {
		t.Errorf("max_tokens = %v, want what was asked", payload["max_tokens"])
	}
	// A request twice as long as the model can answer is cut to what it can.
	long := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"max_tokens":900000}`)
	shaped, _ := traeChatBody(provider, long, "chat_v3")
	var cut map[string]any
	_ = json.Unmarshal(shaped, &cut)
	if limit, _ := cut["max_tokens"].(float64); limit != 32000 {
		t.Errorf("max_tokens = %v, want the model's own ceiling", cut["max_tokens"])
	}
	// Trae's typed field rejects an object: parameters go as a JSON string.
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", payload["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if _, ok := fn["parameters"].(string); !ok {
		t.Errorf("parameters went as %T, want a JSON string", fn["parameters"])
	}
	if !json.Valid([]byte(fn["parameters"].(string))) {
		t.Errorf("parameters string is not JSON: %v", fn["parameters"])
	}
}

func TestTraeSessionIDIsStablePerConversation(t *testing.T) {
	provider := domain.Provider{ID: "trae", AuthType: domain.AuthTypeTraeCN, Trae: &domain.TraeCredential{UserID: "u-1"}}
	first := json.RawMessage(`[{"role":"user","content":"start"},{"role":"assistant","content":"a"}]`)
	later := json.RawMessage(`[{"role":"user","content":"start"},{"role":"assistant","content":"a"},{"role":"user","content":"and then"}]`)
	a, b := traeSessionID(provider, first), traeSessionID(provider, later)
	if a != b {
		t.Errorf("one conversation got %q then %q; Trae would count them as two", a, b)
	}
	other := traeSessionID(provider, json.RawMessage(`[{"role":"user","content":"different"}]`))
	if other == a {
		t.Error("two conversations shared one session id")
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(a) {
		t.Errorf("session id %q is not the v4 shape Trae's clients send", a)
	}
}

// The whole point of the reply conversion: Trae's events become the chunks an
// OpenAI client reads, in order, with the usage and the end where OpenAI puts
// them.
func TestTraeChunkReader(t *testing.T) {
	upstream := "event: output\n" +
		`data: {"response":"Hel"}` + "\n\n" +
		": keep-alive\n\n" +
		"event: output\n" +
		`data: {"delta":{"content":"lo","reasoning_content":"thinking"}}` + "\n\n" +
		"event: token_usage\n" +
		`data: {"usage":{"prompt_tokens":11,"completion_tokens":7,"cache_read_input_tokens":3}}` + "\n\n" +
		"event: done\n" +
		`data: {"finish_reason":"stop"}` + "\n\n"
	got := readAllTraeChunks(t, upstream)
	if !strings.Contains(got, `"content":"Hel"`) || !strings.Contains(got, `"content":"lo"`) {
		t.Errorf("text did not come through:\n%s", got)
	}
	if !strings.Contains(got, `"reasoning_content":"thinking"`) {
		t.Errorf("reasoning did not come through:\n%s", got)
	}
	if !strings.Contains(got, `"role":"assistant"`) {
		t.Errorf("no role delta was sent:\n%s", got)
	}
	if !strings.Contains(got, `"object":"chat.completion.chunk"`) {
		t.Errorf("chunks are not OpenAI's:\n%s", got)
	}
	if !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Errorf("the reply did not end with a finish:\n%s", got)
	}
	if !strings.Contains(got, `"total_tokens":18`) {
		t.Errorf("usage did not ride on the finish chunk:\n%s", got)
	}
	if !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Errorf("the stream did not end with [DONE]: %q", got)
	}
	if strings.Contains(got, "keep-alive") {
		t.Error("a comment line reached the client")
	}
}

func TestTraeChunkReaderToolCalls(t *testing.T) {
	upstream := "event: output\n" +
		`data: {"tool_calls":[{"id":"c1","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]}` + "\n\n" +
		"event: done\n`data: {}`\n\n"
	got := readAllTraeChunks(t, upstream)
	if !strings.Contains(got, `"name":"bash"`) || !strings.Contains(got, "ls") {
		t.Errorf("the call did not come through:\n%s", got)
	}
	if !strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Errorf("a reply that called a tool did not say so:\n%s", got)
	}
}

func TestTraeChunkReaderError(t *testing.T) {
	upstream := "event: error\n" + `data: {"code":4008,"message":"额度已用完"}` + "\n\n"
	got := readAllTraeChunks(t, upstream)
	if !strings.Contains(got, "额度已用完") || !strings.Contains(got, "Trae CN") {
		t.Errorf("the refusal did not reach the client:\n%s", got)
	}
}

// A reply that runs out of tokens must not look like a finished answer.
func TestTraeFinishReason(t *testing.T) {
	cases := map[string]string{
		`{"finish_reason":"stop"}`:            "stop",
		`{"finish_reason":"length"}`:          "length",
		`{"finish_reason":"max_tokens"}`:      "length",
		`{"stop_reason":"MAX_OUTPUT_TOKENS"}`: "length",
		`{"finish_reason":"content_filter"}`:  "content_filter",
		`{"finishReason":"tool_calls"}`:       "tool_calls",
		`{}`:                                  "",
	}
	for raw, want := range cases {
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatal(err)
		}
		if got := traeFinishReason(payload); got != want {
			t.Errorf("%s -> %q, want %q", raw, got, want)
		}
	}
}

func readAllTraeChunks(t *testing.T, upstream string) string {
	t.Helper()
	reader := newTraeChunkReader(strings.NewReader(upstream), traeSites["trae-cn"], "glm-5.2")
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading the converted stream: %v", err)
	}
	return string(body)
}

func TestTraeErrorClassification(t *testing.T) {
	signedIn := &traeError{Message: "未登录", Code: "1001", Status: http.StatusOK}
	if !traeErrorMeansSignIn(signedIn) {
		t.Error("code 1001 did not mark the account for a new sign-in")
	}
	lapsed := &traeError{Message: "expired", Status: http.StatusUnauthorized}
	if !traeErrorMeansSignIn(lapsed) {
		t.Error("a 401 did not mark the account for a new sign-in")
	}
	global := &traeError{Message: "no session", Code: "20101"}
	if !traeErrorMeansSignIn(global) {
		t.Error("Trae Global's 20101 did not mark the account for a new sign-in")
	}
	quota := &traeError{Message: "额度", Code: "4008"}
	if !traeErrorMeansQuota(quota, traeSites["trae-cn"]) {
		t.Error("code 4008 was not read as a spent allowance")
	}
	if !wrongTraeFunction("4023") || wrongTraeFunction("4008") {
		t.Error("the retryable function codes were not told from the rest")
	}
}

func TestTraeFunctionsAreTriedInOrderAndRemembered(t *testing.T) {
	fresh := domain.Provider{AuthType: domain.AuthTypeTraeCN, Trae: &domain.TraeCredential{}}
	if got := traeFunctionsToTry(fresh); len(got) != 4 || got[0] != "chat_v3" {
		t.Errorf("an account starts at %v, want the realm's order from chat_v3", got)
	}
	remembered := domain.Provider{AuthType: domain.AuthTypeTraeCN, Trae: &domain.TraeCredential{ChatFunction: "solo_agent"}}
	if got := traeFunctionsToTry(remembered); got[0] != "solo_agent" {
		t.Errorf("a remembered function was not asked first: %v", got)
	}
	if again := traeFunctionsToTry(remembered); len(again) != 4 {
		t.Errorf("the other functions were dropped: %v", again)
	}
}

func TestNormalizeTraeProviders(t *testing.T) {
	cn := normalizeProviderForTest(domain.Provider{Name: "Trae CN", AuthType: domain.AuthTypeTraeCN, BaseURL: "https://example.com"})
	if cn.Protocol != domain.ProtocolOpenAIChat {
		t.Errorf("protocol = %s", cn.Protocol)
	}
	if cn.BaseURL != traeSites["trae-cn"].api {
		t.Errorf("base = %q, want the realm's model host", cn.BaseURL)
	}
	if got := resolveProviderChatURL(cn, "glm-5.2"); got != traeSites["trae-cn"].api+traeChatPath {
		t.Errorf("chat url = %q, want Trae's agent endpoint", got)
	}
	global := normalizeProviderForTest(domain.Provider{Name: "Trae", AuthType: domain.AuthTypeTraeGlobal, BaseURL: "https://example.com"})
	if global.BaseURL != traeSites["trae-global"].api {
		t.Errorf("Global base = %q", global.BaseURL)
	}
}

// A sign-in may name a host of its own (an enterprise deployment, another
// region). It is honoured only when it is Trae's own, over https, and nothing
// else — a token must never be sent to an address the page slipped in.
func TestTraeOwnHostOnlyAcceptsTrae(t *testing.T) {
	site := traeSites["trae-cn"]
	for _, allowed := range []string{"https://api.trae.cn", "https://trae-api-cn.mchost.guru", "https://trae-api-cn.mchost.guru/api/agent", "https://coresg-normal.trae.ai"} {
		if got := traeOwnHost(allowed, site); got == "" {
			t.Errorf("%q was refused although it is Trae's own", allowed)
		}
	}
	for _, bad := range []string{"", "http://api.trae.cn", "https://evil.com/", "https://trae.cn.evil.com", "https://attacker.com/?h=trae.cn", "https://user:pass@api.trae.cn", "https://api.trae.cn:8443", "https://api.trae.cn/#back", "api.trae.cn"} {
		if got := traeOwnHost(bad, site); got != "" {
			t.Errorf("%q was accepted as Trae's own: %q", bad, got)
		}
	}
}

func TestTraeWindowsOfAnswer(t *testing.T) {
	cn := map[string]any{"is_credits_billing": true, "user_entitlement_pack_list": []any{
		map[string]any{
			"entitlement_base_info": map[string]any{"quota": map[string]any{"credits_limit": 1000.0}, "end_time": 1792000000.0},
			"usage":                 map[string]any{"credits_amount": 250.0},
		},
		map[string]any{
			"entitlement_base_info": map[string]any{"quota": map[string]any{"credits_limit": 1000.0}, "end_time": 1791000000.0},
			"usage":                 map[string]any{"credits_amount": 100.0},
		},
	}}
	windows, plan, unlimited := traeWindowsOf(cn, traeSites["trae-cn"])
	if unlimited {
		t.Error("a metered account read as unlimited")
	}
	if len(windows) != 1 {
		t.Fatalf("windows = %v, want the two packs added up into one", windows)
	}
	if windows[0].Limit != 2000 || windows[0].Amount != 350 || windows[0].Used != 17.5 {
		t.Errorf("window = %+v, want 350 of 2000 used", windows[0])
	}
	if windows[0].Unit != "credits" {
		t.Errorf("unit = %q", windows[0].Unit)
	}
	// The card resets when the soonest pack ends.
	if soonest, _ := time.Parse(time.RFC3339, windows[0].ResetsAt); soonest.Unix() != 1791000000 {
		t.Errorf("resets at %q, want the soonest pack end", windows[0].ResetsAt)
	}
	_ = plan

	// -1 means the plan does not meter at all.
	_, _, free := traeWindowsOf(map[string]any{"user_entitlement_pack_list": []any{
		map[string]any{"entitlement_base_info": map[string]any{"quota": map[string]any{"credits_limit": -1.0}}},
	}}, traeSites["trae-cn"])
	if !free {
		t.Error("an unmetered pack was read as a quota")
	}

	global := map[string]any{"user_entitlement_pack_list": []any{
		map[string]any{
			"display_desc":          "Pro",
			"entitlement_base_info": map[string]any{"quota": map[string]any{"basic_usage_limit": 20.0, "bonus_usage_limit": 5.0}, "end_time": "2026-11-01 00:00:00"},
			"usage":                 map[string]any{"basic_usage_amount": 10.0, "bonus_usage_amount": 1.0},
		},
	}}
	globalWindows, globalPlan, _ := traeWindowsOf(global, traeSites["trae-global"])
	if len(globalWindows) != 1 || globalWindows[0].Unit != "usd" || globalWindows[0].Limit != 25 || globalWindows[0].Amount != 11 {
		t.Errorf("dollar windows = %+v, want 11 of 25 usd", globalWindows)
	}
	if globalPlan != "Pro" {
		t.Errorf("plan = %q, want the pack's own name", globalPlan)
	}
	if resets, err := time.Parse(time.RFC3339, globalWindows[0].ResetsAt); err != nil || resets.Year() != 2026 {
		t.Errorf("resets at %q, read from a Beijing date string", globalWindows[0].ResetsAt)
	}
}

func TestTraeModelHostIsUsedWhenItIsTraesOwn(t *testing.T) {
	provider := domain.Provider{AuthType: domain.AuthTypeTraeCN, Trae: &domain.TraeCredential{
		APIHost:  "https://trae-api-cn.mchost.guru",
		AuthHost: "https://evil.example",
	}}
	site, ok := traeSiteOf(provider)
	if !ok {
		t.Fatal("a Trae provider resolved to no realm")
	}
	if site.api != "https://trae-api-cn.mchost.guru" {
		t.Errorf("model host = %q, want the one the sign-in named", site.api)
	}
	if site.auth != traeSites["trae-cn"].auth {
		t.Errorf("token host = %q, want the realm's own after refusing the address that is not Trae's", site.auth)
	}
}
