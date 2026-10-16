package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// ZCode is Z.ai's and BigModel's (智谱) GLM Coding Plan client. It is a
// sign-in and a key service, not an inference API: signing in through ZCode's
// own browser flow produces (or reuses) an API key attached to the account's
// default project, and that key is what the plan's Anthropic-compatible
// endpoint bills against. So a ZCode provider forwards as an ordinary Claude
// provider — x-api-key and bearer both carrying the minted key — and only the
// sign-in, the model list and the quota card are ZCode's own.
//
// Two realms, chosen by the connection kind rather than by a setting: Z.ai
// (api.z.ai) and BigModel (open.bigmodel.cn). Their sign-in is the same page on
// zcode.z.ai, but their business APIs differ in one detail that has to be
// right: Z.ai exchanges the sign-in's token for a business token and sends it
// as a bearer, while BigModel uses the token as it came and sends it bare.

const (
	// zcodeAppVersion is the ZCode release this integration asks as. ZCode
	// gates its free plan on looking like its own client, and its release
	// config is fetched by version, so this is the version pinned here.
	zcodeAppVersion = "3.14.3"
	zcodeUserAgent  = "ZCode/" + zcodeAppVersion

	zcodeInitPath        = "/api/v1/oauth/cli/init"
	zcodePollPath        = "/api/v1/oauth/cli/poll/"
	zcodeConfigsPath     = "/api/v1/client/configs"
	zcodeLoginPath       = "/api/auth/z/login"
	zcodeCustomerPath    = "/api/biz/customer/getCustomerInfo"
	zcodeSubscriptionKey = "/api/biz/subscription/list"
	zcodeTeamDetailPath  = "/api/biz/team/subscribe/product/querySubscribeDetail"
	zcodeKeysPathFmt     = "/api/biz/v1/organization/%s/projects/%s/api_keys"
	zcodeKeyCopyFmt      = "/copy/%s"

	zcodePersonalKeyName = "zcode-api-key"
	zcodeTeamKeyName     = "zcode-team-api-key"

	// zcodeSignInDeadline bounds the browser flow. ZCode's own expiry on the
	// flow is shorter, and is read from the init answer when it gives one.
	zcodeSignInDeadline = 10 * time.Minute

	zcodeKeyNotFound = "该账号还没有 ZCode 项目密钥"
)

// zcodeSiteHost is ZCode's own site: where a browser signs in and where the
// release config is read. A variable rather than a constant so a test can point
// a whole sign-in at a local server.
var zcodeSiteHost = "https://zcode.z.ai"

// zcodeSite is one ZCode realm and the endpoints it owns.
type zcodeSite struct {
	id    string
	label string
	// siteAPI is where the business calls go during sign-in: api.z.ai for
	// Z.ai, bigmodel.cn for BigModel.
	siteAPI string
	// inference is the endpoint a plan's key bills against, and root is its
	// host without the path, where the quota and subscription calls go.
	inference string
	root      string
	// providerConfigID names this realm inside ZCode's release config, which
	// is where the plan's model list comes from.
	providerConfigID string
	providerParam    string // "zai" | "bigmodel", as the init call names it
	subscribe        string
	// bearerBiz says whether the business token goes as a bearer (Z.ai) or
	// bare (BigModel).
	bearerBiz bool
}

var zcodeSites = map[string]zcodeSite{
	"zai": {
		id:               "zai",
		label:            "Z.ai",
		siteAPI:          "https://api.z.ai",
		inference:        "https://api.z.ai/api/anthropic",
		root:             "https://api.z.ai",
		providerConfigID: "account:zai-individual-coding-plan",
		providerParam:    "zai",
		subscribe:        "https://z.ai/subscribe",
		bearerBiz:        true,
	},
	"bigmodel": {
		id:               "bigmodel",
		label:            "BigModel (智谱)",
		siteAPI:          "https://bigmodel.cn",
		inference:        "https://open.bigmodel.cn/api/anthropic",
		root:             "https://open.bigmodel.cn",
		providerConfigID: "account:bigmodel-individual-coding-plan",
		providerParam:    "bigmodel",
		subscribe:        "https://bigmodel.cn/glm-coding",
		bearerBiz:        false,
	},
}

// The two ZCode realms are two connection kinds, the way WorkBuddy's China and
// international builds are: a signed-in provider never has to say which it is.
func zcodeSiteForAuthType(authType string) (zcodeSite, bool) {
	switch authType {
	case domain.AuthTypeZCode:
		return zcodeSites["zai"], true
	case domain.AuthTypeZCodeBigModel:
		return zcodeSites["bigmodel"], true
	}
	return zcodeSite{}, false
}

func isZCodeAuthType(authType string) bool {
	return authType == domain.AuthTypeZCode || authType == domain.AuthTypeZCodeBigModel
}

func isZCodeProvider(provider domain.Provider) bool {
	return isZCodeAuthType(provider.AuthType)
}

// zcodeSiteForProvider resolves a provider's realm. The credential kept with a
// signed-in account says which realm it was minted in; until then it is the
// connection kind.
func zcodeSiteForProvider(provider domain.Provider) (zcodeSite, bool) {
	if !isZCodeAuthType(provider.AuthType) {
		return zcodeSite{}, false
	}
	if provider.ZCode != nil {
		if site, ok := zcodeSites[strings.TrimSpace(provider.ZCode.Site)]; ok {
			return site, true
		}
	}
	site, _ := zcodeSiteForAuthType(provider.AuthType)
	return site, true
}

// zcodePlatform is ZCode's own spelling of this machine, which is how its
// release config is asked: "win32-x64", "darwin-arm64", "linux-x64".
func zcodePlatform() string {
	os := runtime.GOOS
	switch os {
	case "windows":
		os = "win32"
	case "darwin", "linux":
	default:
		os = "linux"
	}
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x64"
	case "386":
		arch = "ia32"
	}
	return os + "-" + arch
}

// ---- the JSON envelope -----------------------------------------------------

// zcodeError is an answer ZCode refused: its own message and code, which carry
// what to do next (1113 means no plan, 3001 a missing header, 3012 a blocked
// client).
type zcodeError struct {
	Message string
	Status  int
	Code    int
}

func (e *zcodeError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("%s（HTTP %d，code %d）", e.Message, e.Status, e.Code)
	}
	return fmt.Sprintf("%s（HTTP %d）", e.Message, e.Status)
}

func zcodeErrorCode(err error) (int, bool) {
	var e *zcodeError
	if errors.As(err, &e) && e.Code != 0 {
		return e.Code, true
	}
	return 0, false
}

// zcodeEnvelope is the shape every ZCode business answer comes in. Success is
// code 0 or 200; anything else is the message, and the data is what the call
// asked for.
type zcodeEnvelope struct {
	Code    json.Number     `json:"code"`
	Msg     string          `json:"msg"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// zcodeCall makes one ZCode JSON request. Unlike most of this package's calls
// it is not a proxy: it reads the whole answer, so it is for the control-plane
// endpoints (sign-in, keys, quota, config), never for a model reply.
func zcodeCall(ctx context.Context, method, endpoint string, headers map[string]string, body, dst any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(raw))
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", zcodeUserAgent)
	for key, value := range headers {
		request.Header.Set(key, value)
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
	var envelope zcodeEnvelope
	if decodeErr := json.Unmarshal(payload, &envelope); decodeErr != nil {
		if response.StatusCode >= 400 {
			return &zcodeError{Message: zcodeStatusMessage(response, payload), Status: response.StatusCode}
		}
		return fmt.Errorf("ZCode 返回了无法解析的内容（HTTP %d）", response.StatusCode)
	}
	code, _ := envelope.Code.Int64()
	message := firstNonEmptyString(envelope.Msg, envelope.Message)
	if response.StatusCode >= 400 {
		if message == "" {
			message = zcodeStatusMessage(response, payload)
		}
		return &zcodeError{Message: message, Status: response.StatusCode, Code: int(code)}
	}
	if code != 0 && code != 200 {
		if message == "" {
			message = "ZCode 拒绝了该请求"
		}
		return &zcodeError{Message: message, Status: response.StatusCode, Code: int(code)}
	}
	if dst == nil {
		return nil
	}
	if len(envelope.Data) == 0 {
		return json.Unmarshal(payload, dst)
	}
	return json.Unmarshal(envelope.Data, dst)
}

func zcodeStatusMessage(response *http.Response, payload []byte) string {
	body := strings.TrimSpace(string(payload))
	if len(body) > 200 {
		body = body[:200]
	}
	if body == "" {
		return response.Status
	}
	return fmt.Sprintf("%s：%s", response.Status, body)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// zcodeHeaders builds the headers one control-plane call needs. The device id
// is sent only to zcode.z.ai itself — ZCode's own sites want it, its business
// APIs answer badly when they get it.
func zcodeHeaders(endpoint, authorization, device string) map[string]string {
	headers := map[string]string{}
	if strings.TrimSpace(authorization) != "" {
		headers["Authorization"] = authorization
	}
	if device != "" && strings.HasPrefix(endpoint, zcodeSiteHost) {
		headers["X-Device-Mid"] = device
	}
	return headers
}

// zcodeBearer prefixes a token for a call that wants it as a bearer.
func zcodeBearer(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		return token
	}
	return "Bearer " + token
}

// ---- signing in ------------------------------------------------------------

type zcodeInitAnswer struct {
	FlowID        string `json:"flow_id"`
	AuthorizeURL  string `json:"authorize_url"`
	ExpiresAt     int64  `json:"expires_at"`
	PollIntervalS int64  `json:"poll_interval_sec"`
	// PollToken is what ZCode hands back to poll the flow with. Its own secret
	// is accepted too, but the one the site issues is the one it documents, so
	// it is preferred when present.
	PollToken string `json:"poll_token"`
}

// zcodeAccessToken is one realm's access token inside the poll answer. BigModel
// has written the field under both spellings across its client versions, so
// both are read.
type zcodeAccessToken struct {
	AccessToken string `json:"access_token"`
	CamelCase   string `json:"accessToken"`
}

type zcodeUser struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
}

type zcodePollAnswer struct {
	Status   string           `json:"status"`
	Token    string           `json:"token"`
	ZAI      zcodeAccessToken `json:"zai"`
	BigModel zcodeAccessToken `json:"bigmodel"`
	User     zcodeUser        `json:"user"`
}

// startZCodeOAuth opens ZCode's browser sign-in for one realm and starts
// polling it in the background. The secret the poll asks with is what makes the
// poll the same flow the browser finished, and is never stored on the provider.
func (s *Server) startZCodeOAuth(providerID string, site zcodeSite) (authURL, flowID string, err error) {
	pollSecret := randomHex(32)
	device := newUUIDV4()
	if pollSecret == "" || device == "" {
		return "", "", errors.New("无法生成登录所需的随机数")
	}
	ctx, cancel := context.WithTimeout(context.Background(), zcodeSignInDeadline)
	var init zcodeInitAnswer
	err = zcodeCall(ctx, http.MethodPost, zcodeSiteHost+zcodeInitPath,
		zcodeHeaders(zcodeSiteHost+zcodeInitPath, zcodeBearer(pollSecret), device),
		map[string]any{"provider": site.providerParam}, &init)
	if err != nil {
		cancel()
		return "", "", fmt.Errorf("ZCode 登录初始化失败：%w", err)
	}
	resolved, urlErr := zcodeSignInURL(init.AuthorizeURL, site)
	if urlErr != nil {
		cancel()
		return "", "", urlErr
	}
	idBytes, _ := url.Parse(resolved)
	if idBytes == nil {
		cancel()
		return "", "", urlErr
	}
	pollWith := firstNonEmptyString(init.PollToken, pollSecret)
	flowID = pollSecret[:32]
	s.subscriptionOAuthFlows.put(providerID, subscriptionOAuthPending{
		State:     pollWith,
		FlowID:    flowID,
		CreatedAt: time.Now(),
	})
	s.subscriptionOAuthFlows.setStatus(flowID, "pending", "等待浏览器授权…")
	go s.pollZCodeOAuthInBackground(ctx, cancel, providerID, flowID, site, pollWith, device, init, time.Now())
	return resolved, flowID, nil
}

// zcodeSignInURL is the page the user signs in on. ZCode hands back its own
// authorize URL with a redirect that goes to the desktop app; this realm's
// browser page is the same URL with the redirect pointed at ZCode's login
// screen instead, which is what a sign-in without the app completes.
func zcodeSignInURL(authorizeURL string, site zcodeSite) (string, error) {
	raw := strings.TrimSpace(authorizeURL)
	if raw == "" {
		return "", errors.New("ZCode 没有给出登录页面")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "https" {
		return "", errors.New("ZCode 没有给出有效的登录页面")
	}
	query := parsed.Query()
	// The parameter the redirect rides in is per realm.
	if site.id == "bigmodel" {
		query.Set("redirect", zcodeSiteHost+"/app/oauth/login?redirect=zcode%3A%2F%2Foauth%2Fcallback&app_version="+zcodeAppVersion)
	} else {
		query.Set("redirect_uri", zcodeSiteHost+"/app/oauth/login?redirect=zcode%3A%2F%2Foauth%2Fcallback&app_version="+zcodeAppVersion)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// pollZCodeOAuthInBackground waits for the browser sign-in, then does what
// ZCode does after it: take the business token, find the account's default
// project, get or make its key, and see which plan that key bills on. Only a
// key with a plan behind it is worth storing.
func (s *Server) pollZCodeOAuthInBackground(
	ctx context.Context,
	cancel context.CancelFunc,
	providerID, flowID string,
	site zcodeSite,
	pollSecret, device string,
	init zcodeInitAnswer,
	started time.Time,
) {
	defer cancel()
	fail := func(message string) {
		s.subscriptionOAuthFlows.setStatus(flowID, "failed", message)
		s.logs.AddApp("warn", "zcode oauth failed: "+message, providerID)
	}
	deadline := started.Add(zcodeSignInDeadline)
	if init.ExpiresAt > 0 {
		if until := time.Unix(init.ExpiresAt, 0); until.After(started) && until.Before(deadline) {
			deadline = until
		}
	}
	interval := time.Duration(init.PollIntervalS) * time.Second
	if interval < time.Second {
		interval = 2 * time.Second
	}
	endpoint := zcodeSiteHost + zcodePollPath + url.PathEscape(init.FlowID)
	var answered zcodePollAnswer
	for {
		if time.Now().After(deadline) {
			fail("登录超时，请重新发起")
			return
		}
		err := zcodeCall(ctx, http.MethodGet, endpoint, zcodeHeaders(endpoint, zcodeBearer(pollSecret), device), nil, &answered)
		if err != nil {
			var refused *zcodeError
			if errors.As(err, &refused) && refused.Status >= 400 && refused.Status < 500 && refused.Status != http.StatusTooManyRequests && refused.Status != http.StatusRequestTimeout {
				fail(err.Error())
				return
			}
		}
		switch strings.ToLower(strings.TrimSpace(answered.Status)) {
		case "ready":
			goto ready
		case "failed":
			fail(fmt.Sprintf("%s 拒绝了这次登录", site.label))
			return
		}
		select {
		case <-ctx.Done():
			fail("登录已取消")
			return
		case <-time.After(interval):
		}
		answered = zcodePollAnswer{}
	}
ready:
	credential, err := zcodeSignedIn(ctx, site, answered)
	if err != nil {
		fail(err.Error())
		return
	}
	provider, setErr := s.router.SetProviderZCode(providerID, credential)
	if setErr != nil {
		fail(setErr.Error())
		return
	}
	if err := s.saveState(); err != nil {
		fail("保存登录状态失败：" + err.Error())
		return
	}
	s.subscriptionOAuthFlows.setStatus(flowID, "done", credential.AccountLabel)
	s.subscriptionOAuthFlows.finish(providerID)
	if synced, syncErr := s.syncZCodeModels(provider); syncErr == nil {
		provider = synced
	} else {
		s.logs.AddApp("warn", "zcode model list not refreshed: "+syncErr.Error(), providerID)
	}
	_ = provider
	s.logs.AddApp("info", "zcode oauth signed in", providerID)
}

// zcodeSignedIn is the half that runs once the browser flow is ready: exchange
// the sign-in for a business token, find the project, mint the key, and check
// the plan. A personal plan, a team seat and no plan at all are the three
// answers; the third is an error that says where to subscribe.
func zcodeSignedIn(ctx context.Context, site zcodeSite, answered zcodePollAnswer) (domain.ZCodeCredential, error) {
	access := strings.TrimSpace(answered.ZAI.AccessToken)
	if site.id == "bigmodel" {
		access = firstNonEmptyString(answered.BigModel.AccessToken, answered.BigModel.CamelCase)
	}
	if access == "" {
		return domain.ZCodeCredential{}, fmt.Errorf("%s 没有返回可用的登录凭证", site.label)
	}
	biz, err := zcodeBusinessToken(ctx, site, access)
	if err != nil {
		return domain.ZCodeCredential{}, err
	}
	label := zcodeAccountLabel(answered.User.Email, answered.User.Name, answered.User.UserID)
	credential := domain.ZCodeCredential{
		Site:           site.id,
		Base:           site.inference,
		BizToken:       biz,
		AccountLabel:   label,
		SessionToken:   strings.TrimSpace(answered.Token),
		OrganizationID: "",
		Connected:      true,
	}
	org, project, err := zcodeDefaultProject(ctx, site, biz)
	if err != nil {
		return credential, err
	}
	key, err := zcodeMintKey(ctx, site, biz, org, project, zcodePersonalKeyName, 0)
	if err != nil {
		return credential, err
	}
	credential.APIKey = key
	plan, planErr := zcodePlanOf(ctx, site, key)
	if planErr == nil && plan != "" {
		credential.Plan = plan
		return credential, nil
	}
	// No plan of its own: look for a seat on a team's plan, which is what
	// ZCode does next. A team key bills on the team's plan and needs no plan
	// check of its own — the seat query already said the plan is live.
	seat, seatErr := zcodeFindTeamSeat(ctx, site, biz)
	if seatErr == nil {
		teamKey, keyErr := zcodeMintKey(ctx, site, biz, seat.organizationID, seat.projectID, zcodeTeamKeyName, 2)
		if keyErr != nil {
			return credential, keyErr
		}
		credential.APIKey = teamKey
		credential.OrganizationID = seat.organizationID
		credential.ProjectID = seat.projectID
		credential.Plan = firstNonEmptyString(seat.plan, "GLM Coding Team")
		return credential, nil
	}
	if planErr != nil {
		return credential, planErr
	}
	return credential, fmt.Errorf("%s 账号上没有 GLM Coding Plan（个人的或团队席位），也没有 ZCode 免费体验计划之外的可用套餐；可在 %s 订阅", site.label, site.subscribe)
}

// zcodeBusinessToken is the token the business API answers to. Z.ai trades the
// sign-in's token for one of its own; BigModel uses the sign-in's token as it
// came, bare.
func zcodeBusinessToken(ctx context.Context, site zcodeSite, access string) (string, error) {
	if site.id != "zai" {
		return access, nil
	}
	endpoint := site.siteAPI + zcodeLoginPath
	var login struct {
		AccessToken string `json:"access_token"`
	}
	if err := zcodeCall(ctx, http.MethodPost, endpoint, zcodeHeaders(endpoint, "", ""), map[string]any{"token": access}, &login); err != nil {
		return "", err
	}
	if strings.TrimSpace(login.AccessToken) == "" {
		return "", errors.New("Z.ai 没有返回业务凭证")
	}
	return login.AccessToken, nil
}

// zcodeAuthorization is how a business call is signed: a bearer on Z.ai, the
// bare token on BigModel.
func zcodeAuthorization(site zcodeSite, biz string) string {
	if site.bearerBiz {
		return zcodeBearer(biz)
	}
	return strings.TrimSpace(biz)
}

type zcodeOrg struct {
	OrganizationID   string `json:"organizationId"`
	OrganizationName string `json:"organizationName"`
	Projects         []struct {
		ProjectID   string `json:"projectId"`
		ProjectName string `json:"projectName"`
		ProjectType int    `json:"projectType"`
	} `json:"projects"`
}

// zcodeDefaultProject picks the organization and project ZCode itself would: a
// name saying 默认 wins, and the first usable one otherwise.
func zcodeDefaultProject(ctx context.Context, site zcodeSite, biz string) (organizationID, projectID string, err error) {
	endpoint := site.siteAPI + zcodeCustomerPath
	var info struct {
		Organizations []zcodeOrg `json:"organizations"`
	}
	if err := zcodeCall(ctx, http.MethodGet, endpoint, zcodeHeaders(endpoint, zcodeAuthorization(site, biz), ""), nil, &info); err != nil {
		return "", "", err
	}
	for _, org := range info.Organizations {
		if strings.TrimSpace(org.OrganizationID) == "" {
			continue
		}
		for _, project := range org.Projects {
			if strings.TrimSpace(project.ProjectID) == "" {
				continue
			}
			if strings.Contains(org.OrganizationName, "默认") && strings.Contains(project.ProjectName, "默认") {
				return org.OrganizationID, project.ProjectID, nil
			}
		}
	}
	for _, org := range info.Organizations {
		if strings.TrimSpace(org.OrganizationID) == "" || len(org.Projects) == 0 {
			continue
		}
		for _, project := range org.Projects {
			if strings.TrimSpace(project.ProjectID) != "" {
				return org.OrganizationID, project.ProjectID, nil
			}
		}
	}
	return "", "", errors.New(zcodeKeyNotFound + "：账号上找不到默认项目")
}

type zcodeTeamSeat struct {
	organizationID string
	projectID      string
	plan           string
}

// zcodeFindTeamSeat looks for a seat on a team's GLM Coding Plan, which is the
// second place ZCode finds a plan. A team project is projectType 2; its seat is
// usable while the subscription is effective and the grant is valid.
func zcodeFindTeamSeat(ctx context.Context, site zcodeSite, biz string) (zcodeTeamSeat, error) {
	endpoint := site.siteAPI + zcodeCustomerPath
	var info struct {
		Organizations []zcodeOrg `json:"organizations"`
	}
	if err := zcodeCall(ctx, http.MethodGet, endpoint, zcodeHeaders(endpoint, zcodeAuthorization(site, biz), ""), nil, &info); err != nil {
		return zcodeTeamSeat{}, err
	}
	for _, org := range info.Organizations {
		for _, project := range org.Projects {
			if project.ProjectType != 2 || strings.TrimSpace(project.ProjectID) == "" || strings.TrimSpace(org.OrganizationID) == "" {
				continue
			}
			detail, err := zcodeTeamDetail(ctx, site, biz, org.OrganizationID, project.ProjectID)
			if err != nil {
				continue
			}
			return zcodeTeamSeat{
				organizationID: org.OrganizationID,
				projectID:      project.ProjectID,
				plan:           detail.ProductName,
			}, nil
		}
	}
	return zcodeTeamSeat{}, errors.New("该账号没有团队 GLM Coding Plan 席位")
}

type zcodeTeamDetailAnswer struct {
	HasSubscription  *bool  `json:"hasSubscription"`
	Status           string `json:"status"`
	MemberGrant      string `json:"memberGrantStatus"`
	ProductName      string `json:"productName"`
	SubscribeEndTime string `json:"subscribeEndTime"`
}

func (a zcodeTeamDetailAnswer) usable() bool {
	granted := a.HasSubscription == nil || *a.HasSubscription
	return granted && strings.EqualFold(a.Status, "EFFECTIVE") && strings.EqualFold(a.MemberGrant, "VALID")
}

func zcodeTeamDetail(ctx context.Context, site zcodeSite, biz, organizationID, projectID string) (zcodeTeamDetailAnswer, error) {
	endpoint := site.siteAPI + zcodeTeamDetailPath
	headers := zcodeHeaders(endpoint, zcodeAuthorization(site, biz), "")
	for key, value := range zcodeTeamHeaders(organizationID, projectID, site) {
		headers[key] = value
	}
	var detail zcodeTeamDetailAnswer
	if err := zcodeCall(ctx, http.MethodGet, endpoint, headers, nil, &detail); err != nil {
		return detail, err
	}
	if !detail.usable() {
		return detail, errors.New("团队 GLM Coding Plan 席位不可用或已过期")
	}
	return detail, nil
}

// zcodeTeamHeaders name the team project a call is about. ZCode sends them on
// every team call; Set-Language follows the realm rather than the user.
func zcodeTeamHeaders(organizationID, projectID string, site zcodeSite) map[string]string {
	language, accept := "en", "en-US,en"
	if site.id == "bigmodel" {
		language = "zh"
	}
	return map[string]string{
		"Bigmodel-Organization": organizationID,
		"Bigmodel-Project":      projectID,
		"Set-Language":          language,
		"Accept-Language":       accept,
	}
}

// zcodeMintKey gets the project key named `name`, or makes it, then copies its
// secret out: the key a request carries is "<id>.<secret>", and only the copy
// endpoint gives the second half.
func zcodeMintKey(ctx context.Context, site zcodeSite, biz, organizationID, projectID, name string, keyType int) (string, error) {
	base := fmt.Sprintf(zcodeKeysPathFmt, url.PathEscape(organizationID), url.PathEscape(projectID))
	endpoint := site.siteAPI + base
	headers := zcodeHeaders(endpoint, zcodeAuthorization(site, biz), "")
	if keyType == 2 {
		for key, value := range zcodeTeamHeaders(organizationID, projectID, site) {
			headers[key] = value
		}
	}
	var listed []struct {
		Name    string      `json:"name"`
		APIKey  string      `json:"apiKey"`
		KeyType json.Number `json:"keyType"`
	}
	var key string
	if err := zcodeCall(ctx, http.MethodGet, endpoint, headers, nil, &listed); err == nil {
		for _, item := range listed {
			if item.Name != name || strings.TrimSpace(item.APIKey) == "" {
				continue
			}
			if keyType != 0 && zcodeNumber(item.KeyType) != keyType {
				continue
			}
			key = strings.TrimSpace(item.APIKey)
		}
	}
	if key == "" {
		body := map[string]any{"name": name}
		if keyType != 0 {
			body["keyType"] = keyType
		}
		var made struct {
			APIKey string `json:"apiKey"`
		}
		if err := zcodeCall(ctx, http.MethodPost, endpoint, headers, body, &made); err != nil {
			return "", err
		}
		key = strings.TrimSpace(made.APIKey)
	}
	if key == "" {
		return "", errors.New(zcodeKeyNotFound)
	}
	var copied struct {
		SecretKey string `json:"secretKey"`
	}
	copyEndpoint := endpoint + fmt.Sprintf(zcodeKeyCopyFmt, url.PathEscape(key))
	if err := zcodeCall(ctx, http.MethodGet, copyEndpoint, headers, nil, &copied); err != nil {
		if keyType == 2 {
			// A team key that answers no secret is still the key; ZCode uses it
			// bare rather than failing the sign-in.
			return key, nil
		}
		return "", err
	}
	if strings.TrimSpace(copied.SecretKey) == "" {
		if keyType == 2 {
			return key, nil
		}
		return "", errors.New(zcodeKeyNotFound)
	}
	return key + "." + strings.TrimSpace(copied.SecretKey), nil
}

func zcodeNumber(raw json.Number) int {
	n, err := raw.Int64()
	if err != nil {
		return 0
	}
	return int(n)
}

// zcodePlanOf asks the plan the key bills on. An empty answer means the key is
// real but nothing stands behind it — a pay-as-you-go account, which ZCode
// itself does not serve as a coding plan.
func zcodePlanOf(ctx context.Context, site zcodeSite, key string) (string, error) {
	endpoint := site.root + zcodeSubscriptionKey
	var listed []struct {
		ProductName string `json:"productName"`
		Status      string `json:"status"`
		Valid       bool   `json:"valid"`
	}
	if err := zcodeCall(ctx, http.MethodGet, endpoint, zcodeHeaders(endpoint, strings.TrimSpace(key), ""), nil, &listed); err != nil {
		return "", err
	}
	for _, item := range listed {
		if strings.EqualFold(strings.TrimSpace(item.Status), "VALID") {
			return strings.TrimSpace(item.ProductName), nil
		}
	}
	return "", nil
}

func zcodeAccountLabel(email, name, uid string) string {
	// A phone sign-in comes back as an address that only says so.
	if at := strings.ToLower(email); at != "" && !strings.HasSuffix(at, "@phone.local") {
		return email
	}
	return firstNonEmptyString(name, uid, "ZCode")
}

// ---- forwarding ------------------------------------------------------------

// applyZCodeHeaders signs an inference request with the plan's key. ZCode's
// endpoint takes the key twice — the Anthropic header and the bearer — and
// answers without both.
func applyZCodeHeaders(request *http.Request, provider domain.Provider) {
	if request == nil || !isZCodeProvider(provider) || provider.ZCode == nil {
		return
	}
	key := strings.TrimSpace(provider.ZCode.APIKey)
	if key == "" || provider.ZCode.Disconnected {
		return
	}
	request.Header.Set("x-api-key", key)
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("User-Agent", zcodeUserAgent)
}

// ensureZCodeCredential reports what is wrong when a ZCode provider cannot
// forward. A key never rotates, so there is nothing to renew; what can be wrong
// is that there is no key, or it was paused.
func ensureZCodeCredential(provider domain.Provider) (domain.Provider, error) {
	if !isZCodeProvider(provider) {
		return provider, nil
	}
	if provider.ZCode == nil || provider.ZCode.Disconnected {
		return provider, errors.New("ZCode 尚未登录，请先在控制台完成登录")
	}
	if strings.TrimSpace(provider.ZCode.APIKey) == "" {
		return provider, errors.New("ZCode 登录没有取到项目密钥，请重新登录")
	}
	return provider, nil
}

// ---- models ----------------------------------------------------------------

// zcodeModels is the list ZCode serves when its release config cannot be read.
// The ids are the names its client uses; a plan's own list replaces this when
// the config answers.
var zcodeModels = []struct {
	id      string
	context int
	output  int
}{
	{"GLM-5.3", 1000000, 128000},
	{"GLM-5.3-Flash", 1000000, 128000},
	{"GLM-5.2", 1000000, 128000},
	{"GLM-5-Turbo", 200000, 64000},
}

func zcodeFallbackModels(providerID string) []domain.Model {
	models := make([]domain.Model, 0, len(zcodeModels))
	for _, item := range zcodeModels {
		models = append(models, domain.Model{
			ID:              item.id,
			ProviderID:      providerID,
			Protocol:        domain.ProtocolClaude,
			ContextLength:   item.context,
			MaxOutputTokens: item.output,
			InMenu:          true,
		})
	}
	return models
}

// zcodeReleaseConfig is the config file ZCode ships by reference: which models
// a plan's provider has, and what each can do.
type zcodeReleaseConfig struct {
	Revision string `json:"revision"`
	Config   struct {
		ProviderConfigRules struct {
			ProviderRules []struct {
				ProviderID      string   `json:"providerId"`
				BuiltinModelIDs []string `json:"builtinModelIds"`
			} `json:"providerRules"`
		} `json:"providerConfigRules"`
		ModelConfigRules struct {
			BuiltinProviderModelRules []struct {
				ProviderID string `json:"providerId"`
				ModelID    string `json:"modelId"`
				Config     struct {
					Enabled *bool `json:"enabled"`
				} `json:"config"`
			} `json:"builtinProviderModelRules"`
			ModelRules []struct {
				ModelMatch string `json:"modelMatch"`
				Config     struct {
					Properties struct {
						ContextWindow int `json:"contextWindow"`
						InputFormat   struct {
							SupportsImage bool `json:"supportsImage"`
						} `json:"inputFormat"`
					} `json:"properties"`
					OptionSpecs struct {
						MaxOutputTokens struct {
							Max int `json:"max"`
						} `json:"maxOutputTokens"`
					} `json:"optionSpecs"`
				} `json:"config"`
			} `json:"modelRules"`
		} `json:"modelConfigRules"`
	} `json:"config"`
}

// fetchZCodeModels reads the model list off ZCode's release config: the release
// zcode.z.ai names for this platform, then the models its rules give this
// realm's coding plan. Anything that fails to read answers the fallback table,
// because a list the console can show is worth more than an exact one it
// cannot.
func fetchZCodeModels(ctx context.Context, site zcodeSite) ([]domain.Model, error) {
	configURL, err := zcodeReleaseConfigURL(ctx)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, configURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", zcodeUserAgent)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("读取 ZCode 模型配置失败（%s）", response.Status)
	}
	var config zcodeReleaseConfig
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&config); err != nil {
		return nil, err
	}
	return zcodeConfigModels(config, site.providerConfigID)
}

func zcodeReleaseConfigURL(ctx context.Context) (string, error) {
	endpoint := fmt.Sprintf("%s%s?app_version=%s&platform=%s", zcodeSiteHost, zcodeConfigsPath,
		url.QueryEscape(zcodeAppVersion), url.QueryEscape(zcodePlatform()))
	// ZCode asks its own site for the config anonymously, and the site wants a
	// device id even for that.
	var configs struct {
		BuiltinProviderConfigJSON string `json:"builtin_provider_config_json"`
	}
	if err := zcodeCall(ctx, http.MethodGet, endpoint, zcodeHeaders(endpoint, "", newUUIDV4()), nil, &configs); err != nil {
		return "", err
	}
	raw := strings.TrimSpace(configs.BuiltinProviderConfigJSON)
	if !strings.HasPrefix(strings.ToLower(raw), "https://") {
		return "", errors.New("ZCode 没有给出模型配置地址")
	}
	return raw, nil
}

// zcodeConfigModels applies the release config's rules to one plan's provider:
// the ids it lists, minus the ones it disables, each sized by the last rule
// that matches it. modelMatch is anchored and case-insensitive, as ZCode reads
// it.
func zcodeConfigModels(config zcodeReleaseConfig, providerID string) ([]domain.Model, error) {
	seen := map[string]bool{}
	ids := []string{}
	for _, rule := range config.Config.ProviderConfigRules.ProviderRules {
		if rule.ProviderID != providerID {
			continue
		}
		for _, id := range rule.BuiltinModelIDs {
			id = strings.TrimSpace(id)
			if id == "" || seen[strings.ToLower(id)] {
				continue
			}
			seen[strings.ToLower(id)] = true
			ids = append(ids, id)
		}
	}
	disabled := map[string]bool{}
	for _, rule := range config.Config.ModelConfigRules.BuiltinProviderModelRules {
		if rule.ProviderID != providerID {
			continue
		}
		if rule.Config.Enabled != nil && !*rule.Config.Enabled {
			disabled[strings.ToLower(strings.TrimSpace(rule.ModelID))] = true
		}
	}
	models := []domain.Model{}
	for _, id := range ids {
		if disabled[strings.ToLower(id)] {
			continue
		}
		model := domain.Model{ID: id, Protocol: domain.ProtocolClaude, InMenu: true}
		for _, rule := range config.Config.ModelConfigRules.ModelRules {
			if !zcodeModelMatches(rule.ModelMatch, id) {
				continue
			}
			// Later rules override earlier ones, as ZCode applies them.
			if rule.Config.Properties.ContextWindow > 0 {
				model.ContextLength = rule.Config.Properties.ContextWindow
			}
			if rule.Config.OptionSpecs.MaxOutputTokens.Max > 0 {
				model.MaxOutputTokens = rule.Config.OptionSpecs.MaxOutputTokens.Max
			}
		}
		if model.ContextLength == 0 {
			model.ContextLength = 200000
		}
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("ZCode 的模型配置（revision %s）里没有 %s 的模型", config.Revision, providerID)
	}
	return models, nil
}

// zcodeMatches caches the compiled modelMatch patterns. A model list is read on
// the console's every refresh, and ZCode's config carries dozens of rules.
var zcodeMatches sync.Map

func zcodeModelMatches(match, model string) bool {
	match = strings.TrimSpace(match)
	if match == "" {
		return false
	}
	if cached, ok := zcodeMatches.Load(match); ok {
		if pattern, ok := cached.(*regexp.Regexp); ok {
			return pattern.MatchString(model)
		}
		return false
	}
	pattern, err := regexp.Compile(`(?i)^(?:` + match + `)$`)
	if err != nil {
		// A rule this code cannot read is kept as one that never matches,
		// rather than retried on every model of every refresh.
		zcodeMatches.Store(match, (*regexp.Regexp)(nil))
		return false
	}
	zcodeMatches.Store(match, pattern)
	return pattern.MatchString(model)
}

// syncZCodeModels refreshes a provider's model list from its plan's config.
func (s *Server) syncZCodeModels(provider domain.Provider) (domain.Provider, error) {
	site, ok := zcodeSiteForProvider(provider)
	if !ok {
		return provider, errors.New("不是 ZCode 连接方式")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	models, err := fetchZCodeModels(ctx, site)
	if err != nil {
		models = zcodeFallbackModels(provider.ID)
	}
	for index := range models {
		models[index].ProviderID = provider.ID
	}
	return s.storeZCodeModels(provider, models)
}

func (s *Server) storeZCodeModels(provider domain.Provider, models []domain.Model) (domain.Provider, error) {
	defaultModel := strings.TrimSpace(provider.DefaultModel)
	if defaultModel == "" && len(models) > 0 {
		// A provider created from the catalog has no model picked yet; the
		// plan's own first model is what a request without one should get.
		defaultModel = models[0].ID
	}
	updated, err := s.router.UpdateProvider(provider.ID, domain.Provider{Models: models, DefaultModel: defaultModel})
	if err != nil {
		return provider, err
	}
	if err := s.saveState(); err != nil {
		return updated, err
	}
	return updated, nil
}

// ---- quota -----------------------------------------------------------------

type zcodeQuotaWindow struct {
	Name     string  `json:"name"`
	Used     float64 `json:"used"`
	Amount   float64 `json:"amount,omitempty"`
	Limit    float64 `json:"limit,omitempty"`
	Display  string  `json:"display,omitempty"`
	ResetsAt string  `json:"resetsAt,omitempty"`
	Aside    bool    `json:"aside,omitempty"`
}

type zcodeQuota struct {
	Plan      string             `json:"plan,omitempty"`
	Windows   []zcodeQuotaWindow `json:"windows"`
	Error     string             `json:"error,omitempty"`
	FetchedAt string             `json:"fetchedAt,omitempty"`
}

// zcodeQuotaFor reads the plan behind a key. The quota API is the one the plan
// itself serves — the same product surface a pasted bigmodel key is read on —
// so the existing coder-plan query does the work, and its five-hour and weekly
// tiers are turned into the windows the card shows.
func zcodeQuotaFor(ctx context.Context, site zcodeSite, credential domain.ZCodeCredential) zcodeQuota {
	key := strings.TrimSpace(credential.APIKey)
	quota := zcodeQuota{Windows: []zcodeQuotaWindow{}, Plan: strings.TrimSpace(credential.Plan)}
	if key == "" {
		quota.Error = "ZCode 登录没有取到项目密钥"
		return quota
	}
	var (
		report ZhipuUsageReport
		err    error
	)
	// The plan's own host answers for it, so the query goes to the realm the
	// key was minted in rather than to whichever Zhipu host the endpoint's
	// host name resembles.
	url := site.root + zhipuQuotaPath
	if credential.OrganizationID != "" && credential.ProjectID != "" {
		url += "?type=2"
		report, err = zhipuQuotaRequest(ctx, url, key, credential.OrganizationID, credential.ProjectID)
	} else {
		report, err = zhipuQuotaRequest(ctx, url, key, "", "")
	}
	if err != nil {
		quota.Error = err.Error()
		return quota
	}
	if report.Error != "" {
		quota.Error = report.Error
	}
	if report.Level != "" {
		quota.Plan = "GLM Coding " + zcodeUpperFirst(report.Level)
	}
	quota.Windows = append(quota.Windows, zcodeWindowOf("5 小时", report.FiveHour)...)
	quota.Windows = append(quota.Windows, zcodeWindowOf("每周", report.Weekly)...)
	quota.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	return quota
}

func zcodeWindowOf(name string, bucket *ZhipuUsageBucket) []zcodeQuotaWindow {
	if bucket == nil {
		return nil
	}
	return []zcodeQuotaWindow{{Name: name, Used: bucket.Utilization, ResetsAt: bucket.ResetsAt}}
}

func zcodeUpperFirst(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}
