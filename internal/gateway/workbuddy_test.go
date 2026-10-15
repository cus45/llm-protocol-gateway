package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// pointWorkBuddyAt redirects both WorkBuddy builds at a test server until the
// returned restore function runs.
func pointWorkBuddyAt(t *testing.T, cn, ai string) {
	t.Helper()
	oldCN, oldAI := workBuddyCNEndpoint, workBuddyIntlEndpoint
	workBuddyCNEndpoint, workBuddyIntlEndpoint = cn, ai
	t.Cleanup(func() { workBuddyCNEndpoint, workBuddyIntlEndpoint = oldCN, oldAI })
}

func TestPrepareWorkBuddyChatBody(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantSame bool
		check    func(t *testing.T, payload map[string]any)
	}{
		{
			name: "非流式且无 system：强制流式并补上 system",
			body: `{"model":"glm-5.3","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, payload map[string]any) {
				if payload["stream"] != true {
					t.Fatalf("stream = %v, want true", payload["stream"])
				}
				messages, _ := payload["messages"].([]any)
				first, _ := messages[0].(map[string]any)
				if first["role"] != "system" {
					t.Fatalf("first message role = %v, want system", first["role"])
				}
				if len(messages) != 2 {
					t.Fatalf("messages = %d, want 2", len(messages))
				}
			},
		},
		{
			name: "已带 system 且流式：原样不动",
			body: `{"model":"glm-5.3","stream":true,"messages":[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, payload map[string]any) {
				messages, _ := payload["messages"].([]any)
				if len(messages) != 2 {
					t.Fatalf("messages = %d, want 2 (system not duplicated)", len(messages))
				}
			},
			wantSame: true,
		},
		{
			name: "带 messages 但 stream 缺省：补 stream",
			body: `{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]}`,
			check: func(t *testing.T, payload map[string]any) {
				if payload["stream"] != true {
					t.Fatalf("stream = %v, want true", payload["stream"])
				}
			},
		},
		{
			name:     "不是聊天请求（count_tokens 之类）：原样返回",
			body:     `{"model":"kimi-k3"}`,
			check:    func(*testing.T, map[string]any) {},
			wantSame: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, changed := prepareWorkBuddyChatBody([]byte(testCase.body))
			if changed == testCase.wantSame {
				t.Fatalf("changed = %v, want changed = %v", changed, !testCase.wantSame)
			}
			var payload map[string]any
			if err := json.Unmarshal(got, &payload); err != nil {
				t.Fatalf("result is not valid JSON: %v", err)
			}
			testCase.check(t, payload)
		})
	}
}

func TestBuildWorkBuddyCompletionRebuildsStream(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-1","created":1700000000,"model":"glm-5.3","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"你"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"content":"好"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"get_time","arguments":"{\"z"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"one\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
		`data: [DONE]`,
		"",
	}, "\n")

	completion, err := buildWorkBuddyCompletion([]byte(stream), "glm-5.3")
	if err != nil {
		t.Fatalf("build completion: %v", err)
	}
	if completion["object"] != "chat.completion" {
		t.Fatalf("object = %v, want chat.completion", completion["object"])
	}
	if completion["id"] != "chatcmpl-1" {
		t.Fatalf("id = %v, want chatcmpl-1", completion["id"])
	}
	choices, _ := completion["choices"].([]map[string]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(choices))
	}
	message, _ := choices[0]["message"].(map[string]any)
	if message["content"] != "你好" {
		t.Fatalf("content = %v, want 你好", message["content"])
	}
	if choices[0]["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v, want tool_calls", choices[0]["finish_reason"])
	}
	calls, _ := message["tool_calls"].([]map[string]any)
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	function, _ := calls[0]["function"].(map[string]any)
	if function["name"] != "get_time" {
		t.Fatalf("name = %v, want get_time", function["name"])
	}
	if function["arguments"] != `{"zone"}` {
		t.Fatalf("arguments = %v, want %q", function["arguments"], `{"zone"}`)
	}
	usage, _ := completion["usage"].(map[string]any)
	if usage["total_tokens"] != 18 {
		t.Fatalf("total_tokens = %v, want 18", usage["total_tokens"])
	}
}

func TestBuildWorkBuddyCompletionRejectsEmptyStream(t *testing.T) {
	if _, err := buildWorkBuddyCompletion([]byte("data: [DONE]\n\n"), "glm-5.3"); err == nil {
		t.Fatal("expected an error for a stream with no chunks")
	}
}

func TestWorkBuddyCallSurfacesBusinessErrorCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":11217,"msg":"token not ready","data":null}`)
	}))
	defer server.Close()

	err := workBuddyCall(t.Context(), http.MethodGet, server.URL+"/v2/plugin/auth/token", nil, nil, nil)
	code, ok := workBuddyErrorCode(err)
	if !ok || code != workBuddyRetryTokenCode {
		t.Fatalf("err = %v (code %d, ok %v), want retry code %d", err, code, ok, workBuddyRetryTokenCode)
	}
}

func TestWorkBuddyCallUnwrapsData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"msg":"ok","data":{"accessToken":"at","refreshToken":"rt","expiresIn":3600}}`)
	}))
	defer server.Close()

	var got workBuddyCredentialPayload
	if err := workBuddyCall(t.Context(), http.MethodGet, server.URL, nil, map[string]any{}, &got); err != nil {
		t.Fatalf("call: %v", err)
	}
	if got.AccessToken != "at" || got.RefreshToken != "rt" {
		t.Fatalf("payload = %+v", got)
	}
	credential := got.toCredential(domain.WorkBuddyCredential{})
	expiresAt, err := time.Parse(time.RFC3339, credential.ExpiresAt)
	if err != nil {
		t.Fatalf("expiresAt %q: %v", credential.ExpiresAt, err)
	}
	if delta := time.Until(expiresAt); delta < 55*time.Minute || delta > 61*time.Minute {
		t.Fatalf("expiry computed from expiresIn is %v out; want ~1h", delta)
	}
}

func TestApplyWorkBuddyHeaders(t *testing.T) {
	credential := domain.WorkBuddyCredential{
		AccessToken: "jwt-access", UserID: "uid-1", Domain: "copilot.tencent.com",
	}
	request, err := http.NewRequest(http.MethodPost, "https://example.test/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyWorkBuddyHeaders(request, domain.Provider{AuthType: domain.AuthTypeWorkBuddy, WorkBuddy: &credential})

	for header, want := range map[string]string{
		"Authorization": "Bearer jwt-access",
		"X-User-Id":     "uid-1",
		"X-Domain":      "copilot.tencent.com",
		"X-Product":     "SaaS",
		"X-IDE-Type":    "WorkBuddy",
		"User-Agent":    "WorkBuddy/" + workBuddyUAVersion,
	} {
		if got := request.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	// The China build signs like WorkBuddy's desktop app and nothing more.
	if request.Header.Get("X-Agent-Intent") != "" {
		t.Fatal("WorkBuddy (China) must not carry WorkBuddy AI's client headers")
	}
}

func TestApplyWorkBuddyHeadersAIAddsClientHeaders(t *testing.T) {
	credential := domain.WorkBuddyCredential{AccessToken: "jwt", UserID: "uid-2"}
	request, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyWorkBuddyHeaders(request, domain.Provider{AuthType: domain.AuthTypeWorkBuddyAI, WorkBuddy: &credential})

	if request.Header.Get("X-Agent-Intent") != "craft" {
		t.Fatalf("X-Agent-Intent = %q, want craft", request.Header.Get("X-Agent-Intent"))
	}
	// X-Domain falls back to the endpoint's authority when the account didn't say.
	if got := request.Header.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Fatalf("X-Domain = %q, want www.workbuddy.ai", got)
	}
	for _, header := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID", "X-Request-ID"} {
		if got := request.Header.Get(header); len(got) != 32 {
			t.Fatalf("%s = %q, want a 32-hex id", header, got)
		}
	}
}

func TestNormalizeProviderWorkBuddyPinsChatProtocol(t *testing.T) {
	for _, authType := range []string{domain.AuthTypeWorkBuddy, domain.AuthTypeWorkBuddyAI} {
		provider := domain.Provider{ID: "wb", Name: "WB", AuthType: authType, BaseURL: "https://example.com"}
		normalizeProvider(&provider)
		if provider.Protocol != domain.ProtocolOpenAIChat {
			t.Fatalf("%s: protocol = %s, want openai_chat", authType, provider.Protocol)
		}
		site, ok := workBuddySiteForAuthType(authType)
		if !ok {
			t.Fatalf("%s: no site", authType)
		}
		if provider.BaseURL != site.chatBaseURL() {
			t.Fatalf("%s: baseUrl = %q, want the placeholder replaced by %q", authType, provider.BaseURL, site.chatBaseURL())
		}
	}
	// A hand-set endpoint survives: the builds can move, and that should be a
	// config edit rather than a rebuild.
	provider := domain.Provider{ID: "wb", AuthType: domain.AuthTypeWorkBuddy, BaseURL: "https://mirror.example/v2"}
	normalizeProvider(&provider)
	if provider.BaseURL != "https://mirror.example/v2" {
		t.Fatalf("baseUrl = %q, want the operator's endpoint kept", provider.BaseURL)
	}
}

// workBuddySignInServer stands in for a WorkBuddy build: it hands out a sign-in
// page, refuses the token twice before the user "finishes" signing in, then
// answers with the token and the account.
func workBuddySignInServer(t *testing.T, tokenAttempts int) *httptest.Server {
	t.Helper()
	var attempts int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/plugin/auth/state", func(w http.ResponseWriter, r *http.Request) {
		for _, header := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
			if r.Header.Get(header) != "true" {
				t.Errorf("state request missing %s", header)
			}
		}
		if r.URL.Query().Get("platform") != "workbuddy" {
			t.Errorf("platform = %q, want workbuddy", r.URL.Query().Get("platform"))
		}
		fmt.Fprint(w, `{"code":0,"msg":"ok","data":{"state":"state-123","authUrl":"https://www.codebuddy.cn/login"}}`)
	})
	mux.HandleFunc("GET /v2/plugin/auth/token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "state-123" {
			t.Errorf("token state = %q", r.URL.Query().Get("state"))
		}
		attempts++
		if attempts <= tokenAttempts {
			fmt.Fprintf(w, `{"code":%d,"msg":"not yet"}`, workBuddyRetryTokenCode)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{"accessToken":"at-1","refreshToken":"rt-1","expiresIn":7200}}`)
	})
	mux.HandleFunc("GET /v2/plugin/login/account", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-1" {
			t.Errorf("account request authorization = %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"code":0,"data":{"uid":"uid-9","nickname":"郭庆"}}`)
	})
	mux.HandleFunc("GET /v3/config", func(w http.ResponseWriter, r *http.Request) {
		// The flow syncs the plan's models right after signing in.
		if r.Header.Get("Authorization") != "Bearer at-1" {
			t.Errorf("config request authorization = %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"code":0,"data":{"agents":[{"name":"cli","models":["glm-5.3"]}],"models":[{"id":"glm-5.3","maxInputTokens":1000000}]}}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected workbuddy endpoint %s %s", r.Method, r.URL.Path)
	})
	return httptest.NewServer(mux)
}

func TestWorkBuddySignInStoresAccountSession(t *testing.T) {
	const refusedAttempts = 2
	server := workBuddySignInServer(t, refusedAttempts)
	defer server.Close()
	pointWorkBuddyAt(t, server.URL, server.URL)

	router := NewRouter(domain.GatewayState{Providers: []domain.Provider{
		{ID: "wb", Name: "WorkBuddy", AuthType: domain.AuthTypeWorkBuddy},
	}})
	s := &Server{
		router:                router,
		logs:                  monitor.NewStore(),
		pendingWorkBuddyOAuth: newWorkBuddyOAuthPendingStore(),
	}

	site, _ := workBuddySiteForAuthType(domain.AuthTypeWorkBuddy)
	authURL, flowID, err := s.startWorkBuddyOAuth("wb", site)
	if err != nil {
		t.Fatalf("start sign-in: %v", err)
	}
	if !strings.HasPrefix(authURL, "https://www.codebuddy.cn/login") {
		t.Fatalf("authUrl = %q, want the page WorkBuddy handed out", authURL)
	}
	if !strings.Contains(authURL, "loginSessionId=") || !strings.Contains(authURL, "version="+workBuddyAppVersion) {
		t.Fatalf("authUrl %q lacks the session/version params", authURL)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		status, ok := s.pendingWorkBuddyOAuth.status(flowID)
		if ok && status.Status == "connected" {
			break
		}
		if ok && status.Status == "failed" {
			t.Fatalf("sign-in failed: %s", status.Message)
		}
		if time.Now().After(deadline) {
			t.Fatalf("sign-in did not finish; last status %+v", status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	provider, err := s.router.ProviderByID("wb")
	if err != nil {
		t.Fatal(err)
	}
	if provider.WorkBuddy == nil {
		t.Fatal("no workbuddy credential stored")
	}
	if provider.WorkBuddy.AccessToken != "at-1" || provider.WorkBuddy.RefreshToken != "rt-1" {
		t.Fatalf("credential = %+v", *provider.WorkBuddy)
	}
	if provider.WorkBuddy.UserID != "uid-9" || provider.WorkBuddy.AccountLabel != "郭庆" {
		t.Fatalf("account = %q / %q, want uid-9 / 郭庆", provider.WorkBuddy.UserID, provider.WorkBuddy.AccountLabel)
	}
	if provider.BaseURL != server.URL+"/v2" {
		t.Fatalf("baseUrl = %q, want the test server's /v2", provider.BaseURL)
	}
	if provider.Protocol != domain.ProtocolOpenAIChat {
		t.Fatalf("protocol = %s, want openai_chat", provider.Protocol)
	}
	// The console must never see the tokens themselves.
	redacted := redactProviderForClient(provider)
	if redacted.WorkBuddy == nil || redacted.WorkBuddy.AccessToken != "" || redacted.WorkBuddy.RefreshToken != "" {
		t.Fatalf("redacted credential leaked tokens: %+v", redacted.WorkBuddy)
	}
	if !redacted.WorkBuddy.Connected {
		t.Fatal("redacted credential should report connected")
	}
}

func TestEnsureFreshWorkBuddyTokenRefreshesExpiringSession(t *testing.T) {
	var refreshed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != workBuddyRefreshPath {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("X-Refresh-Token") != "rt-old" {
			t.Errorf("X-Refresh-Token = %q", r.Header.Get("X-Refresh-Token"))
		}
		refreshed = true
		fmt.Fprint(w, `{"code":0,"data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":7200}}`)
	}))
	defer server.Close()
	pointWorkBuddyAt(t, server.URL, server.URL)

	router := NewRouter(domain.GatewayState{Providers: []domain.Provider{{
		ID: "wb", Name: "WorkBuddy", AuthType: domain.AuthTypeWorkBuddy,
		WorkBuddy: &domain.WorkBuddyCredential{
			AccessToken:  "at-old",
			RefreshToken: "rt-old",
			UserID:       "uid-9",
			// Already lapsed: the next request must trade it in.
			ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		},
	}}})
	s := &Server{router: router, logs: monitor.NewStore()}

	provider, err := s.router.ProviderByID("wb")
	if err != nil {
		t.Fatal(err)
	}
	refreshedProvider, err := s.ensureFreshWorkBuddyToken(provider)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !refreshed {
		t.Fatal("refresh endpoint was not called")
	}
	if refreshedProvider.WorkBuddy.AccessToken != "at-new" || refreshedProvider.WorkBuddy.RefreshToken != "rt-new" {
		t.Fatalf("credential = %+v, want the refreshed tokens", *refreshedProvider.WorkBuddy)
	}
	if refreshedProvider.WorkBuddy.UserID != "uid-9" {
		t.Fatalf("userId = %q, want it preserved across a refresh", refreshedProvider.WorkBuddy.UserID)
	}
}

func TestEnsureFreshWorkBuddyTokenKeepsUsableSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a fresh session must not be refreshed")
	}))
	defer server.Close()
	pointWorkBuddyAt(t, server.URL, server.URL)

	router := NewRouter(domain.GatewayState{Providers: []domain.Provider{{
		ID: "wb", AuthType: domain.AuthTypeWorkBuddy,
		WorkBuddy: &domain.WorkBuddyCredential{
			AccessToken: "at", RefreshToken: "rt",
			ExpiresAt: time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		},
	}}})
	s := &Server{router: router, logs: monitor.NewStore()}
	provider, _ := s.router.ProviderByID("wb")
	got, err := s.ensureFreshWorkBuddyToken(provider)
	if err != nil {
		t.Fatalf("ensure fresh: %v", err)
	}
	if got.WorkBuddy.AccessToken != "at" {
		t.Fatalf("token = %q, want the stored one untouched", got.WorkBuddy.AccessToken)
	}
}

func TestEnsureFreshWorkBuddyTokenRefusesSignedOutProvider(t *testing.T) {
	s := &Server{router: NewRouter(domain.GatewayState{Providers: []domain.Provider{
		{ID: "wb", AuthType: domain.AuthTypeWorkBuddy},
	}})}
	provider, _ := s.router.ProviderByID("wb")
	if _, err := s.ensureFreshWorkBuddyToken(provider); err == nil {
		t.Fatal("expected a 'not signed in' error for a provider with no session")
	}
}

func TestFetchWorkBuddyModelsReadsCLIAgentConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != workBuddyConfigPath {
			t.Errorf("path = %s, want %s", r.URL.Path, workBuddyConfigPath)
		}
		if want := "CLI/" + workBuddyAppVersion + " WorkBuddy/" + workBuddyUAVersion; r.Header.Get("User-Agent") != want {
			t.Errorf("User-Agent = %q, want %q (a bare one gets CodeBuddy IDE's config)", r.Header.Get("User-Agent"), want)
		}
		fmt.Fprint(w, `{"code":0,"data":{
			"agents":[{"name":"ide","models":["ignored-model"]},{"name":"cli","models":["glm-5.3","auto"]}],
			"models":[{"id":"glm-5.3","maxInputTokens":1000000,"maxOutputTokens":32000}]}}`)
	}))
	defer server.Close()
	pointWorkBuddyAt(t, server.URL, server.URL)

	site, _ := workBuddySiteForAuthType(domain.AuthTypeWorkBuddy)
	models, err := fetchWorkBuddyModels(t.Context(), site, domain.WorkBuddyCredential{AccessToken: "at", UserID: "uid"})
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v, want the cli agent's two", models)
	}
	if models[0].ID != "glm-5.3" || models[0].ContextLength != 1_000_000 || models[0].MaxOutputTokens != 32_000 {
		t.Fatalf("first model = %+v", models[0])
	}
	if models[1].ID != "auto" || models[1].ContextLength == 0 {
		t.Fatalf("second model = %+v, want auto with a fallback context window", models[1])
	}
	for _, model := range models {
		if model.Protocol != domain.ProtocolOpenAIChat {
			t.Fatalf("%s protocol = %s", model.ID, model.Protocol)
		}
	}
}

func TestRewriteWorkBuddyUpstreamStreamedBody(t *testing.T) {
	newResponse := func(status int, contentType, body string) *http.Response {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}
	}

	t.Run("SSE 流重建成单个 chat.completion", func(t *testing.T) {
		response := newResponse(http.StatusOK, "text/event-stream",
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n"+
				"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		if err := rewriteWorkBuddyUpstreamStreamedBody(response, "glm-5.3"); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		payload, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		var completion map[string]any
		if err := json.Unmarshal(payload, &completion); err != nil {
			t.Fatalf("rebuilt body is not JSON: %v (%s)", err, payload)
		}
		if completion["object"] != "chat.completion" {
			t.Fatalf("object = %v, want chat.completion", completion["object"])
		}
		if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Fatalf("content type = %q, want application/json", got)
		}
		if response.ContentLength != int64(len(payload)) {
			t.Fatalf("content length = %d, body is %d", response.ContentLength, len(payload))
		}
	})

	t.Run("上游错误原样保留，不编造完成对象", func(t *testing.T) {
		response := newResponse(http.StatusForbidden, "application/json", `{"code":11140,"displayMsg":"账号存在异常行为"}`)
		if err := rewriteWorkBuddyUpstreamStreamedBody(response, "glm-5.3"); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		payload, _ := io.ReadAll(response.Body)
		if !strings.Contains(string(payload), "11140") {
			t.Fatalf("error body = %s, want WorkBuddy's answer kept verbatim", payload)
		}
	})

	t.Run("上游已经给了 JSON 就不动", func(t *testing.T) {
		response := newResponse(http.StatusOK, "application/json", `{"object":"chat.completion"}`)
		if err := rewriteWorkBuddyUpstreamStreamedBody(response, "glm-5.3"); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		payload, _ := io.ReadAll(response.Body)
		if string(payload) != `{"object":"chat.completion"}` {
			t.Fatalf("body = %s, want it untouched", payload)
		}
	})
}

func TestWorkBuddyQuotaReadsCreditPackages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/billing/meter/get-user-resource-summary":
			fmt.Fprint(w, `{"code":0,"data":{"IsPaidUser":true,"Packages":[
				{"CycleTotalCapacity":"3300","CycleUsedCapacity":"438.88"}]}}`)
		default:
			t.Errorf("unexpected quota endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()
	pointWorkBuddyAt(t, server.URL, server.URL)

	site, _ := workBuddySiteForAuthType(domain.AuthTypeWorkBuddy)
	quota := workBuddyQuotaFor(t.Context(), site, domain.WorkBuddyCredential{AccessToken: "at", UserID: "uid", AccountLabel: "郭庆"})
	if quota.Error != "" {
		t.Fatalf("quota error: %s", quota.Error)
	}
	if quota.Plan != "Pro" {
		t.Fatalf("plan = %q, want Pro", quota.Plan)
	}
	if len(quota.Windows) != 1 {
		t.Fatalf("windows = %+v", quota.Windows)
	}
	window := quota.Windows[0]
	if window.Limit != 3300 || window.Amount != 438.88 {
		t.Fatalf("window = %+v, want the string-encoded capacities parsed", window)
	}
	if window.Used < 13 || window.Used > 14 {
		t.Fatalf("used = %.2f%%, want ~13.3%%", window.Used)
	}
}
