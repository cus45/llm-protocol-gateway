package monitoring

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// telegramAPIBase is a variable so tests can point the adapter at a local
// server; production always uses the public Bot API.
var telegramAPIBase = "https://api.telegram.org"

func severityTag(severity Severity) string {
	return "[" + strings.ToUpper(string(severity)) + "] "
}

type feishuAdapter struct {
	now func() time.Time
}

func (a feishuAdapter) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a feishuAdapter) Send(ctx context.Context, ch Channel, n Notification) error {
	webhook := strings.TrimSpace(ch.Config.Webhook)
	if webhook == "" {
		return fmt.Errorf("feishu: webhook is empty")
	}
	payload, err := feishuPayload(ch, n, a.clock())
	if err != nil {
		return err
	}
	status, body, err := postJSON(ctx, webhook, payload)
	if err != nil {
		return channelTransportError("feishu", ch, err)
	}
	if status < 200 || status >= 300 {
		return channelError("feishu", ch, status, body)
	}
	return channelResponseError("feishu", ch, body)
}

func feishuPayload(ch Channel, n Notification, now time.Time) (map[string]any, error) {
	ts := strconv.FormatInt(now.Unix(), 10)
	payload := map[string]any{
		"timestamp": ts,
		"msg_type":  "interactive",
		"card": map[string]any{
			"schema": "2.0",
			"header": map[string]any{
				"title": map[string]any{
					"tag": "plain_text", "content": severityTag(n.Severity) + n.Title,
				},
				"template": feishuTemplate(n.Severity),
			},
			"body": map[string]any{"elements": []any{
				map[string]any{"tag": "markdown", "content": n.Markdown},
			}},
		},
	}
	if secret := strings.TrimSpace(ch.Config.Secret); secret != "" {
		// 飞书自定义机器人：key = timestamp+"\n"+secret，message 为空。
		mac := hmac.New(sha256.New, []byte(ts+"\n"+secret))
		mac.Write(nil)
		payload["sign"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	return payload, nil
}

func feishuTemplate(severity Severity) string {
	switch severity {
	case SeverityP0:
		return "red"
	case SeverityP1:
		return "orange"
	case SeverityP2:
		return "blue"
	default:
		return "grey"
	}
}

type dingtalkAdapter struct {
	now func() time.Time
}

func (a dingtalkAdapter) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a dingtalkAdapter) Send(ctx context.Context, ch Channel, n Notification) error {
	webhook := strings.TrimSpace(ch.Config.Webhook)
	if webhook == "" {
		return fmt.Errorf("dingtalk: webhook is empty")
	}
	signed, err := dingtalkURL(webhook, strings.TrimSpace(ch.Config.Secret), a.clock())
	if err != nil {
		return err
	}
	status, body, err := postJSON(ctx, signed, dingtalkPayload(n))
	if err != nil {
		return channelTransportError("dingtalk", ch, err)
	}
	if status < 200 || status >= 300 {
		return channelError("dingtalk", ch, status, body)
	}
	return channelResponseError("dingtalk", ch, body)
}

func dingtalkURL(webhook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return webhook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	sep := "?"
	if strings.Contains(webhook, "?") {
		sep = "&"
	}
	return webhook + sep + "timestamp=" + ts + "&sign=" + sign, nil
}

func dingtalkPayload(n Notification) map[string]any {
	actionCard := map[string]any{
		"title":          severityTag(n.Severity) + n.Title,
		"text":           "### " + severityTag(n.Severity) + n.Title + "\n\n" + n.Markdown,
		"btnOrientation": "0",
	}
	if n.URL != "" {
		actionCard["singleTitle"] = "查看监控中心"
		actionCard["singleURL"] = n.URL
	}
	return map[string]any{"msgtype": "actionCard", "actionCard": actionCard}
}

type wecomAdapter struct{}

func (wecomAdapter) Send(ctx context.Context, ch Channel, n Notification) error {
	webhook := strings.TrimSpace(ch.Config.Webhook)
	if webhook == "" {
		return fmt.Errorf("wecom: webhook is empty")
	}
	status, body, err := postJSON(ctx, webhook, wecomPayload(n))
	if err != nil {
		return channelTransportError("wecom", ch, err)
	}
	if status < 200 || status >= 300 {
		return channelError("wecom", ch, status, body)
	}
	return channelResponseError("wecom", ch, body)
}

func wecomPayload(n Notification) map[string]any {
	content := severityTag(n.Severity) + n.Title + "\n\n" + n.Markdown
	if n.URL != "" {
		content += "\n\n[查看监控中心](" + n.URL + ")"
	}
	return map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
}

type telegramAdapter struct{}

func (telegramAdapter) Send(ctx context.Context, ch Channel, n Notification) error {
	token := strings.TrimSpace(ch.Config.BotToken)
	chatID := strings.TrimSpace(ch.Config.ChatID)
	if token == "" || chatID == "" {
		return fmt.Errorf("telegram: bot token and chat id are required")
	}
	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     severityTag(n.Severity) + n.Title + "\n\n" + n.Markdown,
		"parse_mode":               "Markdown",
		"disable_web_page_preview": true,
	}
	endpoint := telegramAPIBase + "/bot" + token + "/sendMessage"
	status, body, err := postJSON(ctx, endpoint, payload)
	if err != nil {
		return channelTransportError("telegram", ch, err)
	}
	if status < 200 || status >= 300 {
		return channelError("telegram", ch, status, body)
	}
	return channelResponseError("telegram", ch, body)
}
