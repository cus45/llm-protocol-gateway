package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// handleQoderPATComplete accepts a pasted personal access token, exchanges it
// once to prove it works and to learn whose account it is, and stores the
// result. It is the fallback sign-in: a browser is not needed, which is what a
// headless deployment has to have.
func (s *Server) handleQoderPATComplete(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	if !s.requireProviderOwnerForUser(w, r, providerID) {
		return
	}
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	site := qoderSiteOf(provider)
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "failed to read body: "+err.Error())
		return
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	pat := strings.TrimSpace(payload.Token)
	if pat == "" {
		writeOpenAIError(w, http.StatusBadRequest, "token is required")
		return
	}
	// Exchange up front so a bad token fails here rather than on the first
	// proxied request. 400, not 502: this is Qoder rejecting the *client's*
	// token — a request-input problem, not this gateway failing to reach an
	// upstream. Using 502 here previously made every rejection
	// indistinguishable, over the public tunnel, from a genuine Cloudflare
	// "origin unreachable" page.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	base := domain.QoderCredential{}
	if provider.QoderPAT != nil {
		base = *provider.QoderPAT
	}
	credential, err := exchangeQoderJobToken(ctx, site, base, pat)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.router.SetProviderQoderPAT(providerID, credential)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := s.saveState(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to save configuration: "+err.Error())
		return
	}
	if synced, syncErr := s.syncQoderProviderModels(providerID); syncErr == nil {
		updated = synced
	} else {
		s.logs.AddApp("warn", "qoder model sync failed", syncErr.Error())
	}
	s.logs.AddApp("info", "qoder personal access token connected", providerID)
	writeJSON(w, http.StatusOK, redactProviderForClient(updated))
}

// handleQoderPATStatus reports connection state and token freshness only. It
// never returns token material.
func (s *Server) handleQoderPATStatus(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	if !s.requireProviderOwnerForUser(w, r, providerID) {
		return
	}
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, qoderStatusOf(provider))
}

// qoderStatusOf is what the console may know about a Qoder connection: how it
// was signed in, who it is, and whether the token still has room to run.
func qoderStatusOf(provider domain.Provider) map[string]any {
	status := map[string]any{"connected": false}
	credential := provider.QoderPAT
	if credential == nil {
		return status
	}
	hasToken := qoderPersonalToken(credential) != "" || strings.TrimSpace(credential.RefreshToken) != ""
	mode := credential.SignInMode
	if mode == "" {
		mode = qoderSignInModePAT
	}
	status["signInMode"] = mode
	status["hasStoredToken"] = hasToken
	status["disconnected"] = credential.Disconnected
	status["connected"] = hasToken && !credential.Disconnected
	status["expiresAt"] = credential.ExpiresAt
	status["accountLabel"] = credential.AccountLabel
	status["email"] = credential.Email
	status["deviceChat"] = credential.DeviceChat
	status["tokenStale"] = qoderTokenNeedsRefresh(credential)
	status["models"] = len(provider.Models)
	return status
}

// handleQoderPATDisconnect pauses a Qoder connection: forwarding is blocked
// (see ensureFreshQoderToken) and the console shows it as disconnected, but the
// credential itself is kept, so reconnecting costs no browser trip and no fresh
// token.
func (s *Server) handleQoderPATDisconnect(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	if !s.requireProviderOwnerForUser(w, r, providerID) {
		return
	}
	updated, err := s.router.DisconnectProviderQoderPAT(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	if updated.QoderPAT != nil {
		if err := s.persistProviderOAuth(updated); err != nil {
			s.logs.AddApp("warn", "failed to persist qoder disconnect", err.Error())
		}
	}
	if err := s.saveState(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to save configuration: "+err.Error())
		return
	}
	s.qoderSignIns.finish(providerID)
	s.logs.AddApp("info", "qoder connection disconnected (credential retained for reconnect)", providerID)
	writeJSON(w, http.StatusOK, redactProviderForClient(updated))
}

// handleQoderPATReconnect brings a paused connection back with the credential
// already stored, with no body required. A device-flow account is renewed from
// its refresh token; a token account is exchanged again.
func (s *Server) handleQoderPATReconnect(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	if !s.requireProviderOwnerForUser(w, r, providerID) {
		return
	}
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	if provider.QoderPAT == nil {
		writeOpenAIError(w, http.StatusBadRequest, "没有可重连的 Qoder 登录，请重新登录")
		return
	}
	site := qoderSiteOf(provider)
	credential := *provider.QoderPAT
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var renewed domain.QoderCredential
	if pat := qoderPersonalToken(&credential); pat != "" && credential.SignInMode != qoderSignInModeDevice {
		renewed, err = exchangeQoderJobToken(ctx, site, credential, pat)
	} else {
		renewed, err = renewQoderCredential(ctx, site, credential)
	}
	if err != nil {
		// See handleQoderPATComplete: a refused credential is a client-input
		// problem, and 502 is indistinguishable from a Cloudflare page.
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	renewed.Disconnected = false
	updated, err := s.router.SetProviderQoderPAT(providerID, renewed)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := s.persistProviderOAuth(updated); err != nil {
		s.logs.AddApp("warn", "failed to persist qoder reconnect", err.Error())
	}
	if err := s.saveState(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to save configuration: "+err.Error())
		return
	}
	if synced, syncErr := s.syncQoderProviderModels(providerID); syncErr == nil {
		updated = synced
	} else {
		s.logs.AddApp("warn", "qoder model sync failed", syncErr.Error())
	}
	s.logs.AddApp("info", "qoder reconnected using the stored credential", providerID)
	writeJSON(w, http.StatusOK, redactProviderForClient(updated))
}
