package monitoring

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ChannelAdapter interface {
	Send(ctx context.Context, ch Channel, n Notification) error
}

// AdapterFor returns the adapter for a channel type. Unknown types are
// rejected instead of silently dropping notifications.
func AdapterFor(channelType string) (ChannelAdapter, bool) {
	switch channelType {
	case "feishu":
		return feishuAdapter{}, true
	case "dingtalk":
		return dingtalkAdapter{}, true
	case "wecom":
		return wecomAdapter{}, true
	case "telegram":
		return telegramAdapter{}, true
	default:
		return nil, false
	}
}

var channelHTTPClient = &http.Client{Timeout: 15 * time.Second}

// postJSON only moves bytes; each adapter decides what a non-2xx status means
// and scrubs the body before it reaches an error message.
func postJSON(ctx context.Context, url string, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := channelHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	limited, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, limited, nil
}

func channelError(channelType string, ch Channel, status int, body []byte) error {
	message := truncate(strings.TrimSpace(string(body)), 500)
	return fmt.Errorf("%s HTTP %d: %s", channelType, status, scrubChannelSecret(message, ch))
}

// channelTransportError scrubs client errors too: net/http embeds the request
// URL in them, and Telegram puts the bot token in the URL path.
func channelTransportError(channelType string, ch Channel, err error) error {
	return fmt.Errorf("%s: %s", channelType, scrubChannelSecret(err.Error(), ch))
}

// channelResponseError catches HTTP 200 responses that still signal failure,
// which all four vendors use.
func channelResponseError(channelType string, ch Channel, body []byte) error {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	switch channelType {
	case "feishu":
		raw, ok := payload["code"]
		if !ok {
			raw, ok = payload["StatusCode"]
		}
		if !ok {
			return nil
		}
		if code := intValue(raw); code != 0 {
			return fmt.Errorf("feishu code %d: %s", code, scrubChannelSecret(truncate(string(body), 500), ch))
		}
	case "dingtalk", "wecom":
		raw, ok := payload["errcode"]
		if !ok {
			return nil
		}
		if code := intValue(raw); code != 0 {
			return fmt.Errorf("%s errcode %d: %s", channelType, code, scrubChannelSecret(truncate(string(body), 500), ch))
		}
	case "telegram":
		ok, _ := payload["ok"].(bool)
		if ok {
			return nil
		}
		description, _ := payload["description"].(string)
		return fmt.Errorf("telegram rejected: %s", scrubChannelSecret(description, ch))
	}
	return nil
}

func scrubChannelSecret(text string, ch Channel) string {
	if text == "" {
		return text
	}
	replacements := make([]string, 0, 8)
	if v := strings.TrimSpace(ch.Config.Webhook); v != "" {
		replacements = append(replacements, v)
		if parsed, err := url.Parse(v); err == nil {
			query := parsed.Query()
			for _, key := range []string{"key", "access_token"} {
				if value := query.Get(key); value != "" {
					replacements = append(replacements, value)
				}
			}
		}
	}
	if v := strings.TrimSpace(ch.Config.Secret); v != "" {
		replacements = append(replacements, v)
	}
	if v := strings.TrimSpace(ch.Config.BotToken); v != "" {
		replacements = append(replacements, v)
	}
	for _, secret := range replacements {
		text = strings.ReplaceAll(text, secret, "***")
	}
	return text
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}

func intValue(raw any) int {
	switch value := raw.(type) {
	case float64:
		return int(value)
	case json.Number:
		parsed, _ := value.Int64()
		return int(parsed)
	case int:
		return value
	default:
		return 0
	}
}
