package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsModelProtocolPathEmbeddings(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/embeddings", true},
		{"/v1/audio/transcriptions", true},
		{"/v1/audio/translations", true},
		{"/v1/embeddings?x=1", true},
		{"/openai/v1/embeddings", true},
		{"/v1/chat/completions", true},
		{"/v1/images/generations", true},
		{"/v1/models", true},
		{"/api/providers", false},
		{"/", false},
	}
	for _, tc := range cases {
		if got := isModelProtocolPath(tc.path); got != tc.want {
			t.Fatalf("isModelProtocolPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// The API hostname must route /v1/embeddings to the gateway instead of
// answering "management UI is not available on the API hostname", and the UI
// hostname must refuse it instead of serving the SPA fallback.
func TestHostSeparatedServingRoutesEmbeddings(t *testing.T) {
	hits := 0
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	})
	roleForHost := func(host string) string {
		switch host {
		case "api.example.com":
			return "api"
		case "ui.example.com":
			return "ui"
		}
		return ""
	}
	handler := withHostSeparatedServing(api, t.TempDir(), roleForHost)

	// Every OpenAI-compatible passthrough endpoint is a model call: the API
	// hostname must reach the gateway, the UI hostname must not.
	for _, path := range []string{"/v1/embeddings", "/v1/audio/transcriptions", "/v1/audio/translations"} {
		for _, tc := range []struct {
			host      string
			wantToAPI bool
		}{
			{"api.example.com", true},
			{"ui.example.com", false},
		} {
			before := hits
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "http://"+tc.host+path, nil)
			handler.ServeHTTP(rec, req)
			reached := hits > before
			if reached != tc.wantToAPI {
				t.Fatalf("%s on host %s: reached api = %v, want %v (status %d)", path, tc.host, reached, tc.wantToAPI, rec.Code)
			}
		}
	}
}

func TestHostSeparatedServingRoutesEmbeddingsLocally(t *testing.T) {
	hits := 0
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	})
	handler := withHostSeparatedServing(api, t.TempDir(), nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18090/v1/embeddings", nil)
	handler.ServeHTTP(rec, req)
	if hits != 1 {
		t.Fatalf("local request must reach the gateway, hits=%d status=%d", hits, rec.Code)
	}
}
