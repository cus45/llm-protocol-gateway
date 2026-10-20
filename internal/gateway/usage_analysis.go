package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

func captureRequestAnalysis(r *http.Request, keyID string, body []byte) monitor.RequestAnalysis {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return monitor.RequestAnalysis{}
	}
	a := monitor.RequestAnalysis{Captured: true}
	_ = json.Unmarshal(payload["stream"], &a.Stream)
	var messages []map[string]json.RawMessage
	_ = json.Unmarshal(payload["messages"], &messages)
	if len(messages) == 0 {
		_ = json.Unmarshal(payload["input"], &messages)
	}
	a.Messages = len(messages)
	a.SystemBytes = len(payload["system"]) + len(payload["instructions"])
	for _, m := range messages {
		var role, kind string
		_ = json.Unmarshal(m["role"], &role)
		_ = json.Unmarshal(m["type"], &kind)
		raw, _ := json.Marshal(m)
		if role == "system" || role == "developer" {
			a.SystemBytes += len(raw)
		} else {
			a.MessageBytes += len(raw)
		}
		if role == "tool" || kind == "function_call_output" {
			a.ToolResults++
		}
		var blocks []struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(m["content"], &blocks)
		for _, b := range blocks {
			if b.Type == "tool_result" {
				a.ToolResults++
			}
		}
	}
	if a.Messages == 0 && len(payload["input"]) > 0 {
		a.Messages = 1
		a.MessageBytes = len(payload["input"])
	}
	var tools []json.RawMessage
	_ = json.Unmarshal(payload["tools"], &tools)
	a.Tools = len(tools)
	a.ToolBytes = len(payload["tools"])
	session := ""
	if r != nil {
		for _, header := range []string{"Session_id", "X-Session-Id", "X-Claude-Code-Session-Id"} {
			if session = strings.TrimSpace(r.Header.Get(header)); session != "" {
				break
			}
		}
	}
	if session == "" {
		_ = json.Unmarshal(payload["session_id"], &session)
	}
	if session == "" {
		var meta struct {
			SessionID string `json:"session_id"`
			UserID    string `json:"user_id"`
		}
		_ = json.Unmarshal(payload["metadata"], &meta)
		session = meta.SessionID
		// Claude Code metadata embeds an explicit session suffix; a user ID alone
		// must never merge unrelated conversations into one session.
		if session == "" {
			if i := strings.LastIndex(meta.UserID, "_session_"); i >= 0 {
				session = meta.UserID[i+9:]
			}
		}
	}
	if session != "" {
		digest := sha256.Sum256([]byte(keyID + "\x00" + session))
		a.SessionID = hex.EncodeToString(digest[:16])
	}
	return a
}

func (s *Server) handleUsageAnalysis(w http.ResponseWriter, r *http.Request) {
	from, err := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
	if err != nil {
		writeOpenAIError(w, 400, "invalid from")
		return
	}
	to, err := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
	if err != nil || !to.After(from) {
		writeOpenAIError(w, 400, "invalid to")
		return
	}
	query := monitor.RequestLogQuery{From: from, To: to, Status: "all", PageSize: 500}
	identity := s.requestIdentity(r)
	if !identity.isAdmin() {
		query.APIKeyIDs = s.ownedKeyIDs(identity.UserID)
	}
	s.FlushRequestLogs()
	items := make([]monitor.RequestLog, 0)
	total := 0
	for query.Page = 1; ; query.Page++ {
		var page monitor.RequestLogPage
		if s.requestLogStore != nil {
			page, err = s.requestLogStore.QueryRequestLogs(query)
		} else {
			page = s.logs.Query(query)
		}
		if err != nil {
			writeOpenAIError(w, 500, "failed to read usage analysis")
			return
		}
		total = page.Total
		if query.Page == 1 && len(page.Items) > 0 {
			query.BeforeID = page.Items[0].ID
			query.BeforeTime = page.Items[0].Time
		}
		for _, entry := range page.Items {
			entry.RequestBody = ""
			entry.ResponseBody = ""
			entry.ClientIP = ""
			entry.ClientHost = ""
			entry.ErrorDescription = ""
			items = append(items, entry)
		}
		if len(page.Items) < 500 || len(items) >= total {
			break
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "from": from, "to": to, "retentionDays": s.RequestLogRetentionDays()})
}
