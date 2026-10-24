# 第 20 轮审查报告（另一 agent 改动）

- **审查时间**：2026-10-23 12:13–12:28 (+0800)
- **HEAD**：`b1e935c fix(logs): 裸 token 字段同样脱敏`（10:44）
- **审查范围**：工作区未提交改动，41 改 + 6 新增，`+1846 / -286`
  - 快照指纹：`sha256(git diff HEAD) = ffef332161de9a94e9823ec3807d87381268d1cc9aa49873f57a17b610bb2ab2`
  - 审查结束时指纹：`cba6c2116e0bdb26b60ed2baf383d80a3881c0105bfde8de30e110772979bbbc`（12:18:05 `provider_lanes.go` 又有编辑，已逐条复核本报告引用的行号与结论未变）
  - 快照文件：`.workbuddy/reviews/round20-wip-snapshot.patch`
- **审查方式**：快照后 `git worktree` 隔离复现（HEAD + patch），在隔离树里跑全量测试与探针；**未修改工作区任何文件**。
- **变动强度**：审查期间对方 agent 仍在实时编辑（12:13:28 workbuddy.go、12:13:41 types.go、12:13:50 workbuddy.go）。12:13:39 的快照**编译不过**（`workbuddy.go:629 credential.ObtainedAt undefined`），12:13:50 之后自行修成 `SignInAt` 并补了 `domain.WorkBuddyCredential.SignInAt`。

---

## 一、结论

| 维度 | 结论 |
|---|---|
| 可编译 | ✅ 当前快照 `go build ./...` / `go vet ./internal/...` 全绿（12:13:41 曾有一分钟不可编译窗口） |
| 测试 | ✅ **首次多轮以来全绿**：`go test ./internal/gateway/` 98.06s，0 红；`go test ./...` 仅 `internal/tunnel` 失败（`/bin/sh` 不在 Windows PATH，环境性、与改动无关）；`web: tsc -b` 0 error |
| 代码质量 | 🟡 新增测试是**真测试**（断言实现行为，非空转）；但 `TestUpstreamTransportHasHeaderTimeout` 守的是一份生产零引用的 transport，属"测死代码" |
| 生产状态 | ❌ **未提交的 WIP 已于 12:15 (+0800) 部署上 VPS**，见 W20-1 |
| 上轮遗留 | ❌ 头号问题 N10-1 仍在线上；本轮新增 5 条 P1，其中 3 条在线上 |

一句话：**这轮的功能方向（长流用量、A4 负缓存、WorkBuddy 令牌生命周期、价格表全局行）基本正确且测试是真的，但三处"新增正确逻辑被旧 bug 抵消""新逻辑只接在一条路径上"，加上未提交即上线，仍不建议以现状收口。**

---

## 二、本轮改动逐项判定

| 改动 | 判定 | 依据 |
|---|---|---|
| **N11-1** `streamUsageReader`（`usage.go` 新增 ~180 行） | 🟢 解析器正确，**但收益在透传路径被 N10-1 抵消** | 隔离树跑 `TestStreamUsageReaderReportsUsageBeyondLogCap` / `LateUsage` / `ChunkBoundaries` 全绿；探针确认 558KB 流的 usage 在**返回路径**被 `return TokenUsage{}` 丢掉（见 W20-2） |
| **N11-4** Responses 收尾帧带 id/model/sequence_number | 🟡 只在缓冲路径生效，透传路径发出**重复 sequence 0** | 探针：透传中断帧 `{"id":"resp_<nanos>","sequence_number":0}`，而客户端已收 0..5（W20-3） |
| **N11-2** 心跳 `stop()` join goroutine | 🟢 修复正确 | `sse_heartbeat.go:43-48`，`<-exited` 保证 stop 返回时不再有写；副作用见 W20-11 |
| **A4** `seven_day_overage_included` | 🟢 正确 | `claude_oauth_usage.go:32/97/101`，解析 + 计入"报告可用"判定 |
| **A4** 负缓存（`setFailure`/`clearFailure`/`waitForRefresh`，指数退避到 10min） | 🟡 机制正确、接入不全 | 5 个 refresh 函数都接 `setFailure/clearFailure` ✅；`waitForRefresh` 仍只在 Claude 同步 handler（`server.go:2647`）1/5；错误在有 stale 值时不可见（W20-10） |
| **N-1 + A1** WorkBuddy 令牌生命周期 | 🟢 **本轮最扎实的一处** | ①`SignInAt` 老化替代"永不过期"（`workbuddy.go:630`）②`refreshWorkBuddyTokenLocked` 统一锁内重读+持久化 ③401 → 强制刷新 → 单次重放，聊天（`sendWorkBuddyChat`）、额度（`workBuddyQuota`）、签到（`workBuddyCheckin`）三条路都接；`forceRefreshWorkBuddyToken` 用 `Lock` 而非 `TryLock`，锁内重读，`retire` 在 persist 之后 ✅ |
| **Claude** 后台令牌轮换（30min 提前量 + invalid_grant 告警） | 🟢 正确 | `oauth_usage_refresh.go:87-140`，`claudeTokenNeedsRefreshWithin` 与请求路径 5min 轴分离，测试 `TestClaudeTokenRefreshLead` 有效 |
| **Claude OAuth** 远程控制台改 manual + 回调未被认领页 | 🟢 正确（含 XSS 防护） | `requestHostIsLoopback` 有表驱动测试；未认领页 `stdhtml.EscapeString` 转义回调串，测试断言 `&amp;`；manual 分支确实 `pendingClaudeOAuth.put`，粘回可收尾 |
| **R1** 滑动窗口 `admits []int64` | 🟢 实现正确，🐞 排队语义仍坏 | 滑窗/trim/`retryAfterSeconds` 都对了；上轮红测试改成 65s 后转绿且语义正确；**rate-only 排队仍 100% 失败**（W20-6） |
| **R2** 双阶梯（rate 30s→30min / quota 15min floor） | 🟡 方向对，两处判据有问题 | 429 不再被 6 天窗口拉成 1h ✅；但 resets 取"最早"会误判（W20-8）、软阶梯实际不可升（W20-9） |
| **S5** idle 守卫扩到 15 处调用点 | 🟡 机制可用，**误判慢消费者** | 探针实证（W20-5）；`upstreamTransport` 仍是死代码（W20-12） |
| **价格表全局行**（`providerId="*"`） | 🟢 正确、有测试、UI 同步 | `lookupPrice` 四级查找；`handleSavePrice` 放行 `*`；`pricing.tsx` 加"全局"范围 + 查找顺序说明；`tsc -b` 通过。注意优先级"Provider 默认 > 全局精确"是刻意选择，需在文档里保持（已写进 UI 文案） |

---

## 三、本轮新发现问题

### 汇总表

| # | 级别 | 问题 | 位置 | 状态 |
|---|---|---|---|---|
| W20-1 | **P0** | 未提交 WIP 已部署到生产，无提交可回滚 | VPS `/opt/llm-protocol-gateway/bin/gateway` | 线上 |
| W20-2 | **P0** | >256KB 完整流仍被判截断；usage 在返回路径被丢弃 | `passthrough_stream.go:372,378` | 线上 |
| W20-3 | **P1** | 透传路径 Responses 收尾帧发 `sequence_number:0` + 伪造 id | `passthrough_stream.go:131,376` | 线上 |
| W20-4 | **P1** | `streamUsageReader.pending` 永不截断（重复解析 + 1MB 清零丢帧） | `usage.go:474-487` | 未上线（随 WIP 上线） |
| W20-5 | **P1** | idle 守卫把"慢消费者"判成"上游静默" | `upstream_timeout.go:85-120` | 线上 |
| W20-6 | **P1** | rate-only lane 排队等满窗口后仍 429（无最后一次准入尝试） | `provider_lanes.go:213-217` | 未启用则无影响 |
| W20-7 | P2 | 软失败阶梯实际只停在第一档（窗口 60s ≤ 最短 rest 60s） | `provider_failover.go:638` | — |
| W20-8 | P2 | 冷却取"最早 resets_at"，周额度耗尽时只歇 9 分钟 | `provider_failover.go:567-600` | — |
| W20-9 | P2 | 负缓存错误在有 stale 值时永远不展示（`else if` 不可达） | `server.go:2637-2661` | — |
| W20-10 | P2 | `stop()` join 后可能长时间阻塞（向死连接写） | `sse_heartbeat.go:45-48` | — |
| W20-11 | P2 | `upstreamTransport` 生产零引用，`ResponseHeaderTimeout` 等于没设 | `upstream_timeout.go:47` | 沿用上轮 |
| W20-12 | P2 | 120s idle 守卫套到出图路径（原 `Timeout:0`） | `server.go:5447,5473` | 线上 |
| W20-13 | P2 | 401/403 hardAuth 吃 15 分钟 quota floor | `provider_failover.go:150-160` | 沿用上轮 |
| W20-14 | P3 | `upstream_timeout.go` gofmt 不过（多一个空行）；3 个 `.bak` 仍在树里 | — | — |
| W20-15 | P3 | `usageEvent` 上一个函数被写了两段 doc 注释 | `usage.go:125-133` | — |

### W20-1 · 未提交代码进了生产（P0，治理问题）

```
2026-10-22 21:15:19 PDT（= 2026-10-23 12:15 +0800）  /opt/llm-protocol-gateway/bin/gateway
进程 etime 03:59（12:15 启动），https :18093 健康 200
```

二进制内含**只存在于未提交工作区**的字符串：

| 特征串 | 所在改动 | 结果 |
|---|---|---|
| `upstream stopped sending mid-stream` | `upstream_timeout.go`（未提交） | PRESENT |
| `usage probe failed` | `oauth_usage_cache.go` 负缓存（未提交） | PRESENT |
| `seven_day_overage_included` | `claude_oauth_usage.go`（未提交） | PRESENT |
| `没有被当前网关认领` | `claude_oauth.go` 未认领页（未提交） | PRESENT |

同时该快照在 12:13:39 有一分钟编译不过。**结论**：生产跑的是"未提交 + 正在被改"的树，且没有 commit 可回滚；出问题时只能靠猜。这一条本身比任何单个 bug 都危险。

### W20-2 · 长流 usage 的修复被旧 bug 吃掉（P0，线上）

探针（隔离树，558KB Chat SSE，无 Content-Length，结尾 `usage` + `[DONE]`）：

```
PROBE stream_bytes=558121
PROBE passthrough_usage={InputTokens:0 OutputTokens:0 CacheTokens:0 CacheWriteTokens:0}
      err=upstream stream interrupted after 558121 bytes: unexpected EOF
PROBE long_complete_stream err=<interrupted> done_frames=2 finish_frames=1 out=558270
PROBE with_content_length err=<nil> out=in=558014
```

三件事同时发生：
1. `isUpstreamStreamInterruptedResponse(response, closesUpstreamStream(tee.Bytes()))` 用的仍是**头部 256KB 副本**（`passthrough_stream.go:372`）→ 完整流被判截断（N10-1 未修）；
2. 判截断后**返 `TokenUsage{}`**（`:378`）→ 本轮新写的 `streamUsageReader` 明明看到了全部字节，结果仍记 0（N11-1 净效果未修复）；
3. 客户端多收一份收尾：`[DONE]` 出现 2 次、多一帧 `finish_reason`，且该次请求计入 Provider 软失败。

带 `Content-Length` 时判定正确（`out == in`），说明判据本身可用，缺的是"用实读字节而不是头部副本"。

**修法（任选其一，推荐 2）**：
1. `isUpstreamStreamInterruptedResponse` 增加"实读字节 == ContentLength"判定（上轮 N14-3）；
2. 让 `streamUsageReader` 顺带记录"是否见到收尾帧"（它本来就逐行扫全部字节），用它替代 `tee.Bytes()`；截断时也返回 `usage.final(...)` 而不是零值。

### W20-3 · 透传路径的 Responses 收尾帧序列号重复（P1，线上）

本轮给 `responsesStreamInterrupter` 加了"复用 id/model/sequence_number"的能力，但**只有缓冲路径传了 body**：

- `writeBufferedStream` → `end[0].closeReplay(fw, streamBody)` ✅
- `writePassThroughResponse` → `end[0].closeInterruptedStream(w)` → `closeReplay(w, nil)` ❌

探针（客户端已收 `sequence_number` 0..5，然后流被切断）：

```
closing="event: response.incomplete
data: {"response":{"id":"resp_1792729151492894000","incomplete_details":{...},"object":"response","status":"incomplete"},"sequence_number":0,"type":"response.incomplete"}"
```

注释写着"a closing frame with a fresh number of its own would make the interruption look worse"，实际发出的正是**重复的 0** 和一个凭空生成的 id。按 sequence 校验的 SDK（Codex/Responses SDK）会把它当协议错误抛出，比裸 EOF 更糟。同路径也没带 `model`。

> 注意：不能简单改成传 `tee.Bytes()` —— 它是头部 256KB，取到的"最后一个 sequence"来自流开头，会发出更离谱的旧号。正解仍是让 `streamUsageReader`（全量字节）顺带记 `id/model/lastSeq`。

### W20-4 · `streamUsageReader.pending` 永不截断（P1）

```go
r.pending = append(r.pending, p...)
copy(r.pending, r.scan(r.pending))   // 拷到头部，但长度没变
r.trimPending()                      // 只在 >1MB 时整体清零
```

探针（5 行 × 87 字节，全部以 `\n\n` 收尾）：

```
PROBE pending_len_after_5_lines=435   ← 应为 0
PROBE pending_content="…第1行…第2行…第3行…第4行…第5行…"
```

后果：
1. **每次 Write 都重扫全部历史**，直到 1MB 上限——典型长流是 O(n²) 的 memcpy + 反复 `usageEvent` 解码；
2. 到 1MB 时 `pending[:0]` **整体丢弃**，若正好卡在一帧中间，该帧（可能正是 usage 或收尾帧）永久丢失；
3. 旧帧被反复 fold。`mergeClaudePassthroughUsage` 是"字段级后写覆盖"，所以**数字目前是对的**（探针 100/42/90 正确），但这是运气不是保证：任何改成累加的合并都会立刻翻倍。

另有 **P2-8.1 未修**：`final` 不 flush `pending`，SSE 允许最后一帧不带换行，那一帧的 usage 仍会丢。

修法：`r.pending = r.pending[:copy(r.pending, r.scan(r.pending))]`。

### W20-5 · idle 守卫把慢消费者判成上游静默（P1，线上）

```
PROBE first_read n=11 err=<nil>
（消费者停止 Read 250ms，守卫 100ms）
PROBE guard.tripped=true
```

生产形态：`io.Copy(writer, response.Body)` 在向慢客户端 `Write` 时阻塞 → 不再 `Read` → 计时器仍从"上次 Read"起算 → 120s 到点 `Close()` 上游 body → 后续 Read 返回 `errUpstreamIdleTimeout` → 该请求按"上游故障"计入 Provider 软失败（3 次进冷却），客户端收到 incomplete 收尾。

即：**一个网速慢的客户端可以连续打挂一个账号**。

修法：计时器只在 Read **进入时**启动、Read 返回时停止（只度量"正在等上游"的时间），而不是"距上次 Read 的时间"。

### W20-6 · rate-only lane 排队等满后仍 429（P1，上轮 P1-1 未修）

```
PROBE rate_only_explicit_wait err=…(waited for a free slot) waited=3.001s   ← 给的 3s 预算
PROBE self_budget_after_window_slides err=… waited=2.001s (window frees at ~2s)
```

本轮 `acquire` 已经能算出"窗口滑出"的精确时刻（`admits[0]+1min-now+1ms`），但 `waitFor` 的 timer 分支**直接返回 429，不再试一次准入**，而 rate-only lane 的 release 是空函数、永不 broadcast。于是：等满整个窗口 → 恰好在这一刻有额度 → 仍然拒绝。

修法：`waitFor` 因 deadline 返回后，`continue` 循环用 `now = deadline` 再 `admitLocked` 一次（约 2 行）。

### W20-7/8/9 · 调度与负缓存的三处判据问题

- **W20-7 阶梯不升**：`providerRateRestBase(30s) << count` 被 `providerSoftUnavailableTTL(60s)` 地板吃掉，第一档=第二档=60s；而 `providerSoftFailureWindow=60s` ≤ 最短 rest，顺序重试时 count 永远回 0。探针：注入 count=2 才能拿到 2m（并发失败可升，顺序重试不可升）。
- **W20-8 取最早 resets**：周窗口 100% 耗尽、5h 窗口 5%，探针 `hard_quota_ttl=9m0s`——按"最早重置"取到了 9 分钟后的 5h 窗口。判据应该是"哪个桶真的满了（utilization ≥ 阈值）"，而不是"哪个先重置"。
- **W20-9 错误不展示**：`server.go:2637` 的 `else if cached, ok := getAllowStale(...)` 先命中（有 stale 值就 return），`else if !forceRefresh { lastError }` 只有在**没有值**时才可达。文档描述的"旧快照 + 错误"这一路径是死代码。建议把 `lastError` 并进第一个分支的 report。

### W20-12 · `upstreamTransport` 仍是死代码（P2，上轮 P2-7）

```
internal/gateway/upstream_timeout.go:47   var upstreamTransport = newUpstreamTransport()
internal/gateway/upstream_idle_test.go:78,90,93   只有测试引用
```

`guardUpstreamIdleClient(client, …)` 用的仍是调用方自带的 client（未设 Transport → `http.DefaultTransport`），所以 `ResponseHeaderTimeout: 120s`、`MaxIdleConnsPerHost: 32` **全部没生效**；注释"the one transport every provider request goes through"与事实相反。`TestUpstreamTransportHasHeaderTimeout` 是为这份死配置写的测试——它绿着，但守的是没人用的东西。要么接进所有 `http.Client`，要么连测试一起删。

---

## 四、对前 19 轮 60 项清单的复核

图例：✅ 已修并复核 · 🟡 部分 · ❌ 未动 · 🆕 本轮新增/新证据
（协议转换的 `convert_*.go` 本轮 `git diff` 零改动，故 A 组状态沿用第 19 轮，已注明"文件未动"。）

### A. 协议转换（P1–P11）

| # | 问题 | 状态 |
|---|---|---|
| P1 | Claude→Chat 流式 prompt_tokens 归零 | ✅ |
| P2 | stop_reason 全量映射 | ✅ |
| P3 | Chat→Responses 有 tool_calls 时正文被清空 | ✅ |
| P4 | Responses→Chat 流式丢 reasoning | ✅ |
| P5 | incomplete 被当正常结束 | 🟡（转换路径已修；透传 `stream=true` 回整段 JSON 仍误判，且 `primed` 分支把整段 JSON 直接 skip 掉 → usage 归零） |
| P6 | Chat→Claude block index / stop 顺序 | ✅ |
| P7 | tool_use id 归一化碰撞 | ✅ |
| P8 | Responses→Claude 缺合成 tool_result | ✅ |
| P9 | redacted_thinking 丢失 | ❌ 文件未动 |
| P10 | strict/top_k/stop_sequences/metadata/cache_key 不转发 | 🟡 文件未动 |
| P11 | 两两手写转换 vs 单一 IR | ❌ 文件未动 |

### B. 路由 / 调度（R1–R6）

| # | 问题 | 状态 |
|---|---|---|
| R1 | 无 RPM 限流 | 🟡 滑窗正确，**rate-only 排队仍必失败（W20-6）**，默认不开启 |
| R2 | 冷却不看上游 | 🟡 双阶梯已落地；阶梯不可升（W20-7）、resets 取最早（W20-8）、401 吃 15min（W20-13） |
| R3 | 额度/鉴权未分类 | ✅ |
| R4 | 流截断无法续传 | ❌ 转换路径 `isUpstreamStreamInterruptedReader(r)` **在扫描前取值**（`convert_claude_openai_stream.go:87` 等 4 处），恒 false（N14-2，文件未动）；透传路径仍误判 256KB+（W20-2） |
| R5 | 会话粘性只在内存 | ✅ |
| R6 | 闸门按 Provider 而非账号、非 FIFO | ❌ 文件未动 |

### C. 横切安全（S1–S5）

| # | 问题 | 状态 |
|---|---|---|
| S1 | ACAO `*` | ✅ |
| S2 | 请求体无上限 | ✅ |
| S3 | 登录/setup 无频控 | 🟡 先解码后限流、条目无上限（文件未动） |
| S4 | 正文不脱敏 | ✅ 127c194 + b1e935c |
| S5 | 上游停顿无空闲超时 | 🟡 守卫覆盖面扩大（新增 15 处）**但慢消费者误判（W20-5）**、transport 死代码（W20-12）、120s 砍出图（W20-12b） |

### D. Provider 账号（A1–A5）

| # | 问题 | 状态 |
|---|---|---|
| A1 | 三家刷新无锁 | ✅ **本轮 WorkBuddy 补齐并做对**（锁内重读 + 401 重放 + 签到/额度/聊天三条路）；Trae/Cursor 沿用 16c00d3 |
| A2 | 用量探测子串匹配 host | ✅ |
| A3 | images 路径 + 全局 Key | ✅ |
| A4 | 漏 overage / 429 无负缓存 | 🟡→ 大幅改善：overage ✅、5 个 refresh 全接负缓存 ✅、指数退避封顶 ✅；剩"waitForRefresh 只 1/5""错误不展示（W20-9）""失败条目不过期" |
| A5 | Cursor 无提前量 | ✅（round10 已纠错） |
| N-1 | workbuddy 无 exp → 永不续期 | ✅ **本轮修复**（SignInAt 老化 + 401 强制刷新） |

### E. 控制台（C1–C6）

C1 实时请求流 / C2 路由决策聚合 / C3 会话视图 / C4 一键重放 / C5 成本列 / C6 告警角标 —— **6 项全部 ❌**（本轮 `web/` 只动了价格表）。

### F. 线上仍在跑的 P0

| # | 问题 | 本轮 |
|---|---|---|
| N10-1 | >256KB 完整 SSE 被判截断 | ❌ 未修，且**新证据：连带 usage 归零 + 双 `[DONE]` + 账号软失败**（W20-2） |
| N11-1 | 长流 usage 丢失 | 🟡 解析器已修（未提交），但被 N10-1 的返回路径抵消（W20-2） |
| N11-3 | 透传 Claude 收尾漏 `content_block_stop` | 🟡 同 W20-3：只有缓冲路径走 `closeReplay`，透传仍只发 `message_delta+message_stop` |

---

## 五、与 magpie 对照

| 维度 | magpie | 本项目 | 评述 |
|---|---|---|---|
| 长流用量 | 单一 IR 解析后由编码器写 usage，缓冲与解析解耦 | 增量 Reader 看全量字节 🟢，但返回值被"是否截断"的旧判据拦下 | 缺的是同一份数据只判一次；magpie 不会出现"解析对了但没记上" |
| 截断收尾 | `stream_cut_test.go` 保持流式并把切断如实报给客户端 | 补收尾帧，但透传路径补的帧序列号非法（W20-3） | 补帧思路可以，但**必须用本流自己的 id/序号**，否则不如不补 |
| 上游静默 | 无空闲守卫，请求失败即 re-plan | 120s 空闲守卫 | 我们要它（长连接+账号冷却），但不能把下游背压算成上游故障（W20-5） |
| 429 | **请求内重试**（pause 1/2/4s，Retry-After cap 8s） | 账号级冷却阶梯 | 短 Retry-After 用重试消化更贴近厂商语义；本项目把 1s 写成账号标记，等于既没退避也没重试 |
| 令牌轮换 | 统一 refresh + 锁 | 本轮补齐 WorkBuddy，Claude 加后台轮换 | 已对齐，且 WorkBuddy 的 401 自愈比 magpie 更进一步 |

---

## 六、验收门槛（下一次提交前）

1. `go build ./... && go vet ./...` 绿，**在提交那一刻**再跑一次（本轮出现过不可编译窗口）。
2. `go test ./internal/gateway/` 0 红（本轮已达成，保持）；`internal/tunnel` 的 5 个失败是 Windows 无 `/bin/sh`，与改动无关，可在 CI（Linux）验证。
3. **W20-2**：新增测试——558KB 无法 Content-Length 的完整 Chat 流，断言 `err == nil`、`[DONE]` 只出现 1 次、usage == 流内真实值。当前必红。
4. **W20-3**：透传路径的 Responses 中断帧断言 `sequence_number == 已发出最大序号+1` 且 `id` 等于本流 id。当前必红（发出 0 + 伪造 id）。
5. **W20-4**：`pending` 在 `scan` 之后必须被截断——加一条"5 行之后 `len(pending)==0`"的断言；`final` 要 flush 末尾无换行的帧。
6. **W20-5**：idle 守卫的测试要覆盖"消费者不 Read 时守卫**不**触发"，只有"Read 被阻塞"才触发。
7. **W20-6**：rate-only lane 增加"预算内等到窗口滑出后**成功**"的用例（当前必红）。
8. **W20-1**：先提交再部署。生产回滚点必须是一个 commit hash，而不是工作区。
9. 清理：3 个 `.bak`（`provider_lanes.go.bak` / `provider_lane_capacity_test.go.bak` / `usage.go.bak`）；`gofmt -w internal/gateway/upstream_timeout.go`；删掉 `usageEvent` 上重复的 doc 注释。
10. `upstreamTransport`：接进所有 client，或连同 `TestUpstreamTransportHasHeaderTimeout` 一起删。
11. 提交拆分建议（互不依赖，可独立回滚）：
    - `fix(usage): 长流用量按全量字节统计` + W20-2/W20-4 的测试
    - `fix(sse): 收尾帧复用本流的 id 与序号`（含透传路径）
    - `feat(oauth): WorkBuddy 令牌 401 自愈 + Claude 后台轮换`
    - `fix(usage-cache): 负缓存退避与错误展示`
    - `fix(lanes): rate-only 排队在窗口滑出时重新准入`
    - `feat(pricing): 全局价格行`

---

## 七、复现方式（隔离树）

```bash
cd /c/guodevelop/llm-protocol-gateway
git worktree add .workbuddy/bench/r20 HEAD --detach
cd .workbuddy/bench/r20
git apply ../../reviews/round20-wip-snapshot.patch
go build ./... && go test ./internal/gateway/ -count=1     # 98s，0 红
```

探针（已随 worktree 删除，逻辑见本报告正文）：
- `TestZZLongCompleteStreamJudgedInterrupted` → W20-2
- `TestZZResponsesPassthroughClosingSequence` → W20-3
- `TestZZPendingTruncation` → W20-4
- `TestZZIdleGuardSlowConsumer` → W20-5
- `TestZZRateOnlyLaneSelfBudget` → W20-6
- `TestZZCooldownHintSoonest` → W20-8

> 本轮未修改任何业务代码；快照补丁保留在 `.workbuddy/reviews/round20-wip-snapshot.patch`，隔离 worktree 与探针已删除。
