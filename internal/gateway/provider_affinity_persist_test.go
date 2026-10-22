package gateway

import (
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	storepkg "github.com/luca/llm-protocol-gateway/internal/store"
)

// memoryPinSaver is a sessionPinSaver that keeps the rows in a map.
type memoryPinSaver struct {
	mu      sync.Mutex
	rows    map[string]string // session key → "provider|expiresAt"
	failOn  bool
	saves   int
	deletes int
}

func newMemoryPinSaver() *memoryPinSaver {
	return &memoryPinSaver{rows: map[string]string{}}
}

func (m *memoryPinSaver) SaveSessionPins(pins []storepkg.SessionPin) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	if m.failOn {
		return errPinStoreDown
	}
	for _, pin := range pins {
		m.rows[pin.SessionKey] = pin.ProviderID + "|" + pin.ExpiresAt
	}
	return nil
}

func (m *memoryPinSaver) DeleteSessionPins(keys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes++
	if m.failOn {
		return errPinStoreDown
	}
	for _, key := range keys {
		delete(m.rows, key)
	}
	return nil
}

func (m *memoryPinSaver) LoadSessionPins() ([]storepkg.SessionPin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.rows))
	for key := range m.rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]storepkg.SessionPin, 0, len(keys))
	for _, key := range keys {
		providerID, expiresAt, _ := strings.Cut(m.rows[key], "|")
		out = append(out, storepkg.SessionPin{SessionKey: key, ProviderID: providerID, ExpiresAt: expiresAt})
	}
	return out, nil
}

var errPinStoreDown = errPinStoreUnavailable{}

type errPinStoreUnavailable struct{}

func (errPinStoreUnavailable) Error() string { return "pin store unavailable" }

// A pin written before a restart must come back: the conversation's next turn
// has to land on the account that already cached its prompt, or the whole
// prompt is billed again as fresh input.
func TestSessionPinsSurviveRestart(t *testing.T) {
	saver := newMemoryPinSaver()
	first := newSessionPinStore()
	first.nowFunc = func() time.Time { return time.Unix(1_700_000_000, 0) }
	first.setSaver(saver)
	first.set("session-a", "provider-a")
	first.set("session-b", "provider-b")
	first.flush()

	second := newSessionPinStore()
	second.nowFunc = func() time.Time { return time.Unix(1_700_000_000, 0).Add(time.Minute) }
	second.setSaver(saver)
	if got := second.sessionPin("session-a"); got != "provider-a" {
		t.Fatalf("session-a pin after restart = %q, want provider-a", got)
	}
	if got := second.sessionPin("session-b"); got != "provider-b" {
		t.Fatalf("session-b pin after restart = %q, want provider-b", got)
	}
}

// An expired pin is not resurrected by a restart.
func TestSessionPinsExpiredAreNotRestored(t *testing.T) {
	saver := newMemoryPinSaver()
	first := newSessionPinStore()
	first.nowFunc = func() time.Time { return time.Unix(1_700_000_000, 0) }
	first.setSaver(saver)
	first.set("session-old", "provider-a")
	first.flush()

	second := newSessionPinStore()
	second.nowFunc = func() time.Time { return time.Unix(1_700_000_000, 0).Add(sessionAffinityTTL + time.Minute) }
	second.setSaver(saver)
	if got := second.sessionPin("session-old"); got != "" {
		t.Fatalf("an expired pin must not be restored, got %q", got)
	}
}

// Forgetting a pin (failover to another account, or an unusable provider) must
// remove the row too, or the next restart pins the conversation back onto the
// account that already failed.
func TestSessionPinClearedIsRemovedFromStore(t *testing.T) {
	saver := newMemoryPinSaver()
	store := newSessionPinStore()
	store.setSaver(saver)
	store.set("session-a", "provider-a")
	store.flush()
	store.clear("session-a")
	store.flush()
	if len(saver.rows) != 0 {
		t.Fatalf("cleared pin still stored: %v", saver.rows)
	}
}

// A store that is down must not lose the pin: the failure puts the key back on
// the dirty list and the next write catches up.
func TestSessionPinFlushRetriesAfterFailure(t *testing.T) {
	saver := newMemoryPinSaver()
	saver.failOn = true
	store := newSessionPinStore()
	store.setSaver(saver)
	store.set("session-a", "provider-a")
	store.flush()
	if len(saver.rows) != 0 {
		t.Fatalf("a failing save must not write rows: %v", saver.rows)
	}
	saver.mu.Lock()
	saver.failOn = false
	saver.mu.Unlock()
	store.flushMu.Lock()
	store.mu.Lock()
	pending := len(store.dirty)
	store.mu.Unlock()
	store.flushMu.Unlock()
	if pending == 0 {
		t.Fatal("a failed flush must keep the pin pending")
	}
	store.flush()
	if len(saver.rows) != 1 {
		t.Fatalf("the retried flush should have written the pin, got %v", saver.rows)
	}
}

// Without a saver the store is the plain memory table it always was, and a
// conversation still stays on its provider.
func TestSessionPinWithoutSaverStaysInMemory(t *testing.T) {
	store := newSessionPinStore()
	store.set("session-a", "provider-a")
	if got := store.sessionPin("session-a"); got != "provider-a" {
		t.Fatalf("pin = %q", got)
	}
	store.clear("session-a")
	if got := store.sessionPin("session-a"); got != "" {
		t.Fatalf("cleared pin = %q", got)
	}
}
