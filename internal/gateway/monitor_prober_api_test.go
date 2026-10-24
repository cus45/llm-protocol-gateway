package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitoring"
)

func TestMonitorProbesListsAccountsWithoutFabricatingHealth(t *testing.T) {
	upstream := chatUpstream(t, nil)
	db := newProberTestStore(t)
	server, _ := newQuotaTestServer(t, upstream.URL,
		domain.Provider{ID: "a", Name: "跑过的", Protocol: domain.ProtocolClaude, AuthType: domain.AuthTypeAPIKey,
			BaseURL: upstream.URL + "/v1", APIKeySource: "literal:sk", DefaultModel: "claude-sonnet-4-5"},
		domain.Provider{ID: "b", Name: "没跑过的", Protocol: domain.ProtocolClaude, AuthType: domain.AuthTypeAPIKey,
			BaseURL: upstream.URL + "/v1", APIKeySource: "literal:sk", DefaultModel: "claude-sonnet-4-5"},
	)
	server.monitoringStore = db
	if err := db.InsertMonitorProbeRun(monitoring.ProbeRunRecord{
		ProviderID: "a", Kind: probeKindLight, Case: probeCaseChat, OK: true, Status: 200,
		Day: monitorProbeDay(server, time.Now().UTC()), Time: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	recorder := monitorCall(t, server.handleMonitorProbes, http.MethodGet, "/__monitor/probes", "", nil, monitorAdminID())
	if recorder.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var page struct {
		Accounts []monitorProbeAccount       `json:"accounts"`
		Runs     []monitoring.ProbeRunRecord `json:"runs"`
		Config   monitoring.ProbeConfig      `json:"config"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Accounts) != 2 || len(page.Runs) != 1 {
		t.Fatalf("accounts = %d runs = %d", len(page.Accounts), len(page.Runs))
	}
	byID := map[string]monitorProbeAccount{}
	for _, row := range page.Accounts {
		byID[row.ProviderID] = row
	}
	if !byID["a"].Probed || byID["a"].LastStatus != 200 {
		t.Fatalf("probed account = %+v", byID["a"])
	}
	// 没跑过的账号必须是 probed=false，界面才能显示「暂无数据」而不是「0 次失败 = 健康」。
	if byID["b"].Probed || byID["b"].ConsecutiveFailures != 0 {
		t.Fatalf("unprobed account must stay unknown: %+v", byID["b"])
	}
	if page.Config.EventDriven.MaxPerDayPerAccount != 4 {
		t.Fatalf("config = %+v", page.Config)
	}
}

func TestMonitorProbeRunLightConflictAndGuards(t *testing.T) {
	calls := 0
	upstream := chatUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-5","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	db := newProberTestStore(t)
	server, _ := newQuotaTestServer(t, upstream.URL)
	server.monitoringStore = db

	ok := monitorCall(t, server.handleMonitorProbeRun, http.MethodPost, "/__monitor/probes/run",
		`{"providerId":"p1"}`, nil, monitorAdminID())
	if ok.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", ok.Code, ok.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(ok.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true || body["kind"] != probeKindLight {
		t.Fatalf("body = %+v", body)
	}
	if calls != 1 {
		t.Fatalf("upstream calls = %d", calls)
	}

	server.claimQuotaProbe("p1")
	defer server.releaseQuotaProbe("p1")
	conflict := monitorCall(t, server.handleMonitorProbeRun, http.MethodPost, "/__monitor/probes/run",
		`{"providerId":"p1"}`, nil, monitorAdminID())
	if conflict.Code != http.StatusConflict {
		t.Fatalf("in-flight probe must answer 409, got %d %s", conflict.Code, conflict.Body.String())
	}

	for _, item := range []struct{ name, body string }{
		{"missing provider", `{}`},
		{"unknown kind", `{"providerId":"p1","kind":"cli"}`},
		{"unknown provider", `{"providerId":"gone"}`},
	} {
		recorder := monitorCall(t, server.handleMonitorProbeRun, http.MethodPost, "/__monitor/probes/run",
			item.body, nil, monitorAdminID())
		if recorder.Code == http.StatusOK {
			t.Fatalf("%s must not succeed", item.name)
		}
	}
	if calls != 1 {
		t.Fatalf("rejected calls must not reach the upstream, calls = %d", calls)
	}
}

func TestMonitorProbeConfigRoundTripAndGuards(t *testing.T) {
	db := newProberTestStore(t)
	server, _ := newQuotaTestServer(t, "http://127.0.0.1:1")
	server.monitoringStore = db

	good := `{"eventDriven":{"enabled":true,"idleMinutes":45,"maxPerDayPerAccount":2,"minIntervalMinutes":120},` +
		`"daily":{"enabled":true,"windowStart":"03:00","windowEnd":"05:00"},"quotaStaleMinutes":30}`
	saved := monitorCall(t, server.handleMonitorProbeConfigUpdate, http.MethodPut, "/__monitor/probe-config",
		good, nil, monitorAdminID())
	if saved.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", saved.Code, saved.Body.String())
	}
	var cfg monitoring.ProbeConfig
	if err := json.Unmarshal(saved.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.EventDriven.IdleMinutes != 45 || cfg.EventDriven.MaxPerDayPerAccount != 2 || cfg.Daily.WindowStart != "03:00" {
		t.Fatalf("cfg = %+v", cfg)
	}

	for _, bad := range []string{
		`{"eventDriven":{"maxPerDayPerAccount":99}}`,
		`{"eventDriven":{"minIntervalMinutes":-1}}`,
		`{"daily":{"windowStart":"25:00","windowEnd":"06:00"}}`,
		`not json`,
	} {
		recorder := monitorCall(t, server.handleMonitorProbeConfigUpdate, http.MethodPut, "/__monitor/probe-config",
			bad, nil, monitorAdminID())
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s → code = %d, want 400", bad, recorder.Code)
		}
	}
	// 坏配置不能已经落库。
	after := monitorCall(t, server.handleMonitorProbeConfig, http.MethodGet, "/__monitor/probe-config", "", nil, monitorAdminID())
	var current monitoring.ProbeConfig
	if err := json.Unmarshal(after.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current.EventDriven.MaxPerDayPerAccount != 2 {
		t.Fatalf("rejected writes must not persist: %+v", current)
	}
}
