package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 登录/初始化口令是网关唯一可被离线穷举的入口（管理员口令最短 8 位）。这里按
// 来源 IP + 账号名做滑动窗口限流：窗口内失败次数到阈值后一律 429，成功登录
// 清空该组合的计数。只统计失败，正常用户连续登录不受影响。
const (
	authFailureWindow   = 10 * time.Minute
	authMaxFailures     = 8
	authThrottleSweepAt = 1024
	// authLoginBodyPeekLimit 是登录限流预读正文的上限：一个登录请求体远小于此，
	// 超出的部分原样留在 r.Body 里交给 Decode。
	authLoginBodyPeekLimit = 64 << 10
)

type authFailureKey struct {
	client string
	user   string
}

type authFailureEntry struct {
	windowStart time.Time
	count       int
}

// authThrottle 是进程内滑动窗口失败计数。Map 里的条目在窗口过期后被清扫，
// 不会被无限增长的用户名撑爆。
type authThrottle struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[authFailureKey]authFailureEntry
}

func newAuthThrottle() *authThrottle {
	return &authThrottle{now: time.Now, entries: map[authFailureKey]authFailureEntry{}}
}

// blocked 报告该来源+账号当前是否已被限流，以及还要等多久。
func (t *authThrottle) blocked(client, user string, limit int) (bool, time.Duration) {
	if t == nil {
		return false, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	entry, ok := t.entries[authFailureKey{client: client, user: user}]
	if !ok || now.Sub(entry.windowStart) >= authFailureWindow || entry.count < limit {
		return false, 0
	}
	return true, authFailureWindow - now.Sub(entry.windowStart)
}

// fail 记一次失败；返回是否因此刚触发限流。
func (t *authThrottle) fail(client, user string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if len(t.entries) >= authThrottleSweepAt {
		for key, entry := range t.entries {
			if now.Sub(entry.windowStart) >= authFailureWindow {
				delete(t.entries, key)
			}
		}
	}
	key := authFailureKey{client: client, user: user}
	entry, ok := t.entries[key]
	if !ok || now.Sub(entry.windowStart) >= authFailureWindow {
		entry = authFailureEntry{windowStart: now}
	}
	entry.count++
	t.entries[key] = entry
	return entry.count == authMaxFailures
}

// succeeded 清空该组合的失败计数：口令对了就不该再被此前的失败拖累。
func (t *authThrottle) succeeded(client, user string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, authFailureKey{client: client, user: user})
}

// authThrottleUser 归一账号名，空账号与 admin 走同一个计数桶。
func authThrottleUser(username string) string {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return "admin"
	}
	return username
}

// rejectIfAuthThrottled 在口令校验前限流；命中时写 429 并返回 true。
// blocked reports whether this source, or this account for any source, has
// failed too often recently, and how long the block lasts.
func (s *Server) blocked(r *http.Request, username string) (bool, time.Duration) {
	source, user := authThrottleSource(r), authThrottleUser(username)
	// 来源维度只看来源（与用户名无关）：换账号名绕不过已经失败很多次的来源。
	if blocked, wait := s.authThrottle.blocked(source, authThrottleAnyUserKey, authMaxFailures); blocked {
		return true, wait
	}
	// 账号维度跨来源累计，所以阈值更高：单台机器靠来源维度本来就挡住了，
	// 这一层是给「同一账号从很多来源试」和「换账号绕过来源」两头兜底。
	return s.authThrottle.blocked(authThrottleAccountKey, user, authAccountMaxFailures)
}

func (s *Server) rejectIfAuthThrottled(w http.ResponseWriter, r *http.Request, username string) bool {
	blocked, wait := s.blocked(r, username)
	if !blocked {
		return false
	}
	retry := int(wait.Seconds())
	if retry < 1 {
		retry = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	writeOpenAIError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
	return true
}

// rejectIfAuthThrottledPeek 只看请求体里的 username 决定挡不挡，不读正文。
// 登录在解析 JSON 之前就要限流（被挡下的来源不该再消耗一次解析），用一个只认
// 字符串值的轻量扫描取账号名：取不到就按 admin 桶处理，最多是多挡一层，不会漏。
func (s *Server) rejectIfAuthThrottledPeek(w http.ResponseWriter, r *http.Request) bool {
	return s.rejectIfAuthThrottled(w, r, peekUsername(r))
}

// peekUsername 只读请求头，从请求体 JSON 里认出 username 字符串；正文原样留给
// 后续的 Decode。它只是限流用的提示，格式不合法的正文照样交给 Decode 去报错。
func peekUsername(r *http.Request) string {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, authLoginBodyPeekLimit))
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), r.Body))
	if r.GetBody != nil && int64(len(raw)) < authLoginBodyPeekLimit {
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(raw)), nil
		}
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return ""
		}
		key, ok := keyTok.(string)
		if !ok {
			return ""
		}
		if key != "username" {
			if err := dec.Decode(new(json.RawMessage)); err != nil {
				return ""
			}
			continue
		}
		var username string
		if err := dec.Decode(&username); err != nil {
			return ""
		}
		return strings.TrimSpace(username)
	}
	return ""
}

// recordAuthFailure 记一次口令失败（供后续限流判断）。
func (s *Server) recordAuthFailure(r *http.Request, username string) {
	source, user := authThrottleSource(r), authThrottleUser(username)
	// 来源维度与账号维度各记一次：换来源绕不过账号，换账号绕不过来源。
	s.authThrottle.fail(source, authThrottleAnyUserKey)
	if s.authThrottle.fail(authThrottleAccountKey, user) {
		s.logs.AddApp("warn", "auth throttled after repeated failures", source)
	}
}

// recordAuthSuccess 清空该来源+账号的失败计数。
func (s *Server) recordAuthSuccess(r *http.Request, username string) {
	source, user := authThrottleSource(r), authThrottleUser(username)
	s.authThrottle.succeeded(source, authThrottleAnyUserKey)
	s.authThrottle.succeeded(authThrottleAccountKey, user)
}
