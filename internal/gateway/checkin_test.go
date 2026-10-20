package gateway

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

// memoryCheckinStore is the check-in table as a map, plus the StateSaver the
// gateway needs before it will hand out a checkinStore at all.
type memoryCheckinStore struct {
	mu       sync.Mutex
	records  map[string]domain.CheckinRecord
	saves    int
	state    domain.GatewayState
	failSave error
}

func newMemoryCheckinStore() *memoryCheckinStore {
	return &memoryCheckinStore{records: map[string]domain.CheckinRecord{}}
}

func (store *memoryCheckinStore) Save(state domain.GatewayState) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.saves++
	store.state = state
	return nil
}

func (store *memoryCheckinStore) SaveCheckin(record domain.CheckinRecord) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failSave != nil {
		return store.failSave
	}
	store.records[record.ProviderID] = record
	return nil
}

func (store *memoryCheckinStore) ListCheckins() ([]domain.CheckinRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	out := []domain.CheckinRecord{}
	for _, record := range store.records {
		out = append(out, record)
	}
	return out, nil
}

func (store *memoryCheckinStore) CheckinByProvider(providerID string) (domain.CheckinRecord, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, ok := store.records[providerID]
	return record, ok, nil
}

func checkinTestServer(providers ...domain.Provider) (*Server, *memoryCheckinStore) {
	store := newMemoryCheckinStore()
	router := NewRouter(domain.GatewayState{Providers: providers})
	return NewServer(router, monitor.NewStore(), store), store
}

func TestCheckinDayIsTheBeijingDay(t *testing.T) {
	// 16:00 UTC is midnight in Beijing: the day turns there, not at 00:00 UTC,
	// which is what makes a gateway on a US VPS and its user agree.
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"just before the turn", time.Date(2026, 10, 19, 15, 59, 59, 0, time.UTC), "2026-10-19"},
		{"at the turn", time.Date(2026, 10, 19, 16, 0, 0, 0, time.UTC), "2026-10-20"},
		{"an hour after", time.Date(2026, 10, 19, 17, 0, 0, 0, time.UTC), "2026-10-20"},
		{"already read as Beijing", time.Date(2026, 10, 20, 0, 30, 0, 0, beijingZone), "2026-10-20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkinDay(tc.at); got != tc.want {
				t.Fatalf("checkinDay(%s) = %q, want %q", tc.at, got, tc.want)
			}
		})
	}
}

func TestCheckinVendorOfOnlyListsVendorsWithAnEvent(t *testing.T) {
	cases := []struct {
		authType  string
		want      string
		supported bool
	}{
		{domain.AuthTypeQoderPAT, checkinVendorQoder, true},
		{domain.AuthTypeQoderCN, checkinVendorQoder, true},
		{domain.AuthTypeWorkBuddy, checkinVendorWorkBuddy, true},
		// WorkBuddy AI ships without the event, and every other subscription has
		// no daily grant to press — hence no button on its card.
		{domain.AuthTypeWorkBuddyAI, "", false},
		{domain.AuthTypeClaudeOAuth, "", false},
		{domain.AuthTypeChatGPTOAuth, "", false},
		{domain.AuthTypeTraeCN, "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%v", tc.authType), func(t *testing.T) {
			vendor, ok := checkinVendorOf(domain.Provider{AuthType: tc.authType})
			if ok != tc.supported || vendor != tc.want {
				t.Fatalf("checkinVendorOf(%q) = (%q, %v), want (%q, %v)", tc.authType, vendor, ok, tc.want, tc.supported)
			}
		})
	}
}

func TestCheckinUsableNeedsASignedInAccount(t *testing.T) {
	connected := domain.Provider{ID: "q1", AuthType: domain.AuthTypeQoderCN, QoderPAT: &domain.QoderCredential{UID: "u"}}
	if !checkinUsable(connected) {
		t.Fatal("a signed-in Qoder account must be usable")
	}
	paused := connected
	paused.QoderPAT = &domain.QoderCredential{UID: "u", Disconnected: true}
	if checkinUsable(paused) {
		t.Fatal("a paused account must not be checked in")
	}
	if checkinUsable(domain.Provider{ID: "q2", AuthType: domain.AuthTypeQoderCN}) {
		t.Fatal("an account that never signed in must not be checked in")
	}
	if checkinUsable(domain.Provider{ID: "c1", AuthType: domain.AuthTypeClaudeOAuth, ClaudeOAuth: &domain.ClaudeOAuthCredential{}}) {
		t.Fatal("a vendor with no event is never usable")
	}
}

func TestCheckinSettled(t *testing.T) {
	now := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC) // 11:00 Beijing, day 2026-10-20
	day := checkinDay(now)
	stamp := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }
	record := func(mutate func(*domain.CheckinRecord)) domain.CheckinRecord {
		item := domain.CheckinRecord{ProviderID: "p", AccountKey: "acct", Day: day, At: stamp(time.Minute), Outcome: domain.CheckinDone}
		mutate(&item)
		return item
	}
	cases := []struct {
		name   string
		record domain.CheckinRecord
		vendor string
		key    string
		soon   bool
		want   bool
	}{
		{"done today", record(func(*domain.CheckinRecord) {}), checkinVendorWorkBuddy, "acct", false, true},
		{"a press re-reads", record(func(*domain.CheckinRecord) {}), checkinVendorWorkBuddy, "acct", true, false},
		{"another day", record(func(r *domain.CheckinRecord) { r.Day = "2026-10-19" }), checkinVendorWorkBuddy, "acct", false, false},
		{"another account on the same card", record(func(r *domain.CheckinRecord) { r.AccountKey = "other" }), checkinVendorWorkBuddy, "acct", false, false},
		{"a failure inside its retry", record(func(r *domain.CheckinRecord) {
			r.Outcome = domain.CheckinFailed
			r.At = stamp(5 * time.Minute)
		}), checkinVendorWorkBuddy, "acct", false, true},
		{"a failure past its retry is asked again", record(func(r *domain.CheckinRecord) {
			r.Outcome = domain.CheckinFailed
			r.At = stamp(31 * time.Minute)
		}), checkinVendorWorkBuddy, "acct", false, false},
		{"an offline failure comes back in a minute", record(func(r *domain.CheckinRecord) {
			r.Outcome = domain.CheckinFailed
			r.Offline = true
			r.At = stamp(2 * time.Minute)
		}), checkinVendorWorkBuddy, "acct", false, false},
		{"an offline failure inside its doubled wait", record(func(r *domain.CheckinRecord) {
			r.Outcome = domain.CheckinFailed
			r.Offline = true
			r.Tries = 2
			r.At = stamp(3 * time.Minute)
		}), checkinVendorWorkBuddy, "acct", false, true},
		{"Qoder is re-read inside the same day", record(func(r *domain.CheckinRecord) {
			r.At = stamp(31 * time.Minute)
		}), checkinVendorQoder, "acct", false, false},
		{"Qoder a success is re-read when its campaign ends", record(func(r *domain.CheckinRecord) {
			r.Outcome = domain.CheckinClaimed
			r.Until = now.Add(-time.Minute).UTC().Format(time.RFC3339)
		}), checkinVendorQoder, "acct", false, false},
		{"Qoder a success still inside its window waits for the look", record(func(r *domain.CheckinRecord) {
			r.Outcome = domain.CheckinClaimed
			r.Until = now.Add(time.Hour).UTC().Format(time.RFC3339)
		}), checkinVendorQoder, "acct", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkinSettled(tc.record, tc.vendor, tc.key, day, now, tc.soon); got != tc.want {
				t.Fatalf("checkinSettled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckinRetryAfterDoublesForAnOfflineFailure(t *testing.T) {
	full := checkinRetry
	offline := func(tries int) domain.CheckinRecord {
		return domain.CheckinRecord{Offline: true, Tries: tries}
	}
	wants := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, full, full}
	for tries, want := range wants {
		if got := checkinRetryAfter(offline(tries)); got != want {
			t.Fatalf("tries %d: retryAfter = %s, want %s", tries, got, want)
		}
	}
	if got := checkinRetryAfter(domain.CheckinRecord{Outcome: domain.CheckinFailed}); got != full {
		t.Fatalf("an answered failure should wait the full retry, got %s", got)
	}
}

func TestCheckinAccountKeyFollowsTheAccount(t *testing.T) {
	qoder := domain.Provider{ID: "q1", AuthType: domain.AuthTypeQoderCN, QoderPAT: &domain.QoderCredential{UID: "uid-9"}}
	key, err := checkinAccountKey(qoder)
	if err != nil || key != domain.AuthTypeQoderCN+"|uid-9" {
		t.Fatalf("qoder key = %q, err %v", key, err)
	}
	// A connection that never reached the account endpoints has no uid, so the
	// row is the only identity there is — and it must still be stable.
	anonymous := domain.Provider{ID: "q2", AuthType: domain.AuthTypeQoderPAT, QoderPAT: &domain.QoderCredential{}}
	if key, err = checkinAccountKey(anonymous); err != nil || key != "q2" {
		t.Fatalf("anonymous key = %q, err %v", key, err)
	}
	workBuddy := domain.Provider{ID: "w1", AuthType: domain.AuthTypeWorkBuddy, WorkBuddy: &domain.WorkBuddyCredential{UserID: "u-1"}}
	if key, err = checkinAccountKey(workBuddy); err != nil || key != domain.AuthTypeWorkBuddy+"|u-1" {
		t.Fatalf("workbuddy key = %q, err %v", key, err)
	}
	if _, err = checkinAccountKey(domain.Provider{ID: "c1", AuthType: domain.AuthTypeClaudeOAuth}); err == nil {
		t.Fatal("a vendor with no check-in must refuse")
	}
}

func TestRecordCheckinStoresAndReusesTheDay(t *testing.T) {
	server, store := checkinTestServer(domain.Provider{ID: "q1", AuthType: domain.AuthTypeQoderCN})
	now := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	server.recordCheckin(domain.Provider{ID: "q1", Name: "Qoder CN"}, domain.CheckinRecord{
		Outcome: domain.CheckinClaimed,
		Credit:  100,
	}, now)
	record, found, err := store.CheckinByProvider("q1")
	if err != nil || !found {
		t.Fatalf("record not stored: found=%v err=%v", found, err)
	}
	if record.Day != checkinDay(now) || record.At == "" || record.AccountKey != "q1" {
		t.Fatalf("record = %+v", record)
	}
}
