package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The console builds "today" for 用量统计 and the per-key badge from the
// gateway's day, not the browser's: the zone must reach /__state.
func TestStateCarriesServerTimeZone(t *testing.T) {
	previous := time.Local
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("no tz database: %v", err)
	}
	time.Local = shanghai
	t.Cleanup(func() { time.Local = previous })

	server, _ := newAuthTestServer(t)
	handler := server.Handler()
	req := httptest.NewRequest(http.MethodGet, "/__state", nil)
	req.Host = "127.0.0.1:18093"
	req.RemoteAddr = "127.0.0.1:4321"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var state struct {
		ServerTimeZone string `json:"serverTimeZone"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.ServerTimeZone != "Asia/Shanghai" {
		t.Fatalf("serverTimeZone = %q, want Asia/Shanghai", state.ServerTimeZone)
	}
}

// A fixed numeric offset has no IANA name; the console still needs a usable
// offset, not "Local".
func TestServerTimeZoneNameFallsBackToOffset(t *testing.T) {
	previous := time.Local
	t.Cleanup(func() { time.Local = previous })

	if _, err := time.LoadLocation("Asia/Tokyo"); err != nil {
		t.Skipf("no tz database: %v", err)
	}
	time.Local = time.FixedZone("", 9*3600)
	if got := serverTimeZoneName(); got != "+09:00" {
		t.Fatalf("fixed-zone offset = %q, want +09:00", got)
	}
}
