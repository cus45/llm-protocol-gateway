package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Both converted-proxy helpers must hand the upstream's Retry-After to the
// client writer: the failover writer reads it back to size the cooldown, and the
// client gets to see when to retry. Neither used to forward any header.
func TestConvertedProxyForwardsRetryAfter(t *testing.T) {
	t.Parallel()
	upstreamError := func() *http.Response {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header: http.Header{
				"Retry-After":  []string{"77"},
				"Content-Type": []string{"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
		}
	}
	failing := func(body []byte, model string) ([]byte, TokenUsage, error) {
		return nil, TokenUsage{}, errors.New("cannot convert")
	}
	passThrough := func(body []byte, model string) ([]byte, TokenUsage, error) {
		return body, TokenUsage{}, nil
	}
	failingStream := func(w http.ResponseWriter, reader io.Reader, model string) (TokenUsage, error) {
		return TokenUsage{}, errors.New("cannot stream")
	}

	cases := []struct {
		name    string
		convert responseConverter
		finish  func(*Server, http.ResponseWriter, *http.Response) (int, TokenUsage, []byte, error, bool)
	}{
		{
			name:    "converted through responses",
			convert: passThrough,
			finish: func(s *Server, w http.ResponseWriter, resp *http.Response) (int, TokenUsage, []byte, error, bool) {
				return s.finishConvertedProxy(w, resp, "m", false, passThrough, failingStream)
			},
		},
		{
			name:    "converted through claude",
			convert: failing,
			finish: func(s *Server, w http.ResponseWriter, resp *http.Response) (int, TokenUsage, []byte, error, bool) {
				return s.finishConvertedProxy(w, resp, "m", false, failing, failingStream)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			w := newFailoverResponseWriter(httptest.NewRecorder(), true)
			status, _, _, _, _ := tc.finish(s, w, upstreamError())
			if status != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", status)
			}
			if got := w.UpstreamHeader("Retry-After"); got != "77" {
				t.Fatalf("Retry-After = %q, want 77", got)
			}
		})
	}
}
