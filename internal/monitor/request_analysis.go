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
}
