package monitoring

import "time"

type Severity string

const (
	SeverityP0   Severity = "p0"
	SeverityP1   Severity = "p1"
	SeverityP2   Severity = "p2"
	SeverityInfo Severity = "info"
)

func (s Severity) Rank() int {
	switch s {
	case SeverityP0:
		return 3
	case SeverityP1:
		return 2
	case SeverityP2:
		return 1
	default:
		return 0
	}
}

func (s Severity) Meets(min Severity) bool { return s.Rank() >= min.Rank() }

func SeverityFromLegacy(raw string) Severity {
	switch raw {
	case "error", "critical":
		return SeverityP0
	case "warn":
		return SeverityP1
	default:
		return SeverityP1
	}
}

const (
	EventKindAlert   = "alert"
	EventKindRenewal = "renewal"
	EventKindProbe   = "probe"
	EventKindCheckin = "checkin"
	EventKindQuota   = "quota"
	EventKindSystem  = "system"

	StatusFiring   = "firing"
	StatusResolved = "resolved"
	StatusInfo     = "info"

	DispositionOpen         = "open"
	DispositionAcknowledged = "acknowledged"
	DispositionIgnored      = "ignored"

	ResolutionRecovered       = "recovered"
	ResolutionDataUnavailable = "data_unavailable"
	ResolutionLegacy          = "legacy"

	PushPending    = "pending"
	PushSent       = "sent"
	PushPartial    = "partial"
	PushFailed     = "failed"
	PushSuppressed = "suppressed"
	PushSkipped    = "skipped"

	TargetProvider = "provider"
	TargetAPIKey   = "api_key"
	TargetModel    = "model"
	TargetGateway  = "gateway"
)

type Event struct {
	ID               int64          `json:"id"`
	Time             time.Time      `json:"time"`
	Kind             string         `json:"kind"`
	Severity         Severity       `json:"severity"`
	RuleKey          string         `json:"ruleKey,omitempty"`
	TargetType       string         `json:"targetType"`
	TargetID         string         `json:"targetId,omitempty"`
	TargetName       string         `json:"targetName,omitempty"`
	Title            string         `json:"title"`
	Detail           map[string]any `json:"detail"`
	Fingerprint      string         `json:"fingerprint,omitempty"`
	Status           string         `json:"status"`
	Count            int            `json:"count"`
	FirstSeen        time.Time      `json:"firstSeen"`
	LastSeen         time.Time      `json:"lastSeen"`
	ResolvedAt       time.Time      `json:"resolvedAt,omitempty"`
	ResolutionReason string         `json:"resolutionReason,omitempty"`
	PushStatus       string         `json:"pushStatus,omitempty"`
	PushError        string         `json:"pushError,omitempty"`
}

type AlertState struct {
	ID               int64          `json:"id"`
	Fingerprint      string         `json:"fingerprint"`
	RuleKey          string         `json:"ruleKey"`
	Severity         Severity       `json:"severity"`
	TargetType       string         `json:"targetType"`
	TargetID         string         `json:"targetId"`
	TargetName       string         `json:"targetName"`
	Status           string         `json:"status"`
	Disposition      string         `json:"disposition"`
	Count            int            `json:"count"`
	FirstSeen        time.Time      `json:"firstSeen"`
	LastSeen         time.Time      `json:"lastSeen"`
	LastNotifiedAt   time.Time      `json:"lastNotifiedAt,omitempty"`
	AcknowledgedAt   time.Time      `json:"acknowledgedAt,omitempty"`
	IgnoredAt        time.Time      `json:"ignoredAt,omitempty"`
	SuppressedUntil  time.Time      `json:"suppressedUntil,omitempty"`
	UnavailableSince time.Time      `json:"unavailableSince,omitempty"`
	ResolutionReason string         `json:"resolutionReason,omitempty"`
	Detail           map[string]any `json:"detail"`
}

// AlertView 是告警列表/详情返回给前端的形状：在状态行上补最近事件与推送状态。
type AlertView struct {
	AlertState
	LatestEventID   int64   `json:"latestEventId,omitempty"`
	PushStatus      string  `json:"pushStatus,omitempty"`
	PushError       string  `json:"pushError,omitempty"`
	RelatedEventIDs []int64 `json:"relatedEventIds"`
}

// RequestSummary 是 overview 的 24h 汇总，过滤口径与 WindowStatsByProvider 相同。
type RequestSummary struct {
	Total        int   `json:"total"`
	Success      int   `json:"success"`
	P95LatencyMs int64 `json:"p95LatencyMs"`
	CostMicros   int64 `json:"costMicros"`
}

type EventPush struct {
	EventID   int64
	ChannelID string
	Status    string
	Attempts  int
	LastError string
	UpdatedAt time.Time
}

// TimelineEntry 追加到监听周期最新事件的 detail_json.timeline。
// Action 为 acknowledged / ignored / resolved 等，Time 为 UTC。
type TimelineEntry struct {
	Action string    `json:"action"`
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor,omitempty"`
}

type Channel struct {
	ID          string        `json:"id"`
	Type        string        `json:"type"`
	Name        string        `json:"name"`
	Config      ChannelConfig `json:"config"`
	MinSeverity Severity      `json:"minSeverity"`
	Enabled     bool          `json:"enabled"`
}

type ChannelConfig struct {
	Webhook  string `json:"webhook,omitempty"`
	Secret   string `json:"secret,omitempty"`
	BotToken string `json:"botToken,omitempty"`
	ChatID   string `json:"chatId,omitempty"`
}

type Notification struct {
	Fingerprint string    `json:"fingerprint"`
	Severity    Severity  `json:"severity"`
	Title       string    `json:"title"`
	Target      string    `json:"target"`
	Markdown    string    `json:"markdown"`
	URL         string    `json:"url,omitempty"`
	At          time.Time `json:"at"`
	Kind        string    `json:"kind"` // firing | resolved | digest | test
}

type WindowStats struct {
	Total       int   `json:"total"`
	Success     int   `json:"success"`
	Auth401403  int   `json:"auth401403"`
	RateLimited int   `json:"rateLimited"`
	ServerError int   `json:"serverError"`
	Pseudo200   int   `json:"pseudo200"`
	Empty       int   `json:"empty"`
	TTFTp50     int64 `json:"ttftP50"`
	TTFTp95     int64 `json:"ttftP95"`
	LatencyP95  int64 `json:"latencyP95"`
	CostMicros  int64 `json:"costMicros"`
}

func (w WindowStats) SuccessRatio() float64 {
	if w.Total == 0 {
		return 0
	}
	return float64(w.Success) / float64(w.Total)
}

func (w WindowStats) ErrorRatio() float64 {
	if w.Total == 0 {
		return 0
	}
	return float64(w.ServerError) / float64(w.Total)
}

func (w WindowStats) AuthRatio() float64 {
	if w.Total == 0 {
		return 0
	}
	return float64(w.Auth401403) / float64(w.Total)
}

func (w WindowStats) RateLimitRatio() float64 {
	if w.Total == 0 {
		return 0
	}
	return float64(w.RateLimited) / float64(w.Total)
}

func (w WindowStats) EmptyRatio() float64 {
	if w.Total == 0 {
		return 0
	}
	return float64(w.Empty) / float64(w.Total)
}

type Capabilities struct {
	Quota   bool `json:"quota"`
	Checkin bool `json:"checkin"`
	Models  bool `json:"models"`
	OAuth   bool `json:"oauth"`
}

type QuotaSnapshot struct {
	Source     string    `json:"source"`
	Remaining  float64   `json:"remaining"`
	Limit      float64   `json:"limit"`
	Ratio      float64   `json:"ratio"`
	ExpiresAt  time.Time `json:"expiresAt,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
	Stale      bool      `json:"stale"`
	Error      string    `json:"error,omitempty"`
}

func (q QuotaSnapshot) Available() bool { return q.Limit > 0 && q.Ratio >= 0 }

func (q QuotaSnapshot) IsStale(now time.Time, maxAge time.Duration) bool {
	return q.ObservedAt.IsZero() || now.Sub(q.ObservedAt) > maxAge
}

type RenewalOutcome struct {
	Known       bool
	LastAt      time.Time
	LastOK      bool
	Consecutive int
	LastError   string
}

// RenewalRecord 是一次续期尝试的埋点输入（Task 11 在调用点构造）。
// Err 只用于本地日志与 classifyRenewalError，绝不进入事件 detail_json。
type RenewalRecord struct {
	ProviderID string
	Vendor     string
	StartedAt  time.Time
	Duration   time.Duration
	Err        error
	ExpiresAt  time.Time
}

type ProviderObservation struct {
	ID              string
	Name            string
	Vendor          string
	AuthType        string
	ExpiresAt       time.Time
	HealthStatus    string
	Capabilities    Capabilities
	Quota           *QuotaSnapshot
	Renewal         RenewalOutcome
	ModelFailures   int
	BaselineTTFTP95 int64
}

type KeyObservation struct {
	ID              string
	Name            string
	QuotaWindow     string
	TokenLimit      int64
	TokenUsed       int64
	CostLimitMicros int64
	CostUsedMicros  int64
	MultiIPs        []string
	MultiIPCount    int
	ConcurrentIPs   int
	ConcurrentAt    time.Time
}

type Snapshot struct {
	Now                  time.Time
	Providers            []ProviderObservation
	Keys                 []KeyObservation
	Windows              map[string]map[int]WindowStats
	CollectorLastSuccess time.Time
	DataErrors           []string
}

type EventQuery struct {
	Kind       string
	Severity   string
	TargetType string
	TargetID   string
	Status     string
	From       time.Time
	To         time.Time
	Page       int
	PageSize   int
}

type EventPage struct {
	Items    []Event `json:"items"`
	Total    int     `json:"total"`
	Page     int     `json:"page"`
	PageSize int     `json:"pageSize"`
}

type AlertQuery struct {
	Status      string
	Disposition string
	Severity    string
	Rule        string
	TargetType  string
	TargetID    string
	From        time.Time
	To          time.Time
	Page        int
	PageSize    int
}

type AlertPage struct {
	Items    []AlertView `json:"items"`
	Total    int         `json:"total"`
	Page     int         `json:"page"`
	PageSize int         `json:"pageSize"`
}

// MetricSample maps one provider_metric_samples row. Nil means "unknown",
// which is different from a zero quota and must never be rendered as 0.
type MetricSample struct {
	ProviderID       string
	Hour             string
	QuotaRatio       *float64
	Credits          *float64
	CreditValidUntil string
	AuthExpiresAt    string
	HealthStatus     string
	SourceError      string
	ObservedAt       time.Time
}
