package monitoring

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func channelTestNotification() Notification {
	return Notification{
		Fingerprint: "oauth_expiring|provider:p1",
		Severity:    SeverityP0,
		Title:       "P1 凭证即将过期",
		Target:      "P1",
		Markdown:    "- 对象：P1\n- 级别：P0",
		At:          time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC),
		Kind:        "firing",
	}
}

func TestFeishuSignAndPayload(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	secret := "feishu-secret"
	ch := Channel{ID: "c1", Type: "feishu", Config: ChannelConfig{Webhook: "https://example.invalid/hook", Secret: secret}}
	n := channelTestNotification()

	payload, err := feishuPayload(ch, n, now)
	if err != nil {
		t.Fatal(err)
	}
	ts := strconv.FormatInt(now.Unix(), 10)
	if payload["timestamp"] != ts {
		t.Fatalf("timestamp = %v", payload["timestamp"])
	}
	mac := hmac.New(sha256.New, []byte(ts+"\n"+secret))
	mac.Write(nil)
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if payload["sign"] != want {
		t.Fatalf("sign = %v want %s", payload["sign"], want)
	}
	if payload["msg_type"] != "interactive" {
		t.Fatalf("msg_type = %v", payload["msg_type"])
	}

	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0}`)
	}))
	defer srv.Close()

	ch.Config.Webhook = srv.URL
	adapter := feishuAdapter{now: func() time.Time { return now }}
	if err := adapter.Send(context.Background(), ch, n); err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(captured, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["msg_type"] != "interactive" || sent["sign"] == nil || sent["timestamp"] != ts {
		t.Fatalf("unexpected payload: %s", captured)
	}
	card, _ := sent["card"].(map[string]any)
	header, _ := card["header"].(map[string]any)
	title, _ := header["title"].(map[string]any)
	content, _ := title["content"].(string)
	if !strings.HasPrefix(content, "[P0] ") {
		t.Fatalf("card title lacks severity prefix: %q", content)
	}
}

func TestDingTalkSignInQuery(t *testing.T) {
	now := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC)
	secret := "ding-secret"
	webhook := "https://example.invalid/robot/send?access_token=token"

	signed, err := dingtalkURL(webhook, secret, now)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(signed)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("timestamp") != strconv.FormatInt(now.UnixMilli(), 10) {
		t.Fatalf("timestamp = %q", query.Get("timestamp"))
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(query.Get("timestamp") + "\n" + secret))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if query.Get("sign") != want {
		t.Fatalf("sign = %q want %q", query.Get("sign"), want)
	}

	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		if r.URL.Query().Get("sign") == "" {
			t.Errorf("missing sign query")
		}
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	ch := Channel{ID: "c2", Type: "dingtalk", Config: ChannelConfig{Webhook: srv.URL + "/robot/send?access_token=t", Secret: secret}}
	adapter := dingtalkAdapter{now: func() time.Time { return now }}
	if err := adapter.Send(context.Background(), ch, channelTestNotification()); err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(captured, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["msgtype"] != "actionCard" {
		t.Fatalf("msgtype = %v", sent["msgtype"])
	}
}

func TestWeComMarkdown(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	ch := Channel{ID: "c3", Type: "wecom", Config: ChannelConfig{Webhook: srv.URL + "/cgi-bin/webhook/send?key=super-key"}}
	if err := (wecomAdapter{}).Send(context.Background(), ch, channelTestNotification()); err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	if err := json.Unmarshal(captured, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["msgtype"] != "markdown" {
		t.Fatalf("msgtype = %v", sent["msgtype"])
	}
	markdown, _ := sent["markdown"].(map[string]any)
	content, _ := markdown["content"].(string)
	if !strings.HasPrefix(content, "[P0] ") {
		t.Fatalf("markdown content does not start with severity: %q", content)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "bad request for key super-key")
	}))
	defer failing.Close()
	ch.Config.Webhook = failing.URL + "/cgi-bin/webhook/send?key=super-key"
	err := (wecomAdapter{}).Send(context.Background(), ch, channelTestNotification())
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if strings.Contains(err.Error(), "super-key") {
		t.Fatalf("error leaked webhook key: %v", err)
	}
}

func TestTelegramPayloadAndTokenScrub(t *testing.T) {
	token := "123456:AA-secret-token"
	var captured []byte
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		captured, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	oldBase := telegramAPIBase
	telegramAPIBase = srv.URL
	defer func() { telegramAPIBase = oldBase }()

	ch := Channel{ID: "c4", Type: "telegram", Config: ChannelConfig{BotToken: token, ChatID: "42"}}
	if err := (telegramAdapter{}).Send(context.Background(), ch, channelTestNotification()); err != nil {
		t.Fatal(err)
	}
	if path != "/bot"+token+"/sendMessage" {
		t.Fatalf("path = %q", path)
	}
	var sent map[string]any
	if err := json.Unmarshal(captured, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["chat_id"] != "42" {
		t.Fatalf("chat_id = %v", sent["chat_id"])
	}
	text, _ := sent["text"].(string)
	if !strings.HasPrefix(text, "[P0] ") {
		t.Fatalf("text = %q", text)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"ok":false,"description":"invalid token `+token+`"}`)
	}))
	defer failing.Close()
	telegramAPIBase = failing.URL
	err := (telegramAdapter{}).Send(context.Background(), ch, channelTestNotification())
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaked bot token: %v", err)
	}
}
