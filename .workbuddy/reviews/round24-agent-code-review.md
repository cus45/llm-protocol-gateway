# 第 24 轮审查 — 另一 agent 的改动是否正确

- 审查时间：2026-10-23 16:20 (+0800)
- 范围：HEAD `8b5542d` + 未提交工作区（47 改 / 6 新增）
- 对照项目：`C:\guodevelop\demo\magpie`
- 结论：**不可合入**。本轮零提交、工作区与上一轮逐字节一致，但首次把「流式是否还在流」做成端到端取证 —— **所有配了备用链的 Key，其流式应答都被整段缓冲后才发给客户端，且该版本已在生产运行**。

---

## 一、取证与基线

| 项 | 结果 |
|---|---|
| HEAD | `8b5542d` fix(lanes): RPM 改为滑动分钟窗口，纯限流 Provider 可排队（2026-10-23 13:00:11 +0800） |
| 工作区 | 47 改 / 6 新增，`+2424 / −401` |
| 指纹 `sha256(git diff HEAD)` | `f86d25eb…a2a5ebebc` — **与 round23 逐字节一致** |
| 最新 mtime | 13:35:34（`provider_lane_fifo_test.go`），静默 **2h44m** |
| `go build ./...` / `go vet ./internal/...` | 绿 |
| `go test ./internal/gateway/ -count=1` | **ok 38.401s，0 红** |
| `web: npx tsc -b` | 0 error |
| `gofmt -l internal/…` | 命中 `store/sqlite.go`、`store/session_pins_test.go`；取出 HEAD 版本 gofmt 干净 ⇒ 是工作区 CRLF 行尾所致，非格式问题 |
| 生产服务 | `llm-protocol-gateway.service` active（**注意：服务名不是 `llm-gateway`，路径 `/opt/llm-protocol-gateway/bin/gateway`**） |
| 生产二进制 | mtime `2026-10-22 22:24:57 PDT` = `2026-10-23 13:24:57 +0800`，active since 22:39:45 PDT |
| 生产特征串 | `isMyTurnLocked` PRESENT / `passTurnLocked` PRESENT / `strikeLocked` **ABSENT** / `retireLocked` **ABSENT** ⇒ **线上仍是「持锁调 retire 自死锁」那一版未提交 WIP（连续第 6 轮）** |
| 生产规模 | 44 Provider，9 个 API Key，其中 **5 个配了备用链**（luca-glm / work buddy / wb / wb-luoxiwen-claude / luca-workbuddy） |
| 限流是否启用 | 生产库 `rpm_limit>0` 为空 ⇒ lane 未启用，死锁版侥幸未爆 |

判定依据均来自本轮实测，未在生产发起真实模型请求（避免写库与消耗额度），生产侧证据限于二进制特征串与只读配置查询。

---

## 二、本轮新发现（探针实证，探针文件已删、指纹已复原）

### N24-1（P0，线上生效）流式名存实亡：整条备用链都整段缓冲

两处「无条件拦截」，`canDefer` 只决定「出错后重试还是直接交付」，**不决定是否走流式**：

| 路径 | 位置 | 行为 |
|---|---|---|
| 同协议透传 | `passthrough_stream.go:246`（`if fw, ok := w.(*failoverResponseWriter); ok && stream && status<400`） | 一律进 `writeBufferedStream` → `:407 io.ReadAll(io.TeeReader(...))` 读完整个上游响应 |
| 跨协议转换 | `protocol_proxy.go:623`（`if fw, ok := w.(*failoverResponseWriter); ok`） | 一律进 `interceptingStreamWriter` → `runStreamConverter` 全部写进内存 → `replay(fw)` 一次性下发 |

探针（`writePassThroughResponse` + 慢速 SSE body，首块 0ms、收尾帧 400ms）：

```
canDefer=true  total=401ms firstClientWrite=401ms writes=1
canDefer=false total=400ms firstClientWrite=400ms writes=1
```

`firstClientWrite == total` ⇒ 客户端在整段答案生成完之前收不到任何字节。

后果（按严重度）：

1. **TTFT = 完整生成时间**。带工具循环或长思考的回合，客户端看起来像卡死。
2. **生成期没有任何心跳**：`hw := newSSEHeartbeatResponseWriter(fw)` 在 `:434`，即 `ReadAll` 之后才创建，`startSSEHeartbeat` 根本没在这条分支调用 ⇒ 15s 保活帧缺失，CF/Nginx/客户端读超时都会先杀连接。
3. **内存 ≈ 2× 整段答案**：`interceptingStreamWriter.records` 每写一份都 copy 一次，`body()` 再拼一份，`streamBody` 又持一份；`io.ReadAll` 无上限。
4. **退化为批量网关**：整段缓冲期间 `upstreamIdleTimeout`(120s) 仍在计时，但慢消费者永远看不到渐进输出。

生效条件（重要修正）：`provider_failover.go:1076` 的 `len(chain)==0` 快速路径用裸 `w`，**只有「Key 没配备用链」才真流式**；一旦配了链（`:1166 canDefer := i < len(attempts)-1`），**包括末次尝试在内**都进 `failoverResponseWriter` ⇒ 整条链都缓冲。生产 9 个 Key 中 5 个有链。

> 对 round23 N23-1 的更正：不是「身后有备用链路才缓冲」，而是「进入链式路径后，末次尝试也照旧缓冲」。作者在 `canDefer` 上做的所有区分，在流式路径上是死变量。

对照 magpie：`internal/gateway/gateway.go:1396` 是 `io.Copy(w, res.Body)` 真流式，全仓没有 buffer-then-replay；代价是它不做「切断后换账号续流」（`stream_cut_test.go` 只把切断如实报给客户端）。我们把「可重试」做到了极致，代价是把流式整个取消 —— 这是一笔明显不划算的交易。

### N24-2（P1）`idleGuardedBody.Read` 在并发 Close 后空指针 panic

`upstream_timeout.go`：`Read` 在 `:120-124` 无判空调用 `b.timer.Stop()`，而 `Close`(`:138-146`) 会把 `b.timer` 置 nil。`Read` 在 `body.Read(p)` 期间不持锁，Close 可插入。

探针 PROBE-24D（in-flight Read + 并发 Close）：

```
PANIC in Read after concurrent Close: runtime error: invalid memory address or nil pointer dereference
```

线上影响：客户端断连/空闲守卫触发时都可能踩到；net/http 会 recover 掉 handler goroutine 的 panic，但该连接被直接掐断且请求日志可能漏记。附带：`reading` 字段只写不读（死字段）。

### N24-3（P2）收尾标记跨写边界漏判（4 字节携带不足）

`stream_end_reader.go:19` `tail [4]byte`、`:90-91` 只携带 4 字节。最长标记 `response.incomplete`(19B) 的任何跨越切分，只要首段 ≥5 字节就丢。

探针 PROBE-24C（单次出现的标记，按不同位置切两半）：

```
split=17 (首段 2B)  closed=true   whole-write closed=true
split=20 (首段 5B)  closed=false  whole-write closed=true
split=24 (首段 9B)  closed=false  whole-write closed=true
split=27 (首段 12B) closed=false  whole-write closed=true
```

后果：**完整流被判截断** → 客户端多收一组收尾帧（`[DONE]`×2 / `message_delta`+`message_stop`）、该请求被计为 Provider 软失败（进冷却），这是「正常请求把账号打进冷却」的一个真实来源。修法：携带 `len(最长标记)-1 = 18` 字节即可，成本可忽略。

### N24-4（P0，复核）转换路径「流被切断」判定恒为 false

5 处调用都在**开始读取之前**取值：

```
convert_claude_openai_stream.go:87      convert_claude_responses_stream.go:253
convert_openai_claude_stream.go:126     convert_openai_responses_chat_stream.go:576 / :815
```

`r := countingReader{r: reader}` 之后立刻 `interrupted := isUpstreamStreamInterruptedReader(r)`，此刻 `r.err == nil` ⇒ 恒 false，之后从不重算。三个 `closeInterrupted*` 分支为死代码。

探针 PROBE-24E（无 `[DONE]`、无 `finish_reason` 的 Chat 流 → Claude 客户端）：

```
err=<nil> usage={0 0 0 0}
message_stop=true  stop_reason=end_turn=true  stop_reason=max_tokens=false
```

即：**被切断的回合以「正常结束」交付给客户端**，不重试、不算 Provider 失败、token 记 0（成本与配额双重少算）。R4 在转换路径上至今未生效。

### N24-5（P3，复核）两套完成判定都缺回声守卫

PROBE-24B（正文里出现 `message_stop` 字样的**截断**流）：

```
truncated-mentions-marker  closesUpstreamStream=true   streamEndReader.closed=true
truncated-plain            closesUpstreamStream=false  streamEndReader.closed=false
```

一套（`closesUpstreamStream`，末 4KB Contains）与另一套（`streamEndReader`，`markersRejectingEcho` 只放了 `[DONE]`）在同一份字节上**同时判错**：真截断被当成完整，收尾帧不补、也不换账号。另注：`streamEndReader.seen` 只写不读。

---

## 三、历史项复核总账（六张表）

图例：✅ 已修（本轮复核）/ ✔️ 已修（沿用前轮证据）/ 🟡 部分 / ❌ 未动

### 3.1 协议转换

| 编号 | 项 | 位置 | 状态 |
|---|---|---|---|
| P1 | Claude→Chat 流式丢 prompt_tokens | `convert_claude_openai_stream.go:122-128` | ✅ 保留 message_start 的原始计数 |
| P2 | stop_reason 不全量映射 | `convert_claude_openai.go:429-431` | ✅ refusal/pause_turn/context_window_exceeded 已映射 |
| P3 | Chat→Responses 有 tool_calls 时正文被清空 | `convert_openai_responses_chat.go` | ✔️ (a55c35d) |
| P4 | Responses→Chat 丢 reasoning 增量 | `convert_openai_responses_chat_stream.go` | ✔️ |
| P5 | Responses→Chat 忽略 incomplete | `…chat_stream.go:657,957` | ✅ 转成 length |
| P6 | Chat→Claude block index 错乱 | `convert_openai_claude_stream.go` | ✔️ (597e96a) |
| P7 | 工具 ID 归一化碰撞 | `convert_tools.go:114-126` | ✅ 追加原 id 的 sha256 短后缀 |
| P8 | Responses→Claude 缺合成 tool_result | `convert_responses_claude.go` | ✔️ |
| P9 | redacted_thinking 不可回放 | `convert_claude_responses*.go` | 🟡 占位而非真签名块 |
| P10 | strict/top_k/stop_sequences/metadata/prompt_cache_key 未转发 | 各工具构造器 | 🟡 部分字段 |
| P11 | 架构：两两手写转换 vs magpie 单一 IR | — | ❌ 未动 |
| N24-1 | 转换流整段拦截（新） | `protocol_proxy.go:623` | ❌ **P0** |
| N24-4 | 切断判定恒 false | 5 处 | ❌ **P0** |

### 3.2 路由 / 调度

| 编号 | 项 | 状态 |
|---|---|---|
| R1 | 无 RPM 限流 | ✅ `provider_lanes.go:73-79,161` 滑动分钟窗口（8b5542d） |
| R2 | 冷却不看上游说法 | ✅ 骨架：`provider_failover.go:600-660` 解析 Retry-After/resets_at；🟡 短 Retry-After 仍按账号级长冷却处理，magpie 是同请求内重试（1/2/4s，cap 8s） |
| R3 | billing 归进 hardQuotaHints | ✅ (6a1256b) |
| R4 | 流截断无法续传 | ❌ 转换路径完全无效（N24-4）；透传路径因 N24-1 变成「缓冲后整段重试」 |
| R5 | 会话粘性只在内存 | ✅ `internal/store/session_pins.go` 落库（30519bb） |
| R6 | 并发闸门每 Provider、非 FIFO | 🟡 `provider_lanes.go:110-139` 仍是 provider 粒度；`provider_lane_fifo_test.go` 覆盖了排队顺序，但「每账号」未做 |

### 3.3 横切安全

| 编号 | 项 | 状态 |
|---|---|---|
| S1 | 管理 API `ACAO: *` | ✅ `cors.go:12,67-69` 已改白名单（同源/配置来源） |
| S2 | 请求体无大小上限 | ✅ `request_body_limit.go:43-48` 全局 MaxBytesReader |
| S3 | 登录/注册/setup 无限流 + setup 可被首访者初始化 | 🟡 `admin_auth.go:328` 已限回环；登录限流已有；遗留 authThrottle 全表扫、先解码后限流 |
| S4 | 请求/响应正文不脱敏入库 | ✅ (127c194 / b1e935c) 两侧都脱敏 |
| S5 | 上游头已返回后无空闲超时 | 🟡 主路径接 `guardUpstreamIdle`；仍有 29 处裸 `.Do(...)`（多为短探针/OAuth，风险低），但 N24-2 的 panic 在这一层 |

### 3.4 Provider 账号

| 编号 | 项 | 状态 |
|---|---|---|
| A1 | Trae/WorkBuddy/Cursor 续期无锁 | ✅ `oauth_renew_lock.go` 统一锁（16c00d3） |
| A2 | 子串匹配任意 host | ✅ `zhipu_usage.go:30` / `deepseek_usage.go:34` 改 `baseURLHostIn` 精确后缀 |
| A3 | `/v1/images/*` 盲拼路径 + 回落全局 Key | 🟡 落点拒绝 `imagesRouteRefusal`(:6046) + 授权复检 `imagesEffectiveProvider` 已做；`resolveProviderImageURL`(:3159) 仍无条件拼 `/images/generations` |
| A4 | Claude 额度漏 overage 窗口 / 429 无负缓存 | ✅ `claude_oauth_usage.go:98` + 负缓存 5/5 |
| A5 | Cursor 无过期提前量 | ✅ |
| A6 | WorkBuddy 令牌 401 自愈 | ✅ (round20 起) |

### 3.5 控制台（能力缺口）

| 项 | 状态 | 本轮证据 |
|---|---|---|
| 实时请求流（SSE） | ❌ | 前端无 `EventSource`/`text/event-stream` |
| 路由决策聚合视图 | ❌ | `analysis.attempts` 仍只在单条日志弹窗 |
| 会话视图 | ❌ | `sessionId` 只存在于类型定义（`types.ts:676`），无 UI |
| 一键重放 | ❌ | 全仓无 replay 入口 |
| 日志成本列 | ❌ | `costMicros` 只出现在图表 tooltip 与密钥页 |
| 告警未读角标 | ❌ | 无 unread 状态 |

唯一新增是价格表改版（全局范围 `*` + 按上游模型聚类的「未设价」清单），数据链路核对过：`usage.tsx:366` → `modelCoverage` → `pricing.tsx`，`ModelDayStats.PricedCount` 在 `monitor/store.go:723-724` 有累加，链路成立。

### 3.6 工程质量

| 项 | 状态 |
|---|---|
| `stream_end_reader.go` 单测 | ❌ 0 个（只有 `usage_test.go:213,230` 的间接用例） |
| 脚手架残留 | ❌ 5 个 `.bak`（`chatgpt_oauth/claude_oauth/cursor_oauth/qoder_pat*`）+ `reading`/`seen` 死字段 |
| 调试打印 | ✅ 本轮未见 `println("DBG` |
| 生产可回滚性 | ❌ 线上二进制无对应 commit（第 6 轮） |

---

## 四、与 magpie 的对照（本轮新增）

| 维度 | magpie | 本项目 |
|---|---|---|
| 流式 | `io.Copy(w, res.Body)` 直通，无缓冲 | 进链即 `io.ReadAll` / 拦截重放（N24-1） |
| 流被切断 | 如实报给客户端（`stream_cut_test.go`），不重放 | 试图「缓冲→换账号重试」，但转换路径判定恒 false（N24-4），透传路径靠末 4KB Contains 猜（N24-5） |
| 429 | 请求内重试 1/2/4s（cap 8s） | 账号级冷却 1min→15min 阶梯 |
| 未知参数 | `unknownOptionalField` 整流器去掉字段重试 | 一次性采样整流器，覆盖不到 stop 之类（历史遗留） |

结论：**magpie 选择「流式优先，切断就告诉客户端」；本项目选择「可重试优先，代价是取消流式」。当前实现拿到了两者的缺点**：既没有真流式，切断判定又不可靠。建议保留流式为主路径，把可重试限定在「首个 token 之前」这一真正无损的窗口内。

---

## 五、验收门槛（下一版合入前必须全绿）

1. 配了备用链的 Key，其最末次尝试也必须真流式：探针 `firstClientWrite < total/4`。
2. 生成期有 SSE 心跳，间隔 ≤15s，且不打断正常帧。
3. 转换路径首块直出，禁止 `interceptingStreamWriter` 全量拦截后再 replay。
4. PROBE-24E 必须反转：截断 Chat 流 → 客户端看到 `finish_reason=length` 或 `stop_reason=max_tokens`，且 `err != nil`（可重试）。
5. PROBE-24C 四个切点全部 `closed=true`（携带长度 ≥ 最长标记−1）。
6. 正文含 `message_stop`/`[DONE]` 字样的截断流必须判 `closed=false`（回声守卫）。
7. PROBE-24D 无 panic；`go test -race`（需 CGO）或等价并发单测通过。
8. 完整流不得出现第二个 `[DONE]` / 第二组收尾帧（round23 N23-2 遗留用例）。
9. `streamEndReader` 至少 6 个单测：普通完成/截断/跨写切分/回声/纯思考流/空流。
10. 每个上线二进制必须对应一个 commit；`./deploy/deploy-linux.sh` 只能部署 `git rev-parse HEAD` 构建产物。
11. 5 个 `.bak` 与死字段清理；`gofmt -l` 为空（按 LF 检查）。
12. `go build ./...`、`go vet ./...`、`go test ./internal/gateway/`、`web: tsc -b` 全绿。

---

## 六、建议动作顺序（本轮给出最小热修路径）

1. **先止血：生产回滚到 `6a1256b`**（最后一个「功能完整 + 无死锁」的可回滚提交），或对线上做两行热修：`passthrough_stream.go:246` 追加 `&& fw.canDefer`、`protocol_proxy.go:623` 追加 `&& fw.canDefer`。后者能让末次尝试立即恢复流式（这是收益最大、改动最小的一个条件）。
2. 修 N24-4：把 `interrupted` 移到循环结束后计算（`r.err == io.EOF && !sawClosingFrame`），补 PROBE-24E 的回归用例。
3. 修 N24-2（`if b.timer != nil`）与 N24-3（携带 18 字节），各配一个单测。
4. 再提交当前 47 文件 WIP（先补第 1/3/9 条门槛），让线上有 commit 可回滚。
5. 之后才回到控制台与能力扩展（第 4/5 批）。

---

## 七、本轮没做的事

- 未修改任何业务代码；探针文件（`zz_probe24_test.go`、`zz_probe24e_test.go`、`.workbuddy/probe_q.py`）已删，工作区指纹已复原为 `f86d25eb…`。
- 未在生产发起真实模型请求（避免写库与消耗额度），生产侧只做只读查询与二进制特征串比对。
- 未验证：`stream+JSON` 回包的 usage=0（round23 N23-4）、`sequence_number` 对写粒度的敏感性（V22-4）、图片 OpenAI 兼容分支 120s 超时 —— 这三项沿用上轮结论，未在本轮重跑。
