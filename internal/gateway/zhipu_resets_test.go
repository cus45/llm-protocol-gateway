package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestFetchZhipuResetsCountsAvailableByWindow(t *testing.T) {
	soon := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("targetType"); got != "PERSONAL" {
			t.Errorf("targetType = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "glm-key" {
			t.Errorf("authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"code":200,"msg":"","data":{` +
			`"fiveHourResets":[` +
			`{"available":true,"expireTime":` + itoa(soon.UnixMilli()) + `},` +
			`{"available":false,"expireTime":` + itoa(soon.UnixMilli()) + `},` +
			`{"available":true,"expireTime":"2026-10-23T08:00:00Z"}` +
			`],"weekResets":[` +
			`{"available":true,"expireTime":"2026-10-25T08:00:00Z"}` +
			`]}}`))
	}))
	defer server.Close()

	got, err := fetchZhipuResets(context.Background(), server.URL, "glm-key", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected resets")
	}
	if got.Count != 3 || got.FiveHour != 2 || got.Weekly != 1 {
		t.Fatalf("resets = %+v", got)
	}
	if got.Earliest != soon.Format(time.RFC3339) {
		t.Fatalf("earliest = %q, want %q", got.Earliest, soon.Format(time.RFC3339))
	}
}

func TestFetchZhipuResetsTeamHeadersAndEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("targetType"); got != "TEAM" {
			t.Errorf("targetType = %q", got)
		}
		if r.Header.Get("bigmodel-organization") != "org" || r.Header.Get("bigmodel-project") != "proj" {
			t.Errorf("team headers = %v", r.Header)
		}
		_, _ = w.Write([]byte(`{"fiveHourResets":[{"available":false}],"weekResets":[]}`))
	}))
	defer server.Close()

	if got, err := fetchZhipuResets(context.Background(), server.URL, "key", "org", "proj", true); got != nil || err != nil {
		t.Fatalf("no available resets should hide the section, got %+v err %v", got, err)
	}
}

func TestFetchZhipuResetsAuthRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":1000,"msg":"身份验证失败。","success":false}`))
	}))
	defer server.Close()
	if _, err := fetchZhipuResets(context.Background(), server.URL, "bad-key", "", "", false); err == nil {
		t.Fatal("refused key should be an error, not an empty list")
	}
}

func TestFetchZhipuResetsFailuresAreQuiet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer server.Close()
	if got, err := fetchZhipuResets(context.Background(), server.URL, "key", "", "", false); got != nil || err == nil {
		t.Fatalf("HTTP failure should return an error, got %+v err %v", got, err)
	}
}

func TestFetchZhipuResetsVendorPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"msg":"操作成功","data":{` +
			`"fiveHourResets":[{"recordId":0,"grantType":"DIRECT","expireTime":"2026-11-02 23:59:59","available":true}],` +
			`"weekResets":[` +
			`{"recordId":0,"grantType":"DIRECT","expireTime":"2026-10-30 23:59:59","available":true},` +
			`{"recordId":0,"grantType":"DIRECT","expireTime":"2026-10-04 23:59:59","available":false}` +
			`]},"success":true}`))
	}))
	defer server.Close()

	got, err := fetchZhipuResets(context.Background(), server.URL, "glm-key", "", "", false)
	if err != nil || got == nil || got.Count != 2 || got.FiveHour != 1 || got.Weekly != 1 {
		t.Fatalf("resets = %+v err %v", got, err)
	}
	if got.Earliest != "2026-10-30T15:59:59Z" {
		t.Fatalf("earliest = %q", got.Earliest)
	}
}

func TestZhipuResetExpiry(t *testing.T) {
	at := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		in   any
		want string
	}{
		{"millis", float64(at.UnixMilli()), at.Format(time.RFC3339)},
		{"seconds", float64(at.Unix()), at.Format(time.RFC3339)},
		{"string date", "2026-10-22T08:00:00Z", at.Format(time.RFC3339)},
		{"vendor date", "2026-10-22 16:00:00", at.Format(time.RFC3339)},
		{"string millis", itoa(at.UnixMilli()), at.Format(time.RFC3339)},
		{"missing", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := zhipuResetExpiry(tc.in)
			if tc.want == "" {
				if !got.IsZero() {
					t.Fatalf("got %v", got)
				}
				return
			}
			if got.UTC().Format(time.RFC3339) != tc.want {
				t.Fatalf("got %v want %s", got.UTC(), tc.want)
			}
		})
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
