# 第十一轮 · 另一 Agent 改动代码审查（round11）

- 审查时间：2026-10-23 05:02–05:20
- 审查范围：`6a1256b`（HEAD，未变）+ 未提交工作区（13 改 + 3 新增，与 round10 逐字节一致）
- 对照基准：`C:\guodevelop\demo\magpie`
- **本轮结论：连续第四轮「零新代码」**（HEAD 停在 6a1256b 已 3 小时 34 分；最后一次编辑 01:38:58，已 3 小时 24 分）
- 基线：`go build ./...` ✅ / `go vet ./internal/gateway/` ✅ / `go test ./internal/gateway/` ❌ **5 顶层用例红（7 条子用例）**，与 round10 完全一致
- VPS（173.249.207.202:18093）：`active`，`ActiveEnterTimestamp=2026-10-22 10:29:24 PDT`（=01:29:24 +0800），`/__health` 200 → 线上仍是 HEAD，WIP 未上线
- **本轮未修改任何业务代码、未提交**；探针已跑完并从仓库删除，副本存 `.workbuddy/reviews/round11-evidence/`

---

## 结论先说

1. **没有新代码可审。** 工作区指纹 `sha256(git diff HEAD)=5dfa6dfe…`，16 个文件 mtime 全部 ≤ 01:38:58，`find -newermt '2026-10-23 01:40'` 零命中。所以"改动是否正确"本轮没有新判断对象，上轮 9 高危 + 全部遗留原样。

2. **本轮换成查「已在生产的代码」——挖出两条 HEAD 就有的真 bug**（不是 WIP，线上正在跑）：

   - **N11-1（P1，已部署）**：`limitedBuffer` 只保留**头部** 256KB，而**所有 usage 帧都在流尾**。于是**任何超过 256KB 的透传流，用量统计直接归零**。
     实测：Chat 435KB 流 `prompt=1234/completion=567` → 经 tee 解析成 **0/0**；Claude 456KB 流 `in=800/out=4321` → 变成 **in=800/out=1**。
     影响面：Claude Code 读大文件、长回答这类 300KB+ 的流，Key 配额扣减、成本、日用量全部低估（Claude 甚至只丢 output，看起来"输入正常"更难发现）。
     位置：`passthrough_stream.go:138`（HEAD）/ `:221`、`:254`（WIP 两处），解析用的是 `tee.Bytes()` 而不是完整 body。

   - **N11-2（P1，已部署）**：心跳 `stop()` **只 close(done) 不 join**。goroutine 若正好在 `WriteSSEHeartbeat` 里，`defer stopHeartbeat()` 返回后它仍在写 `ResponseWriter` —— 违反 net/http「handler 返回后不得再写」的契约，属真实数据竞争。
     探针实证：阻塞式 writer 下 `stop()` 返回时写操作仍在飞行中，释放后才完成。未能稳定复现"破帧"，但这条本身就足以判不合格（一行可修：`stop()` 里 `<-exited`）。
     位置：`sse_heartbeat.go:31-63`，三个调用点（`passthrough_stream.go:119/232`、`protocol_proxy.go:645`、`server.go:5217`）全部受影响。

3. **WIP 里又挑出两条与前几轮同族的收尾帧缺陷**：
   - **N11-3（P1）**：透传 Claude 的收尾器只发 `message_delta + message_stop`，**漏发 `content_block_stop`** —— 正文块永远没关，违反 Anthropic 事件序列，SDK 侧累积的消息可能丢正文；而转换器侧的 `closeInterruptedClaudeStream` 是**先关块再 delta**，同一功能两套语义。
   - **N11-4（P2）**：透传 Responses 的收尾帧 `response.incomplete` **缺 `id`/`model`/`sequence_number`**，与转换器侧 `closeInterruptedChatToResponsesStream` 的同类帧字段差一半，严格实现（Codex）可能拒收或事件乱序。

4. **上轮 9 高危（H-1…H-9）+ M-19 + N-1 + 假测试 + R2/R4/R6 + S3/S4/S5 + A4 + 控制台 6 项，全部原样**，本轮逐条带行号复验（见第四节）。

5. **合入门槛不变**：`go test ./internal/gateway/` 全绿。当前 5 红同一根因（判定恒 true），改一处能全绿，但同时会把 N10-1 暴露到线上。

---

## 一、范围与证据

```bash
git log -1 --format='%h %ci %s'                 # 6a1256b 2026-10-23 01:28:25 +0800
git status --porcelain                          # 13 M + 3 ??
git diff HEAD | sha256sum                       # 5dfa6dfe9619b5d87f553c036f0f6b3096590d62805a872c5b5b7404e0772596
stat -c '%y %n' <13 modified>                   # 01:33:51 – 01:38:58（最新 protocol_proxy.go）
find internal cmd web/src -newermt '2026-10-23 01:40'   # 空
```

| 检查项 | 结果 |
|---|---|
| 新提交（round10 之后） | **0** |
| 工作区变更 | 与 round10 一致（16 文件，mtime 未动，指纹本轮首次落库） |
| `go build ./...` | 通过 |
| `go vet ./internal/gateway/` | 无输出 |
| `go test ./internal/gateway/` | 5 顶层 / 7 子用例红，37.9s |
| VPS | `active`，health 200，仍是 `6a1256b` |

红用例（与 round9/round10 逐字一致）：

```
--- FAIL: TestResponsesIncompleteReportsLengthInChatStream
    unexpected error: upstream stream ended after 526 bytes without its closing event
--- FAIL: TestChatLengthClosesResponsesStreamAsIncomplete
    incomplete_details.reason = "upstream_stream_interrupted", want max_output_tokens
--- FAIL: TestClaudeStopSequenceToChatFinishStream   (5 子用例全红)
--- FAIL: TestChatToClaudeStreamReportsStopSequence
--- FAIL: TestFinishConvertedProxyMidStreamDropIsRetryableWhenDeferred
    client should not see the interrupted attempt, got ..."content":"partial"...
```

---

## 二、本轮新发现（含探针实证）

### N11-1（P1，**HEAD 已存在、线上在跑**）长流 usage 归零：tee 只留头部 256KB

`limitedBuffer`（`passthrough_stream.go:71-100`）在写满 `max` 之后只累加 `total`，**不保留尾部**；而用量帧按协议都在流尾：

- Chat：`stream_options.include_usage` 的 usage 在最后一个 chunk；
- Claude：`input_tokens` 在 `message_start`（头部，幸存），`output_tokens` 在 `message_delta`（尾部，**丢**）；
- Responses：usage 在 `response.completed`（尾部，**丢**）。

而解析入口拿的正是这份被截断的副本：

```go
// HEAD passthrough_stream.go:138
usage = parseUsage(tee.Bytes())          // 只含前 256KB
// WIP :221（缓冲分支）、:254（直通分支）同错
```

**探针结果**（源码 `.workbuddy/reviews/round11-evidence/zz_round11_probe_test.go.txt`）：

```
# Chat 435,791 bytes → tee 262,144
usage from full body: prompt=1234 completion=567
usage from tee      : prompt=0    completion=0        ← 全丢
# Claude 455,999 bytes → tee 262,144
full  : in=800 out=4321
capped: in=800 out=1                                  ← 只丢 output，最隐蔽
```

后果：Key 配额、成本（CostMicros）、日用量、`/__usage-analysis` 全部低估；Claude 侧因为输入还在，看起来"只是输出少"，几乎不可能被察觉。

**修法**：解析必须用**完整 body**。直通分支要么用独立的小 ring 只保留尾部（usage 帧 < 8KB，保 32KB 足够），要么在 `tee` 之外再维护一个"尾部 8KB"缓存；WIP 的缓冲分支本来就有完整 `streamBody`，直接把 `parse(tee.Bytes())` 改成 `parse(streamBody)` 即可。

### N11-2（P1，**HEAD 已存在、线上在跑**）心跳 goroutine 不 join → handler 返回后仍在写 ResponseWriter

```go
// sse_heartbeat.go:38-44
done := make(chan struct{})
stop = func() { once.Do(func() { close(done) }) }   // 只发信号，不等待
```

若 goroutine 此刻正在 `hb.WriteSSEHeartbeat()`（持锁写 base），`stop()` 立即返回，handler 随即结束，而写操作还在飞。net/http 明确规定 `ResponseWriter` 在 `ServeHTTP` 返回后不得使用；这条路径上写的是**裸 `w`**（`sseHeartbeatResponseWriter.base`），没有任何"已结束"保护。

**探针结果**（阻塞式 writer）：`stop()` 返回时写仍在飞行中，释放后才完成 → 未 join 成立。

后果分级：属确定性契约违反 + 数据竞争；线上表现可能只是偶发丢一帧心跳（`": ping\n\n"` 被截断成坏行），也可能是 `superfluous WriteHeader` 之类的 net/http 告警。**本轮只证明了「未 join」，没有复现出客户端可见的破帧**，按 P1（而非 P0）记。

**修法**：

```go
exited := make(chan struct{})
go func() { defer close(exited); ... }()
stop = func() { once.Do(func() { close(done); <-exited }) }
```

### N11-3（P1，WIP）透传 Claude 收尾漏 `content_block_stop`

```go
// passthrough_stream.go:24-34 claudeStreamInterrupter.closeInterruptedStream
writeClaudeSSE(w, "message_delta", {...stop_reason:"max_tokens"...})
writeClaudeSSE(w, "message_stop", ...)
```

被截断时正文块（`content_block_start` 已发）**从未关闭**，直接 `message_delta → message_stop`，违反 Anthropic 事件序列（每个 block 必须先 `content_block_stop`）。转换器侧同类收尾 `closeInterruptedClaudeStream`（`stream_interruption_close.go:132-147`）是**先关块再 delta**，两条路径语义不一致。

后果：SDK 的流式累积器常以 `content_block_stop` 作为块完成信号，缺失时最终 `message.content` 可能为空或只有未完成块 —— 客户端看到"有输出但消息是空的"。修法：先按 `content_block_start` 的 index 补 `content_block_stop`，再发 delta/stop（可直接复用 `claudeInterruptedTurn`）。

### N11-4（P2，WIP）透传 Responses 收尾帧字段残缺

```go
// passthrough_stream.go:59-65
writeResponsesSSEEvent(w, "response.incomplete", map[string]any{
    "type": "response.incomplete",
    "response": map[string]any{"object":"response","status":"incomplete","incomplete_details":{...}},
})   // 缺 id / model / sequence_number / usage
```

对照转换器侧 `closeInterruptedChatToResponsesStream`（`stream_interruption_close.go:188-198`）带 `id`、`model`、`sequence_number`。Codex 侧按 `sequence_number` 排序、按 `response.id` 归并，缺字段的实现会被丢弃或报错。修法：两处统一到一个构造器。

### N11-5（P3，WIP，沿用 item g）缓冲分支「空流 / 非空截断」两个 return 一字不差

```go
// passthrough_stream.go:197-212
if len(bytes.TrimSpace(streamBody)) == 0 {
    return ..., interrupted      // 注释说"同 Provider 再试一次"
}
return ..., interrupted          // 实际两分支等价
```

注释承诺的"空流走同 Provider 重试、非空走备用账号"在代码里没有区分，是否重试完全由下游 `isUpstreamStreamInterruption` 决定。要么删掉 if，要么把差异做出来。

---

## 三、未提交「流中断」改造：既有 P0 复验（全部仍在）

| # | 问题 | 本轮证据 |
|---|---|---|
| P0-1 | 判定函数恒 true | `stream_interruption_close.go:21-23` `return r.r != nil`；5 个调用点全部在循环之前取值（`convert_claude_openai_stream.go:87`、`convert_claude_responses_stream.go:253`、`convert_openai_claude_stream.go:126`、`convert_openai_responses_chat_stream.go:576`、`:815`） |
| P0-2 | 「已缓冲可重试」前提不成立，会重复正文 | `empty_stream_retry.go:166-177` `Write` 直接 `w.base.Write(p)`；`provider_failover.go:236-262` 首个 delta 就把 `passthrough` 置 true。作者自己的 `TestFinishConvertedProxyMidStreamDropIsRetryableWhenDeferred` 报 `got "partial"` 就是证据 |
| P0-3 | 多账号链路丢心跳 | `protocol_proxy.go:645` 的 `startSSEHeartbeat` 在 `deferBackup` 的 return（`:628-640`）之后 |
| a | `runStreamConverter` 用 `ContentLength > 0` 判中断 | `protocol_proxy.go:586-592`，带 Content-Length 的完整转换流必被判截断 |
| b | 真实 RST 不写收尾帧 | `passthrough_stream.go:250-253`（`copyErr != nil` 直接返回，判据反了） |
| c | `closesUpstreamStream` 裸 `Contains` | `stream_interruption_close.go:39-46`（正文里出现 `[DONE]` 就算完） |
| d | 无条件 `stop_reason:"max_tokens"` | `:148-152`，覆盖真实 `stop_sequence`/`tool_use` |
| e | `streamRetryRecorder` 无实现 | `protocol_proxy.go:578-581`、`:623` |
| f | `countingReader.n` 只用于错误消息 | `empty_stream_retry.go:120-124` |
| h | `attemptFailoverReason` 用非 2xx 正文做子串匹配 | `provider_failover.go:1074-1080`；真中断走 `execErr` 分支，到不了这里 |
| i | `closeInterruptedResponsesToChatStream` 丢 usage | `:221` `_ = usage` |
| j | `zz_dbg2_test.go` 调试脚手架在车上 | 仍在（`TestDbgWriterType`） |

---

## 四、历史遗留逐项复核（无提交 ⇒ 不可能被修，逐条带行号确认）

| # | 问题 | 当前证据 | 状态 |
|---|---|---|---|
| H-1 | `output_config.format` 带 `name` → Anthropic 400 | `convert_response_format.go:42` `format["name"]=name`；magpie `anthropic.go:957` 只发 `{type,schema}` | ❌ |
| H-2 | `json_object` 原样发 Claude | `convert_response_format.go:19-20`、`:59-60` | ❌ |
| H-3 | adaptive 分支整体替换 `output_config`，丢 format | `convert_claude_openai.go:321-324` | ❌ |
| H-4 | `allowed_tools` 塞进 Claude `tool_choice` | `convert_responses_claude.go:497-509` | ❌ |
| H-5 | 缓存断点白名单不含 `tool_result` | `convert_claude_cache_marks.go` 全文无 `tool_result` 分支 | ❌ |
| H-6 | 思考余量补正生产不可达 | `claude_pass_through_normalize.go:151` 先把 max_tokens 改写掉 | ❌ |
| H-7 | 非 5.2/5.3 的 `glm-*` 回落 `none` | `claude_thinking_off.go:239`、`:241` | ❌ |
| H-8 | `knownClaude==false` 直接删 thinking | `claude_thinking_off.go:269-282` | ❌ |
| H-9 | 出图拒绝检查 `route.ProviderID` 而非 `decision.ProviderID` | `server.go:4206` 先 `imagesRouteRefusal(route)`，`:4216` 才取 effective provider | ❌ |
| M-19 | 响应正文不脱敏入库 | `redact*` 只覆盖凭证 | ❌ |
| N-1 | `workbuddy` 空 `expires_at` → 永不续期 | `workbuddy.go:611-620`：`expiresAt == ""` → `return false` | ❌ |
| 假测试 | `TestClaudeToolChoiceWireShape` 只 marshal 自己的 map | `convert_tool_restrictions_test.go:126-140` | ❌ |
| M-1 | Claude→Chat 无条件 stop | 已被 stop-sequence 条件守住 | ✅（round10 纠正，不再报） |
| A5 | Cursor 过期提前量 | `cursorTokenExpiry` 内建 5 分钟 | ✅（round10 纠正，不再报） |

---

## 五、完整排查清单（本轮复验后的总账）

### 5.1 协议转换

| ID | 项目 | 状态 | 备注 / 证据 |
|---|---|---|---|
| P1 | Claude→Chat 流式 usage 合并 | ✅ | `message_start` + `message_delta` 合并 |
| P2 | `stop_reason` 全量映射 | ✅ | `convert_claude_openai.go:427-438`，default→stop |
| P3 | Chat→Responses 保留工具前正文 | ✅ | `a55c35d` |
| P4 | Responses→Chat reasoning 增量 | ✅ | — |
| P5 | `incomplete`/`max_output_tokens` | 🟡 | `1ad2f50` 修好，**被 WIP 覆盖**（测试报 `upstream_stream_interrupted`） |
| P6 | Chat→Claude block index / 关闭顺序 | ✅ | — |
| P7 | 工具 ID 哈希后缀 | ✅ | — |
| P8 | Responses→Claude 合成 `tool_result` | ✅ | — |
| P9 | `redacted_thinking` / 反向 reasoning | 🟡 | 反向仍丢全部 reasoning |
| P10 | `strict`/`top_k`/`metadata`/`prompt_cache_key` | 🟡 | Claude 工具构造器仍无 |
| P11 | 单一 IR 架构（对齐 magpie `ir.go`） | ❌ | 架构级 |
| **N11-1** | **长流 usage 归零（tee 只留头 256KB）** | 🔴 **新·已部署** | `passthrough_stream.go:138`；探针实证 |
| **N11-2** | **心跳 goroutine 不 join** | 🔴 **新·已部署** | `sse_heartbeat.go:38-44`；探针实证 |
| **N11-3** | **透传 Claude 收尾漏 content_block_stop** | 🔴 **新** | `passthrough_stream.go:24-34` |
| N10-1 | >256KB 完整流判"中断" + 假截断帧 | 🔴 | `passthrough_stream.go:242` |
| N10-2 | 收尾帧绕过心跳锁 | 🔴 | `:246` 写裸 `w` |
| P0-1/2/3 | WIP 三条 P0 | 🔴 | 见第三节 |

### 5.2 路由 / 调度

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| R1 | RPM 限流 | 🟡 | 仅 Provider lane，无按 Key/用户 |
| R2 | 冷却跟随 Retry-After / resets_at | 🟡 | `provider_failover.go:489-493` 仍取 `max` → 30s 被拉成 60min；`:1206` 复探传空 hint 把 1h 缩回 2m |
| R3 | billing 不算硬额度 | ✅ | `6a1256b` |
| R4 | 截断续传 | 🔴 | WIP 会重复正文（P0-2） |
| R5 | session pin 落库 | ✅ | `30519bb`，两个小缺口 |
| R6 | per-account 闸门 + FIFO | 🟡 | `provider_lanes.go:155-157` 纯 RPM lane `max<=0` 直接放行；`close(broadcast)` 非 FIFO |

### 5.3 横切安全

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| S1 | 管理 API CORS | ✅ | 已白名单；`trae.go:383` 页面级 `*` 仍在（非管理 API） |
| S2 | 请求体大小上限 | ✅ | `request_body_limit.go:48` |
| S3 | 登录/注册/自检/pac/setup 限流 | 🟡 | 仅登录有 `authThrottle`；`admin_auth.go:448` setup loopback 检查有 TOCTOU |
| S4 | 请求/响应脱敏 | 🟡 | 请求侧已做，响应侧未做（M-19） |
| S5 | 首包后空闲超时 | ❌ | `Timeout: 0` 仍有 **10 处**（`protocol_proxy.go:144,448`、`request_timing.go:291`、`server.go:5166,5284,5334,5414,5440,5563,5652`） |

### 5.4 Provider 账号

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| A1 | 刷新令牌加锁 | ✅ | `16c00d3`；锁表 3 瑕疵未收 |
| A2 | host 白名单 | ✅ | `zhipu_usage.go:30` |
| A3 | images 路由 / 全局 Key 回落 | 🟡 | 路由已收；H-9 判定错位仍在 |
| A4 | Claude 额度窗口 + 429 负缓存 | ❌ | `claude_oauth_usage.go:88-92` 无 `seven_day_overage_included` |
| A5 | Cursor 过期提前量 | ✅ | 内建 5 分钟（round10 纠正） |

### 5.5 控制台（能力缺口）

| 项目 | 状态 | 证据 |
|---|---|---|
| 实时请求流 | ❌ | `web/src` 只有轮询 |
| 路由解释聚合视图 | ❌ | `analysis.attempts` 已存未 surface |
| 会话视图 | ❌ | `sessionId` 只声明未展示 |
| 一键重放 | ❌ | `grep -ri replay web/src` 零命中 |
| 日志成本列 | ❌ | `costMicros` 只在密钥页 |
| 告警未读角标 | ❌ | — |

---

## 六、验收门槛（合入前必须全过）

1. `go test ./internal/gateway/` 全绿（当前 5 顶层 / 7 子用例红）。
2. **判定改成"见到收尾帧"**：删 `isUpstreamStreamInterruptedReader`，各转换器用自己已有的 `sawAnyEvent / messageStarted / [DONE]` 布尔量。
3. **N11-1**：usage 解析不得用头部截断的 `tee.Bytes()`；WIP 缓冲分支直接 `parse(streamBody)`，直通分支加尾部 ring。
4. **N11-2**：`stop()` 必须等 goroutine 退出（`<-exited`）。
5. **N11-3**：透传 Claude 收尾先补 `content_block_stop`。
6. **N11-4**：两个 Responses 收尾帧合并到同一构造器，带 `id/model/sequence_number`。
7. N10-1：`tee` 不得参与完成性判定。
8. N10-2：收尾帧写给 `hw` 而不是 `w`。
9. P0-2：转换输出先入 `bytes.Buffer`、判定后一次性下发，或只保留"一个字都没写过"时的重试。
10. 心跳必须在任何可能阻塞客户端的分支之前启动（P0-3）；流中断不得计入 Provider 软失败。
11. 删 `zz_dbg2_test.go`、`streamRetryRecorder`、`countingReader.n`、`runStreamConverter` 的 ContentLength 推断、缓冲分支死 if。
12. 修 `TestClaudeToolChoiceWireShape`（当前恒真，等于没测）。

## 七、建议顺序

1. **先修 N11-1**（纯统计 bug，风险最低、收益最高，且线上正在错）。
2. **再修 N11-2**（一行等待）。
3. 修判定（门槛 2）→ 5 个红用例转绿。
4. 修 N10-1 / N10-2 / N11-3 / N11-4（收尾帧族一起做，抽公共构造器）。
5. 统一缓冲/重试语义（P0-2 + N9-1）+ 补回心跳（P0-3）+ 流中断不计软失败。
6. 回到老账：H-1…H-9（小改动高收益）、A4、控制台 6 项。

## 八、复现

```bash
git log -1 --format='%h %ci %s'                  # 6a1256b 01:28:25
git diff HEAD | sha256sum                        # 5dfa6dfe…
go build ./... && go vet ./internal/gateway/
go test ./internal/gateway/                      # 5 顶层红
# 探针（复制到 internal/gateway/ 后跑，跑完删除）
cp .workbuddy/reviews/round11-evidence/zz_round11_probe_test.go.txt \
   internal/gateway/zz_round11_probe_test.go
go test ./internal/gateway/ -run TestProbeRound11 -v
ssh root@173.249.207.202 'systemctl show llm-protocol-gateway -p ActiveEnterTimestamp --value'
```
