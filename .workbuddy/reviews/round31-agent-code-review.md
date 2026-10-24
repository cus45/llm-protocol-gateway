# 第 31 轮 对抗性代码审查报告

> 审查对象：`C:\guodevelop\llm-protocol-gateway` ｜ 参照实现：`C:\guodevelop\demo\magpie`
> 审查者姿态：默认不信任修复方自述；只回答"这个 bug 在真实执行路径上还会不会发生"。
> 本轮只读，未修改任何业务代码；探针文件已删除，工作区复原（见第十节自检）。

## 0 审查对象与增量判定

| 项 | 值 |
|---|---|
| `git rev-parse --short HEAD` | `2045543` |
| HEAD 提交 | `fix(stream): 截断收尾改用协议错误事件并对齐 magpie`（2026-10-24 02:16 +0800） |
| `git status --porcelain` | **空**（0 改 / 0 增 / 0 删；`.workbuddy/` 已被 `.gitignore:56` 忽略） |
| `git diff --stat HEAD` | 空（+0/−0） |
| 工作区内容指纹 | `git diff` = `e3b0c442…b855`（空文件哈希）、`git diff --cached` = `e3b0c442…b855` ⇒ 拼接指纹 `e3b0c442…/e3b0c442…` |
| 增量口径（d93ec02..HEAD） | **1 个提交 / 128 文件 / +20673 −875** |
| 上轮指纹 | r30：WIP 126 文件，`25feda62…`/`5e268b3f…` |
| 本轮类型 | **新增提交（且第一次把全部 WIP 一次性入仓）** |

判定：上轮（r30）126 文件的未提交 WIP，本轮被**压成单个提交** `2045543`。仓库第一次回到干净工作区（自 r8 之后首次）。因此本轮不是"无增量"，而是**一次巨量入仓**：既包含 r30 已核对过的修复，也包含 r30 之后 02:00–02:16 的新增改动（含 `stream_end_reader.go` 1072 行、`monitoring_*` 三文件）。账本 67 项全部照常复核。

第五节另有一条**治理事实**：生产 VPS 上的二进制仍是 `vcs.revision=d93ec02` + `vcs.modified=true`，即**本提交未部署**，线上跑的是 d93ec02 之后的 WIP 快照。

## 一 结论先说（能不能上线）

**不能上线。** 本轮提交方向正确（截断收尾改成协议错误事件，与 magpie `streamFailure` 对齐；转换路径的流截断判定也终于真正生效），但存在一条**连续第 11 轮未修的 P0**：所有经过故障转移写者的**透传流仍然是全量缓冲**，首字节时延等于整段生成时间。

1. **P0 · W29-1（未修，连续第 11 轮）**：`writePassThroughResponse` 对 `*failoverResponseWriter` 一律走 `writeBufferedStream`，而该路径 `hw.Write → fw.Write` 只落 `buf`，`FlushError()` 在 `io.Copy` 结束后才提交。探针 PROBE-31（本提交内 A/B）：`failover-writer firstWrite=801ms total=801ms writes=1`，同函数 `plain-writer firstWrite=0ms total=801ms writes=3`。**零改动、纯 A/B，同一提交内对照。**
2. **P0 · 面上影响**：每个网关 Key 请求都经过 failover 循环（`provider_failover.go:1174` 无条件 `newFailoverResponseWriter`），所以"透传 + 流式"类请求（Cursor/Codex 的非转换腿、`/v1/chat/completions` 直通）TTFT = 整段生成时间、生成期零心跳、内存 ≈ 2× 整段且无上限。
3. **P1 · W29-2（未修）**：`fw.Write` 在 passthrough 下把每个字节再复制进**无上限**的 `bytes.Buffer`（`provider_failover.go:359-362`），而 `Prefix()/streamClosed()/StreamLen()` 全仓零/仅测试调用 ⇒ 8.5MB 流常驻 8.5MB。W29-3（心跳进缓冲）同因。
4. **P1 · D31-1（新增）**：`reviveScheduledTask` 的注释写"人工暂停永不被撤销"，实现是"disabled + 至少一个 provider 可用即复活" ⇒ 用户在控制台暂停的额度保活任务会被每分钟的 tick 重新启用并继续对真实账号发探测请求。
5. **P1 · D31-2（新增）**：监控中心启动时传的是 `context.Background()`（`app/runtime.go:353`），`Runtime.Stop` 只 `db.Close()`（`:492`）⇒ ticker 停不下来，继续每 60s 往已关闭的库写 `settings`，错误被 `_ = s.Tick(ctx)` 吞掉。
6. **P1 · D31-5（新增）**：ChatGPT 令牌续期在等锁后重读了 provider，但只用重读结果做"短路返回"，真正兑换仍用调用方快照的 `RefreshToken`（`chatgpt_oauth.go:436`）；Claude/Cursor 已改用最新凭证（`claude_oauth.go:549-555`）。OpenAI 是轮换式刷新令牌，用旧 token 兑换会 `refresh_token_reused` 整链作废。
7. **做对的**：R4（转换路径流截断）本轮**真正闭环**——6 处判定改为"读后取值"，`cleanEOFInterruption` + `streamEndReader` 全字节判据，测试绿；截断收尾从伪造 `finish_reason=length`/`stop_reason=max_tokens` 改为协议错误帧，与 magpie `gateway.go:4940 streamFailure` 一致。
8. **治理（连续第 10 轮）**：线上二进制 `vcs.revision=d93ec02`、`vcs.modified=true`、部署时间 06:32:39 PDT（=21:32:40 +0800）⇒ 线上是**无 commit 可复现的 WIP 构建**，且不含本轮修复。回滚只能重编译。

## 二 基线（build / vet / test / 卫生）

| 项 | 结果 |
|---|---|
| `go build ./...` | 通过（rc=0，无输出） |
| `go vet ./...` | 通过（无输出） |
| `go test ./internal/gateway/ ./internal/store/ ./internal/app/ ./internal/monitoring/` | **全绿**：gateway 55.653s ok、store ok、app 1.503s ok、monitoring 0.036s ok |
| 工作区卫生 | `git status --porcelain` 空；无 `zz_*_test.go` 残留；gofmt 未跑（只读约束，且历史 CRLF 假阳性） |
| 提交卫生 | **不合格**：`.superpowers/brainstorm/43832-1792745803/{.server.pid,.server-stopped,*.html}`（含运行时 pid 文件）、`docs/superpowers/plans/*.md`（3159 行）、`docs/superpowers/specs/*.md`（762 行）、`internal/gateway/zz_quota_keeper_e2e_hooks.go`（生产源码里的测试钩子）一并入仓，混在 128 文件 20k 行的单提交里 |

## 三 新提交审查（2045543，d93ec02..HEAD）

### 做对的

- **截断收尾改协议错误帧**（`stream_interruption_close.go:38-77` Chat、`:94-135` Claude、`:157-186` Chat→Responses）：不再发 `finish_reason=length`+`[DONE]`/`stop_reason=max_tokens`+`message_stop`，改为 `{"error":{"type":"api_error"}}`。对照 magpie `internal/gateway/gateway.go:4940-4966 streamFailure` 逐协议一致（Chat 无 event 名、Responses `response.failed`、Claude `event: error`）。这是 r1 以来 R4 的正面收口。
- **转换路径流式落地**（`passthrough_stream.go:354-470` `converterStreamWriter`）：首字节前只 hold 开头帧（≤1500ms），首个内容事件即提交并直写；对照 magpie `holdWriter`（`fallback.go:1779-1834`）思路一致。
- **`interceptingStreamWriter` 已删除**（`protocol_proxy.go` 全仓零命中）⇒ r24/N24-1 的"全量拦截再 replay"第二半确实消除。
- **R4 六处判定**：`convert_claude_openai_stream.go:216`、`convert_claude_responses_stream.go:430`、`convert_openai_claude_stream.go:297`、`convert_openai_responses_chat_stream.go:674`+`:1036`，均为"循环结束后取值"（r27 之前是在读取前快照 ⇒ 恒 false）。`convert_clean_eof_test.go` 覆盖，本轮全绿。
- **`stream_end_reader.go` 判据改为全字节**（1072 行，`Write` 收全部字节，`usage.closed()` 由折叠器给出）⇒ N10-1 的"只看头部 256KB"确已彻底消失；`TestWritePassThroughResponseJudgesLongStreamWhole`（3× cap）绿。
- **负缓存 5/5 入口、A4 overage、lane_group 落库、monitoring 迁移与路由接线**（r30 已验，本轮随提交固化）。

### 问题表

| 编号 | 级别 | 问题 | 依据 |
|---|---|---|---|
| D31-1 | P1 | 用户手动暂停的额度保活任务会被 keeper 每分钟复活；注释自称"人工暂停永不撤销"，实现只判 `Enabled==false && 有 live provider` | `quota_keeper.go:482-500`（判据 `:485-489`）、调用点 `:348`/`:524` |
| D31-2 | P1 | 监控中心不可停止：`StartMonitoring(context.Background())` 无 cancel；`Runtime.Stop` 直接 `db.Close()`；tick 错误被 `_ =` 丢弃 | `app/runtime.go:353`、`:492`；`monitoring/service.go:38-51`（`_ = s.Tick(ctx)` 两处） |
| D31-3 | P2 | 引擎吞事件写库错误：`UpsertMonitorState` 失败 `return nil`（无日志无重试），`InsertMonitorEvent` 失败 `id, _ :=` 仍照常分发 ⇒ 有告警状态、无事件行 | `monitoring/engine.go:315-324`（`:322` `id, _ :=`） |
| D31-4 | P1 | 用量缓存 key 只含 providerID，不含账号身份 ⇒ 同 provider 行换号重连后，旧账号额度最长 1h 仍当新账号显示，并参与调度排序 | `oauth_usage_refresh.go:187/194/234/284`（`"<provider>:"+providerID`）、`oauth_usage_cache.go:15`、`provider_load.go:246-266` |
| D31-5 | P1 | ChatGPT 令牌续期等锁重读后仍用旧快照的 refresh_token 兑换（轮换令牌 ⇒ reused ⇒ 整链作废） | `chatgpt_oauth.go:433-441`（`:436` 用 `provider.ChatGPTOAuth.RefreshToken`）；对照 `claude_oauth.go:549-555` 用 `current` |
| D31-6 | P2 | 负缓存"读侧"是死代码：`lastError`/`waitForRefresh` 生产代码零调用（仅测试），注释宣称的面板"上次好值+错误/重试倒计时"未实现 | `oauth_usage_cache.go:64-72`、`:116-131`（grep 排除 `_test` 与本文件定义后无调用点） |
| D31-7 | P3 | 提交卫生：`.superpowers/**`（含 `.server.pid`/`.server-stopped` 运行时文件）、3.9k 行 superpowers 文档、生产源码里的测试钩子一并入仓；单提交混杂 128 文件 | `git show --stat 2045543`、`internal/gateway/zz_quota_keeper_e2e_hooks.go:1-34` |
| D31-8 | P2 | `checkin_history` 无 `account_key`，主键 `(provider_id, day)` ⇒ 同一天换号互相覆盖；且 `ListCheckinHistory` 全仓零调用（只写不读） | `internal/store/checkin.go:57-65`、`monitoring_schema.go:61-73` |

## 四 工作区 WIP 审查

**本轮无 WIP。** `git status --porcelain` 为空，`git diff` 与 `git diff --cached` 均为空（哈希 `e3b0c442…`）。相对 r30 的进展：上轮 126 文件 WIP 全部转为提交 `2045543`，其中 r30 之后新增的部分已计入第三节 D31-*。

## 五 生产环境只读核查（173.249.207.202）

| 项 | 结果 |
|---|---|
| 服务状态 | `systemctl is-active llm-protocol-gateway.service` → **active**；`ActiveEnterTimestamp=Fri 2026-10-23 06:32:39 PDT` |
| 二进制 | `/opt/llm-protocol-gateway/bin/gateway`，16498850 字节，mtime `2026-10-23 06:32:24`（PDT）= 21:32:24 +0800 |
| 版本串 | `vcs.revision=d93ec02c9365b20a65d0e92702a92b998b9632d7`，**`vcs.modified=true`** |
| 独有符号 | `shares account lane`：1（d93ec02 提交串）；`converterStreamWriter`：2、`monitor_events`：4（**仅 WIP/新提交有**）；`connection lost mid-reply`：**0**（本提交的截断收尾串不在线上） |
| 判定 | 线上 = **d93ec02 + 未提交改动**，不含 2045543。连续第 10 轮"生产跑无 commit 的 WIP"；W29-1 的缓冲路径在线上原样生效，本轮修复也尚未上线 |
| 数据 | 本轮未执行任何库查询，未写入、未触发上游计费请求 |

## 六 完整排查清单（账本全量，按模块）

> 状态：✅ 已修复 / 🟡 部分修复 / ❌ 未修复 / ⚪ 不适用；附加：回归 / 新增 / 未核查。`本轮变化` 为 `—` 表示继承（代码哈希未变则复用上轮证据）。

### M1 协议转换·请求方向

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| P1 | Claude→Chat 流式丢 prompt_tokens | P1 | ✅ | usage 折叠 | — |
| P2 | stop_reason 全量映射缺项 | P1 | ✅ | 映射表 | — |
| P3 | Chat→Responses 有 tool_calls 清空正文 | P1 | ✅ | 测试 | — |
| P4 | Responses→Chat 丢 reasoning 增量 | P1 | ✅ | 测试 | — |
| P5 | Responses→Chat 忽略 incomplete | P2 | 🟡 | 部分字段 | — |
| P7 | sanitizeAnthropicToolUseID 碰撞 | P1 | ✅ | 测试 | — |
| P8 | Responses→Claude 缺合成 tool_result | P1 | ✅ | 测试 | — |
| P9 | redacted_thinking 占位非回放 | P2 | 🟡 | convert_* | — |
| P10 | strict/top_k/stop_sequences 不转发 | P2 | 🟡 | convert_* | — |
| P11 | 两两手写转换 vs magpie 单一 IR | P2 | ❌ | 架构未动 | — |
| H-1 | Provider `name` 直发 Claude 上游 400 | P1 | 🟡 未核查 | `restoreDeclaredToolName` 在，未逐用例重跑 | — |
| H-2 | `response_format=json_object` 直传 Claude | P1 | 🟡 未核查 | `convert_response_format.go:19-20` 映射 output_config.format | — |
| H-3 | thinking 覆盖 output_config | P1 | 🟡 未核查 | `claude_pass_through_normalize.go:89-94` | — |
| H-4 | Responses allowed_tools 塞进 tool_choice | P1 | 🟡 未核查 | `convert_responses_allowed_tools.go` | — |
| H-5 | cache marks 白名单漏 tool_result | P1 | 🟡 未核查 | `convert_openai_claude.go:150-151` | — |
| H-6 | thinking 五连 | P1 | 🟡 未核查 | 未复跑 | — |
| M-1 | Claude→Chat 无条件 stop | P1 | ✅ | 有条件守卫（r10 纠错） | — |

### M2 协议转换·流式

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| P6 | Chat→Claude block index 错乱 | P1 | ✅ | 测试 | — |
| R4 | 流截断被当正常结束 / 不能续 | P0 | ✅ | 6 处读后判定（`convert_claude_openai_stream.go:216`、`convert_claude_responses_stream.go:430`、`convert_openai_claude_stream.go:297`、`convert_openai_responses_chat_stream.go:674/1036`）+ 收尾改协议错误帧（`stream_interruption_close.go:38/94/157`）+ `convert_clean_eof_test.go` 绿 | **确认闭环**（E2+E3） |
| N10-1 | 透传 >256KB 流被判截断 | P0 | ✅ | `usage.closed()` 全字节判据；`TestWritePassThroughResponseJudgesLongStreamWhole` 绿 | — |
| N10-2 | 中断收尾帧写裸 w | P1 | ✅ | 改走 hw | — |
| N11-2 | 心跳 stop 不 join | P1 | ✅ | 有界 join 2s | — |
| N11-3 | 透传 Claude 收尾漏 content_block_stop | P2 | ✅ | openBlocks 逐个 close | — |
| N11-4 | 透传 Responses 收尾帧缺 id/model/seq | P2 | ✅ | responsesStreamIdentity | — |
| N14-2 | 转换路径中断判定恒 false | P0 | ✅ | 读后判定 | — |
| N23-1 | canDefer 在流式路径是死变量 | P1 | ❌ | 与 W29-1 同因 | — |
| N23-2/3 | 两套完成判定结论相反 | P2 | 🟡 | 部分统一 | — |
| N24-3 | streamEndReader 只携带 4 字节 | P2 | ✅ | 按 marker-1 | — |
| N24-5 | 两套完成判定无回声守卫 | P3 | 🟡 | 仅末 64B | — |
| N25-1 | 中断谓词语义反了 | P1 | ✅ | 已修 | — |
| N25-3 | scanResponses 非 Responses 流全量累积 | P2 | 🟡 未核查 | 未复跑 | — |
| N25-5 | 缓冲路径零心跳 | P2 | ❌ | 并入 W29-3 | — |
| **W29-1** | **透传流全量缓冲（TTFT=总时长）** | **P0** | **❌** | **PROBE-31（本提交内 A/B）：`failover-writer(canDefer=true) firstWrite=801ms total=801ms writes=1`；`plain-writer(control) firstWrite=0s total=801ms writes=3`。成因：`passthrough_stream.go:280-283` 无条件走 `writeBufferedStream`，`:530-537` `hw.Write→fw.Write` 只落 `buf`，`:560` 才 `FlushError()` 提交** | **连续第 11 轮，未修** |
| W29-2 | passthrough 下每字节再复制进 fw.body（无上限） | P1 | ❌ | `provider_failover.go:359-362`（`w.body.Write(p[:n])` 无条件）；`Prefix/streamClosed/StreamLen` 零或仅测试调用 | — |
| W29-3 | 心跳进缓冲区，流中收不到 ping | P1 | ❌ | 同 W29-1：`hw→fw.Flush` 在 `!passthrough` 时是空操作 | — |
| W29-4 | 红测试 TestConverterStreamWriterKeepsTheStatusItWasGiven | P1 | ✅ | 本机绿 | — |
| W29-6 | scanResponses 非 Responses 流全量累积 | P2 | 🟡 未核查 | 未复跑 | — |
| W29-7 | 回声守卫只看末 64B | P2 | 🟡 | 代码 | — |

### M3 调度/路由/并发/故障转移

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| R1 | 无 RPM 限流 | P1 | ✅ | 8b5542d 滑动窗口 | — |
| R2 | 冷却不看 Retry-After | P1 | ✅ | 阶梯可达 | — |
| R3 | billing 归入 hardQuota | P1 | ✅ | 代码 | — |
| R5 | 会话粘性不落库 | P1 | ✅ | session_pins | — |
| R6 | 并发闸门非 FIFO | P1 | 🟡 | D29-3/D29-4 仍在 | — |
| D29-1 | laneGroup 完全不落库 | P0 | ✅ | 列+迁移+回环测试绿 | — |
| D29-2 | Retry-After 出口按行号查分组 | P1 | 🟡 | `protocol_proxy.go:833`、`provider_failover.go:1234` 已用 `retryAfterSecondsFor`；**`server.go:4492` 仍 `retryAfterSeconds(provider.ID, …)`** | 复核，2/3 |
| D29-3 | 组内限额取"最后到达那行" | P1 | ❌ | `provider_lanes.go:123-139`：`laneFor` 每次 acquire 覆盖 `lane.max/queueCap/rpm` | 未修 |
| D29-4 | 组内 0/0 行整组绕过 | P1 | ❌ | `provider_lanes.go:558-561`：`max<=0 && rpm<=0` 直接返回空 release，未查组 | 未修 |
| D29-5 | 大小写：注释称不敏感、实现精确匹配 | P2 | ❌ | `providerLaneKey` 仅 `TrimSpace` | 未修 |
| D29-6 | providerInFlight 按行号查 | P2 | ❌ | `provider_lanes.go:565-578` 用原始 `providerID` 取 `lanes[]`，分组 lane 键是 `group:xxx` ⇒ 组内恒 0 | 未修 |
| D29-7 | d93ec02 夹带 failover 行为变更 | P3 | ❌ | 提交粒度 | — |
| D29-8 | lane 只增不删 | P3 | ❌ | 无回收 | — |
| W29-11 | failoverResponseWriter 死代码 | P3 | 🟡 | `stream()` 在用 | — |

### M4 Provider/账号/OAuth/额度

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| A1 | Trae/WorkBuddy/Cursor 刷新无锁 | P1 | ✅ | oauth_renew_lock | — |
| A2 | 子串匹配 host | P1 | ✅ | baseURLHostIn | — |
| A3 | /v1/images 路由误判 | P2 | 🟡 | `resolveProviderImageURL` 仍盲拼 | — |
| A4 | Claude overage / 负缓存 | P1 | ✅ | 负缓存 5/5 | — |
| A5 | Cursor 配额提前量 | P2 | ✅ | r10 纠错 | — |
| A6 | 出图超时被 120s 砍 | P2 | ✅ | Cursor 10min | — |
| N-1 | workbuddy_ai exp 空→永不续期 | P1 | ✅ | 401 自愈 | — |
| H-7 | images 拒绝检查路由而非决策 ProviderID | P1 | 🟡 未核查 | 未复跑 | — |
| H-9 | workbuddy 11128 校验矩阵 | P1 | 🟡 未核查 | 未复跑 | — |
| **D31-1** | 用户暂停的保活任务被每分钟复活 | P1 | ❌ 新增 | `quota_keeper.go:485-489` 判据 + 调用点 `:348/:524` | 新增 |
| **D31-4** | 用量缓存 key 不含账号身份（跨账号串额度 1h） | P1 | ❌ 新增 | `oauth_usage_refresh.go:187/284`、`oauth_usage_cache.go:15` | 新增 |
| **D31-5** | ChatGPT 续期用旧快照 refresh_token | P1 | ❌ 新增 | `chatgpt_oauth.go:436` vs `claude_oauth.go:549-555` | 新增 |
| **D31-6** | 负缓存读侧死代码（lastError/waitForRefresh 零调用） | P2 | ❌ 新增 | `oauth_usage_cache.go:64-72/116-131` | 新增 |
| **D31-8** | checkin_history 无 account_key 且只写不读 | P2 | ❌ 新增 | `store/checkin.go:57-65`、`monitoring_schema.go:61-73` | 新增 |

### M5 计费/用量/日志

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| N11-1 | 长流 usage 归零→估算放大 115× | P0 | ✅ | streamUsageReader 逐字节折叠；3×cap 测试绿 | — |
| M-19 | 错误响应被估算计费 | P1 | ✅ | 已修 | — |
| N12-2 | 日桶用服务器时区 | P2 | ⚪ | 生产 unit TZ=Asia/Shanghai（/proc 实测） | — |
| N13-2 | /__usage-analysis 无上限 | P2 | ❌ 未核查 | 本轮未复跑 | — |
| N23-5 | stream+JSON 回包 usage=0 | P2 | 🟡 | 部分 | — |
| W29-5 | internal/monitoring 整包未接线 | P2 | ✅ | 已随提交固化（18 路由 + 迁移 + 启动） | **固化** |
| W29-9 | streamUsageFolder reflect 分派脆 | P3 | ❌ | 代码 | — |
| **D31-3** | 引擎吞事件写库错误（状态 firing、无事件行） | P2 | ❌ 新增 | `monitoring/engine.go:315-324` | 新增 |

### M6 安全/鉴权/隔离

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| S1 | 管理 API `ACAO: *` | P0 | ✅ | cors 白名单 | — |
| S2 | 请求体无上限 | P1 | ✅ | request_body_limit | — |
| S3 | 登录/setup 无限流 | P1 | 🟡 | 死代码/TOCTOU | — |
| S4 | 日志正文不脱敏 | P1 | ✅ | log_scrub | — |
| S5 | 首包后无空闲超时 | P1 | 🟡 | 守卫有，心跳被 W29-3 吞 | — |
| N12-3 | 登录先解码后限流 | P3 | 🟡 | 部分 | — |
| H-8 | 管理员 Key 链不过滤 | P1 | 🟡 未核查 | 未复跑 | — |
| W30-3 | 18 条 /__monitor 路由无 Origin/CSRF 校验 | P2 | ❌ 新增 | `monitoring_handlers.go:118-127`（全部 requireAdmin）、`cors.go:61-81`（只写响应头不拒请求）、`admin_auth.go:326-333`（回环免鉴权） | 复核：全部 admin 成立，无 CSRF 成立 |

### M7 存储/迁移

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| W30-2 | 生产库已建 monitor_* 六表但全空 | P3 | 观察 | 生产只读查询（r30） | 本轮未重查 |
| D31-8 | checkin_history 丢 account_key / 只写不读 | P2 | ❌ 新增 | 见 M4 | 新增 |

### M8 前端控制台

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| W-UI1 | 无实时日志推送（无 EventSource） | P2 | ❌ | `grep -rn EventSource web/src` 零命中 | 仅 grep，未做几何实测 |
| W-UI2 | 无"重放请求"入口 | P2 | ❌ | `grep 重放|replay web/src` 零命中 | 同上 |
| W-UI3 | sessionId 只在类型里，界面无展示 | P3 | ❌ | `web/src/types.ts:679`（唯一命中，无组件消费） | 同上 |
| W-UI4 | 日志列表无成本列 | P2 | ❌ | 成本列只在 apikeys/charts | 同上 |
| W-UI5 | 未读告警角标 | P3 | ❌ | 无命中 | 同上 |
| W-UI6 | 监控页无会话/请求时间线 | P2 | ❌ | 无命中 | 同上 |

> M8 本轮仅做 grep 断言，未用 `measure.cjs`/`shot-tz.cjs` 实测几何与时区 ⇒ 标记"未核查（超时盒）"，不计通过。

### M9 隧道/系统面

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| N13-1 | `gateway version` 零 flag 解析会起服务 | P2 | ❌ | 历史复现 | 本轮未复现（遵守硬约束 5） |
| N13-3 | 容器无 TZ 且 verify 脚本期望 LA | P3 | ❌ | 未改 | — |

### M0 卫生

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| W30-1 | `zz_quota_keeper_e2e_hooks.go` 生产源码里的测试钩子 | P3 | ❌ | 文件已随提交入仓，导出 `RunQuotaKeeperTickForTest`/`HandleRestoreProviderForTest`（伪造 admin 身份） | **从 WIP 变成正式提交** |
| D31-7 | 提交卫生：.superpowers 运行时文件、3.9k 行文档、单提交 128 文件 | P3 | ❌ 新增 | `git show --stat 2045543` | 新增 |

## 七 回退与假修复

- **回归**：本轮无状态从 ✅ 变非 ✅ 的项。但需点名一个**结构性事实**：D31-1/D31-2/D31-5 都是"注释/设计意图与实现相反"（假修复形态之一），其中 D31-1 的注释直接写着"人工暂停永不被撤销"——这与 r15/r30 的 `canDefer` 死变量、`interrupted` 恒 false 属同一类错误，建议作为团队级检查项。
- **关联回退嫌疑**：
  - `W29-1`（透传全量缓冲）与 `R4`（截断判定）本是一对相反需求：为了"截断后能换账号重试"，透传被整体缓冲；本轮把 R4 换成"错误帧诚实上报"后，缓冲的**唯一理由已消失，缓冲却留着**。这不是回退，而是**遗留设计未随之拆除**。
  - `W30-1` 从"未提交 WIP"变成"正式提交内容"，风险面从"本地"扩大到"任何拉这个 commit 的环境"，属**关联回退嫌疑**（治理层面倒退）。
- **假修复/自相矛盾**：无新增"测试与实现互相矛盾"项（本轮测试全绿，且 r30 的红测试已转绿）。但 D31-1 的注释与实现矛盾、D31-6 的注释宣称能力而调用点为零，均按"假修复"记账。

## 八 下轮验收门槛（逐条可机械验证）

1. `writePassThroughResponse` 的失败转移路径（`*failoverResponseWriter` + `stream` + 2xx）必须在 `io.Copy` **之前**完成 header 提交：验证命令——跑一个慢上游探针，断言 `firstWrite < total/2` 且 `writes >= 3`。（当前：`firstWrite==total`、`writes==1`）
2. `go test ./internal/gateway/ -run 'TestWritePassThroughResponse|TestZZProbeStreamingFirstByte'` 中新增一个**断言首字节时延**的用例（不要只断言 body 内容），且该用例对本轮实现为红。
3. `fw.body` 增加上限（与 `passThroughLogBufferMax` 同源）或删除该字段；验证：8MB 流跑完后 `len(fw.body) <= cap`，`go vet` 无未使用字段告警。
4. 心跳在 passthrough 路径可见：探针在流中途收集 ≥1 个 `: ping`，且 `flushes > 0`。
5. `reviveScheduledTask` 只在"keeper 自己暂停的"任务上生效：给 `domain.ScheduledTask` 增加暂停来源标记并落库，验证用例——用户把健康任务 `Enabled=false` 后跑一次 tick，任务仍为 disabled。
6. `StartMonitoring` 接收可取消 ctx；`Runtime.Stop` 先 `cancel()` 再 `db.Close()`；验证——Stop 后 3 分钟内 `settings` 表无新写入。
7. `engine.go` 的 `UpsertMonitorState`/`InsertMonitorEvent` 错误必须 `slog.Warn` 且不产生"状态 firing 而事件缺行"：用例注入 store 错误，断言返回 error 且状态未置 firing。
8. OAuth 用量缓存 key 加入账号指纹（token hash 或 open_id），换号后 `get` 必须 miss：用例——同 providerID 改 token 后旧值不可读。
9. `chatgpt_oauth.go` 兑换必须用 `fresh.ChatGPTOAuth.RefreshToken`；用例——重读后 token 已变，断言调用参数为新值。
10. `server.go:4492` 改用 `retryAfterSecondsFor(provider, now)`；用例——分组账号 429 后 `Retry-After` 为组窗口而非 1s。
11. `providerLanes` 组内限额来自成员行的**最大值**或显式组配置（不得由最后一次 acquire 覆盖）；`max/rpm 之和为 0` 的行不得绕过组 lane；`providerInFlight` 用 `providerLaneKey` 取 lane。
12. 提交里不得出现 `.superpowers/**`（含 `.server.pid`/`.server-stopped`）与 `zz_*_hooks.go`；`git show --stat` 中源码与文档分离为独立提交。
13. 生产二进制 `vcs.modified` 必须为 `false`，且 `vcs.revision` 等于已推送的 HEAD；`grep -a "connection lost mid-reply"` 命中 1（证明本轮修复已上线）。

## 附 覆盖度声明

| 模块 | 已扫文件数 | 该模块文件总数（约） | 未核查项 |
|---|---|---|---|
| M0 基线与卫生 | 全仓 build/vet/test + 提交元数据 | — | gofmt（只读约束未跑） |
| M1 请求方向转换 | 6（convert_* 非流式抽样 + response_format/allowed_tools/thinking 定位） | ≈14 | H-1..H-6 逐用例（超时盒） |
| M2 流式转换 | 9（passthrough_stream / sse_heartbeat / stream_end_reader / stream_interruption_close / provider_failover 写者 / 4 个 convert_*_stream） | ≈16 | N25-3、W29-6（未复跑）；W29-7 仅读代码 |
| M3 调度/路由/并发 | 5（provider_lanes / provider_failover / protocol_proxy / server.go 相关段 / key_quota） | ≈12 | 并发下的 lane 行为未做运行时压测 |
| M4 Provider/账号/额度 | 12（claude_oauth* / chatgpt_oauth / cursor_oauth / oauth_usage_* / quota_keeper* / checkin / workbuddy 定位） | ≈25 | H-7/H-9；未连真实上游 |
| M5 计费/用量/日志 | 8（usage / pricing / store 相关 / server 用量段 / monitoring 引擎） | ≈15 | N13-2 未复跑；`/__logs` 未实测 |
| M6 安全/鉴权 | 6（admin_auth / cors / access_source / api key 隔离 / monitoring_handlers） | ≈10 | 未做越权实弹测试 |
| M7 存储/迁移 | 6（sqlite / monitoring_schema / monitoring_migration / store/monitoring / checkin） | ≈10 | 生产库本轮未查询 |
| M8 前端控制台 | 4（grep main.tsx / components / types / styles） | ≈30 | **几何与时区实测未做**（超时盒），仅 grep |
| M9 隧道/系统面 | 0 | ≈6 | 全部未核查（只读未触发） |

未核查项一律不记为通过。M8 六项仅以 grep 佐证"能力缺失"，不构成 ✅/❌ 之外的质量结论。

## 收尾自检

- [x] `git status --porcelain` 只剩 `.workbuddy/reviews/` 产物（该目录被 `.gitignore` 忽略，porcelain 为空；探针 `zz_probe31_test.go` 已删除）
- [x] 无临时 worktree 残留（`git worktree list` 仅主树 `C:/guodevelop/llm-protocol-gateway 2045543`）
- [x] 账本已更新，未闭环 ID 全部出现在第六节
- [x] 每个 ✅ 均能指到 E1/E2/E3 之一（本轮新增的 R4 为 E2+E3；W29-1 的 ❌ 为 E1 反向证据）
- [x] 覆盖度声明已填，未核查项已列举
- [x] 无 commit / push / 生产写操作发生（VPS 仅 `systemctl is-active`、`ls -l`、`grep -a`、`systemctl show`）

## 给修复 agent 的订单（按级别，最小必要改动）

**P0**
1. `passthrough_stream.go:277-283`：透传 + 2xx + stream 时，在 `io.Copy` 之前 `fw.WriteHeader(status)` 并 `fw.FlushError()`（或直接给 `writeBufferedStream` 加"首块即提交"分支），使 `fw.passthrough=true`、后续 `fw.Write` 直写 `base`。语义目标：**首字节 = 首个上游 chunk 到达时刻**。验收：探针 `firstWrite < total/2`、`writes>=3`；生产 A/B `avg(latency−ttft)` 从 ~0 升到与 convert 路径同量级。
2. 同上，顺带删除"透传缓冲以便换账号重试"的语义（R4 已改为错误帧上报，缓冲已无理由）；若保留重试，只允许在**零字节已写出**时重试。

**P1**
3. `provider_failover.go:359-362`：`w.body` 加上限（复用 `passThroughLogBufferMax`），或彻底删除该字段并把 `delivered` 改为计数。
4. `quota_keeper.go:482-500`：暂停来源落库（`paused_by`），`reviveScheduledTask` 只复活 `paused_by=keeper` 的任务。
5. `app/runtime.go:353` + `monitoring/service.go:38-51`：`StartMonitoring` 接可取消 ctx，`Runtime.Stop` 先 cancel 再 `db.Close()`；`_ = s.Tick(ctx)` 至少 `slog.Warn`。
6. `chatgpt_oauth.go:436`：改用重读后的 `fresh.ChatGPTOAuth.RefreshToken`；无 fresh 时不得继续。
7. `oauth_usage_refresh.go` 各 `cacheKey`：加入账号指纹（token 前缀 hash 或 open_id），换号即 miss。
8. `server.go:4492`：改 `retryAfterSecondsFor(provider, time.Now())`。

**P2**
9. `monitoring/engine.go:315-324`：`UpsertMonitorState` 失败返回 error 并中止；`InsertMonitorEvent` 失败必须记 warn，且不得把状态留在 firing-无事件。
10. `provider_lanes.go:123-139/558-561/565-578`：组限额取成员最大值或组级配置；0/0 行查组；`providerInFlight` 走 `providerLaneKey`。
11. `oauth_usage_cache.go`：`lastError`/`waitForRefresh` 要么接进面板（`/__providers` 响应带 error 字段），要么删除并改注释。
12. `store/checkin.go:57-65`：`checkin_history` 补 `account_key` 并进主键；或明确删除该表的写入（当前只写不读）。

**P3**
13. 提交卫生：`.superpowers/**` 与 `internal/gateway/zz_*_hooks.go` 不得入库；文档与源码分提交；`git show --stat` 单提交文件数控制在可审范围（< 30）。
14. 生产发布流程：`vcs.modified=true` 连续 10 轮，改为"先 commit 再 build 再部署"，并把 `vcs.revision` 与 `grep -a` 特征串纳入部署后自检。
