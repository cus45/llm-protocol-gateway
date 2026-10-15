package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// WorkBuddy is Tencent's CodeBuddy subscription, which its WorkBuddy desktop
// app signs in to; WorkBuddy AI is the same plan's international build. Both
// serve it on an OpenAI-compatible endpoint, authenticated by the account's
// own JWT access token plus the account id and domain in headers.
//
// Sign-in is WorkBuddy's external-link flow: ask the plan for a state and a
// page, have the user sign in there, then poll for the token and the account.
// A near-expired access token is traded for a fresh one with the refresh
// token, as the desktop app does.
//
// Two upstream quirks are enforced here rather than left to the client, both
// from WorkBuddy's own refusals: a request must stream ("Non-stream chat
// request is currently not supported"), and its first message must be the
// system prompt ("first message is not system prompt").

const (
	// workBuddyAppVersion is the version the sign-in page is told, and
	// workBuddyUAVersion the desktop version copilot.tencent.com's gateway
	// expects in the User-Agent: it rejects an unrecognised one with code
	// 10085 "请求不合法".
	workBuddyAppVersion = "2.0.0"
	workBuddyUAVersion  = "5.5.6"

	workBuddyChatPath   = "/v2"
	workBuddyConfigPath = "/v3/config"

	// The sign-in and refresh endpoints, all under the plan's API root.
	workBuddyStatePath   = "/v2/plugin/auth/state"
	workBuddyTokenPath   = "/v2/plugin/auth/token"
	workBuddyAccountPath = "/v2/plugin/login/account"
	workBuddyRefreshPath = "/v2/plugin/auth/token/refresh"

	// workBuddyRetryTokenCode / workBuddyRetryAccountCode answer while the
	// user has not finished signing in; they mean "ask again", not "failed".
	workBuddyRetryTokenCode   = 11217
	workBuddyRetryAccountCode = 12151

	workBuddyPollInterval   = time.Second
	workBuddySignInDeadline = 5 * time.Minute
	workBuddyPendingTTL     = 15 * time.Minute
	// workBuddyTokenExpirySkew refreshes slightly before the token lapses.
	workBuddyTokenExpirySkew = time.Minute
)

// The two builds' API roots; vars so tests can point them at an httptest
// server.
var (
	workBuddyCNEndpoint   = "https://copilot.tencent.com"
	workBuddyIntlEndpoint = "https://www.workbuddy.ai"
)

// workBuddySite is one WorkBuddy build: its endpoint, the platform its
// sign-in names, whether it carries WorkBuddy AI's extra client headers, and
// the models its plan is known to serve.
type workBuddySite struct {
	authType   string
	name       string
	platform   string
	intl       bool
	endpoint   func() string
	models     []workBuddyModel
	wbClientID string
}

type workBuddyModel struct {
	ID      string
	Context int
}

var (
	workBuddyCNSite = workBuddySite{
		authType: domain.AuthTypeWorkBuddy,
		name:     "WorkBuddy",
		platform: "workbuddy",
		endpoint: func() string { return workBuddyCNEndpoint },
		models: []workBuddyModel{
			{ID: "auto", Context: 168_000},
			{ID: "hy4-preview-f", Context: 1_000_000},
			{ID: "hy3", Context: 192_000},
			{ID: "hy3-x", Context: 192_000},
			{ID: "deepseek-v4.1-flash", Context: 1_000_000},
			{ID: "glm-5.3", Context: 1_000_000},
			{ID: "glm-5.3-flash", Context: 1_000_000},
			{ID: "glm-5.2", Context: 1_000_000},
			{ID: "glm-5.1", Context: 200_000},
			{ID: "glm-5v-turbo", Context: 200_000},
			{ID: "minimax-m3", Context: 512_000},
			{ID: "kimi-k3-1", Context: 1_000_000},
			{ID: "kimi-k2.7", Context: 256_000},
			{ID: "kimi-k2.6", Context: 256_000},
			{ID: "deepseek-v4-pro", Context: 1_000_000},
		},
	}
	workBuddyIntlSite = workBuddySite{
		authType: domain.AuthTypeWorkBuddyAI,
		name:     "WorkBuddy AI",
		platform: "workbuddy-ai",
		intl:     true,
		endpoint: func() string { return workBuddyIntlEndpoint },
		models: []workBuddyModel{
			{ID: "default-model", Context: 176_000},
			{ID: "fast-model", Context: 200_000},
			{ID: "balanced-model", Context: 256_000},
			{ID: "primary-model", Context: 272_000},
			{ID: "deep-model", Context: 176_000},
			{ID: "gpt-5.5", Context: 1_000_000},
			{ID: "gpt-5.4", Context: 272_000},
			{ID: "gpt-5.3-codex", Context: 272_000},
			{ID: "gemini-3.1-pro", Context: 400_000},
			{ID: "gemini-3.5-flash", Context: 1_000_000},
			{ID: "glm-5.3", Context: 1_000_000},
			{ID: "glm-5.2", Context: 1_000_000},
			{ID: "hy3", Context: 192_000},
			{ID: "kimi-k3", Context: 1_000_000},
			{ID: "kimi-k2.6", Context: 256_000},
			{ID: "minimax-m3", Context: 512_000},
		},
	}
)

func workBuddySiteForAuthType(authType string) (workBuddySite, bool) {
	switch strings.TrimSpace(authType) {
	case domain.AuthTypeWorkBuddy:
		return workBuddyCNSite, true
	case domain.AuthTypeWorkBuddyAI:
		return workBuddyIntlSite, true
	}
	return workBuddySite{}, false
}

// isWorkBuddyAuthType reports whether an auth type authenticates with a
// WorkBuddy subscription, whichever build it is.
func isWorkBuddyAuthType(authType string) bool {
	_, ok := workBuddySiteForAuthType(authType)
	return ok
}

// isWorkBuddyProvider reports whether a provider authenticates with a
// WorkBuddy subscription, whichever build it is.
func isWorkBuddyProvider(provider domain.Provider) bool {
	return isWorkBuddyAuthType(provider.AuthType)
}

// chatBaseURL is the OpenAI-compatible root the plan is served on; the
// gateway appends /chat/completions to it like it does for any OpenAI-Chat
// provider.
func (w workBuddySite) chatBaseURL() string {
	return strings.TrimRight(w.endpoint(), "/") + workBuddyChatPath
}

func (w workBuddySite) api() string { return strings.TrimRight(w.endpoint(), "/") }

// defaultModels is the plan's known model list, used until the product
// config answers with the live one.
func (w workBuddySite) defaultModels(providerID string) []domain.Model {
	out := make([]domain.Model, 0, len(w.models))
	for _, item := range w.models {
		out = append(out, domain.Model{
			ID:            item.ID,
			ProviderID:    providerID,
			Protocol:      domain.ProtocolOpenAIChat,
			ContextLength: item.Context,
			InMenu:        true,
		})
	}
	return out
}

// workBuddyDomain is the X-Domain a request carries: the account's own
// domain, or the endpoint's authority when the account did not say.
func workBuddyDomain(w workBuddySite, credential domain.WorkBuddyCredential) string {
	if domainName := strings.TrimSpace(credential.Domain); domainName != "" {
		return domainName
	}
	if parsed, err := url.Parse(w.api()); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return ""
}

// ---- signing in ------------------------------------------------------------

type workBuddyOAuthPending struct {
	State     string
	FlowID    string
	CreatedAt time.Time
}

type workBuddyOAuthFlowStatus struct {
	Status    string    `json:"status"`
	Message   string    `json:"message,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type workBuddyOAuthPendingStore struct {
	mu       sync.Mutex
	pending  map[string]workBuddyOAuthPending
	statuses map[string]workBuddyOAuthFlowStatus
}

func newWorkBuddyOAuthPendingStore() *workBuddyOAuthPendingStore {
	return &workBuddyOAuthPendingStore{
		pending:  map[string]workBuddyOAuthPending{},
		statuses: map[string]workBuddyOAuthFlowStatus{},
	}
}

func (store *workBuddyOAuthPendingStore) put(providerID string, entry workBuddyOAuthPending) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.pending[providerID] = entry
}

func (store *workBuddyOAuthPendingStore) take(providerID string) (workBuddyOAuthPending, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.pending[providerID]
	if !ok {
		return workBuddyOAuthPending{}, false
	}
	delete(store.pending, providerID)
	if time.Since(entry.CreatedAt) > workBuddyPendingTTL {
		return workBuddyOAuthPending{}, false
	}
	return entry, true
}

func (store *workBuddyOAuthPendingStore) finish(providerID string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.pending, providerID)
}

func (store *workBuddyOAuthPendingStore) setStatus(flowID, status, message string) {
	if strings.TrimSpace(flowID) == "" {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.statuses[flowID] = workBuddyOAuthFlowStatus{
		Status:    status,
		Message:   message,
		UpdatedAt: time.Now(),
	}
}

func (store *workBuddyOAuthPendingStore) status(flowID string) (workBuddyOAuthFlowStatus, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.statuses[flowID]
	return entry, ok
}

// workBuddySignInURL is the page the user signs in on: WorkBuddy's own
// authUrl plus the app version and a fresh login session id its page expects.
func workBuddySignInURL(authURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(authURL))
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "https" {
		return "", errors.New("WorkBuddy 没有给出有效的登录页面")
	}
	session := make([]byte, 16)
	if _, err = rand.Read(session); err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("version", workBuddyAppVersion)
	query.Set("loginSessionId", hex.EncodeToString(session))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

type workBuddyCredentialPayload struct {
	AccessToken      string `json:"accessToken"`
	RefreshToken     string `json:"refreshToken"`
	ExpiresAt        int64  `json:"expiresAt"`
	ExpiresIn        int64  `json:"expiresIn"`
	RefreshExpiresAt int64  `json:"refreshExpiresAt"`
	RefreshExpiresIn int64  `json:"refreshExpiresIn"`
	Domain           string `json:"domain"`
	TokenType        string `json:"tokenType"`
}

// toCredential turns what WorkBuddy's token endpoint answered into the stored
// credential, normalising the two ways it can express an expiry (an absolute
// millisecond timestamp, or a lifetime in seconds from now).
func (p workBuddyCredentialPayload) toCredential(account domain.WorkBuddyCredential) domain.WorkBuddyCredential {
	now := time.Now()
	out := account
	out.AccessToken = strings.TrimSpace(p.AccessToken)
	if refresh := strings.TrimSpace(p.RefreshToken); refresh != "" {
		out.RefreshToken = refresh
	}
	if domainName := strings.TrimSpace(p.Domain); domainName != "" {
		out.Domain = domainName
	}
	switch {
	case p.ExpiresAt > 0:
		out.ExpiresAt = time.UnixMilli(p.ExpiresAt).UTC().Format(time.RFC3339)
	case p.ExpiresIn > 0:
		out.ExpiresAt = now.Add(time.Duration(p.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	return out
}

// workBuddyAccountLabel names a signed-in account for the console: its
// nickname, its phone number, or its id.
func workBuddyAccountLabel(nickname, phone, uid string) string {
	for _, candidate := range []string{nickname, phone, uid} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return "WorkBuddy"
}

// ---- the plan's JSON API ---------------------------------------------------

// workBuddyError is a WorkBuddy business error. Its code lets the sign-in
// poll tell a "not ready yet, ask again" answer from a real failure.
type workBuddyError struct {
	code int
	msg  string
}

func (e *workBuddyError) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return fmt.Sprintf("错误码 %d", e.code)
}

func workBuddyErrorCode(err error) (int, bool) {
	var target *workBuddyError
	if errors.As(err, &target) {
		return target.code, true
	}
	return 0, false
}

// workBuddyCall asks one of WorkBuddy's JSON endpoints, which wrap what they
// say in {code, msg, data}; code 0 is a success and dst reads data.
func workBuddyCall(ctx context.Context, method, endpoint string, headers map[string]string, body, dst any) error {
	var reader io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "WorkBuddy/"+workBuddyUAVersion)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var envelope struct {
		Code json.Number     `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(payload, &envelope)
	code, _ := envelope.Code.Int64()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if code != 0 {
			return &workBuddyError{code: int(code), msg: envelope.Msg}
		}
		if envelope.Msg != "" {
			return &workBuddyError{code: response.StatusCode, msg: envelope.Msg}
		}
		return fmt.Errorf("WorkBuddy 返回 HTTP %d", response.StatusCode)
	}
	if code != 0 {
		return &workBuddyError{code: int(code), msg: envelope.Msg}
	}
	if dst == nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil
	}
	return json.Unmarshal(envelope.Data, dst)
}

// ---- the sign-in flow ------------------------------------------------------

func (s *Server) startWorkBuddyOAuth(providerID string, site workBuddySite) (authURL, flowID string, err error) {
	var state struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	// The state endpoint sits behind the plan's auth gate even though nothing
	// is signed in yet; these headers are how WorkBuddy says "ask it bare".
	headers := map[string]string{
		"X-No-Authorization":   "true",
		"X-No-User-Id":         "true",
		"X-No-Enterprise-Id":   "true",
		"X-No-Department-Info": "true",
	}
	ctx, cancel := context.WithTimeout(context.Background(), workBuddySignInDeadline)
	endpoint := site.api() + workBuddyStatePath + "?platform=" + url.QueryEscape(site.platform)
	if callErr := workBuddyCall(ctx, http.MethodPost, endpoint, headers, map[string]any{}, &state); callErr != nil {
		cancel()
		return "", "", fmt.Errorf("WorkBuddy 登录初始化失败：%w", callErr)
	}
	if strings.TrimSpace(state.State) == "" || strings.TrimSpace(state.AuthURL) == "" {
		cancel()
		return "", "", errors.New("WorkBuddy 没有给出登录页面")
	}
	resolved, urlErr := workBuddySignInURL(state.AuthURL)
	if urlErr != nil {
		cancel()
		return "", "", urlErr
	}
	idBytes := make([]byte, 16)
	if _, err = rand.Read(idBytes); err != nil {
		cancel()
		return "", "", err
	}
	flowID = hex.EncodeToString(idBytes)
	s.pendingWorkBuddyOAuth.put(providerID, workBuddyOAuthPending{
		State:     state.State,
		FlowID:    flowID,
		CreatedAt: time.Now(),
	})
	s.pendingWorkBuddyOAuth.setStatus(flowID, "pending", "等待浏览器授权…")
	go s.pollWorkBuddyOAuthInBackground(ctx, cancel, providerID, flowID, site, state.State)
	return resolved, flowID, nil
}

// pollWorkBuddyOAuthInBackground waits for the user to finish signing in, then
// takes the token and the account the sign-in produced and stores them on the
// provider. WorkBuddy hands the two out on separate endpoints, each answered
// with a "not ready" code until the browser flow completes.
func (s *Server) pollWorkBuddyOAuthInBackground(
	ctx context.Context,
	cancel context.CancelFunc,
	providerID, flowID string,
	site workBuddySite,
	state string,
) {
	defer cancel()
	fail := func(message string) {
		s.pendingWorkBuddyOAuth.setStatus(flowID, "failed", message)
		s.pendingWorkBuddyOAuth.finish(providerID)
		s.logs.AddApp("warn", "workbuddy oauth flow failed", fmt.Sprintf("provider=%s %s", providerID, message))
	}

	var token workBuddyCredentialPayload
	tokenEndpoint := site.api() + workBuddyTokenPath + "?state=" + url.QueryEscape(state)
	if err := s.pollWorkBuddyJSON(ctx, site, tokenEndpoint, nil, workBuddyRetryTokenCode, func(payload json.RawMessage) error {
		return json.Unmarshal(payload, &token)
	}); err != nil {
		fail("WorkBuddy 登录失败：" + err.Error())
		return
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		fail("WorkBuddy 没有返回访问令牌")
		return
	}

	var account domain.WorkBuddyCredential
	credential := token.toCredential(account)
	headers := map[string]string{
		"Authorization":      "Bearer " + credential.AccessToken,
		"X-Domain":           workBuddyDomain(site, credential),
		"X-No-User-Id":       "true",
		"X-No-Enterprise-Id": "true",
	}
	var who struct {
		UID         string `json:"uid"`
		Nickname    string `json:"nickname"`
		PhoneNumber string `json:"phoneNumber"`
	}
	accountEndpoint := site.api() + workBuddyAccountPath + "?state=" + url.QueryEscape(state)
	if err := s.pollWorkBuddyJSON(ctx, site, accountEndpoint, headers, workBuddyRetryAccountCode, func(payload json.RawMessage) error {
		return json.Unmarshal(payload, &who)
	}); err != nil {
		fail("WorkBuddy 登录失败：" + err.Error())
		return
	}
	if strings.TrimSpace(who.UID) == "" {
		fail("WorkBuddy 没有返回账号信息")
		return
	}
	credential.UserID = strings.TrimSpace(who.UID)
	credential.AccountLabel = workBuddyAccountLabel(who.Nickname, who.PhoneNumber, who.UID)

	updated, err := s.router.SetProviderWorkBuddy(providerID, site.authType, credential)
	if err != nil {
		fail("WorkBuddy 登录失败：" + err.Error())
		return
	}
	if err := s.persistProviderOAuth(updated); err != nil {
		s.logs.AddApp("warn", "failed to persist workbuddy oauth token", err.Error())
	}
	if err := s.saveState(); err != nil {
		fail("WorkBuddy 登录失败：配置保存失败 " + err.Error())
		return
	}
	if synced, syncErr := s.syncWorkBuddyModels(providerID); syncErr == nil {
		updated = synced
	} else {
		s.logs.AddApp("warn", "workbuddy model sync failed", syncErr.Error())
	}
	s.pendingWorkBuddyOAuth.finish(providerID)
	s.pendingWorkBuddyOAuth.setStatus(flowID, "connected", "connected")
	s.logs.AddApp("info", "workbuddy oauth connected", fmt.Sprintf("provider=%s account=%s", providerID, credential.AccountLabel))
}

// pollWorkBuddyJSON asks endpoint every second until it answers with data,
// reading it through consume, and gives up (with the last error) when the
// flow's context ends. A retry code means the user has not finished yet.
func (s *Server) pollWorkBuddyJSON(
	ctx context.Context,
	site workBuddySite,
	endpoint string,
	headers map[string]string,
	retryCode int,
	consume func(json.RawMessage) error,
) error {
	for {
		select {
		case <-ctx.Done():
			return errors.New("登录超时，请重新开始")
		case <-time.After(workBuddyPollInterval):
		}
		var raw json.RawMessage
		err := workBuddyCall(ctx, http.MethodGet, endpoint, headers, nil, &raw)
		switch {
		case ctx.Err() != nil:
			return errors.New("登录超时，请重新开始")
		case err == nil && len(raw) > 0 && string(raw) != "null":
			return consume(raw)
		case err == nil:
			continue // ready but empty: ask again
		}
		if code, ok := workBuddyErrorCode(err); ok && code == retryCode {
			continue // not signed in yet
		}
		return err
	}
}

// ---- token refresh ---------------------------------------------------------

func workBuddyTokenNeedsRefresh(credential *domain.WorkBuddyCredential) bool {
	if credential == nil {
		return true
	}
	expiresAt := strings.TrimSpace(credential.ExpiresAt)
	if expiresAt == "" {
		return false // WorkBuddy didn't say; trust the token until it refuses
	}
	parsed, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return false // unparseable expiry: fail open, the request will show it
	}
	return time.Now().Add(workBuddyTokenExpirySkew).After(parsed)
}

func workBuddyRefreshRequestHeaders(site workBuddySite, credential domain.WorkBuddyCredential) map[string]string {
	return map[string]string{
		"X-Refresh-Token":       credential.RefreshToken,
		"X-Auth-Refresh-Source": "plugin",
		"X-Domain":              workBuddyDomain(site, credential),
	}
}

// refreshWorkBuddyCredential trades the refresh token for a fresh access
// token, as WorkBuddy's own auth provider does.
func refreshWorkBuddyCredential(ctx context.Context, site workBuddySite, credential domain.WorkBuddyCredential) (domain.WorkBuddyCredential, error) {
	var got workBuddyCredentialPayload
	endpoint := site.api() + workBuddyRefreshPath
	err := workBuddyCall(ctx, http.MethodPost, endpoint, workBuddyRefreshRequestHeaders(site, credential), map[string]any{}, &got)
	if err != nil {
		return credential, fmt.Errorf("WorkBuddy 令牌刷新失败: %w", err)
	}
	if strings.TrimSpace(got.AccessToken) == "" {
		return credential, errors.New("WorkBuddy 没有返回刷新后的令牌")
	}
	return got.toCredential(credential), nil
}

// ensureFreshWorkBuddyToken guarantees the provider holds a usable access
// token. Called on every proxied request, so the unexpired path performs no
// network I/O at all.
func (s *Server) ensureFreshWorkBuddyToken(provider domain.Provider) (domain.Provider, error) {
	site, ok := workBuddySiteForAuthType(provider.AuthType)
	if !ok {
		return provider, nil
	}
	credential := provider.WorkBuddy
	if credential == nil || strings.TrimSpace(credential.AccessToken) == "" {
		return domain.Provider{}, errors.New("WorkBuddy 尚未登录，请先在控制台完成登录")
	}
	if !workBuddyTokenNeedsRefresh(credential) {
		return provider, nil
	}
	if strings.TrimSpace(credential.RefreshToken) == "" {
		return provider, nil // nothing to refresh with; let the request try
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	refreshed, err := refreshWorkBuddyCredential(ctx, site, *credential)
	if err != nil {
		// A refresh hiccup is not fatal: the current token may still work.
		s.logs.AddApp("warn", "workbuddy token refresh failed", fmt.Sprintf("provider=%s %s", provider.ID, err.Error()))
		return provider, nil
	}
	updated, err := s.router.SetProviderWorkBuddy(provider.ID, site.authType, refreshed)
	if err != nil {
		return domain.Provider{}, err
	}
	if err := s.persistProviderOAuth(updated); err != nil {
		s.logs.AddApp("warn", "failed to persist workbuddy token refresh", err.Error())
	}
	markTimingFlag(context.Background(), timingFlagOAuthRefresh)
	markTimingFlag(context.Background(), timingFlagSaveState)
	return updated, nil
}

// ---- request shaping -------------------------------------------------------

// workBuddyRequestID is a new id as WorkBuddy makes them: 32 hex digits.
func workBuddyRequestID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// workBuddySystemPrompt is the system message a chat that has none is sent
// with: WorkBuddy refuses one whose first message isn't the system prompt, and
// scripts and plain chat clients often send none.
const workBuddySystemPrompt = "You are a helpful assistant."

// prepareWorkBuddyChatBody rewrites an OpenAI-Chat request into the shape
// WorkBuddy accepts: streaming forced on (it refuses a non-stream request) and
// a leading system message. The result is only ever a stream, so a client that
// asked for a single JSON object gets one rebuilt by
// workBuddyStreamAsCompletion.
func prepareWorkBuddyChatBody(body []byte) ([]byte, bool) {
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil || payload == nil {
		return body, false
	}
	messages, hasMessages := payload["messages"].([]any)
	if !hasMessages {
		return body, false // not a chat request (count_tokens, embeddings, …)
	}
	changed := false
	if streaming, _ := payload["stream"].(bool); !streaming {
		payload["stream"] = true
		changed = true
	}
	if len(messages) > 0 {
		if first, isObject := messages[0].(map[string]any); !isObject || first["role"] != "system" {
			payload["messages"] = append([]any{map[string]any{"role": "system", "content": workBuddySystemPrompt}}, messages...)
			changed = true
		}
	}
	if !changed {
		return body, false
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(payload) != nil {
		return body, false
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), true
}

// applyWorkBuddyHeaders authenticates a request with the account's WorkBuddy
// session. It runs after the generic provider auth, and overwrites the
// Authorization the API-key path may have set: a WorkBuddy request is only
// ever signed by its account.
func applyWorkBuddyHeaders(request *http.Request, provider domain.Provider) {
	site, ok := workBuddySiteForAuthType(provider.AuthType)
	if !ok || provider.WorkBuddy == nil {
		return
	}
	credential := provider.WorkBuddy
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(credential.AccessToken))
	request.Header.Set("X-User-Id", strings.TrimSpace(credential.UserID))
	request.Header.Set("X-Domain", workBuddyDomain(site, *credential))
	request.Header.Set("X-Product", "SaaS")
	request.Header.Set("X-IDE-Type", "WorkBuddy")
	request.Header.Set("User-Agent", "WorkBuddy/"+workBuddyUAVersion)
	if site.intl {
		applyWorkBuddyClientHeaders(request)
	}
}

// applyWorkBuddyClientHeaders are the rest of what WorkBuddy AI's own chats
// carry: the request marked as an XHR, the agent's intent, the client's name
// and version, and the conversation and request ids. Only the international
// build needs them; its gateway answers an "illegal API invocation from an
// unapproved channel" without them.
func applyWorkBuddyClientHeaders(request *http.Request) {
	request.Header.Set("X-Requested-With", "XMLHttpRequest")
	request.Header.Set("X-Agent-Intent", "craft")
	request.Header.Set("X-Agent-Type", "main")
	request.Header.Set("X-IDE-Name", "WorkBuddy")
	request.Header.Set("X-IDE-Version", workBuddyUAVersion)
	for _, header := range []string{"X-Conversation-ID", "X-Conversation-Request-ID"} {
		if request.Header.Get(header) == "" {
			request.Header.Set(header, workBuddyRequestID())
		}
	}
	request.Header.Set("X-Conversation-Message-ID", workBuddyRequestID())
	request.Header.Set("X-Request-ID", workBuddyRequestID())
}

// ---- streaming for clients that asked for one JSON object ------------------

// workBuddyStreamAsCompletion rebuilds a chat.completion from the SSE stream
// WorkBuddy sent, for a client that asked for a non-streaming request. The
// gateway always streams upstream because WorkBuddy refuses otherwise, so this
// is what makes a plain "stream": false caller still get a plain JSON answer.
func workBuddyStreamAsCompletion(streamBody []byte, model string) ([]byte, error) {
	completion, err := buildWorkBuddyCompletion(streamBody, model)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(completion)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

type workBuddyStreamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// workBuddyToolCall accumulates one streamed tool call: WorkBuddy, like any
// OpenAI-compatible stream, splits a call's arguments across chunks.
type workBuddyToolCall struct {
	id        string
	name      string
	arguments strings.Builder
}

func buildWorkBuddyCompletion(streamBody []byte, model string) (map[string]any, error) {
	var (
		completionID   string
		created        int64
		streamModel    = strings.TrimSpace(model)
		role           = "assistant"
		content        strings.Builder
		reasoning      strings.Builder
		finishReason   string
		usage          map[string]any
		toolCalls      = map[int]*workBuddyToolCall{}
		toolCallOrder  []int
		sawChunk       bool
		upstreamErrMsg string
	)
	for _, line := range strings.Split(string(streamBody), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if line == "" || line == "[DONE]" {
			continue
		}
		var chunk workBuddyStreamChunk
		if json.Unmarshal([]byte(line), &chunk) != nil {
			continue
		}
		if chunk.Error != nil {
			upstreamErrMsg = strings.TrimSpace(chunk.Error.Message)
			continue
		}
		if chunk.ID != "" {
			completionID = chunk.ID
		}
		if chunk.Created > 0 {
			created = chunk.Created
		}
		if strings.TrimSpace(chunk.Model) != "" {
			streamModel = strings.TrimSpace(chunk.Model)
		}
		sawChunk = true
		if chunk.Usage != nil {
			usage = map[string]any{
				"prompt_tokens":     chunk.Usage.PromptTokens,
				"completion_tokens": chunk.Usage.CompletionTokens,
				"total_tokens":      chunk.Usage.TotalTokens,
			}
			if chunk.Usage.PromptTokensDetails.CachedTokens != 0 {
				usage["prompt_tokens_details"] = map[string]any{"cached_tokens": chunk.Usage.PromptTokensDetails.CachedTokens}
			}
			if chunk.Usage.CompletionTokensDetails.ReasoningTokens != 0 {
				usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": chunk.Usage.CompletionTokensDetails.ReasoningTokens}
			}
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Role != "" {
				role = choice.Delta.Role
			}
			content.WriteString(choice.Delta.Content)
			reasoning.WriteString(choice.Delta.ReasoningContent)
			for index, call := range choice.Delta.ToolCalls {
				slot := 0
				if call.Index != nil {
					slot = *call.Index
				} else {
					slot = index
				}
				entry, ok := toolCalls[slot]
				if !ok {
					entry = &workBuddyToolCall{}
					toolCalls[slot] = entry
					toolCallOrder = append(toolCallOrder, slot)
				}
				if call.ID != "" {
					entry.id = call.ID
				}
				if call.Function.Name != "" {
					entry.name = call.Function.Name
				}
				entry.arguments.WriteString(call.Function.Arguments)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
		}
	}
	if !sawChunk {
		if upstreamErrMsg != "" {
			return nil, fmt.Errorf("WorkBuddy 流式响应出错：%s", upstreamErrMsg)
		}
		return nil, errors.New("WorkBuddy 的流式响应没有内容")
	}
	message := map[string]any{"role": role, "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCallOrder) > 0 {
		calls := make([]map[string]any, 0, len(toolCallOrder))
		for _, slot := range toolCallOrder {
			entry := toolCalls[slot]
			calls = append(calls, map[string]any{
				"id":   entry.id,
				"type": "function",
				"function": map[string]any{
					"name":      entry.name,
					"arguments": entry.arguments.String(),
				},
			})
		}
		message["tool_calls"] = calls
	}
	if finishReason == "" {
		finishReason = "stop"
	}
	if completionID == "" {
		completionID = "chatcmpl-workbuddy"
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	if usage == nil {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	out := map[string]any{
		"id":      completionID,
		"object":  "chat.completion",
		"created": created,
		"model":   streamModel,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": usage,
	}
	return out, nil
}

// rewriteWorkBuddyUpstreamStreamedBody turns the upstream SSE body into a
// single chat.completion, and reopens it on the response so the passthrough
// writer can read it as it would any buffered body.
func rewriteWorkBuddyUpstreamStreamedBody(response *http.Response, model string) error {
	payload, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	response.Body.Close()
	if err != nil {
		response.Body = io.NopCloser(bytes.NewReader(payload))
		return err
	}
	if response.StatusCode >= 400 || !bytes.Contains(payload, []byte("data:")) {
		response.Body = io.NopCloser(bytes.NewReader(payload))
		return nil
	}
	completion, err := workBuddyStreamAsCompletion(payload, model)
	if err != nil {
		response.Body = io.NopCloser(bytes.NewReader(payload))
		return nil // forward what came back rather than inventing an error
	}
	response.Body = io.NopCloser(bytes.NewReader(completion))
	response.ContentLength = int64(len(completion))
	response.Header.Set("Content-Type", "application/json; charset=utf-8")
	return nil
}

// ---- models ----------------------------------------------------------------

// workBuddyProductConfig is the plan's product config: its agents and each
// model's details. Which product answers goes by the User-Agent — WorkBuddy
// sends "CLI/<version> WorkBuddy/<version>", and a bare WorkBuddy/<version>
// gets CodeBuddy IDE's config, which lists no CLI agent.
type workBuddyProductConfig struct {
	Agents []struct {
		Name   string   `json:"name"`
		Models []string `json:"models"`
	} `json:"agents"`
	Models []struct {
		ID              string `json:"id"`
		MaxInputTokens  int    `json:"maxInputTokens"`
		MaxOutputTokens int    `json:"maxOutputTokens"`
	} `json:"models"`
}

// fetchWorkBuddyModels asks the plan's product config for the models its CLI
// agent is served.
func fetchWorkBuddyModels(ctx context.Context, site workBuddySite, credential domain.WorkBuddyCredential) ([]domain.Model, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, site.api()+workBuddyConfigPath, nil)
	if err != nil {
		return nil, err
	}
	applyWorkBuddyHeaders(request, domain.Provider{AuthType: site.authType, WorkBuddy: &credential})
	request.Header.Set("User-Agent", "CLI/"+workBuddyAppVersion+" WorkBuddy/"+workBuddyUAVersion)
	request.Header.Set("X-Requested-With", "XMLHttpRequest")
	request.Header.Set("Accept", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("WorkBuddy 模型列表: HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Code int                    `json:"code"`
		Msg  string                 `json:"msg"`
		Data workBuddyProductConfig `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("WorkBuddy 模型列表: %w", err)
	}
	if envelope.Code != 0 {
		return nil, fmt.Errorf("WorkBuddy 模型列表: %s (%d)", envelope.Msg, envelope.Code)
	}
	out := make([]domain.Model, 0, len(envelope.Data.Models))
	for _, agent := range envelope.Data.Agents {
		if agent.Name != "cli" {
			continue
		}
		for _, id := range agent.Models {
			model := domain.Model{
				ID:            id,
				Protocol:      domain.ProtocolOpenAIChat,
				ContextLength: resolveModelContextLength(id, 0),
				InMenu:        true,
			}
			for _, detail := range envelope.Data.Models {
				if detail.ID != id {
					continue
				}
				model.ContextLength = detail.MaxInputTokens
				model.MaxOutputTokens = detail.MaxOutputTokens
				break
			}
			out = append(out, model)
		}
	}
	for index := range out {
		out[index].ProviderID = ""
	}
	return out, nil
}

// syncWorkBuddyModels stores the plan's live model list on the provider,
// falling back to the known list when the config can't be read.
func (s *Server) syncWorkBuddyModels(providerID string) (domain.Provider, error) {
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		return domain.Provider{}, err
	}
	site, ok := workBuddySiteForAuthType(provider.AuthType)
	if !ok {
		return provider, nil
	}
	credential := provider.WorkBuddy
	if credential == nil {
		return s.storeWorkBuddyModels(provider, site.defaultModels(provider.ID))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	models, err := fetchWorkBuddyModels(ctx, site, *credential)
	if err != nil || len(models) == 0 {
		if err != nil {
			s.logs.AddApp("warn", "workbuddy product config unavailable, using known models", err.Error())
		}
		return s.storeWorkBuddyModels(provider, site.defaultModels(provider.ID))
	}
	for index := range models {
		models[index].ProviderID = providerID
	}
	return s.storeWorkBuddyModels(provider, models)
}

func (s *Server) storeWorkBuddyModels(provider domain.Provider, models []domain.Model) (domain.Provider, error) {
	updated, err := s.router.UpdateProviderModels(provider.ID, models, "healthy")
	if err != nil {
		return provider, err
	}
	if err := s.saveState(); err != nil {
		return updated, err
	}
	return updated, nil
}

// workBuddyQuota is an account's credit allowance, read the way WorkBuddy's
// own usage page reads it.
type workBuddyQuota struct {
	Plan      string              `json:"plan,omitempty"`
	User      string              `json:"user,omitempty"`
	Windows   []workBuddyQuotaWin `json:"windows"`
	Error     string              `json:"error,omitempty"`
	FetchedAt string              `json:"fetchedAt,omitempty"`
}

type workBuddyQuotaWin struct {
	Name   string  `json:"name"`
	Used   float64 `json:"used"`
	Amount float64 `json:"amount"`
	Limit  float64 `json:"limit"`
	Unit   string  `json:"unit"`
}

// workBuddyNumber reads a capacity the billing API sends as a JSON string
// ("3300", "438.88"), though it occasionally comes as a bare number.
type workBuddyNumber float64

func (n *workBuddyNumber) UnmarshalJSON(raw []byte) error {
	trimmed := strings.Trim(string(raw), `"`)
	if trimmed == "" || trimmed == "null" {
		*n = 0
		return nil
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return err
	}
	*n = workBuddyNumber(parsed)
	return nil
}

// workBuddyQuotaFor reads an account's credit allowance from its resource
// summary, and from the enterprise meter when the summary grants no packages
// (a team account's credits only live there).
func workBuddyQuotaFor(ctx context.Context, site workBuddySite, credential domain.WorkBuddyCredential) workBuddyQuota {
	quota := workBuddyQuota{User: credential.AccountLabel, Windows: []workBuddyQuotaWin{}}
	headers := map[string]string{
		"Authorization": "Bearer " + credential.AccessToken,
		"X-User-Id":     credential.UserID,
		"X-Domain":      workBuddyDomain(site, credential),
		"X-Product":     "SaaS",
		"X-IDE-Type":    "WorkBuddy",
	}
	// The resource-summary meter has no /v2 gateway prefix: WorkBuddy asks it
	// at /billing/meter/... on both desktop and web.
	var summary struct {
		Packages   []struct{ CycleTotalCapacity, CycleUsedCapacity workBuddyNumber } `json:"Packages"`
		IsPaidUser bool                                                              `json:"IsPaidUser"`
	}
	if err := workBuddyCall(ctx, http.MethodPost, site.api()+"/billing/meter/get-user-resource-summary", headers, map[string]any{}, &summary); err != nil {
		quota.Error = err.Error()
		return quota
	}
	if summary.IsPaidUser {
		quota.Plan = "Pro"
	} else {
		quota.Plan = "Free"
	}
	var total, used float64
	for _, pkg := range summary.Packages {
		total += float64(pkg.CycleTotalCapacity)
		used += float64(pkg.CycleUsedCapacity)
	}
	if total > 0 {
		quota.Windows = append(quota.Windows, workBuddyQuotaWin{
			Name: "Credits", Used: 100 * used / total, Amount: used, Limit: total, Unit: "credits",
		})
		quota.FetchedAt = time.Now().UTC().Format(time.RFC3339)
		return quota
	}
	var enterprise struct {
		Credit   workBuddyNumber `json:"credit"`
		LimitNum workBuddyNumber `json:"limitNum"`
	}
	if err := workBuddyCall(ctx, http.MethodPost, site.api()+"/billing/meter/get-enterprise-user-usage", headers, map[string]any{}, &enterprise); err == nil {
		if limit := float64(enterprise.LimitNum); limit > 0 {
			quota.Plan = "Enterprise"
			quota.Windows = append(quota.Windows, workBuddyQuotaWin{
				Name: "Credits", Used: 100 * float64(enterprise.Credit) / limit,
				Amount: float64(enterprise.Credit), Limit: limit, Unit: "credits",
			})
		}
	}
	quota.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	return quota
}
