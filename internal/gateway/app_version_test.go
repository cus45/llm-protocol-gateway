package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// The console learns that a new release is live by comparing its own build
// version with the one this endpoint reports.
func TestAppVersionEndpointReportsTheBuildVersion(t *testing.T) {
	previous := currentBuildVersion()
	t.Cleanup(func() { SetBuildVersion(previous) })
	SetBuildVersion("20261024.021934")

	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/__app/version", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, recorder.Body.String())
	}
	if payload.Version != "20261024.021934" {
		t.Fatalf("version = %q, want the stamped build version", payload.Version)
	}
	// A release must be visible to an open tab on its next poll, so the answer
	// may not be cached anywhere along the way.
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

// The version is readable before login: a tab sitting on the login screen must
// also be able to learn that a release went live.
func TestAppVersionIsReachableOnTheLoginScreen(t *testing.T) {
	previous := currentBuildVersion()
	t.Cleanup(func() { SetBuildVersion(previous) })
	SetBuildVersion("20261024.022000")

	server := NewServer(NewRouter(domain.GatewayState{}), monitor.NewStore())
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/__app/version", nil))
	if recorder.Code == http.StatusUnauthorized || recorder.Code == http.StatusFound {
		t.Fatalf("version endpoint answered %d: a login screen cannot poll it", recorder.Code)
	}
}
