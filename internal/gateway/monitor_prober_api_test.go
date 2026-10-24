package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
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
	// 一轮轻探测 = chat + tools 两个 case，各一发上游请求。
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want chat + tools", calls)
	}
	// 两行记录只扣一次当日配额：maxPerDayPerAccount 按「次」（轮）算。
	if _, used, err := db.MonitorProbeOutcome("p1", monitorProbeDay(server, time.Now().UTC())); err != nil || used != 1 {
		t.Fatalf("daily budget used = %d err = %v, want one round", used, err)
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
		{"unknown kind", `{"providerId":"p1","kind":"deep"}`},
		// CLI 深检已撤下：旧界面传 kind=cli 也得被拒，且不能打上游。
		{"retired cli kind", `{"providerId":"p1","kind":"cli"}`},
		{"unknown provider", `{"providerId":"gone"}`},
	} {
		recorder := monitorCall(t, server.handleMonitorProbeRun, http.MethodPost, "/__monitor/probes/run",
			item.body, nil, monitorAdminID())
		if recorder.Code == http.StatusOK {
			t.Fatalf("%s must not succeed", item.name)
		}
	}
	if cli := monitorCall(t, server.handleMonitorProbeRun, http.MethodPost, "/__monitor/probes/run",
		`{"providerId":"p1","kind":"cli"}`, nil, monitorAdminID()); cli.Code != http.StatusBadRequest ||
		!strings.Contains(cli.Body.String(), "kind_unavailable") {
		t.Fatalf("kind=cli must answer 400 kind_unavailable, got %d %s", cli.Code, cli.Body.String())
	}
	if calls != 2 {
		t.Fatalf("rejected calls must not reach the upstream, calls = %d", calls)
	}
}

func TestMonitorProbeConfigRoundTripAndGuards(t *testing.T) {
	db := newProberTestStore(t)
	server, _ := newQuotaTestServer(t, "http://127.0.0.1:1")
	server.monitoringStore = db

	good := `{"eventDriven":{"enabled":true,"idleMinutes":45,"maxPerDayPerAccount":2,"minIntervalMinutes":120},` +
		`"daily":{"enabled":true,"windowStart":"03:00","windowEnd":"05:00"},` +
		`"quotaStaleMinutes":30}`
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
	if strings.Contains(saved.Body.String(), "weeklyCli") {
		t.Fatalf("the removed weekly CLI schedule must not come back: %s", saved.Body.String())
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

// 旧库里存着已撤下的 weeklyCli 键：解析必须照常读出其余字段、不报错；再保存一次
// 时该键随结构体一起消失（期望行为，见 monitoring.Config.Probe 的注释）。
func TestMonitorProbeConfigToleratesLegacyWeeklyCLIKey(t *testing.T) {
	db := newProberTestStore(t)
	server, _ := newQuotaTestServer(t, "http://127.0.0.1:1")
	server.monitoringStore = db

	legacy := `{"eventDriven":{"enabled":true,"idleMinutes":50,"maxPerDayPerAccount":3,"minIntervalMinutes":90},` +
		`"daily":{"enabled":true,"windowStart":"04:30","windowEnd":"06:30"},` +
		`"weeklyCli":{"enabled":true,"weekday":0,"time":"05:00"},"quotaStaleMinutes":45}`
	if err := db.SetSetting("monitor_probe", legacy); err != nil {
		t.Fatal(err)
	}
	cfg := monitoring.NewConfig(db).Probe()
	if cfg.EventDriven.IdleMinutes != 50 || cfg.EventDriven.MaxPerDayPerAccount != 3 ||
		cfg.EventDriven.MinIntervalMinutes != 90 || cfg.Daily.WindowStart != "04:30" ||
		cfg.Daily.WindowEnd != "06:30" || cfg.QuotaStaleMinutes != 45 {
		t.Fatalf("legacy config must still be read: %+v", cfg)
	}

	recorder := monitorCall(t, server.handleMonitorProbeConfig, http.MethodGet, "/__monitor/probe-config", "", nil, monitorAdminID())
	if recorder.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var served monitoring.ProbeConfig
	if err := json.Unmarshal(recorder.Body.Bytes(), &served); err != nil {
		t.Fatalf("legacy config must decode: %v", err)
	}
	if served.QuotaStaleMinutes != 45 {
		t.Fatalf("served config = %+v", served)
	}

	if err := monitoring.NewConfig(db).SaveProbe(cfg); err != nil {
		t.Fatal(err)
	}
	if raw := db.Setting("monitor_probe"); strings.Contains(raw, "weeklyCli") {
		t.Fatalf("SaveProbe must drop the removed key, got %s", raw)
	}
}
