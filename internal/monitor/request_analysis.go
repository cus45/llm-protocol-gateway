package monitor

// RequestAnalysis holds numeric context metadata independently of body logging.
// SessionID is a digest scoped to the gateway key, never the client's raw ID.
type RequestAnalysis struct {
	SessionID    string `json:"sessionId,omitempty"`
	Stream       bool   `json:"stream"`
	Captured     bool   `json:"captured"`
	Messages     int    `json:"messages"`
	Tools        int    `json:"tools"`
	ToolResults  int    `json:"toolResults"`
	SystemBytes  int    `json:"systemBytes"`
	MessageBytes int    `json:"messageBytes"`
	ToolBytes    int    `json:"toolBytes"`
	// Scheduling trace: what the gateway decided and what it did about it.
	// Requests logged before this existed have none, and the console says so
	// rather than showing an empty chain as if nothing was tried.
	ScheduleMode  string           `json:"scheduleMode,omitempty"`
	SessionPinned string           `json:"sessionPinned,omitempty"`
	Candidates    []string         `json:"candidates,omitempty"`
	Attempts      []RequestAttempt `json:"attempts,omitempty"`
}

// RequestAttempt is one provider the request touched, in the order it touched
// them. Deferred means the answer was thrown away in favour of the next
// candidate, so the client never saw that status code.
type RequestAttempt struct {
	ProviderID string `json:"providerId"`
	Model      string `json:"model,omitempty"`
	Status     int    `json:"status,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Error      string `json:"error,omitempty"`
	Ms         int64  `json:"ms,omitempty"`
	Deferred   bool   `json:"deferred,omitempty"`
	Skipped    bool   `json:"skipped,omitempty"`
}
