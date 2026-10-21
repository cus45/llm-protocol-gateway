package gateway

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// maxFirstPacketTimeoutSec bounds the per-provider setting so one mistyped
// value cannot park a request longer than any client is willing to wait.
const maxFirstPacketTimeoutSec = 600

// errUpstreamFirstPacketTimeout replaces the raw context error when the
// first-packet timer fires. Returning context.Canceled would be a lie the
// failover loop cannot survive: isRequestContextCanceled treats it as "the
// client hung up" and stops there, so a provider that never answered would
// never be followed by the next one in the chain.
var errUpstreamFirstPacketTimeout = errors.New("upstream did not return response headers in time")

type firstPacketTimeoutKey struct{}

func withFirstPacketTimeout(ctx context.Context, timeout time.Duration) context.Context {
	if timeout <= 0 {
		return ctx
	}
	return context.WithValue(ctx, firstPacketTimeoutKey{}, timeout)
}

func firstPacketTimeoutFrom(ctx context.Context) time.Duration {
	if ctx == nil {
		return 0
	}
	timeout, _ := ctx.Value(firstPacketTimeoutKey{}).(time.Duration)
	return timeout
}

func providerFirstPacketTimeout(provider domain.Provider) time.Duration {
	if provider.FirstPacketTimeoutSec <= 0 {
		return 0
	}
	if provider.FirstPacketTimeoutSec > maxFirstPacketTimeoutSec {
		return time.Duration(maxFirstPacketTimeoutSec) * time.Second
	}
	return time.Duration(provider.FirstPacketTimeoutSec) * time.Second
}

// headerDeadlineState decides, under one lock, whether the deadline won the
// race with the response headers. Without it a timer firing while the upstream
// was answering would cancel the context of a response the caller already owns
// and cut a healthy stream short.
type headerDeadlineState struct {
	mu       sync.Mutex
	headers  bool
	timedOut bool
}

// fire returns true when the deadline should cancel the request, i.e. the
// headers have not arrived yet.
func (s *headerDeadlineState) fire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.headers {
		return false
	}
	s.timedOut = true
	return true
}

func (s *headerDeadlineState) arrived() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.headers = true
}

func (s *headerDeadlineState) didTimeOut() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.timedOut
}

// doUpstreamHTTP sends the request and enforces the first-packet deadline
// carried by ctx, if there is one. The deadline covers only the wait for the
// upstream's response headers; once they arrive the timer is stopped, so a long
// reasoning stream or a slow body is never cut short.
func doUpstreamHTTP(ctx context.Context, client *http.Client, request *http.Request) (*http.Response, error) {
	timeout := firstPacketTimeoutFrom(ctx)
	if timeout <= 0 {
		return client.Do(request)
	}
	attemptCtx, cancel := context.WithCancel(ctx)
	request = request.WithContext(attemptCtx)
	state := &headerDeadlineState{}
	timer := time.AfterFunc(timeout, func() {
		if state.fire() {
			cancel()
		}
	})
	response, err := client.Do(request)
	state.arrived()
	timer.Stop()
	if err != nil {
		cancel()
		if state.didTimeOut() {
			return nil, errUpstreamFirstPacketTimeout
		}
		return nil, err
	}
	// The cancel is deliberately not called on the success path: the body still
	// streams under attemptCtx, which is released when the request's own
	// context ends.
	return response, nil
}
