package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// chatgptFlexibleFloat accepts both a JSON number and a numeric string,
// since the ChatGPT backend has returned credits.balance as either.
type chatgptFlexibleFloat float64

func (f *chatgptFlexibleFloat) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*f = 0
		return nil
	}
	var n float64
	if err := json.Unmarshal(b, &n); err == nil {
		*f = chatgptFlexibleFloat(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("invalid number value %q", string(b))
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return fmt.Errorf("invalid number value %q: %w", string(b), err)
	}
	*f = chatgptFlexibleFloat(v)
	return nil
}

const (
	chatgptOAuthUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
	chatgptOAuthResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
)

// ChatGPTResetCredit is one rate-limit reset card granted by OpenAI
// (sub2api-compatible: id / expires_at are surfaced, never redeemed here).
type ChatGPTResetCredit struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	Status    string `json:"status,omitempty"`
	GrantedAt string `json:"grantedAt,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// ChatGPTResetCredits summarizes the account's unused reset cards.
type ChatGPTResetCredits struct {
	AvailableCount int                  `json:"availableCount"`
	Applicable     int                  `json:"applicable,omitempty"`
	Credits        []ChatGPTResetCredit `json:"credits,omitempty"`
}

type chatgptResetCreditsResponse struct {
	Credits []struct {
		ID         string `json:"id"`
		CreditID   string `json:"credit_id"`
		ResetType  string `json:"reset_type"`
		Status     string `json:"status"`
		Title      string `json:"title"`
		GrantedAt  string `json:"granted_at"`
		ExpiresAt  string `json:"expires_at"`
		RedeemedAt string `json:"redeemed_at"`
	} `json:"credits"`
	AvailableCount int `json:"available_count"`
}

// ChatGPTOAuthUsageBucket is one display row for ChatGPT/Codex quota.
type ChatGPTOAuthUsageBucket struct {
	Label       string  `json:"label"`
	Utilization float64 `json:"utilization"` // 0-100 used percent
	Detail      string  `json:"detail,omitempty"`
	ResetsAt    string  `json:"resetsAt,omitempty"` // RFC3339 when known
}

// ChatGPTOAuthUsageReport is the client-safe usage snapshot for chatgpt_oauth.
type ChatGPTOAuthUsageReport struct {
	Available    bool                      `json:"available"`
	Error        string                    `json:"error,omitempty"`
	FetchedAt    string                    `json:"fetchedAt,omitempty"`
	PlanName     string                    `json:"planName,omitempty"`
	Message      string                    `json:"message,omitempty"`
	Buckets      []ChatGPTOAuthUsageBucket `json:"buckets,omitempty"`
	ResetCredits *ChatGPTResetCredits      `json:"resetCredits,omitempty"`
}

type chatgptWhamUsageResponse struct {
	Email     string `json:"email"`
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		Allowed       bool `json:"allowed"`
		LimitReached  bool `json:"limit_reached"`
		PrimaryWindow *struct {
			UsedPercent        float64 `json:"used_percent"`
			LimitWindowSeconds int64   `json:"limit_window_seconds"`
			ResetAfterSeconds  int64   `json:"reset_after_seconds"`
			ResetAt            int64   `json:"reset_at"`
		} `json:"primary_window"`
		SecondaryWindow *struct {
			UsedPercent        float64 `json:"used_percent"`
			LimitWindowSeconds int64   `json:"limit_window_seconds"`
			ResetAfterSeconds  int64   `json:"reset_after_seconds"`
			ResetAt            int64   `json:"reset_at"`
		} `json:"secondary_window"`
	} `json:"rate_limit"`
	Credits *struct {
		HasCredits          bool                  `json:"has_credits"`
		Unlimited           bool                  `json:"unlimited"`
		OverageLimitReached bool                  `json:"overage_limit_reached"`
		Balance             *chatgptFlexibleFloat `json:"balance"`
	} `json:"credits"`
}

func formatChatGPTWindowLabel(seconds int64, fallback string) string {
	switch {
	case seconds <= 0:
		return fallback
	case seconds%(24*3600) == 0:
		days := seconds / (24 * 3600)
		if days == 1 {
			return "1 天额度"
		}
		return fmt.Sprintf("%d 天额度", days)
	case seconds%3600 == 0:
		hours := seconds / 3600
		if hours == 1 {
			return "1 小时额度"
		}
		return fmt.Sprintf("%d 小时额度", hours)
	default:
		return fallback
	}
}

func formatChatGPTWindowDetail(seconds int64) string {
	switch {
	case seconds <= 0:
		return ""
	case seconds%(24*3600) == 0:
		days := seconds / (24 * 3600)
		if days == 1 {
			return "统计窗口：1 天"
		}
		return fmt.Sprintf("统计窗口：%d 天", days)
	case seconds%3600 == 0:
		hours := seconds / 3600
		if hours == 1 {
			return "统计窗口：1 小时"
		}
		return fmt.Sprintf("统计窗口：%d 小时", hours)
	case seconds%60 == 0:
		return fmt.Sprintf("统计窗口：%d 分钟", seconds/60)
	default:
		return fmt.Sprintf("统计窗口：%d 秒", seconds)
	}
}

func chatgptResetAtRFC3339(resetAtUnix, resetAfterSeconds int64) string {
	if resetAtUnix > 0 {
		return time.Unix(resetAtUnix, 0).UTC().Format(time.RFC3339)
	}
	if resetAfterSeconds > 0 {
		return time.Now().UTC().Add(time.Duration(resetAfterSeconds) * time.Second).Format(time.RFC3339)
	}
	return ""
}

func buildChatGPTOAuthUsageReport(raw chatgptWhamUsageResponse) ChatGPTOAuthUsageReport {
	plan := strings.TrimSpace(raw.PlanType)
	if plan == "" {
		plan = "unknown"
	}
	report := ChatGPTOAuthUsageReport{
		Available: true,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		PlanName:  plan,
	}
	if raw.RateLimit == nil {
		report.Message = "未返回 rate_limit"
		return report
	}
	if raw.RateLimit.LimitReached || !raw.RateLimit.Allowed {
		report.Message = "额度已用尽或暂时不可用"
	} else {
		report.Message = "可用"
	}

	buckets := make([]ChatGPTOAuthUsageBucket, 0, 3)
	if w := raw.RateLimit.PrimaryWindow; w != nil {
		buckets = append(buckets, ChatGPTOAuthUsageBucket{
			Label:       formatChatGPTWindowLabel(w.LimitWindowSeconds, "主窗口额度"),
			Utilization: clampPercent(w.UsedPercent),
			Detail:      formatChatGPTWindowDetail(w.LimitWindowSeconds),
			ResetsAt:    chatgptResetAtRFC3339(w.ResetAt, w.ResetAfterSeconds),
		})
	}
	if w := raw.RateLimit.SecondaryWindow; w != nil {
		buckets = append(buckets, ChatGPTOAuthUsageBucket{
			Label:       formatChatGPTWindowLabel(w.LimitWindowSeconds, "次窗口额度"),
			Utilization: clampPercent(w.UsedPercent),
			Detail:      formatChatGPTWindowDetail(w.LimitWindowSeconds),
			ResetsAt:    chatgptResetAtRFC3339(w.ResetAt, w.ResetAfterSeconds),
		})
	}
	if raw.Credits != nil {
		detail := "无额外 credits"
		if raw.Credits.Unlimited {
			detail = "credits 无限"
		} else if raw.Credits.HasCredits {
			if raw.Credits.Balance != nil {
				detail = fmt.Sprintf("credits 余额 %.2f", float64(*raw.Credits.Balance))
			} else {
				detail = "有 credits"
			}
		}
		buckets = append(buckets, ChatGPTOAuthUsageBucket{
			Label:       "Credits",
			Utilization: 0,
			Detail:      detail,
		})
	}
	report.Buckets = buckets
	return report
}

func fetchChatGPTOAuthResetCredits(ctx context.Context, provider domain.Provider) *ChatGPTResetCredits {
	return fetchChatGPTOAuthResetCreditsAt(ctx, provider, chatgptOAuthResetCreditsURL)
}

// fetchChatGPTOAuthResetCreditsAt queries the reset-card endpoint. Failures are
// non-fatal: the usage panel simply omits the section.
func fetchChatGPTOAuthResetCreditsAt(ctx context.Context, provider domain.Provider, url string) *ChatGPTResetCredits {
	if provider.ChatGPTOAuth == nil || strings.TrimSpace(provider.ChatGPTOAuth.AccessToken) == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	applyChatGPTCodexHeaders(req, provider)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	var raw chatgptResetCreditsResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	out := &ChatGPTResetCredits{AvailableCount: raw.AvailableCount}
	for _, credit := range raw.Credits {
		id := strings.TrimSpace(credit.ID)
		if id == "" {
			id = strings.TrimSpace(credit.CreditID)
		}
		status := strings.TrimSpace(credit.Status)
		if status == "" {
			status = "available"
		}
		if status != "available" {
			continue
		}
		if credit.ResetType != "" && credit.ResetType != "codex_rate_limits" {
			continue
		}
		out.Credits = append(out.Credits, ChatGPTResetCredit{
			ID:        id,
			Title:     strings.TrimSpace(credit.Title),
			Status:    status,
			GrantedAt: strings.TrimSpace(credit.GrantedAt),
			ExpiresAt: strings.TrimSpace(credit.ExpiresAt),
		})
	}
	if out.AvailableCount == 0 {
		out.AvailableCount = len(out.Credits)
	}
	return out
}

func fetchChatGPTOAuthUsage(ctx context.Context, provider domain.Provider) (ChatGPTOAuthUsageReport, error) {
	if provider.ChatGPTOAuth == nil || strings.TrimSpace(provider.ChatGPTOAuth.AccessToken) == "" {
		return ChatGPTOAuthUsageReport{Available: false, Error: "missing oauth access token"}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, chatgptOAuthUsageURL, nil)
	if err != nil {
		return ChatGPTOAuthUsageReport{}, err
	}
	applyChatGPTCodexHeaders(req, provider)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ChatGPTOAuthUsageReport{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return ChatGPTOAuthUsageReport{Available: false, Error: msg}, nil
	}
	var raw chatgptWhamUsageResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return ChatGPTOAuthUsageReport{}, fmt.Errorf("failed to parse chatgpt oauth usage: %w", err)
	}
	report := buildChatGPTOAuthUsageReport(raw)
	if reset := fetchChatGPTOAuthResetCredits(ctx, provider); reset != nil {
		report.ResetCredits = reset
	}
	return report, nil
}
