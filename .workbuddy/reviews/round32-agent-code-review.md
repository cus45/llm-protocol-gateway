# round32 对抗性代码审查报告

> HEAD `2045543` ｜ `git status --porcelain` 0 行（工作区干净）｜ `git diff` 空 ｜ `git diff --cached` 空
> 内容指纹：`git diff | sha256sum` = `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`，`git diff --cached | sha256sum` = `e3b0c442…b855`
> 轮次：**round32**（.workbuddy/reviews 下最大 N=31，+1）；审查时间 2026-10-24 04:04–04:50 (+0800)；只读，未改任何业务代码

---

## 0 审查对象与增量判定

**判定：无增量（第三类）。** HEAD 与 round31 同为 `2045543`，`git diff`/`git diff --cached` 均为空文件哈希（`e3b0c442…` 是空输入的 SHA-256），即工作区与上一轮结束时逐字节一致。本轮没有新提交、没有 WIP 变化。

因此本轮口径：
1. 全量清单照常出（第六节 96 条）；
2. 允许复用上轮证据的项一律写成「继承（代码哈希未变，round31 第 E 号证据）」；
3. **所有未闭环 P0/P1 全部重跑一次验证**——本轮真正重跑的是：W29-1（P0，本地 A/B 探针 PROBE-32 重跑）、H-1/H-2/H-3/H-5（P1，决定性行逐条重看）、D31-1/D31-4/D31-5/H-7（P1，调用链重走）、D29-2/3/4/5/6（P1/P2，lane 代码重看）、生产只读核查（二进制特征串 + DB 结构/计数）。

增量统计：改动 0 文件 / 新增 0 / 删除 0；`git diff --stat` 空；新增行 0、删除行 0。

---

## 一 结论先说

**不能上线。** 本轮最大问题不是"有新 bug"，而是"第 12 轮了，唯一 P0 一行没动、生产上跑的还不是任何 commit"。

1. **P0 W29-1 透传流全量缓冲，连续第 12 轮未修。** PROBE-32 在本轮 HEAD 上重跑：经 `failoverResponseWriter` 的透传 SSE 首字节 `802ms` = 末字节 `802ms`、`writes=1`；同一函数换成裸 writer 则 `firstWrite=0ms`、`writes=3`。这就是 TTFT 被拉到总时长的直接原因，凡走网关 Key 的透传流全部命中。
2. **生产二进制仍是"脏树 + 非 HEAD"。** `vcs.revision=d93ec02`、**`vcs.modified=true`（连续第 11 轮）**，且二进制里没有 `lane_group`、没有 `monitoring` 任何字符串 ⇒ 生产既缺 `2045543` 的 R4 修复、也缺 `lane_group` 落库，却含 WIP 独有的 `without its closing frame` / `idleGuardedBody`。**当前没有任何一个 commit 能复现线上行为，回滚只能重编。**
3. **P1 H-1/H-2 是同一个病：同一语义两套实现互相打脸。** `convert_response_format.go:18-20/40-43` 给 Claude 写 `{"type":"json_object"}` 和 `format.name`；同仓库 `convert_responses_fields.go:71-80` 的注释明写「Claude GA 拒绝 `format.name`（Extra inputs are not permitted）」、并且把 json_object 降级成宽松 `json_schema`。两者不可能都对，错在 Chat 侧就是硬 400。
4. **P1 H-3 的账本指错了文件。** 覆盖 `output_config` 的不是 `claude_pass_through_normalize.go:89`（那里是 merge），而是 `convert_claude_openai.go:322-325`（`applyAdaptiveThinking` 整块赋值）与 `:394-397`（显式 thinking）。同一请求同时带 `response_format` + `reasoning_effort` 时 format 被静默抹掉。
5. **P1 H-5 白名单漏 `tool_result`。** `convert_claude_cache_marks.go:93-101` 只认 text/image/document；agent 续跑时最后一条正是 `tool_result` ⇒ 断点 0、前缀每轮全价。magpie 用黑名单（`anthropic.go:921-928`），天然覆盖。
6. **P1 D31-1 注释与实现相反。** `quota_keeper.go:477-481` 自称「a person's deliberate pause on a healthy task is never undone here」，`:486` 的实际判据是"有 live provider 就复活"，而人工暂停的正是健康的那些。用户暂停 20s 内被改回。
7. **P1 新增 D32-4：隧道 token 明文落盘。** `tunnel/manager.go:300-301` 把 `strings.Join(args," ")` 写进 `cloudflared.log`，而 args 含 `--token <token>`（`:653`），文件 0644（`:343`）。同机任意用户可读 ⇒ 可接管公网域名。
8. **修好的事也要说**：`H-9`（workbuddy 11128 矩阵）本轮可结案 ✅——`workbuddy.go:857` 同时覆盖 system/assistant，`:972-983` developer→system 归一化在 unflag 之前，测试 E3 可红；缓存 token 未重复计数 ✅（`pricing.go:139-160` 显式扣减 + 测试可红）；W29-5 18/18 路由全在 `requireAdmin` 后（逐条核过，无一处漏鉴权）。

---

## 二 基线（build / vet / test / 卫生）

| 项 | 结果 |
|---|---|
| `go build ./...` | 通过（rc=0，无输出） |
| `go vet ./...` | 通过（rc=0，无输出） |
| `go test ./internal/gateway/ ./internal/store/ ./internal/app/ ./internal/monitoring/ -count=1` | **全绿**：gateway 42.474s、store 1.511s、app 1.519s、monitoring 0.041s |
| 工作区卫生 | `git status --porcelain` 空；`.bak` 0 个；非测试 `println` 0 处 |
| 生产源码里的测试钩子 | `internal/gateway/zz_quota_keeper_e2e_hooks.go` **仍在且已入仓**（W30-1，第 2 轮挂账） |
| 提交卫生 | `.superpowers/` 5 个文件仍被跟踪（含 `.server.pid`/`.server-stopped`）（D31-7） |
| 前端类型/构建 | 不重复跑（本轮零增量，继承 round31 的 0 错） |

> 注：`gofmt` 仍不跑——本仓库 CRLF 检出会把 6 个文件报成假阳性，且只读约束下也无法 `gofmt -w` 后回写。

---

## 三 新提交审查

**本轮无新提交。** 上一轮 `2045543` 已审（R4 闭环、`interceptingStreamWriter` 删除、对齐 magpie `streamFailure`），本轮复核结论不变：

- 做对的：6 处中断判定改为「读后取值 + `cleanEOFInterruption`」；收尾从伪造 `finish_reason=stop`/`stop_reason=end_turn` 改为协议错误帧（与 magpie `gateway.go:4940 streamFailure` 同语义）；`convert_clean_eof_test.go` 绿。
- 提交里仍然带着 D31-7（`.superpowers`、3.9k 行文档、128 文件混一个提交）和 W30-1（生产源码测试钩子）。

问题表：无新增 D* 行（历史 D29-*、D31-* 见第六节）。

---

## 四 工作区 WIP 审查

**无 WIP。** `git diff` 为空哈希，相对 round31 的进展是 0（round31 把 126 文件 WIP 压成单提交后一直没再动）。

问题表：无新增 W* 行；历史 W29-1/W29-2/W29-3/W29-6/W29-7/W29-9/W29-11/W30-1/W30-2/W30-3 见第六节。

---

## 五 生产环境只读核查（173.249.207.202）

只用了 `systemctl`、`ls -l`、`sha256sum`、`grep -a`（**VPS 上没有 `strings` 也没有 `go`**，本轮改用 `grep -a -o` 取 vcs 串）、`journalctl`、`sqlite3 uri mode=ro`（python3）。未 restart、未写库、未触发上游计费请求、未运行 `gateway version`。

| 项 | 读数 |
|---|---|
| 服务 | `active`，MainPID `911205` |
| 二进制 | `/opt/llm-protocol-gateway/bin/gateway`，16498850 B，mtime `2026-10-23 06:32`（= 21:32 +0800），sha256 `9a0d86d6…c31f` |
| 版本归属 | `vcs.revision=d93ec02c9365…` ｜ **`vcs.modified=true`（连续第 11 轮）** |
| 特征串 | `without its closing frame`=1（WIP 独有）、`idleGuardedBody`=3（WIP 独有）、`shares account lane`=1；`monitoring snapshot failed`=0、`lane_group`=0、`monitor_provider_samples`=0 |
| 库 | `gateway.db` 2.41 GB，mtime `2026-10-23 12:32`（约 30 分钟前仍在写） |
| 稳定性 | `journalctl` 近 24h `panic|fatal` 命中 **0** |
| Provider | 50 行（其中 disabled/deleted 之外 20 行），API Key 9 个 |
| lane 配置 | `providers` 表**无 `lane_group` 列**；`max_concurrent_requests=1` 的 Provider **1 个**（`deepseek-gq-api`，queue=0、rpm=0）；`requests_per_minute>0` 为 0 |
| monitor 表 | 6 张表都存在；`monitor_events` / `monitor_alert_states` / `monitor_event_pushes` / `monitor_baselines` / `provider_metric_samples` 全 **0 行**；`checkin_history` 18 行 |
| 24h 流量 | convert(OpenAI Chat→Claude) 13714 条、avg gap(latency−ttft) 1117.8ms；pass_through(Claude→Claude) 1742 条、avg gap 3145.7ms；pass_through 全部 1883 条中 1367 条 gap≤5ms |

**对 W29-1 的 E4 说明（不作为判据）**：pass_through 有 73% 的行 gap≤5ms，与全量缓冲一致；但 `request_logs` 没有 stream 列，非流式请求按实现会把 `ttft := latency`（`server.go:1370`）而同样得到 gap=0，**无法从库上把两类分开**，所以本条不构成 A/B，只作旁证。W29-1 的判定依据是本地 E1（PROBE-32）。

**对 W30-2 的修正**：M5 子 agent 提出「线上 `vcs.revision=d93ec02` 树内 `internal/monitoring/` 为 0 文件 ⇒ 空表来自脏树构建」。本轮实测二进制里 `monitoring snapshot failed` 命中 **0**，与该解释一致：建表来自更早的脏树构建，当前进程不含监控代码 ⇒ 空表不是"采集坏了"，是"采集根本没跑"。

**给运维的两条硬事实**：`providers` 无 `lane_group` 列 ⇒ D29-1 的落库修复未上线；有 1 个 Provider 配了并发 1 ⇒ lane admission 在生产**已生效**（分组类缺陷 D29-3/4 仍潜伏，因为无法配组）。

---

## 六 完整排查清单（按 M1–M9 分表，账本全量）

图例：✅ 已修复（满足第四节门槛）｜🟡 部分修复｜❌ 未修复（含假修复）｜⚪ 不适用。证据级别：E1 运行时 A/B、E2 决定性代码位置、E3 测试可红、E4 只读观察。

### M1 协议转换·请求方向

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| P1 | Claude→Chat 流式丢 prompt_tokens | P1 | ✅ | 继承（代码哈希未变，r29 usage 折叠） | 无 |
| P2 | stop_reason 全量映射缺项 | P1 | ✅ | 继承（r29 映射表） | 无 |
| P3 | Chat→Responses 有 tool_calls 清空正文 | P1 | ✅ | 继承（r29 测试） | 无 |
| P4 | Responses→Chat 丢 reasoning 增量 | P1 | ✅ | 继承（r29 测试） | 无 |
| P5 | Responses→Chat 忽略 incomplete | P2 | ✅ | 非流 `convert_openai_responses_chat.go:870-872` + `convert_tools.go:1749-1760`；流 `convert_openai_responses_chat_stream.go:661-666`→`:525-536` | **🟡→✅**（E2+E3 等价，`stop_reason_mapping_test.go:90-136` 改回旧实现必红） |
| P6 | Chat→Claude block index 错乱 | P1 | ✅ | 继承（r29 测试） | 无 |
| P7 | sanitizeAnthropicToolUseID 碰撞 | P1 | ✅ | 继承（r29 测试） | 无 |
| P8 | Responses→Claude 缺合成 tool_result | P1 | ✅ | 继承（r29 测试） | 无 |
| P9 | redacted_thinking 丢失/占位非回放 | P2 | 🟡 | 已通：`convert_claude_responses.go:205`、`convert_responses_claude.go:24-78/241`、`claude_pass_through_normalize.go:321-326`；仍丢：`convert_openai_claude.go:73-75/139-141`（Claude↔Chat 无条件丢）、`convert_responses_claude.go:380-394` 末尾退化为空 thinking | 细化缺口 |
| P10 | strict/top_k/stop_sequences 等不转发 | P2 | 🟡 | strict 已通（`convert_tools.go:442/450/499-504/1000-1004`）；stop 3/4 方向通，Chat→Claude 缺（见 D32-1）；top_k 仅 `convert_responses_claude.go:484` | 细化缺口 |
| P11 | 两两手写转换 vs magpie 单一 IR | P2 | ❌ | 18 个非测试 `convert_*.go`，无 `ir.go`；magpie `ir.go:154 Request` + `buildAnthropic:839`/`buildChat:268`/`buildResponses:1089` | 确认未动 |
| H-1 | `output_config.format` 形状：name 是否该发 | P1 | ❌ | `convert_response_format.go:40-43` 写 name；**同仓 `convert_responses_fields.go:71-72` 注释明写 Claude 拒收** ⇒ 两者必有一错 | **复核成立（E2）** |
| H-2 | `response_format=json_object` 原样直传 | P1 | ❌ | `convert_response_format.go:18-20` 直出 `{"type":"json_object"}`；`convert_responses_fields.go:73-80` 明说 Claude 无此模式并降级为宽松 json_schema | **复核成立（E2）** |
| H-3 | thinking 覆盖 output_config | P1 | 🟡→实质 ❌ | 真正覆盖点：`convert_claude_openai.go:322-325`（`applyAdaptiveThinking` 整块赋值）、`:394-397`；顺序 `:208`(写 format) → `:213`(thinking) ⇒ format 被抹。账本原指的 `claude_pass_through_normalize.go:89` 是 merge，**指错文件** | **定位纠正，缺陷仍在** |
| H-4 | Responses allowed_tools 塞进 Claude tool_choice | P1 | 🟡 | `convert_responses_claude.go:496-516` 构造 `allowed_tools` 键并收窄 tools；magpie `responses.go:502-533` 只置 mode + 过滤，从不下发该键。缺上游 schema 实证 | 未变（🟡） |
| H-5 | cache marks 白名单漏 tool_result | P1 | ❌ | `convert_claude_cache_marks.go:93-101` 只认 text/image/document；`markClaudeMessageTail:68-90` 找不到即 return false；agent 续跑末条为 tool_result ⇒ 断点 0。magpie 用黑名单 `anthropic.go:921-928` | **复核成立（E2，账本原引 `convert_openai_claude.go:150` 是反方向）** |
| H-6 | thinking 五连（预算补正/glm none/unknown 模型） | P1 | 🟡 | 仅确认 `claude_pass_through_normalize.go:151-169` 无条件改写 max_tokens；其余分支未读 | 未核查 |
| M-1 | Claude→Chat 无条件 stop | P1 | ✅ | 继承（r10 纠错） | 无 |

### M2 协议转换·流式

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| R4 | 流截断不能续/被当正常结束 | P0 | ✅ | 6 处读后判定（`convert_claude_openai_stream.go:216`、`convert_claude_responses_stream.go:430`、`convert_openai_claude_stream.go:297`、`convert_openai_responses_chat_stream.go:674/1036`）+ `stream_interruption_close.go:38/94/157` 协议错误帧（对齐 magpie `gateway.go:4940`）+ `convert_clean_eof_test.go` 绿 | 继承 r31（E2+E3） |
| N10-1 | 透传 >256KB 流被判截断 | P0 | ✅ | 继承（r29 全字节判据） | 无 |
| N10-2 | 中断收尾帧写裸 w 绕过锁 | P1 | ✅ | 继承（r29 改走 hw） | 无 |
| N11-2 | 心跳 stop 不 join | P1 | ✅ | 继承（r29 有界 join 2s） | 无 |
| N11-3 | 透传 Claude 收尾漏 content_block_stop | P2 | ✅ | 继承（r29 openBlocks 逐个 close） | 无 |
| N11-4 | 透传 Responses 收尾帧缺 id/model/seq | P2 | ✅ | 继承（r29 responsesStreamIdentity） | 无 |
| N14-2 | 转换路径中断判定恒 false | P0 | ✅ | 继承（r29 读后判定） | 无 |
| N23-1 | canDefer 在流式路径是死变量 | P1 | ❌ | 与 W29-1 同因，合并 | 无 |
| N23-2/3 | 两套完成判定结论相反 | P2 | 🟡 | 继承（r29 部分统一） | 无 |
| N24-3 | streamEndReader 只携带 4 字节 | P2 | ✅ | 继承（r29） | 无 |
| N24-5 | 两套完成判定无回声守卫 | P3 | 🟡 | 继承（守卫仅末 64B） | 无 |
| N25-1 | 中断谓词语义反了 | P1 | ✅ | 继承（r29 已在 isUpstreamStreamInterruption） | 无 |
| N25-3 | scanResponses 非 Responses 流全量累积 | P2 | 🟡 | 未复跑 | 未核查 |
| N25-5 | 缓冲路径零心跳 | P2 | ❌ | 探针 `flushes=0`，心跳写进 fw.buf；并入 W29-3 | 无 |
| W29-1 | **透传流全量缓冲（TTFT=总时长）** | **P0** | ❌ | **PROBE-32（本轮重跑，E1）**：`failover-writer(canDefer=true) firstWrite=802ms total=802ms writes=1`／`plain-writer firstWrite=0ms total=801ms writes=3`。成因链：`passthrough_stream.go:280-283` 对任何 `*failoverResponseWriter`+stream+<400 **无条件**走 `writeBufferedStream`；`:556` `hw := newSSEHeartbeatResponseWriter(fw)`；`provider_failover.go:347-366` 在 `passthrough=false` 时 `Write` 只落 `w.buf`；`:368-374` `Flush` 只在 passthrough 时生效；`passthrough_stream.go:560` 结束才 `fw.FlushError()` | **连续第 12 轮未修** |
| W29-2 | 透传下响应再复制进 fw.body（无上限） | P1 | ❌ | `provider_failover.go:359-362` `w.body.Write(p[:n])` 无条件；`Prefix/streamClosed/StreamLen` 零或仅测试调用 | 无（连带 W29-1 未修） |
| W29-3 | 心跳进缓冲区，流中收不到 ping | P1 | ❌ | 同 W29-1：`hw.flush→fw.Flush` 在 !passthrough 时空操作 | 无 |
| W29-4 | 红测试 TestConverterStreamWriterKeepsTheStatusItWasGiven | P1 | ✅ | 继承（r30 本机绿） | 无 |
| W29-6 | scanResponses 非 Responses 流全量累积 | P2 | 🟡 | 未复跑 | 未核查 |
| W29-7 | 回声守卫只看末 64B | P2 | 🟡 | 继承（代码） | 无 |
| W29-9 | streamUsageFolder reflect 分派脆 | P3 | ❌ | 继承（代码） | 无 |

### M3 调度/路由/并发/故障转移

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| R1 | 无 RPM 限流 | P1 | ✅ | 继承（r21 滑动窗口） | 无 |
| R2 | 冷却不看上游 Retry-After | P1 | ✅ | 继承（r26 阶梯可达） | 无 |
| R3 | billing 归入 hardQuota | P1 | ✅ | 继承（r8） | 无 |
| R5 | 会话粘性不落库 | P1 | ✅ | 继承（r24 session_pins） | 无 |
| R6 | 并发闸门非 FIFO | P1 | 🟡 | D29-3/D29-4 仍在 | 无 |
| D29-2 | Retry-After 出口按行号查分组 | P1 | 🟡 | `protocol_proxy.go:833`、`provider_failover.go:1234` 已改 `retryAfterSecondsFor`；**`server.go:4488` 仍 `retryAfterSeconds(provider.ID,…)`**（本轮重看确认） | 2/3 |
| D29-3 | 组内限额取"最后到达那行" | P1 | ❌ | `provider_lanes.go:124-139` `laneFor` 每次 acquire 覆盖 `lane.max/queueCap/rpm`（本轮重看确认） | 未修 |
| D29-4 | 组内 0/0 行整组绕过 | P1 | ❌ | `provider_lanes.go:556-561` `max<=0&&rpm<=0` 提前 return 空 release，不查组 | 未修 |
| D29-5 | 大小写：注释称不敏感，实现精确匹配 | P2 | ❌ | `providerLaneKey:542-547` 只 `TrimSpace` | 未修 |
| D29-6 | providerInFlight 按行号查 | P2 | ❌ | `:565-578` 用原始 providerID 取 `lanes[]`，组键为 `group:xxx` ⇒ 组内恒 0 | 未修 |
| D29-7 | d93ec02 夹带 failover 行为变更 | P3 | ❌ | 继承（提交粒度） | 无 |
| D29-8 | lane 只增不删 | P3 | ❌ | 继承（无回收） | 无 |
| W29-11 | failoverResponseWriter 死代码 | P3 | 🟡 | `StreamLen()/streamClosed()` 全仓零调用 | 无 |

### M4 Provider/账号/OAuth/额度

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| A1 | Trae/WorkBuddy/Cursor 刷新无锁 | P1 | ✅ | 继承（r24 oauth_renew_lock） | 无 |
| A2 | 子串匹配 host | P1 | ✅ | 继承（r24 baseURLHostIn） | 无 |
| A3 | /v1/images 路由误判 | P2 | ❌ | `server.go:3099-3105` 用 `Contains` 盲拼（对照 `:3110-3116` 用 HasSuffix）；带 query 的 BaseURL 会把路径拼进 query | 无 |
| A4 | Claude overage / 负缓存 | P1 | ✅ | 继承（r26 负缓存 5/5） | 无 |
| A5 | Cursor 配额提前量 | P2 | ✅ | 继承（r10 纠错） | 无 |
| A6 | 出图超时被 120s 砍 | P2 | ❌ | `server.go:5712`、`:5749` 均 `Timeout:0` 且不走 `upstreamHTTPClient`；所谓 10min 是 OAuth 登录轮询（`cursor_oauth.go:494`） | 无（两条都不生效） |
| N-1 | workbuddy_ai 凭证 exp 空→永不续期 | P1 | ✅ | 继承（r20 401 自愈） | 无 |
| H-7 | images 拒绝检查 route.ProviderID 而非 decision.ProviderID | P1 | ❌ | `server.go:6255` `ProviderByID(route.ProviderID)`；真正落点 `:4287-4301` 用 `decision.ProviderID`；发送侧不校验 Disabled/Protocol | **复核成立** |
| H-8 | 管理员 Key 链不过滤 | P1 | 🟡 | 未复跑 | 未核查 |
| H-9 | workbuddy 11128 校验矩阵 | P1 | ✅ | `workbuddy.go:857` 覆盖 system+assistant、`:861-891` string/parts 双分支、`:972-983` developer→system 且**先归一化后 unflag（:997）**；E3：`workbuddy_test.go:158-174/205-206` 可红 | **🟡→✅（E2+E3）** |
| D31-1 | 用户暂停的额度任务被 keeper 复活 | P1 | ❌ | `quota_keeper.go:477-481` 注释与 `:483-505` 实现相反；`:486` 只判 `len(live)==0`；调用点 `:348` 在 `if !task.Enabled` **之前**；暂停写入 `quota_keeper_api.go:180-185/273` 同一 `Enabled` 字段 ⇒ 无法区分来源。magpie 拆 `Off`（人工）与 `Rest`（冷却） | **复核成立** |
| D31-4 | OAuth 用量缓存 key 不含账号身份 | P1 | ❌ | `oauth_usage_refresh.go:284` `"claude:"+providerID`、`:327` `"chatgpt:"+providerID`…；`oauth_usage_cache.go:13-15` stale 1h；读点 `provider_failover.go:530/612/628`、`provider_load.go:155`；换号/导入无 invalidate | **复核成立** |
| D31-5 | ChatGPT 续期等锁重读后仍用旧快照 refresh_token | P1 | 🟡 | `chatgpt_oauth.go:433-436` **有**重读但只用于早退；`:437` 仍花 `provider.ChatGPTOAuth.RefreshToken`、`:442` 仍以旧快照为底；对照 `claude_oauth.go:548-568` 用 `current` | 描述纠正，缺陷仍在 |
| D31-6 | 负缓存读侧死代码 | P2 | ❌ | `oauth_usage_cache.go:64-72/116-131`，生产代码零调用 | 无 |

### M5 计费/用量/日志

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| M-19 | 错误响应被估算计费 | P1 | ✅ | 继承（r21） | 无 |
| N11-1 | 长流 usage 归零→估算放大 115× | P0 | ✅ | 继承（r29 逐字节折叠） | 无 |
| N12-2 | 日桶用服务器时区 | P2 | ⚪ | 生产 unit TZ=Asia/Shanghai（已举证） | 无 |
| N13-2 | /__usage-analysis 无上限 | P2 | ❌ | 无 limit/分页；`items` 全量驻留 + 每页 `COUNT(*)` + 深 OFFSET | 未修 |
| N23-5 | stream+JSON 回包 usage=0 | P2 | ❌ | **症状更正**：`usage.go:535-544` 在 `fold!=nil` 时永不回落 `parseUsage`；`server.go:1367-1369` 于是走 `EstimateTokenUsage`（len/4）**按真钱入账** | 症状纠正 |
| W29-5 | internal/monitoring 整包未接线 | P2 | ✅ | `app/runtime.go:194/353`、`server.go:358-375` 十八条路由；**18/18 逐条过 `requireAdmin`，无漏** | 加固复核 |
| W30-2 | 生产 monitor_* 表全空 | P3 | 新增 | 生产只读：5/6 表 0 行、`checkin_history` 18 行；二进制无 monitoring 串 ⇒ 采集未跑 | 见第五节 |
| 缓存重复计数 | prompt 已含 cached 是否再加 | P1 | ✅ | `pricing.go:139-160` 显式扣减 cache 档，`pricing_test.go:64-80` 可红 | 复核成立 |
| D31-3 | 引擎吞事件写库错误 | P2 | ❌ | `engine.go:315-324`（`:322 id,_ :=`）；`dispatcher.go:274-285` 对 `ID==0` no-op ⇒ 推送与提醒全丢 | 无 |
| D31-8 | checkin_history 无 account_key | P3 | ❌ | `store/checkin.go:57-65` PK `(provider_id,day)`；`ListCheckinHistory` 零调用；另有零调用且不在事务内的第二套写函数 `store/monitoring.go:824` | 降级 P2→P3 |

### M6 安全/鉴权/隔离

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| S1 | 管理 API `ACAO: *` | P0 | ✅ | 继承（r24 cors 白名单） | 无 |
| S2 | 请求体无上限 | P1 | ✅ | 继承（r24 request_body_limit） | 无 |
| S3 | 登录/setup 无限流 | P1 | 🟡 | 继承（r18 auth_throttle 死代码/TOCTOU） | 无 |
| S4 | 日志正文不脱敏 | P1 | ✅ | 继承（r18 log_scrub） | 无 |
| S5 | 首包后无空闲超时 | P1 | 🟡 | 守卫存在，但心跳被 W29-3 缓冲吞掉 ⇒ 长静默仍可能被 CF 掐 | 无 |
| N12-3 | 登录先解码后限流 + 全表扫 | P3 | 🟡 | 继承（r18 部分） | 无 |
| W30-3 | /__monitor 路由无 CSRF/Origin 校验 | P2 | ❌ | `monitoring_handlers.go:118-127` 全 `requireAdmin`；`cors.go:60-82` **只写响应头、不拒请求**；`admin_auth.go:326-333` 回环免鉴权 | 复核成立 |

### M7 存储/迁移

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| D29-1 | laneGroup 完全不落库 | P0 | ✅ | `store/sqlite.go:239` 列 + `:641/:766` 读写 + 回环/迁移测试绿 | 继承 r30 |
| — | **生产未部署该修复** | P1 | ❌ | 生产 `providers` 表**无 `lane_group` 列**（只读 pragma） | 新增观察（治理） |
| D31-8 | checkin_history 无 account_key | P3 | ❌ | 见 M5 | 无 |

### M8 前端控制台

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| 控制台 6 项（SSE 实时、重放、sessionId、成本列、角标、按 key 下钻） | 无实现 | P2 | ❌ | `grep -rln EventSource web/src/` = **0 命中**；`components/*.tsx` 无 sessionId UI | 连续第 6 轮零进展 |

### M9 隧道/系统面

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| N13-1 | `gateway version` 零 flag 会起服务 | P2 | ❌ | 继承（r29 复现，运维风险） | 无 |
| N13-3 | 容器无 TZ 且 verify 脚本期望 LA | P3 | ❌ | 继承 | 无 |
| D32-4 | **隧道 token 明文落 cloudflared.log** | **P1** | ❌ 新增 | `tunnel/manager.go:300-301` 把含 `--token <secret>` 的 argv 写入日志（`:653`），`openCloudflaredLogFile:343` 以 `0644` 建文件 ⇒ 同机可读，可接管公网域名 | **新（E2）** |
| W30-1 | zz_quota_keeper_e2e_hooks.go 生产源码测试钩子 | P3 | ❌ | `git ls-files | grep zz_` 命中 1；导出伪造 admin 的 `HandleRestoreProviderForTest` | 已入仓 |
| D31-2 | 监控中心不可停止 | P2 | ❌ | `app/runtime.go:353` `StartMonitoring(context.Background())`；`monitoring/service.go:38-51` 的 `Run` 有 `ctx.Done()` 但 ctx 永不取消；`Stop` 直接 `db.Close()` | **降级 P1→P2**（两个 Stop 调用点之后进程即退出，进程内重启路径不存在） |
| D31-7 | 提交卫生：.superpowers/3.9k 行文档/128 文件单提交 | P3 | ❌ | `git ls-files | grep -c "^\.superpowers/"` = 5 | 无 |

### 新发现汇总（D32-*，含 M1/M4/M5 子 agent 提出、主 agent 复核过的）

| 编号 | 问题 | 级别 | 状态 | 依据 | 复核 |
|---|---|---|---|---|---|
| D32-1 | Chat→Claude **完全丢弃 `stop`**：`chatStopToClaudeSequences` 是死代码（全包无调用点），`openAIChatToClaudeRequest` 从不读 `stop` ⇒ 客户端设 stop 无效，生成到 max_tokens | P1 | ❌ 新增 | `convert_tools.go:1165-1175`（仅定义）；调用点 `grep` 0 命中；`convert_claude_openai.go:177-218` 无 `stop` | ✅ 主 agent 复核（grep 实跑） |
| D32-2 | Claude→OpenAI 回复路径不做 declared 名回填：`resolveOpenAIToolNameFromClaude` 只试精确/ToLower/OAuth 反向映射，不调 `restoreDeclaredToolName` ⇒ 模型改大小写即 `unsupported call` | P2 | ❌ 新增 | `convert_tools.go:1272-1311` vs `:1337-1360`；现有调用点仅 `convert_openai_claude_stream.go:248`、`workbuddy.go:1250` | ✅ 主 agent 复核 |
| D32-3 | thinking 预算与 `max_tokens` 无钳制：`budget_tokens` 16000/32000 与 `claudeReq["max_tokens"]`（`:196`）从不比较；密钥级 override 小于预算即 400 | P2 | ❌ 新增 | `convert_claude_openai.go:325-331`、`:196`；magpie `anthropic.go:968` 有 `min(budget, max-1024)` | ✅ 主 agent 复核（触发条件：override 小 + reasoning_effort） |
| D32-4 | 隧道 token 明文落盘 | P1 | ❌ 新增 | 见 M9 | ✅ 主 agent 复核 |
| D32-5 | 出图请求按 base64 正文 `len/4` 估算 token 入账，且价格表无图像行 ⇒ key token 额度被打爆、$ 上限永不触发 | P2 | ❌ 新增 | `server.go:5755` 恒空 usage → `:1367-1369` → `usage.go:450-455`；`grep -i image pricing.go` 空 | 未复核（时间盒） |
| D32-6 | `safety_identifier` 全仓 0 命中（Responses 已 deprecated `user`，新字段静默丢失） | P2 | ❌ 新增 | `grep -rn safety_identifier internal/` 空 | 未复核（时间盒） |
| D32-7 | F 名：`resumeTasksForProvider` 无生产调用点，注释宣称的"恢复 Provider 即恢复任务"不存在，掩盖了"任何 disabled 任务都会被 20s tick 复活" | P3 | ❌ 新增 | `quota_keeper.go:507-528`；`grep` 仅 `quota_keeper_test.go:765/777` | 未复核（时间盒） |
| D32-8 | `unflagWorkBuddyMessages` 的 string 分支把带 `tool_calls` 的 assistant 轮整条丢弃（parts 分支有例外，string 分支没有） | P3 | ❌ 新增 | `workbuddy.go:861-871` vs `:878-887` | 未复核（时间盒） |

> ⚪ 占比：96 条中 ⚪ 仅 1 条（N12-2），1.0%，远低于 30%，不存在用"不适用"当逃生口的情况。

---

## 七 回退与假修复

**回归项：无。** 逐项比对 round31 状态：R4 ✅→✅、W29-5 ✅→✅、D29-1 ✅→✅、H-9 🟡→✅（升级，非回归）、P5 🟡→✅（升级）。没有任何一项从 ✅ 掉下来。

**假修复 / 假声明的现成例子（本轮新增取证）**

| 项 | 假在哪 | 依据 |
|---|---|---|
| D31-1 | 注释写"人工暂停永不被撤销"，代码 `len(live)>0` 就复活 | `quota_keeper.go:477-481` vs `:483-505` |
| D32-7 | 注释写"Provider 出回收站时运行"，全仓无调用点 | `quota_keeper.go:507-528` |
| H-1/H-2 | 同一语义两套实现互相否定，`convert_responses_fields.go:71-72` 的注释直接反驳 `convert_response_format.go:40-43` 的行为 | 两文件并读 |
| W29-1 | 注释（`passthrough_stream.go:275-277`）宣称"first byte goes straight through"，实际进 `writeBufferedStream` | `:280-283` + PROBE-32 |
| D31-5 | 注释（`:431-432`）宣称"Re-read after acquiring the lock"，实际重读结果只用于早退 | `:433-437` |

**关联回退嫌疑**：无（本轮无代码变化，不存在把某已修项改回去的提交）。

**上一轮误判纠正（非回归，须记账）**：A6「出图超时」在 r26 被判 ✅，本轮重看 `server.go:5712/5749` 两条出图路径都是 `http.Client{Timeout: 0}` 且不走 `upstreamHTTPClient`，r26 引用的"Cursor 10min"实为 `cursor_oauth.go:494` 的 OAuth 登录轮询 ⇒ A6 改判 ❌（P2）。这不是代码退化，是证据链纠正；同类情况请下轮继续按"宁可写未核查"处理。

---

## 八 下轮验收门槛（逐条可勾、可机械验证）

1. [ ] `git rev-parse --short HEAD` 与 `.workbuddy/reviews/round32` 记录的 `2045543` 不同，且 `git status --porcelain` 为空（先 commit 再谈别的）。
2. [ ] W29-1：新增一条**断言首字节时延**的测试（不是断言 body 内容）：`newFailoverResponseWriter` 包装下 `slowBody{delay:400ms}` 的 `firstWrite < 200ms`。验收命令 `go test ./internal/gateway/ -run 'TestZZProbeStreamingFirstByte' -v` 的输出必须出现 `writes>=2` 且 `firstWrite` 明显小于 `total`。
3. [ ] W29-1 修法验证：`writeBufferedStream` 在 `fw.canDefer` 为真时仍缓冲；为假时必须先 `fw.WriteHeader(200)`（或直接 `copyPassThroughHeaders` + `w.WriteHeader`）再逐块 `Write`——对照 `protocol_proxy.go:695` 已有的正确写法。
4. [ ] W29-2：8MB 流跑完后 `len(fw.body) <= passThroughLogBufferMax`；`go vet` 无 unused 告警（`Prefix/StreamLen/streamClosed` 要么接线要么删）。
5. [ ] W29-3：同一条测试断言 stream 期间至少有 1 次 `Flush` 到达底层 writer（`stampWriter.Flush` 计数 ≥2）。
6. [ ] H-1/H-2：`convert_response_format.go` 的 `claudeStructuredFormat` 不再写 `name`、`json_object` 改为降级宽松 `json_schema`（复用 `convert_responses_fields.go` 的实现）；`convert_response_format_test.go:29/107/19-23` 必须同步改为断言"不含 name"——否则测试会把修复判红。
7. [ ] H-3：`applyAdaptiveThinking` / 显式 thinking 分支改为 merge 已有 `output_config`（`applyAdaptiveThinking` 只写 `output_config["effort"]`，不整块替换）；验收：同一请求同时带 `response_format` + `reasoning_effort` 后 `output_config` 同时含 `format` 与 `effort`（新测试断言两个键都在）。
8. [ ] H-5：`claudeCacheableBlock` 改黑名单（排除 `thinking`/`redacted_thinking`），并新增用例：末条为 `tool_result` 时必须打上 `cache_control`。
9. [ ] D32-1：在 `openAIChatToClaudeRequest` 中接上 `chatStopToClaudeSequences`；验收：`stop=["\n\n"]` 的 Chat 请求转出的 Claude body 含 `stop_sequences`（新用例），且旧实现该用例红。
10. [ ] D32-4：`tunnel/manager.go:300-301` 写日志时把 `--token` 后的值替换为 `***`（或整条 argv 只记 `bin` + 参数个数）；验收：`grep -a "token" cloudflared.log` 无明文；同时把日志文件权限降到 `0600`。
11. [ ] D31-1：给 `scheduled_tasks` 增 `pause_reason`（`user|keeper`），`reviveScheduledTask` 只复活 `pause_reason='keeper'`；验收：控制台暂停后连续 3 个 tick（60s）任务仍 `enabled=0`（可用 `TickAt` 模拟）。
12. [ ] D31-4：`oauth_usage_refresh.go` 的 cache key 加账号维度（`AccountLabel`/`ChatGPTAccountID` 或 refresh_token 指纹），并在 `SetProviderXxxOAuth` 写入时 invalidate；验收：换号后 `cachedClaudeFiveHourUtilization` 返回新值（新用例）。
13. [ ] D29-2 收口：`server.go:4488` 改 `retryAfterSecondsFor(provider, …)`；验收：`grep -n "retryAfterSeconds(provider.ID" internal/gateway/server.go` 为空。
14. [ ] D29-3/D29-4/D29-6：组内限额改为「组内各行的严格最小非零值」、0/0 行必须查组、`providerInFlight` 用 `providerLaneKey`；每项一个用例。
15. [ ] 部署治理：发布后自检必须包含 `grep -a -o "vcs.modified=[a-z]*" bin/gateway` 得到 `false`，且 `vcs.revision` 等于发布 commit；`grep -a -c "without its closing frame"` 这类 WIP 特征串必须能对上该 commit 的存在与否。
16. [ ] W30-1：把 `zz_quota_keeper_e2e_hooks.go` 移到 `_test.go`（或用 build tag `//go:build e2e`），验收 `git ls-files | grep zz_` 为空。

---

## 附 覆盖度声明

| 模块 | 已扫文件数 / 总数 | 未核查项 |
|---|---|---|
| M0 基线与卫生 | 全仓 build/vet/test + 提交元数据 + 卫生 grep（zz_/bak/println/superpowers） | gofmt（只读约束 + CRLF 假阳性，未跑） |
| M1 协议转换·请求 | 13/18 个非测试 `convert_*.go`（全文 10、关键段 3）+ `claude_pass_through_normalize.go` 全文 + `codex_tool_context.go:340-395` | `convert_claude_openai_stream.go`、`convert_claude_responses*.go`、`convert_responses_claude_stream.go`、`convert_responses_text_format.go` 未逐行；H-6 三子项 |
| M2 流式 | `passthrough_stream.go`（260-600）、`provider_failover.go`（240-440）、PROBE-32 实跑 | `sse_heartbeat.go` 全文未读；N25-3/W29-6 未复跑 |
| M3 调度 | `provider_lanes.go` 全量关键段（110-145/542-585）、`server.go:4485-4500` | `provider_cooldown*`、`upstream_timeout.go` 未读 |
| M4 Provider/OAuth | 21 个白名单文件中实质读 10 个（整文件 3 + 关键段 7）+ 12 个非白名单依赖（追调用链） | `checkin*` 系列、`claude_oauth_cloak.go`、`cursor_oauth.go` 全文；运行时 A/B 为 0 |
| M5 计费/用量 | `usage.go`（525-560）、`pricing.go:139-160`、`server.go:1360-1375`、生产库计数 | `request_log_batcher.go`、`usage_analysis.go` 未逐行；N13-2 仅代码级 |
| M6 安全 | `cors.go:55-85`、`admin_auth.go` 相关段、`monitoring_handlers.go` 路由表 | S3 的 TOCTOU 未复跑 |
| M7 存储 | `sqlite.go`（lane_group 读写）、`monitoring_schema.go` 表名、生产 `pragma table_info` | 其余迁移未逐条走查 |
| M8 前端 | 仅 grep（EventSource/sessionId） | **整模块未做几何实测（时间盒顺延）** |
| M9 隧道 | `tunnel/manager.go`（290-312/343/645-665） | 未触发真实隧道（按约束只读）；`internal/daemon` 类对照未做 |

**magpie 对照要点**：`streamFailure`（`gateway.go:4940`）与我们的 `stream_interruption_close.go` 同语义（R4 已对齐）；`unknownOptionalField`（`:5536`，重试在 `:4564-4577`）只对**顶层白名单字段**的 unknown/unsupported 400 生效，且显式跳过 located/unsupported_value（`:5610-5615`）与嵌套路径（`:5576-5582`）——**它救不了 H-1/H-2/H-3/H-4/H-5 任何一条**，别把它当兜底；账号身份在 magpie 是数据模型实体（`internal/access/accounts.go:15-57`、`login_usage.go:164/426` 的 `Agent+"/"+User` 缓存 key），我们的"自由文本 provider 行 + 无账号维度缓存"正是 D31-4/D31-1 的根因。

---

## 收尾自检

- [x] `git status --porcelain` 为空（`.workbuddy/reviews/` 被 `.gitignore` 忽略），探针 `zz_probe32_test.go` 已删除，残留 0
- [x] 无临时 worktree 残留（`git worktree list` 仅主树 `C:/guodevelop/llm-protocol-gateway 2045543`）
- [x] 账本已更新（70 行），账本每个未闭环 ID 都在第六节出现
- [x] 每个 ✅ 都能指到 E1/E2/E3 之一（本轮新增 ✅：P5=E2+E3、H-9=E2+E3）
- [x] 覆盖度声明已填，未核查项已逐条列举（M8 整模块顺延已注明）
- [x] 无 commit / push / 生产写操作发生（生产仅 systemctl/ls/sha256sum/grep -a/journalctl/sqlite ro）

---

## 给修复 agent 的订单（按级别，最小必要改动）

**P0**

1. **W29-1 透传流全量缓冲**（`passthrough_stream.go:280-283`、`:556`、`provider_failover.go:347-374`）
   - 改哪里：`writeBufferedStream` 在 `fw.canDefer` 为假时走"直发"分支——先 `copyPassThroughHeaders(fw, response)` + `fw.WriteHeader(response.StatusCode)`（**这一步才会 commit**，参考 `protocol_proxy.go:695` 的既有写法），再 `io.Copy` 逐块写；`canDefer` 为真时才保留现有全缓冲。
   - 改成什么语义：只要已经开始向客户端交付第一个字节，本次尝试就不再可重试；未交付前保持可 defer。
   - 怎么验收：门槛 2、3（断言 `firstWrite ≪ total`、`writes≥2`）。

**P1**

2. **H-1/H-2 统一结构化输出映射**：删 `claudeStructuredFormat` 里的 `name`，`json_object` 走宽松 `json_schema`；同步改 `convert_response_format_test.go:29/107/19-23`（否则测试会把修复判红）。验收：门槛 6。
3. **H-3 合并 `output_config`**：`applyAdaptiveThinking` 只写 `effort` 不整块替换；显式 thinking 分支同样 merge。验收：门槛 7。
4. **H-5 缓存断点白名单改黑名单**：排除 `thinking`/`redacted_thinking`，让 `tool_result` 能当断点。验收：门槛 8。
5. **D32-1 接上 `stop`**：`openAIChatToClaudeRequest` 调 `chatStopToClaudeSequences` 写 `stop_sequences`。验收：门槛 9。
6. **D32-4 隧道 token 脱敏**：日志里 `--token` 值替换 `***`，文件权限 `0600`。验收：门槛 10。
7. **D31-1 暂停来源分字段**：`pause_reason`（user/keeper），keeper 只复活自己的。验收：门槛 11。
8. **D31-4 缓存 key 加账号维度 + 写入侧 invalidate**。验收：门槛 12。
9. **H-7 images 拒绝改用 `decision.ProviderID`**：`server.go:4277` 的 refusal 必须在 `imagesEffectiveProvider`（`:4287`）之后判定。验收：构造 decision≠route 的用例（现在没有）。
10. **W29-2/W29-3**：随 W29-1 一并解决（直发分支下 `fw.body` 不再累积、心跳能真正 flush）。
11. **D29-2 收口**：`server.go:4488` 改 `retryAfterSecondsFor`。验收：门槛 13。

**P2**

12. D29-3/D29-4/D29-6 组内限额取最小值 / 0-0 查组 / inFlight 用组键（门槛 14）。
13. D31-2 给 `StartMonitoring` 传可取消 ctx，`Stop` 先 cancel 再 Close（当前两个调用点后进程即退，优先级低于上面）。
14. D31-6 负缓存读侧要么接进面板、要么删掉（死代码）。
15. D32-3 thinking 预算加 `min(budget, max_tokens-1024)` 钳制。
16. N23-5 明确"fold 为 nil 时回落 parseUsage"的边界，避免 0/0 被 len/4 当真钱。

**P3**

17. W30-1 把 `zz_quota_keeper_e2e_hooks.go` 移出生产源码（build tag 或 `_test.go`）。验收：`git ls-files | grep zz_` 为空。
18. D31-7 清理 `.superpowers/` 跟踪；D31-8 给 `checkin_history` 加 `account_key` 或删掉零调用的 `SaveCheckinHistory`。
19. 部署治理：发布自检加 `vcs.modified=false`（门槛 15）。**这一条不修，前 18 条修了也无法证明线上生效。**
