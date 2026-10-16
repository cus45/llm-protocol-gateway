package gateway

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// The console-facing half of the two subscription sign-ins that need a browser:
// ZCode mints a GLM Coding Plan key through zcode.z.ai, and Trae hands its
// Cloud-IDE-JWT back on a loopback callback. Each gets the same four endpoints
// as WorkBuddy — start, status, disconnect, usage — because the console's card
// is the same shape: a login button, a progress line while it waits, and a
// quota panel once it is signed in.

// handleZCodeOAuthStart opens ZCode's browser sign-in for the realm this
// provider was created as, and polls the flow in the background.
func (s *Server) handleZCodeOAuthStart(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	if !s.requireProviderOwnerForUser(w, r, providerID) {
		return
	}
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	site, ok := zcodeSiteForAuthType(provider.AuthType)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "该 Provider 不是 ZCode 连接方式")
		return
	}
	authURL, flowID, startErr := s.startZCodeOAuth(providerID, site)
	if startErr != nil {
		writeOpenAIError(w, http.StatusBadGateway, startErr.Error())
		return
	}
	s.logs.AddApp("info", "zcode oauth flow started", providerID)
	writeJSON(w, http.StatusOK, map[string]any{
		"authUrl": authURL,
		"flowId":  flowID,
		"mode":    "poll",
	})
}

func (s *Server) handleZCodeOAuthStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviderOwnerForUser(w, r, r.PathValue("id")) {
		return
	}
	flowID := strings.TrimSpace(r.URL.Query().Get("flowId"))
	if flowID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "flowId is required")
		return
	}
	status, ok := s.subscriptionOAuthFlows.status(flowID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "unknown or expired oauth flow")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleZCodeOAuthPause keeps the minted key — it does not expire, so a
// reconnect costs no browser trip — and only stops forwarding with it.
func (s *Server) handleZCodeOAuthPause(w http.ResponseWriter, r *http.Request) {
	s.setZCodeDisconnected(w, r, true)
}

func (s *Server) handleZCodeOAuthResume(w http.ResponseWriter, r *http.Request) {
	s.setZCodeDisconnected(w, r, false)
}

func (s *Server) setZCodeDisconnected(w http.ResponseWriter, r *http.Request, disconnected bool) {
	providerID := r.PathValue("id")
	if !s.requireProviderOwnerForUser(w, r, providerID) {
		return
	}
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	if provider.ZCode == nil {
		writeOpenAIError(w, http.StatusBadRequest, "ZCode 尚未登录")
		return
	}
	credential := *provider.ZCode
	credential.Disconnected = disconnected
	updated, err := s.router.SetProviderZCode(providerID, credential)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.saveState(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to save configuration: "+err.Error())
		return
	}
	if disconnected {
		s.subscriptionOAuthFlows.finish(providerID)
		s.logs.AddApp("info", "zcode paused", providerID)
	} else {
		s.logs.AddApp("info", "zcode resumed", providerID)
	}
	writeJSON(w, http.StatusOK, redactProviderForClient(updated))
}

// handleZCodeUsage reports the plan behind the minted key. The key and the
// business token never leave the server; the plan name and the used shares do.
func (s *Server) handleZCodeUsage(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	if !s.requireProviderOwnerForUser(w, r, providerID) {
		return
	}
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	site, ok := zcodeSiteForProvider(provider)
	if !ok || provider.ZCode == nil || provider.ZCode.Disconnected {
		writeJSON(w, http.StatusOK, map[string]any{"error": "ZCode 尚未登录", "windows": []any{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, zcodeQuotaFor(ctx, site, *provider.ZCode))
}

// handleTraeOAuthStart opens Trae's authorization page. Its callback is a
// loopback address on the machine running the gateway, so a gateway reached
// over the network signs in from that machine — Trae accepts nothing else.
func (s *Server) handleTraeOAuthStart(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	if !s.requireProviderOwnerForUser(w, r, providerID) {
		return
	}
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	site, ok := traeSiteForAuthType(provider.AuthType)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "该 Provider 不是 Trae 连接方式")
		return
	}
	authURL, flowID, startErr := s.startTraeOAuth(providerID, site)
	if startErr != nil {
		writeOpenAIError(w, http.StatusBadGateway, startErr.Error())
		return
	}
	s.logs.AddApp("info", "trae oauth flow started", providerID)
	writeJSON(w, http.StatusOK, map[string]any{
		"authUrl":   authURL,
		"flowId":    flowID,
		"mode":      "callback",
		"hint":      "请在运行网关的这台机器上完成授权：Trae 只接受回环地址的回调。",
		"expiresAt": time.Now().Add(traeSignInDeadline).Format(time.RFC3339),
	})
}

func (s *Server) handleTraeOAuthStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviderOwnerForUser(w, r, r.PathValue("id")) {
		return
	}
	flowID := strings.TrimSpace(r.URL.Query().Get("flowId"))
	if flowID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "flowId is required")
		return
	}
	status, ok := s.subscriptionOAuthFlows.status(flowID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "unknown or expired oauth flow")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleTraeOAuthPause(w http.ResponseWriter, r *http.Request) {
	s.setTraeDisconnected(w, r, true)
}

func (s *Server) handleTraeOAuthResume(w http.ResponseWriter, r *http.Request) {
	s.setTraeDisconnected(w, r, false)
}

func (s *Server) setTraeDisconnected(w http.ResponseWriter, r *http.Request, disconnected bool) {
	providerID := r.PathValue("id")
	if !s.requireProviderOwnerForUser(w, r, providerID) {
		return
	}
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	if provider.Trae == nil {
		writeOpenAIError(w, http.StatusBadRequest, "Trae 尚未登录")
		return
	}
	credential := *provider.Trae
	credential.Disconnected = disconnected
	updated, err := s.router.SetProviderTrae(providerID, provider.AuthType, credential)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.saveState(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to save configuration: "+err.Error())
		return
	}
	if disconnected {
		s.subscriptionOAuthFlows.finish(providerID)
		s.logs.AddApp("info", "trae paused", providerID)
	} else {
		s.logs.AddApp("info", "trae resumed", providerID)
	}
	writeJSON(w, http.StatusOK, redactProviderForClient(updated))
}

// handleTraeUsage reports the account's plan and allowance. Tokens stay here;
// only the plan name and the used/limit numbers go out.
func (s *Server) handleTraeUsage(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	if !s.requireProviderOwnerForUser(w, r, providerID) {
		return
	}
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	if provider.Trae == nil || provider.Trae.Disconnected {
		writeJSON(w, http.StatusOK, map[string]any{"error": "Trae 尚未登录", "windows": []any{}})
		return
	}
	refreshed, err := s.ensureFreshTraeToken(provider)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": err.Error(), "windows": []any{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	quota := traeQuotaFor(ctx, refreshed)
	// The renewal may have rotated the refresh token; keep the stored one current.
	if refreshed.Trae != nil && provider.Trae != nil && refreshed.Trae.AccessToken != provider.Trae.AccessToken {
		_ = s.persistProviderOAuth(refreshed)
	}
	writeJSON(w, http.StatusOK, quota)
}
