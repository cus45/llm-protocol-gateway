# 第二十九轮审查（round29）— 另一 agent 改动核查

- 时间：2026-10-23 21:35–22:05 (+0800)
- 审查对象：`d93ec02`（新提交，21:32）+ 工作区 63 改 / 6 新增（+3209/−769，指纹 `ce1c11dc…`，审查期间仍在编辑）
- 对照基准：`C:\guodevelop\demo\magpie`
- 未修改任何业务代码；探针文件已删，临时 worktree 已移除

---

## 一、结论先说

1. **新提交 `d93ec02`（账号级 lane 共享）方向正确，但不能上线**：`laneGroup` **完全没有落库**（`internal/store/sqlite.go` 无该列，`providers` 表 INSERT/SELECT 都不带它），控制台设置成功、重启/重新部署即静默丢失；另有 3 条一致性缺陷（Retry-After 仍有两处按行号查、组内限额取"最后到达那行"、组内 0/0 行可整组绕过），1 条文档与实现相反（大小写）。
2. **WIP 的"透传真流式"没修成**：`writeBufferedStream` 改成 `io.Copy` 但从不提交/刷新，`failoverResponseWriter.Write` 在未 passthrough 时只写内存 —— 实测首字节 900ms = 末字节 900ms、`writes=1`，与 HEAD 行为一致。**这条 P0 仍在生产上**。
3. **WIP 顺带引入一条新的 P1（已在生产）**：`failoverResponseWriter` 在 passthrough 模式下把每个写出的字节再复制进 `w.body`，而读它的 `Prefix()/streamClosed()/StreamLen()` 全仓零调用 ⇒ 每个请求多留一整份响应在内存（探针：8.5MB 流 → 8.5MB 常驻）。
4. **WIP 大面积做对**：转换路径中断判定改为读后判定（R4 五腿补齐）、长流 usage 逐字节折叠（N11-1）、idle 守卫 nil 竞争修复、心跳 stop 有界 join、`CleanEOF` 对 Chat→Responses 补齐、`responsesStreamIdentity` 收尾帧身份、成本表树形改造。
5. **WIP 仍不能提交**：1 个自写测试红（`TestConverterStreamWriterKeepsTheStatusItWasGiven`：要 429 实际 200，测试与实现互相矛盾）、`zz_dbg60_test.go` 调试脚手架、`internal/monitoring/config.go.bak`，且 `internal/monitoring` 整包**未接线**（`MigrateMonitoringV1` 无调用者，表永远不会建）。
6. **生产**：二进制 `2026-10-23 06:32:24 PDT`（=21:32:24 +0800，提交后 13 秒），含 WIP 独有串 `upstream stream ended without its closing frame` / `idleGuardedBody` ⇒ **线上跑的是未提交 WIP**（连续第 8 轮）。

---

## 二、基线

| 对象 | build | vet | go test | 备注 |
|---|---|---|---|---|
| 干净 HEAD `d93ec02`（隔离 worktree） | 绿 | 绿 | `./internal/gateway` 38.9s **0 红** | 全量 |
| WIP 快照 | 绿 | 绿 | `./internal/gateway` **1 红**；`store`/`monitoring`/`app` 全绿 | 审查期仍在编辑 |
| web | — | — | 无法测（worktree 未带 node_modules） | 主树 `tsc` 上轮 0 错 |
| gofmt | CRLF 假阳性 | — | — | 命中文件全部是 CRLF（HEAD 版本干净） |

---

## 三、新提交 `d93ec02` 审查

### 3.1 做对的

- `providerLaneKey`/`providerGrouped` 抽象清楚，未分组行为逐字节不变（`TestProviderLaneWithoutGroupKeepsSeparateBudgets`）。
- `uncountLocked(now, at)` 按条目撤销：修掉了"同一纳秒两次准入撤错"的真实缺陷，且带注释说明为什么不能撤最新一条。
- `handleSetProviderConcurrency` 的 `laneGroup` 用 `*string`（省略=保持、`""`=解除），语义与既有 `*int` 风格一致；120 字符上限 + 日志写明挂在哪个账号。
- 控制台输入框带 `key`、`maxLength`、`defaultValue`，与既有面板风格一致。
- `NewRouter` 存取校验加了 `stored.LaneGroup != provider.LaneGroup` 的测试断言。

### 3.2 问题

| # | 级别 | 问题 | 证据 |
|---|---|---|---|
| D29-1 | **P0** | **`laneGroup` 不落库**：`domain.Provider.LaneGroup` 无对应 SQLite 列，`saveProviders` 的 INSERT 与 `loadProviders` 的 SELECT 都不含它 ⇒ 保存后重启/重新部署即回到"各自独立"，控制台无任何提示 | PROBE-29E：`Save` 后 `Load` 得 `laneGroup="" max=2 rpm=30` |
| D29-2 | P1 | **Retry-After 只有 failover 出口修了**，`protocol_proxy.go:769` 与 `server.go:4391` 仍 `retryAfterSeconds(provider.ID, …)`；分组后 lane 键是 `group:<name>`，按行号查必然落空 ⇒ 退回 1 秒常量。提交说明写的"协议代理与 failover 两个出口"与实际不符 | PROBE-29A：`byRow=1` vs `byAccount=60` |
| D29-3 | P1 | **组内限额取"最后到达那行"**：`laneFor(key,max,rpm)` 每次 acquire 都用当次 provider 的配置覆盖 `lane.max/rpm`。同一组 A(max=2)、B(max=10)：经 B 连开 3 个成功，再经 A 立刻报 `3/2 already in flight` ⇒ 账号额度随请求路径抖动 | PROBE-29B |
| D29-4 | P1 | **组内 0/0 行整组绕过**：`acquireProviderSlot` 在 `max<=0 && rpm<=0` 时直接返回空 release，根本不进 lane。运营"只在一行填限额、其他行留空"就会得到 0 限制——恰是这功能想解决的场景 | PROBE-29C：账号 1/min 花光后，0/0 那行连放 5/5 |
| D29-5 | P2 | 注释写"matched exactly (case-insensitively)"，实现是精确匹配（无 `ToLower`）⇒ `Acme` 与 `acme` 两条 lane，账号又被拆成两份 | PROBE-29D：`keyA="group:Acme" keyB="group:acme"` |
| D29-6 | P2 | `providerInFlight(providerID)` 仍按行号查 lane ⇒ 分组行恒 0，`leastUsedChain`（`provider_schedule.go:144`）会把分组账号当"最闲"反复优先 | 代码 |
| D29-7 | P3 | 提交里夹带了一处未在说明中提及的行为变更：`provider_failover.go` 的 `if !writer.deferring() { transportFailoverWorthy = false }`。语义本身合理（首字节已出就不再换账号），但与提交主题无关，应单独成提交 | `git show d93ec02 -- internal/gateway/provider_failover.go` |
| D29-8 | P3 | lane 只增不删：改分组名/删 Provider 后旧 lane 常驻（含 inflight 引用），无回收路径 | 代码 |

### 3.3 与 magpie 的建模差异（这是上面 4 条的根因）

magpie 把"账号"放在 **Provider 配置对象内部**：`MaxConcurrency` + `AccountConcurrency map[keyID]int`，`magpie provider account-concurrency <id> [account|key] [n|off|default]`，key 自己的限额优先、否则回落到 provider 级（`internal/cli/providers_cli.go:1528`，`internal/gateway/concurrency_account_test.go`）。账号身份来自数据模型，随 provider 一起持久化，不存在"跨行字符串匹配"。

我们用 `laneGroup` 自由文本匹配跨行，于是必然要额外解决：持久化、大小写、限额来源唯一性、0/0 绕过。**建议**：既然 Provider 行=一把密钥，就在 store 里加 `lane_group` 列（迁移 v(N+1)，`saveProviders`/`loadProviders` 双改）+ 组限额只认"组内第一个非零配置"或增加组级设置项，而不是每行各存一份。

---

## 四、WIP 审查（63 改 / 6 新增）

### 4.1 做对的（相对上轮的实质进展）

| 项 | 结论 | 证据 |
|---|---|---|
| N14-2 转换路径中断判定恒 false | **已修**：5 处改为读循环之后判定，`cleanEOFInterruption(r, closed)` 由转换器自己给的 `closed` 决定 | `convert_*_stream.go` 决定性位置 + 新测试 `TestChatToResponsesTreatsCleanEOFWithoutDoneAsInterruption` 绿 |
| N11-1 长流 usage | **已修**：`streamUsageReader` 逐字节折叠（`pending` 只留未完成行，`trimPending` 有上限），`final` 会 flush pending | `usage.go` |
| N11-3/N11-4 收尾帧 | **已修**：`streamCut` 带 `id/model/lastSequence`，`responsesStreamIdentity` 用 last+1；Claude 收尾按 `openBlocks` 逐个 close | `passthrough_stream.go:90-130` |
| N24-2/D26-3 idle 守卫 nil panic | **已修**：`Read` 后 `if b.timer != nil`，`Close` 同时置 nil，均在锁内 | `upstream_timeout.go:104-150` |
| 心跳 stop 阻塞 | **已修**：有界 join（2s）+ `exited` 通道 | `sse_heartbeat.go:34-62` |
| 4B 携带漏判 | **已修**：`streamEndReader` 按 `maxClosingMarkerLen-1` 携带 | `stream_end_reader.go:36-39` |
| 成本/价格表 | 全局价 + `*` 兜底 + 树形 UI 改造 | `pricing.go` / `pricing.tsx` |

### 4.2 问题

| # | 级别 | 问题 | 证据 |
|---|---|---|---|
| W29-1 | **P0（生产）** | **透传流仍是全量缓冲**：`writeBufferedStream` 的 `io.Copy(writer, …)` 里 `hw.Write` → `fw.Write`，而 `fw` 未 passthrough 时只 `buf.Write`；`fw.Flush()` 在未 passthrough 时是空操作；循环前也没有 `WriteHeader` ⇒ 直到 `FlushError()` 才一次性下发 | PROBE-29H：canDefer=true 首字节 900.76ms = 末字节，`writes=1`；canDefer=false 同样 901.21ms |
| W29-2 | **P1（生产）** | **passthrough 下每个字节被再复制一份**：`w.body.Write(p[:n])` 只为 `Prefix()/stream()/streamClosed()/StreamLen()`，而这几个全仓**零调用**（`streamClosed` 只在定义处出现）⇒ 响应多大就多占多少内存 | PROBE-29F：8,503,377 字节流 → `fw.body` 8,503,377 字节常驻 |
| W29-3 | P1 | **心跳进了缓冲区**：`startSSEHeartbeat(hw)` 的 `: ping` 走 `fw.Write` 同样落 buf，客户端在流结束前一个心跳都收不到（Cloudflare 100s 空闲仍会掐），结束时反而在正文前多出若干 ping | 代码 + W29-1 同因 |
| W29-4 | P1 | **红测试未清**：`TestConverterStreamWriterKeepsTheStatusItWasGiven` 断言 429，实现里 `if status < 200 \|\| status >= 300 { status = 200 }` 强制 200（注释："a stream is a success by definition"）。二者必须二选一：要么尊重转换器显式给的 4xx（客户端能看见错误），要么删掉该断言并说明理由 | `go test ./internal/gateway/` 唯一红 |
| W29-5 | P2 | **`internal/monitoring` 整包未接线**：`internal/app`/`internal/gateway`/`cmd` 无一处 import；`MigrateMonitoringV1`、`internal/store/monitoring*.go` 也无调用者 ⇒ 表不建、采集不跑。3400 行新代码处于"编译通过但零生效"状态 | grep 全仓零命中 |
| W29-6 | P2 | `streamEndReader.scanResponses` 对**非 Responses 流**永远在累积：Chat 流的 `protocol` 恒为 `""`，`id/model` 永远为空 ⇒ 每行都 append 到 `tailBytes`（上限 512KB，每行一次拷贝）；同时 `scanClaude` 对每个 Chat chunk 做一次 `json.Unmarshal`。热路径上给纯透传 Chat 加了约两倍解析成本 | `stream_end_reader.go:162-217` |
| W29-7 | P2 | 回声守卫仍可被骗：`scanClosing` 只看"当前 chunk 末 64 字节"，模型正文若在某个 chunk 结尾正好写出 `message_stop`/`[DONE]` 就会被判为已收尾 ⇒ 真截断不补收尾帧 | `stream_end_reader.go:114-143` |
| W29-8 | P2 | `windowEnd` 在"尾块无换行且 hold>0"时算成 `len(p)` 而不是 `len(buffer)`，窗口被放大 up-to-18 字节（偏宽松，非漏判） | 同上 |
| W29-9 | P3 | `streamUsageFolder` 用 `reflect` 比较函数指针，只有直接传 `ParseOpenAIUsage/ParseResponsesUsage/ParseClaudeUsage` 才生效；任何包装（`func(b){return ParseClaudeUsage(b)}`）静默退化为"整段解析日志副本" = 长流 usage 归零 | `usage.go` |
| W29-10 | P3 | 脚手架：`internal/monitoring/config.go.bak`、`internal/gateway/zz_dbg60_test.go`（`println("DBG60"…)`）、`web/` 无类型检查证据 | `ls` / `grep` |
| W29-11 | P3 | `failoverResponseWriter` 死代码：`Prefix()`（无调用）、`streamClosed()`（无调用）、`StreamLen()`（无调用）、`recordsForRetry()`（空方法） | grep |

---

## 五、生产环境核查（173.249.207.202）

| 项 | 事实 |
|---|---|
| 服务 | `llm-protocol-gateway.service` active，主进程 PID 911205，启动 06:32:38 PDT |
| 二进制 | `/opt/llm-protocol-gateway/bin/gateway`，16,498,850 B，mtime `2026-10-23 06:32:24 PDT` = **21:32:24 +0800** |
| 版本归属 | 含 `shares account lane`（d93ec02 独有）+ `upstream stream ended without its closing frame`、`idleGuardedBody`（**仅 WIP**）⇒ 线上 = **d93ec02 之后的未提交工作树**，无 commit 可复现，回滚只能重编 |
| 数据 | 生产库 `/var/lib/llm-protocol-gateway/gateway.db`（未触碰） |
| 副作用 | 本轮误用 `gateway version` 探版本，该子命令零 flag 解析会启动服务：写 `/root/.config/llm-protocol-gateway/gateway.db-wal`（341,992 B）后因端口占用退出，**未起残留进程、未碰生产库**。N13-1 第八次复现 |

---

## 六、完整排查清单（历史累计 → 本轮状态）

### 6.1 协议转换

| 编号 | 问题 | 状态 |
|---|---|---|
| P1 | Claude→Chat 流式丢 prompt_tokens | ✅ |
| P2 | stop_reason 全量映射 | ✅ |
| P3 | Chat→Responses 有 tool_calls 清空正文 | ✅ |
| P4 | Responses→Chat 丢 reasoning 增量 | ✅ |
| P5 | Responses→Chat 忽略 incomplete | ✅ |
| P6 | Chat→Claude block index 错乱 | ✅ |
| P7 | sanitizeAnthropicToolUseID 碰撞 | ✅ |
| P8 | Responses→Claude 缺合成 tool_result | ✅ |
| P9 | redacted_thinking 丢失 | 🟡（占位方案，非回放） |
| P10 | strict/top_k/stop_sequences 等不转发 | 🟡（部分） |
| P11 | 两两手写转换 vs magpie 单一 IR | ❌ 架构未动 |
| H-1..H-9 | 上轮 9 条高危（name→Claude、json_object 直传、output_config 覆盖、allowed_tools、cache marks、thinking 五连、images 路由、管理员 Key 链） | 抽样复核 **仍在** |

### 6.2 调度 / 路由

| 编号 | 问题 | 状态 |
|---|---|---|
| R1 | 无 RPM 限流 | ✅ 滑动窗口 |
| R2 | 冷却不看上游 | ✅ 阶梯可达（4m/8m/16m 日志实测） |
| R3 | billing 归 hardQuota | ✅ |
| R4 | 流截断不能续 | 🟡 转换路径已补；**透传路径被 W29-1 的缓冲行为掩盖** |
| R5 | 会话粘性不落库 | ✅ session_pins |
| R6 | 并发闸门每 Provider 非 FIFO | 🟡 票号队列 + 本轮 laneGroup（D29-1..4 待修） |

### 6.3 安全

| 编号 | 问题 | 状态 |
|---|---|---|
| S1 | 管理 API `ACAO: *` | ✅ 白名单（模型端点 *、管理同源） |
| S2 | 请求体无上限 | ✅ |
| S3 | 登录/setup 无限流 | 🟡 |
| S4 | 日志正文不脱敏 | ✅ |
| S5 | 首包后无空闲超时 | 🟡 守卫有，W29-3 使心跳失效 |

### 6.4 Provider 账号

| 编号 | 问题 | 状态 |
|---|---|---|
| A1 | Trae/WorkBuddy/Cursor 刷新无锁 | ✅ |
| A2 | 子串匹配 host | ✅ |
| A3 | /v1/images 路由 | 🟡 |
| A4 | Claude overage / 负缓存 | ✅ |
| A5/A6 | Cursor 提前量 / 出图超时 | ✅ |

### 6.5 控制台（6 项排障能力）

| 项 | 状态 |
|---|---|
| 实时流（EventSource） | ❌ grep 零命中 |
| 路由决策聚合视图 | ❌ |
| 会话视图（sessionId） | ❌ |
| 一键重放 | ❌ |
| 日志成本列 | ❌（cost 只出现在价格/用量页） |
| 告警未读角标 | 🟡 告警页内有 unread 计数与筛选，侧边栏无角标 |

### 6.6 本轮新增（W29/D29）

见第三、四节表格：D29-1..8、W29-1..11。

---

## 七、验收门槛（下轮按此逐条勾）

1. `go build ./... && go vet ./...` 绿；`go test ./internal/gateway/ ./internal/store/ ./internal/app/ ./internal/monitoring/` **0 红**。
2. 工作区无 `zz_*_test.go`、无 `println("DBG…")`、无 `.bak`。
3. `laneGroup` 有 SQLite 列与迁移，`Save→Load` 断言通过（D29-1）。
4. 三条 `retryAfterSeconds` 出口对分组 provider 都给出真实窗口（D29-2）。
5. 组内限额来源唯一（组级或"首个非零"），0/0 行不得绕过（D29-3/4）。
6. 透传流首字节 < 末字节的 50%（W29-1）：`writeBufferedStream` 在首块前提交。
7. 大流（≥8MB）请求结束后 `fw.body` 不再持有整段（W29-2）。
8. 流式期间客户端能收到 `: ping`（W29-3）。
9. `monitoring` 要么接线（迁移在 `Open()` 内执行）要么不进主干（W29-5）。
10. 转换器显式 4xx 与实现二选一，红测试清零（W29-4）。
11. 生产二进制含 `vcs.revision` 与 `vcs.modified=false`（连续 8 轮未达标）。
12. 提交粒度：`d93ec02` 里夹带的 failover 行为变更单独成提交（D29-7）。
