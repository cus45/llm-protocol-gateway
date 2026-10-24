# 第二十二轮：另一 agent 代码审查（round22）

- 审查时间：2026-10-23 14:02 → 14:12 (+0800)
- 基线 HEAD：`8b5542d`（`fix(lanes): RPM 改为滑动分钟窗口，纯限流 Provider 可排队`）
- 范围：工作区未提交改动 47 改 + 6 新增（+2424 / −401），指纹 `sha256(git diff HEAD)=f86d25eb…`，**审查全程未变**（14:02 与 14:10 一致，最新 mtime 13:35:34）
- 未修改任何业务代码；探针文件已删除，工作区回到审查前状态（53 条 git status，与开始时一致）

---

## 一、结论

**不可合入、更不可部署。** 方向对的比例是历轮最高（lane 死锁修好了、A4 五路齐、WorkBuddy 401 自愈闭环、失败请求不再假记账），但有三件事必须先处理：

1. **线上又跑着未提交的 WIP，而且正是带死锁的那一版。** VPS 二进制 `2026-10-22 22:24:57 -0700`（= 13:24:57 +0800），含 `isMyTurnLocked`、不含 13:35 才写的 `strikeLocked` ⇒ 生产是新旧两版之间的快照，恰好是 round21 判定"持锁调 retire ⇒ 自死锁"的实现。44 个 Provider 限流三项全 0 ⇒ lane 未启用 ⇒ 侥幸没爆。这是连续第 4 轮把未提交代码推上生产。
2. **lane FIFO 只对"已经排进队列的请求"成立**，新到的请求不看 turn 直接抢槽（`provider_lanes.go:195`），持续新流量可饿死队首 ⇒ R6 只算修了一半。
3. **本轮的 core（`streamEndReader` + `idleGuardedBody`）有两个可复现缺陷**：中断收尾帧的 id/sequence 在分块输入下算错（探针：815KB 流真实最后序号 8000，收尾帧写 325）；`idleGuardedBody` 并发 Close 时 `nil.Stop()` 崩溃（探针实证 panic）。二者当前被 `writeBufferedStream` 的 `io.ReadAll` 整段缓冲**掩盖**——也就是说，等你们按前几轮建议恢复真流式，它们会立刻变成线上问题。

基线：`go build ./...` 绿、`go vet ./...` 静默、`go test ./internal/gateway/` **38.48s ok**、lane 专项 `-count=10` 3.12s ok、`web tsc -b` 0 error、`gofmt` 仅 `internal/store/{sqlite.go,session_pins_test.go}`（非本轮文件）。`-race` 本轮不可用（`CGO_ENABLED` 关闭，`go test -race` 直接拒绝）。

---

## 二、本轮做对的（逐条复核，不是听作者说）

| # | 改动 | 复核结论 |
|---|---|---|
| ✅ G1 | `provider_lanes.go` 重写：`retire` 分持锁/不持锁两版（`:355`/`:365`），`acquireSlot` 里三处退队都走正确的那一版 | **W21-1 自死锁已修**。`-run 'TestProviderLane\|TestRateOnlyLane…' -count=10` 3.1s 全绿（round21 是 300s 超时） |
| ✅ G2 | 唤醒之后 `now = time.Now()`（`:241`） | round21 我提的"队首醒了却拿旧时钟判额度"一并修好 |
| ✅ G3 | A4 负缓存 5/5：claude / cursor / chatgpt / zhipu / deepseek 的 handler 都有 `else if !forceRefresh` + `waitForRefresh` 分支，成功 `clearFailure`、失败 `setFailure`（指数退避 1min→10min 封顶） | 达标，且"stale 值旁边显示失败原因"也做了 |
| ✅ G4 | WorkBuddy 401 自愈闭环：`SignInAt` + 30min 假定寿命 + `forceRefreshWorkBuddyToken`（带 renewLock + 锁内重读）接进 checkin / usage / 发送 401 重试 | N-1 关闭；`sendWorkBuddyChat` 的 401→刷新→重发是正确形态 |
| ✅ G5 | `recordRequestLogEx`：失败请求不再用 body 长度估 token（`server.go:1330`） | round19 的"429 打 200KB prompt 记 $1.12"真被修掉，且有正反两个用例（失败不估算 / 失败但厂商报了 usage 仍计价） |
| ✅ G6 | 价格表全局 scope + `pruneModelPrices` 不再把 `providerId='*'` 当过期行删掉 | 后者是**真 bug**：一次 state 保存（令牌刷新即可触发）就会删掉全部共用价 |
| ✅ G7 | `startSSEHeartbeat.stop()` join goroutine（`sse_heartbeat.go:44`） | N11-2 关闭（但引入新的阻塞面，见 V22-12） |
| ✅ G8 | Claude 授权 manual 模式 + 未认领回调页 | `stdhtml.EscapeString` 转义到位，无反射 XSS；`/callback?state=<script>` 打不出脚本 |
| ✅ G9 | Claude 令牌后台轮换（30min 提前量，15min 扫一轮） | 与 ChatGPT 那套同构，注册进 `runtime.go:303` |
| ✅ G10 | 中断流返回厂商已报的 usage（`passthrough_stream.go:300`），不再一律 `TokenUsage{}` | 长流被截断时不再把已花的钱记成 0 |

另外：本轮 WIP **没有** println 调试残留（前几轮的老毛病），也没有半成品测试；新增 6 个测试文件都不是空转用例。

---

## 三、必须修

### V22-1（P0，治理）生产=未提交 WIP，且是死锁版

```
/opt/llm-protocol-gateway/bin/gateway  2026-10-22 22:24:57 -0700
isMyTurnLocked => PRESENT      strikeLocked => ABSENT
streamEndReader => PRESENT     usage probe failed => PRESENT
providers: 44，max_concurrent/queue_wait/rpm 三项非零者 = 0
```

`strikeLocked`/`passTurnLocked`/`retireLocked` 是 13:35 那次重写才有的符号，线上缺 `strikeLocked` 却含更早的 `isMyTurnLocked` ⇒ 线上跑的是 13:21 那版（round21 的 W21-1 死锁版）。同时它又是**未提交**代码（HEAD `8b5542d` 没有 `streamEndReader`）。两个问题叠加：出问题时既无 commit 可回滚，退回 HEAD 又会丢掉一整批修复。

建议：先在本地把当前工作区按主题拆 4~5 个提交（协议/调度/安全/provider/价格），跑绿后 `./deploy/deploy-linux.sh` 一次上机；部署脚本加硬门槛 `git diff --quiet HEAD`（round21 门槛 1 至今未落地）。

### V22-2（P2，R6 半修）新到请求可以插队，队首可能被饿死

`provider_lanes.go:195`：

```go
if !queued || lane.turnLocked(ticket) {   // ← 未排队的请求直接跳过 turn 校验
    if lane.admitLocked(now) { ... lane.inflight++ ... }
}
```

队列非空时，只要 `inflight < max`，**新请求可以立刻拿槽**。释放槽位时 `release()` 广播，队首需要醒来抢锁，而新请求是在自己 goroutine 里已经持锁的——高频到达场景下队首会持续输掉这场竞争。`TestProviderLaneWakesWaitersInArrivalOrder` 只验证"两个已排队者的先后"，没有新请求插入，所以测不出来。

修法：`len(lane.queue) > 0 && !turnLocked(ticket)` 时一律入队，不抢槽（即未排队者也要排在队尾）。

### V22-3（P2，会 panic）`idleGuardedBody.Read` 尾部对可能已被 Close 置 nil 的 timer 调 Stop

`upstream_timeout.go:102-126`：Read 在锁内装 timer，出锁读 body，再进锁 `b.timer.Stop()`；`Close()`（`:138`）同样在锁内把 `b.timer = nil`。Close 插在两次加锁之间 ⇒ `nil.Stop()`。

探针（两个 goroutine：一个阻塞在 Read，一个 Close）：

```
PROBE-B CONFIRMED panic: runtime error: invalid memory address or nil pointer dereference
```

现实可达性偏窄（main 路径的 Close 是同 goroutine defer；`http.Client.Timeout` 关的是 transport 内层 body，不是这个 wrapper），但这是"迟早被别的调用方踩到"的一行 bug，加 `if b.timer != nil` 即可。顺带：`reading bool`（`:93`）是死字段。

### V22-4（P2，恢复真流式后立刻踩）收尾标记只有 4 字节跨块携带

`stream_end_reader.go:19` `tail [4]byte` + `:90` 只留 4 字节。标记最短 6 字节（`[DONE]`），跨块时若前一共有 ≥5 字节在上一块，拼起来就不含完整标记：

```
PROBE-C 前一共有 31 字节（标记完好）closed=true
        前一共有 26 字节（"messa"+5 字节在第二块）closed=false   ← 完整流被误判为截断
```

后果：**一个完整收尾的流被当成截断** ⇒ 客户端多收一帧 `max_tokens`/`finish_reason=length`（Chat 还会多一个 `[DONE]`），并计 Provider 软失败（3 次进冷却）。修法：tail 至少留 16 字节。

### V22-5（P2，同上掩盖条件）中断收尾帧的 id / sequence_number 会算错

`stream_end_reader.go:98-108`：`scanResponses` 一旦看到子串 `response.created` 就把 `id/model` 定下来并 **`return`**，`tailBytes` 从此冻结在首块；`cut()` 只从这个快照里取最大 `sequence_number`。

探针（按 `io.Copy` 的 32KB 分块喂 815KB Responses 流，最后一条事件 `sequence_number=8000`）：

```
PROBE-A tailBytes=32768 seen=815017
        real last sequence_number=8000; reader kept lastSequence=324
        closing frame would use 325
```

按 200B 分块更糟：首块里 `"type":"response.created"` 出现但 JSON 被截断 ⇒ 连 id 都没解析出来，收尾帧另生成一个 `resp_<nano>`，客户端会看到一个**换了 id 的 response**。

当前 `/v1/responses` 走 `writeBufferedStream`（`io.ReadAll` 一次 Write，tailBytes=全文）所以没暴露——注释里"同一个 787KB 流实测 709 正确"就是这么来的。**这正是前几轮一直在要求去掉的整段缓冲**：一旦恢复真流式，收尾帧立刻错。

修法：`scanResponses` 不要因为 `id/model` 已知就停止累积（或只在尾部保留最近 2×日志上限并每块更新 `lastSequence`），并把 `id/model` 的确定改为"JSON 解析成功"而非子串命中。

### V22-6（P2）`stream=true` 但上游回 JSON 时 usage 归零

`usage.go:497` `final()`：`fold != nil` 时**完全忽略** `body` 参数，只返回折叠结果；而非 SSE 的 JSON body 没有 `data:` 行，折叠永远为空。

```
PROBE-D stream-reader usage={0 0 0 0}   whole-body parser={1234 567}
```

影响的调用点：`qoder_chat.go:73`、`trae.go:1838`、`server.go:5395/5459`。上游忽略 `stream` 直接回 JSON（或某些中转对长 prompt 回退非流式）时，这一条请求的 token 就是 0，随后被 `recordRequestLogEx` 的估算兜底放大成 `len/4`。修法：`final()` 在折叠为空时回落 `parseUsage(body)`。

### V22-7（P2，W21-6 原样）中断收尾帧仍写裸 `w`

`passthrough_stream.go:297` `end[0].closeInterruptedStream(w, …)`：此时 `stopHeartbeat()` 还在 defer 里没跑，心跳 goroutine 可能正在写同一个 ResponseWriter ⇒ 帧交错 / 数据竞争。缓冲路径 `:423` 传的是 `fw`，是对的。改传 `hw` 即可。

### V22-8（P2，W21-5 原样，R4 对转换路径无效）中断判定恒 false

5 处仍是"读之前按值快照"：`convert_claude_openai_stream.go:87`、`convert_claude_responses_stream.go:253`、`convert_openai_claude_stream.go:126`、`convert_openai_responses_chat_stream.go:576`、`:815`：

```go
r := countingReader{r: reader}
scanner := bufio.NewScanner(&r)
interrupted := isUpstreamStreamInterruptedReader(r)   // r.err 此刻必为 nil
for scanner.Scan() { … }                              // 后续写入不影响已算出的 bool
```

`isUpstreamStreamInterruptedReader` 收的是值（`r.err == io.EOF`），判定必须挪到 `scanner.Err()` 之后，或改成 `*countingReader`。也就是说 R4（流截断续流）对四条转换路径**至今没生效**。

### V22-9（P2）图片透传路径被 120s idle 砍

`server.go:5553` `client := &http.Client{Timeout: 0}` + `guardUpstreamIdleClient` ⇒ 每次 Read 计时 120s。出图常常整段无字节（60~180s），round19 的"101 长图被砍"在 **Cursor 分支**修了（`imageGenerationTimeout = 10min`，`:5527`），**OpenAI 兼容分支没修**（`proxyOpenAIImagesUpstream`）。要么给它 10min，要么让 `guardUpstreamIdle` 接受一个窗口参数。

### V22-10（P2）401 + hardAuth 命中 ⇒ 硬冷却 15min

`classifyProviderFailover`（`provider_failover.go:156`）里 401/403 命中 `hardAuthHints` 归 `failoverHard`，而本轮把硬额度的下限从 2min 提到 `providerQuotaRestFloor=15min`（`:59`）。对会自愈的 OAuth 账号（WorkBuddy 现在已经能 401 自愈）来说，一次"账号在别处登录导致令牌被撤"会让整个账号白等 7.5 倍。建议 hardAuth 走独立的短阶梯（2~5min），别复用额度下限。

### V22-11（P3）401 刷新失败时把上游的 401 换成 transport error

`workbuddy.go:1032`：`return nil, refreshErr`——原本那个 401 body 已经读掉丢弃，客户端只能看到"刷新失败"，无法区分"令牌失效"和"上游炸了"。同理 `workBuddyQuota` 的 refresh 闭包失败时返回 `current`，后续请求会再吃一次 401。建议保留原响应（body 重放）或把刷新失败包进错误文本而仍返回原 401。

### V22-12（P3）心跳 stop 现在会阻塞 handler

`sse_heartbeat.go:45` `<-exited` 是正确的（避免响应已返回还在写），但没有任何写超时：客户端开连接不读（背压）时，心跳的 Write 可能永久阻塞 ⇒ `stop()` 永久阻塞 ⇒ handler 不返回 ⇒ Provider lane 槽位不释放。建议给 ResponseWriter 加写 deadline，或心跳写走 `context.WithTimeout`。

### V22-13（P3）零散

- `closingMarkers` 只有 `[DONE]` 有回声守卫（`:80`），正文里出现 `message_stop` / `response.completed` 的长流会被误判"已完整"（round21 W21-8 第一条，未修）。
- `resetConsumed` 用 `ttl == providerSoftUnavailableTTL(60s)` 当"无 hint"哨兵（`provider_failover.go:718`），而 `30s<<1` 恰好也是 60s ⇒ 误判。
- `scanClosing` 每个 chunk 对 5 个标记做 `bytes.Index` + 一次 `append` 复制（`:79`），Claude 流还要逐行 `json.Unmarshal`（`:118`）——热路径 CPU 成本，建议用 `bytes.Contains` 前置过滤。
- `providerConcurrencyConfig` 的 `queueCap = rpm`（等于不设上限），round18 提过，仍在。
- 仓库里 5 个 `.bak`（`chatgpt_oauth.go.bak`、`claude_oauth.go.bak`、`cursor_oauth.go.bak`、`qoder_pat.go.bak`、`qoder_pat_handlers.go.bak`）。
- `lookupPrice` 的优先级是 provider 通配 > 全局精确（`:99`）；作者注释说明是有意的，但"全局精确价"被"Provider 兜底价"压过，从具体性看可疑，至少在 UI 上说清楚。

---

## 四、上轮遗留复核（W21-x）

| 编号 | round21 判定 | 本轮现状 |
|---|---|---|
| W21-1 lane 自死锁 | P0 | ✅ 已修（重写 + `-count=10` 绿）；但**线上仍是死锁版**（V22-1） |
| W21-2 幽灵占满 queueCap | P1 | ✅ 随重写修掉（`queue` 是 ticket 切片，退队即删） |
| W21-3 idle 守卫 nil timer | P1 | ❌ 原样，探针实证 panic（V22-3） |
| W21-4 Responses 收尾 sequence | P1/P2 | ❌ 原样，且分块探针给出量化偏差（V22-5） |
| W21-5 转换路径中断判定 | P2 | ❌ 原样（V22-8） |
| W21-6 收尾帧写裸 w | P2 | ❌ 原样（V22-7） |
| W21-7 120s 砍长图 | P2 | 🟡 只修 Cursor 分支（V22-9） |
| W21-8 零散（回声守卫/哨兵/死字段/价格顺序） | P3 | 🟡 `reading` 死字段仍在；其余原样 |

---

## 五、历史 60 项总账（截至本轮）

图例：✅ 已修并复核 ｜ 🟡 部分 ｜ ❌ 原样 ｜ ⛔ 回退/新伤

### 协议转换

| # | 问题 | 状态 |
|---|---|---|
| P1 | Claude→Chat 流式 usage 丢 0 | ✅ |
| P2 | stop_reason 不全量映射 | ✅ |
| P3 | Chat→Responses 有 tool_calls 时正文清空 | ✅ |
| P4 | Responses→Chat 丢 reasoning 增量 | ✅ |
| P5 | Responses incomplete 当正常结束 | 🟡 透传侧修，转换侧受 V22-8 影响仍可能漏 |
| P6 | Chat→Claude block index 错乱 | ✅ |
| P7 | tool_use ID 归一化碰撞 | ✅ |
| P8 | Responses→Claude 缺合成 tool_result | ✅ |
| P9 | redacted_thinking / reasoning 丢失 | 🟡 占位仍在 |
| P10 | strict/top_k/stop_sequences/metadata 不转发 | ✅ 大部分；`stop` 进 Responses 已堵 |
| P11 | 两两手写转换 vs magpie 单一 IR | ❌ 架构性，不做 |
| P12（新）| 中断收尾帧 identity/sequence 不可靠 | ⛔ V22-5 |
| P13（新）| 完整流被误判截断（4B 携带） | ⛔ V22-4 |
| P14（新）| stream=true + JSON body → usage 0 | ⛔ V22-6 |

### 路由 / 调度

| # | 问题 | 状态 |
|---|---|---|
| R1 | 无 RPM 限流 | ✅ `8b5542d` 滑动窗口 |
| R2 | 冷却不看上游 | ✅ 10min 窗口 + `30s<<n` 阶梯 + Retry-After/resets_at 取用规则，用例到位；401+hardAuth 15min 偏长 🟡 |
| R3 | billing 归进硬额度 | ✅ |
| R4 | 流截断无法续流 | 🟡 透传侧生效；转换侧因 V22-8 无效 |
| R5 | 会话粘性只在内存 | ✅ 落库 |
| R6 | 并发闸门非 FIFO / 非每账号 | 🟡 已排队者之间 FIFO，新到请求可插队（V22-2）；仍非"每账号" |

### 安全（S1–S5）

| # | 问题 | 状态 |
|---|---|---|
| S1 | 管理 API `ACAO: *` | ✅ `cors.go` 白名单 |
| S2 | 请求体无上限 / 无 ReadTimeout | ✅ `request_body_limit.go` |
| S3 | 登录/注册/自检无限流、setup 首个调用者可设密码 | 🟡 有限流 + 实例在用守卫，仍靠 Host 判回环 |
| S4 | 正文不脱敏入库 | ✅ 请求+响应双向 |
| S5 | 上游头返回后无空闲超时 | 🟡 覆盖面扩到 20 处，但 120s 硬编码 + V22-3 + 图片被砍 |

### Provider 账号

| # | 问题 | 状态 |
|---|---|---|
| A1 | Trae/WorkBuddy/Cursor 刷新无锁 | ✅ 统一 renewLock |
| A2 | 子串匹配 host 泄露 Key | ✅ 白名单 |
| A3 | `/v1/images/*` 回落全局 Key | ✅ |
| A4 | Claude 漏 overage / 429 无负缓存 | ✅ 两项齐（5/5 路） |
| A5 | Cursor 无过期提前量 | ✅（round10 已纠错） |
| A6（旧 N-1）| workbuddy_ai 永不过期 | ✅ 401 自愈 + SignInAt |

### 控制台 6 项

实时请求流 ❌ ｜ 路由聚合视图 ❌ ｜ 会话视图 ❌ ｜ 一键重放 ❌ ｜ 日志成本列 🟡（仅详情弹窗）｜ 告警未读角标 ❌
本轮 `web/` 只动了价格表（全局 scope、按模型覆盖率）与 Claude manual 授权，**6 项零进展**。

---

## 六、验收门槛（下一轮逐条打勾，不勾不许部署）

1. `git diff --quiet HEAD` 为真，或部署脚本硬拒未提交状态（已连续 4 轮未落地）。
2. 部署后二进制 mtime 与 `git log -1 --format=%h` 对得上（把 commit 编进去：`-ldflags -X main.commit=…`）。
3. `go test ./internal/gateway/ -timeout 300s` 在 ~40s 内 `ok`，不是靠超时才结束。
4. lane 专项 `-count=20` 全绿；新增"新请求不得越过已排队者"用例（V22-2）。
5. `idleGuardedBody` 并发 Close/Read 用 `-race` 覆盖，禁止 nil timer（本轮环境 `CGO_ENABLED=0`，需先打开）。
6. `streamEndReader` 补分块矩阵测试：512B / 4KB / 32KB / 单次写 × 完整流 / 截断流，断言 `closed()` 与 `cut()` 的 id、model、lastSequence。
7. 4 条转换路径各写一个"上游半路截断"用例，断言客户端收到 `finish_reason=length` / `response.incomplete`（V22-8）。
8. 中断收尾帧改走 `hw`，SSE 用例跑 `-race`（V22-7）。
9. `stream=true` + JSON 回包的 usage 断言 ≥ 报文字段（V22-6）。
10. 图片路径用 180s 静默的上游桩测一次，不得被封（V22-9）。
11. 拆提交：协议 / 调度 / 安全 / provider / 价格 UI 分开，每个提交自带用例。

---

## 七、复现命令（本轮实际执行）

```bash
# 基线
go build ./... && go vet ./...                       # 绿
go test ./internal/gateway/ -timeout 600s            # ok 38.480s
go test ./internal/gateway/ -run 'TestProviderLane|TestRateOnlyLane|TestProviderLaneRate|TestProviderLaneWaiter' -count=10
(cd web && npx tsc -b)                               # 0 error
gofmt -l internal/ web/src                           # 仅 internal/store/{sqlite.go,session_pins_test.go}

# 线上版本判定（只读）
ssh root@173.249.207.202 'ls -l --time-style=full-iso /opt/llm-protocol-gateway/bin/gateway; \
  for s in isMyTurnLocked strikeLocked streamEndReader; do \
    printf "%s => " "$s"; grep -qaF "$s" /opt/llm-protocol-gateway/bin/gateway && echo PRESENT || echo ABSENT; done'
python3 - <<'PY'
import sqlite3
c=sqlite3.connect("file:/var/lib/llm-protocol-gateway/gateway.db?mode=ro",uri=True)
print(c.execute("select count(*) from providers where coalesce(max_concurrent_requests,0)>0 or coalesce(requests_per_minute,0)>0 or coalesce(queue_wait_seconds,0)>0").fetchone())
PY
```

四条探针（本轮临时写入 `internal/gateway/zz_probe_test.go`，跑完即删）：

```
PROBE-A tailBytes=32768 seen=815017; real last sequence_number=8000; kept 324; closing uses 325
PROBE-B CONFIRMED panic: runtime error: invalid memory address or nil pointer dereference
PROBE-C 前一共有 31 字节 closed=true / 26 字节 closed=false
PROBE-D stream-reader usage={0 0 0 0}  whole-body parser={1234 567}
```
