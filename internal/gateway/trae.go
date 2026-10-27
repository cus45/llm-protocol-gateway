package gateway

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// Trae is ByteDance's AI IDE, and its subscription is served over the IDE's own
// protocol rather than an OpenAI-compatible one. A request is signed with the
// account's Cloud-IDE-JWT and the device that JWT was issued for, goes to one
// agent function of the IDE's, and answers in Trae's own SSE events. So a Trae
// provider is a translation on both ends, not a relay.
//
// Two realms, two connection kinds: Trae CN (trae.cn) and Trae Global
// (trae.ai). They share the request machinery and differ in a table — hosts,
// client versions, the fallback model list, the billing page and the codes that
// mean "sign in again".

const (
	traeChatPath        = "/api/agent/v3/llm_utils_chat"
	traeModelsBatchPath = "/api/ide/v1/batch_get_detail_param"
	traeModelsPath      = "/api/ide/v1/get_detail_param"
	traeBoundTokenPath  = "/trae/api/v3/oauth/ExchangeToken"
	traeTokenPath       = "/cloudide/api/v3/trae/oauth/ExchangeToken"

	traeClientID = "ono9krqynydwx5"
	traeAppID    = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	// traeClientVersion is what the request headers say the client is. Trae
	// offers a model only to a client new enough for it, so this is load-bearing
	// in a way an app version normally is not.
	traeClientVersion      = "0.1.69"
	traeClientVersionCode  = "20260917"
	traeDeviceBrand        = "ASUS TUF Gaming A15 FA507RM_FA507RM"
	traeMaxOutputDefault   = 32000
	traeRenewLead          = 10 * time.Minute
	traeSignInDeadline     = 10 * time.Minute
	traeCallTimeout        = 20 * time.Second
	traeStreamHeaderStatus = "X-Trae-Sign-In"
)

// traeSite is one Trae realm.
type traeSite struct {
	id    string
	label string
	// web is the authorization page, auth the token service, api the model
	// host, pay the billing page. us/usPay are Global's second deployment.
	web       string
	auth      string
	api       string
	pay       string
	us        string
	usPay     string
	device    bool // CN renews bound to the device; Global only has the legacy call
	ideVer    string
	pluginVer string
	functions []string
	models    []traeModel
	// lapsed and quota are the codes that mean "sign in again" and "no
	// allowance"; they are compared as both a number and a string, because Trae
	// answers them either way.
	lapsed []string
	quota  []string
}

type traeModel struct {
	id      string
	name    string
	context int
	output  int
}

var traeSites = map[string]traeSite{
	"trae-cn": {
		id:        "trae-cn",
		label:     "Trae CN",
		web:       "https://www.trae.cn",
		auth:      "https://api.trae.cn",
		api:       "https://trae-api-cn.mchost.guru",
		pay:       "https://api.trae.cn",
		device:    true,
		ideVer:    "3.3.65",
		pluginVer: "2.3.24254",
		functions: []string{"chat_v3", "solo_work_lite", "solo_agent", "solo_agent_lite"},
		models: []traeModel{
			{"glm-5.2", "GLM-5.2", 200000, 32000},
			{"glm-5", "GLM-5", 128000, 32000},
			{"kimi-k2.6", "Kimi K2.6", 256000, 32000},
			{"qwen-3.7-plus", "Qwen 3.7 Plus", 128000, 32000},
			{"DeepSeek-V4-Pro", "DeepSeek V4 Pro", 128000, 32000},
			{"DeepSeek-V4-Flash", "DeepSeek V4 Flash", 128000, 32000},
		},
		lapsed: []string{"1001", "401"},
		quota:  []string{"4008", "1005"},
	},
	"trae-global": {
		id:        "trae-global",
		label:     "Trae Global",
		web:       "https://www.trae.ai",
		auth:      "https://growsg-normal.trae.ai",
		api:       "https://coresg-normal.trae.ai",
		pay:       "https://api-sg-central.trae.ai",
		us:        "https://coreva-normal.trae.ai",
		usPay:     "https://api-us-east.trae.ai",
		ideVer:    "3.5.51",
		pluginVer: "2.3.62834",
		functions: []string{"chat_v3", "solo_work_lite", "solo_agent", "solo_agent_lite"},
		models: []traeModel{
			{"gpt-5.4", "GPT-5.4", 128000, 32000},
			{"gemini-3-flash", "Gemini 3 Flash", 1000000, 32000},
			{"kimi-k2.5", "Kimi K2.5", 256000, 32000},
			{"minimax-m2", "MiniMax M2", 128000, 32000},
			{"deepseek-v3.2", "DeepSeek V3.2", 128000, 32000},
		},
		lapsed: []string{"1001", "20101", "401"},
		quota:  []string{"4008", "1005", "4011"},
	},
}

// traeDeviceExchangeClient is a client Trae CN's device-bound renewal asks as.
type traeDeviceExchangeClient struct {
	clientID string
	platform string
	version  string
}

// traeDeviceClients, in the order Trae's own clients were seen to work: SOLO
// first, then the IDE.
var traeDeviceClients = []traeDeviceExchangeClient{
	{"en1oxy7wnw8j9n", "SOLO_PC", "0.1.64"},
	{"ono9krqynydwx5", "IDE_PC", "3.3.99"},
}

func traeSiteForAuthType(authType string) (traeSite, bool) {
	switch authType {
	case domain.AuthTypeTraeCN:
		return traeSites["trae-cn"], true
	case domain.AuthTypeTraeGlobal:
		return traeSites["trae-global"], true
	}
	return traeSite{}, false
}

func isTraeAuthType(authType string) bool {
	return authType == domain.AuthTypeTraeCN || authType == domain.AuthTypeTraeGlobal
}

func isTraeProvider(provider domain.Provider) bool {
	return isTraeAuthType(provider.AuthType)
}

func traeSiteOf(provider domain.Provider) (traeSite, bool) {
	site, ok := traeSiteForAuthType(provider.AuthType)
	if !ok {
		return traeSite{}, false
	}
	// An enterprise or per-region sign-in names its own hosts; they are only
	// trusted when they are Trae's own, on the same realm.
	if provider.Trae != nil {
		if host := traeOwnHost(provider.Trae.APIHost, site); host != "" {
			site.api = host
		}
		if host := traeOwnHost(provider.Trae.AuthHost, site); host != "" {
			site.auth = host
		}
	}
	return site, true
}

// traeDomains are the hosts Trae's own deployments live on. A sign-in may name
// another host for an account (an enterprise tenant, a regional gateway), and
// its token follows that host — so only these are ever accepted, and only over
// https with nothing else in the address.
var traeDomains = []string{"trae.cn", "trae.ai", "mchost.guru"}

func traeOwnHost(raw string, site traeSite) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return ""
	}
	if parsed.User != nil || parsed.Port() != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	allowed := false
	for _, domain := range traeDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			allowed = true
			break
		}
	}
	if !allowed {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host + strings.TrimRight(parsed.Path, "/")
}

// traeChatURL is where a Trae request goes. It takes the place of the
// OpenAI-completions path a relay would append.
func traeChatURL(provider domain.Provider) string {
	site, ok := traeSiteOf(provider)
	if !ok {
		return ""
	}
	return site.api + traeChatPath
}

// ---- the device ------------------------------------------------------------

// newTraeDevice makes the identity a Trae account is bound to. Trae has been
// seen to drop requests whose device keeps changing, so one device is made at
// sign-in and kept with the account forever.
func newTraeDevice() (deviceID, machineID, publicKey, privateKey string, err error) {
	digits := make([]byte, 19)
	if _, err = rand.Read(digits); err != nil {
		return "", "", "", "", err
	}
	for i := range digits {
		digits[i] = byte('0' + digits[i]%10)
	}
	// A device id does not start with a zero.
	if digits[0] == '0' {
		digits[0] = '7'
	}
	deviceID = string(digits)
	raw := make([]byte, 16)
	if _, err = rand.Read(raw); err != nil {
		return "", "", "", "", err
	}
	machineID = hex.EncodeToString(raw)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", "", "", err
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", "", "", err
	}
	publicKey = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", "", "", err
	}
	privateKey = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
	return deviceID, machineID, publicKey, privateKey, nil
}

// traePrivateKey decodes the stored PEM back into a signing key.
func traePrivateKey(encoded string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(encoded))
	if block == nil {
		return nil, errors.New("Trae 设备密钥无法解析")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("Trae 设备密钥不是 ECDSA 私钥")
	}
	return key, nil
}

// traeProofOf is the signature Trae CN asks a renewal to carry: the ECDSA
// signature of the method, the path, the client, the refresh token, the
// timestamp and a nonce, joined by newlines.
func traeProofOf(key *ecdsa.PrivateKey, path, clientID, refresh string, at int64, nonce string) (string, error) {
	signed := strings.Join([]string{"POST", path, clientID, refresh, strconv.FormatInt(at, 10), nonce}, "\n")
	digest := sha256.Sum256([]byte(signed))
	der, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// ---- signing in ------------------------------------------------------------

// traeAuthorizeURL is the page the user signs in on, with the loopback callback
// the sign-in hands the tokens to. Trae accepts no other kind of callback, which
// is why the gateway listens on 127.0.0.1 itself: the browser has to be able to
// reach this process, so a remote deployment signs in from the machine it runs
// on.
func traeAuthorizeURL(site traeSite, callback string, deviceID, machineID string) string {
	query := url.Values{}
	query.Set("login_version", "1")
	query.Set("auth_from", "solo")
	query.Set("login_channel", "native_ide")
	query.Set("plugin_version", site.pluginVer)
	query.Set("auth_type", "local")
	query.Set("client_id", traeClientID)
	query.Set("redirect", "0")
	query.Set("login_trace_id", newUUIDV4())
	query.Set("auth_callback_url", callback)
	query.Set("machine_id", machineID)
	query.Set("device_id", deviceID)
	query.Set("x_device_id", deviceID)
	query.Set("x_machine_id", machineID)
	query.Set("x_device_brand", traeDeviceBrand)
	query.Set("x_device_type", "windows")
	query.Set("x_os_version", "Windows 10 Pro")
	query.Set("x_env", "")
	query.Set("x_app_version", site.ideVer)
	query.Set("x_app_type", "stable")
	query.Set("hide_saas_login", "false")
	return site.web + "/authorization?" + query.Encode()
}

// startTraeOAuth opens Trae's browser sign-in and waits on a loopback callback
// for the account's tokens.
func (s *Server) startTraeOAuth(providerID string, site traeSite) (authURL, flowID string, err error) {
	deviceID, machineID, publicKey, privateKey, err := newTraeDevice()
	if err != nil {
		return "", "", err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("无法打开 Trae 登录的本地回调端口：%w", err)
	}
	callback := fmt.Sprintf("http://127.0.0.1:%d/authorize", listener.Addr().(*net.TCPAddr).Port)
	flowID = randomHex(16)
	if flowID == "" {
		_ = listener.Close()
		return "", "", errors.New("无法生成登录会话标识")
	}
	credential := domain.TraeCredential{
		DeviceID:         deviceID,
		MachineID:        machineID,
		DevicePublicKey:  publicKey,
		DevicePrivateKey: privateKey,
		ClientID:         traeClientID,
	}
	result := make(chan domain.TraeCredential, 1)
	server := &http.Server{
		Handler:           traeCallbackHandler(result, credential),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.subscriptionOAuthFlows.put(providerID, subscriptionOAuthPending{
		State:     callback,
		FlowID:    flowID,
		CreatedAt: time.Now(),
	})
	s.subscriptionOAuthFlows.setStatus(flowID, "pending", "等待浏览器授权…")
	go func() {
		_ = server.Serve(listener)
	}()
	go s.finishTraeSignIn(providerID, flowID, site, server, listener, result)
	return traeAuthorizeURL(site, callback, deviceID, machineID), flowID, nil
}

// traeCallbackHandler serves the one path Trae redirects to and nothing else:
// an unknown path answers empty, so a stray probe of the port never looks like
// a completed sign-in.
func traeCallbackHandler(result chan<- domain.TraeCredential, seed domain.TraeCredential) http.Handler {
	var once sync.Once
	settle := func(credential domain.TraeCredential) {
		once.Do(func() { result <- credential })
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		}
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/authorize" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		query := r.URL.Query()
		if message := firstNonEmptyString(query.Get("error_description"), query.Get("error")); message != "" {
			w.Write([]byte("<html><body>登录未完成：" + html.EscapeString(message) + "</body></html>"))
			settle(domain.TraeCredential{})
			return
		}
		credential, err := traeCredentialFromCallback(query, seed)
		if err != nil {
			w.Write([]byte("<html><body>登录未完成：" + html.EscapeString(err.Error()) + "</body></html>"))
			settle(domain.TraeCredential{})
			return
		}
		w.Write([]byte("<html><body>登录完成，可以关闭这个页面。</body></html>"))
		settle(credential)
	})
}

// traeCallbackJSON is one of the two JSON documents the callback carries as a
// query string. Trae names its fields both ways across client versions, so
// both spellings are read.
type traeCallbackJSON map[string]any

func (j traeCallbackJSON) string(keys ...string) string {
	for _, key := range keys {
		if value, ok := j[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (j traeCallbackJSON) number(keys ...string) float64 {
	for _, key := range keys {
		switch value := j[key].(type) {
		case float64:
			return value
		case string:
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
				return parsed
			}
		}
	}
	return 0
}

func traeCredentialFromCallback(query url.Values, seed domain.TraeCredential) (domain.TraeCredential, error) {
	credential := seed
	jwt := traeCallbackJSON{}
	if raw := query.Get("userJwt"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &jwt); err != nil {
			return credential, errors.New("Trae 返回的凭证无法解析")
		}
	}
	info := traeCallbackJSON{}
	if raw := query.Get("userInfo"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &info)
	}
	credential.AccessToken = firstNonEmptyString(jwt.string("Token", "token"), query.Get("userJwt"))
	credential.RefreshToken = firstNonEmptyString(jwt.string("RefreshToken", "refreshToken"), query.Get("refreshToken"))
	credential.UserID = firstNonEmptyString(info.string("UserID", "userId"), query.Get("userId"))
	credential.AccountLabel = firstNonEmptyString(info.string("ScreenName", "screenName"), credential.UserID, "Trae")
	credential.ClientID = firstNonEmptyString(jwt.string("ClientID", "clientId"), query.Get("clientId"), traeClientID)
	credential.APIHost = firstNonEmptyString(query.Get("host"), info.string("Host"))
	if region := strings.TrimSpace(info.string("region", "aiRegion", "Region", "AIRegion")); region != "" {
		credential.Region = region
	}
	if expire := jwt.number("TokenExpireAt", "tokenExpireAt"); expire > 0 {
		credential.ExpiresAt = time.Unix(int64(expire), 0).UTC().Format(time.RFC3339)
	} else if exp, ok := traeJWTExpiry(credential.AccessToken); ok {
		credential.ExpiresAt = exp.UTC().Format(time.RFC3339)
	}
	if credential.AccessToken == "" && credential.RefreshToken != "" {
		// Some pages hand out only a refresh token; exchanging it immediately
		// is what the IDE does with the same answer.
		exchanged, err := traeRenew(context.Background(), traeSites[firstSiteIDForCredential(credential)], credential, "")
		if err != nil {
			return credential, err
		}
		credential = exchanged
	}
	if credential.AccessToken == "" {
		return credential, errors.New("Trae 没有返回账号凭证")
	}
	if credential.RefreshToken == "" {
		return credential, errors.New("Trae 没有返回续期凭证")
	}
	credential.Connected = true
	return credential, nil
}

func firstSiteIDForCredential(credential domain.TraeCredential) string {
	if strings.Contains(strings.ToLower(credential.APIHost), "trae.ai") {
		return "trae-global"
	}
	return "trae-cn"
}

// finishTraeSignIn waits for the browser to hand the tokens back, then stores
// them. The listener is closed either way: leaving it open would keep the port
// bound after the flow ended.
func (s *Server) finishTraeSignIn(providerID, flowID string, site traeSite, server *http.Server, listener net.Listener, result <-chan domain.TraeCredential) {
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = listener.Close()
	}()
	fail := func(message string) {
		s.subscriptionOAuthFlows.setStatus(flowID, "failed", message)
		s.logs.AddApp("warn", "trae oauth failed: "+message, providerID)
	}
	select {
	case credential := <-result:
		if credential.AccessToken == "" {
			fail("登录未完成")
			return
		}
		// A CN JWT from the page is not bound to the device, and an unbound one
		// is refused by the check-in and model pages. Renewing once at sign-in
		// makes it the account's device token; failing that is not fatal, the
		// legacy renewal still works.
		if site.device {
			if bound, err := traeBoundExchange(context.Background(), site, credential, traeDeviceClients[0]); err == nil && bound.AccessToken != "" {
				credential = bound
			}
		}
		updated, err := s.router.SetProviderTrae(providerID, site.id, credential)
		if err != nil {
			fail(err.Error())
			return
		}
		if err := s.saveState(); err != nil {
			fail("保存登录状态失败：" + err.Error())
			return
		}
		s.subscriptionOAuthFlows.setStatus(flowID, "done", credential.AccountLabel)
		s.subscriptionOAuthFlows.finish(providerID)
		if synced, syncErr := s.syncTraeModels(updated); syncErr == nil {
			_ = s.saveState()
			_ = synced
		}
		s.logs.AddApp("info", "trae oauth signed in", providerID)
	case <-time.After(traeSignInDeadline):
		fail("登录超时，请重新发起")
	}
}

// ---- renewing --------------------------------------------------------------

// traeRenew gets a fresh Cloud-IDE-JWT. Trae CN renews bound to the account's
// device, which takes a signature over the renewal itself; the other path is
// the older one, where Trae spends the refresh token and issues a new one, so
// only one renewal for a token may run at a time.
func traeRenew(ctx context.Context, site traeSite, credential domain.TraeCredential, _ string) (domain.TraeCredential, error) {
	if site.device {
		for _, client := range traeDeviceClients {
			bound, err := traeBoundExchange(ctx, site, credential, client)
			if err == nil && bound.AccessToken != "" {
				return bound, nil
			}
		}
	}
	return traeLegacyExchange(ctx, site, credential)
}

// traeRenewOverride lets tests inject a renewal failure; nil in production.
var traeRenewOverride func(context.Context, traeSite, domain.TraeCredential, string) (domain.TraeCredential, error)

var traeRenewMu sync.Map // refresh token → its lock, so one token is never spent twice

func traeLegacyExchange(ctx context.Context, site traeSite, credential domain.TraeCredential) (domain.TraeCredential, error) {
	refresh := strings.TrimSpace(credential.RefreshToken)
	if refresh == "" {
		return credential, errors.New("Trae 登录没有可续期的凭证")
	}
	lock, _ := traeRenewMu.LoadOrStore(refresh, &sync.Mutex{})
	mutex := lock.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()
	endpoint := site.auth + traeTokenPath
	var answer struct {
		Result struct {
			Token        string `json:"Token"`
			RefreshToken string `json:"RefreshToken"`
			ExpireAt     int64  `json:"TokenExpireAt"`
			UserInfo     string `json:"UserInfo"`
			Host         string `json:"Host"`
		} `json:"result"`
		Message string      `json:"message"`
		Code    json.Number `json:"code"`
	}
	body := map[string]any{
		"ClientID":     firstNonEmptyString(credential.ClientID, traeClientID),
		"RefreshToken": refresh,
		"ClientSecret": "-",
		"UserID":       credential.UserID,
	}
	if err := traePost(ctx, endpoint, credential, body, &answer); err != nil {
		return credential, err
	}
	token := strings.TrimSpace(answer.Result.Token)
	if token == "" {
		return credential, fmt.Errorf("Trae 续期没有返回凭证：%s", firstNonEmptyString(answer.Message, answer.Code.String(), "无详情"))
	}
	renewed := credential
	renewed.AccessToken = token
	if rotated := strings.TrimSpace(answer.Result.RefreshToken); rotated != "" {
		// Trae spent the old one; only the new one works from here.
		renewed.RefreshToken = rotated
		traeRenewMu.Delete(refresh)
	}
	if answer.Result.ExpireAt > 0 {
		renewed.ExpiresAt = time.Unix(answer.Result.ExpireAt, 0).UTC().Format(time.RFC3339)
	} else if exp, ok := traeJWTExpiry(token); ok {
		renewed.ExpiresAt = exp.UTC().Format(time.RFC3339)
	}
	if host := strings.TrimSpace(answer.Result.Host); host != "" {
		renewed.APIHost = host
	}
	return renewed, nil
}

func traeBoundExchange(ctx context.Context, site traeSite, credential domain.TraeCredential, client traeDeviceExchangeClient) (domain.TraeCredential, error) {
	refresh := strings.TrimSpace(credential.RefreshToken)
	if refresh == "" {
		return credential, errors.New("Trae 登录没有可续期的凭证")
	}
	privateKey := credential.DevicePrivateKey
	// An account signed in before this renewal existed has no key yet; Trae
	// makes one then, and it is kept with the account from here on.
	if strings.TrimSpace(privateKey) == "" {
		deviceID, machineID, publicKey, made, err := newTraeDevice()
		if err != nil {
			return credential, err
		}
		if credential.DeviceID == "" {
			credential.DeviceID, credential.MachineID = deviceID, machineID
		}
		credential.DevicePublicKey, credential.DevicePrivateKey = publicKey, made
		privateKey = made
	}
	key, err := traePrivateKey(privateKey)
	if err != nil {
		return credential, err
	}
	nonce := randomHex(16)
	at := time.Now().Unix()
	proof, err := traeProofOf(key, traeBoundTokenPath, client.clientID, refresh, at, nonce)
	if err != nil {
		return credential, err
	}
	endpoint := site.auth + traeBoundTokenPath
	body := map[string]any{
		"ClientID":     client.clientID,
		"ClientSecret": "",
		"RefreshToken": refresh,
		"DeviceInfo": map[string]any{
			"DeviceID":        credential.DeviceID,
			"MachineID":       credential.MachineID,
			"PlatformCode":    client.platform,
			"DeviceType":      "PC",
			"DeviceName":      "",
			"DeviceModel":     "",
			"ClientVersion":   client.version,
			"DevicePublicKey": credential.DevicePublicKey,
			"DeviceBrand":     traeDeviceBrand,
			"DeviceCPU":       "AMD",
			"OSInfo":          "Windows",
			"OSVersion":       "10.0.22631",
		},
		"DeviceProof": map[string]any{
			"Signature": proof,
			"Timestamp": at,
			"Nonce":     nonce,
		},
		"IDEVersion": client.version,
	}
	var answer struct {
		Result struct {
			Token        string `json:"Token"`
			RefreshToken string `json:"RefreshToken"`
			ExpireAt     int64  `json:"TokenExpireAt"`
		} `json:"Result"`
		Message string      `json:"message"`
		Code    json.Number `json:"code"`
	}
	if err := traePost(ctx, endpoint, credential, body, &answer); err != nil {
		return credential, err
	}
	token := strings.TrimSpace(answer.Result.Token)
	if token == "" {
		return credential, fmt.Errorf("Trae 设备续期没有返回凭证：%s", firstNonEmptyString(answer.Message, answer.Code.String(), "无详情"))
	}
	renewed := credential
	renewed.AccessToken = token
	// This renewal does not spend the refresh token, so the old one stays.
	if rotated := strings.TrimSpace(answer.Result.RefreshToken); rotated != "" {
		renewed.RefreshToken = rotated
	}
	if answer.Result.ExpireAt > 0 {
		renewed.ExpiresAt = time.Unix(answer.Result.ExpireAt, 0).UTC().Format(time.RFC3339)
	} else if exp, ok := traeJWTExpiry(token); ok {
		renewed.ExpiresAt = exp.UTC().Format(time.RFC3339)
	}
	return renewed, nil
}

// ensureFreshTraeToken renews the account's JWT before it lapses. A renewal
// that says the sign-in itself is gone marks the provider so the console can
// ask for it again, rather than failing every request with the same 401.
func (s *Server) ensureFreshTraeToken(provider domain.Provider) (domain.Provider, error) {
	if !isTraeProvider(provider) {
		return provider, nil
	}
	if provider.Trae == nil || provider.Trae.Disconnected {
		return provider, errors.New("Trae 尚未登录，请先在控制台完成登录")
	}
	credential := *provider.Trae
	if strings.TrimSpace(credential.AccessToken) == "" && strings.TrimSpace(credential.RefreshToken) == "" {
		return provider, errors.New("Trae 登录没有可用凭证，请重新登录")
	}
	if !traeTokenNeedsRefresh(&credential) {
		return provider, nil
	}
	site, ok := traeSiteOf(provider)
	if !ok {
		return provider, errors.New("不是 Trae 连接方式")
	}
	// 同一枚 refresh token 只能花一次：并发续期按令牌串行，等锁期间别人续好
	// 的直接用，不再花第二枚。
	renewLock, settled := beginOauthRenew(provider.ID, credential.RefreshToken, s.oauthRenewProviderReader(provider.ID))
	defer renewLock.Unlock()
	if settled != nil && settled.err != nil {
		return settledOauthRenewFailure(settled, provider)
	}
	// 等锁期间别人可能已经续好了：以路由里的最新凭证为准，别把手上这枚已经
	// 花掉的 refresh token 再花一次。
	current := credential
	if fresh, ok := s.freshTraeCredential(provider.ID); ok {
		current = fresh
		if !traeTokenNeedsRefresh(&fresh) {
			return providerWithTrae(provider, fresh), nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), traeCallTimeout)
	defer cancel()
	renew := traeRenew
	if traeRenewOverride != nil {
		renew = traeRenewOverride
	}
	renewed, err := renew(ctx, site, current, "")
	if err != nil {
		s.recordRenewal(provider, "trae", time.Now(), err)
		// A renewal that merely failed now is not a reason to stop serving while
		// the stored token still works. Once it is past its expiry, though,
		// returning nil turns into an upstream 401 the client reads as "bad key";
		// say what actually happened instead. The waiters read the same answer
		// (settleOauthRenew), so they have to reach the same verdict as this one.
		expired := traeCredentialExpired(&current)
		settleOauthRenew(provider.ID, current.RefreshToken, provider, err, expired)
		if traeErrorMeansSignIn(err) {
			return provider, fmt.Errorf("Trae 登录已失效，请重新登录：%w", err)
		}
		if expired {
			s.logs.AddApp("error", "trae renewal failed and the token has expired", provider.ID+": "+err.Error())
			return provider, fmt.Errorf("Trae 令牌已过期且续期失败：%w", err)
		}
		s.logs.AddApp("warn", "trae renewal failed: "+err.Error(), provider.ID)
		return provider, nil
	}
	if strings.TrimSpace(renewed.AccessToken) == "" {
		return provider, nil
	}
	updated, err := s.router.SetProviderTrae(provider.ID, provider.AuthType, renewed)
	if err != nil {
		return provider, err
	}
	s.recordRenewal(updated, "trae", time.Now(), nil)
	retireOauthRenewLock(provider.ID, current.RefreshToken, renewLock)
	if err := s.persistProviderOAuth(updated); err != nil {
		return updated, err
	}
	return updated, nil
}

func traeTokenNeedsRefresh(credential *domain.TraeCredential) bool {
	expires, err := time.Parse(time.RFC3339, strings.TrimSpace(credential.ExpiresAt))
	if err != nil {
		// No usable expiry: renew, since asking with a dead token is the more
		// expensive mistake.
		return true
	}
	return time.Now().Add(traeRenewLead).After(expires)
}

// traeCredentialExpired reports whether the stored token is past its expiry
// (not merely inside the renew lead): a refresh that failed then has nothing
// left to serve with.
func traeCredentialExpired(credential *domain.TraeCredential) bool {
	if credential == nil {
		return true
	}
	expires, err := time.Parse(time.RFC3339, strings.TrimSpace(credential.ExpiresAt))
	if err != nil {
		return false
	}
	return time.Now().After(expires)
}

// traeJWTExpiry reads the exp claim out of the JWT. It is decoded, not
// verified: the token's owner is Trae, and its signature is checked there.
func traeJWTExpiry(token string) (time.Time, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// ---- headers ---------------------------------------------------------------

// applyTraeHeaders signs a request as the account and the device. Trae reads
// the same token from three headers and its client versions from four, and
// offers a model only to a client it considers new enough.
func applyTraeHeaders(request *http.Request, provider domain.Provider) {
	if request == nil || !isTraeProvider(provider) || provider.Trae == nil {
		return
	}
	credential := provider.Trae
	if strings.TrimSpace(credential.AccessToken) == "" || credential.Disconnected {
		return
	}
	jwt := "Cloud-IDE-JWT " + credential.AccessToken
	request.Header.Set("Authorization", jwt)
	request.Header.Set("X-Cloudide-Token", credential.AccessToken)
	request.Header.Set("x-ide-token", credential.AccessToken)
	request.Header.Set("x-uid", credential.UserID)
	request.Header.Set("x-app-id", traeAppID)
	request.Header.Set("x-device-id", credential.DeviceID)
	request.Header.Set("x-machine-id", credential.MachineID)
	request.Header.Set("x-device-cpu", "AMD")
	request.Header.Set("x-device-brand", traeDeviceBrand)
	request.Header.Set("x-device-type", "windows")
	request.Header.Set("x-os-version", "Windows 10")
	request.Header.Set("x-system-type", "Windows")
	request.Header.Set("x-app-version", traeClientVersion)
	request.Header.Set("x-app-version-code", traeClientVersionCode)
	request.Header.Set("x-ide-version", traeClientVersion)
	request.Header.Set("x-ide-version-code", traeClientVersionCode)
	request.Header.Set("x-ide-version-type", "stable")
}

// traePost makes one of Trae's control-plane calls, JSON both ways.
func traePost(ctx context.Context, endpoint string, credential domain.TraeCredential, body, dst any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if strings.TrimSpace(credential.AccessToken) != "" && strings.Contains(endpoint, traeBoundTokenPath) {
		request.Header.Set("Authorization", "Cloud-IDE-JWT "+credential.AccessToken)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	if response.StatusCode >= 400 {
		message, code := traeErrorOf(payload)
		return &traeError{Message: message, Code: code, Status: response.StatusCode}
	}
	if dst == nil {
		return nil
	}
	if err := json.Unmarshal(payload, dst); err != nil {
		return fmt.Errorf("Trae 返回了无法解析的内容（HTTP %d）", response.StatusCode)
	}
	// Trae answers a refusal inside an HTTP 200 as often as not, and a 200 with
	// a code in it is the refusal.
	var envelope struct {
		Code    json.Number `json:"code"`
		Message string      `json:"message"`
	}
	if json.Unmarshal(payload, &envelope) == nil {
		if code := strings.TrimSpace(envelope.Code.String()); code != "" && code != "0" {
			return &traeError{Message: firstNonEmptyString(envelope.Message, "Trae 拒绝了该请求"), Code: code, Status: response.StatusCode}
		}
	}
	return nil
}

type traeError struct {
	Message string
	Code    string
	Status  int
}

func (e *traeError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s（code %s）", e.Message, e.Code)
	}
	return e.Message
}

var traeErrorCodesRe = regexp.MustCompile(`"code"\s*:\s*"?(\d+)"?`)
var traeErrorTextRe = regexp.MustCompile(`"message"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// traeErrorOf pulls Trae's code and message out of a body that may be JSON in
// any of three shapes, or plain text.
func traeErrorOf(payload []byte) (message, code string) {
	var parsed struct {
		Code    json.Number `json:"code"`
		Message string      `json:"message"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		ResponseMetadata struct {
			Error struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"ResponseMetadata"`
	}
	if json.Unmarshal(payload, &parsed) == nil {
		message = firstNonEmptyString(parsed.Message, parsed.Error.Message, parsed.ResponseMetadata.Error.Message)
		code = firstNonEmptyString(parsed.Code.String(), parsed.Error.Code, parsed.ResponseMetadata.Error.Code)
	}
	if message == "" {
		if match := traeErrorTextRe.FindSubmatch(payload); match != nil {
			message = string(match[1])
		}
	}
	if code == "" {
		if match := traeErrorCodesRe.FindSubmatch(payload); match != nil {
			code = string(match[1])
		}
	}
	if message == "" {
		message = strings.TrimSpace(string(payload))
		if len(message) > 200 {
			message = message[:200]
		}
		if message == "" {
			message = "Trae 返回了错误"
		}
	}
	return message, code
}

// traeCodeIn compares a code against a realm's set in both spellings, because
// Trae answers some as numbers and some as strings.
func traeCodeIn(code string, set []string) bool {
	if code == "" {
		return false
	}
	for _, item := range set {
		if item == code {
			return true
		}
	}
	return false
}

func traeErrorMeansSignIn(err error) bool {
	var e *traeError
	if !errors.As(err, &e) {
		return false
	}
	if e.Status == http.StatusUnauthorized {
		return true
	}
	return traeCodeIn(e.Code, []string{"1001", "401", "20101", "10101"})
}

func traeErrorMeansQuota(err error, site traeSite) bool {
	var e *traeError
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == http.StatusTooManyRequests || traeCodeIn(e.Code, site.quota)
}

// wrongTraeFunction is the answer that says "this model is not served by the
// function you asked", which is Trae's way of telling the caller to try one of
// its other agents.
func wrongTraeFunction(code string) bool {
	return traeCodeIn(code, []string{"4001", "4023", "1005"})
}

// ---- chat ------------------------------------------------------------------

// traeChatBody turns an OpenAI chat request into the shape the IDE agent
// takes: text-only content parts, the model named three ways, and the
// conversation's session id.
func traeChatBody(provider domain.Provider, body []byte, function string) ([]byte, error) {
	var asked struct {
		Model    string          `json:"model"`
		Messages json.RawMessage `json:"messages"`
		Tools    json.RawMessage `json:"tools"`
		// max_completion_tokens is the newer spelling of the same limit.
		MaxCompletionTokens *int            `json:"max_completion_tokens"`
		MaxTokens           *int            `json:"max_tokens"`
		Temperature         *float64        `json:"temperature"`
		ToolChoice          json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &asked); err != nil {
		return nil, err
	}
	model := strings.TrimSpace(asked.Model)
	if model == "" {
		model = strings.TrimSpace(provider.DefaultModel)
	}
	site, ok := traeSiteOf(provider)
	if !ok {
		return nil, errors.New("不是 Trae 连接方式")
	}
	messages := traeMessages(asked.Messages)
	payload := map[string]any{
		"messages":    messages,
		"function":    function,
		"config_name": model,
		"model":       model,
		"stream":      true,
		"request_id":  newUUIDV4(),
		"session_id":  traeSessionID(provider, asked.Messages),
	}
	if named := traeNamedModel(provider, model); named != "" {
		payload["model_name"] = named
	}
	if limit := traeLimit(asked.MaxCompletionTokens, asked.MaxTokens, provider, model); limit > 0 {
		payload["max_tokens"] = limit
	}
	if asked.Temperature != nil {
		payload["temperature"] = *asked.Temperature
	}
	if tools := traeTools(asked.Tools); len(tools) > 0 && !traeToolChoiceIsNone(asked.ToolChoice) {
		payload["tools"] = tools
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	_ = site
	return encoded, nil
}

// traeLimit is what the answer may be: what the client asked for, cut to what
// the model can produce.
func traeLimit(askedNew, askedOld *int, provider domain.Provider, model string) int {
	limit := 0
	if askedNew != nil && *askedNew > 0 {
		limit = *askedNew
	} else if askedOld != nil && *askedOld > 0 {
		limit = *askedOld
	} else {
		limit = traeMaxOutputDefault
	}
	if most := traeModelMaxOutput(provider, model); most > 0 && limit > most {
		limit = most
	}
	return limit
}

func traeModelMaxOutput(provider domain.Provider, model string) int {
	for _, item := range provider.Models {
		if strings.EqualFold(item.ID, strings.TrimSpace(model)) && item.MaxOutputTokens > 0 {
			return item.MaxOutputTokens
		}
	}
	return 0
}

// traeNamedModel is the model the entry actually serves. Trae lists a model
// under its own name and serves it as a "__dev" build of another; only the
// discovered list knows the pair, and a request that names it is the one Trae
// accepts for some models.
func traeNamedModel(provider domain.Provider, model string) string {
	for _, item := range provider.Models {
		if strings.EqualFold(item.ID, strings.TrimSpace(model)) && item.MaxOutputTokens < 0 {
			// A negative MaxOutputTokens is not a limit, and this field is only
			// a marker here: no model is listed that way.
			return ""
		}
	}
	return ""
}

// traeMessages flattens a chat conversation into Trae's text-only form. Trae's
// agent protocol has no turn for a tool call or its result, so both go in as
// text: the assistant's calls are named in the text of its turn, and a tool
// result becomes a user turn saying what came back.
func traeMessages(raw json.RawMessage) []map[string]any {
	var messages []struct {
		Role      string          `json:"role"`
		Name      string          `json:"name"`
		Content   json.RawMessage `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if json.Unmarshal(raw, &messages) != nil {
		return nil
	}
	converted := make([]map[string]any, 0, len(messages))
	appendText := func(role, text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		if len(converted) > 0 {
			last := converted[len(converted)-1]
			if last["role"] == role && role != "system" {
				if parts, ok := last["content"].([]map[string]any); ok && len(parts) > 0 {
					parts[len(parts)-1]["text"] = parts[len(parts)-1]["text"].(string) + "\n\n" + text
					return
				}
			}
		}
		converted = append(converted, map[string]any{
			"role":    role,
			"content": []map[string]any{{"type": "text", "text": text}},
		})
	}
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		switch role {
		case "developer":
			role = "system"
		case "tool":
			appendText("user", fmt.Sprintf("Result of %s (call %s):\n%s", message.Name, traeContentText(message.Content), traeContentText(message.Content)))
			continue
		}
		text := traeContentText(message.Content)
		for _, call := range message.ToolCalls {
			text += fmt.Sprintf("\n<tool_call>{\"name\":%q,\"arguments\":%s}", call.Function.Name, call.Function.Arguments)
		}
		if role == "assistant" && text == "" {
			continue
		}
		appendText(role, text)
	}
	return converted
}

// traeContentText reads a message's content, which may be a string or a list
// of parts. An image is not sent: Trae's models take no attachment, so its
// place is marked in text instead.
func traeContentText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Input   string `json:"input"`
		Content string `json:"content"`
		ToolUse struct {
			Name string `json:"name"`
		} `json:"tool_use"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	out := []string{}
	for _, part := range parts {
		switch part.Type {
		case "image", "image_url", "input_image":
			out = append(out, "[an image, which this model can't see]")
		default:
			if line := firstNonEmptyString(part.Text, part.Input, part.Content); line != "" {
				out = append(out, line)
			}
		}
	}
	return strings.Join(out, "\n")
}

// traeTools re-offers the client's tools the way Trae's schema wants them:
// parameters as a JSON string rather than an object.
func traeTools(raw json.RawMessage) []map[string]any {
	var tools []struct {
		Type       string          `json:"type"`
		Function   json.RawMessage `json:"function"`
		Name       string          `json:"name"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if json.Unmarshal(raw, &tools) != nil {
		return nil
	}
	out := []map[string]any{}
	for _, tool := range tools {
		name, description, parameters := tool.Name, "", json.RawMessage(`{}`)
		if len(tool.Function) > 0 {
			var fn struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			}
			if json.Unmarshal(tool.Function, &fn) != nil {
				continue
			}
			name, description = fn.Name, fn.Description
			if len(fn.Parameters) > 0 {
				parameters = fn.Parameters
			}
		}
		if strings.TrimSpace(name) == "" {
			continue
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": description,
				// Trae's typed field rejects an object here.
				"parameters": string(parameters),
			},
		})
	}
	return out
}

func traeToolChoiceIsNone(raw json.RawMessage) bool {
	var choice string
	return json.Unmarshal(raw, &choice) == nil && strings.EqualFold(strings.TrimSpace(choice), "none")
}

// traeSessionID is the conversation's id, stable across its turns. Trae counts
// a conversation by it: a fresh one per request makes one user send that takes
// several steps look like several conversations in its usage detail.
func traeSessionID(provider domain.Provider, messages json.RawMessage) string {
	first := ""
	var list []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(messages, &list) == nil {
		for _, message := range list {
			if strings.EqualFold(message.Role, "user") {
				first = traeContentText(message.Content)
				break
			}
		}
	}
	account := provider.ID
	if provider.Trae != nil {
		account = provider.ID + "|" + firstNonEmptyString(provider.Trae.UserID, provider.Trae.AccountLabel)
	}
	seed := "llm-gateway-trae|" + account
	if strings.TrimSpace(first) == "" {
		// No first user turn to hang the conversation on: a request of its own.
		seed += "|request|" + randomHex(8)
	} else {
		seed += "|" + first
	}
	return traeUUIDFrom(seed)
}

// traeUUIDFrom makes a v4-shaped UUID out of a seed, so the same conversation
// gets the same id every turn and across a restart.
func traeUUIDFrom(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// traeFunctionsToTry is the order this request's agent functions are asked in:
// the one that answered for this account before, then the realm's own order.
func traeFunctionsToTry(provider domain.Provider) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	if provider.Trae != nil {
		add(provider.Trae.ChatFunction)
	}
	if site, ok := traeSiteOf(provider); ok {
		for _, name := range site.functions {
			add(name)
		}
	}
	if len(out) == 0 {
		out = append(out, "chat_v3")
	}
	return out
}

// ---- the reply -------------------------------------------------------------

// traeChunkReader turns Trae's SSE events into OpenAI's chunks as they arrive,
// so a streaming client still streams. Trae answers every reply this way and
// this reader is the only place its event grammar is read.
type traeChunkReader struct {
	site    traeSite
	model   string
	id      string
	created int64
	scanner *bufio.Scanner

	// out is the chunks produced but not yet handed to the reader, and pos how
	// much of the first has gone.
	out [][]byte
	pos int
	// done stops the reader after [DONE], and failed says the reply ended in a
	// Trae error rather than an answer.
	done     bool
	failed   bool
	started  bool // the role delta has gone out
	answered bool
	called   bool
	finished bool
	// pendingUsage is Trae's usage event, held until the finish chunk, which is
	// where OpenAI's shape carries it.
	pendingUsage map[string]any
}

func newTraeChunkReader(body io.Reader, site traeSite, model string) *traeChunkReader {
	scanner := bufio.NewScanner(body)
	// A single Trae event can carry a long prompt-echo or a tool call's whole
	// argument blob, so the default 64KB line limit is too small.
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	return &traeChunkReader{
		site:    site,
		model:   model,
		id:      "chatcmpl-" + randomHex(6),
		created: time.Now().Unix(),
		scanner: scanner,
	}
}

func (r *traeChunkReader) Read(p []byte) (int, error) {
	for {
		if len(r.out) > 0 {
			n := copy(p, r.out[0][r.pos:])
			r.pos += n
			if r.pos >= len(r.out[0]) {
				r.out, r.pos = r.out[1:], 0
			}
			if n > 0 {
				return n, nil
			}
			continue
		}
		if r.done {
			return 0, io.EOF
		}
		event, data, ok := r.nextEvent()
		if !ok {
			r.finish()
			continue
		}
		r.consume(event, data)
	}
}

func (r *traeChunkReader) Close() error { return nil }

// nextEvent reads one framed SSE event: the event name from its line or from
// the data itself, and the data lines joined the way SSE says to join them.
func (r *traeChunkReader) nextEvent() (string, []byte, bool) {
	name := ""
	var data []string
	for r.scanner.Scan() {
		line := strings.TrimRight(r.scanner.Text(), "\r")
		switch {
		case line == "":
			if len(data) == 0 {
				name = ""
				continue
			}
			return name, []byte(strings.Join(data, "\n")), true
		case strings.HasPrefix(line, ":"):
			// A comment: keep-alive, nothing to dispatch.
			continue
		case strings.HasPrefix(line, "event:"):
			if len(data) > 0 {
				// An event line with data still pending means the previous
				// event was never blank-line terminated; dispatch it as it is.
				name = strings.TrimSpace(line[len("event:"):])
				return name, []byte(strings.Join(data, "\n")), true
			}
			name = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if len(data) > 0 {
		return name, []byte(strings.Join(data, "\n")), true
	}
	return "", nil, false
}

// finish closes the stream the way an OpenAI stream ends: [DONE] after the
// finish chunk, or nothing more after an error event.
func (r *traeChunkReader) finish() {
	if r.done {
		return
	}
	if !r.finished && !r.failed {
		r.emitFinish("")
	}
	if !r.failed {
		r.out = append(r.out, []byte("data: [DONE]\n\n"))
	}
	r.done = true
}

// consume turns one Trae event into the chunk or chunks it implies.
func (r *traeChunkReader) consume(event string, data []byte) {
	name := traeEventName(event, data)
	var payload map[string]any
	decoded := json.Unmarshal(data, &payload) == nil
	if decoded {
		if usage := traeUsageOf(payload); usage != nil {
			r.pendingUsage = usage
		}
	}
	if string(data) == "[DONE]" {
		r.finish()
		return
	}
	switch name {
	case "error":
		message, code := traeErrorOf(data)
		r.failed = true
		r.appendChunk(map[string]any{"error": map[string]any{
			"message": r.site.label + ": " + message,
			"type":    "api_error",
			"code":    code,
		}})
		r.out = append(r.out, []byte("data: [DONE]\n\n"))
		r.done = true
		return
	case "done", "response_done", "stream_done":
		r.emitFinish(traeFinishReason(payload))
		r.out = append(r.out, []byte("data: [DONE]\n\n"))
		r.done = true
		return
	case "token_usage":
		return
	}
	if !decoded || !traeIsAnswerEvent(name) {
		// Queueing, timing and metadata events are Trae's own business.
		return
	}
	if think := firstNonEmptyString(traeStringOf(payload["reasoning_content"]), traeStringOf(payload["reasoning"])); think != "" {
		r.emitReasoning(think)
	}
	if text := traeStringAt(payload, "response", "content"); text != "" {
		r.emitText(text)
	} else if delta, ok := payload["delta"].(map[string]any); ok {
		if inner := firstNonEmptyString(traeStringOf(delta["reasoning_content"]), traeStringOf(delta["reasoning"])); inner != "" {
			r.emitReasoning(inner)
		}
		if piece := traeStringOf(delta["content"]); piece != "" {
			r.emitText(piece)
		}
	} else if piece := traeStringOf(payload["delta"]); piece != "" {
		r.emitText(piece)
	}
	if calls := traeToolCallsOf(payload); len(calls) > 0 {
		r.emitToolCalls(calls)
	}
	if finish := traeFinishReason(payload); finish != "" {
		r.emitFinish(finish)
	}
}

// appendChunk writes one chunk of the client-visible shape.
func (r *traeChunkReader) appendChunk(body map[string]any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return
	}
	r.out = append(r.out, []byte("data: "+string(encoded)+"\n\n"))
}

func (r *traeChunkReader) emit(delta map[string]any) {
	r.appendChunk(map[string]any{
		"id":      r.id,
		"object":  "chat.completion.chunk",
		"created": r.created,
		"model":   r.model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}},
	})
}

func (r *traeChunkReader) begin() {
	if r.started {
		return
	}
	r.started = true
	r.emit(map[string]any{"role": "assistant", "content": ""})
}

func (r *traeChunkReader) emitText(text string) {
	r.begin()
	r.emit(map[string]any{"content": text})
	r.answered = true
}

func (r *traeChunkReader) emitReasoning(text string) {
	r.begin()
	r.emit(map[string]any{"reasoning_content": text})
	r.answered = true
}

func (r *traeChunkReader) emitToolCalls(calls []map[string]any) {
	r.begin()
	for index, call := range calls {
		call["index"] = index
		call["type"] = "function"
	}
	r.emit(map[string]any{"tool_calls": calls})
	r.answered = true
	r.called = true
}

// emitFinish ends the answer. The reason a client gets is the one that fits
// what came: a call is a call, otherwise what Trae said, otherwise a stop.
func (r *traeChunkReader) emitFinish(reason string) {
	if r.finished {
		return
	}
	r.finished = true
	final := "stop"
	switch {
	case r.called:
		final = "tool_calls"
	case reason != "":
		final = reason
	}
	chunk := map[string]any{
		"id":      r.id,
		"object":  "chat.completion.chunk",
		"created": r.created,
		"model":   r.model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": final}},
	}
	if r.pendingUsage != nil {
		chunk["usage"] = r.pendingUsage
	}
	r.appendChunk(chunk)
}

var traeAnswerEvents = map[string]bool{
	"": true, "output": true, "message": true, "delta": true, "content": true,
	"text": true, "answer": true, "chunk": true, "output_delta": true, "message_delta": true,
}

func traeIsAnswerEvent(name string) bool { return traeAnswerEvents[name] }

// traeEventName is the event's name as Trae means it: the SSE event line, or
// when there is none, the name inside the data. Camel case becomes snake case
// because Trae writes some events that way and reads them back the same.
func traeEventName(event string, data []byte) string {
	name := strings.TrimSpace(event)
	if name == "" {
		var probe struct {
			Event string `json:"event"`
			Type  string `json:"type"`
		}
		if json.Unmarshal(data, &probe) == nil {
			name = firstNonEmptyString(probe.Event, probe.Type)
		}
	}
	lowered := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	return strings.Trim(lowered, "_")
}

func traeStringOf(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func traeStringAt(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if text := traeStringOf(payload[key]); text != "" {
			return text
		}
	}
	return ""
}

func traeToolCallsOf(payload map[string]any) []map[string]any {
	raw, ok := payload["tool_calls"].([]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := []map[string]any{}
	for _, item := range raw {
		call, _ := item.(map[string]any)
		if call == nil {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			fn = map[string]any{}
		}
		name := firstNonEmptyString(traeStringOf(fn["name"]), traeStringOf(call["tool_name"]), traeStringOf(call["name"]))
		arguments := firstNonEmptyString(traeStringOf(fn["arguments"]), traeStringOf(fn["args"]), traeStringOf(fn["parameters"]), traeStringOf(call["input"]))
		if name == "" {
			continue
		}
		if !json.Valid([]byte(arguments)) {
			encoded, err := json.Marshal(arguments)
			if err != nil {
				continue
			}
			arguments = string(encoded)
		}
		out = append(out, map[string]any{
			"id":   firstNonEmptyString(traeStringOf(call["id"]), traeStringOf(call["tool_call_id"]), "call_"+randomHex(4)),
			"type": "function",
			"function": map[string]any{
				"name":      name,
				"arguments": arguments,
			},
		})
	}
	return out
}

// traeFinishReason normalizes what Trae calls the end of an answer. Its own
// words for "it ran out" all become OpenAI's.
func traeFinishReason(payload map[string]any) string {
	reason := strings.ToLower(firstNonEmptyString(
		traeStringOf(payload["finish_reason"]),
		traeStringOf(payload["stop_reason"]),
		traeStringOf(payload["finishReason"]),
	))
	switch reason {
	case "":
		return ""
	case "length", "max_tokens", "max_output_tokens", "max_length":
		return "length"
	case "content_filter", "sensitive":
		return "content_filter"
	case "tool_calls", "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

func traeUsageOf(payload map[string]any) map[string]any {
	usage, ok := payload["usage"].(map[string]any)
	if !ok {
		usage = payload
	}
	prompt := traeNumberOf(usage, "prompt_tokens", "input_tokens", "promptTokens", "inputTokens")
	completion := traeNumberOf(usage, "completion_tokens", "output_tokens", "completionTokens", "outputTokens")
	if prompt == 0 && completion == 0 {
		return nil
	}
	total := traeNumberOf(usage, "total_tokens", "totalTokens")
	if total == 0 {
		total = prompt + completion
	}
	out := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
	}
	if reasoning := traeNumberOf(usage, "reasoning_tokens"); reasoning > 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	if cached := traeNumberOf(usage, "cache_read_input_tokens"); cached > 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	return out
}

func traeNumberOf(payload map[string]any, keys ...string) int64 {
	for _, key := range keys {
		switch value := payload[key].(type) {
		case float64:
			return int64(value)
		case string:
			if parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
				return parsed
			}
		}
	}
	return 0
}

// ---- the request -----------------------------------------------------------

// sendTraeChat asks Trae's agent functions in turn until one takes the model.
// Trae answers 4001 / 4023 / 1005 when the model is served by a different one
// of its agents, and the answer is the same apart from which function is
// named, so the retry is cheap and the first success is remembered for the
// account.
func (s *Server) sendTraeChat(ctx context.Context, r *http.Request, provider domain.Provider, model string, body []byte, skipIncomingAuth bool) (*http.Response, string, error) {
	functions := traeFunctionsToTry(provider)
	var lastErr error
	var lastResponse *http.Response
	for index, function := range functions {
		shaped, err := traeChatBody(provider, body, function)
		if err != nil {
			return nil, "", err
		}
		upstreamURL := traeChatURL(provider)
		if upstreamURL == "" {
			return nil, "", errors.New("无法确定 Trae 的接口地址")
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, strings.NewReader(string(shaped)))
		if err != nil {
			return nil, "", err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "text/event-stream")
		applyTraeHeaders(request, provider)
		if incoming := traeIncomingAuth(r, skipIncomingAuth); incoming != "" {
			request.Header.Set("X-Trae-Incoming-Authorization", incoming)
		}
		response, err := doHTTPWithTiming(ctx, &http.Client{Timeout: 0}, request)
		if err != nil {
			return nil, function, err
		}
		if response.StatusCode < 400 {
			return s.decorateTraeResponse(response, provider, model, function), function, nil
		}
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		_ = lastResponse
		message, code := traeErrorOf(payload)
		lastErr = &traeError{Message: message, Code: code, Status: response.StatusCode}
		if response.StatusCode == http.StatusBadRequest && wrongTraeFunction(code) && index+1 < len(functions) {
			continue
		}
		// The answer is final: hand the client what Trae said, in its own
		// status, and mark a sign-in that has lapsed.
		return traeFailureResponse(response, provider, message, code), function, lastErr
	}
	return traeFailureResponse(nil, provider, traeMessageOf(lastErr), ""), "", lastErr
}

func traeIncomingAuth(r *http.Request, skip bool) string {
	if skip || r == nil {
		return ""
	}
	return r.Header.Get("Authorization")
}

func traeMessageOf(err error) string {
	if err == nil {
		return "Trae 拒绝了该请求"
	}
	return err.Error()
}

// decorateTraeResponse puts the stream converter in front of the upstream
// body, so everything downstream — pass-through writing, protocol conversion,
// the console's aggregation — only ever sees OpenAI chunks.
func (s *Server) decorateTraeResponse(response *http.Response, provider domain.Provider, model, function string) *http.Response {
	site, _ := traeSiteOf(provider)
	// The remembered function is only worth storing when it was not the first
	// one tried: that is the case where Trae told us to ask elsewhere.
	if function != "" && provider.Trae != nil && !strings.EqualFold(provider.Trae.ChatFunction, function) {
		renewed := *provider.Trae
		renewed.ChatFunction = function
		if updated, err := s.router.SetProviderTrae(provider.ID, provider.AuthType, renewed); err == nil {
			_ = s.persistProviderOAuth(updated)
		}
	}
	response.Body = newTraeChunkReader(response.Body, site, model)
	response.Header.Set("Content-Type", "text/event-stream; charset=utf-8")
	response.Header.Set("Cache-Control", "no-cache")
	response.ContentLength = -1
	return response
}

// traeFailureResponse turns Trae's refusal into the status a client of this
// gateway understands: an expired sign-in as 401, a spent allowance as 429,
// anything else as Trae's own status or a 502.
func traeFailureResponse(response *http.Response, provider domain.Provider, message, code string) *http.Response {
	site, _ := traeSiteOf(provider)
	status := http.StatusBadGateway
	if response != nil && response.StatusCode >= 400 {
		status = response.StatusCode
	}
	kind := "api_error"
	switch {
	case response != nil && response.StatusCode == http.StatusUnauthorized || traeCodeIn(code, site.lapsed):
		status = http.StatusUnauthorized
		kind = "api_error"
		message = fmt.Sprintf("%s 的登录已失效（%s），请重新登录", site.label, message)
	case response != nil && response.StatusCode == http.StatusTooManyRequests || traeCodeIn(code, site.quota):
		status = http.StatusTooManyRequests
		kind = "rate_limit_error"
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": kind, "code": code}})
	if response == nil {
		response = &http.Response{Header: http.Header{}, Request: &http.Request{}}
		response.StatusCode = status
	}
	response.StatusCode = status
	response.Status = fmt.Sprintf("%d %s", status, http.StatusText(status))
	response.Body = io.NopCloser(strings.NewReader(string(body)))
	response.ContentLength = int64(len(body))
	response.Header.Set("Content-Type", "application/json; charset=utf-8")
	if status == http.StatusUnauthorized {
		response.Header.Set(traeStreamHeaderStatus, "expired")
	}
	return response
}

// proxyTraeChat is the pass-through path for a Trae provider: Trae only answers
// as a stream of its own events, so the request always goes up streamed, and a
// client that asked for a single object gets the converted stream rebuilt into
// one.
func (s *Server) proxyTraeChat(w http.ResponseWriter, r *http.Request, provider domain.Provider, model string, body []byte, skipIncomingAuth bool) (int, TokenUsage, []byte, error) {
	stream := requestBodyWantsStream(body)
	refreshed, err := s.ensureFreshTraeToken(provider)
	if err != nil {
		return 0, TokenUsage{}, nil, err
	}
	response, _, sendErr := s.sendTraeChat(r.Context(), r, refreshed, model, body, skipIncomingAuth)
	if response == nil {
		if sendErr == nil {
			sendErr = errors.New("Trae 没有应答")
		}
		return 0, TokenUsage{}, nil, sendErr
	}
	defer response.Body.Close()
	// Trae only answers as a stream of its own events; sendTraeChat has already
	// put the OpenAI-shaped conversion in front of the body, so what is left is
	// whether this client wants a stream at all.
	if !stream && response.StatusCode < 400 {
		_ = forcedStreamAsCompletion(response, model, workBuddyStreamAsCompletion)
	}
	status, usage, responseBody, err := writePassThroughResponse(w, response, stream, usageStreamOpenAI, ParseOpenAIUsage, openAIChatStreamInterrupter{model: model})
	if err == nil && sendErr != nil && status >= 400 {
		// Trae's own reason is what the request log should carry, not the
		// writer's success at moving its body along.
		err = sendErr
	}
	return status, usage, responseBody, err
}

// ---- models ----------------------------------------------------------------

// fetchTraeModels asks the account's own model list: every agent function's
// config in one call, as TRAE SOLO does. The lists are merged, because SOLO's
// and the agent's carry models chat_v3 does not, and the IDE's helpers (the
// summarizer, the fast-apply engine) are left out — they are not chat models.
func (s *Server) fetchTraeModels(ctx context.Context, provider domain.Provider) ([]domain.Model, error) {
	site, ok := traeSiteOf(provider)
	if !ok {
		return nil, errors.New("不是 Trae 连接方式")
	}
	body := map[string]any{
		"functions":                 site.functions,
		"agent_type":                "",
		"current_config_info":       map[string]any{"config_name": "", "is_custom_model": false},
		"mode_type":                 0,
		"access_type":               0,
		"ab_force_vids":             "",
		"ab_autotest_advanced_mode": 0,
		"show_custom_model":         true,
	}
	var answer struct {
		FunctionConfigs []struct {
			Function       string `json:"function"`
			ConfigInfoList []struct {
				ConfigName        string `json:"config_name"`
				Usage             string `json:"usage"`
				ConfigSwitch      *bool  `json:"config_switch"`
				IsCustomModel     bool   `json:"is_custom_model"`
				IsInvisibleToUser bool   `json:"is_invisible_to_user"`
				DisplayName       string `json:"display_name"`
				DisplayModelName  string `json:"display_model_name"`
				DisplayConfig     struct {
					DisplayName string `json:"display_name"`
				} `json:"display_config"`
				ContextWindowTokens struct {
					Dev json.Number `json:"dev"`
					Max json.Number `json:"max"`
				} `json:"context_window_tokens"`
				ModelDetailList []struct {
					ModelName string      `json:"model_name"`
					MaxTokens json.Number `json:"max_tokens"`
				} `json:"model_detail_list"`
			} `json:"config_info_list"`
		} `json:"function_configs"`
	}
	endpoint := site.api + traeModelsBatchPath
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(jsonBodyOf(body)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	applyTraeHeaders(request, provider)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if response.StatusCode != http.StatusOK {
		message, code := traeErrorOf(payload)
		return nil, &traeError{Message: message, Code: code, Status: response.StatusCode}
	}
	if err := json.Unmarshal(payload, &answer); err != nil {
		return nil, err
	}
	models := []domain.Model{}
	seen := map[string]bool{}
	for _, group := range answer.FunctionConfigs {
		if !traeCodeIn(group.Function, site.functions) && !traeInList(group.Function, site.functions) {
			continue
		}
		for _, item := range group.ConfigInfoList {
			id := strings.TrimSpace(item.ConfigName)
			if id == "" || seen[id] {
				continue
			}
			// The IDE's own helpers and the custom-model slots are not chat
			// models; neither is an entry the account switched off.
			if strings.HasPrefix(strings.ToLower(id), "custom_model") {
				continue
			}
			if item.Usage != "" && item.Usage != "chat_completion" {
				continue
			}
			if item.ConfigSwitch != nil && !*item.ConfigSwitch {
				continue
			}
			if item.IsCustomModel {
				continue
			}
			seen[id] = true
			context := zcodeNumber(item.ContextWindowTokens.Dev)
			output := 0
			for _, detail := range item.ModelDetailList {
				if strings.HasSuffix(detail.ModelName, "__dev") {
					output = zcodeNumber(detail.MaxTokens)
				}
			}
			if context <= 0 {
				context = 128000
			}
			models = append(models, domain.Model{
				ID:              id,
				ProviderID:      provider.ID,
				Protocol:        domain.ProtocolOpenAIChat,
				ContextLength:   context,
				MaxOutputTokens: output,
				InMenu:          true,
			})
		}
	}
	if len(models) == 0 {
		return nil, errors.New("Trae 没有返回模型列表")
	}
	return models, nil
}

func traeInList(item string, list []string) bool {
	for _, value := range list {
		if value == item {
			return true
		}
	}
	return false
}

func (s *Server) syncTraeModels(provider domain.Provider) (domain.Provider, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	models, err := s.fetchTraeModels(ctx, provider)
	if err != nil || len(models) == 0 {
		site, ok := traeSiteOf(provider)
		if !ok {
			return provider, errors.New("不是 Trae 连接方式")
		}
		models = traeFallbackModels(site, provider.ID)
	}
	for index := range models {
		models[index].ProviderID = provider.ID
	}
	defaultModel := strings.TrimSpace(provider.DefaultModel)
	if defaultModel == "" && len(models) > 0 {
		defaultModel = models[0].ID
	}
	updated, err := s.router.UpdateProvider(provider.ID, domain.Provider{Models: models, DefaultModel: defaultModel})
	if err != nil {
		return provider, err
	}
	return updated, nil
}

func traeFallbackModels(site traeSite, providerID string) []domain.Model {
	models := make([]domain.Model, 0, len(site.models))
	for _, item := range site.models {
		models = append(models, domain.Model{
			ID:              item.id,
			ProviderID:      providerID,
			Protocol:        domain.ProtocolOpenAIChat,
			ContextLength:   item.context,
			MaxOutputTokens: item.output,
			InMenu:          true,
		})
	}
	return models
}

// ---- usage -----------------------------------------------------------------

type traeQuotaWindow struct {
	Name     string  `json:"name"`
	Used     float64 `json:"used"`
	Amount   float64 `json:"amount,omitempty"`
	Limit    float64 `json:"limit,omitempty"`
	Unit     string  `json:"unit,omitempty"`
	ResetsAt string  `json:"resetsAt,omitempty"`
}

type traeQuota struct {
	Plan      string            `json:"plan,omitempty"`
	Windows   []traeQuotaWindow `json:"windows"`
	Error     string            `json:"error,omitempty"`
	FetchedAt string            `json:"fetchedAt,omitempty"`
}

// traeQuotaFor reads what the account has used of what it has. Trae CN bills in
// credits and Trae Global in dollars, from different pages, so the realm picks
// the endpoint and the reader.
func traeQuotaFor(ctx context.Context, provider domain.Provider) traeQuota {
	site, ok := traeSiteOf(provider)
	quota := traeQuota{Windows: []traeQuotaWindow{}}
	if !ok || provider.Trae == nil {
		quota.Error = "Trae 尚未登录"
		return quota
	}
	endpoint := site.pay
	if site.id == "trae-global" && (strings.HasPrefix(strings.ToUpper(provider.Trae.Region), "US") || strings.Contains(provider.Trae.APIHost, "api-us")) {
		endpoint = site.usPay
	}
	path := "/trae/api/v2/pay/ide_user_ent_usage"
	body := map[string]any{"require_usage": true, "req_source": 0}
	if site.id == "trae-global" {
		path = "/trae/api/v1/pay/user_current_entitlement_list"
		body = map[string]any{}
	}
	var answer map[string]any
	if err := traePost(ctx, endpoint+path, *provider.Trae, body, &answer); err != nil {
		quota.Error = err.Error()
		return quota
	}
	windows, plan, unlimited := traeWindowsOf(answer, site)
	quota.Plan = plan
	quota.Windows = windows
	if unlimited {
		quota.Plan = firstNonEmptyString(plan, "Unlimited")
	}
	quota.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	return quota
}

// traeWindowsOf adds up the account's entitlement packs: a limit and what was
// used of each, in credits or dollars depending on the realm.
func traeWindowsOf(answer map[string]any, site traeSite) ([]traeQuotaWindow, string, bool) {
	packs, _ := answer["user_entitlement_pack_list"].([]any)
	var limit, used float64
	var soonest string
	plan := ""
	seen := false
	for _, item := range packs {
		pack, _ := item.(map[string]any)
		if pack == nil {
			continue
		}
		base, _ := pack["entitlement_base_info"].(map[string]any)
		quota, _ := base["quota"].(map[string]any)
		usage, _ := pack["usage"].(map[string]any)
		packLimit, packUsed := 0.0, 0.0
		if site.id == "trae-global" {
			packLimit = traeFloatOf(quota["basic_usage_limit"]) + traeFloatOf(quota["bonus_usage_limit"])
			packUsed = traeFloatOf(usage["basic_usage_amount"]) + traeFloatOf(usage["bonus_usage_amount"])
		} else {
			packLimit = traeFloatOf(quota["credits_limit"])
			packUsed = traeFloatOf(usage["credits_amount"])
			if packLimit == -1 {
				return nil, "Credits", true
			}
		}
		if packLimit <= 0 && packUsed <= 0 {
			continue
		}
		seen = true
		limit += packLimit
		used += packUsed
		if name := traeStringOf(base["display_desc"]); name != "" {
			plan = name
		}
		if name := traeStringOf(pack["display_desc"]); name != "" {
			plan = name
		}
		if end := traeWhenOf(base["end_time"], base["expire_time"]); end != "" && (soonest == "" || end < soonest) {
			soonest = end
		}
	}
	if !seen {
		return nil, plan, false
	}
	percent := 0.0
	if limit > 0 {
		percent = used * 100 / limit
	}
	if percent > 100 {
		percent = 100
	}
	unit := "credits"
	name := "Credits"
	if site.id == "trae-global" {
		unit = "usd"
		name = "Dollar Usage"
	}
	return []traeQuotaWindow{{
		Name:     name,
		Used:     traeRound(percent),
		Amount:   traeRound(used),
		Limit:    traeRound(limit),
		Unit:     unit,
		ResetsAt: soonest,
	}}, firstNonEmptyString(plan, "Free"), false
}

func traeFloatOf(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case string:
		parsed, _ := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed
	}
	return 0
}

func traeRound(value float64) float64 {
	return float64(int64(value*100+0.5)) / 100
}

func traeWhenOf(values ...any) string {
	trimmed := ""
	for _, value := range values {
		if text := strings.Trim(strings.TrimSpace(traeRawText(value)), `"`); text != "" && text != "null" {
			trimmed = text
			break
		}
	}
	if trimmed == "" {
		return ""
	}
	if seconds, err := strconv.ParseFloat(trimmed, 64); err == nil && seconds > 0 {
		return traeTimeOf(int64(seconds))
	}

	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if parsed, err := time.ParseInLocation(layout, trimmed, time.FixedZone("CST", 8*3600)); err == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

func traeRawText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	case json.Number:
		return typed.String()
	}
	return ""
}

func traeTimeOf(seconds int64) string {
	if seconds > 1_000_000_000_000 {
		return time.UnixMilli(seconds).UTC().Format(time.RFC3339)
	}
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
}

func jsonBodyOf(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// providerWithTrae 返回带最新 Trae 凭证的 provider 副本。
func providerWithTrae(provider domain.Provider, credential domain.TraeCredential) domain.Provider {
	provider.Trae = &credential
	return provider
}
