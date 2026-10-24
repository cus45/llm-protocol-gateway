package gateway

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// audioUpstream records what the upstream got: path, auth, content type, the
// raw body, and the form's fields.
type audioUpstream struct {
	server      *httptest.Server
	path        string
	auth        string
	contentType string
	raw         []byte
	fields      map[string]string
	fileLen     int
	fileName    string
}

func newAudioUpstream(t *testing.T, status int, response string) *audioUpstream {
	t.Helper()
	up := &audioUpstream{}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.path = r.URL.Path
		up.auth = r.Header.Get("Authorization")
		up.contentType = r.Header.Get("Content-Type")
		up.raw, _ = io.ReadAll(r.Body)
		up.fields = map[string]string{}
		_, params, err := mime.ParseMediaType(up.contentType)
		if err != nil {
			t.Errorf("upstream got an unparsable content type %q", up.contentType)
			return
		}
		reader := multipart.NewReader(bytes.NewReader(up.raw), params["boundary"])
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("upstream could not parse the form: %v", err)
				return
			}
			if part.FileName() != "" {
				data, _ := io.ReadAll(part)
				up.fileLen = len(data)
				up.fileName = part.FileName()
				continue
			}
			data, _ := io.ReadAll(part)
			up.fields[part.FormName()] = string(data)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(up.server.Close)
	return up
}

func audioFormBody(t *testing.T, fields map[string]string, audio []byte) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for name, value := range fields {
		_ = writer.WriteField(name, value)
	}
	part, err := writer.CreateFormFile("file", "clip.mp3")
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := part.Write(audio); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}
	return writer.FormDataContentType(), buf.Bytes()
}

func audioRequest(t *testing.T, server *Server, key, path, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	recorder := httptest.NewRecorder()
	handler := server.handleOpenAIAudioTranscriptions
	if strings.HasSuffix(path, "/translations") {
		handler = server.handleOpenAIAudioTranslations
	}
	handler(recorder, request)
	return recorder
}

// 音频上传按原样转发：multipart 的边界、音频字节、额外字段都保持客户端发来
// 的样子，只有模型名按 Provider 的映射改写；Provider 用自己那把 Key。
func TestAudioTranscriptionPassesTheUploadThrough(t *testing.T) {
	up := newAudioUpstream(t, http.StatusOK, `{"text":"hello world"}`)
	provider := imagesImageProvider("audio-a", false)
	provider.BaseURL = up.server.URL + "/v1"
	provider.RequestAdapter = &domain.RequestAdapter{ModelMapping: map[string]string{"whisper-1": "whisper-vendor"}}
	server := embeddingsServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})

	audio := bytes.Repeat([]byte{0x49, 0x44, 0x33}, 512)
	ctype, body := audioFormBody(t, map[string]string{"model": "whisper-1", "language": "en"}, audio)
	recorder := audioRequest(t, server, "sk-gw-admin", "/v1/audio/transcriptions", ctype, body)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if up.path != "/v1/audio/transcriptions" {
		t.Fatalf("upstream path = %q", up.path)
	}
	if up.auth != "Bearer sk-audio-a" {
		t.Fatalf("upstream authorization = %q, want the provider's own key", up.auth)
	}
	if up.contentType != ctype {
		t.Fatalf("content type changed on the way upstream: %q vs %q", up.contentType, ctype)
	}
	if up.fields["model"] != "whisper-vendor" {
		t.Fatalf("upstream model = %q, want the provider's mapping", up.fields["model"])
	}
	if up.fields["language"] != "en" {
		t.Fatalf("the other form fields must travel unchanged, got %v", up.fields)
	}
	if up.fileLen != len(audio) || up.fileName != "clip.mp3" {
		t.Fatalf("audio part = %d bytes named %q, want %d bytes named clip.mp3", up.fileLen, up.fileName, len(audio))
	}
	if !strings.Contains(recorder.Body.String(), "hello world") {
		t.Fatalf("the transcript should come back unchanged, got %s", recorder.Body.String())
	}
}

// 音频端点必须真的挂在路由表上：处理器写对了但忘了注册，线上就是一个
// 404（嵌入端点刚犯过同样的错）。这里走 Handler() 的 mux，不直接调处理器。
func TestAudioEndpointsAreRouted(t *testing.T) {
	provider := imagesImageProvider("audio-r", false)
	server := embeddingsServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})
	handler := server.Handler()

	for _, path := range []string{
		"/v1/audio/transcriptions",
		"/openai/v1/audio/transcriptions",
		"/v1/audio/translations",
		"/openai/v1/audio/translations",
	} {
		// A JSON body is refused by the handler itself (400), which is proof the
		// request reached it: an unregistered path would answer 404.
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"m"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer sk-gw-admin")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code == http.StatusNotFound {
			t.Fatalf("%s is not routed: got 404", path)
		}
	}
}

// 非 multipart 的音频请求在出发前就拒绝，且不碰上游。
func TestAudioRequiresMultipartForm(t *testing.T) {
	up := newAudioUpstream(t, http.StatusOK, `{}`)
	provider := imagesImageProvider("audio-a", false)
	provider.BaseURL = up.server.URL + "/v1"
	server := embeddingsServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})

	recorder := audioRequest(t, server, "sk-gw-admin", "/v1/audio/transcriptions", "application/json", []byte(`{"model":"whisper-1"}`))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", recorder.Code, recorder.Body.String())
	}
	if up.path != "" {
		t.Fatalf("a non-form request reached the upstream at %q", up.path)
	}
}

// 没有 Key 时 401，请求不会到上游。
func TestAudioRequiresAPIKey(t *testing.T) {
	up := newAudioUpstream(t, http.StatusOK, `{}`)
	provider := imagesImageProvider("audio-a", false)
	provider.BaseURL = up.server.URL + "/v1"
	server := embeddingsServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})

	ctype, body := audioFormBody(t, map[string]string{"model": "whisper-1"}, []byte("x"))
	if recorder := audioRequest(t, server, "", "/v1/audio/transcriptions", ctype, body); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if up.path != "" {
		t.Fatalf("an unauthenticated request reached the upstream at %q", up.path)
	}
}

// 翻译端点走同一套逻辑，只是上游路径不同。
func TestAudioTranslationUsesItsOwnPath(t *testing.T) {
	up := newAudioUpstream(t, http.StatusOK, `{"text":"hi"}`)
	provider := imagesImageProvider("audio-a", false)
	provider.BaseURL = up.server.URL + "/v1"
	server := embeddingsServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})

	ctype, body := audioFormBody(t, map[string]string{"model": "whisper-1"}, []byte("abc"))
	recorder := audioRequest(t, server, "sk-gw-admin", "/v1/audio/translations", ctype, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if up.path != "/v1/audio/translations" {
		t.Fatalf("upstream path = %q, want /v1/audio/translations", up.path)
	}
}

// Claude 输出的路由没有音频端点，必须在出发前拒绝。
func TestAudioRefusesNonOpenAIOutputRoute(t *testing.T) {
	up := newAudioUpstream(t, http.StatusOK, `{}`)
	provider := imagesImageProvider("audio-a", false)
	provider.BaseURL = up.server.URL + "/v1"
	server := embeddingsServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})
	route, err := server.router.RouteByID("img-route")
	if err != nil {
		t.Fatal(err)
	}
	route.OutputProtocol = domain.ProtocolClaude
	if _, err := server.router.UpdateRoute("img-route", route); err != nil {
		t.Fatalf("update route: %v", err)
	}

	ctype, body := audioFormBody(t, map[string]string{"model": "whisper-1"}, []byte("x"))
	if recorder := audioRequest(t, server, "sk-gw-admin", "/v1/audio/transcriptions", ctype, body); recorder.Code == http.StatusOK {
		t.Fatalf("a Claude-output route must not serve audio, got 200")
	}
	if up.path != "" {
		t.Fatalf("the request reached the upstream at %q", up.path)
	}
}

// 上游拒绝时要以 warn 记录，并带上端点名。
func TestAudioLogsUpstreamRejection(t *testing.T) {
	up := newAudioUpstream(t, http.StatusNotFound, `{}`)
	provider := imagesImageProvider("audio-a", false)
	provider.BaseURL = up.server.URL + "/v1"
	server := embeddingsServer(t, provider, domain.APIKey{ID: "k1", Key: "sk-gw-admin"})

	ctype, body := audioFormBody(t, map[string]string{"model": "whisper-1"}, []byte("x"))
	if recorder := audioRequest(t, server, "sk-gw-admin", "/v1/audio/transcriptions", ctype, body); recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want the upstream's 404", recorder.Code)
	}
	found := ""
	for _, entry := range server.logs.ListApp(0) {
		if entry.Message == "handled audio/transcriptions request" {
			found = entry.Level + " " + entry.Context
			break
		}
	}
	if !strings.HasPrefix(found, "warn ") || !strings.Contains(found, "status=404") {
		t.Fatalf("upstream rejection should be logged as warn with its status, got %q", found)
	}
}

// 表单里的 model 是唯一被读的字段；文件部分不进内存第二份。
func TestMultipartFormField(t *testing.T) {
	ctype, body := audioFormBody(t, map[string]string{"model": "  whisper-1  ", "prompt": "x"}, []byte("audio"))
	got, err := multipartFormField(ctype, body, "model")
	if err != nil {
		t.Fatalf("read model: %v", err)
	}
	if got != "whisper-1" {
		t.Fatalf("model = %q, want it trimmed", got)
	}
	if _, err := multipartFormField("application/json", body, "model"); err == nil {
		t.Fatalf("a JSON body must be refused as not a form")
	}
}

// 改写 model 时沿用同一个边界，音频字节与其他字段完全不变；表单里没有 model
// 字段时原样返回，不凭空加一个。
func TestMultipartFormWithFieldKeepsTheRest(t *testing.T) {
	audio := bytes.Repeat([]byte{0x89, 0x50, 0x4e, 0x47}, 256)
	ctype, body := audioFormBody(t, map[string]string{"model": "whisper-1", "language": "en"}, audio)

	rewritten, err := multipartFormWithField(ctype, body, "model", "whisper-vendor")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got, _ := multipartFormField(ctype, rewritten, "model"); got != "whisper-vendor" {
		t.Fatalf("model after rewrite = %q", got)
	}
	if got, _ := multipartFormField(ctype, rewritten, "language"); got != "en" {
		t.Fatalf("language after rewrite = %q, want it untouched", got)
	}

	_, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(bytes.NewReader(rewritten), params["boundary"])
	var file []byte
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("rewritten form does not parse: %v", err)
		}
		if part.FileName() != "" {
			file, _ = io.ReadAll(part)
		}
	}
	if !bytes.Equal(file, audio) {
		t.Fatalf("the audio part changed: %d bytes vs %d", len(file), len(audio))
	}

	// A form that never named a model keeps its bytes as sent.
	ctypeNoModel, bodyNoModel := audioFormBody(t, map[string]string{"language": "en"}, audio)
	same, err := multipartFormWithField(ctypeNoModel, bodyNoModel, "model", "whisper-vendor")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !bytes.Equal(same, bodyNoModel) {
		t.Fatalf("a form without a model field must pass through unchanged")
	}
}
