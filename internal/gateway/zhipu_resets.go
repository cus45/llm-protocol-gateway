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
)

// ZhipuResetCredits is how many five-hour and weekly resets a GLM Coding Plan
// still holds, as bigmodel.cn's usage page lists them (magpie #1191).
// Read-only: a reset is spent on the vendor's own page, never from the gateway.
type ZhipuResetCredits struct {
	Count    int    `json:"count"`
	FiveHour int    `json:"fiveHour,omitempty"`
	Weekly   int    `json:"weekly,omitempty"`
	Earliest string `json:"earliest,omitempty"`
}

const zhipuResetPath = "/api/biz/customer-package-reset/list"

type zhipuResetWire struct {
	Available bool `json:"available"`
	Expire    any  `json:"expireTime"`
}

// fetchZhipuResets reads the plan's reset list best effort: a nil list with a
// nil error means the account holds none, so the card shows nothing rather
// than a wrong 0.
func fetchZhipuResets(ctx context.Context, base, apiKey, organizationID, projectID string, team bool) (*ZhipuResetCredits, error) {
	target := "PERSONAL"
	if team {
		target = "TEAM"
	}
	u := strings.TrimRight(base, "/") + zhipuResetPath + "?targetType=" + target
	fiveHour, week, err := readZhipuResetList(ctx, u, apiKey, organizationID, projectID)
	if err != nil {
		return nil, err
	}
	return buildZhipuResetCredits(fiveHour, week), nil
}

func readZhipuResetList(ctx context.Context, url, apiKey, organizationID, projectID string) (fiveHour, week []zhipuResetWire, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", apiKey) // Zhipu does NOT use a "Bearer " prefix.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en")
	if organizationID != "" {
		req.Header.Set("bigmodel-organization", organizationID)
	}
	if projectID != "" {
		req.Header.Set("bigmodel-project", projectID)
	}
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("reset list: HTTP %d", resp.StatusCode)
	}
	payload := body
	var envelope struct {
		Code json.RawMessage `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		if c := strings.Trim(string(envelope.Code), `"`); c != "" && c != "null" && c != "0" && c != "200" {
			return nil, nil, fmt.Errorf("reset list: %s", envelope.Msg)
		}
		if len(envelope.Data) > 0 && string(envelope.Data) != "null" {
			payload = envelope.Data
		}
	}
	var data struct {
		FiveHour []zhipuResetWire `json:"fiveHourResets"`
		Week     []zhipuResetWire `json:"weekResets"`
	}
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil, nil, err
	}
	return data.FiveHour, data.Week, nil
}

func buildZhipuResetCredits(fiveHour, week []zhipuResetWire) *ZhipuResetCredits {
	out := &ZhipuResetCredits{}
	var earliest time.Time
	count := func(items []zhipuResetWire) int {
		n := 0
		for _, item := range items {
			if !item.Available {
				continue
			}
			n++
			if t := zhipuResetExpiry(item.Expire); !t.IsZero() && (earliest.IsZero() || t.Before(earliest)) {
				earliest = t
			}
		}
		return n
	}
	out.FiveHour = count(fiveHour)
	out.Weekly = count(week)
	out.Count = out.FiveHour + out.Weekly
	if !earliest.IsZero() {
		out.Earliest = earliest.UTC().Format(time.RFC3339)
	}
	if out.Count == 0 {
		return nil
	}
	return out
}

// zhipuResetExpiry reads expireTime, which bigmodel sends as epoch
// milliseconds or as a Beijing-time date string ("2026-11-02 23:59:59").
func zhipuResetExpiry(v any) time.Time {
	switch x := v.(type) {
	case float64:
		return zhipuEpochTime(x)
	case string:
		x = strings.TrimSpace(x)
		if x == "" {
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339, x); err == nil {
			return t
		}
		cst := time.FixedZone("CST", 8*3600)
		for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02"} {
			if t, err := time.ParseInLocation(layout, x, cst); err == nil {
				return t
			}
		}
		if n, err := strconv.ParseFloat(x, 64); err == nil {
			return zhipuEpochTime(n)
		}
	}
	return time.Time{}
}

func zhipuEpochTime(n float64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	if n > 1e12 {
		return time.UnixMilli(int64(n))
	}
	return time.Unix(int64(n), 0)
}
