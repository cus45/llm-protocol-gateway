package gateway

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"identity", false},
		{"gzip", true},
		{"deflate, gzip, br", true},
		{"gzip;q=0.5", true},
		{"gzip;q=0", false},
		{"br;q=1.0, gzip; q=0", false},
	}
	for _, tc := range cases {
		if got := acceptsGzip(tc.header); got != tc.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

func TestWriteJSONGzipRoundTrips(t *testing.T) {
	payload := map[string]any{"items": []map[string]any{{"model": "claude-sonnet-4-5", "inputTokens": 12}}}

	for _, header := range []string{"gzip", ""} {
		req := httptest.NewRequest(http.MethodGet, "/__usage-analysis", nil)
		if header != "" {
			req.Header.Set("Accept-Encoding", header)
		}
		rec := httptest.NewRecorder()
		writeJSONGzip(rec, req, http.StatusOK, payload)

		body := rec.Body.Bytes()
		if header == "gzip" {
			if rec.Header().Get("Content-Encoding") != "gzip" {
				t.Fatalf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
			}
			reader, err := gzip.NewReader(strings.NewReader(string(body)))
			if err != nil {
				t.Fatalf("not a gzip body: %v", err)
			}
			body, err = io.ReadAll(reader)
			if err != nil {
				t.Fatalf("read gzip body: %v", err)
			}
		} else if rec.Header().Get("Content-Encoding") != "" {
			t.Fatalf("plain client got Content-Encoding %q", rec.Header().Get("Content-Encoding"))
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("body is not JSON: %v", err)
		}
		if _, ok := decoded["items"]; !ok {
			t.Fatalf("decoded body lost items: %s", body)
		}
	}
}
