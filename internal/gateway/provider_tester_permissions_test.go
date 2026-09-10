package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// requireProviderTesterForUser must let a normal user test (fetch-models /
// chat-test) any provider they can see: their own, and ones the admin granted
// to them. Un-granted providers stay 403, and edit/delete keep the stricter
// owner-only check.
func TestRequireProviderTesterForUser(t *testing.T) {
	router := NewRouter(domain.GatewayState{Providers: []domain.Provider{
		{ID: "own", Name: "Own", Protocol: domain.ProtocolOpenAIChat, OwnerUserID: "u1", BaseURL: "https://own.example/v1"},
		{ID: "granted", Name: "Granted", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://granted.example/v1"},
		{ID: "other", Name: "Other", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://other.example/v1"},
	}})
	server := NewServer(router, monitor.NewStore())
	server.userStore = newMemoryUserStore(domain.User{
		ID: "u1", Username: "u1", Role: domain.UserRoleUser,
		AllowedProviderIDs: []string{"granted"},
	})

	withUser := func(providerID string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/__providers/"+providerID+"/test", nil)
		return r.WithContext(context.WithValue(r.Context(), authContextKey{}, sessionIdentity{UserID: "u1", Role: domain.UserRoleUser}))
	}

	for _, providerID := range []string{"own", "granted"} {
		rec := httptest.NewRecorder()
		if !server.requireProviderTesterForUser(rec, withUser(providerID), providerID) {
			t.Fatalf("provider %q should be testable by its owner / a granted user, got %d %s", providerID, rec.Code, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	if server.requireProviderTesterForUser(rec, withUser("other"), "other") {
		t.Fatal("un-granted provider must stay forbidden")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("un-granted provider status = %d, want 403", rec.Code)
	}

	// Edit/delete stay owner-only even for granted providers.
	rec = httptest.NewRecorder()
	if server.requireProviderOwnerForUser(rec, withUser("granted"), "granted") {
		t.Fatal("granted-but-not-owned provider must not pass the owner-only check")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("owner-only check status = %d, want 403", rec.Code)
	}
}
