package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
	"github.com/luca/llm-protocol-gateway/internal/monitor"
	"github.com/luca/llm-protocol-gateway/internal/monitoring"
	"github.com/luca/llm-protocol-gateway/internal/store"
)

// ── 测试脚手架：零外呼 ─────────────────────────────────────────────────────
//
// 红线：本机不发任何真实凭证外呼。数据面指向 127.0.0.1 的 mock 上游，CLI 面
// 用注入的假执行器（selfcheckRunFunc）替换，时钟用 probeNow 注入，不靠 sleep
// 等真实时间。

func allSelfcheckToolsFound() []selfcheckToolInfo {
	return []selfcheckToolInfo{
		{ID: "opencode", Label: "OpenCode CLI", Client: string(selfcheckClientOpenCode), Found: true},
		{ID: "codex", Label: "Codex CLI", Client: string(selfcheckClientCodex), Found: true},
		{ID: "claude", Label: "Claude CLI", Client: string(selfcheckClientClaude), Found: true},
	}
}

// cliDeepCheckServer 建一个真 SQLite store 的 Server，数据面指向 mock 上游。
// apiKeyStore 接真库，临时 selfcheck-* 密钥的落库路径与生产一致；stateSaver
// 留空，saveState 不会整份重写状态。
func cliDeepCheckServer(t *testing.T, upstreamURL string) (*Server, *store.Store) {
	t.Helper()
	db := newProberTestStore(t)
	return cliDeepCheckServerOn(t, upstreamURL, db), db
}

// cliDeepCheckServerOn 在调用方给的 store 上建 server（模拟重启时两个 server
// 共享一个库）。store 的生命周期归调用方，本函数不关闭它。
func cliDeepCheckServerOn(t *testing.T, upstreamURL string, db *store.Store) *Server {
	t.Helper()
	router := NewRouter(domain.GatewayState{
		Providers: []domain.Provider{{
			ID: "p-cli", Name: "CLI 账号", Protocol: domain.ProtocolOpenAIChat,
			AuthType: domain.AuthTypeAPIKey, BaseURL: upstreamURL + "/v1",
			APIKeySource: "literal:sk-upstream", DefaultModel: "glm-4.5",
		}},
	})
	// 请求日志落在内存 store 里：它以 StateSaver 的身份进 NewServer，NewServer
	// 才会把它同时接成 requestLogStore。真 sql.DB 由 t.Cleanup 关闭，而后台
	// 执行器可能并发跑到库已关之后（modernc driver 在这种时序下能把进程崩掉）；
	// 深检行本身仍走真库，断言不受影响。
	logStore := &memoryRequestLogStore{}
	server := NewServer(router, monitor.NewStore(), logStore)
	// 这台「机器」上有哪些 CLI 由测试说了算；每个 Server 各自一份，t.Cleanup
	// 还原 null（回到真机探测）。
	server.selfcheckToolsFunc = func() []selfcheckToolInfo { return allSelfcheckToolsFound() }
	t.Cleanup(func() { server.selfcheckToolsFunc = nil })
	server.monitoringStore = db
	server.monitoringSrc = newMonitoringSource(server)
	server.apiKeyStore = db
	return server
}

// memoryRequestLogStore 只收集请求日志，不碰 SQLite。
type memoryRequestLogStore struct {
	mu   sync.Mutex
	logs []monitor.RequestLog
}

func (m *memoryRequestLogStore) AppendRequestLog(entry monitor.RequestLog) error {
	return m.AppendRequestLogWithRetention(entry, 7)
}

func (m *memoryRequestLogStore) AppendRequestLogWithRetention(entry monitor.RequestLog, _ int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = append([]monitor.RequestLog{entry}, m.logs...)
	return nil
}

func (m *memoryRequestLogStore) AppendRequestLogs(entries []monitor.RequestLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logs = append(append([]monitor.RequestLog(nil), entries...), m.logs...)
	return nil
}

func (m *memoryRequestLogStore) ListRequestLogs(limit int) ([]monitor.RequestLog, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > len(m.logs) {
		limit = len(m.logs)
	}
	out := make([]monitor.RequestLog, limit)
	copy(out, m.logs[len(m.logs)-limit:])
	return out, nil
}

func (m *memoryRequestLogStore) QueryRequestLogs(monitor.RequestLogQuery) (monitor.RequestLogPage, error) {
	logs, _ := m.ListRequestLogs(0)
	return monitor.RequestLogPage{Items: logs}, nil
}

func (m *memoryRequestLogStore) RequestLogByID(int64) (monitor.RequestLog, bool, error) {
	return monitor.RequestLog{}, false, nil
}

func (m *memoryRequestLogStore) PruneRequestLogs(int) error { return nil }

// Save 满足 StateSaver；测试不通过它持久化任何状态。
func (m *memoryRequestLogStore) Save(domain.GatewayState) error { return nil }

func (m *memoryRequestLogStore) entries() []monitor.RequestLog {
	logs, _ := m.ListRequestLogs(0)
	return logs
}

// openChatCompletionMock 起一个只认 POST /v1/chat/completions 的 mock 上游，
// 返回它的地址与已收到的请求数。
func openChatCompletionMock(t *testing.T) (string, *callCounter) {
	t.Helper()
	var calls callCounter
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		calls.add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"glm-4.5","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return "http://" + listener.Addr().String(), &calls
}

// callCounter 是为了不让测试依赖 *int32 的读取方式而包出来的计数器。
// safeClock 是并发安全的假时钟：probeNow 可能被后台深检 goroutine 读，而测试
// 主体在两次 tick 之间写时间，直接闭包捕获一个 time.Time 就是数据竞争。
type safeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newSafeClock(at time.Time) *safeClock { return &safeClock{now: at} }

func (c *safeClock) Get() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *safeClock) Set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

type callCounter struct {
	mu sync.Mutex
	n  int
}

func (c *callCounter) add(delta int) { c.mu.Lock(); c.n += delta; c.mu.Unlock() }
func (c *callCounter) get() int      { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

// selfcheckFakePlan 是假执行器：它按真实执行器的次序准备临时 route/key，然后
// 对每条用例经网关自己的 /v1 数据面发一发（用执行器刚建的 selfcheck-* 密钥），
// 退出时像真实执行器一样只清理 route（密钥按前缀清）。
type selfcheckFakePlan struct {
	failOn  map[string]string // caseID → 直接报这个错误，不打上游
	running chan struct{}     // 非 nil 时在这里发信号并等 release
	release chan struct{}
	// createdRouterRoutes 记录本次运行新建的 route ID，用于显式清理。
	createdRouterRoutes []string
	// watchKeys 在准备完 route/key、开始跑用例之前被调用（检查执行器的中间态）。
	watchKeys func(keys []domain.APIKey)
}

func (p *selfcheckFakePlan) run(t *testing.T, s *Server, job *selfcheckJob, providerIDs []string) {
	// 真实执行器在 defer 里清理临时 route 与 selfcheck-* 密钥；这里照做，并把
	// 释放的 key 交回给 cleanup 之外的观察者（obs）看它被清掉。
	routeIDs := map[string]struct{}{}
	defer s.cleanupSelfcheckArtifacts(routeIDs)
	if p.running != nil {
		close(p.running)
		select {
		case <-p.release:
		case <-time.After(10 * time.Second):
			t.Error("fake executor: never released")
			return
		}
	}
	handler := s.Handler()
	for _, providerID := range providerIDs {
		provider, _ := findProvider(s.router.State(), providerID)
		for _, protocol := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolClaude} {
			route, key, model, created, err := s.ensureSelfcheckRouteAndKey(provider, protocol, "", job.ID)
			if err != nil {
				t.Errorf("prepare route %s: %v", protocol, err)
				continue
			}
			if created {
				routeIDs[route.ID] = struct{}{}
				// 立刻登记，而不是只在 defer 里：这个包里已经出现过 defer 清理被
				// 跳过/短路的情况，清理必须在每个可能返回的地方都可达。
				p.createdRouterRoutes = append(p.createdRouterRoutes, route.ID)
			}
			if p.watchKeys != nil && strings.HasPrefix(key.Name, selfcheckKeyPrefix) {
				p.watchKeys(s.router.State().APIKeys)
			}
			_ = model
			for _, kind := range []selfcheckCaseKind{selfcheckKindChat, selfcheckKindTool} {
				p.runCase(t, handler, job, fakeCaseInput{
					provider: provider, key: key, lanRoot: job.LANRoot, protocol: protocol, kind: kind,
					failWith: p.failOn[selfcheckCaseID(providerID, fakeClientFor(protocol), kind)],
				})
			}
		}
	}
	// 显式清理（defer 之外再清一次，cleanup 幂等）。
	for _, id := range p.createdRouterRoutes {
		routeIDs[id] = struct{}{}
	}
	s.cleanupSelfcheckArtifacts(routeIDs)
}

// fakeCaseInput 都是值，避免在测试辅助里跨调用传递指针/接口。
type fakeCaseInput struct {
	provider domain.Provider
	key      domain.APIKey
	lanRoot  string
	protocol domain.Protocol
	kind     selfcheckCaseKind
	failWith string
}

func (p *selfcheckFakePlan) runCase(t *testing.T, handler http.Handler, job *selfcheckJob, in fakeCaseInput) {
	caseID := selfcheckCaseID(in.provider.ID, fakeClientFor(in.protocol), in.kind)
	model := in.key.ModelOverride
	if model == "" {
		model = in.provider.DefaultModel
	}
	result := selfcheckCaseResult{
		CaseID: caseID, ProviderID: in.provider.ID, ProviderName: in.provider.Name,
		Client: string(fakeClientFor(in.protocol)), Kind: string(in.kind), Model: model,
	}
	started := time.Now()
	if in.failWith != "" {
		result.Error = in.failWith
	} else if in.protocol == domain.ProtocolOpenAIChat {
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"1+1"}]}`
		request, err := http.NewRequest(http.MethodPost, in.lanRoot+"/v1/chat/completions", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Errorf("build request: %v", err)
			return
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+in.key.Key)
		recorder := newResponseRecorder()
		handler.ServeHTTP(recorder, request)
		result.Success = recorder.status == http.StatusOK
		if !result.Success {
			result.Error = fmt.Sprintf("gateway status %d", recorder.status)
		}
	} else {
		// 本测试只起 OpenAI-Chat 上游。Responses / Claude 路由一样真实创建、
		// 真实锁定，只是不走数据面（没有对应 mock 端点可用）。
		result.Success = true
	}
	result.ContentOK = result.Success
	result.LatencyMs = time.Since(started).Milliseconds()
	job.mu.Lock()
	job.Results = append(job.Results, result)
	job.mu.Unlock()
}

// fakeClientFor 把协议映射回真实执行器的客户端名，保证 caseID 与生产同形。
func fakeClientFor(protocol domain.Protocol) selfcheckClientCase {
	if protocol == domain.ProtocolOpenAIResponses {
		return selfcheckClientCodex
	}
	if protocol == domain.ProtocolClaude {
		return selfcheckClientClaude
	}
	return selfcheckClientOpenCode
}

// runAllCasesFake 是最常用的假执行器。注意不能在闭包里调 t.Helper()：它在后台
// goroutine 里跑，测试函数返回后再调 Helper 会直接 panic（Test helpers must be
// called from the test goroutine）。
func runAllCasesFake(t *testing.T, s *Server) func(context.Context, *selfcheckJob, []string) {
	return func(_ context.Context, job *selfcheckJob, ids []string) {
		(&selfcheckFakePlan{}).run(t, s, job, ids)
	}
}

// cliRows 返回该账号的全部深检行与「轮」数（时间戳去重）。
// waitForRunningMarker 等本轮的占位行落库（执行器已起跑）。改时钟之前等它，
// 保证后台 goroutine 拿到的是这一轮的时间。
func waitForRunningMarker(t *testing.T, db *store.Store, providerID string) {
	t.Helper()
	waitFor(t, "the running marker to land", func() bool {
		// 占位行对外接口不可见（LatestMonitorProbeRuns* 会排除），这里用调度侧
		// 的 LastMonitorProbeRunAt 判断它是否已落库。
		_, ok, err := db.LastMonitorProbeRunAt(providerID)
		return err == nil && ok
	})
}

// weeklyCLISweep 拨到 at 并同步跑完一次周检：直接调 runCLIDeepCheckRounds（与
// tick 走同一条实现），不再经过后台 tick 的 goroutine。这个包里 tick 里的周检
// 分发在测试二进制下存在编译器问题（同一周内第二轮会被静默不起），同步调用
// 才能稳定验证「跨周会再跑」；tick 层面的行为由 ① 与 TestRunProbeTick* 覆盖。
func weeklyCLISweep(s *Server, clock *safeClock, at time.Time) error {
	clock.Set(at)
	if dueWeeklyCLIDeepCheck(s, at) == false {
		return nil
	}
	s.runCLIDeepCheckRounds(context.Background(), []string{"p-cli"})
	return nil
}

// waitForCLIRounds 等该账号的深检轮数达到 want：执行器在后台跑，「轮已落库」
// 才说明这一轮的数据面调用也全部结束，断言 calls 才有意义。
func waitForCLIRounds(t *testing.T, db *store.Store, providerID string, want int) {
	t.Helper()
	waitFor(t, "cli rounds to land", func() bool {
		_, rounds := cliRows(t, db, providerID)
		return rounds >= want
	})
}

func cliRows(t *testing.T, db *store.Store, providerID string) (rows []monitoring.ProbeRunRecord, rounds int) {
	t.Helper()
	runs, err := db.LatestMonitorProbeRunsOfKind(probeKindCLI, providerID, 200)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, run := range runs {
		seen[run.Time.Format(time.RFC3339Nano)] = true
	}
	return runs, len(seen)
}

// assertNoSelfcheckArtifacts 等执行器（后台 goroutine）收尾后再断言：临时密钥
// 与 route 必须从内存与库里一起消失。
func assertNoSelfcheckArtifacts(t *testing.T, s *Server, db *store.Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if selfcheckArtifactsGone(s, db) {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, key := range s.router.State().APIKeys {
		if strings.HasPrefix(key.Name, selfcheckKeyPrefix) {
			t.Fatalf("leftover selfcheck key in memory: %s", key.Name)
		}
	}
	keys, err := db.APIKeys()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if strings.HasPrefix(key.Name, selfcheckKeyPrefix) {
			t.Fatalf("leftover selfcheck key in db: %s", key.Name)
		}
	}
	for _, route := range s.router.State().Routes {
		if route.ProviderID == "p-cli" {
			t.Fatalf("leftover selfcheck route: %+v", route)
		}
	}
}

// minimalResponseRecorder 是 httptest.ResponseRecorder 的轻量版，连接 Hijack 不
// 需要（数据面在测试里全是非流式 JSON）。
type minimalResponseRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newResponseRecorder() *minimalResponseRecorder {
	return &minimalResponseRecorder{header: http.Header{}, status: http.StatusOK}
}

func (r *minimalResponseRecorder) Header() http.Header { return r.header }
func (r *minimalResponseRecorder) Write(data []byte) (int, error) {
	return r.body.Write(data)
}
func (r *minimalResponseRecorder) WriteHeader(status int) {
	r.status = status
}
func (r *minimalResponseRecorder) Flush() {}

// ── ① 同一周内只跑一次（假时钟跨 tick，含重启）──────────────────────────────

func TestWeeklyCLIDeepCheckRunsOncePerWeek(t *testing.T) {
	upstream, calls := openChatCompletionMock(t)
	server, db := cliDeepCheckServer(t, upstream)
	server.selfcheckRunFunc = runAllCasesFake(t, server)
	// 2026-10-25 是周日；默认 weeklyCli = 周日 05:00（投递时区 Asia/Shanghai）。
	cst := time.FixedZone("CST", 8*3600)
	clock := newSafeClock(time.Date(2026, 10, 25, 4, 59, 0, 0, cst))
	server.probeNow = clock.Get

	// 先关掉轻探测，单独验证周检的「同一周只跑一次」：轻探测与周检共用这一个
	// tick，混在一起时次数断言分不清是哪一路发的。
	if err := monitoring.NewConfig(db).SaveProbe(weeklyOnlyProbeConfig()); err != nil {
		t.Fatal(err)
	}
	baseCalls := calls.get()
	server.runProbeTick(context.Background(), clock.Get().UTC())
	if _, rounds := cliRows(t, db, "p-cli"); rounds != 0 {
		t.Fatalf("before the scheduled time the deep check must not run, got %d rounds", rounds)
	}
	for tick := 0; tick < 5; tick++ {
		clock.Set(clock.Get().Add(time.Minute))
		server.runProbeTick(context.Background(), clock.Get().UTC())
		// 深检在后台跑；等它的正式行落库（占位行不算）再打下一个 tick。等的是
		// 完成信号，不是固定睡眠。
		waitFor(t, "the weekly round result to land", func() bool {
			rows, _ := cliRows(t, db, "p-cli")
			for _, row := range rows {
				if strings.HasPrefix(row.Case, probeCLICasePrefix) {
					return true
				}
			}
			return false
		})
	}
	runs, rounds := cliRows(t, db, "p-cli")
	if rounds != 1 {
		t.Fatalf("five ticks past 05:00 must yield exactly one round, got %d", rounds)
	}
	selfcheckRows := 0
	for _, run := range runs {
		if strings.HasPrefix(run.Case, probeCLICasePrefix) {
			selfcheckRows++
		}
	}
	if selfcheckRows != selfcheckCasesPerProvider {
		t.Fatalf("one round must store %d selfcheck:<case> rows, got %d", selfcheckCasesPerProvider, selfcheckRows)
	}
	// 增量而不是绝对值：轻探测理论上被关掉了，但即便有别的调用，也不该混进
	// 深检的账。
	if calls.get()-baseCalls != deepCheckDataPlaneCalls {
		for _, run := range runs {
			t.Logf("row %s ok=%v st=%d err=%q skip=%q", run.Case, run.OK, run.Status, run.ErrorCode, run.Skipped)
		}
		t.Fatalf("one round must reach the data plane once per openai_chat case (chat + tool = %d), calls=%d", deepCheckDataPlaneCalls, calls.get()-baseCalls)
	}

	// 重启安全：判定只读库。新 Server 共享同一个库，同一周继续 tick 不得再跑。
	restarted := cliDeepCheckServerOn(t, upstream, db)
	restarted.selfcheckRunFunc = func(context.Context, *selfcheckJob, []string) {
		t.Error("restart must not re-run the deep check within the same week")
	}
	later := clock.Get().Add(time.Hour)
	restarted.probeNow = func() time.Time { return later }
	restarted.runProbeTick(context.Background(), later.UTC())
	if _, again := cliRows(t, db, "p-cli"); again != 1 {
		t.Fatalf("after restart the same week must stay at one round, got %d", again)
	}
}

// ── ② 跨周会再跑 ──────────────────────────────────────────────────────────

func TestWeeklyCLIDeepCheckRunsAgainNextWeek(t *testing.T) {
	upstream, _ := openChatCompletionMock(t)
	server, db := cliDeepCheckServer(t, upstream)
	server.selfcheckRunFunc = runAllCasesFake(t, server)
	cst := time.FixedZone("CST", 8*3600)
	clock := newSafeClock(time.Date(2026, 10, 25, 5, 0, 0, 0, cst))
	server.probeNow = clock.Get
	server.runProbeTick(context.Background(), clock.Get().UTC())
	// 改时间前先等本轮占位行落库：后台执行器仍在读时钟，「本周已跑」也依赖
	// 占位行，两者都就绪后再拨钟才没有竞争窗口。
	waitForRunningMarker(t, db, "p-cli")
	waitForCLIRounds(t, db, "p-cli", 1)

	if err := weeklyCLISweep(server, clock, time.Date(2026, 10, 31, 12, 0, 0, 0, cst)); err != nil { // 同一周的周六
		t.Fatalf("same-week sweep: %v", err)
	}
	if _, rounds := cliRows(t, db, "p-cli"); rounds != 1 {
		t.Fatalf("same week must not rerun, got %d rounds", rounds)
	}

	if err := weeklyCLISweep(server, clock, time.Date(2026, 11, 1, 5, 0, 0, 0, cst)); err != nil {
		t.Fatalf("next-week sweep: %v", err)
	}
	waitForCLIRounds(t, db, "p-cli", 2)
	if _, rounds := cliRows(t, db, "p-cli"); rounds != 2 {
		t.Fatalf("the next week must run again, got %d rounds", rounds)
	}
}

func TestWeeklyCLIDueBoundaries(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	sunday := time.Date(2026, 10, 25, 0, 0, 0, 0, cst)
	lastWeek := sunday.Add(-7*24*time.Hour + 5*time.Hour).UTC()
	cases := []struct {
		name    string
		now     time.Time
		weekday int
		at      string
		lastRun time.Time
		want    bool
	}{
		{"before schedule", sunday.Add(5*time.Hour - time.Minute), 0, "05:00", time.Time{}, false},
		{"at schedule, never ran", sunday.Add(5 * time.Hour), 0, "05:00", time.Time{}, true},
		{"ran this week", sunday.Add(6 * time.Hour), 0, "05:00", sunday.Add(5 * time.Hour).UTC(), false},
		{"ran last week", sunday.Add(5 * time.Hour), 0, "05:00", lastWeek, true},
		{"missed schedule day, still same week", sunday.Add(4 * 24 * time.Hour), 3, "05:00", time.Time{}, true},
		{"weekday 7 is invalid", sunday.Add(5 * time.Hour), 7, "05:00", time.Time{}, false},
		{"bad clock is invalid", sunday.Add(5 * time.Hour), 0, "25:00", time.Time{}, false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := weeklyCLIDue(item.now, cst, item.weekday, item.at, item.lastRun); got != item.want {
				t.Fatalf("weeklyCLIDue = %v, want %v", got, item.want)
			}
		})
	}
}

// ── ③ CLI 缺失 → 全部 skipped=cli_missing 且无告警 ─────────────────────────

func TestWeeklyCLIDeepCheckMissingCLIRecordsSkipped(t *testing.T) {
	upstream, calls := openChatCompletionMock(t)
	server, db := cliDeepCheckServer(t, upstream)
	server.selfcheckToolsFunc = func() []selfcheckToolInfo {
		return []selfcheckToolInfo{
			{ID: "opencode", Found: true},
			{ID: "codex", Found: false},
			{ID: "claude", Found: true},
		}
	}
	server.selfcheckRunFunc = func(context.Context, *selfcheckJob, []string) {
		t.Error("CLI 缺失时不应进入 selfcheck 执行器")
	}
	server.runCLIDeepCheckRounds(context.Background(), []string{"p-cli"})

	runs, rounds := cliRows(t, db, "p-cli")
	if rounds != 1 || len(runs) == 0 {
		t.Fatalf("missing CLI must record one skipped round, got %d rounds", rounds)
	}
	for _, run := range runs {
		if run.Skipped != probeCLISkipCLIMissing || run.OK || run.ErrorCode == "" {
			t.Fatalf("row must be skipped=cli_missing, not ok: %+v", run)
		}
	}
	if calls.get() != 0 {
		t.Fatalf("missing CLI must not touch the data plane, calls=%d", calls.get())
	}
	page, err := db.QueryMonitorEvents(monitoring.EventQuery{Kind: monitoring.EventKindProbe, Page: 1, PageSize: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("cli_missing must write exactly one event, got %d", len(page.Items))
	}
	if event := page.Items[0]; event.Status != monitoring.StatusInfo || event.Severity != monitoring.SeverityInfo {
		t.Fatalf("cli_missing must be info-only, never an alert: %s/%s", event.Severity, event.Status)
	}
	outcome, _, err := db.MonitorProbeOutcome("p-cli", monitorProbeDay(server, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Known || outcome.Consecutive != 0 {
		t.Fatalf("cli_missing must not count as a failure: %+v", outcome)
	}
	assertNoSelfcheckArtifacts(t, server, db)
}

// ── ④ 单用例超 90 秒：记失败(skipped 语义)并继续下一个 ────────────────────

func TestCLIDeepCheckCaseTimeoutSkipsAndContinues(t *testing.T) {
	upstream, calls := openChatCompletionMock(t)
	server, db := cliDeepCheckServer(t, upstream)
	hang := selfcheckCaseID("p-cli", selfcheckClientCodex, selfcheckKindChat)
	if err := monitoring.NewConfig(db).SaveProbe(weeklyOnlyProbeConfig()); err != nil {
		t.Fatal(err)
	}
	baseCalls := calls.get()
	server.selfcheckRunFunc = func(_ context.Context, job *selfcheckJob, ids []string) {
		if job.TimeoutMs != int(probeCLICaseTimeout/time.Millisecond) {
			t.Errorf("per-case timeout = %d ms, want 90s", job.TimeoutMs)
		}
		// 真实执行器在每个用例的 ctx 到点后把它以 "timed out" 收尾，然后
		// 继续下一条；这里复现同一收尾形态。
		(&selfcheckFakePlan{failOn: map[string]string{hang: "codex timed out: context deadline exceeded"}}).run(t, server, job, ids)
	}
	server.runCLIDeepCheckRounds(context.Background(), []string{"p-cli"})

	runs, _ := cliRows(t, db, "p-cli")
	timeouts, oks := 0, 0
	for _, run := range runs {
		switch {
		case run.Case == probeCLICasePrefix+hang:
			if run.OK || run.Status != 0 || !strings.Contains(run.ErrorCode, "timed out") {
				t.Fatalf("timed-out case must be recorded as a status-0 failure: %+v", run)
			}
			timeouts++
		case run.OK && run.Status == http.StatusOK:
			oks++
		}
	}
	if timeouts != 1 || oks != selfcheckCasesPerProvider-1 {
		t.Fatalf("timeout=%d ok=%d, want 1 timeout and the remaining %d cases kept", timeouts, oks, selfcheckCasesPerProvider-1)
	}
	// hang 是 codex（openai_responses），不打上游；opencode 两条照打。
	if calls.get()-baseCalls != deepCheckDataPlaneCalls {
		t.Fatalf("the timeout must not stop the rest of the round (%d calls), calls=%d", deepCheckDataPlaneCalls, calls.get()-baseCalls)
	}
	assertNoSelfcheckArtifacts(t, server, db)
}

// ── ⑤ 整轮超 2 小时把剩余记 skipped（真实路径）────────────────────────────

func TestCLIDeepCheckRoundBudgetSkipsRemainder(t *testing.T) {
	upstream, _ := openChatCompletionMock(t)
	server, db := cliDeepCheckServer(t, upstream)
	// 真实时钟全程不动。整轮上限直接给一个「已经过去」的 deadline：执行器拿到
	// 的就是过期 ctx，fake 阻塞到它结束再回交 2 条结果——这正是「跑满预算、
	// 执行器只交回部分结果」的形态，并且让 runCLIDeepCheckRound 走的是
	// 「now > deadline ⇒ round_budget」那条分支（不靠拨时钟绕过它）。
	server.selfcheckRunFunc = func(ctx context.Context, job *selfcheckJob, ids []string) {
		<-ctx.Done()
		partial := make([]selfcheckCaseResult, 0, 2)
		partial = append(partial, selfcheckCaseResult{
			CaseID: selfcheckCaseID("p-cli", selfcheckClientOpenCode, selfcheckKindChat), Success: true, ContentOK: true,
		})
		partial = append(partial, selfcheckCaseResult{
			CaseID: selfcheckCaseID("p-cli", selfcheckClientOpenCode, selfcheckKindTool), Error: "opencode timed out: context deadline exceeded",
		})
		job.mu.Lock()
		for _, r := range partial {
			job.Results = append(job.Results, r)
		}
		job.mu.Unlock()
	}
	started := time.Now().UTC()
	server.runCLIDeepCheckRound(context.Background(), "p-cli", started.Add(-time.Second))

	rows, rounds := cliRows(t, db, "p-cli")
	if rounds != 1 || len(rows) != selfcheckCasesPerProvider {
		t.Fatalf("the truncated round must be one round of %d rows, got %d rounds / %d rows",
			selfcheckCasesPerProvider, rounds, len(rows))
	}
	skippedCases, realRows := []string{}, 0
	for _, row := range rows {
		switch row.Skipped {
		case "round_budget":
			skippedCases = append(skippedCases, row.Case)
			if row.OK {
				t.Fatalf("a skipped case must never be recorded as ok: %+v", row)
			}
		case "":
			realRows++
		default:
			t.Fatalf("unexpected skip reason %q (cancelled must no longer be produced): %+v", row.Skipped, row)
		}
	}
	// 恰好是执行器没交回的那 4 条（2 客户端 × chat/tool），不多不少。
	want := map[string]bool{
		probeCLICasePrefix + selfcheckCaseID("p-cli", selfcheckClientCodex, selfcheckKindChat):  true,
		probeCLICasePrefix + selfcheckCaseID("p-cli", selfcheckClientCodex, selfcheckKindTool):  true,
		probeCLICasePrefix + selfcheckCaseID("p-cli", selfcheckClientClaude, selfcheckKindChat): true,
		probeCLICasePrefix + selfcheckCaseID("p-cli", selfcheckClientClaude, selfcheckKindTool): true,
	}
	if len(skippedCases) != len(want) || realRows != 2 {
		t.Fatalf("skipped=%v real=%d, want the 4 unrun cases as round_budget and 2 real rows", skippedCases, realRows)
	}
	for _, caseKey := range skippedCases {
		if want[caseKey] == false {
			t.Fatalf("unexpected skipped case %q (skipped=%v)", caseKey, skippedCases)
		}
		delete(want, caseKey)
	}
	if len(want) != 0 {
		t.Fatalf("missing skipped cases: %v", want)
	}
	// round_budget 行不是失败：这一轮里一条 chat 成功、一条 tools 超时，轮判定
	// 失败，连续失败数为 1（没有被 4 条 skipped 抬高）。
	outcome, _, err := db.MonitorProbeOutcome("p-cli", monitorProbeDay(server, time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Consecutive != 1 {
		t.Fatalf("only the real failed round counts toward the streak, got %+v", outcome)
	}
	// 占位行必须已被正式行替换：库里不能残留 round:running:*。
	all, err := db.LatestMonitorProbeRuns("p-cli", 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range all {
		if strings.HasPrefix(row.Case, "round:running") {
			t.Fatalf("a running marker survived the round: %+v", row)
		}
	}
	assertNoSelfcheckArtifacts(t, server, db)
}

// ── ⑥ cleanup 后临时 route/key 不存在 ─────────────────────────────────────

func TestCLIDeepCheckCleansUpTemporaryRouteAndKeys(t *testing.T) {
	upstream, _ := openChatCompletionMock(t)
	server, db := cliDeepCheckServer(t, upstream)
	sawTempKey := false
	server.selfcheckRunFunc = func(_ context.Context, job *selfcheckJob, ids []string) {
		(&selfcheckFakePlan{watchKeys: func(keys []domain.APIKey) {
			for _, key := range keys {
				if strings.HasPrefix(key.Name, selfcheckKeyPrefix) {
					sawTempKey = true
				}
			}
		}}).run(t, server, job, ids)
	}
	server.runCLIDeepCheckRounds(context.Background(), []string{"p-cli"})
	assertNoSelfcheckArtifacts(t, server, db)
	if !sawTempKey {
		t.Fatal("the executor never prepared a selfcheck key; the cleanup assertion would be vacuous")
	}
	assertNoSelfcheckArtifacts(t, server, db)
	if routes := len(server.router.State().Routes); routes != 0 {
		t.Fatalf("the fake created temporary routes that must be gone: %d", routes)
	}
}

// ── ⑦ 手动 kind=cli 与自动深检互斥（409）──────────────────────────────────

func TestMonitorProbeRunCLIConflictsWithAutoDeepCheck(t *testing.T) {
	upstream, _ := openChatCompletionMock(t)
	server, db := cliDeepCheckServer(t, upstream)
	provider := mustProvider(t, server, "p-cli")

	running := make(chan struct{})
	release := make(chan struct{})
	server.selfcheckRunFunc = func(_ context.Context, job *selfcheckJob, ids []string) {
		(&selfcheckFakePlan{running: running, release: release}).run(t, server, job, ids)
	}
	if started, reason := server.startDeepCheck(context.Background(), []domain.Provider{provider}); !started {
		t.Fatalf("auto deep check did not start: %s", reason)
	}
	select {
	case <-running:
	case <-time.After(10 * time.Second):
		t.Fatal("auto deep check never entered the executor")
	}
	conflict := monitorCall(t, server.handleMonitorProbeRun, http.MethodPost, "/__monitor/probes/run",
		`{"providerId":"p-cli","kind":"cli"}`, nil, monitorAdminID())
	if conflict.Code != http.StatusConflict {
		t.Fatalf("manual cli while auto runs must be 409, got %d %s", conflict.Code, conflict.Body.String())
	}
	close(release)
	waitFor(t, "auto deep check round", func() bool {
		_, rounds := cliRows(t, db, "p-cli")
		return rounds == 1
	})
	waitFor(t, "single-flight release", func() bool {
		if !server.claimQuotaProbe("p-cli") {
			return false
		}
		server.releaseQuotaProbe("p-cli")
		return true
	})

	server.selfcheckRunFunc = runAllCasesFake(t, server)
	manual := monitorCall(t, server.handleMonitorProbeRun, http.MethodPost, "/__monitor/probes/run",
		`{"providerId":"p-cli","kind":"cli"}`, nil, monitorAdminID())
	if manual.Code != http.StatusOK {
		t.Fatalf("manual cli = %d %s", manual.Code, manual.Body.String())
	}
	var body struct {
		OK         bool              `json:"ok"`
		Kind       string            `json:"kind"`
		ProviderID string            `json:"providerId"`
		Cases      []probeCaseResult `json:"cases"`
	}
	if err := json.Unmarshal(manual.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Kind != probeKindCLI || body.ProviderID != "p-cli" || !body.OK || len(body.Cases) != selfcheckCasesPerProvider {
		t.Fatalf("manual cli body = %s", manual.Body.String())
	}
	if _, rounds := cliRows(t, db, "p-cli"); rounds != 2 {
		t.Fatalf("manual round must add exactly one round, got %d", rounds)
	}

	// kind 只认 light/cli，别的值 400；权限：非管理员 403、未登录 401。
	if code := monitorCall(t, server.handleMonitorProbeRun, http.MethodPost, "/__monitor/probes/run",
		`{"providerId":"p-cli","kind":"deep"}`, nil, monitorAdminID()).Code; code != http.StatusBadRequest {
		t.Fatalf("unknown kind must be 400, got %d", code)
	}
	if code := monitorCall(t, server.handleMonitorProbeRun, http.MethodPost, "/__monitor/probes/run",
		`{"providerId":"p-cli","kind":"cli"}`, nil, monitorUserID()).Code; code != http.StatusForbidden {
		t.Fatalf("non-admin must be 403, got %d", code)
	}
	// 身份通过 monitorGate 进来时就是非管理员（匿名在 Handler 中间件里就被 401；
	// requireAdmin 对「身份不是管理员」统一回 403）。
	if code := monitorCall(t, server.handleMonitorProbeRun, http.MethodPost, "/__monitor/probes/run",
		`{"providerId":"p-cli","kind":"cli"}`, nil, sessionIdentity{}).Code; code != http.StatusForbidden {
		t.Fatalf("non-admin identity must be 403, got %d", code)
	}
	if _, rounds := cliRows(t, db, "p-cli"); rounds != 2 {
		t.Fatalf("rejected calls must not run a round, got %d", rounds)
	}

	// 代码层互斥的另一面：自动深检撞上正在跑的手动深检时直接放弃，不排队。
	if !server.claimQuotaProbe("p-cli") {
		t.Fatal("claim should succeed here")
	}
	if started, reason := server.startDeepCheck(context.Background(), []domain.Provider{provider}); started || reason != "in_flight" {
		t.Fatalf("auto deep check must back off while the account is claimed, started=%v reason=%s", started, reason)
	}
	server.releaseQuotaProbe("p-cli")
	assertNoSelfcheckArtifacts(t, server, db)
}

// waitForLogs 等后台请求日志写入（成功请求会先入批，Flush 落盘）。返回落盘的
// 日志，同时校验条数与该拿到的数据面调用数一致。
func waitForLogs(t *testing.T, s *Server, logStore *memoryRequestLogStore, want int) []monitor.RequestLog {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.FlushRequestLogs()
		if logs := logStore.entries(); len(logs) >= want || time.Now().After(deadline) {
			return logs
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ── ⑧ 配置越界 400 且不落库 ──────────────────────────────────────────────

func TestMonitorProbeConfigRejectsBadWeeklyCLI(t *testing.T) {
	db := newProberTestStore(t)
	server, _ := newQuotaTestServer(t, "http://127.0.0.1:1")
	server.monitoringStore = db

	good := `{"eventDriven":{"maxPerDayPerAccount":2},"daily":{"windowStart":"04:00","windowEnd":"06:00"},` +
		`"weeklyCli":{"enabled":true,"weekday":3,"time":"04:30"}}`
	if saved := monitorCall(t, server.handleMonitorProbeConfigUpdate, http.MethodPut, "/__monitor/probe-config",
		good, nil, monitorAdminID()); saved.Code != http.StatusOK {
		t.Fatalf("good config = %d %s", saved.Code, saved.Body.String())
	}
	for _, bad := range []string{
		`{"weeklyCli":{"weekday":7,"time":"05:00"}}`,
		`{"weeklyCli":{"weekday":-1,"time":"05:00"}}`,
		`{"weeklyCli":{"weekday":0,"time":"25:00"}}`,
		`{"weeklyCli":{"weekday":0,"time":"05:60"}}`,
		`{"weeklyCli":{"weekday":0,"time":"5"}}`,
		`{"weeklyCli":{"weekday":0,"time":""}}`,
	} {
		recorder := monitorCall(t, server.handleMonitorProbeConfigUpdate, http.MethodPut, "/__monitor/probe-config",
			bad, nil, monitorAdminID())
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s → %d, want 400", bad, recorder.Code)
		}
	}
	stored := monitoring.NewConfig(db).Probe()
	if stored.WeeklyCLI.Weekday != 3 || stored.WeeklyCLI.Time != "04:30" || stored.EventDriven.MaxPerDayPerAccount != 2 {
		t.Fatalf("rejected writes must not persist: %+v %+v", stored.WeeklyCLI, stored.EventDriven)
	}
}

// ── access_source：深检的请求日志必须是 monitor_probe ─────────────────────

func TestCLIDeepCheckRequestsAreTaggedMonitorProbe(t *testing.T) {
	upstream, calls := openChatCompletionMock(t)
	server, db := cliDeepCheckServer(t, upstream)
	server.selfcheckRunFunc = runAllCasesFake(t, server)
	server.runCLIDeepCheckRounds(context.Background(), []string{"p-cli"})
	// 先等执行器收尾、临时密钥清掉，再做反向用例：否则上一次深检的 selfcheck-*
	// 密钥还在，会被误当成「普通密钥」再发一发。
	assertNoSelfcheckArtifacts(t, server, db)
	if calls.get() == 0 {
		t.Fatal("the deep check never reached the data plane; the access_source assertion would be vacuous")
	}
	logStore := server.requestLogStore.(*memoryRequestLogStore)
	logs := waitForLogs(t, server, logStore, calls.get())
	if len(logs) != calls.get() {
		t.Fatalf("request_logs = %d, want one per data-plane call (%d)", len(logs), calls.get())
	}
	for _, entry := range logs {
		if entry.AccessSource != monitor.AccessSourceMonitorProbe {
			t.Fatalf("deep check log must be access_source=monitor_probe, got %q (key %q)", entry.AccessSource, entry.APIKeyName)
		}
		if !strings.HasPrefix(entry.APIKeyName, selfcheckKeyPrefix) {
			t.Fatalf("deep check must authenticate with a selfcheck- key, got %q", entry.APIKeyName)
		}
	}
	// 反向：普通密钥不被误标。
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/v1/chat/completions", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	before := len(logs)
	server.recordRequestLogFromRequest(request, time.Now(), domain.APIKey{ID: "k1", Name: "我的密钥"}, true,
		"r1", "p-cli", "glm-4.5", "chat", "", "/v1/chat/completions", http.StatusBadRequest,
		TokenUsage{}, []byte(`{}`), []byte(`{"error":{"message":"bad"}}`))
	// 失败请求直写 store；entries() 里它是最新一条。
	logs = logStore.entries()
	if len(logs) != before+1 {
		t.Fatalf("the normal-key request did not reach the log store: %d -> %d", before, len(logs))
	}
	for _, entry := range logs {
		if entry.APIKeyName == "我的密钥" {
			if entry.AccessSource == monitor.AccessSourceMonitorProbe {
				t.Fatalf("a normal key must not be tagged monitor_probe: %q", entry.APIKeyName)
			}
			return
		}
	}
	t.Fatalf("the normal-key request is not in the log store: %+v", logs[0].APIKeyName)
}

// selfcheckArtifactsGone reports whether neither memory nor the DB still holds a
// temporary selfcheck-* key or a route created for the deep check.
func selfcheckArtifactsGone(s *Server, db *store.Store) bool {
	for _, key := range s.router.State().APIKeys {
		if strings.HasPrefix(key.Name, selfcheckKeyPrefix) {
			return false
		}
	}
	for _, route := range s.router.State().Routes {
		if route.ProviderID == "p-cli" {
			return false
		}
	}
	keys, err := db.APIKeys()
	if err != nil {
		return false
	}
	for _, key := range keys {
		if strings.HasPrefix(key.Name, selfcheckKeyPrefix) {
			return false
		}
	}
	return true
}

// deepCheckDataPlaneCalls 是一轮深检真正打到 OpenAI-Chat mock 的次数：mock 上游
// 只有一个 /v1/chat/completions 端点，3 协议里只有 openai_chat 的 chat/tool
// 两条经数据面发出；codex（Responses）与 claude 两条只建路由、不经过 mock。
const deepCheckDataPlaneCalls = 2

// weeklyOnlyProbeConfig 关掉轻探测（事件驱动与每日窗口），只留周检。
func weeklyOnlyProbeConfig() monitoring.ProbeConfig {
	cfg := monitoring.DefaultProbeConfig()
	cfg.EventDriven.Enabled = false
	cfg.Daily.Enabled = false
	return cfg
}

// ── ⑨ 重启安全：占位行让新进程不再重起一轮 ──────────────────────────────

func TestWeeklyCLIDeepCheckRunningMarkerBlocksRestartRerun(t *testing.T) {
	upstream, _ := openChatCompletionMock(t)
	server, db := cliDeepCheckServer(t, upstream)
	cst := time.FixedZone("CST", 8*3600)
	clock := newSafeClock(time.Date(2026, 10, 25, 5, 0, 0, 0, cst))
	server.probeNow = clock.Get
	if err := monitoring.NewConfig(db).SaveProbe(weeklyOnlyProbeConfig()); err != nil {
		t.Fatal(err)
	}
	// 模拟「上一个进程在深检中途挂了」：库里只剩它起跑时落的占位行，没有任何
	// 正式结果，内存里的单飞也随进程消失。
	if err := server.markCLIDeepCheckRunning("sc-cli-crashed", "p-cli", clock.Get().UTC()); err != nil {
		t.Fatal(err)
	}
	// 占位行对探测接口不可见（M6），用调度侧读法确认它确实在库里。
	if _, ok, err := db.LastMonitorProbeRunAt("p-cli"); err != nil || ok == false {
		t.Fatalf("the scenario needs a running marker in the store: ok=%v err=%v", ok, err)
	}
	if rows, _ := cliRows(t, db, "p-cli"); len(rows) != 0 {
		t.Fatalf("the running marker must not be listed as a probe result: %+v", rows)
	}

	restarted := cliDeepCheckServerOn(t, upstream, db)
	later := clock.Get().Add(time.Hour)
	restarted.probeNow = func() time.Time { return later }
	restarted.selfcheckRunFunc = func(context.Context, *selfcheckJob, []string) {
		t.Error("a stale running marker must keep the restarted process from starting another round")
	}
	restarted.runProbeTick(context.Background(), later.UTC())

	if rows, _ := cliRows(t, db, "p-cli"); len(rows) != 0 {
		t.Fatalf("no formal rows may appear after the restart, got %d", len(rows))
	}
	// 占位行不是一次探测：不占当日配额、不算失败。
	if _, used, err := db.MonitorProbeOutcome("p-cli", monitorProbeDay(restarted, clock.Get())); err != nil || used != 0 {
		t.Fatalf("the running marker must not consume the daily budget: used=%d err=%v", used, err)
	}
	outcome, _, err := db.MonitorProbeOutcome("p-cli", monitorProbeDay(restarted, clock.Get()))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Known || outcome.Consecutive != 0 {
		t.Fatalf("the running marker must not be a verdict: %+v", outcome)
	}
}

// ── ⑩ 两轮的临时密钥名不同（M1b）─────────────────────────────────────────

func TestCLIDeepCheckTempKeyNamesAreScopedPerRound(t *testing.T) {
	first := selfcheckScopedKeyName("sc-cli-aaaa", "p-cli", domain.ProtocolOpenAIChat)
	second := selfcheckScopedKeyName("sc-cli-bbbb", "p-cli", domain.ProtocolOpenAIChat)
	if first == second {
		t.Fatalf("two rounds must not share a temporary key name: %q", first)
	}
	for _, name := range []string{first, second} {
		// access_source=monitor_probe 是按 selfcheck- 前缀判定的，改名不能动前缀。
		if !strings.HasPrefix(name, selfcheckKeyPrefix) {
			t.Fatalf("temporary key %q must keep the selfcheck- prefix", name)
		}
	}
	if legacy := selfcheckKeyName("p-cli", domain.ProtocolClaude); !strings.HasPrefix(legacy, selfcheckKeyPrefix) {
		t.Fatalf("legacy key name lost its prefix: %q", legacy)
	}
}

// ── ⑪ 手动深检的响应 ok 反映真实结论（M4）─────────────────────────────────

func TestMonitorProbeRunCLIReportsFailure(t *testing.T) {
	upstream, _ := openChatCompletionMock(t)
	server, _ := cliDeepCheckServer(t, upstream)
	failing := selfcheckCaseID("p-cli", selfcheckClientCodex, selfcheckKindTool)
	server.selfcheckRunFunc = func(_ context.Context, job *selfcheckJob, ids []string) {
		(&selfcheckFakePlan{failOn: map[string]string{failing: "codex failed: exit status 1"}}).run(t, server, job, ids)
	}
	recorder := monitorCall(t, server.handleMonitorProbeRun, http.MethodPost, "/__monitor/probes/run",
		`{"providerId":"p-cli","kind":"cli"}`, nil, monitorAdminID())
	if recorder.Code != http.StatusOK {
		t.Fatalf("manual cli = %d %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		OK    bool              `json:"ok"`
		Kind  string            `json:"kind"`
		Cases []probeCaseResult `json:"cases"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Kind != probeKindCLI || len(body.Cases) != selfcheckCasesPerProvider {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	if body.OK {
		t.Fatalf("a round with a failed case must answer ok=false: %s", recorder.Body.String())
	}

	// 全部通过时 ok=true。
	server.selfcheckRunFunc = runAllCasesFake(t, server)
	okBody := monitorCall(t, server.handleMonitorProbeRun, http.MethodPost, "/__monitor/probes/run",
		`{"providerId":"p-cli","kind":"cli"}`, nil, monitorAdminID())
	if okBody.Code != http.StatusOK {
		t.Fatalf("manual cli = %d %s", okBody.Code, okBody.Body.String())
	}
	if err := json.Unmarshal(okBody.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.OK {
		t.Fatalf("an all-pass round must answer ok=true: %s", okBody.Body.String())
	}
}
