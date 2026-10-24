package monitoring

import (
	"context"
	"fmt"
	"regexp"
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
	// digest 是 P1 摘要队列（按渠道分桶），digestDue 是该渠道下一次可发摘要的时刻。
	//
	// 取舍：队列只放内存，进程重启即丢弃。摘要不是账本——事件行、push 记录和
	// 告警状态都在库里，丢的只是「还没凑进摘要的那几条 P1」这一轮投递机会，
	// 重启后不会因此重放历史 P1，也不会重复推送：已发出的摘要不会再被构造，
	// 未发出的条目随进程消失（不补发、不重发）。因此这里不需要也不应该做持久化。
	digest    map[string][]digestEntry
	digestDue map[string]time.Time
}

// digestEntry is one queued P1 (firing, upgrade or reminder) or P1 recovery.
// Recoveries ride the next digest instead of going out on their own.
type digestEntry struct {
	Event   Event
	State   AlertState
	Pending bool // 静默期内排队：等待 push 记录标为 pending，发送后再标 sent
}

// digestGroup is one digest assembled for one channel.
type digestGroup struct {
	Channel Channel
	Entries []digestEntry
	// Collapsed maps event ID → true for firing entries folded away by a
	// recovery of the same alert inside the same window. Those events were not
	// in the message, so flushDigests records them as skipped.
	Collapsed map[int64]bool
}

func NewDispatcher(store ChannelStore, channels []Channel, cfg DispatchConfig, rules []RuleConfig) *Dispatcher {
	d := &Dispatcher{
		store:     store,
		adapters:  map[string]ChannelAdapter{},
		now:       time.Now,
		retryWait: []time.Duration{time.Second, 5 * time.Second},
		digest:    map[string][]digestEntry{},
		digestDue: map[string]time.Time{},
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

	// P1 不发单条：进摘要队列，由 Flush 按 digestMinutes 汇总。P0 仍即时发出。
	if ev.Severity == SeverityP1 {
		queued := d.inQuietHours(now)
		// 入队前定好窗口：第一个条目在 0 分入队时窗口是「0 分 + digestMinutes」，
		// 后入队的条目并入同一份摘要、不改窗口。单条 P1 也要等满窗口，节奏一致。
		// （先算再改队列：改完再读队列长度在这个工具链上不安全，见 go126 记录。）
		interval := d.dispatch.DigestInterval()
		for _, channel := range channels {
			first := len(d.digest[channel.ID]) == 0
			due := d.digestDue[channel.ID]
			if first || due.IsZero() {
				d.digestDue[channel.ID] = now.Add(interval)
			}
			entry := digestEntry{Event: ev, State: state, Pending: queued}
			d.digest[channel.ID] = append(d.digest[channel.ID], entry)
		}
		if queued {
			return d.markEvent(ev.ID, PushPending, "静默时段延后")
		}
		return d.markEvent(ev.ID, PushPending, "已加入 P1 摘要")
	}

	message := buildNotification(state, ev, d.dispatch.BaseURL, d.dispatch.Location())
	// P0 永远即时，静默时段也不例外（计划 §4 红线）；静默只推迟 P1 摘要。
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
	if len(d.deferred) == 0 && d.digestQueued() == 0 {
		return nil
	}
	now := d.now()
	quiet := d.inQuietHours(now)
	if !quiet {
		d.flushDeferred(ctx, now)
	}
	d.flushDigests(ctx, now, quiet)
	return ctx.Err()
}

// flushDeferred resends P0 notifications postponed by quiet hours.
func (d *Dispatcher) flushDeferred(ctx context.Context, now time.Time) {
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
			return
		}
	}
}

// digestQueued counts P1 entries waiting for a digest.
func (d *Dispatcher) digestQueued() int {
	total := 0
	for _, entries := range d.digest {
		total += len(entries)
	}
	return total
}

// flushDigests sends one digest per channel that has queued P1 entries and is
// due. Quiet hours hold everything back; the first non-quiet tick after they
// end sends one digest with everything that accumulated. When the last entry
// is added the next window starts from that moment, so a changed
// digestMinutes takes effect after the current window instead of re-sending
// already-delivered entries.
func (d *Dispatcher) flushDigests(ctx context.Context, now time.Time, quiet bool) {
	if quiet {
		return
	}
	interval := d.dispatch.DigestInterval()
	for _, channel := range d.channels {
		entries := d.digest[channel.ID]
		if len(entries) == 0 {
			continue
		}
		// 窗口在入队时设定：未到点一律等。零值只出现在渠道已被 reject 后重新开始
		// 排队的极短窗口里，按本 tick 起算，避免条目无处可去。
		due := d.digestDue[channel.ID]
		if !due.IsZero() && now.Before(due) {
			continue
		}
		if !channel.Enabled || !SeverityP1.Meets(channel.MinSeverity) {
			d.rejectDigest(channel, entries, now, "channel no longer matches")
			continue
		}
		adapter, ok := d.adapters[channel.Type]
		if !ok {
			d.rejectDigest(channel, entries, now, "unsupported channel type")
			continue
		}
		group := d.digestGroup(channel, entries)
		message := buildDigestNotification(group, d.dispatch.BaseURL, d.dispatch.Location(), now)
		attempts, err := d.send(ctx, adapter, channel, message)
		// 不论成败都收下这份摘要：失败时按事件标 failed，条目本身出队，不并进
		// 下一份（否则收件人会重复看到同一条）。是否重发由控制台的「重试推送」决定。
		delete(d.digest, channel.ID)
		for _, entry := range entries {
			if group.Collapsed[entry.Event.ID] {
				// 同一窗口内先 firing 后 resolved：这条 firing 没有出现在摘要里，
				// 不能记 sent，也不能推进 LastNotifiedAt（否则提醒会被推迟一个冷却）。
				_ = d.store.UpsertMonitorEventPush(EventPush{
					EventID: entry.Event.ID, ChannelID: channel.ID, Status: PushSkipped,
					LastError: "同一窗口内已恢复", UpdatedAt: now,
				})
				_ = d.aggregateEvent(entry.Event.ID)
				continue
			}
			push := EventPush{
				EventID: entry.Event.ID, ChannelID: channel.ID, Attempts: attempts, Status: PushSent, UpdatedAt: now,
			}
			if err != nil {
				push.Status = PushFailed
				push.LastError = truncate(scrubChannelSecret(err.Error(), channel), 500)
			}
			_ = d.store.UpsertMonitorEventPush(push)
			if err == nil && entry.Event.Status != StatusResolved {
				state := entry.State
				state.LastNotifiedAt = now
				_, _ = d.store.UpsertMonitorState(state)
			}
			_ = d.aggregateEvent(entry.Event.ID)
		}
		// 不论成败都推进窗口：失败时下一条新 P1 在下一分钟就能出摘要，
		// 代价是「窗口从发送时刻起算」，避免失败后每分钟重试打爆渠道。
		d.digestDue[channel.ID] = now.Add(interval)
	}
}

// rejectDigest drops entries whose channel stopped matching or whose type has
// no adapter; those events are skipped rather than silently pending forever.
func (d *Dispatcher) rejectDigest(channel Channel, entries []digestEntry, now time.Time, reason string) {
	for _, entry := range entries {
		_ = d.store.UpsertMonitorEventPush(EventPush{
			EventID: entry.Event.ID, ChannelID: channel.ID, Status: PushSkipped, LastError: reason, UpdatedAt: now,
		})
		_ = d.aggregateEvent(entry.Event.ID)
	}
	delete(d.digest, channel.ID)
	// 保留窗口：下一条 P1 入队时若窗口未到，仍并入本窗口节奏，而不是立刻发。
}

// digestGroup folds queued entries into one group: a recovery cancels a firing
// of the same alert that is still waiting in the same window, and repeats of
// one alert collapse into one line.
func (d *Dispatcher) digestGroup(channel Channel, entries []digestEntry) digestGroup {
	group := digestGroup{Channel: channel, Collapsed: map[int64]bool{}}
	firingIndex := map[string]int{} // key → index in group.Entries
	resolvedIndex := map[string]int{}
	for _, entry := range entries {
		key := entry.State.Fingerprint
		if key == "" {
			key = entry.Event.Fingerprint
		}
		if entry.Event.Status == StatusResolved {
			// 恢复盖掉本窗口里同告警的 pending firing：firing 不再单独出现在摘要
			// 里，它的 push 记录交给 flushDigests 标 skipped。
			if index, ok := firingIndex[key]; ok {
				group.Collapsed[group.Entries[index].Event.ID] = true
				group.Entries[index] = digestEntry{State: entry.State, Event: Event{
					Status: StatusResolved, Fingerprint: key, TargetName: entry.State.TargetName,
					RuleKey: entry.State.RuleKey, TargetType: entry.State.TargetType, TargetID: entry.State.TargetID,
				}}
				delete(firingIndex, key)
				resolvedIndex[key] = index
				continue
			}
			if _, ok := resolvedIndex[key]; ok {
				// 同告警再次恢复：保留第一次恢复那一刻的那一行，后来的不重复列出。
				group.Collapsed[entry.Event.ID] = true
				continue
			}
			resolvedIndex[key] = len(group.Entries)
			group.Entries = append(group.Entries, digestEntry{State: entry.State, Event: Event{
				Status: StatusResolved, Fingerprint: key, TargetName: entry.State.TargetName,
				RuleKey: entry.State.RuleKey, TargetType: entry.State.TargetType, TargetID: entry.State.TargetID,
			}})
			continue
		}
		if index, ok := firingIndex[key]; ok {
			// 同一告警在窗口内重复进入（提醒、升级）：只列一次，取最新一条。
			group.Collapsed[group.Entries[index].Event.ID] = true
			group.Entries[index] = entry
			continue
		}
		firingIndex[key] = len(group.Entries)
		group.Entries = append(group.Entries, entry)
	}
	return group
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

// digestItemText is one line of a digest: 「对象：规则名」.
//
// 摘要里刻意只放对象名和规则名，不放事件 Detail：Detail 是自由字段，摘要批量
// 外发的面更大，且可能混入账号侧的错误文本。对象名允许出现（账号名是运维最需要
// 的信息），但它来自用户输入，可能被贴成「sk-…」或「Bearer …」：先经
// scrubCredentialShapes 擦掉凭据形状，渠道级 scrub 只负责渠道自己的 token。
func digestItemText(state AlertState, ev Event) string {
	name := strings.TrimSpace(state.TargetName)
	if name == "" {
		name = strings.TrimSpace(ev.TargetName)
	}
	name = scrubCredentialShapes(name)
	rule := ruleName(state.RuleKey)
	if rule == "" {
		rule = scrubCredentialShapes(strings.TrimSpace(ev.Title))
	}
	if name == "" {
		name = rule
		rule = ""
	}
	if rule == "" {
		return name
	}
	if name == "" {
		return rule
	}
	return name + " " + rule
}

// buildDigestNotification renders the one message that carries a window of P1
// events. The title always starts with 「N 条 P1」 so subscribers can tell a
// digest from a single alert at a glance.
func buildDigestNotification(group digestGroup, baseURL string, loc *time.Location, now time.Time) Notification {
	if loc == nil {
		loc = time.UTC
	}
	firing := make([]string, 0, len(group.Entries))
	resolved := make([]string, 0, len(group.Entries))
	for _, entry := range group.Entries {
		text := digestItemText(entry.State, entry.Event)
		if entry.Event.Status == StatusResolved {
			resolved = append(resolved, text)
			continue
		}
		firing = append(firing, text)
	}
	count := len(firing) + len(resolved)
	lines := make([]string, 0, count+3)
	if len(firing) > 0 {
		lines = append(lines, fmt.Sprintf("待处理（%d）：", len(firing)))
		for _, text := range firing {
			lines = append(lines, "- "+text)
		}
	}
	if len(resolved) > 0 {
		lines = append(lines, fmt.Sprintf("已恢复（%d）：", len(resolved)))
		for _, text := range resolved {
			lines = append(lines, "- "+text)
		}
	}
	if !now.IsZero() {
		lines = append(lines, "", "汇总时间："+now.In(loc).Format("2006-01-02 15:04"))
	}
	return Notification{
		Severity: SeverityP1,
		Title:    fmt.Sprintf("%d 条 P1", count),
		Markdown: scrubDigestSecrets(strings.Join(lines, "\n"), group.Channel),
		At:       now,
		Kind:     "digest",
	}
}

// scrubDigestSecrets removes the channel's own credentials from a digest body.
// The body is built from scrubbed names only, so this is a backstop.
func scrubDigestSecrets(text string, channel Channel) string {
	return scrubChannelSecret(scrubCredentialShapes(text), channel)
}

// credentialShapePatterns 匹配常见凭据形状。对象名/标题是用户自由输入，可能被
// 直接粘贴成密钥；摘要会外发到群聊，所以在这一步统一擦除，而不是等渠道报错。
var credentialShapePatterns = []*regexp.Regexp{
	// 通用前缀：OpenAI 风格的 sk-…。
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`),
	// 厂商私有 token 前缀：Slack / GitHub / GitLab。
	regexp.MustCompile(`xox[bopar]-[A-Za-z0-9-]{8,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{16,}`),
	regexp.MustCompile(`glpat-[A-Za-z0-9_-]{16,}`),
	// Authorization 头里粘过来的 Bearer。
	regexp.MustCompile(`(?i)Bearer\s+\S+`),
	// key=value 形式的敏感参数（不在 URL 里也擦）。
	regexp.MustCompile(`(?i)(api_?key|access_token|token|secret)=[^\s&]+`),
	// URL query 里的敏感参数：对象名常被贴成带鉴权的 webhook 地址。
	// （常与上一条重叠，但两条都替换成 ***，谁先命中结果一样。）
	regexp.MustCompile(`(?i)[?&](key|token|access_token|api_key|apikey|secret|sign|signature|t)=[^&\s]+`),
}

// scrubCredentialShapes replaces credential-shaped substrings with ***.
// Idempotent: every pattern needs a prefix/parameter name that the replacement
// (***) does not reproduce, so a second pass leaves the text unchanged.
func scrubCredentialShapes(text string) string {
	for _, pattern := range credentialShapePatterns {
		text = pattern.ReplaceAllString(text, "***")
	}
	return text
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
