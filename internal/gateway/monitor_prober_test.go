package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitoring"
	"github.com/luca/llm-protocol-gateway/internal/store"
)

func newProberTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// 轻探测必须「便宜」：一发最小 chat，max_tokens 钉死在 16。没有这个上限时
// Claude 这条路径会按 4096 发出去，每轮探测白烧一次订阅额度。
func TestRunLightProbeSendsOneMinimalChat(t *testing.T) {
	var bodies []string
	upstream := chatUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-sonnet-4-5","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	db := newProberTestStore(t)
	server, _ := newQuotaTestServer(t, upstream.URL)
	server.monitoringStore = db

	run := server.runLightProbe(context.Background(), "p1")
	if run.Skipped != "" {
		t.Fatalf("probe skipped: %s", run.Skipped)
	}
	if len(run.Cases) != 1 || !run.Cases[0].OK || run.Cases[0].Status != http.StatusOK {
		t.Fatalf("cases = %+v", run.Cases)
	}
	if run.Failed() || run.AuthFailure() {
		t.Fatalf("a passing probe must not look failed: %+v", run)
	}
	if len(bodies) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(bodies))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &payload); err != nil {
		t.Fatalf("upstream body %q: %v", bodies[0], err)
	}
	if got := payload["max_tokens"]; got != float64(monitorProbeMaxTokens) {
		t.Fatalf("max_tokens = %v, want %d", got, monitorProbeMaxTokens)
	}

	events, err := db.QueryMonitorEvents(monitoring.EventQuery{Kind: monitoring.EventKindProbe, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if events.Total != 1 {
		t.Fatalf("probe events = %d, want 1", events.Total)
	}
	if events.Items[0].Detail["ok"] != true {
		t.Fatalf("event detail = %+v", events.Items[0].Detail)
	}
}

// 401/403 是「凭证废了」，规则要按 authSeverity 抬到 P0；探测侧先把状态留住。
func TestRunLightProbeRecordsAuthFailure(t *testing.T) {
	upstream := chatUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`)
	})
	server, _ := newQuotaTestServer(t, upstream.URL)

	run := server.runLightProbe(context.Background(), "p1")
	if !run.Failed() {
		t.Fatalf("a 401 probe must count as failed: %+v", run)
	}
	if !run.AuthFailure() || run.LastStatus() != http.StatusUnauthorized {
		t.Fatalf("run = %+v", run)
	}
}

// 「没跑成」不等于「跑失败」：账号缺失/停用/没配模型/已有一发在飞都必须记 skipped，
// 否则探测自己的调度问题会变成账号告警。
func TestRunLightProbeSkipsWithoutTouchingUpstream(t *testing.T) {
	calls := 0
	upstream := chatUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})

	server, _ := newQuotaTestServer(t, upstream.URL,
		domain.Provider{ID: "p1", Name: "健康", Protocol: domain.ProtocolClaude, AuthType: domain.AuthTypeAPIKey,
			BaseURL: upstream.URL + "/v1", APIKeySource: "literal:sk", DefaultModel: "claude-sonnet-4-5"},
		domain.Provider{ID: "gone-disabled", Name: "停用", Protocol: domain.ProtocolClaude, AuthType: domain.AuthTypeAPIKey,
			BaseURL: upstream.URL + "/v1", APIKeySource: "literal:sk", Disabled: true, DefaultModel: "claude-sonnet-4-5"},
		domain.Provider{ID: "no-model", Name: "没模型", Protocol: domain.ProtocolClaude, AuthType: domain.AuthTypeAPIKey,
			BaseURL: upstream.URL + "/v1", APIKeySource: "literal:sk"},
	)

	if run := server.runLightProbe(context.Background(), "missing"); run.Skipped != "provider_missing" || run.Failed() {
		t.Fatalf("missing provider: %+v", run)
	}
	if run := server.runLightProbe(context.Background(), "gone-disabled"); run.Skipped != "provider_disabled" || run.Failed() {
		t.Fatalf("disabled provider: %+v", run)
	}
	if run := server.runLightProbe(context.Background(), "no-model"); run.Skipped != "no_model" || run.Failed() {
		t.Fatalf("provider without a model: %+v", run)
	}

	server.claimQuotaProbe("p1")
	defer server.releaseQuotaProbe("p1")
	if run := server.runLightProbe(context.Background(), "p1"); run.Skipped != "in_flight" {
		t.Fatalf("concurrent probe must be refused, got %+v", run)
	}
	if calls != 0 {
		t.Fatalf("skipped probes must not reach the upstream, calls = %d", calls)
	}
}

// 探测记录写不进去也不能把账号判成失败——事件流是观测的副产品，不是探测的前提。
func TestRecordProbeRunWithoutStoreIsQuiet(t *testing.T) {
	server, _ := newQuotaTestServer(t, "http://127.0.0.1:1")
	server.recordProbeRun(probeRun{ProviderID: "p1", Kind: probeKindLight, Cases: []probeCaseResult{{Case: probeCaseChat, OK: true}}})
}

// 端到端钉住「探测结果真的进了规则输入」：两轮失败落库 → 连续失败计数为 2 →
// probe_failed 成级。单测各自绿不代表这条链通。
func TestTwoFailedProbesMakeProbeRuleFire(t *testing.T) {
	upstream := chatUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`)
	})
	db := newProberTestStore(t)
	server, _ := newQuotaTestServer(t, upstream.URL)
	server.monitoringStore = db

	for i := 0; i < 2; i++ {
		if run := server.runLightProbe(context.Background(), "p1"); !run.Failed() {
			t.Fatalf("probe %d should have failed: %+v", i, run)
		}
	}
	now := time.Now().UTC()
	outcome, used, err := db.MonitorProbeOutcome("p1", monitorProbeDay(server, now))
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Known || outcome.Consecutive != 2 || !outcome.AuthFailure {
		t.Fatalf("outcome = %+v", outcome)
	}
	if used != 2 {
		t.Fatalf("daily budget consumed = %d, want 2", used)
	}

	engine := monitoring.NewEngine(db, monitoring.DefaultRules())
	snapshot := func(at time.Time) monitoring.Snapshot {
		return monitoring.Snapshot{
			Now: at, CollectorLastSuccess: at,
			Providers: []monitoring.ProviderObservation{{ID: "p1", Name: "Claude", Probe: outcome}},
		}
	}
	// Consecutive 类规则是双门：观测侧数够 + 引擎侧看到同样多的命中 tick，
	// 所以成级落在第二个 tick（见 engine_rules_test 的同名说明）。
	first, err := engine.Evaluate(context.Background(), snapshot(now))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range first {
		if item.State.RuleKey == "probe_failed" {
			t.Fatalf("probe_failed must wait for its second hit tick, got %+v", item)
		}
	}
	transitions, err := engine.Evaluate(context.Background(), snapshot(now.Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range transitions {
		if item.State.RuleKey == "probe_failed" && item.Firing {
			if item.State.Severity != monitoring.SeverityP0 {
				t.Fatalf("auth failure must escalate to p0, got %+v", item.State)
			}
			return
		}
	}
	t.Fatalf("probe_failed never fired: %+v", transitions)
}
