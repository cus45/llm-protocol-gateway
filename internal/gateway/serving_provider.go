package gateway

import (
	"strings"
	"sync"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// servingProviderTracker remembers, per API key, which provider last answered a
// request for it.
//
// Why this exists next to domain.APIKey.ActiveProviderID: that field means
// "where this key's sticky position is" and is written on every successful
// turn — except when a session pin chose the provider, because several
// conversations on one key would otherwise rewrite it on every turn and make
// the key flap. The console reads the same field to draw the green "在服务"
// dot, so a key whose traffic is pinned to a fallback kept showing the
// preferred provider as serving. The tracker is the runtime truth the console
// needs: the last provider that actually returned a response.
//
// Writes are debounced per key: a fast stream of successes on different
// accounts (round-robin, pinned conversations interleaved) only moves the
// marker once it has stayed put for servingProviderSettleDelay, so the dot
// does not strobe between accounts mid-flight. The value lives in memory on
// purpose — after a restart the first request repopulates it, and a stale
// marker must never outlive the process.
const servingProviderSettleDelay = 3 * time.Second

type servingMark struct {
	// providerID 是最近一笔响应的节点（可能还在等 settle）。
	providerID string
	// effective 是控制台当前展示的节点，只有它决定绿点。
	effective string
	timer     *time.Timer
}

type servingProviderTracker struct {
	mu    sync.Mutex
	marks map[string]*servingMark
}

func newServingProviderTracker() *servingProviderTracker {
	return &servingProviderTracker{marks: map[string]*servingMark{}}
}

// note records that providerID answered keyID. The first answer for a key is
// applied immediately (opening the page right after the first request already
// shows the right node); later changes take effect after the settle delay, so
// two interleaved conversations do not make the console dot strobe.
func (tracker *servingProviderTracker) note(keyID, providerID string, delay time.Duration) {
	keyID = strings.TrimSpace(keyID)
	providerID = strings.TrimSpace(providerID)
	if tracker == nil || keyID == "" || providerID == "" {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	mark, ok := tracker.marks[keyID]
	if !ok {
		tracker.marks[keyID] = &servingMark{providerID: providerID, effective: providerID}
		return
	}
	if mark.providerID != providerID {
		mark.providerID = providerID
		if mark.timer != nil {
			mark.timer.Stop()
			mark.timer = nil
		}
	}
	if mark.effective == providerID {
		return
	}
	if delay <= 0 {
		mark.effective = providerID
		return
	}
	// 每次响应都把 settle 计时推到 delay 之后：只有这个节点连续 keep 住这么久
	// （中间没被别的节点顶替）才真正成为展示值。
	target := providerID
	mark.timer = time.AfterFunc(delay, func() {
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		current, ok := tracker.marks[keyID]
		if !ok || current.providerID != target || current.effective == target {
			return
		}
		current.effective = target
		current.timer = nil
	})
}

// serving returns the provider the console should crown for keyID, "" when
// nothing has been recorded (no request yet since the process started).
func (tracker *servingProviderTracker) serving(keyID string) string {
	if tracker == nil {
		return ""
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if mark, ok := tracker.marks[keyID]; ok {
		return mark.effective
	}
	return ""
}

func (s *Server) noteKeyServingProvider(keyID, providerID string) {
	if s == nil || s.servingProviders == nil {
		return
	}
	s.servingProviders.note(keyID, providerID, servingProviderSettleDelay)
}

// servingProviderFor returns the provider currently answering for keyID. When
// no request has been served yet since start-up it falls back to the key's
// sticky provider (the previous meaning of the console dot), so a freshly
// restarted gateway does not blank every key.
func (s *Server) servingProviderFor(key domain.APIKey) string {
	if s != nil && s.servingProviders != nil {
		if providerID := s.servingProviders.serving(key.ID); providerID != "" {
			return providerID
		}
	}
	return strings.TrimSpace(key.ActiveProviderID)
}
