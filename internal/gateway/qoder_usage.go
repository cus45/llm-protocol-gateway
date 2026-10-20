package gateway

// Qoder's account quota.
//
// Qoder reports the plan and its credit pools on the account host, asked with
// the *device* token — the one the browser sign-in leaves behind, not the job
// token that signs inference. A connection made by pasting a personal access
// token has no device token at all, so it has no quota to read; that says so
// rather than showing an empty panel.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// qoderQuotaWindow is one pool of credits the card shows.
type qoderQuotaWindow struct {
	Name     string  `json:"name"`
	Used     float64 `json:"used"`
	Amount   float64 `json:"amount,omitempty"`
	Limit    float64 `json:"limit,omitempty"`
	Unit     string  `json:"unit,omitempty"`
	ResetsAt string  `json:"resetsAt,omitempty"`
}

type qoderQuota struct {
	Plan      string             `json:"plan,omitempty"`
	Windows   []qoderQuotaWindow `json:"windows"`
	Until     string             `json:"until,omitempty"`
	Error     string             `json:"error,omitempty"`
	FetchedAt string             `json:"fetchedAt,omitempty"`
}

// qoderUsageEnvelope is Qoder's reply: {displayMode, qoderUsage}. Enterprise
// accounts answer with a display mode and no personal quota at all.
type qoderUsageEnvelope struct {
	DisplayMode string          `json:"displayMode"`
	QoderUsage  json.RawMessage `json:"qoderUsage"`
}

// qoderUsageBucket is one credit pool. Qoder names the fields in both cases and
// sometimes caps instead of totalling.
type qoderUsageBucket struct {
	Name      string   `json:"name"`
	Unit      string   `json:"unit"`
	Total     *float64 `json:"total"`
	Cap       *float64 `json:"cap"`
	Used      *float64 `json:"used"`
	Remaining *float64 `json:"remaining"`
}

func (b qoderUsageBucket) total() float64 {
	if b.Total != nil {
		return *b.Total
	}
	if b.Cap != nil {
		return *b.Cap
	}
	return 0
}

func (b qoderUsageBucket) used() (float64, bool) {
	if b.Used != nil {
		return *b.Used, true
	}
	if b.Remaining != nil {
		return b.total() - *b.Remaining, true
	}
	return 0, false
}

// qoderFetchUsage reads the account envelope with a device token.
func qoderFetchUsage(ctx context.Context, site qoderSite, deviceToken string) (qoderUsageEnvelope, error) {
	if strings.TrimSpace(deviceToken) == "" {
		return qoderUsageEnvelope{}, errors.New("Qoder 额度查询缺少设备令牌")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, site.OpenAPI+qoderUsagePath, nil)
	if err != nil {
		return qoderUsageEnvelope{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+deviceToken)
	request.Header.Set("Cosy-ClientType", "10")
	request.Header.Set("User-Agent", "Qoder")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return qoderUsageEnvelope{}, err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return qoderUsageEnvelope{}, &qoderError{Message: qoderErrorMessage(body, response.StatusCode), Status: response.StatusCode}
	}
	var envelope qoderUsageEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return envelope, fmt.Errorf("解析 Qoder 额度失败: %w", err)
	}
	if envelope.DisplayMode != "qoder" && envelope.DisplayMode != "enterprise" {
		return envelope, errors.New("Qoder 额度：未知的账号形态")
	}
	return envelope, nil
}

// qoderParseQuota turns the envelope into what the card shows.
func qoderParseQuota(envelope qoderUsageEnvelope) qoderQuota {
	quota := qoderQuota{Windows: []qoderQuotaWindow{}, FetchedAt: time.Now().UTC().Format(time.RFC3339)}
	if envelope.DisplayMode == "enterprise" {
		quota.Plan = "Enterprise"
		return quota
	}
	if len(envelope.QoderUsage) == 0 || string(envelope.QoderUsage) == "null" {
		quota.Error = "Qoder 没有返回额度数据"
		return quota
	}
	var usage struct {
		UserType     string             `json:"userType"`
		UserTypeSnk  string             `json:"user_type"`
		ExpiresAt    json.RawMessage    `json:"expiresAt"`
		ExpiresAtSnk json.RawMessage    `json:"expires_at"`
		UserQuota    *qoderUsageBucket  `json:"userQuota"`
		UserQuotaSnk *qoderUsageBucket  `json:"user_quota"`
		AddOn        *qoderUsageBucket  `json:"addOnQuota"`
		AddOnSnk     *qoderUsageBucket  `json:"add_on_quota"`
		Org          *qoderUsageBucket  `json:"orgResourcePackage"`
		OrgSnk       *qoderUsageBucket  `json:"org_resource_package"`
		Dedicated    []qoderUsageBucket `json:"dedicatedResourcePackages"`
	}
	if json.Unmarshal(envelope.QoderUsage, &usage) != nil {
		quota.Error = "Qoder 的额度数据读不出来"
		return quota
	}
	quota.Plan = firstNonEmpty(usage.UserType, usage.UserTypeSnk)
	quota.Until = qoderWhen(firstNonRaw(usage.ExpiresAt, usage.ExpiresAtSnk))
	add := func(bucket *qoderUsageBucket, fallback string) {
		if bucket == nil {
			return
		}
		total := bucket.total()
		used, ok := bucket.used()
		if total <= 0 || !ok || used < 0 {
			return
		}
		percent := 100 * used / total
		if percent > 100 {
			percent = 100
		}
		unit := firstNonEmpty(bucket.Unit, "credits")
		quota.Windows = append(quota.Windows, qoderQuotaWindow{
			Name:     firstNonEmpty(bucket.Name, fallback),
			Used:     percent,
			Amount:   used,
			Limit:    total,
			Unit:     unit,
			ResetsAt: qoderWhenString(bucket),
		})
	}
	add(usage.UserQuota, "Credits")
	add(usage.UserQuotaSnk, "Credits")
	add(usage.AddOn, "Add-on credits")
	add(usage.AddOnSnk, "Add-on credits")
	add(usage.Org, "Shared credits")
	add(usage.OrgSnk, "Shared credits")
	for _, bucket := range usage.Dedicated {
		kept := bucket
		add(&kept, "Dedicated credits")
	}
	return quota
}

// qoderWhenString reads a reset time when a pool carries one; Qoder names it
// a few different ways across its replies.
func qoderWhenString(bucket *qoderUsageBucket) string {
	var shaped struct {
		ResetsAt json.RawMessage `json:"resetsAt"`
		ResetAt  json.RawMessage `json:"reset_at"`
	}
	raw, _ := json.Marshal(bucket)
	if json.Unmarshal(raw, &shaped) != nil {
		return ""
	}
	return qoderWhen(firstNonRaw(shaped.ResetsAt, shaped.ResetAt))
}

func firstNonRaw(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) > 0 && string(value) != "null" {
			return value
		}
	}
	return nil
}

// qoderWhen is a time Qoder gave: an RFC3339 string, or seconds, or
// milliseconds — it has used all three.
func qoderWhen(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if parsed, err := time.Parse(time.RFC3339, text); err == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
		if number, err := strconv.ParseFloat(text, 64); err == nil {
			return qoderWhenNumber(number)
		}
		return ""
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil && number > 0 {
		return qoderWhenNumber(number)
	}
	return ""
}

func qoderWhenNumber(value float64) string {
	if value < 1e12 {
		value *= 1000
	}
	return time.UnixMilli(int64(value)).UTC().Format(time.RFC3339)
}

// qoderQuotaFor reads the account's quota, rotating a refused device token once:
// Qoder records no lifetime for it, so a token it has stopped accepting is
// renewed the way its own client does and asked again.
func (s *Server) qoderQuotaFor(ctx context.Context, provider domain.Provider) (domain.Provider, qoderQuota) {
	quota := qoderQuota{Windows: []qoderQuotaWindow{}}
	if provider.QoderPAT == nil {
		quota.Error = "Qoder 尚未登录"
		return provider, quota
	}
	site := qoderSiteOf(provider)
	credential := *provider.QoderPAT
	token := strings.TrimSpace(credential.DeviceToken)
	if token == "" && credential.DeviceChat {
		// On a device-chat account the chat token and the account token are the
		// one pair.
		token = strings.TrimSpace(credential.AccessToken)
	}
	if token == "" {
		quota.Error = "这条连接是个人访问令牌换来的，Qoder 的额度接口不认它；要用额度面板请改用浏览器登录"
		return provider, quota
	}
	envelope, err := qoderFetchUsage(ctx, site, token)
	if err != nil && qoderErrorIsClient(err) && strings.TrimSpace(credential.DeviceRefresh) != "" {
		refreshed, refreshErr := rotateQoderDeviceToken(ctx, site, credential)
		if refreshErr == nil {
			credential = refreshed
			if token = strings.TrimSpace(credential.DeviceToken); token != "" {
				envelope, err = qoderFetchUsage(ctx, site, token)
			}
		}
	}
	if err != nil {
		quota.Error = err.Error()
		return provider, quota
	}
	parsed := qoderParseQuota(envelope)
	// The rotation spent a refresh token, so it has to be kept even when the
	// quota itself says nothing new.
	if credential.DeviceToken != provider.QoderPAT.DeviceToken {
		if updated, updateErr := s.router.SetProviderQoderPAT(provider.ID, credential); updateErr == nil {
			_ = s.persistProviderOAuth(updated)
			provider = updated
		}
	}
	return provider, parsed
}

// rotateQoderDeviceToken renews the device token pair and hands back the
// account with both halves moved, as Qoder's CLI keeps them.
func rotateQoderDeviceToken(ctx context.Context, site qoderSite, credential domain.QoderCredential) (domain.QoderCredential, error) {
	refresh := strings.TrimSpace(credential.DeviceRefresh)
	if refresh == "" {
		return credential, errors.New("Qoder 设备令牌无法续期，请重新登录")
	}
	var token qoderDeviceToken
	if err := qoderAccountCall(ctx, http.MethodPost, site.OpenAPI+qoderDeviceRefreshPath, "",
		map[string]string{"refresh_token": refresh}, &token); err != nil {
		return credential, err
	}
	access := token.accessToken()
	if access == "" || strings.TrimSpace(token.RefreshToken) == "" {
		return credential, errors.New("Qoder 返回的设备令牌对不完整")
	}
	renewed := credential
	renewed.DeviceToken = access
	renewed.DeviceRefresh = strings.TrimSpace(token.RefreshToken)
	if renewed.DeviceChat {
		// The chat token is the device token on this shape of account.
		renewed.AccessToken = access
		renewed.RefreshToken = renewed.DeviceRefresh
		renewed.ExpiresAt = qoderFormatExpiry(token.expiry(time.Now()))
	}
	return renewed, nil
}
