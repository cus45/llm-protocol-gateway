package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// zcodeStub answers ZCode's control-plane endpoints the way its own sites do,
// including the {code,msg,data} envelope, and records what was asked so a test
// can check the headers as well as the result.
type zcodeStub struct {
	t          *testing.T
	server     *httptest.Server
	polls      int
	requests   []zcodeStubRequest
	plan       string // what subscription/list answers; "" means no plan
	teamUsable bool
	secretKey  string
	mu         sync.Mutex
}

type zcodeStubRequest struct {
	method string
	path   string
	auth   string
	body   map[string]any
}

func newZCodeStub(t *testing.T) *zcodeStub {
	stub := &zcodeStub{t: t, plan: "GLM Coding Pro", teamUsable: true, secretKey: "secret-abc"}
	stub.server = httptest.NewServer(http.HandlerFunc(stub.handle))
	t.Cleanup(stub.server.Close)
	return stub
}

func (stub *zcodeStub) handle(w http.ResponseWriter, r *http.Request) {
	stub.mu.Lock()
	body := map[string]any{}
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
	}
	stub.requests = append(stub.requests, zcodeStubRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
	stub.mu.Unlock()
	reply := func(data any) {
		writeJSON(w, http.StatusOK, map[string]any{"code": 0, "msg": "success", "data": data})
	}
	switch {
	case r.URL.Path == "/api/v1/oauth/cli/poll/flow-1":
		stub.mu.Lock()
		stub.polls++
		ready := stub.polls >= 2
		stub.mu.Unlock()
		if !ready {
			reply(map[string]any{"status": "pending"})
			return
		}
		reply(map[string]any{
			"status":   "ready",
			"token":    "zcode-session-jwt",
			"zai":      map[string]any{"access_token": "zai-access-token"},
			"bigmodel": map[string]any{"access_token": "bigmodel-access-token"},
			"user":     map[string]any{"user_id": "u-1", "email": "dev@example.com", "name": "Dev"},
		})
	case r.URL.Path == "/api/auth/z/login":
		reply(map[string]any{"access_token": "business-token"})
	case r.URL.Path == "/api/biz/customer/getCustomerInfo":
		reply(map[string]any{"organizations": []any{
			map[string]any{
				"organizationId":   "org-2",
				"organizationName": "另一个机构",
				"projects": []any{
					map[string]any{"projectId": "p-9", "projectName": "别的项目", "projectType": 1},
				},
			},
			map[string]any{
				"organizationId":   "org-1",
				"organizationName": "默认机构",
				"projects": []any{
					map[string]any{"projectId": "p-7", "projectName": "个人项目", "projectType": 1},
					map[string]any{"projectId": "p-8", "projectName": "默认项目", "projectType": 1},
					map[string]any{"projectId": "team-1", "projectName": "团队项目", "projectType": 2},
				},
			},
		}})
	case strings.HasPrefix(r.URL.Path, "/api/biz/v1/organization/"):
		if strings.HasSuffix(r.URL.Path, "/api_keys") && r.Method == http.MethodGet {
			// The account has no key yet, which is what makes ZCode create one.
			reply([]any{})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/api_keys") && r.Method == http.MethodPost {
			name, _ := body["name"].(string)
			reply(map[string]any{"name": name, "apiKey": "keyid-123", "keyType": body["keyType"]})
			return
		}
		if i := strings.Index(r.URL.Path, "/copy/"); i >= 0 {
			reply(map[string]any{"secretKey": stub.secretKey})
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	case r.URL.Path == "/api/biz/subscription/list":
		if stub.plan == "" {
			reply([]any{})
			return
		}
		reply([]any{map[string]any{"productName": stub.plan, "status": "VALID", "valid": true}})
	case r.URL.Path == "/api/biz/team/subscribe/product/querySubscribeDetail":
		if !stub.teamUsable {
			reply(map[string]any{"hasSubscription": false, "status": "NONE"})
			return
		}
		reply(map[string]any{
			"hasSubscription": true, "status": "EFFECTIVE", "memberGrantStatus": "VALID",
			"productName": "GLM Coding Team",
		})
	case r.URL.Path == "/api/monitor/usage/quota/limit":
		writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
			"level": "pro",
			"limits": []any{
				map[string]any{"type": "TOKENS_LIMIT", "percentage": 42, "usage": 1000, "remaining": 580, "nextResetTime": 1792000000000, "unit": 3, "number": 5},
				map[string]any{"type": "TOKENS_LIMIT", "percentage": 8, "usage": 10000, "remaining": 9200, "nextResetTime": 1792600000000, "unit": 6, "number": 1},
			},
		}})
	default:
		http.Error(w, "unhandled "+r.URL.Path, http.StatusNotFound)
	}
}

// site returns the stub as a ZCode realm, with zcode.z.ai itself also pointed
// at the same server.
func (stub *zcodeStub) site(id string) zcodeSite {
	site := zcodeSites[id]
	site.siteAPI = stub.server.URL
	site.root = stub.server.URL
	site.inference = stub.server.URL + "/api/anthropic"
	return site
}

func (stub *zcodeStub) asked(method, path string) *zcodeStubRequest {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for index := range stub.requests {
		if stub.requests[index].method == method && stub.requests[index].path == path {
			return &stub.requests[index]
		}
	}
	return nil
}

func TestZCodeSignedInMintsThePlanKey(t *testing.T) {
	stub := newZCodeStub(t)
	defer withZCodeSiteHost(t, stub.server.URL)()
	site := stub.site("zai")

	credential, err := zcodeSignedIn(context.Background(), site, zcodePollAnswer{
		Token: "zcode-session-jwt",
		ZAI:   zcodeAccessToken{AccessToken: "zai-access-token"},
		User:  zcodeUser{UserID: "u-1", Email: "dev@example.com", Name: "Dev"},
	})
	if err != nil {
		t.Fatalf("sign-in failed: %v", err)
	}
	if credential.APIKey != "keyid-123.secret-abc" {
		t.Errorf("key = %q, want the id and the copied secret joined", credential.APIKey)
	}
	if credential.Plan != "GLM Coding Pro" {
		t.Errorf("plan = %q", credential.Plan)
	}
	if credential.AccountLabel != "dev@example.com" {
		t.Errorf("account label = %q", credential.AccountLabel)
	}
	if credential.Site != "zai" || credential.Base != site.inference {
		t.Errorf("credential names the wrong realm: %+v", credential)
	}

	// Z.ai trades the sign-in's token for a business token and sends it as a
	// bearer; the plan's own endpoints take the bare key.
	if login := stub.asked(http.MethodPost, "/api/auth/z/login"); login == nil {
		t.Fatal("the sign-in's token was never exchanged for a business token")
	}
	keys := stub.asked(http.MethodGet, "/api/biz/v1/organization/org-1/projects/p-8/api_keys")
	if keys == nil {
		t.Fatal("the default project's keys were not read")
	}
	if keys.auth != "Bearer business-token" {
		t.Errorf("keys asked with Authorization %q, want the bearer business token", keys.auth)
	}
	if created := stub.asked(http.MethodPost, "/api/biz/v1/organization/org-1/projects/p-8/api_keys"); created == nil || created.body["name"] != zcodePersonalKeyName {
		t.Errorf("no %s was created; asked %+v", zcodePersonalKeyName, created)
	}
	plan := stub.asked(http.MethodGet, "/api/biz/subscription/list")
	if plan == nil || plan.auth != "keyid-123.secret-abc" {
		t.Errorf("the plan check did not carry the bare key: %+v", plan)
	}
}

// A phone sign-in comes back as an address that only says it is a phone, which
// is not a name worth showing on a card.
func TestZCodeAccountLabel(t *testing.T) {
	if got := zcodeAccountLabel("13800000000@phone.local", "", "u-9"); got != "u-9" {
		t.Errorf("label = %q, want the user id", got)
	}
	if got := zcodeAccountLabel("", "郭庆", "u-9"); got != "郭庆" {
		t.Errorf("label = %q, want the name", got)
	}
	if got := zcodeAccountLabel("a@b.com", "ignored", "u-9"); got != "a@b.com" {
		t.Errorf("label = %q, want the email", got)
	}
}

// BigModel signs its business calls with the sign-in token itself, bare, and
// never asks /api/auth/z/login — the one detail that used to make a BigModel
// sign-in answer 401.
func TestZCodeBigModelUsesBareToken(t *testing.T) {
	stub := newZCodeStub(t)
	defer withZCodeSiteHost(t, stub.server.URL)()
	site := stub.site("bigmodel")

	credential, err := zcodeSignedIn(context.Background(), site, zcodePollAnswer{
		BigModel: zcodeAccessToken{AccessToken: "bigmodel-access-token"},
		User:     zcodeUser{UserID: "u-2", Name: "郭"},
	})
	if err != nil {
		t.Fatalf("sign-in failed: %v", err)
	}
	if credential.BizToken != "bigmodel-access-token" {
		t.Errorf("business token = %q, want the sign-in token itself", credential.BizToken)
	}
	if asked := stub.asked(http.MethodPost, "/api/auth/z/login"); asked != nil {
		t.Error("BigModel was asked for a Z.ai business login")
	}
	keys := stub.asked(http.MethodGet, "/api/biz/v1/organization/org-1/projects/p-8/api_keys")
	if keys == nil || keys.auth != "bigmodel-access-token" {
		t.Errorf("BigModel keys asked with %q, want the bare token", keys.auth)
	}
}

// An account with no plan of its own is served by a team seat, whose key is a
// different name under a different project and needs the project headers.
func TestZCodeFallsBackToTeamSeat(t *testing.T) {
	stub := newZCodeStub(t)
	stub.plan = ""
	defer withZCodeSiteHost(t, stub.server.URL)()

	credential, err := zcodeSignedIn(context.Background(), stub.site("zai"), zcodePollAnswer{
		ZAI: zcodeAccessToken{AccessToken: "at"},
	})
	if err != nil {
		t.Fatalf("team seat not found: %v", err)
	}
	if credential.OrganizationID != "org-1" || credential.ProjectID != "team-1" {
		t.Errorf("seat = %s/%s, want the team project", credential.OrganizationID, credential.ProjectID)
	}
	if credential.Plan != "GLM Coding Team" {
		t.Errorf("plan = %q", credential.Plan)
	}
	created := stub.asked(http.MethodPost, "/api/biz/v1/organization/org-1/projects/team-1/api_keys")
	if created == nil || created.body["name"] != zcodeTeamKeyName {
		t.Fatalf("no %s was created: %+v", zcodeTeamKeyName, created)
	}
	if n, _ := created.body["keyType"].(float64); n != 2 {
		t.Errorf("team key created with keyType %v, want 2", created.body["keyType"])
	}
	if created.auth != "Bearer business-token" {
		t.Errorf("team key asked with %q", created.auth)
	}
}

// With neither a plan nor a seat there is nothing to mint, and the message has
// to say where to get one rather than looking like a broken sign-in.
func TestZCodeWithoutAnyPlanSaysSo(t *testing.T) {
	stub := newZCodeStub(t)
	stub.plan = ""
	stub.teamUsable = false
	defer withZCodeSiteHost(t, stub.server.URL)()

	_, err := zcodeSignedIn(context.Background(), stub.site("zai"), zcodePollAnswer{
		ZAI: zcodeAccessToken{AccessToken: "at"},
	})
	if err == nil {
		t.Fatal("a plan-less account signed in, which has nothing to bill on")
	}
	if !strings.Contains(err.Error(), "GLM Coding Plan") || !strings.Contains(err.Error(), zcodeSites["zai"].subscribe) {
		t.Errorf("error = %q, want it to name the plan and where to subscribe", err)
	}
}

func withZCodeSiteHost(t *testing.T, host string) func() {
	old := zcodeSiteHost
	zcodeSiteHost = host
	return func() { zcodeSiteHost = old }
}

func TestZCodeSignInURLRedirectsToTheBrowserPage(t *testing.T) {
	// ZCode hands back the URL its desktop app would open; the redirect has to
	// point at the web login screen instead, under the parameter the realm reads.
	zai, err := zcodeSignInURL("https://chat.z.ai/oauth?client_id=1&redirect_uri=zcode%3A%2F%2Fcallback", zcodeSites["zai"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(zai, "redirect_uri=https%3A%2F%2Fzcode.z.ai%2Fapp%2Foauth%2Flogin") {
		t.Errorf("Z.ai URL kept its app redirect: %s", zai)
	}
	if strings.Contains(zai, "redirect=") {
		t.Errorf("Z.ai URL was given BigModel's parameter: %s", zai)
	}
	bigmodel, err := zcodeSignInURL("https://open.bigmodel.cn/oauth?redirect=zcode%3A%2F%2Fcallback", zcodeSites["bigmodel"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bigmodel, "redirect=https%3A%2F%2Fzcode.z.ai%2Fapp%2Foauth%2Flogin") {
		t.Errorf("BigModel URL kept its app redirect: %s", bigmodel)
	}
	for _, bad := range []string{"", "http://chat.z.ai/oauth", "not a url"} {
		if _, err := zcodeSignInURL(bad, zcodeSites["zai"]); err == nil {
			t.Errorf("accepted the sign-in page %q", bad)
		}
	}
}

// The device id is only ever sent to zcode.z.ai: its business APIs answer
// badly to it, and its own plan endpoints answer 3001 without it.
func TestZCodeHeadersScopeTheDeviceID(t *testing.T) {
	site := zcodeHeaders("https://zcode.z.ai/api/v1/zcode-plan/billing/balance", "Bearer jwt", "device-1")
	if site["X-Device-Mid"] != "device-1" {
		t.Errorf("zcode.z.ai call carried no device id: %v", site)
	}
	business := zcodeHeaders("https://api.z.ai/api/biz/subscription/list", "key", "device-1")
	if _, ok := business["X-Device-Mid"]; ok {
		t.Errorf("a business call was sent the device id: %v", business)
	}
	if business["Authorization"] != "key" {
		t.Errorf("Authorization = %q, want it sent as it was", business["Authorization"])
	}
}

func TestZCodeCallReadsTheEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			fmt.Fprint(w, `{"code":0,"msg":"success","data":{"access_token":"t"}}`)
		case "/no-plan":
			fmt.Fprint(w, `{"code":1113,"msg":"余额不足或无可用资源包","data":null}`)
		case "/broken":
			fmt.Fprint(w, `<html>blocked</html>`)
		case "/server":
			http.Error(w, "upstream down", http.StatusBadGateway)
		}
	}))
	defer server.Close()
	type answer struct {
		AccessToken string `json:"access_token"`
	}
	var ok answer
	if err := zcodeCall(context.Background(), http.MethodGet, server.URL+"/ok", nil, nil, &ok); err != nil {
		t.Fatalf("code 0 was an error: %v", err)
	}
	if ok.AccessToken != "t" {
		t.Errorf("data = %+v, want it unwrapped", ok)
	}
	err := zcodeCall(context.Background(), http.MethodGet, server.URL+"/no-plan", nil, nil, &ok)
	if code, isRefusal := zcodeErrorCode(err); !isRefusal || code != 1113 {
		t.Errorf("code 1113 was not kept: %v (%v)", err, code)
	}
	if !strings.Contains(err.Error(), "余额不足") {
		t.Errorf("error lost the message: %v", err)
	}
	if err := zcodeCall(context.Background(), http.MethodGet, server.URL+"/server", nil, nil, &ok); err == nil {
		t.Error("a 502 answered no error")
	}
	if err := zcodeCall(context.Background(), http.MethodGet, server.URL+"/broken", nil, nil, &ok); err == nil {
		t.Error("a non-JSON answer parsed successfully")
	}
}

func TestApplyZCodeHeadersCarriesTheKeyTwice(t *testing.T) {
	provider := domain.Provider{AuthType: domain.AuthTypeZCode, ZCode: &domain.ZCodeCredential{APIKey: "id.secret"}}
	request, _ := http.NewRequest(http.MethodPost, "https://api.z.ai/api/anthropic/v1/messages", nil)
	applyZCodeHeaders(request, provider)
	if request.Header.Get("x-api-key") != "id.secret" || request.Header.Get("Authorization") != "Bearer id.secret" {
		t.Errorf("x-api-key=%q Authorization=%q, want the plan key in both", request.Header.Get("x-api-key"), request.Header.Get("Authorization"))
	}
	paused := domain.Provider{AuthType: domain.AuthTypeZCode, ZCode: &domain.ZCodeCredential{APIKey: "id.secret", Disconnected: true}}
	pausedRequest, _ := http.NewRequest(http.MethodPost, "https://api.z.ai/api/anthropic/v1/messages", nil)
	applyZCodeHeaders(pausedRequest, paused)
	if pausedRequest.Header.Get("x-api-key") != "" {
		t.Error("a paused account still signed its request")
	}
	if got := resolveProviderAuth(provider); got != "id.secret" {
		t.Errorf("resolveProviderAuth = %q, want the minted key", got)
	}
	if got := resolveProviderAuth(paused); got != "" {
		t.Errorf("a paused account resolved to %q, want nothing", got)
	}
}

// A ZCode provider forwards as the plan's Anthropic endpoint, which is what its
// key bills on; the console never types it.
func TestNormalizeZCodeProvider(t *testing.T) {
	for authType, want := range map[string]string{
		domain.AuthTypeZCode:         "https://api.z.ai/api/anthropic",
		domain.AuthTypeZCodeBigModel: "https://open.bigmodel.cn/api/anthropic",
	} {
		provider := normalizeProviderForTest(domain.Provider{Name: "ZCode", AuthType: authType, BaseURL: "https://example.com"})
		if provider.Protocol != domain.ProtocolClaude {
			t.Errorf("%s protocol = %s, want Claude", authType, provider.Protocol)
		}
		if provider.BaseURL != want {
			t.Errorf("%s base = %q, want %q", authType, provider.BaseURL, want)
		}
	}
	// A hand-set endpoint survives: a realm moving is a config edit.
	kept := normalizeProviderForTest(domain.Provider{Name: "ZCode", AuthType: domain.AuthTypeZCode, BaseURL: "https://api.z.ai/api/coding/anthropic"})
	if kept.BaseURL != "https://api.z.ai/api/coding/anthropic" {
		t.Errorf("a typed endpoint was replaced with %q", kept.BaseURL)
	}
}

func TestZCodeConfigModelsAppliesItsRules(t *testing.T) {
	var config zcodeReleaseConfig
	raw := `{
	  "revision": "42",
	  "config": {
	    "providerConfigRules": {"providerRules": [
	      {"providerId": "account:zai-individual-coding-plan", "builtinModelIds": ["GLM-5.3", "GLM-5.2", "GLM-5-Turbo", "GLM-5.3"]},
	      {"providerId": "other", "builtinModelIds": ["NOT-MINE"]}
	    ]},
	    "modelConfigRules": {
	      "builtinProviderModelRules": [
	        {"providerId": "account:zai-individual-coding-plan", "modelId": "GLM-5-Turbo", "config": {"enabled": false}}
	      ],
	      "modelRules": [
	        {"modelMatch": "GLM-5\\..*", "config": {"properties": {"contextWindow": 200000}, "optionSpecs": {"maxOutputTokens": {"max": 32000}}}},
	        {"modelMatch": "glm-5\\.3", "config": {"properties": {"contextWindow": 1000000, "inputFormat": {"supportsImage": true}}}}
	      ]
	    }
	  }
	}`
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	models, err := zcodeConfigModels(config, "account:zai-individual-coding-plan")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]domain.Model{}
	for _, model := range models {
		got[model.ID] = model
	}
	if len(models) != 2 {
		t.Fatalf("got %v, want the two live models with the disabled one and the duplicate left out", len(models))
	}
	if _, ok := got["GLM-5-Turbo"]; ok {
		t.Error("a model its plan disabled was still listed")
	}
	if _, ok := got["NOT-MINE"]; ok {
		t.Error("another plan's model was listed")
	}
	// The later rule wins where two match, as ZCode applies them.
	if got["GLM-5.3"].ContextLength != 1000000 {
		t.Errorf("GLM-5.3 context = %d, want the last matching rule's", got["GLM-5.3"].ContextLength)
	}
	if got["GLM-5.2"].ContextLength != 200000 || got["GLM-5.2"].MaxOutputTokens != 32000 {
		t.Errorf("GLM-5.2 = %+v, want the general rule's window and output", got["GLM-5.2"])
	}
	if _, err := zcodeConfigModels(config, "account:bigmodel-individual-coding-plan"); err == nil {
		t.Error("a plan with no models in the config read as a list")
	}
}

func TestZCodeQuotaWindows(t *testing.T) {
	stub := newZCodeStub(t)
	site := stub.site("zai")
	quota := zcodeQuotaFor(context.Background(), site, domain.ZCodeCredential{APIKey: "id.secret", Plan: "GLM Coding Pro"})
	if quota.Error != "" {
		t.Fatalf("quota read failed: %s", quota.Error)
	}
	if len(quota.Windows) != 2 {
		t.Fatalf("got %d windows, want the five-hour and the weekly", len(quota.Windows))
	}
	if quota.Windows[0].Used != 42 {
		t.Errorf("five-hour used = %v, want the percentage the plan reports", quota.Windows[0].Used)
	}
	if quota.Plan != "GLM Coding Pro" {
		t.Errorf("plan = %q", quota.Plan)
	}
}
