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

// 轻探测必须「便宜」：chat 与 tools 各一发，max_tokens 都钉死在 16。没有这个
// 上限时 Claude 这条路径会按 4096 发出去，每轮探测白烧一次订阅额度。
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
	if len(run.Cases) != 2 || run.Cases[0].Case != probeCaseChat || run.Cases[1].Case != probeCaseTools {
		t.Fatalf("cases = %+v", run.Cases)
	}
	for _, item := range run.Cases {
		if !item.OK || item.Status != http.StatusOK {
			t.Fatalf("case %s = %+v", item.Case, item)
		}
	}
	if run.Failed() || run.AuthFailure() {
		t.Fatalf("a passing probe must not look failed: %+v", run)
	}
	if len(bodies) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(bodies))
	}
	for i, body := range bodies {
		var payload map[string]any
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("upstream body %q: %v", body, err)
		}
		if got := payload["max_tokens"]; got != float64(monitorProbeMaxTokens) {
			t.Fatalf("body %d max_tokens = %v, want %d", i, got, monitorProbeMaxTokens)
		}
		_, hasTools := payload["tools"]
		if wantTools := i == 1; hasTools != wantTools {
			t.Fatalf("body %d has tools = %v, want %v", i, hasTools, wantTools)
		}
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
	if failedCase := events.Items[0].Detail["case"]; failedCase != "" {
		t.Fatalf("a passing round must not name a failed case, got %v", failedCase)
	}

	// 一轮 = 两次上游请求，但只扣一次当日配额：maxPerDayPerAccount 按「次」算。
	_, used, err := db.MonitorProbeOutcome("p1", monitorProbeDay(server, time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	if used != 1 {
		t.Fatalf("daily budget consumed = %d, want 1 round", used)
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

// tools case 失败而 chat 通过 → 整轮失败，事件 detail 必须点名 tools：否则
// 「对话能用、工具声明被上游拒收」会被显示成一条没有原因的失败。
func TestRunLightProbeToolCaseFailureNamesTheCase(t *testing.T) {
	upstream := chatUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		if _, hasTools := payload["tools"]; hasTools {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"tools: schema not supported"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	db := newProberTestStore(t)
	server, _ := newQuotaTestServer(t, upstream.URL)
	server.monitoringStore = db

	run := server.runLightProbe(context.Background(), "p1")
	if !run.Failed() {
		t.Fatalf("a 400 on tools must fail the round: %+v", run)
	}
	if run.FailedCase() != probeCaseTools {
		t.Fatalf("failed case = %q, want %q", run.FailedCase(), probeCaseTools)
	}
	// 400 是「上游拒收」不是「凭证废了」：不能标 AuthFailure。
	if run.AuthFailure() {
		t.Fatalf("a 400 is not an auth failure: %+v", run)
	}
	if run.Cases[1].Status != http.StatusBadRequest {
		t.Fatalf("tool case = %+v", run.Cases[1])
	}

	events, err := db.QueryMonitorEvents(monitoring.EventQuery{Kind: monitoring.EventKindProbe, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if events.Total != 1 {
		t.Fatalf("probe events = %d, want 1", events.Total)
	}
	detail := events.Items[0].Detail
	if detail["ok"] != false || detail["case"] != probeCaseTools {
		t.Fatalf("event detail = %+v", detail)
	}

	// 一轮失败只算一次连续失败：两个 case 各一行不能把 streak 数成 2。
	outcome, used, err := db.MonitorProbeOutcome("p1", monitorProbeDay(server, time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Consecutive != 1 {
		t.Fatalf("one failed round must count once, got %+v", outcome)
	}
	if used != 1 {
		t.Fatalf("daily budget consumed = %d, want 1 round", used)
	}
}

// 2xx 但不是 JSON（HTML 错误页、网关伪装的 200）同样算失败。
func TestRunLightProbeRejectsUnparseableSuccess(t *testing.T) {
	upstream := chatUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<html>maintenance</html>`)
	})
	server, _ := newQuotaTestServer(t, upstream.URL)

	run := server.runLightProbe(context.Background(), "p1")
	if !run.Failed() || run.FailedCase() != probeCaseChat {
		t.Fatalf("run = %+v", run)
	}
	if run.Cases[0].Status != http.StatusOK || run.Cases[0].Error != "unparseable response body" {
		t.Fatalf("case = %+v", run.Cases[0])
	}
}

// 一轮探测只发两个上游请求，任何档位都一样；配额若按行计，默认 4 次会悄悄变成
// 2 轮。这里把两轮跑满后按预算判定，钉住「按次」的口径。
func TestProbeBudgetCountsRoundsNotRequests(t *testing.T) {
	calls := 0
	upstream := chatUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	db := newProberTestStore(t)
	server, _ := newQuotaTestServer(t, upstream.URL)
	server.monitoringStore = db

	for i := 0; i < 2; i++ {
		if run := server.runLightProbe(context.Background(), "p1"); run.Skipped != "" || run.Failed() {
			t.Fatalf("round %d = %+v", i, run)
		}
	}
	if calls != 4 {
		t.Fatalf("upstream calls = %d, want 2 per round", calls)
	}
	_, used, err := db.MonitorProbeOutcome("p1", monitorProbeDay(server, time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	if used != 2 {
		t.Fatalf("two rounds consumed %d of the daily budget, want 2", used)
	}
	gate := probeGate{Enabled: true, UsedToday: used, MaxPerDay: 2}
	if !gate.budgetSpent() {
		t.Fatalf("two rounds must exhaust a 2/day budget: %+v", gate)
	}
}

// 三家协议的工具声明形状各不相同；形状写错上游会直接 400，探测就成了噪声。
func TestProviderChatTestPayloadToolShapes(t *testing.T) {
	req := providerChatTestRequest{UserPrompt: "hi", MaxTokens: monitorProbeMaxTokens, IncludeTools: true}
	plain := providerChatTestRequest{UserPrompt: "hi"}

	toolsOf := func(t *testing.T, payload map[string]any, wantTools bool) []any {
		t.Helper()
		raw, exists := payload["tools"]
		if !exists {
			if wantTools {
				t.Fatalf("tools missing from %v", payload)
			}
			return nil
		}
		tools, ok := raw.([]any)
		if !ok || len(tools) != 1 {
			t.Fatalf("tools = %#v", raw)
		}
		return tools
	}
	requireObjectSchema := func(t *testing.T, value any) {
		t.Helper()
		m, ok := value.(map[string]any)
		if !ok || m["type"] != "object" {
			t.Fatalf("schema = %#v", value)
		}
	}

	cases := []struct {
		name   string
		build  func(providerChatTestRequest) map[string]any
		assert func(t *testing.T, tool map[string]any)
	}{
		{
			name:  "claude",
			build: func(r providerChatTestRequest) map[string]any { return buildProviderClaudeChatPayload("m", r) },
			assert: func(t *testing.T, tool map[string]any) {
				if tool["name"] != probeToolName {
					t.Fatalf("name = %v", tool["name"])
				}
				if _, nested := tool["function"]; nested {
					t.Fatalf("Claude tool must not carry a function wrapper: %#v", tool)
				}
				requireObjectSchema(t, tool["input_schema"])
			},
		},
		{
			name: "chat",
			build: func(r providerChatTestRequest) map[string]any {
				return buildProviderChatPayload("m", buildProviderChatMessages(r), r.IncludeTools)
			},
			assert: func(t *testing.T, tool map[string]any) {
				fn, ok := tool["function"].(map[string]any)
				if tool["type"] != "function" || !ok || fn["name"] != probeToolName {
					t.Fatalf("chat tool = %#v", tool)
				}
				requireObjectSchema(t, fn["parameters"])
			},
		},
		{
			name:  "responses",
			build: func(r providerChatTestRequest) map[string]any { return buildProviderResponsesChatPayload("m", r) },
			assert: func(t *testing.T, tool map[string]any) {
				if tool["type"] != "function" || tool["name"] != probeToolName {
					t.Fatalf("responses tool = %#v", tool)
				}
				// Responses 的 function 工具是扁平的：没有 Chat 那层 function 包装。
				if _, nested := tool["function"]; nested {
					t.Fatalf("responses tool must be flat: %#v", tool)
				}
				requireObjectSchema(t, tool["parameters"])
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools := toolsOf(t, tc.build(req), true)
			tc.assert(t, tools[0].(map[string]any))
			// 不带 IncludeTools 时不能凭空多出 tools 字段。
			toolsOf(t, tc.build(plain), false)
		})
	}
}

// Responses 协议要把上限写进 max_output_tokens；带 tools 时上限同时生效，
// 模型真调工具也会被 16 截住——判定只要求 2xx 可解析。
func TestRunLightProbeResponsesUpstreamGetsToolsAndOutputCap(t *testing.T) {
	type seen struct {
		tools     bool
		maxOut    float64
		hasMaxTok bool
	}
	var calls []seen
	upstream := chatUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		_, hasTools := payload["tools"]
		maxOut, _ := payload["max_output_tokens"].(float64)
		_, hasMaxTokens := payload["max_tokens"]
		calls = append(calls, seen{tools: hasTools, maxOut: maxOut, hasMaxTok: hasMaxTokens})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"function_call","name":"get_time","arguments":"{}"}]}`)
	})
	server, _ := newQuotaTestServer(t, upstream.URL,
		domain.Provider{ID: "p1", Name: "R", Protocol: domain.ProtocolOpenAIResponses, AuthType: domain.AuthTypeAPIKey,
			BaseURL: upstream.URL + "/v1", APIKeySource: "literal:sk", DefaultModel: "gpt-5.6-terra"})

	run := server.runLightProbe(context.Background(), "p1")
	if run.Failed() {
		t.Fatalf("a tool_call-only response is still a working chain: %+v", run)
	}
	if len(calls) != 2 || calls[0].tools || !calls[1].tools {
		t.Fatalf("calls = %+v", calls)
	}
	for i, call := range calls {
		if call.hasMaxTok || call.maxOut != float64(monitorProbeMaxTokens) {
			t.Fatalf("call %d = %+v, want max_output_tokens=%d and no max_tokens", i, call, monitorProbeMaxTokens)
		}
	}
}
