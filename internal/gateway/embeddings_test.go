package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// embeddingsUpstream 起一个假上游，记录收到的路径、Authorization 与正文。
func embeddingsUpstream(t *testing.T) (*httptest.Server, *string, *string, *map[string]any) {
	t.Helper()
	gotPath := new(string)
	gotAuth := new(string)
	gotBody := &map[string]any{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath = r.URL.Path
		*gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"emb-1","usage":{"prompt_tokens":3,"total_tokens":3}}`))
	}))
	t.Cleanup(upstream.Close)
	return upstream, gotPath, gotAuth, gotBody
}

func embeddingsServer(t *testing.T, provider domain.Provider, key domain.APIKey) *Server {
	t.Helper()
	server := imagesTestServer(t, provider)
	key.RouteID = "img-route"
	if strings.TrimSpace(key.Name) == "" {
		key.Name = key.ID
	}
	if _, err := server.router.AddAPIKey(key); err != nil {
		t.Fatalf("add key: %v", err)
	}
	return server
}

func embeddingsRequest(t *testing.T, server *Server, key string, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	recorder := httptest.NewRecorder()
	server.handleOpenAIEmbeddings(recorder, request)
	return recorder
}

// 向量端点原样透传：路径补在 BaseURL 后面，正文里的模型按 Provider 的映射解析，
// Authorization 用 Provider 自己的 Key，响应原样回给客户端。
func TestEmbeddingsPassesThroughToProvider(t *testing.T) {
	upstream, gotPath, gotAuth, gotBody := embeddingsUpstream(t)
	provider := imagesImageProvider("emb-a", false)
	provider.BaseURL = upstream.URL + "/v1"
	provider.RequestAdapter = &domain.RequestAdapter{ModelMapping: map[string]string{"text-embedding-3-small": "emb-mapped"}}
	server := embeddingsServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})

	recorder := embeddingsRequest(t, server, "sk-gw-admin", `{"model":"text-embedding-3-small","input":"hello"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if *gotPath != "/v1/embeddings" {
		t.Fatalf("upstream path = %q, want /v1/embeddings", *gotPath)
	}
	if *gotAuth != "Bearer sk-emb-a" {
		t.Fatalf("upstream authorization = %q, want the provider's own key", *gotAuth)
	}
	if model, _ := (*gotBody)["model"].(string); model != "emb-mapped" {
		t.Fatalf("upstream model = %q, want the provider's mapping emb-mapped", model)
	}
	if !strings.Contains(recorder.Body.String(), `"embedding"`) {
		t.Fatalf("the response should be passed through unchanged, got %s", recorder.Body.String())
	}
}

// 客户端没带 Key 时必须 401，且请求不能发到上游。
func TestEmbeddingsRequiresAPIKey(t *testing.T) {
	upstream, gotPath, _, _ := embeddingsUpstream(t)
	provider := imagesImageProvider("emb-a", false)
	provider.BaseURL = upstream.URL + "/v1"
	server := embeddingsServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})

	recorder := embeddingsRequest(t, server, "", `{"model":"m","input":"x"}`)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (%s)", recorder.Code, recorder.Body.String())
	}
	if *gotPath != "" {
		t.Fatalf("an unauthenticated request reached the upstream at %q", *gotPath)
	}
}

// Provider 没有自己的凭证时必须报错，绝不能把客户端的网关 Key 转发给厂商。
func TestEmbeddingsNeverForwardsConsumerKey(t *testing.T) {
	upstream, _, gotAuth, _ := embeddingsUpstream(t)
	provider := imagesImageProvider("emb-a", false)
	provider.BaseURL = upstream.URL + "/v1"
	provider.APIKeySource = ""
	server := embeddingsServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})

	recorder := embeddingsRequest(t, server, "sk-gw-admin", `{"model":"m","input":"x"}`)
	if recorder.Code == http.StatusOK {
		t.Fatalf("a provider without credentials should not serve, got 200")
	}
	if strings.Contains(*gotAuth, "sk-gw-admin") {
		t.Fatalf("the gateway key was forwarded to the vendor: %q", *gotAuth)
	}
}

// 路由输出协议不是 OpenAI Chat 时，向量端点没有意义，必须在出发前拒绝。
func TestEmbeddingsRefusesNonOpenAIOutputRoute(t *testing.T) {
	upstream, gotPath, _, _ := embeddingsUpstream(t)
	provider := imagesImageProvider("emb-a", false)
	provider.BaseURL = upstream.URL + "/v1"
	server := embeddingsServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})
	route, err := server.router.RouteByID("img-route")
	if err != nil {
		t.Fatal(err)
	}
	route.OutputProtocol = domain.ProtocolClaude
	if _, err := server.router.UpdateRoute("img-route", route); err != nil {
		t.Fatalf("update route: %v", err)
	}

	recorder := embeddingsRequest(t, server, "sk-gw-admin", `{"model":"m","input":"x"}`)
	if recorder.Code == http.StatusOK {
		t.Fatalf("a Claude-output route must not serve embeddings, got 200")
	}
	if *gotPath != "" {
		t.Fatalf("the request reached the upstream at %q", *gotPath)
	}
}

func TestResolveProviderEmbeddingsURL(t *testing.T) {
	cases := []struct{ base, want string }{
		{"https://api.example.com/v1", "https://api.example.com/v1/embeddings"},
		{"https://api.example.com/v1/", "https://api.example.com/v1/embeddings"},
		{"https://api.example.com", "https://api.example.com/embeddings"},
		// 已经是完整端点：不能再拼一次。
		{"https://api.example.com/v1/embeddings", "https://api.example.com/v1/embeddings"},
		{"https://api.example.com/custom/EMBEDDINGS", "https://api.example.com/custom/EMBEDDINGS"},
	}
	for _, tc := range cases {
		provider := domain.Provider{BaseURL: tc.base}
		if got := resolveProviderEmbeddingsURL(provider); got != tc.want {
			t.Fatalf("base %q → %q, want %q", tc.base, got, tc.want)
		}
	}
}

// 上游拒绝（比如嵌入上游不认识的模型回空体 404）必须在应用日志里以 warn 级别
// 留着：很多厂商的错误体是空的，请求日志只有状态码，排障全靠这一条。
func TestEmbeddingsLogsUpstreamRejectionAsWarning(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(upstream.Close)
	provider := imagesImageProvider("emb-a", false)
	provider.BaseURL = upstream.URL + "/v1"
	server := embeddingsServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})

	recorder := embeddingsRequest(t, server, "sk-gw-admin", `{"model":"m","input":"x"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want the upstream's 404 (%s)", recorder.Code, recorder.Body.String())
	}
	found := ""
	for _, entry := range server.logs.ListApp(0) {
		if entry.Message == "handled embeddings request" {
			found = entry.Level + " " + entry.Context
			break
		}
	}
	if !strings.HasPrefix(found, "warn ") {
		t.Fatalf("upstream rejection should be logged as warn, got %q", found)
	}
	if !strings.Contains(found, "status=404") {
		t.Fatalf("the log entry should name the upstream status, got %q", found)
	}
}

// 2xx 的向量响应仍然只记 debug，免得每次调用都刷一条 warn。
func TestEmbeddingsLogsSuccessAsDebug(t *testing.T) {
	upstream, _, _, _ := embeddingsUpstream(t)
	provider := imagesImageProvider("emb-a", false)
	provider.BaseURL = upstream.URL + "/v1"
	server := embeddingsServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})

	if recorder := embeddingsRequest(t, server, "sk-gw-admin", `{"model":"m","input":"x"}`); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	for _, entry := range server.logs.ListApp(0) {
		if entry.Message == "handled embeddings request" && entry.Level != "debug" {
			t.Fatalf("a successful call should stay at debug, got %q", entry.Level)
		}
	}
}
