package gateway

import (
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
func (t *authThrottle) blocked(client, user string) (bool, time.Duration) {
	if t == nil {
		return false, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	entry, ok := t.entries[authFailureKey{client: client, user: user}]
	if !ok || now.Sub(entry.windowStart) >= authFailureWindow || entry.count < authMaxFailures {
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
func (s *Server) rejectIfAuthThrottled(w http.ResponseWriter, r *http.Request, username string) bool {
	blocked, wait := s.authThrottle.blocked(requestClientIP(r), authThrottleUser(username))
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

// recordAuthFailure 记一次口令失败（供后续限流判断）。
func (s *Server) recordAuthFailure(r *http.Request, username string) {
	if s.authThrottle.fail(requestClientIP(r), authThrottleUser(username)) {
		s.logs.AddApp("warn", "auth throttled after repeated failures", requestClientIP(r))
	}
}

// recordAuthSuccess 清空该来源+账号的失败计数。
func (s *Server) recordAuthSuccess(r *http.Request, username string) {
	s.authThrottle.succeeded(requestClientIP(r), authThrottleUser(username))
}
