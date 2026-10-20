package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func (s *Server) handleChatGPTOAuthStart(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	if !s.requireProviderOwnerForUser(w, r, providerID) {
		return
	}
	if _, err := s.router.ProviderByID(providerID); err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}

	verifier, challenge, err := generateChatGPTPKCE()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to generate pkce: "+err.Error())
		return
	}
	state, err := generateChatGPTOAuthState()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to generate oauth state: "+err.Error())
		return
	}
	flowID, err := generateChatGPTOAuthState()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to generate flow id: "+err.Error())
		return
	}

	pending := chatgptOAuthPending{
		Verifier:  verifier,
		State:     state,
		FlowID:    flowID,
		CreatedAt: time.Now(),
		claim:     &atomic.Bool{},
	}
	blocked := ""
	// Give up any callback listener an earlier flow still holds before taking it
	// again, then bind.
	s.pendingChatGPTOAuth.releaseCallbackListeners()
	blocked = s.attachChatGPTOAuthLocalListener(&pending)
	s.pendingChatGPTOAuth.put(providerID, pending)
	s.pendingChatGPTOAuth.setStatus(flowID, "pending", "等待浏览器授权…")
	// The paste-in finish is offered from the start, not only after the port
	// turns out to be unreachable: with the gateway on a server, the browser's
	// redirect can never come back on its own.
	s.pendingChatGPTOAuth.setCallbackHint(flowID, chatgptOAuthFlowStatus{
		PasteCallback:   true,
		CallbackPort:    1455,
		CallbackBlocked: blocked,
		Instructions:    chatgptPasteInstructions,
	})
	if blocked != "" {
		message := "本机 1455 端口已被其他程序占用，浏览器回调进不来；授权完成后请把浏览器地址栏里的整条地址粘贴到下方"
		if blocked == "reserved" {
			message = "Windows 把 1455 端口整段保留了，本机监听不了；授权完成后请把浏览器地址栏里的整条地址粘贴到下方"
		}
		s.pendingChatGPTOAuth.setStatus(flowID, "pending", message)
	}

	authURL := buildChatGPTAuthorizeURL(challenge, state)
	s.logs.AddApp("info", "chatgpt oauth flow started", providerID)
	writeJSON(w, http.StatusOK, map[string]any{
		"authUrl":         authURL,
		"state":           state,
		"flowId":          flowID,
		"mode":            "localhost",
		"pasteCallback":   true,
		"callbackPort":    1455,
		"callbackBlocked": blocked,
		"instructions":    chatgptPasteInstructions,
	})
}

func (s *Server) handleChatGPTOAuthStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireProviderOwnerForUser(w, r, r.PathValue("id")) {
		return
	}
	flowID := strings.TrimSpace(r.URL.Query().Get("flowId"))
	if flowID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "flowId is required")
		return
	}
	status, ok := s.pendingChatGPTOAuth.status(flowID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "unknown or expired oauth flow")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleChatGPTOAuthCallback(w http.ResponseWriter, r *http.Request) {
	callbackState := strings.TrimSpace(r.URL.Query().Get("state"))
	if oauthError := strings.TrimSpace(r.URL.Query().Get("error")); oauthError != "" {
		desc := strings.TrimSpace(r.URL.Query().Get("error_description"))
		message := oauthError
		if desc != "" {
			message += ": " + desc
		}
		s.pendingChatGPTOAuth.setStatusByState(callbackState, "error", message)
		http.Error(w, message, http.StatusBadRequest)
		return
	}

	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	if callbackState == "" {
		http.Error(w, "missing state", http.StatusBadRequest)
		return
	}

	providerID, pending, ok := s.pendingChatGPTOAuth.getByState(callbackState)
	if !ok {
		http.Error(w, "oauth flow expired or unknown state; start again from Protocol Gateway", http.StatusBadRequest)
		return
	}
	if !claimChatGPTCallback(pending) {
		// The code was already spent by the paste-in finish. Say so plainly
		// rather than exchanging again, which OpenAI would reject.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><html><body style='font-family:sans-serif;padding:32px'><h2>这次授权已经处理过了</h2><p>回调地址已被使用一次（浏览器回调与粘贴只会算一次）。账号没连上的话，请回控制台重新点一次连接。</p></body></html>"))
		return
	}
	if err := s.finishChatGPTOAuthExchange(providerID, pending.FlowID, pending, code); err != nil {
		s.logs.AddApp("error", "chatgpt oauth callback exchange failed", err.Error())
		http.Error(w, "failed to exchange oauth code", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!doctype html><html><body style='font-family:sans-serif;padding:32px'><h2>ChatGPT 账号已连接</h2><p>可以关闭此页面并返回 Protocol Gateway。</p></body></html>"))
}

func (s *Server) handleChatGPTOAuthComplete(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	if !s.requireProviderOwnerForUser(w, r, providerID) {
		return
	}
	if _, err := s.router.ProviderByID(providerID); err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	var payload struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	code, state := parseChatGPTOAuthInput(payload.Code, payload.State)
	if code == "" {
		writeOpenAIError(w, http.StatusBadRequest, "code is required")
		return
	}

	pending, ok := s.pendingChatGPTOAuth.take(providerID)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "no pending chatgpt oauth flow for this provider (it may have expired); start again")
		return
	}
	if state == "" {
		state = pending.State
	}
	if state != pending.State {
		writeOpenAIError(w, http.StatusBadRequest, "oauth state mismatch; start the flow again")
		return
	}
	if !claimChatGPTCallback(pending) {
		// The browser's own callback got here first: the code is spent, and a
		// second exchange would fail with invalid_grant.
		writeOpenAIError(w, http.StatusBadRequest, "这个授权流程已经处理过了（浏览器回调先到了）：刷新看账号是否已连上，没连上就重新点一次连接")
		return
	}
	if err := s.finishChatGPTOAuthExchange(providerID, "", pending, code); err != nil {
		s.logs.AddApp("error", "chatgpt oauth code exchange failed", err.Error())
		writeOpenAIError(w, http.StatusBadGateway, "failed to exchange chatgpt oauth code: "+err.Error())
		return
	}

	updated, err := s.router.ProviderByID(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, redactProviderForClient(updated))
}

func (s *Server) handleChatGPTOAuthDisconnect(w http.ResponseWriter, r *http.Request) {
	providerID := r.PathValue("id")
	if !s.requireProviderOwnerForUser(w, r, providerID) {
		return
	}
	updated, err := s.router.ClearProviderChatGPTOAuth(providerID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := s.saveState(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to save configuration: "+err.Error())
		return
	}
	s.logs.AddApp("info", "chatgpt oauth disconnected", providerID)
	writeJSON(w, http.StatusOK, redactProviderForClient(updated))
}

// chatgptCallbackAddr is fixed: OpenAI only sends Codex's client back to port
// 1455, so the gateway cannot pick a free one the way Claude's flow does.
const chatgptCallbackAddr = "127.0.0.1:1455"

// chatgptPasteInstructions is what a user on another machine has to be told to
// do: the browser ends on an address that will not load, and that address is
// what finishes the sign-in.
const chatgptPasteInstructions = "授权完成后浏览器会停在 http://localhost:1455/auth/callback?code=… 。这个页面打不开是正常的（网关不在你这台机器上时必然打不开）：把地址栏里那条完整地址复制粘贴到下面，一样能收尾。"

// attachChatGPTOAuthLocalListener takes Codex's callback port and serves the
// browser's redirect from it. A `codex login` left waiting on the port is asked
// to stop first, the way Codex itself does it. A port that cannot be had does
// not end the sign-in — it only means the pasted-address finish is the way in —
// so the reason is reported and the flow keeps waiting.
func (s *Server) attachChatGPTOAuthLocalListener(pending *chatgptOAuthPending) string {
	var listener net.Listener
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		listener, lastErr = net.Listen("tcp", chatgptCallbackAddr)
		if lastErr == nil {
			break
		}
		if attempt == 0 {
			client := &http.Client{Timeout: 2 * time.Second}
			if response, err := client.Get("http://" + chatgptCallbackAddr + "/cancel"); err == nil {
				_ = response.Body.Close()
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", s.handleChatGPTOAuthCallback)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	blocked := ""
	if listener == nil {
		blocked = chatgptCallbackBlockedReason(lastErr)
	} else {
		go func() { _ = server.Serve(listener) }()
	}
	pending.shutdown = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		if listener != nil {
			_ = listener.Close()
		}
	}
	return blocked
}

// chatgptCallbackBlockedReason names why the port could not be taken, in the two
// words the console needs: Windows reserves whole port ranges (a bind refused
// with 10013 is not something a restart fixes), while "busy" is another program
// — usually a leftover Codex CLI — holding it.
func chatgptCallbackBlockedReason(err error) string {
	var errno syscall.Errno
	if runtime.GOOS == "windows" && errors.As(err, &errno) && errno == 10013 {
		return "reserved"
	}
	return "busy"
}

// claimChatGPTCallback lets only the first of the browser callback and a pasted
// callback URL spend the code.
func claimChatGPTCallback(pending chatgptOAuthPending) bool {
	return pending.claim == nil || pending.claim.CompareAndSwap(false, true)
}

func parseChatGPTOAuthInput(rawCode, rawState string) (code string, state string) {
	raw := strings.TrimSpace(rawCode)
	if raw == "" {
		return "", strings.TrimSpace(rawState)
	}
	if strings.Contains(raw, "://") {
		if parsed, err := url.Parse(raw); err == nil {
			q := parsed.Query()
			if c := strings.TrimSpace(q.Get("code")); c != "" {
				code = c
			}
			if st := strings.TrimSpace(q.Get("state")); st != "" {
				state = st
			}
		}
	}
	if code == "" {
		code = raw
	}
	if state == "" {
		state = strings.TrimSpace(rawState)
	}
	return code, state
}
