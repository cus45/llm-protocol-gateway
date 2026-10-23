package gateway

import (
	"testing"
	"time"
)

// A plain 429 is a request throttle: it must not be stretched by a plan window
// the vendor happens to keep in the usage report (a "resets in 6 days" window
// used to park a healthy account for the whole cooldown cap).
func TestRateLimitRestIgnoresAFarOffReset(t *testing.T) {
	server := &Server{}
	now := time.Now()
	body := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	ttl := server.cooldownTTL("p1", "", body, false, now)
	if ttl > providerRateRestCap {
		t.Fatalf("a plain 429 rested %v, above the rate cap %v", ttl, providerRateRestCap)
	}
	if ttl < providerSoftUnavailableTTL {
		t.Fatalf("a plain 429 rested %v, below the soft floor", ttl)
	}
}

// A reset that falls inside the rate window does extend the rest: the vendor
// really will refuse until then.
func TestRateLimitRestExtendsToANearReset(t *testing.T) {
	server := &Server{}
	now := time.Now()
	reset := now.Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)
	body := []byte(`{"type":"error","error":{"type":"rate_limit_error","resets_at":"` + reset + `"}}`)
	ttl := server.cooldownTTL("p1", "", body, false, now)
	if ttl < 9*time.Minute {
		t.Fatalf("rest %v did not follow the near reset", ttl)
	}
	if ttl > providerRateRestCap {
		t.Fatalf("rest %v above the rate cap", ttl)
	}
}

// Consecutive rate failures back off, so a still-throttled account is probed
// less and less often instead of every minute.
func TestRateLimitRestBacksOff(t *testing.T) {
	server := &Server{}
	now := time.Now()
	var previous time.Duration
	for i := 0; i < 5; i++ {
		server.bumpSoftFailure("p1")
		ttl := server.cooldownTTL("p1", "", nil, false, now)
		if ttl < previous {
			t.Fatalf("failure %d rested %v, less than the previous %v", i+1, ttl, previous)
		}
		if ttl > providerRateRestCap {
			t.Fatalf("failure %d rested %v, above the cap", i+1, ttl)
		}
		previous = ttl
	}
	if previous <= providerSoftUnavailableTTL {
		t.Fatalf("backoff never grew past the soft floor: %v", previous)
	}
}

// A confirmed quota exhaustion rests for at least the quota floor, even when
// the vendor names nothing.
func TestQuotaRestHasAFloor(t *testing.T) {
	server := &Server{}
	ttl := server.cooldownTTL("p1", "", nil, true, time.Now())
	if ttl < providerQuotaRestFloor {
		t.Fatalf("quota rest %v below the floor %v", ttl, providerQuotaRestFloor)
	}
	if ttl > providerCooldownCap {
		t.Fatalf("quota rest %v above the cap %v", ttl, providerCooldownCap)
	}
}

// A reset far beyond the cap is clamped: the recovery loop is what proves an
// account back, and a vendor must not take it out of rotation until next week.
func TestQuotaRestIsCappedAtTheCooldownCap(t *testing.T) {
	server := &Server{}
	now := time.Now()
	reset := now.Add(6 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	body := []byte(`{"error":{"type":"insufficient_quota","resets_at":"` + reset + `"}}`)
	ttl := server.cooldownTTL("p1", "", body, true, now)
	if ttl != providerCooldownCap {
		t.Fatalf("rest %v, want the cap %v", ttl, providerCooldownCap)
	}
}

// A short Retry-After must not be overridden by the ladder: when the vendor
// says "come back in 5s", waiting the whole default rest is pointless.
func TestShortRetryAfterShortensTheRateRest(t *testing.T) {
	server := &Server{}
	ttl := server.cooldownTTL("p1", "5", nil, false, time.Now())
	if ttl != 5*time.Second {
		t.Fatalf("rest %v, want the vendor's 5s", ttl)
	}
}

// Once a rest ran until the window the vendor named, that reset is spent: the
// next failure must not inherit it.
func TestConsumedResetIsNotHandedToTheNextFailure(t *testing.T) {
	server := &Server{}
	now := time.Now()
	reset := now.Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)
	body := []byte(`{"error":{"type":"insufficient_quota","resets_at":"` + reset + `"}}`)
	ttl := server.cooldownTTL("p1", "", body, true, now)
	if !server.resetConsumed("p1", "", body, true, ttl, now) {
		t.Fatal("a rest that ran to the vendor reset should consume it")
	}
	if server.resetConsumed("p1", "5", nil, true, 30*time.Second, now) {
		t.Fatal("a short rest without a reset must not consume anything")
	}
}
