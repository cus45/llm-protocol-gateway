package monitoring

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// recordingAdapter keeps every Notification so digest tests can assert on the
// message body (secrets, item list) and not just the call count. All digest
// tests run against this fake: no test in this file may reach a real webhook.
type recordingAdapter struct {
	sent []Notification
	err  error
}

func (r *recordingAdapter) Send(ctx context.Context, ch Channel, n Notification) error {
	r.sent = append(r.sent, n)
	return r.err
}

type digestClock struct{ at time.Time }

func (c *digestClock) now() time.Time { return c.at }

func (c *digestClock) advance(d time.Duration) { c.at = c.at.Add(d) }

func newDigestHarness(t *testing.T, cfg DispatchConfig, channels ...Channel) (*Dispatcher, *fakeStore, *recordingAdapter, *digestClock) {
	t.Helper()
	st := newFakeStore()
	adapter := &recordingAdapter{}
	clock := &digestClock{at: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)}
	d := newDispatcherForTest(st, channels, cfg, DefaultRules())
	d.now = clock.now
	// 三个类型的适配器都指向同一个 recorder：多渠道路径也只记录、不外呼。
	d.adapters["feishu"] = adapter
	return d, st, adapter, clock
}

// p1Event 构造一条带名字的 P1 firing transition（target 是账号名）。
func p1Event(target, ruleKey string, id int64, at time.Time) Transition {
	tr := testFiringTransition(at, SeverityP1, time.Minute)
	tr.State.Fingerprint = ruleKey + "|provider:" + target
	tr.State.RuleKey = ruleKey
	tr.State.TargetID = target
	tr.State.TargetName = target
	tr.State.FirstSeen = at.Add(-time.Minute)
	tr.State.LastSeen = at
	tr.Event.Fingerprint = tr.State.Fingerprint
	tr.Event.RuleKey = ruleKey
	tr.Event.TargetID = target
	tr.Event.TargetName = target
	tr.Event.Fingerprint = tr.State.Fingerprint
	tr.Event.Time = at
	return tr
}

func mustDispatch(t *testing.T, d *Dispatcher, tr Transition) {
	t.Helper()
	if err := d.Dispatch(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
}

// tickFor advances the clock one minute at a time (flushing each tick) until
// `minutes` minutes have elapsed from the harness clock's current value.
func tickFor(t *testing.T, d *Dispatcher, clock *digestClock, minutes int) {
	t.Helper()
	for i := 0; i < minutes; i++ {
		clock.advance(time.Minute)
		mustFlush(t, d)
	}
}

func mustFlush(t *testing.T, d *Dispatcher) {
	t.Helper()
	if err := d.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// ① P0 即时：Dispatch 当次就发，不经过摘要；摘要队列也不收 P0。
func TestDigestP0StaysImmediate(t *testing.T) {
	cfg := quietDisabled()
	d, st, adapter, clock := newDigestHarness(t, cfg, testChannel(SeverityP0))
	p0 := seedTransition(t, st, testFiringTransition(clock.now(), SeverityP0, time.Minute))

	mustDispatch(t, d, p0)
	if len(adapter.sent) != 1 || adapter.sent[0].Kind != "firing" {
		t.Fatalf("P0 not sent inline: %+v", adapter.sent)
	}
	if d.digestQueued() != 0 {
		t.Fatalf("P0 entered the digest queue: %d", d.digestQueued())
	}
	if got := st.events[p0.Event.ID-1].PushStatus; got != PushSent {
		t.Fatalf("event push status = %q", got)
	}
	// 到点 Flush 也不能再发第二份。
	clock.advance(15 * time.Minute)
	mustFlush(t, d)
	if len(adapter.sent) != 1 {
		t.Fatalf("digest re-sent P0: %+v", adapter.sent)
	}
}

// ② 一个 digestMinutes 窗口内的任意条数 P1（不同对象）→ 恰好一份摘要，标题
// 「N 条 P1」，正文含全部对象。单条 P1 也不特判：它同样等满窗口才发。
func TestDigestOneMessagePerWindowWithAllItems(t *testing.T) {
	cases := []struct {
		name  string
		items []struct{ target, rule string }
	}{
		{name: "single", items: []struct{ target, rule string }{{"账号 A", "quota_low"}}},
		{name: "three", items: []struct{ target, rule string }{
			{"账号 A", "quota_low"}, {"账号 B", "provider_down"}, {"账号 C", "rate_limit_spike"},
		}},
		{name: "five", items: []struct{ target, rule string }{
			{"账号 A", "quota_low"}, {"账号 B", "provider_down"}, {"账号 C", "rate_limit_spike"},
			{"账号 D", "empty_response"}, {"账号 E", "key_budget_exhausted"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := quietDisabled()
			cfg.DigestMinutes = 15
			d, st, adapter, clock := newDigestHarness(t, cfg, testChannel(SeverityP1))

			// 第一个条目在 0 分入队，其余依次在第 1、2…分钟入队，全部落在 15 分窗口内。
			for index, item := range tc.items {
				if index > 0 {
					clock.advance(time.Minute)
				}
				tr := p1Event(item.target, item.rule, int64(index+1), clock.now())
				mustDispatch(t, d, seedTransition(t, st, tr))
				if len(adapter.sent) != 0 {
					t.Fatalf("digest went out before the 15-minute window closed (%d sent)", len(adapter.sent))
				}
				mustFlush(t, d)
			}
			elapsed := time.Duration(len(tc.items)-1) * time.Minute
			total := int((15*time.Minute - elapsed) / time.Minute)
			tickFor(t, d, clock, total-1)
			if len(adapter.sent) != 0 {
				t.Fatalf("digest went out before the 15-minute window closed (%d sent)", len(adapter.sent))
			}
			clock.advance(time.Minute)
			mustFlush(t, d)

			if len(adapter.sent) != 1 {
				t.Fatalf("inside one 15-minute window there must be exactly one digest, got %d", len(adapter.sent))
			}
			digest := adapter.sent[0]
			if digest.Kind != "digest" {
				t.Fatalf("kind = %q", digest.Kind)
			}
			wantTitle := fmt.Sprintf("%d 条 P1", len(tc.items))
			if digest.Title != wantTitle {
				t.Fatalf("title = %q, want %q", digest.Title, wantTitle)
			}
			for _, item := range tc.items {
				if !strings.Contains(digest.Markdown, item.target) {
					t.Fatalf("digest missing object %q\n%s", item.target, digest.Markdown)
				}
			}
			if d.digestQueued() != 0 {
				t.Fatalf("queue not drained: %d", d.digestQueued())
			}
			// 窗口结束后的下一分钟不许再发第二份。
			tickFor(t, d, clock, 2)
			if len(adapter.sent) != 1 {
				t.Fatalf("second digest inside the same cadence: %d", len(adapter.sent))
			}
		})
	}
}

// ③ 静默期一条不发，结束后补发一条，且只有一条。
func TestDigestQuietHoursHoldsThenSendsOne(t *testing.T) {
	cfg := DefaultDispatchConfig()
	cfg.Timezone = "UTC"
	cfg.QuietEnabled = true
	cfg.QuietStart = "23:00"
	cfg.QuietEnd = "08:00"
	d, st, adapter, clock := newDigestHarness(t, cfg, testChannel(SeverityP1))
	clock.at = time.Date(2026, 10, 23, 23, 10, 0, 0, time.UTC)

	mustDispatch(t, d, seedTransition(t, st, p1Event("账号 A", "quota_low", 1, clock.now())))
	clock.advance(20 * time.Minute)
	mustDispatch(t, d, seedTransition(t, st, p1Event("账号 B", "provider_down", 2, clock.now())))
	// 静默期内跨过多个摘要窗口与多个 tick：一条都不许发。
	for i := 0; i < 60; i++ {
		clock.advance(5 * time.Minute)
		mustFlush(t, d)
	}
	if len(adapter.sent) != 0 {
		t.Fatalf("digest sent inside quiet hours: %d", len(adapter.sent))
	}
	if d.digestQueued() != 2 {
		t.Fatalf("queued = %d, want 2", d.digestQueued())
	}

	clock.at = time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	mustFlush(t, d)
	if len(adapter.sent) != 1 {
		t.Fatalf("quiet hours end must send exactly one digest, got %d", len(adapter.sent))
	}
	if !strings.Contains(adapter.sent[0].Markdown, "账号 A") || !strings.Contains(adapter.sent[0].Markdown, "账号 B") {
		t.Fatalf("quiet-hours digest lost items:\n%s", adapter.sent[0].Markdown)
	}
	mustFlush(t, d)
	if len(adapter.sent) != 1 || d.digestQueued() != 0 {
		t.Fatalf("after quiet flush: sent=%d queued=%d", len(adapter.sent), d.digestQueued())
	}
}

// ④ digestMinutes 决定节奏（表驱动：5 / 15 / 30）。窗口从第一条入队时刻起算：
// 0 分入队的 A 在第 digestMinutes 分发出，其间入队的 B 并入同一份。
func TestDigestCadenceFollowsDigestMinutes(t *testing.T) {
	cases := []struct{ minutes int }{{5}, {15}, {30}}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%dmin", tc.minutes), func(t *testing.T) {
			cfg := quietDisabled()
			cfg.DigestMinutes = tc.minutes
			d, st, adapter, clock := newDigestHarness(t, cfg, testChannel(SeverityP1))

			// A 在 0 分入队；单条也不特判，窗口没到一条都不许发。
			mustDispatch(t, d, seedTransition(t, st, p1Event("账号 A", "quota_low", 1, clock.now())))
			mustFlush(t, d)
			tickFor(t, d, clock, tc.minutes-2)
			if len(adapter.sent) != 0 {
				t.Fatalf("%dmin: first P1 went out after %d minutes", tc.minutes, tc.minutes-2)
			}
			// 第 digestMinutes-1 分钟入队的 B 并入同一份摘要，也不立刻发。
			clock.advance(time.Minute)
			mustDispatch(t, d, seedTransition(t, st, p1Event("账号 B", "provider_down", 2, clock.now())))
			mustFlush(t, d)
			if len(adapter.sent) != 0 {
				t.Fatalf("%dmin: second P1 went out immediately", tc.minutes)
			}
			// 窗口边界：恰好一份，含两条。
			clock.advance(time.Minute)
			mustFlush(t, d)
			if len(adapter.sent) != 1 {
				t.Fatalf("%dmin: digest at the window edge = %d", tc.minutes, len(adapter.sent))
			}
			if adapter.sent[0].Title != "2 条 P1" ||
				!strings.Contains(adapter.sent[0].Markdown, "账号 A") ||
				!strings.Contains(adapter.sent[0].Markdown, "账号 B") {
				t.Fatalf("%dmin: digest should carry both entries, got %q\n%s", tc.minutes, adapter.sent[0].Title, adapter.sent[0].Markdown)
			}
		})
	}
}

// digestMinutes 在运行中调小：已发出的条目不会因为窗口缩短被重新打包。
func TestDigestShorterWindowDoesNotResendDelivered(t *testing.T) {
	cfg := quietDisabled()
	cfg.DigestMinutes = 30
	d, st, adapter, clock := newDigestHarness(t, cfg, testChannel(SeverityP1))
	mustDispatch(t, d, seedTransition(t, st, p1Event("账号 A", "quota_low", 1, clock.now())))
	tickFor(t, d, clock, 30)
	if len(adapter.sent) != 1 {
		t.Fatalf("first digest missing")
	}
	d.Reload([]Channel{testChannel(SeverityP1)}, func() DispatchConfig { cfg.DigestMinutes = 5; return cfg }(), DefaultRules())
	for i := 0; i < 10; i++ {
		clock.advance(time.Minute)
		mustFlush(t, d)
	}
	if len(adapter.sent) != 1 {
		t.Fatalf("an already delivered item was re-digested: %d", len(adapter.sent))
	}
}

// ⑤ P1 的 resolved 并入下一份摘要，不发单独消息。
func TestDigestResolvedRidesNextDigest(t *testing.T) {
	cfg := quietDisabled()
	cfg.DigestMinutes = 5
	d, st, adapter, clock := newDigestHarness(t, cfg, testChannel(SeverityP1))

	// A 的 firing 在第 5 分钟出摘要，A 仍处于 firing、未恢复。
	fired := seedTransition(t, st, p1Event("账号 A", "quota_low", 1, clock.now()))
	mustDispatch(t, d, fired)
	tickFor(t, d, clock, 5)
	if len(adapter.sent) != 1 {
		t.Fatalf("firing digest missing")
	}

	// 另一个对象的 P1 在第 6 分钟恢复：并入下一份摘要，不单独发。
	clock.advance(time.Minute)
	resolved := testFiringTransition(clock.now(), SeverityP1, 10*time.Minute)
	resolved.Firing = false
	resolved.Resolved = true
	resolved.State.Fingerprint = "provider_down|provider:账号 B"
	resolved.State.RuleKey = "provider_down"
	resolved.State.TargetID = "账号 B"
	resolved.State.TargetName = "账号 B"
	resolved.State.Status = StatusResolved
	resolved.State.FirstSeen = clock.now().Add(-10 * time.Minute)
	resolved.State.LastSeen = clock.now()
	resolved.Event.Fingerprint = resolved.State.Fingerprint
	resolved.Event.RuleKey = "provider_down"
	resolved.Event.TargetName = "账号 B"
	resolved.Event.Status = StatusResolved
	resolved.Event.Time = clock.now()
	resolved = seedTransition(t, st, resolved)
	mustDispatch(t, d, resolved)
	if len(adapter.sent) != 1 {
		t.Fatalf("P1 recovery went out on its own: %d messages", len(adapter.sent))
	}
	if got := st.events[resolved.Event.ID-1].PushStatus; got != PushPending {
		t.Fatalf("recovery push status = %q, want pending", got)
	}
	// 恢复同样等满一个窗口（从第 6 分钟入队起算，第 11 分钟发出）。
	tickFor(t, d, clock, 4)
	if len(adapter.sent) != 1 {
		t.Fatalf("P1 recovery went out before its window closed: %d messages", len(adapter.sent))
	}
	tickFor(t, d, clock, 1)
	if len(adapter.sent) != 2 {
		t.Fatalf("recovery digest missing: sent=%d", len(adapter.sent))
	}
	recovery := adapter.sent[1]
	if recovery.Kind != "digest" || !strings.HasPrefix(recovery.Title, "1 条 P1") {
		t.Fatalf("recovery digest = %+v", recovery)
	}
	if !strings.Contains(recovery.Markdown, "已恢复") || !strings.Contains(recovery.Markdown, "账号 B") {
		t.Fatalf("recovery digest body:\n%s", recovery.Markdown)
	}
	state := st.states[fired.State.Fingerprint]
	if state.LastNotifiedAt.Before(clock.now().Add(-6 * time.Minute)) {
		t.Fatalf("firing digest did not stamp lastNotifiedAt: %v", state.LastNotifiedAt)
	}
}

// 同一窗口内先 firing 再 resolved（M5）：摘要只留「已恢复」；被折叠的 firing
// 事件记 skipped / 同一窗口内已恢复，且不推进该告警的 LastNotifiedAt。
func TestDigestSameWindowFiringThenResolvedCollapses(t *testing.T) {
	cfg := quietDisabled()
	cfg.DigestMinutes = 5
	d, st, adapter, clock := newDigestHarness(t, cfg, testChannel(SeverityP1))
	fired := seedTransition(t, st, p1Event("账号 A", "quota_low", 1, clock.now()))
	mustDispatch(t, d, fired)

	clock.advance(2 * time.Minute)
	resolved := fired
	resolved.Firing = false
	resolved.Resolved = true
	resolved.State.Status = StatusResolved
	resolved.State.FirstSeen = clock.now().Add(-10 * time.Minute)
	resolved.State.LastSeen = clock.now()
	resolved.Event.Status = StatusResolved
	resolved.Event.Time = clock.now()
	resolved = seedTransition(t, st, resolved)
	mustDispatch(t, d, resolved)

	tickFor(t, d, clock, 3)
	if len(adapter.sent) != 1 {
		t.Fatalf("sent = %d", len(adapter.sent))
	}
	body := adapter.sent[0].Markdown
	if strings.Contains(body, "待处理") || !strings.Contains(body, "已恢复") {
		t.Fatalf("collapsed digest body:\n%s", body)
	}
	if body := adapter.sent[0].Title; body != "1 条 P1" {
		t.Fatalf("title = %q, want only the recovery counted", body)
	}

	if got := st.events[fired.Event.ID-1].PushStatus; got != PushSkipped {
		t.Fatalf("collapsed firing push status = %q, want skipped", got)
	}
	if got := st.events[resolved.Event.ID-1].PushStatus; got != PushSent {
		t.Fatalf("recovery push status = %q, want sent", got)
	}
	state := st.states[fired.State.Fingerprint]
	if !state.LastNotifiedAt.IsZero() {
		t.Fatalf("collapsed firing advanced LastNotifiedAt to %v", state.LastNotifiedAt)
	}
}

// 渠道 minSeverity 过滤：只看 P0 的渠道不收 P1 摘要。
func TestDigestRespectsChannelMinSeverity(t *testing.T) {
	d, st, adapter, clock := newDigestHarness(t, quietDisabled(), testChannel(SeverityP0))
	mustDispatch(t, d, seedTransition(t, st, p1Event("账号 A", "quota_low", 1, clock.now())))
	mustFlush(t, d)
	if len(adapter.sent) != 0 || d.digestQueued() != 0 {
		t.Fatalf("P1 digest reached a P0-only channel: sent=%d queued=%d", len(adapter.sent), d.digestQueued())
	}
}

// 渠道在排队期间被停用：窗口到点时条目被标记 skipped 而不是永远 pending。
func TestDigestDropsEntriesWhenChannelDisabled(t *testing.T) {
	d, st, adapter, clock := newDigestHarness(t, quietDisabled(), testChannel(SeverityP1))
	tr := seedTransition(t, st, p1Event("账号 A", "quota_low", 1, clock.now()))
	mustDispatch(t, d, tr)
	channel := testChannel(SeverityP1)
	channel.Enabled = false
	d.Reload([]Channel{channel}, quietDisabled(), DefaultRules())
	tickFor(t, d, clock, 15)
	if len(adapter.sent) != 0 {
		t.Fatalf("disabled channel received a digest")
	}
	if got := st.events[tr.Event.ID-1].PushStatus; got != PushSkipped {
		t.Fatalf("event push status = %q, want skipped", got)
	}
}

// ⑦ 摘要不含 token / key：事件里塞满凭据形状的字段，消息体里一个都不许出现。
func TestDigestCarriesNoSecrets(t *testing.T) {
	channel := Channel{
		ID: "ch1", Type: "feishu", Name: "飞书", Enabled: true, MinSeverity: SeverityP1,
		Config: ChannelConfig{Webhook: "https://open.feishu.cn/hook/wh-secret-123", Secret: "sign-secret-456"},
	}
	d, st, adapter, clock := newDigestHarness(t, quietDisabled(), channel)
	tr := p1Event("账号 A", "auth_invalid", 1, clock.now())
	tr.Event.Detail = map[string]any{
		"apiKey": "sk-live-abc", "token": "tok-xyz", "accessToken": "at-789",
		"secret": "sec-000", "note": "Authorization: Bearer sk-bearer-111", "webhook": "wh-secret-123",
	}
	mustDispatch(t, d, seedTransition(t, st, tr))
	tickFor(t, d, clock, 15)
	if len(adapter.sent) != 1 {
		t.Fatalf("sent = %d", len(adapter.sent))
	}
	body := adapter.sent[0].Title + "\n" + adapter.sent[0].Markdown
	// Detail 里的凭据形状字段按设计的凭据形状擦除；渠道自己的 webhook / secret
	// 由渠道级 scrub 兜底（这里用「对象名里含渠道 webhook 片段」真正触发它）。
	for _, secret := range []string{"sk-live-abc", "tok-xyz", "at-789", "sec-000", "sk-bearer-111", "wh-secret-123", "sign-secret-456"} {
		if strings.Contains(body, secret) {
			t.Fatalf("digest leaked %q:\n%s", secret, body)
		}
	}
	if !strings.Contains(body, "账号 A") {
		t.Fatalf("digest should still carry the account name:\n%s", body)
	}
}

// credentialShapeCases 是凭据擦除的形状表：secret 是完整原文，payload 是要断言
// 「摘要里不出现」的部分（Bearer 前缀会被整段擦掉，按 payload 断言更严）。
var credentialShapeCases = []struct{ name, secret, payload string }{
	{name: "sk", secret: "sk-proj-AbCd1234EfGh", payload: "sk-proj-AbCd1234EfGh"},
	{name: "bearer", secret: "Bearer eyJhbGciOiJIUzI1NiJ9.payload", payload: "eyJhbGciOiJIUzI1NiJ9.payload"},
	{name: "query_access_token", secret: "access_token=ya29.a0AfH6xx", payload: "ya29.a0AfH6xx"},
	{name: "api_key", secret: "api_key=AKIA1234567890", payload: "AKIA1234567890"},
	{name: "slack_bot", secret: "xoxb-123456789012-AbCdEfGhIjKlMn", payload: "123456789012-AbCdEfGhIjKlMn"},
	{name: "slack_oauth", secret: "xoxo-987654321098-ZyXwVuTsRqPoNm", payload: "987654321098-ZyXwVuTsRqPoNm"},
	{name: "github_pat", secret: "ghp_ABCDEFGHIJKLMNOPQRSTUVWX", payload: "ABCDEFGHIJKLMNOPQRSTUVWX"},
	{name: "github_oauth", secret: "gho_ABCDEFGHIJKLMNOPQRSTUVWX", payload: "ABCDEFGHIJKLMNOPQRSTUVWX"},
	{name: "github_fine", secret: "github_pat_11ABCDEFG0123456789_abcdef", payload: "11ABCDEFG0123456789_abcdef"},
	{name: "gitlab", secret: "glpat-AbCdEfGhIjKlMnOpQrSt", payload: "AbCdEfGhIjKlMnOpQrSt"},
	{name: "url_key", secret: "https://open.feishu.cn/hook/1?key=wh-1234567890", payload: "wh-1234567890"},
	{name: "url_t", secret: "https://example.com/api?v=1&t=abcdef123456", payload: "abcdef123456"},
	{name: "url_sign", secret: "https://example.com/api?sign=Zm9vYmFyYmF6", payload: "Zm9vYmFyYmF6"},
	{name: "url_signature", secret: "https://example.com/api?signature=deadbeefcafe", payload: "deadbeefcafe"},
	{name: "url_secret", secret: "https://example.com/api?secret=s3cr3tValue", payload: "s3cr3tValue"},
	{name: "url_token", secret: "https://example.com/api?token=tok-998877", payload: "tok-998877"},
}

// scrubCredentialShapes 本身的表驱动：直接测函数，覆盖幂等与中文名不被破坏。
func TestScrubCredentialShapesTable(t *testing.T) {
	for _, tc := range credentialShapeCases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrubCredentialShapes("账号 " + tc.secret)
			if strings.Contains(got, tc.payload) || strings.Contains(got, tc.secret) {
				t.Fatalf("scrub left %q in %q", tc.payload, got)
			}
			if !strings.Contains(got, "账号 ") {
				t.Fatalf("scrub damaged the name: %q", got)
			}
			// 幂等：擦过的串再擦一遍不变。
			again := scrubCredentialShapes(got)
			if again != got {
				t.Fatalf("not idempotent: %q → %q", got, again)
			}
			if three := scrubCredentialShapes(again); three != got {
				t.Fatalf("not idempotent on third pass: %q → %q", got, three)
			}
		})
	}
	// 正常中文名、规则名、含 URL 但不带敏感参数的名字都不该被改写。
	for _, clean := range []string{
		"账号 A", "上游 1 号（备用）", "Claude Pro 订阅账号", "节点-东京-02",
		"https://example.com/api?v=1&page=2", "sk-short", "xox", "gh short",
		"我的key是机密", "签到 3 天",
	} {
		if got := scrubCredentialShapes(clean); got != clean {
			t.Fatalf("normal name %q was rewritten to %q", clean, got)
		}
	}
}

// M4：对象名是用户自由输入，可能被贴成密钥。State.TargetName 与 Event.TargetName
// 两条路径分别注入形状表里的每一种凭据，摘要正文都不许出现原文。
func TestDigestScrubsCredentialShapesInTargetName(t *testing.T) {
	sources := []struct {
		field  string
		assign func(tr *Transition, leak string)
	}{
		{field: "state", assign: func(tr *Transition, leak string) {
			tr.State.TargetName = "账号 " + leak
		}},
		// event 路径：摘要优先取 State.TargetName，所以这里把它留空，强迫走
		// Event.TargetName（与生产里 engine 的 fall-through 一致）。
		{field: "event", assign: func(tr *Transition, leak string) {
			tr.State.TargetName = ""
			tr.Event.TargetName = "账号 " + leak
		}},
	}
	for _, source := range sources {
		for _, shape := range credentialShapeCases {
			t.Run(source.field+"/"+shape.name, func(t *testing.T) {
				cfg := quietDisabled()
				cfg.DigestMinutes = 5
				d, st, adapter, clock := newDigestHarness(t, cfg, testChannel(SeverityP1))
				tr := p1Event("placeholder", "quota_low", 1, clock.now())
				source.assign(&tr, shape.secret)
				mustDispatch(t, d, seedTransition(t, st, tr))
				tickFor(t, d, clock, 5)
				if len(adapter.sent) != 1 {
					t.Fatalf("sent = %d", len(adapter.sent))
				}
				body := adapter.sent[0].Title + "\n" + adapter.sent[0].Markdown
				if strings.Contains(body, shape.secret) || strings.Contains(body, shape.payload) {
					t.Fatalf("digest leaked %q via %s.TargetName:\n%s", shape.payload, source.field, body)
				}
				if !strings.Contains(body, "***") || !strings.Contains(body, "账号") {
					t.Fatalf("digest did not scrub %s.TargetName (or lost the name):\n%s", source.field, body)
				}
			})
		}
	}
}

// 重启安全：新进程（新 Dispatcher）不会因为旧队列而重放历史 P1。
func TestDigestRestartDoesNotReplay(t *testing.T) {
	cfg := quietDisabled()
	d, st, adapter, clock := newDigestHarness(t, cfg, testChannel(SeverityP1))
	mustDispatch(t, d, seedTransition(t, st, p1Event("账号 A", "quota_low", 1, clock.now())))
	tickFor(t, d, clock, 15)
	if len(adapter.sent) != 1 {
		t.Fatalf("first digest missing")
	}

	// 进程重启：同一份存储、同样的时钟与渠道，但 dispatcher 全新。
	restarted := newDispatcherForTest(st, []Channel{testChannel(SeverityP1)}, cfg, DefaultRules())
	restarted.now = clock.now
	restarted.adapters["feishu"] = adapter
	clock.advance(15 * time.Minute)
	mustFlush(t, restarted)
	if len(adapter.sent) != 1 {
		t.Fatalf("restart replayed a delivered P1: %d messages", len(adapter.sent))
	}
}
