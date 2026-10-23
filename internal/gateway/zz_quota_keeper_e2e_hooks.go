package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// Test hooks for the end-to-end quota-keeper tests that live in internal/app:
// they exercise the real SQLite store, so they cannot reuse the in-package fakes.

// RunQuotaKeeperTickForTest runs one keeper tick synchronously.
func (s *Server) RunQuotaKeeperTickForTest(ctx context.Context) {
	s.runDueScheduledTasks(ctx)
}

// HandleRestoreProviderForTest drives POST /__providers/{id}/restore as an admin,
// the same door the console uses.
func (s *Server) HandleRestoreProviderForTest(w http.ResponseWriter, providerID string) {
	r := httptest.NewRequest(http.MethodPost, "/__providers/"+providerID+"/restore", nil)
	r.SetPathValue("id", providerID)
	identity := sessionIdentity{UserID: "test-admin", Username: "test-admin", Role: domain.UserRoleAdmin}
	s.handleRestoreProvider(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, identity)))
}
