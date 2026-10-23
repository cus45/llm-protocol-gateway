package monitoring

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ChannelStore is the persistence slice the dispatcher needs; channels come
// from settings, not the store, so each tick can pick up CRUD changes.
type ChannelStore interface {
	UpsertMonitorState(AlertState) (AlertState, error)
	UpsertMonitorEventPush(EventPush) error
	UpdateMonitorEventPush(eventID int64, status, message string) error
	ListMonitorEventPushes(eventID int64) ([]EventPush, error)
}

type queuedNotification struct {
	Event    Event
	State    AlertState
	Channel  Channel
	Message  Notification
	Attempts int
}

type Dispatcher struct {
	mu        sync.Mutex
	store     ChannelStore
	adapters  map[string]ChannelAdapter
	channels  []Channel
	dispatch  DispatchConfig
	rules     map[string]RuleConfig
	now       func() time.Time
	retryWait []time.Duration // 默认 {1s, 5s}；3 次尝试，不是 3 次重试
	deferred  []queuedNotification
}

func NewDispatcher(store ChannelStore, channels []Channel, cfg DispatchConfig, rules []RuleConfig) *Dispatcher {
	d := &Dispatcher{
		store:     store,
		adapters:  map[string]ChannelAdapter{},
		now:       time.Now,
		retryWait: []time.Duration{time.Second, 5 * time.Second},
	}
	for _, channelType := range []string{"feishu", "dingtalk", "wecom", "telegram"} {
		if adapter, ok := AdapterFor(channelType); ok {
			d.adapters[channelType] = adapter
		}
	}
	d.Reload(channels, cfg, rules)
	return d
}

// Reload makes channel CRUD effective on the next tick without a restart.
func (d *Dispatcher) Reload(channels []Channel, cfg DispatchConfig, rules []RuleConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.channels = append([]Channel(nil), channels...)
	d.dispatch = cfg
	d.rules = indexRules(rules)
}

func (d *Dispatcher) Dispatch(ctx context.Context, tr Transition) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	ev := tr.Event
	if ev.Kind != EventKindAlert || ev.Severity == SeverityInfo {
		return nil
	}
	now := d.now()
	state := tr.State

	if ev.Severity == SeverityP2 {
		return d.markEvent(ev.ID, PushSkipped, "p2 事件不推送")
	}
	if state.Disposition == DispositionIgnored {
		return d.markEvent(ev.ID, PushSuppressed, "告警已忽略")
	}
	if state.SuppressedUntil.After(now) {
		return d.markEvent(ev.ID, PushSuppressed, "抖动抑制中")
	}
	if !tr.Upgraded && !tr.Reminder && tr.Firing && !state.LastNotifiedAt.IsZero() &&
		now.Sub(state.LastNotifiedAt) < d.cooldownFor(state.RuleKey) {
		return d.markEvent(ev.ID, PushSkipped, "冷却中")
	}

	if tr.Resolved {
		if !d.dispatch.RecoveryNotify {
			return d.markEvent(ev.ID, PushSkipped, "恢复通知已关闭")
		}
		if state.LastSeen.Sub(state.FirstSeen) < 5*time.Minute {
			return d.markEvent(ev.ID, PushSkipped, "持续时间不足 5 分钟")
		}
	}

	channels := d.targetChannels(ev.Severity)
	if len(channels) == 0 {
		return d.markEvent(ev.ID, PushSkipped, "没有匹配的通知渠道")
	}

	message := buildNotification(state, ev, d.dispatch.BaseURL, d.dispatch.Location())
	if d.inQuietHours(now) && ev.Severity != SeverityP0 {
		for _, channel := range channels {
			d.deferred = append(d.deferred, queuedNotification{Event: ev, State: state, Channel: channel, Message: message})
		}
		return d.markEvent(ev.ID, PushPending, "静默时段延后")
	}

	delivered := false
	for _, channel := range channels {
		adapter, ok := d.adapters[channel.Type]
		if !ok {
			push := EventPush{
				EventID: ev.ID, ChannelID: channel.ID, Status: PushSkipped,
				LastError: "unsupported channel type", UpdatedAt: now,
			}
			_ = d.store.UpsertMonitorEventPush(push)
			continue
		}
		attempts, err := d.send(ctx, adapter, channel, message)
		push := EventPush{EventID: ev.ID, ChannelID: channel.ID, Attempts: attempts, UpdatedAt: now}
		if err != nil {
			push.Status = PushFailed
			push.LastError = truncate(scrubChannelSecret(err.Error(), channel), 500)
		} else {
			push.Status = PushSent
			delivered = true
		}
		_ = d.store.UpsertMonitorEventPush(push)
	}
	if delivered && !tr.Resolved {
		state.LastNotifiedAt = now
		_, _ = d.store.UpsertMonitorState(state)
	}
	return d.aggregateEvent(ev.ID)
}

// Flush resends notifications that were deferred by quiet hours.
func (d *Dispatcher) Flush(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.deferred) == 0 {
		return nil
	}
	now := d.now()
	if d.inQuietHours(now) {
		return nil
	}
	queue := d.deferred
	d.deferred = nil
	touched := map[int64]bool{}
	remaining := make([]queuedNotification, 0)
	for index, item := range queue {
		if err := ctx.Err(); err != nil {
			remaining = append(remaining, queue[index:]...)
			break
		}
		touched[item.Event.ID] = true
		if !item.Channel.Enabled || !item.Event.Severity.Meets(item.Channel.MinSeverity) {
			_ = d.store.UpsertMonitorEventPush(EventPush{
				EventID: item.Event.ID, ChannelID: item.Channel.ID, Status: PushSkipped,
				LastError: "channel no longer matches", UpdatedAt: now,
			})
			continue
		}
		adapter, ok := d.adapters[item.Channel.Type]
		if !ok {
			_ = d.store.UpsertMonitorEventPush(EventPush{
				EventID: item.Event.ID, ChannelID: item.Channel.ID, Status: PushSkipped,
				LastError: "unsupported channel type", UpdatedAt: now,
			})
			continue
		}
		message := item.Message
		message.At = now
		attempts, err := d.send(ctx, adapter, item.Channel, message)
		item.Attempts += attempts
		push := EventPush{EventID: item.Event.ID, ChannelID: item.Channel.ID, Attempts: item.Attempts, UpdatedAt: now}
		if err != nil {
			push.Status = PushFailed
			push.LastError = truncate(scrubChannelSecret(err.Error(), item.Channel), 500)
			remaining = append(remaining, item)
		} else {
			push.Status = PushSent
			if item.Event.Status != StatusResolved {
				item.State.LastNotifiedAt = now
				_, _ = d.store.UpsertMonitorState(item.State)
			}
		}
		_ = d.store.UpsertMonitorEventPush(push)
	}
	d.deferred = remaining
	for eventID := range touched {
		if err := d.aggregateEvent(eventID); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (d *Dispatcher) send(ctx context.Context, adapter ChannelAdapter, channel Channel, message Notification) (int, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		lastErr = adapter.Send(ctx, channel, message)
		if lastErr == nil {
			return attempt, nil
		}
		if attempt < 3 {
			wait := d.retryWait[attempt-1]
			select {
			case <-ctx.Done():
				return attempt, lastErr
			case <-time.After(wait):
			}
		}
	}
	return 3, lastErr
}

func (d *Dispatcher) targetChannels(severity Severity) []Channel {
	out := make([]Channel, 0, len(d.channels))
	for _, channel := range d.channels {
		if !channel.Enabled || !severity.Meets(channel.MinSeverity) {
			continue
		}
		out = append(out, channel)
	}
	return out
}

func (d *Dispatcher) cooldownFor(ruleKey string) time.Duration {
	rule, ok := d.rules[ruleKey]
	if !ok || rule.CooldownMinutes <= 0 {
		return 60 * time.Minute
	}
	return time.Duration(rule.CooldownMinutes) * time.Minute
}

func (d *Dispatcher) inQuietHours(now time.Time) bool {
	if !d.dispatch.QuietEnabled {
		return false
	}
	start, okStart := parseClock(d.dispatch.QuietStart)
	end, okEnd := parseClock(d.dispatch.QuietEnd)
	if !okStart || !okEnd || start == end {
		return false
	}
	local := now.In(d.dispatch.Location())
	minutes := local.Hour()*60 + local.Minute()
	if start < end {
		return minutes >= start && minutes < end
	}
	return minutes >= start || minutes < end
}

func parseClock(raw string) (int, bool) {
	parts := strings.SplitN(strings.TrimSpace(raw), ":", 2)
	if len(parts) != 2 {
		return 0, false
	}
	hour, errHour := strconv.Atoi(parts[0])
	minute, errMinute := strconv.Atoi(parts[1])
	if errHour != nil || errMinute != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

func (d *Dispatcher) markEvent(eventID int64, status, message string) error {
	if eventID == 0 {
		return nil
	}
	return d.store.UpdateMonitorEventPush(eventID, status, message)
}

// aggregateEvent folds per-channel results back into the event row the list
// view reads: sent / partial / failed / pending / skipped.
func (d *Dispatcher) aggregateEvent(eventID int64) error {
	if eventID == 0 {
		return nil
	}
	pushes, err := d.store.ListMonitorEventPushes(eventID)
	if err != nil {
		return err
	}
	status, firstError, ok := AggregatePushStatus(pushes)
	if !ok {
		return nil
	}
	return d.store.UpdateMonitorEventPush(eventID, status, firstError)
}

// AggregatePushStatus turns the per-channel push rows of one event into the
// single status the list view shows. ok is false when nothing was attempted.
// The console's retry-push handler needs the same folding as Dispatch, so it
// lives here rather than being duplicated per caller.
func AggregatePushStatus(pushes []EventPush) (status, firstError string, ok bool) {
	if len(pushes) == 0 {
		return "", "", false
	}
	sent, failed, pending, skipped := 0, 0, 0, 0
	for _, push := range pushes {
		switch push.Status {
		case PushSent:
			sent++
		case PushFailed:
			failed++
			if firstError == "" {
				firstError = push.LastError
			}
		case PushSkipped:
			skipped++
		default:
			pending++
		}
	}
	status = PushSkipped
	switch {
	case pending > 0:
		status = PushPending
	case sent > 0 && failed > 0:
		status = PushPartial
	case sent > 0:
		status = PushSent
	case failed > 0:
		status = PushFailed
	case skipped > 0:
		status = PushSkipped
	}
	return status, firstError, true
}

// BuildNotification renders the message one dispatch sends. The console's
// manual "retry push" needs byte-identical output to the automatic send, so it
// calls this instead of rebuilding the markdown.
func BuildNotification(state AlertState, ev Event, baseURL string, loc *time.Location) Notification {
	return buildNotification(state, ev, baseURL, loc)
}

func buildNotification(state AlertState, ev Event, baseURL string, loc *time.Location) Notification {
	when := ev.Time
	if when.IsZero() {
		when = time.Now()
	}
	if loc == nil {
		loc = time.UTC
	}
	markdown := fmt.Sprintf("- 对象：%s\n- 级别：%s\n- 时间：%s\n- 详情：%s",
		state.TargetName, strings.ToUpper(string(ev.Severity)),
		when.In(loc).Format("2006-01-02 15:04:05"), summaryDetail(ev.Detail))
	notification := Notification{
		Fingerprint: state.Fingerprint,
		Severity:    ev.Severity,
		Title:       ev.Title,
		Target:      state.TargetName,
		Markdown:    markdown,
		At:          when,
		Kind:        "firing",
	}
	if ev.Status == StatusResolved {
		notification.Kind = "resolved"
	}
	if baseURL != "" {
		notification.URL = strings.TrimRight(baseURL, "/") + "/monitoring?tab=alerts&alert=" + strconv.FormatInt(state.ID, 10)
	}
	return notification
}

// summaryDetail renders only scalar values and IP lists. Credential-shaped
// keys are dropped even though no detail should ever carry them.
func summaryDetail(detail map[string]any) string {
	if len(detail) == 0 {
		return "无"
	}
	keys := make([]string, 0, len(detail))
	for key := range detail {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "key") {
			continue
		}
		switch value := detail[key].(type) {
		case string:
			if value != "" {
				parts = append(parts, key+"="+value)
			}
		case bool:
			parts = append(parts, key+"="+strconv.FormatBool(value))
		case int:
			parts = append(parts, key+"="+strconv.Itoa(value))
		case int64:
			parts = append(parts, key+"="+strconv.FormatInt(value, 10))
		case float64:
			parts = append(parts, key+"="+strconv.FormatFloat(value, 'f', -1, 64))
		case []string:
			ips := value
			if len(ips) > 12 {
				ips = ips[:12]
			}
			if len(ips) > 0 {
				parts = append(parts, key+"="+strings.Join(ips, ","))
			}
		}
	}
	if len(parts) == 0 {
		return "无"
	}
	return strings.Join(parts, "，")
}
