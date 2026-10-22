package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func TestShouldRectifySamplingRefusal(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "anthropic deprecated temperature",
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"` + "`temperature`" + ` is deprecated for this model."}}`,
			want: true,
		},
		{
			name: "not supported top_p",
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"` + "`top_p`" + ` is not supported for this model."}}`,
			want: true,
		},
		{
			name: "relay without backticks",
			body: `top_k is not supported for this model`,
			want: true,
		},
		{
			name: "unsupported parameter naming temperature",
			body: `{"error":{"message":"invalid request: unsupported parameter: temperature"}}`,
			want: true, // names the field and the refusal phrase
		},
		{
			name: "thinking signature error is a different family",
			body: `{"type":"error","error":{"message":"messages.1.content.0: Invalid ` + "`signature`" + ` in ` + "`thinking`" + ` block"}}`,
			want: false,
		},
		{
			name: "temperature out of range is not a refusal of the parameter",
			body: `{"type":"error","error":{"message":"temperature: Input should be a valid number"}}`,
			want: false,
		},
		{
			name: "empty",
			body: ``,
			want: false,
		},
	}
	for _, tc := range cases {
		if got := shouldRectifySamplingRefusal([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: shouldRectifySamplingRefusal = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStripSamplingFields(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","temperature":0,"top_p":1,"top_k":5,"max_tokens":64,"messages":[]}`)
	out, changed := stripSamplingFields(body)
	if !changed {
		t.Fatal("expected changed")
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("not json: %v", err)
	}
	for _, key := range []string{"temperature", "top_p", "top_k"} {
		if _, exists := payload[key]; exists {
			t.Errorf("%s survived the strip", key)
		}
	}
	for _, key := range []string{"model", "max_tokens", "messages"} {
		if _, exists := payload[key]; !exists {
			t.Errorf("%s was dropped", key)
		}
	}

	if _, changed := stripSamplingFields([]byte(`{"model":"m"}`)); changed {
		t.Fatal("body without sampling fields reported changed")
	}
	if _, changed := stripSamplingFields([]byte(`not json`)); changed {
		t.Fatal("non-json body reported changed")
	}
}

// TestMaybeRectifySamplingRefusalRetries 验证：上游 400 拒绝 temperature 时，
// 整流器去掉采样字段重发一次，客户端拿到的是重试后的 200。
func TestMaybeRectifySamplingRefusalRetries(t *testing.T) {
	var attempts int32
	var secondBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		raw, _ := io.ReadAll(r.Body)
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"` + "`temperature`" + ` is deprecated for this model."}}`))
			return
		}
		secondBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","id":"msg_1","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer upstream.Close()

	server := &Server{logs: monitor.NewStore()}
	provider := domain.Provider{ID: "p1", Name: "p1", BaseURL: upstream.URL}
	body := []byte(`{"model":"claude-sonnet-5","temperature":0,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	resend := func(retryBody []byte) (*http.Response, error) {
		httpReq, reqErr := http.NewRequest(http.MethodPost, upstream.URL+"/v1/messages", strings.NewReader(string(retryBody)))
		if reqErr != nil {
			return nil, reqErr
		}
		return http.DefaultClient.Do(httpReq)
	}

	first, err := resend(body)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	got, ok := server.maybeRectifySamplingRefusal(req, provider, body, first, resend)
	if !ok {
		t.Fatal("expected a retry after sampling refusal")
	}
	defer got.Body.Close()
	if got.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", got.StatusCode)
	}
	if strings.Contains(secondBody, "temperature") {
		t.Fatalf("retry still carried temperature: %s", secondBody)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

// TestMaybeRectifySamplingRefusalLeavesOtherErrors 验证非采样类 400 原样返回，
// 不触发重发。
func TestMaybeRectifySamplingRefusalLeavesOtherErrors(t *testing.T) {
	server := &Server{logs: monitor.NewStore()}
	provider := domain.Provider{ID: "p1"}
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	body := []byte(`{"model":"m","temperature":0}`)
	errBody := `{"type":"error","error":{"message":"max_tokens: field required"}}`
	first := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(errBody)),
		Header:     http.Header{},
	}
	resent := false
	got, ok := server.maybeRectifySamplingRefusal(httpReq, provider, body, first, func([]byte) (*http.Response, error) {
		resent = true
		return nil, nil
	})
	if ok || resent {
		t.Fatalf("retried on unrelated error: ok=%v resent=%v", ok, resent)
	}
	restored, _ := io.ReadAll(got.Body)
	if string(restored) != errBody {
		t.Fatalf("original body not restored: %s", restored)
	}
}
