package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitoring"
)

const (
	// lightProbeTimeout 是一发轻探测的上限。慢到 30 秒以上的账号交给 TTFT 规则
	// 去盯，探测只回答「还能不能用」。
	lightProbeTimeout = 30 * time.Second
	// monitorProbeMaxTokens 刻意给到最小：探测不该消耗订阅额度。
	monitorProbeMaxTokens = 16
	probeKindLight        = "light"
	probeCaseChat         = "chat"
	probeChatPrompt       = "reply with the single word ok"
)

// probeCaseResult 是一发探测的结论。Status 为 0 表示传输层失败（连不上/超时）——
// 它同样算失败，只是没法按状态码归类。
type probeCaseResult struct {
	Case     string
	OK       bool
	Status   int
	Duration int64
	Model    string
	Error    string
}

// probeRun 是一个账号一轮探测的结果。Skipped 非空表示这一轮根本没跑成
// （账号没了/没配模型/已有一发在飞），它不算失败——把「没跑成」记成失败会让
// 探测自己的调度问题变成账号告警。
type probeRun struct {
	ProviderID   string
	ProviderName string
	Kind         string
	At           time.Time
	Cases        []probeCaseResult
	Skipped      string
}

func (r probeRun) Failed() bool {
	if r.Skipped != "" || len(r.Cases) == 0 {
		return false
	}
	for _, item := range r.Cases {
		if !item.OK {
			return true
		}
	}
	return false
}

// AuthFailure 区分「凭证废了」和「上游抖了一下」：401/403 正是 probe_failed
// 要抬到 P0 的那一类。
func (r probeRun) AuthFailure() bool {
	for _, item := range r.Cases {
		if item.Status == http.StatusUnauthorized || item.Status == http.StatusForbidden {
			return true
		}
	}
	return false
}

// LastStatus 取第一个非零状态码，供规则与界面显示「上游到底回了什么」。
func (r probeRun) LastStatus() int {
	for _, item := range r.Cases {
		if item.Status != 0 {
			return item.Status
		}
	}
	return 0
}

func lightProbeModel(provider domain.Provider) string {
	if model := strings.TrimSpace(provider.DefaultModel); model != "" {
		return model
	}
	for _, model := range provider.Models {
		if model.InMenu && strings.TrimSpace(model.ID) != "" {
			return strings.TrimSpace(model.ID)
		}
	}
	return ""
}

// runLightProbe 对一个账号发一轮最小 chat 探测。
//
// 它刻意走 chat-test 而不是 /v1 数据面：chat-test 不写 request_logs，所以既不进
// 账号成功率也不进成本账本。与额度保活共用同一把单飞锁，避免「保活一发 + 监控一发」
// 同时打在同一账号上——那既多花额度又像滥用。
func (s *Server) runLightProbe(ctx context.Context, providerID string) probeRun {
	run := probeRun{ProviderID: providerID, Kind: probeKindLight, At: time.Now().UTC()}
	provider, err := s.router.ProviderByID(providerID)
	if err != nil {
		run.Skipped = "provider_missing"
		return run
	}
	run.ProviderName = provider.Name
	if provider.Disabled {
		run.Skipped = "provider_disabled"
		return run
	}
	model := lightProbeModel(provider)
	if model == "" {
		run.Skipped = "no_model"
		return run
	}
	if !s.claimQuotaProbe(providerID) {
		run.Skipped = "in_flight"
		return run
	}
	defer s.releaseQuotaProbe(providerID)

	run.Cases = append(run.Cases, s.probeChatCase(ctx, providerID, model))
	s.recordProbeRun(run)
	return run
}

func (s *Server) probeChatCase(ctx context.Context, providerID, model string) probeCaseResult {
	started := time.Now().UTC()
	req := providerChatTestRequest{
		Model:      model,
		Message:    probeChatPrompt,
		UserPrompt: probeChatPrompt,
		MaxTokens:  monitorProbeMaxTokens,
	}
	callCtx, cancel := context.WithTimeout(ctx, lightProbeTimeout)
	defer cancel()
	request, err := quotaProbeHTTPRequest(callCtx, providerID, req)
	if err != nil {
		return probeCaseResult{Case: probeCaseChat, Status: 0, Duration: time.Since(started).Milliseconds(), Error: err.Error()}
	}
	result, _ := s.testProviderChat(request, providerID, req, started)
	status := mapIntValue(result, "status")
	ok, _ := result["success"].(bool)
	caseResult := probeCaseResult{
		Case:     probeCaseChat,
		OK:       ok,
		Status:   status,
		Duration: mapInt64Value(result, "latencyMs"),
		Model:    mapStringValue(result, "model"),
	}
	if !ok {
		caseResult.Error = truncateRunReason(mapStringValue(result, "error"))
	}
	return caseResult
}

// monitorProbeDay 按投递时区给出探测所属的自然日。当日次数上限与每日窗口都以它
// 为准——按 UTC 算的话，+08:00 的部署要到早上 8 点才"新的一天"，配额会白白少给一天。
func monitorProbeDay(s *Server, now time.Time) string {
	location := time.UTC
	if s != nil && s.monitoringStore != nil {
		if configured := monitoring.NewConfig(s.monitoringStore).Dispatch().Location(); configured != nil {
			location = configured
		}
	}
	return now.In(location).Format("2006-01-02")
}

// persistProbeRun 把每一发探测落库。库里存的是「跑成了什么」，规则与调度都只读库，
// 所以重启不会把连续失败计数和当日配额清零。
func (s *Server) persistProbeRun(run probeRun) {
	if s.monitoringStore == nil || len(run.Cases) == 0 {
		return
	}
	day := monitorProbeDay(s, run.At)
	for _, item := range run.Cases {
		errorCode := item.Error
		if len(errorCode) > 200 {
			errorCode = errorCode[:200]
		}
		if err := s.monitoringStore.InsertMonitorProbeRun(monitoring.ProbeRunRecord{
			ProviderID: run.ProviderID, Kind: run.Kind, Case: item.Case, OK: item.OK,
			Status: item.Status, DurationMs: item.Duration, Model: item.Model,
			ErrorCode: errorCode, Day: day, Time: run.At,
		}); err != nil {
			slog.Warn("monitoring probe run persist failed", "provider", run.ProviderID, "error", err)
			return
		}
	}
}

// recordProbeRun 把一轮探测写进事件流。成功与失败都只写 info 事件：成级由
// probe_failed 规则按连续次数决定，探测自己不该报警。
func (s *Server) recordProbeRun(run probeRun) {
	if s.monitoringStore == nil {
		return
	}
	s.persistProbeRun(run)
	name := run.ProviderName
	if name == "" {
		name = run.ProviderID
	}
	cases := make([]map[string]any, 0, len(run.Cases))
	for _, item := range run.Cases {
		entry := map[string]any{"case": item.Case, "ok": item.OK, "durationMs": item.Duration}
		if item.Status != 0 {
			entry["status"] = item.Status
		}
		if item.Model != "" {
			entry["model"] = item.Model
		}
		if item.Error != "" {
			entry["error"] = item.Error
		}
		cases = append(cases, entry)
	}
	title := name + " 轻探测通过"
	if run.Failed() {
		title = name + " 轻探测失败"
	}
	if _, err := s.monitoringStore.InsertMonitorEvent(monitoring.Event{
		Time: run.At, Kind: monitoring.EventKindProbe, Severity: monitoring.SeverityInfo,
		TargetType: monitoring.TargetProvider, TargetID: run.ProviderID, TargetName: name,
		Title: title, Status: monitoring.StatusInfo,
		Detail: map[string]any{
			"kind":    run.Kind,
			"ok":      !run.Failed(),
			"skipped": run.Skipped,
			"cases":   cases,
		},
	}); err != nil {
		// 探测记录失败不影响探测本身，也不能把账号判成不可用。
		slog.Warn("monitoring probe event failed", "provider", run.ProviderID, "error", err)
	}
}
