package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// failingStateSaver stands in for a store whose Save failed (locked database,
// disk full).
type failingStateSaver struct{ saves int }

func (f *failingStateSaver) Save(domain.GatewayState) error {
	f.saves++
	return errFailingSave
}

var errFailingSave = errStateSaverUnavailable{}

type errStateSaverUnavailable struct{}

func (errStateSaverUnavailable) Error() string { return "store is not writable" }

func adminPurgeRequest(providerID string) *http.Request {
	request := httptest.NewRequest(http.MethodDelete, "/__providers/"+providerID+"/purge", nil)
	request.SetPathValue("id", providerID)
	return request.WithContext(context.WithValue(request.Context(), authContextKey{}, adminIdentity()))
}

// A purge whose Save fails must not leave the provider half-purged: the row is
// back in the in-memory state and the admin is told the purge did not happen.
// Leaving it out of memory with the row still in the store also made the next
// full Save fail on "UNIQUE constraint failed: providers.id".
func TestPurgeProviderRestoresStateWhenSaveFails(t *testing.T) {
	t.Parallel()
	state := domain.GatewayState{Providers: []domain.Provider{{
		ID: "p-purge", Name: "p-purge", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://example.com/v1",
		Deleted: true, DeletedAt: "2026-01-01T00:00:00Z",
	}}}
	server := NewServer(NewRouter(state), monitor.NewStore())
	saver := &failingStateSaver{}
	server.stateSaver = saver

	recorder := httptest.NewRecorder()
	server.handlePurgeProvider(recorder, adminPurgeRequest("p-purge"))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", recorder.Code, recorder.Body.String())
	}
	if saver.saves == 0 {
		t.Fatal("Save was never attempted")
	}
	if _, err := server.router.ProviderByID("p-purge"); err == nil {
		t.Fatal("a failed purge must not leave the provider live")
	}
	found := false
	for _, provider := range server.router.DeletedProviders() {
		if provider.ID == "p-purge" {
			found = true
		}
	}
	if !found {
		t.Fatal("a failed purge must put the provider back in the trash")
	}
}

// And the happy path still removes it for good.
func TestPurgeProviderRemovesWhenSaveSucceeds(t *testing.T) {
	t.Parallel()
	state := domain.GatewayState{Providers: []domain.Provider{{
		ID: "p-purge", Name: "p-purge", Protocol: domain.ProtocolOpenAIChat, BaseURL: "https://example.com/v1",
		Deleted: true, DeletedAt: "2026-01-01T00:00:00Z",
	}}}
	server := NewServer(NewRouter(state), monitor.NewStore())
	server.stateSaver = savingRecorder{}

	recorder := httptest.NewRecorder()
	server.handlePurgeProvider(recorder, adminPurgeRequest("p-purge"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &payload)
	if payload["purged"] != true {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	if len(server.router.DeletedProviders()) != 0 {
		t.Fatal("a successful purge must remove the provider from the trash")
	}
	if _, err := server.router.ProviderByIDIncludingDeleted("p-purge"); err == nil {
		t.Fatal("a successful purge must remove the provider entirely")
	}
}

type savingRecorder struct{}

func (savingRecorder) Save(domain.GatewayState) error { return nil }
