package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	storepkg "github.com/luca/llm-protocol-gateway/internal/store"
)

const (
	// sessionAffinityTTL is how long a conversation stays on the provider that
	// answered its last turn. Sliding: every successful turn renews it. It is
	// deliberately shorter than a vendor's longest cache window, and a session
	// that goes quiet simply loses its pin — the key's sticky provider takes
	// over again.
	sessionAffinityTTL = 30 * time.Minute
	// sessionAffinityMaxPins bounds the table: a gateway shared by many agents
	// creates a session id per conversation, and an unbounded map would grow
	// with the number of past conversations.
	sessionAffinityMaxPins = 10000
	// sessionPinFlushDelay is how long a changed pin waits before it is written:
	// pins change on every successful turn, and the delay turns a busy gateway's
	// thousands of turns into a handful of small writes.
	sessionPinFlushDelay = 500 * time.Millisecond
)

type sessionPin struct {
	providerID string
	expiresAt  time.Time
}

// sessionPinStore remembers which provider a conversation is sticky to.
//
// The key's ActiveProviderID is global to the key, so one key driven by several
// agents (or one agent with several concurrent conversations) moves all of them
// whenever any single request fails over — and every move throws away the
// prompt cache that upstream account built for that conversation. The pin is
// per session: a conversation keeps the account that has it cached, while other
// conversations on the same key are free to use other accounts.
type sessionPinStore struct {
	mu      sync.Mutex
	pins    map[string]sessionPin
	ttl     time.Duration
	max     int
	nowFunc func() time.Time
	// dirty holds the keys whose pin changed since the last write, and saver is
	// where they are written; without a saver the store is a plain memory table.
	dirty        map[string]bool
	saver        sessionPinSaver
	flushPending bool
	// flushMu serializes writes so two flushes never interleave their rows.
	flushMu sync.Mutex
}

func newSessionPinStore() *sessionPinStore {
	return &sessionPinStore{
		pins:  make(map[string]sessionPin),
		ttl:   sessionAffinityTTL,
		max:   sessionAffinityMaxPins,
		dirty: make(map[string]bool),
	}
}

// sessionPinSaver is where pins live between restarts. It is optional: a server
// without one keeps the table in memory only.
type sessionPinSaver interface {
	SaveSessionPins([]storepkg.SessionPin) error
	DeleteSessionPins([]string) error
	LoadSessionPins() ([]storepkg.SessionPin, error)
}

// setSaver attaches the persistent half and loads what it remembered. It runs
// before the server serves anything, so a restart puts every live conversation
// back on the account that already has its prompt cached.
func (store *sessionPinStore) setSaver(saver sessionPinSaver) {
	if store == nil || saver == nil {
		return
	}
	pins, err := saver.LoadSessionPins()
	if err != nil {
		slog.Warn("session pins could not be loaded", "error", err)
	}
	now := store.now()
	store.mu.Lock()
	defer store.mu.Unlock()
	store.saver = saver
	for _, pin := range pins {
		expiresAt, err := time.Parse(time.RFC3339, pin.ExpiresAt)
		if err != nil || !now.Before(expiresAt) {
			continue
		}
		store.pins[pin.SessionKey] = sessionPin{providerID: pin.ProviderID, expiresAt: expiresAt}
	}
}

// scheduleFlushLocked marks a pin changed and arranges the write. Called with mu
// held.
func (store *sessionPinStore) scheduleFlushLocked(sessionKey string) {
	if store.saver == nil {
		return
	}
	store.dirty[sessionKey] = true
	if store.flushPending {
		return
	}
	store.flushPending = true
	time.AfterFunc(sessionPinFlushDelay, store.flush)
}

// flush writes every changed pin. A failure puts the keys back on the dirty list:
// the table already holds the right answer, so a failed write only costs the pins
// on the next restart.
func (store *sessionPinStore) flush() {
	if store == nil {
		return
	}
	store.flushMu.Lock()
	defer store.flushMu.Unlock()
	store.mu.Lock()
	store.flushPending = false
	if len(store.dirty) == 0 || store.saver == nil {
		store.mu.Unlock()
		return
	}
	now := store.now()
	upserts := make([]storepkg.SessionPin, 0, len(store.dirty))
	deletes := make([]string, 0)
	keys := make([]string, 0, len(store.dirty))
	for key := range store.dirty {
		keys = append(keys, key)
		pin, ok := store.pins[key]
		if ok && now.Before(pin.expiresAt) {
			upserts = append(upserts, storepkg.SessionPin{SessionKey: key, ProviderID: pin.providerID, ExpiresAt: pin.expiresAt.UTC().Format(time.RFC3339)})
		} else {
			deletes = append(deletes, key)
		}
	}
	store.dirty = make(map[string]bool)
	saver := store.saver
	store.mu.Unlock()

	if err := saver.SaveSessionPins(upserts); err != nil {
		slog.Warn("session pins could not be saved", "error", err)
		store.markDirty(keys)
		return
	}
	if err := saver.DeleteSessionPins(deletes); err != nil {
		slog.Warn("session pins could not be cleared", "error", err)
		store.markDirty(keys)
	}
}

func (store *sessionPinStore) markDirty(keys []string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.saver == nil {
		return
	}
	for _, key := range keys {
		store.dirty[key] = true
	}
}

func (s *Server) pinStore() *sessionPinStore { return s.sessionPins }

func (s *Server) setSessionPin(sessionKey, providerID string) {
	s.pinStore().set(sessionKey, providerID)
}

func (s *Server) clearSessionPin(sessionKey string) {
	s.pinStore().clear(sessionKey)
}

// applySessionPin moves the conversation's pinned provider to the front of the
// attempt order. It returns "" when the pin does not name a provider this
// request may use — deleted, disabled, revoked from the key's owner, or a
// cross-protocol backup with no locked model — and drops the pin in that case,
// so the next turn chooses again instead of being stuck on an unusable account.
func (s *Server) applySessionPin(sessionKey string, attempts []string) ([]string, string) {
	if sessionKey == "" {
		return attempts, ""
	}
	pinned := s.pinStore().sessionPin(sessionKey)
	if pinned == "" {
		return attempts, ""
	}
	for _, providerID := range attempts {
		if providerID == pinned {
			return moveProviderToFront(attempts, pinned), pinned
		}
	}
	s.clearSessionPin(sessionKey)
	return attempts, ""
}

// sessionPin returns the provider a session is pinned to, "" when there is no
// live pin. Expired pins are dropped on read.
func (store *sessionPinStore) sessionPin(sessionKey string) string {
	if store == nil || sessionKey == "" {
		return ""
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	pin, ok := store.pins[sessionKey]
	if !ok {
		return ""
	}
	if !store.now().After(pin.expiresAt) {
		return pin.providerID
	}
	delete(store.pins, sessionKey)
	store.scheduleFlushLocked(sessionKey)
	return ""
}

func (store *sessionPinStore) set(sessionKey, providerID string) {
	if store == nil || sessionKey == "" || providerID == "" {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.pins[sessionKey] = sessionPin{providerID: providerID, expiresAt: store.now().Add(store.ttlOrDefault())}
	store.scheduleFlushLocked(sessionKey)
	if len(store.pins) <= store.maxOrDefault() {
		return
	}
	// Past the cap, drop what has already expired; if that is not enough, evict
	// the pin that would expire soonest.
	now := store.now()
	for key, pin := range store.pins {
		if now.After(pin.expiresAt) {
			delete(store.pins, key)
			store.scheduleFlushLocked(key)
		}
	}
	for len(store.pins) > store.maxOrDefault() {
		oldestKey, oldest := "", sessionPin{}
		for key, pin := range store.pins {
			if oldestKey == "" || pin.expiresAt.Before(oldest.expiresAt) {
				oldestKey, oldest = key, pin
			}
		}
		if oldestKey == "" {
			break
		}
		delete(store.pins, oldestKey)
		store.scheduleFlushLocked(oldestKey)
	}
}

func (store *sessionPinStore) clear(sessionKey string) {
	if store == nil || sessionKey == "" {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.pins[sessionKey]; !ok {
		return
	}
	delete(store.pins, sessionKey)
	store.scheduleFlushLocked(sessionKey)
}

func (store *sessionPinStore) now() time.Time {
	if store.nowFunc != nil {
		return store.nowFunc()
	}
	return time.Now()
}

func (store *sessionPinStore) ttlOrDefault() time.Duration {
	if store.ttl > 0 {
		return store.ttl
	}
	return sessionAffinityTTL
}

func (store *sessionPinStore) maxOrDefault() int {
	if store.max > 0 {
		return store.max
	}
	return sessionAffinityMaxPins
}

// sessionAffinityKey is the pin table's key: the conversation's own session id
// scoped to the gateway key that sent it, so two clients of one key that happen
// to use the same session string never share a pin. Empty means the request
// carries no conversation identity and stays on the key-level stickiness.
func sessionAffinityKey(r *http.Request, req map[string]any, keyID string) string {
	session := requestSessionID(r, req)
	if session == "" || strings.TrimSpace(keyID) == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(keyID) + "\x00" + session))
	return hex.EncodeToString(digest[:16])
}

// requestSessionID is how a conversation names itself. It reads the same
// signals the request log uses (headers first, then the body fields agents
// actually send) plus prompt_cache_key, which is what Codex/ChatGPT route their
// prompt cache by. A bare metadata.user_id is never a session: Claude Code puts
// several conversations under one user id, and merging them into a single pin
// would move them all together.
func requestSessionID(r *http.Request, req map[string]any) string {
	if r != nil {
		for _, header := range []string{"Session_id", "X-Session-Id", "X-Claude-Code-Session-Id"} {
			if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
				return value
			}
		}
	}
	if req == nil {
		return ""
	}
	if value, ok := req["session_id"].(string); ok {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	if meta, ok := req["metadata"].(map[string]any); ok {
		if value, ok := meta["session_id"].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
		if userID, ok := meta["user_id"].(string); ok {
			if index := strings.LastIndex(userID, "_session_"); index >= 0 {
				if tail := strings.TrimSpace(userID[index+len("_session_"):]); tail != "" {
					return tail
				}
			}
		}
	}
	if value, ok := req["prompt_cache_key"].(string); ok {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// releaseSessionPin forgets a conversation's pin when the provider it pointed
// at is the one that just failed over, so the next turn is free to pick a live
// account instead of following a dead one.
func (s *Server) releaseSessionPin(sessionKey, pinnedProviderID, failedProviderID string) {
	if sessionKey != "" && pinnedProviderID != "" && pinnedProviderID == failedProviderID {
		s.clearSessionPin(sessionKey)
	}
}

// moveProviderToFront puts providerID first when it is in the list and leaves
// the list untouched when it is not (the pin may point at a provider the key
// has lost access to, or one that was deleted).
func moveProviderToFront(chain []string, providerID string) []string {
	if providerID == "" {
		return chain
	}
	index := -1
	for i, id := range chain {
		if id == providerID {
			index = i
			break
		}
	}
	if index <= 0 {
		return chain
	}
	out := make([]string, 0, len(chain))
	out = append(out, providerID)
	out = append(out, chain[:index]...)
	return append(out, chain[index+1:]...)
}
