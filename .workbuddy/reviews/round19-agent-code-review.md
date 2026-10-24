# 第十九轮 · 另一 agent 改动审查（round19）

- 审查时间：2026-10-23 11:17 → 11:30（+0800）
- 审查范围：HEAD `b1e935c`（自 round18 起**零新提交**）+ 工作区未提交改动（17 改 5 新增，838+/146-）
- 对照项目：`C:/guodevelop/demo/magpie`
- 本轮未修改任何业务代码，探针文件已删除

---

## 一、结论先说

1. **没有任何新提交**。HEAD 仍是 `b1e935c`（10:44 提交），线上 VPS 跑的也是这个版本。
2. **工作区仍在实时编辑中**，审查期内指纹变了 4 次（`bc4dabe5` → `8aa0bd5f` → `a100943a` → `203fc314`），usage.go mtime 到 11:25。11:22 那一版**编译不过**（usage.go:174 类型错误），11:25 作者自行修好。**这种半成品形态不可提交**。
3. **测试 1 红**：`TestProviderLaneRateIsASlidingMinute`（测试自己写错，实现正确，上一轮已指出，30 分钟未修）。`go build` / `go vet` 当前绿。
4. 本轮 WIP 打的四个靶子：**S5 空闲超时、A4 用量负缓存 + overage 窗口、R2 冷却阶梯、N11-1 长流 usage 丢失**。方向全对，其中：
   - **N11-1（线上 P0）方向正确且有实效**：增量 SSE usage 解析绕开 256KB 头部缓冲 ✅
   - **S5 只修了一半**：`upstreamTransport` 仍是**全仓零引用的死代码**，`ResponseHeaderTimeout` 依旧零作用 ❌
   - **A4 只接了 1/5 handler**，Cursor/ChatGPT/DeepSeek/智谱的同步路径仍会被每次轮询打上游 ❌
   - **R2 的阶梯在产线不可达**（失败窗口 60s < 任何 rest），且 Retry-After=1s 会把冷却压到 1 秒 ❌
5. **本轮新增 1 条 P1 回归**（已探针实证）：只配 RPM 的 Provider 现在会**白等满窗口再 429**——排队永远不会成功，纯粹把拒绝变成 60 秒延迟。

---

## 二、上轮（round18）遗留复核

| 编号 | 上轮结论 | 本轮复验 | 证据 |
|---|---|---|---|
| 线上 P0 N10-1（>256KB 完整流被判截断） | 未修 | **仍在** | `passthrough_stream.go:284` `closesUpstreamStream(tee.Bytes())` 读的是头部 256KB 副本，收尾帧在尾部；`empty_stream_retry.go:92` ContentLength<0 → `!sawClosingFrame` → true |
| 线上 P0 N11-1（长流 usage 归零→估算放大） | 未修 | **本轮修了 SSE 路径** | `usage.go:441` `streamUsageReader` 见字节即解析，`passthrough_stream.go:267/380` 挂进 `io.MultiWriter` |
| N14-1（成功流全部 io.ReadAll 整段缓冲） | 未修 | 仍在（文件未改该段） | `passthrough_stream.go:385` |
| N14-2（转换器中断判定恒 false） | 未修 | **仍在** | 5 处 `isUpstreamStreamInterruptedReader(r)` 读前按值快照：`convert_*_stream.go:87/126/253/576/815` |
| W18-2（rate-only lane 等满窗口仍 429） | 未修 | **仍在，且变成新回归** | 见第三节 P1-1 |
| W18-1（ErrorDescription 不脱敏） | 未修 | 未动（文件未改） | — |
| 假测试 `TestUpstreamTransportHasHeaderTimeout` | 未修 | **仍在**（测的是死变量） | `upstream_timeout.go:47` 零引用，仅测试引用 |
| R2 阶梯不可达 | 未修 | **仍在**（窗口仍 60s） | `provider_failover.go:38` + `:665` |
| 「15 处全部接入」实为点名 15 处 | — | 现在接了 9 处 `guardUpstreamIdleClient`，另有若干 `client.Do` 未接 | 见第四节 S5-2 |
| 2 个 `.bak` + 调试脚手架 | 未清 | **仍在**，本轮又新增 `zz_perf_test.go` | `provider_lanes.go.bak`(11:15) 等 |
| A4（overage 窗口 + 负缓存） | 半 | **本轮大部分完成**，接线不全 | 见第四节 A4 |
| S5 主路径 | 半 | 空闲守卫已接，transport 仍死 | 见第四节 S5 |
| R1（RPM 限流） | 骨架 | 滑动窗口实现正确，排队语义错 | 见 P1-1 |

---

## 三、本轮新发现（按严重度）

### P1-1 · rate-only lane 的排队是纯延迟，永远不会被准入（回归）

- 位置：`provider_lanes.go:145-160`（acquire 预算）+ `:203-207`（wait<=0 拒绝分支）+ `:222-235`（waitFor）
- 逻辑：`acquire` 给 rate-only lane 算出"最老一条滑出窗口的时刻 + 1ms"当 budget；`acquireSlot` 用这个 budget 建 deadline，等 `waitFor` 的 **timer 到期**；timer 到期时 `waitFor` 直接 `return errProviderSaturated`，**没有最后一次准入尝试**。
- 而 rate-only lane 的 `release` 是 `func(){}`（`lane.max<=0` 分支），**永远不会 close(broadcast)** → 等待者只能靠 timer 醒来 → 必然 429。
- 探针实证：

```
PROBE1 err=provider is at its concurrent request limit (waited for a free slot) waited=1.001s
```

  窗口 1 秒后就空出来了，客户端等满 1.001s 后仍然 429。
- 影响：只配了 RPM 的 Provider 在打满后，**每个请求都要陪客户端干等最多 60s 再返回 429**（`providerQueueWaitCap=60s`）。这比改造前的"立即 429"更糟：TTFT 变成分钟级、连接/协程全占满，而成功率为 0。
- 修法（任选）：`waitFor` 到期返回一个"deadline 到了"的哨兵让主循环再做一次 `admitLocked` 尝试；或给 rate lane 装一个 `time.AfterFunc` 到最早滑出点去 `close(broadcast)`。

### P2-1 · R2 退避阶梯在产线不可达（只改在纸面上）

- 位置：`provider_failover.go:616` `cooldownTTL` 软路径 + `:665` `softFailureCount` + `:38` `providerSoftFailureWindow = 60s`
- 阶梯是 `30s << min(count,6)`，但 `count` 来自"60 秒内的失败次数"，而任何一次登记冷却都 ≥60s（`providerSoftUnavailableTTL` 下限）。下一次失败落在 60s 之外 → `softFailureCount` 归 0 → 永远从 60s 重新开始。
- 探针实证（把 windowStart 拨到 70 秒前）：

```
PROBE2 count_after_70s=0 ttl=1m0s
```

- 结论：注释里写的"doubling up to 30 minutes"在产线**只能到 4 分钟**（3 连击触发时的 count=3），30 分钟的 cap 不可达。要让阶梯成立，窗口必须 ≥ 阶梯最大 rest（或改成"上一次失败时间 + 指数"的衰减模型）。

### P2-2 · Retry-After=1s 会把冷却压到 1 秒，标记形同虚设

- 位置：`provider_failover.go:656` `if d := hint.retryAfter; d > 0 && d != ttl { ttl = d }`，覆盖掉 60s 下限。
- 探针实证：`PROBE4 ttl with Retry-After=1s: 1s (soft floor is 1m0s)`。
- 影响：厂商对每次 429 都回 `Retry-After: 1`（很常见）时，"标记不可用"只持续 1 秒，等于没有冷却；叠加 R1 尚未默认开启 RPM，突发流量会变成"请求→429→1s→再请求"的抖动循环——正是当初 R1/R2 想解决的问题。
- 建议：`ttl = max(ladder, retryAfter)`，短 Retry-After 通过"同一请求内重试"消化，而不是取消账号冷却（magpie 就是这个思路，见第五节）。

### P2-3 · hard 冷却下限从 2 分钟抬高到 15 分钟，401 也被套上

- 位置：`provider_failover.go:53` `providerQuotaRestFloor = 15 * time.Minute`，替代原 `providerFailoverRecoveryInterval = 2min`
- `401/403` 且 `hardAuth` → `failoverHard`（`:155`）→ 15 分钟。测试日志实测：`provider marked unavailable provider=p2 hard=true reason=http_401_auth retry_in=15m0s`。
- 缓解：控制台保存 Provider 会 `markProviderAvailable`（`server.go:2311`），所以人工换 Key 能立刻解除。但**自动轮换/刷新竞态导致的 401**（非人为）要白等 15 分钟，比原来慢 7.5 倍。
- 建议：quota 用 15 分钟下限，auth 类维持 2 分钟（分开两个常量）。

### P2-4 · 失败条目永不过期 → 控制台可能长期展示几小时前的快照

- 位置：`oauth_usage_cache.go:86` `if entry.err == nil && time.Since(...) > oauthUsageStaleTTL`
- 探针实证：10 小时前的值 + 一次失败后，`getAllowStale` 仍返回 true（`PROBE3 CONFIRMED`）。
- Claude handler 会把 `lastError` 一起返回（还能看出是旧的），但 **Cursor/ChatGPT handler 只返回报告本身**，用户看到的是"正常但其实是 10 小时前"的额度。
- 建议：`getAllowStale` 仍按 `oauthUsageStaleTTL` 过期（或返回 `StaleSince`）。

### P2-5 · 负缓存只接了 Claude 一个 handler

- `waitForRefresh` 分支只在 `server.go:2636`（Claude）；`handleCursorOAuthUsage`(2718)、`handleChatGPTOAuthUsage`(2782)、以及 2906/2968 两处仍是 `getAllowStale → get` 老流程。
- 命中条件很窄：只有在**从来没有成功过**（`value==nil`）时才会走到同步拉取 → 那时每次轮询仍打上游。`refresh*HoldingLock` 里的 `setFailure`（`oauth_usage_refresh.go:156/203/302/337/364`）只帮到了后台 sweep。
- 另外 `server.go:2636` 的 `else if !forceRefresh` 是冗余条件（外层 else 已保证），且该分支在"有 stale 值"时**永远不可达**（上一分支已 return）——它实际只覆盖"没有任何缓存"的情况，注释描述的"last good snapshot + error"不会发生。

### P2-6 · idle 守卫把"慢消费者"误判为"上游静默"

- `idleGuardedBody`（`upstream_timeout.go:19-70`）只在 `Read` 返回后 `timer.Reset`。若下游客户端读得慢（背压），网关 120 秒不发下一次 `Read`，timer 就会到期 → `body.Close()` → 报 `errUpstreamIdleTimeout` → **计成 Provider 故障并触发冷却**。
- 这条 round18 已提，本轮仍未处理。建议按"上游两次 Read 之间的字节到达"计时（在读协程里 tick），而不是按"我们发起 Read 的间隔"。

### P2-7 · `upstreamTransport` 仍是死代码，唯一的测试是假测试

- `upstream_timeout.go:47` 定义、`:49` 构造；全仓生产代码**零引用**（`grep` 仅命中测试）。所有调用点都是 `&http.Client{Timeout: …}`（nil Transport）→ 走 `http.DefaultTransport` → `ResponseHeaderTimeout` 依然 0。
- `TestUpstreamTransportHasHeaderTimeout` 断言的是这个没人用的变量，**验证不了任何线上行为**；`maxUpstreamConns=400` 注释写"bounds the whole process"，实际是 `MaxIdleConns`（空闲池），不约束总连接数。
- 修法：要么把所有 `&http.Client{}` 换成 `&http.Client{Transport: upstreamTransport}`，要么删掉这个变量和测试。

### P2-8 · `streamUsageReader` 的三处残缺

1. **`final` 不 flush `pending`**（`usage.go:456`, `consume:494`）：最后一个 SSE 事件若不以 `\n` 收尾（SSE 规范允许），其 usage 直接丢。`final` 里应把 `pending` 当最后一行解析一次。
2. **`reflect.ValueOf(parseUsage).Pointer()` 分派很脆**（`usage.go:439`）：任何调用点改成闭包包装（`func(b []byte) TokenUsage { return ParseClaudeUsage(b) }`）就会静默掉回 `fold=nil` → 又变回"从 256KB 头部副本解析"→ N11-1 复发。建议改成显式枚举/接口，而不是函数指针比较。
3. **`stream=true` 但上游回的是整段 JSON**（不代表 SSE）时：`event()` 的 `primed` 分支会 `return`，`final` 返回零 → 只能靠 `EstimateTokenUsage` 兜底，同时 `closesUpstreamStream` 判为截断（N14-3 老问题叠加）。

### P3-1 · `usage.go` 审查期内编译不过（已自行修复）

- 11:22 快照：`usage.go:174: cannot use nested["usage"] (map index expression of interface type any) as map[string]any value`。11:25 作者改造函数签名（`streamUsageFolder` 改传 `(eventType, usage)`）后恢复。
- 记录在案：**工作区在 8 分钟窗口内不可编译**，说明改动未形成闭环就落盘。

### P3-2 · 脚手架/备份文件继续堆积

- `provider_lanes.go.bak`（11:15，14KB）、`provider_lane_capacity_test.go.bak`（00:37）、`zz_perf_test.go`（`BenchmarkA_OpenAI`/`BenchmarkB_Claude` 命名，纯调试）。
- `gofmt`：把 CRLF 归一化后检查，只有 `upstream_timeout.go` 不合规（`errUpstreamFirstPacketTimeout` 后多了一个空行）。其余文件的 `gofmt -l` 命中全是仓库 CRLF 噪音，不算作者的问题。

### P3-3 · `TestProviderLaneRateIsASlidingMinute` 自身逻辑自相矛盾（唯一红项）

- `provider_rpm_test.go:178` 在 t=65s 断言"必须被拒"，但 t=0 的准入在 t=65s 已经滑出 60s 窗口 → 只剩 1/2 → 实现允许是正确的；`:182` 又倒回 t=61s 断言"应该放行"。
- 写成 t=59s（拒）→ t=61s（放行）即可。**红测试留在树里 30 分钟未处理**，是"半成品不可提交"的直接证据。

---

## 四、本轮改动逐项判定

| 改动 | 判定 | 说明 |
|---|---|---|
| S5 `idleGuardedBody` + `guardUpstreamIdleClient`（9 处） | **部分正确** | 机制本身可用（探针 + 自带测试均绿），但 transport 死代码（P2-7）、慢消费者误判（P2-6）、120s 会砍长图生成（`server.go:5439/5465` 原本 `Timeout: 0`） |
| A4 `SevenDayOverageIncluded` + `cooldownHint` 纳入 | **正确** | `claude_oauth_usage.go:32/97` 解析与可用性判定都补上了 |
| A4 负缓存（`setFailure`/`clearFailure`/`waitForRefresh`） | **部分正确** | 后台 sweep 全覆盖 ✅；同步 handler 只接 1/5 ❌（P2-5）；条目永不过期 ❌（P2-4） |
| R2 `cooldownTTL` 双阶梯 | **部分正确** | 远端 resets_at 不再把 429 拉成 1 小时（round8 的头号问题真修了 ✅）；但阶梯不可达（P2-1）、1s Retry-After 取消冷却（P2-2）、401 被抬到 15 分钟（P2-3） |
| R2 `resetConsumed` 清零 | **可疑** | 用 `ttl == providerSoftUnavailableTTL` 做"无 hint"判定是浮点数等值式的脆弱写法；且 hard 路径已 `clearSoftFailures`，属重复 |
| R1 滑窗 `admits []int64` | **实现正确** | 窗口语义、trim、`retryAfterSeconds` 都改对了（`provider_lanes.go:241-275`）；但排队语义见 P1-1，`queueCap=rpm` 在 rpm 大时等于无上限 |
| N11-1 `streamUsageReader` | **方向正确** | 解决"usage 帧在流尾、缓冲只留头部"；三处残缺见 P2-8 |

---

## 五、与 magpie 的对照

| 维度 | magpie | 本项目（本轮 WIP） | 评述 |
|---|---|---|---|
| 429 处理 | **请求内重试**：pause 1/2/4s，Retry-After 优先但 **hard cap 8s**，rate limit 重试 3 次（`internal/gateway/fallback.go:1369-1400`） | 账号级冷却阶梯 60s→30min + 15min quota floor | 架构不同（magpie 不长期封锁账号）。但 magpie 的"短 Retry-After 用重试消化"恰好说明本项目 P2-2 的问题：拿 1 秒冷却换掉账号标记，等于既没退避也没重试 |
| 长流 usage | 单一 IR 解析后由编码器写 usage，不存在"头部截断导致 usage 丢" | 本轮用增量 Reader 绕过缓冲上限 | 方向与 magpie 的"解析与缓冲解耦"一致，**判定正确** |
| 上游静默 | 无对应机制（magpie 以请求为单位、上游断开即失败并 re-plan） | 120s idle 守卫 | 本项目需要它（长连接 + 账号冷却），但不该把慢消费者算进去 |
| transport 调优 | 未单独定制（用默认 client） | 定义了却没用 | 不如 magpie：至少 magpie 不会留下"看起来调过其实没生效"的代码 |

---

## 六、完整排查清单（60 项总账，本轮更新）

图例：✅ 已修并复核 · 🟡 部分 · ❌ 未动 · 🆕 本轮新增

### A. 协议转换（原报告 P1–P11）

| # | 问题 | 状态 |
|---|---|---|
| P1 | Claude→Chat 流式 prompt_tokens 归零 | ✅（round3 前后修复） |
| P2 | stop_reason 全量映射 | ✅ |
| P3 | Chat→Responses 有 tool_calls 时正文被清空 | ✅ |
| P4 | Responses→Chat 流式丢 reasoning | ✅ |
| P5 | incomplete/max_output_tokens 被当正常结束 | 🟡（转换路径修，透传 `stream=true` 回 JSON 仍误判截断 = N14-3） |
| P6 | Chat→Claude block index / stop 顺序 | ✅ |
| P7 | tool_use id 归一化碰撞 | ✅ |
| P8 | Responses→Claude 缺合成 tool_result | ✅ |
| P9 | redacted_thinking 丢失 / reasoning 丢弃 | ❌ |
| P10 | strict/top_k/stop_sequences/metadata/cache_key 不转发 | 🟡（stop 相关已修，其余仍在） |
| P11 | 两两手写转换 vs 单一 IR（架构） | ❌ |

### B. 路由 / 调度（R1–R6）

| # | 问题 | 状态 |
|---|---|---|
| R1 | 没有 RPM 限流 | 🟡 滑窗已实现且正确，**但 rate-only 排队永不成功（🆕P1-1）**、默认不开启 |
| R2 | 冷却不看上游说的话 | 🟡 已解析 Retry-After/resets_at 并做远近裁剪，**但阶梯不可达(P2-1)、1s 覆盖下限(P2-2)、401 抬到 15min(P2-3)** |
| R3 | 额度耗尽与鉴权失败未分类 | ✅（billing 移出 hardQuotaHints） |
| R4 | 流被截断无法续传 | ❌ 转换路径仍恒 false（N14-2）；透传路径会误判 256KB 以上完整流（N10-1） |
| R5 | 会话粘性只在内存 | ✅（affinity 落库） |
| R6 | 并发闸门按 Provider 而非账号，非 FIFO | ❌（`laneFor(provider.ID)`） |

### C. 横切安全（S1–S5）

| # | 问题 | 状态 |
|---|---|---|
| S1 | 管理 API `ACAO: *` | ✅ |
| S2 | 请求体无上限 / 无 ReadTimeout | ✅ |
| S3 | 登录/setup 无频控 | 🟡 有限流但"先解码后限流"；setup 回环守卫已加 |
| S4 | 正文不脱敏入库 | ✅（127c194 + b1e935c，含嵌套/SSE/JWT/裸 token） |
| S5 | 上游停顿无空闲超时 | 🟡 守卫已接 9 处，**transport 死代码 + 慢消费者误判 + 长图生成被 120s 砍** |

### D. Provider 账号（A1–A5）

| # | 问题 | 状态 |
|---|---|---|
| A1 | Trae/WorkBuddy/Cursor 刷新无锁 | ✅（16c00d3，遗留 3 个小瑕疵） |
| A2 | 用量探测子串匹配任意 host | ✅（`base_url_host.go:32` 按 `host==domain` 或 `.domain` 后缀匹配，fizz.ai 不再命中） |
| A3 | /v1/images/* 固定拼路径 + 回落全局 Key | ✅（e650076/4c7bed9） |
| A4 | Claude 漏 overage 窗口 / 429 无负缓存 | 🟡 **本轮补了 overage ✅、负缓存接 1/5 🟡、条目永不过期 ❌** |
| A5 | Cursor 无过期提前量 | ✅（round10 已纠错：实际达标） |

### E. 控制台能力缺口（6 项）

| # | 缺口 | 状态 |
|---|---|---|
| C1 | 实时请求流（日志页只刷第 1 页 / 应用日志只拉一次） | ❌ |
| C2 | 路由决策聚合视图（analysis.attempts 未 surface） | ❌ |
| C3 | 会话视图（sessionId 从不聚合） | ❌ |
| C4 | 一键重放 | ❌ |
| C5 | 日志表成本列（CostMicros 已有） | ❌ |
| C6 | 告警未读角标 | ❌ |

### F. 线上仍在跑的两条 P0（最重要）

| # | 问题 | 影响 |
|---|---|---|
| N10-1 | >256KB 的**完整** SSE 被判定截断（`passthrough_stream.go:284` 读头部副本） | 客户端多收 `finish_reason=length` + 第二个 `[DONE]`；计 Provider 软失败 → 账号进冷却 |
| N11-1 | 长流 usage 丢失 → `EstimateTokenUsage` 兜底（`server.go:1331`） | Key 额度/成本按 len/4 记，输出最多放大 115×（round12 实测） |

> N11-1 的 SSE 路径本轮 WIP 已修（未提交、未上线），**VPS 上仍是有问题的 16c00d3/34ec9a4 之后的 b1e935c**。

---

## 七、验收门槛（提交前必须全部满足）

1. `go build ./...` + `go vet ./...` 绿，且**在提交那一刻**再跑一次（本轮出现过 8 分钟不可编译窗口）。
2. `go test ./internal/gateway/` **0 红**；`TestProviderLaneRateIsASlidingMinute` 的断言改成 59s/61s。
3. rate-only lane：新增"等到窗口滑出后**成功**"的用例（当前实现必红，等于 P1-1 的回归测试）。
4. 阶梯可达性：用真实时序（rest 之间相隔 > 60s）证明 count 能增长，或把失败窗口改成不与 rest 冲突的模型。
5. 1s Retry-After：明确决策 = 覆盖下限还是取 max，并写进测试名。
6. hard 冷却：quota 与 auth 用两个常量，401 不得吃 15 分钟。
7. 负缓存：5 个 handler 全部接 `waitForRefresh`，且失败条目按 stale TTL 过期。
8. `upstreamTransport`：要么接进所有 client，要么连同 `TestUpstreamTransportHasHeaderTimeout` 一起删。
9. `streamUsageReader`：`final` 要 flush `pending`；去掉函数指针反射分派。
10. idle 守卫：按"上游字节到达"计时，避免慢消费者误判；长图生成路径单独放宽或不算 Provider 故障。
11. 清理 `*.bak`、`zz_perf_test.go`、`zz_*` 脚手架；`gofmt -w internal/gateway/upstream_timeout.go`。
12. 上线前对照：N10-1 与 N14-2 至少修一个，否则 R4"流中断续传"在生产仍然不生效。

---

## 八、建议

- **不建议提交当前工作区**：它同时包含 4 个互不相关的主题（S5/A4/R2/N11-1）+ 1 个回归（P1-1）+ 1 个红测试 + 脚手架。上一轮已经出现过"15 文件一起提、回归一起进"的情况。
- 建议拆成 4 个提交，顺序：`N11-1 用量`（纯收益、可离线验证）→ `S5 空闲守卫`（去掉 transport 死代码后再提）→ `A4 负缓存`（5 个 handler 全接）→ `R2 阶梯`（先解决 P1-1 与阶梯可达性）。
- **先修 P1-1 再谈 R1 上线**：现在打开 RPM 限制 = 给客户端加 60 秒延迟，比没有限流更伤。
