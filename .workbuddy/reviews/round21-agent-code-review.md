# round21 审查报告（另一 agent 改动）

审查时间：2026-10-23 13:21–14:05（+0800）
审查范围：新提交 `8b5542d`（13:00:11）+ 未提交工作区（44 改 / 6 新增，+2293/−354）
审查方式：干净 worktree 独立构建/测试 + 单独快照（`git stash create`）分析 + 探针实证 + 生产库只读查询
未修改任何业务代码；探针文件、临时 worktree 已清理。

---

## 一、结论先说

| 项 | 结果 |
|---|---|
| 干净 HEAD `8b5542d` | build/vet 绿，`go test ./internal/gateway/` **38.8s，0 红，全部通过** |
| 工作区 WIP（快照 9c2446c，13:23） | build/vet 绿，但 `go test` **300s 超时**：3 个用例**自死锁**（其中 2 个是作者自己新写的测试） |
| 生产 | 未提交 WIP 已上线（13:24:57 +0800）：二进制内含 `providerLane).retire` / `isMyTurnLocked` / `enqueueLocked` / `passTurnLocked` 符号与 `upstream stopped sending mid-stream`、`usage probe failed` 文案 ⇒ **跑的就是死锁版** |
| 生产是否已受伤 | **否（侥幸）**：44 个 Provider 的 `max_concurrent_requests` / `requests_per_minute` / `queue_wait_seconds` **全为 0** ⇒ lane 根本不启用。但控制台一旦给任何 Provider 设 RPM，**第一个排队超时就永久锁死该 Provider 的 lane，直到进程重启** |

一句话：**已提交的 8b5542d 可以做基线；WIP 整体不可合入**——新引入的 FIFO 座位制度在"等不到就放弃"这条最普通的路径上自己把自己锁死，而且它是唯一被我实测为"跑两次都必挂"的问题。

---

## 二、P0（WIP 新引入，必须先修）

### W21-1 `acquireSlot` 在持锁状态下调用 `retire()` → 自死锁

`internal/gateway/provider_lanes.go`：

- `:188` `lane.mu.Lock()`
- `:212` `if queued { lane.retire(ticket) }` ← **仍持锁**
- `:360` `func (lane *providerLane) retire(...)` 第一行 `lane.mu.Lock()`（`:361`）

`sync.Mutex` 不可重入，因此该分支一旦走到就永久阻塞在 `:361`，**并且锁永远不释放**——同 lane 的所有后续请求（包括不需要排队的）全部在 `:188` 挂死，HTTP handler 永不返回（server 也未设 ReadTimeout/WriteTimeout，客户端只能干等）。只有进程重启能解。

触发条件是**最普通的那条路**：请求进入队列 → 等到 budget 用尽 → `!now.Before(budget)` 成立 → retire。

证据（不是推测）：

1. 作者自己的用例挂死在三处同一栈上（`go test ./internal/gateway/ -timeout 300s`，dump 第 69–73 行）：
   ```
   panic: test timed out after 5m0s
       running tests:
           TestProviderLaneSkippedWaiterDoesNotHoldTheQueue (4m22s)
           TestProviderLaneWaitTimeoutAndCancel        (4m22s)
           TestRateOnlyLaneQueueCapacityAllowsWaiting   (4m22s)
   ```
2. 栈帧逐条对上（dump 第 354–357 行）：
   ```
   providerLanes.go:361  (*providerLane).retire        <- 等锁
   providerLanes.go:212  (*providerLane).acquireSlot   <- 持锁调用
   ```
3. 我另写的独立探针（3 个 120ms waiter + 1 个持槽者）5 秒内即复现同样栈。

`git log` 显示这段时间对方在实时编辑（13:22:1x 一版甚至编译不过：`unknown field broadcast`，13:23:09 自行修成 `wake`），属于"改完没跑全量测试"。

修法（二选一，推荐第一个）：

- 把逻辑拆成 `retireLocked(ticket)`，`retire()` 只做 `Lock + retireLocked + Unlock`，`:212` 与 `:238` 改调 `retireLocked`；
- 或 `:212` 直接 `if queued { lane.passTurnLocked(ticket) }`（此处语义就是放弃自己的座位并把turn交给下一位）。

连带必须补的：`waiters` 计数现在**只在 `passTurnLocked` 里减**，`waitFor` 已不再 `doneWaiting`。非队首 waiter 超时离开时 `retire` 是 no-op（`:360` 注释明确如此），**`waiters` 只增不减** ⇒ `queueCap` 会被幽灵占到满，之后所有请求直接吃 `queue full`。修 retire 用的同时要保证离队者一定减计数（`retireLocked` 里无条件 `waiters--` 并跳过塌陷的票号）。

### W21-2 P0 治理：未提交的死锁版已上生产

- VPS `/opt/llm-protocol-gateway/bin/gateway` mtime `2026-10-22 22:24:57 PDT` = `13:24:57 +0800`；
- 该二进制含**只在工作区存在**的符号与文案：`providerLane).retire`、`isMyTurnLocked`、`enqueueLocked`、`passTurnLocked`（同时 `doneWaiting` 已消失）、`upstream stopped sending mid-stream`、`usage probe failed`；
- HEAD `8b5542d` 里没有这些符号 ⇒ **生产跑的是 WIP，不是任何 commit**，出问题时无 commit 可回滚。

这是连续第三轮出现"未提交即上线"。建议硬性流程：`deploy-linux.sh` 里加 `git diff --quiet || exit 1`（或注明 `FORCE_UNCOMMITTED=1` 才允许），否则"生产版本 = ？"这个问题永远答不出来。

现状风险的具体量化（只读查询 `/var/lib/llm-protocol-gateway/gateway.db`）：

```
providers: 44, 配了 limit 的: 0
```
⇒ 目前 lane 不启用，死锁**尚未**触发。但控制台已经提供 RPM/并发设置入口，一旦有人填数字，这个 P0 立刻变成"该 Provider 全挂"。

---

## 三、其他新发现

### 已提交的 `8b5542d`（RPM 滑动窗口 + 纯限流 Provider 可排队）——判定：**正确，建议保留**

- 窗口语义由固定桶改为 `admits []int64` 滑动：`trimLocked` 丢掉 `<= now-1min` 的准入，`retryAfterSeconds` 用 `admits[0]+1min` 算真实等待 ⇒ 修掉了 round19/20 的"固定窗口边界跳变"。
- 纯限流 lane 的 `acquire()` 现在算出"最早一条滑出窗口的时刻 + 1ms"作为 budget（`:146-158`），`acquireSlot` 在 timer 到期后 `now = budget; continue` 再试一次准入（`:210-211`）⇒ 修掉了 round19/20 的"等满窗口仍 429"。
- 我实测：干净 HEAD 下 `TestProviderLaneRateIsASlidingMinute`、`TestRateOnlyLaneQueueCapacityAllowsWaiting` 等 9 个 lane 用例全绿（这也是 round18 起长期红的那个测试，现已正确修好）。
- 遗留小口子（不阻塞）：`providerQueueCapacity` 仍是 `max(rpm, max*4, 8)`，rpm=1000 时队列 1000 等于无上限（round18 就提过）；纯限流 lane 里 N 个 waiter 同一时刻到期，只有一个能真的准入，其余拿 429 —— 这是硬 RPM 的固有排队行为，可接受但要写进文档。

### WIP 做对的部分（本轮实测确认，不要回退）

| 项 | 证据 |
|---|---|
| 长流 usage（N11-1 线上 P0） | 探针：409,959 字节 Claude 流（64KB 分块喂入）→ `in:920 cache:120 out:4321`，与上游真实值一致；`closed()` 读全量字节判 true |
| 收尾判定不再看 256KB 头部（N10-1） | `usage.closed()` 由 `streamEndReader.scanClosing` 逐字节累计，`short`/`long` 流同判 |
| 负缓存扩展到 5 路 | `server.go` 中 Claude/Cursor/ChatGPT/Zhipu/DeepSeek 五个 handler 全部接入 `waitForRefresh` + `lastError`；`oauth_usage_cache.go` 指数退避封顶 10min、`needsRefresh` 到期即重试 |
| A4 overage | `cooldownHint` 纳入 `SevenDayOverageIncluded`，并区分"已用尽窗口"与"空闲窗口"（`exhaustedReset`） |
| 心跳 stop 真正 join（N11-2） | `sse_heartbeat.go` 增 `exited` channel，`stop()` 等 goroutine 退出后才返回 |
| HTTP 错误不再估算 token 计费 | `server.go` 把 `EstimateTokenUsage` 限定在 `!treatAsFailure` 分支（原来 429 会被按 200KB 正文估成 $1.12） |
| 全局价格行不再被状态保存清掉 | `store/model_prices.go` 跳过 `PriceWildcard` 作用域的 prune；`lookupPrice` 增加全局回落 |
| R2 阶梯真正可达 | 软失败窗口 60s→10min、rate 阶梯 `30s<<count` 封顶 30min；`bumpSoftFailure` 在 `markProviderUnavailable` 之前 ⇒ 第 3 次失败即 4 分钟，实测 `TestRateLimitRestBacksOff` 单调递增 |
| WorkBuddy 401 自愈 | 新增 `SignInAt` + 30min 假定寿命 + `forceRefreshWorkBuddyToken`（checkin / expiry / props 三处接线） |
| `upstreamTransport` 不再死代码 | `guardUpstreamIdleClient` 统一注入，27 处引用；`ResponseHeaderTimeout` 真正生效 |

### 仍存在的问题（按严重度）

**W21-3（P1）`idleGuardedBody.Read` 与 `Close` 竞态可空指针崩溃**
`upstream_timeout.go:122` 在 Read 尾部 `b.timer.Stop()`，而 `:141` 的 `Close()` 可能已把 `b.timer` 置 nil ⇒ `nil.Stop()` panic。`http.Client` 在 context 取消/连接复用时确实会在读途中 Close body。修：Read 里先判空，或用 `sync.Once` 保护 timer。

**W21-4（P1/P2）Responses 收尾帧的 `sequence_number` 对写入粒度敏感**
作者自己的用例 `TestWritePassThroughResponseClosesLongStreamWithItsIdentity`（3×256KB，787KB 流）实测收尾帧 `sequence_number=709`，正确。但我把同一份字节按 4KB/32KB 分块喂 `streamEndReader` 时，`lastSequence` 退化为 12/29 —— 因为 `scanResponses` 一旦拿到 `id`+`model` 就 `return`，`tailBytes` 冻结在首块，`cut()` 只从这个快照算最大序号。真实路径恰好因 `io.ReadAll` 增长式读取而没踩到。建议补一个分块边界矩阵测试（512B / 4KB / 32KB / 1 次写），别依赖"恰好没踩到"。

**W21-5（P2）转换流的中断判定仍然恒为 false（N14-2 未修）**
五处仍是"读之前按值快照"：`convert_claude_openai_stream.go:87`、`convert_claude_responses_stream.go:253`、`convert_openai_claude_stream.go:126`、`convert_openai_responses_chat_stream.go:576`、`:815`，全部形如 `interrupted := isUpstreamStreamInterruptedReader(r)`，而 `countingReader.Read` 是**指针接收者**，传值快照时 `r.err` 必为 nil ⇒ 截断的转换流仍会被当成正常结束（R4 对转换路径依旧无效）。passthrough 路径已改用 `usage.closed()`，转换路径没跟上。修：把判定挪到 `scanner.Err()` 之后，或传 `*countingReader`。

**W21-6（P2）中断收尾帧仍写裸 `w`，绕过心跳写锁**
`passthrough_stream.go:297` `end[0].closeInterruptedStream(w, ...)`：此时 `stopHeartbeat()` 还在 defer 里没执行，心跳 goroutine 可能正在写同一个 ResponseWriter ⇒ 帧交错 / 数据竞争（round10 的 N10-2，未修）。缓冲路径 `:423` 写的是 `fw`，是对的。改成传 `hw` 即可。

**W21-7（P2）`upstreamIdleTimeout = 120s` 硬编码**
图片生成（`/v1/images/*`，round19 提过的 101 长图）本来 `Timeout: 0`，现在任何上游 120s 不出字节即被判"上游停止发送"。要么让图片路由用独立更长的窗口，要么把该值做成 Provider 级配置。

**W21-8（P3）零散**
- `closingMarkers` 只有 `[DONE]` 有"回声守卫"，`message_stop` / `response.completed` 只要在任意分块出现即判闭流；正文里出现这两个词的长流会被误判为完整。
- `resetConsumed` 用 `ttl == providerSoftUnavailableTTL(60s)` 当"无 hint"哨兵，而 `30s<<1` 恰好也是 60s ⇒ 该分支会误判。
- `idleGuardedBody.reading` 是死字段（写不读）。
- `lookupPrice` 顺序是 provider 通配 > 全局精确（作者注释说明是有意的）；从"具体性优先"看顺序可疑，若确为设计意图请写进文档。
- `pricing` 之外，`web/src` 只动了 OAuth manual 模式与价格覆盖率面板，控制台 6 项（实时流/路由解释/会话视图/一键重放/成本列/告警角标）**本轮零进展**。

---

## 四、历史问题复查总账（第一轮提出的 60 项 + 后续新增）

图例：✅ 已修并复核 ｜ 🟡 部分 ｜ ❌ 原样 ｜ ⛔ 本轮回退/新伤

### 协议转换

| # | 问题 | 状态 |
|---|---|---|
| P1 | Claude→Chat 流式 usage 丢 0 | ✅ |
| P2 | stop_reason 不映射 refusal/pause_turn/context_window | ✅ |
| P3 | Chat→Responses 有 tool_calls 时正文清空 | ✅ |
| P4 | Responses→Chat 丢 reasoning 增量 | ✅ |
| P5 | Responses incomplete 当正常结束 | 🟡 透传侧已修，转换侧受 W21-5 影响仍可能漏 |
| P6 | Chat→Claude block index 错乱 | ✅ |
| P7 | tool_use ID 归一化碰撞 | ✅（哈希后缀） |
| P8 | Responses→Claude 缺合成 tool_result | ✅ |
| P9 | redacted_thinking 丢失 / reasoning 被丢 | 🟡 占位仍在，不可回放 |
| P10 | strict/top_k/stop_sequences/metadata 不转发 | ✅ 大部分；`stop` 进 Responses 的老坑已彻底堵住 |
| P11 | 两两手写转换 vs magpie 单一 IR | ❌ 架构性，不做 |

### 路由 / 调度

| # | 问题 | 状态 |
|---|---|---|
| R1 | 无 RPM 限流 | ✅ `8b5542d` 滑动窗口 + 纯限流排队 |
| R2 | 冷却不看上游 | ✅ 本轮达标（10min 窗口 + 30s<<n 阶梯 + Retry-After/resets_at 取用规则）；401 走 hard 15min 下限仍偏长 🟡 |
| R3 | billing 归进硬额度 | ✅ |
| R4 | 流截断无法续流 | 🟡 透传路径生效；转换路径因 W21-5 仍无效 |
| R5 | 会话粘性只在内存 | ✅ 落库（`session_pins`） |
| R6 | 并发闸门非 FIFO / 非每账号 | ⛔ WIP 改成 FIFO 后自死锁（W21-1） |

### 安全（S1–S5）

| # | 问题 | 状态 |
|---|---|---|
| S1 | 管理 API `ACAO: *` | ✅ `cors.go` 白名单 |
| S2 | 请求体无上限 / 无 ReadTimeout | ✅ `request_body_limit.go` + `ReadHeaderTimeout: 10s` |
| S3 | 登录/注册/自检无限流；setup 首个调用者可设密码 | 🟡 限流有，`/__auth/setup` 回环限制仍靠 Host 判断 |
| S4 | 正文不脱敏入库 | ✅ 请求 + 响应双向脱敏 |
| S5 | 上游头返回后无空闲超时 | 🟡 `idleGuardedBody` 已接入 27 处，但 120s 硬编码 + W21-3 竞态 |

### Provider 账号

| # | 问题 | 状态 |
|---|---|---|
| A1 | Trae/WorkBuddy/Cursor 刷新令牌无锁 | ✅ 统一 `renewLock` |
| A2 | Zhipu/DeepSeek 子串匹配 host 泄露 Key | ✅ 白名单 |
| A3 | `/v1/images/*` 回落全局 Key / 账号必 404 | ✅ |
| A4 | Claude 漏 overage 窗口；429 无负缓存 | ✅ 两项都做（负缓存 5/5 路） |
| A5 | Cursor 无过期提前量 | ✅（round10 已纠错为达标） |

### 控制台 6 项

实时请求流 ❌ ｜ 路由聚合视图 ❌ ｜ 会话视图 ❌ ｜ 一键重放 ❌ ｜ 日志成本列 🟡（仅详情弹窗）｜ 告警未读角标 ❌

---

## 五、验收门槛（下一轮必须逐条打勾）

1. `git diff --quiet HEAD` 为真（部署前），或部署脚本硬拒未提交状态。
2. `go test ./internal/gateway/ -timeout 300s` **不许依赖 300s 上限才结束**：正常应在 ~40s 内 `ok`。
3. lane 专项：`-run 'TestProviderLane|TestRateOnlyLane|TestProviderLaneWaiter' -count=20` 全绿（把超时路径重复跑，专抓死锁/计数泄漏）。
4. 新增断言：waiter 超时离开后 `waiterCount()` 归零、`serving == nextTick`。
5. `interrupted` 必须在读完之后判定：给 4 条转换路径各写一个"上游半路截断"用例，断言客户端收到 `finish_reason=length` / `response.incomplete`。
6. 中断收尾帧写 `hw`（心跳 writer），用 `-race` 跑一遍 SSE 用例。
7. `idleGuardedBody`：并发 Close/Read 用 `-race` 覆盖，禁止 nil timer。
8. Responses 收尾帧 sequence 补分块矩阵测试（512B/4KB/32KB/单次写）。
9. 生产验证：把任一 Provider 的 RPM 设为 e.g. 10，用 `ab -n 30 -c 30` 打 30 发，确认不再出现"请求永不返回"，且 429 带正确 `Retry-After`。
10. 部署后 `git log -1 --format=%h` 与二进制 mtime 能对上（把 commit 写进二进制：`-ldflags -X main.commit=$(git rev-parse --short HEAD)`）。

---

## 六、复现命令（均已实际执行）

```bash
# 1) 干净 HEAD 基线：绿
git worktree add --detach ../_head_r21 HEAD
cd ../_head_r21 && go build ./... && go test ./internal/gateway/ -timeout 400s
# => ok ... 38.850s

# 2) 工作区快照：300s 超时（3 个用例死锁）
git stash create review-snapshot   # 不动工作区
git worktree add --detach ../_wip_r21 <sha>
cd ../_wip_r21 && go test ./internal/gateway/ -timeout 300s
# => panic: test timed out after 5m0s
#    running: TestProviderLaneSkippedWaiterDoesNotHoldTheQueue / TestProviderLaneWaitTimeoutAndCancel / TestRateOnlyLaneQueueCapacityAllowsWaiting
#    栈：provider_lanes.go:361 retire -> :212 acquireSlot（持锁调用）

# 3) 生产二进制证据
grep -qaF "providerLane).retire" /opt/llm-protocol-gateway/bin/gateway && echo PRESENT
grep -qaF "upstream stopped sending mid-stream" /opt/llm-protocol-gateway/bin/gateway && echo PRESENT
ls -l --time-style=full-iso /opt/llm-protocol-gateway/bin/gateway   # 2026-10-22 22:24:57 -0700 = 13:24:57 +0800
```

```sql
-- 4) 生产限流配置（只读）
select id,max_concurrent_requests,queue_wait_seconds,requests_per_minute from providers;
-- => 44 行，三项全 0
```
