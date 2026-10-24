# 第九轮 · 另一 Agent 改动代码审查（round9）

- 审查时间：2026-10-23 02:52–03:0x
- 审查范围：`6a1256b`（HEAD，自 round8 起**零新提交**）+ 未提交工作区（13 改 + 3 新增）
- 对照基准：`C:\guodevelop\demo\magpie`
- 快照比对：`git diff 1cf920c`（round8 快照，01:46:16）→ **空**。工作区与 round8 审查对象逐字节一致。
- 文件 mtime：全部 16 个文件最后修改于 01:35–01:38，之后 75 分钟无人动过。
- 基线：
  - `go build ./...` ✅ 通过；`go vet ./internal/gateway/` ✅ 无输出
  - `go test ./internal/gateway/` ❌ **5 个顶层用例红**（含 5 个子用例，共 9 条断言）
- VPS（173.249.207.202）：`llm-protocol-gateway.service` active，`ActiveEnterTimestamp=10:29:24 PDT`（=01:29:24 +0800，紧跟 `6a1256b` 构建）→ 线上跑的就是 HEAD，**未提交的"流中断"改造没有上线**
- **本轮未修改任何业务代码，未提交**

---

## 结论先说

1. **本轮没有新代码可审。** HEAD 仍是 `6a1256b`，工作区与 round8 快照 `1cf920c` 完全一致（`git diff` 为空），对方 agent 在 01:38 之后停手 75 分钟。
2. **round8 判定的"不可合并"结论全部仍然成立**，且**验收门槛依旧没过**：`go test ./internal/gateway/` 5 个顶层用例红，核心原因是 `isUpstreamStreamInterruptedReader` 恒返回 `true`（`stream_interruption_close.go:21-23`，5 处调用）。
3. **本轮唯一的新信息是我把透传侧改动读透了，发现一个 round8 漏掉的 P0 级性能/可用性回归**（N9-1）：只要 Key 配了 ≥2 个账号，**所有 passthrough 流式响应会被整段读进内存后才发给客户端**——首字延迟变成"整段回答的时长"、期间没有心跳（Cloudflare 100s 空闲切断）、内存无上限。这条比 round8 的 P0-2 影响面更大，且**与"流中断可重试"的初衷完全无关，是纯粹的功能退化**。
4. 另外把"流中断算不算 Provider 故障"这条链走通了：`classifyProviderFailover` 对 `transportErr != nil` 一律 `failoverSoft`（`provider_failover.go:118-123`）。叠加 N9-2（所有流都被判成中断）后，**单个 Provider 只要被请求 3 次转换流就会被冷却 1 分钟**——这是一条从"判定 stub"直通"账号池雪崩"的路径。新加的 `attemptReasonStreamInterrupted` 恰好到不了这条链（见 N9-7）。
5. 上轮 9 个高危（H-1…H-9）+ M-1/M-19/N-1 + 假测试**一项未动**；调度 R1/R4/R6、安全 S3/S4/S5、A4/A5、控制台 6 项**全部原样**。

---

## 一、本轮范围与证据

```bash
git log -1 --format='%h %ci %s'        # 6a1256b 2026-10-23 01:28:25 +0800
git diff 1cf920c --stat                # 空
git status --porcelain                 # 13 M + 3 ??
find . -newermt '2026-10-23 01:30' ...  # 与 round8 同一批 16 个文件
```

`ssh root@173.249.207.202`：`systemctl show` → `ActiveEnterTimestamp=Thu 2026-10-22 10:29:24 PDT`；`curl /__health` → 200。部署内容 = `6a1256b`，与本轮审查的差异区无关。

**含义**：本轮不存在"改动是否正确"的新判断对象；能做且唯一有价值的，是把全部历史问题在当前代码状态下**逐项复验并汇总成一份可勾选的清单**（下第五节）。

---

## 二、未提交改造：本轮新增/加深的发现

### N9-1（P0，新）passthrough 流被整段缓冲，首字延迟 = 整段回答时长

`passthrough_stream.go:201-215`：

```go
if canDefer && !passthrough {
    streamBody, readErr := io.ReadAll(io.TeeReader(response.Body, tee))   // ← 整段读完
    ...
    hw := newSSEHeartbeatResponseWriter(w)
    hw.WriteHeader(response.StatusCode)
    if _, writeErr := hw.Write(streamBody); writeErr != nil { ... }      // ← 才写给客户端
```

`canDefer := i < len(attempts)-1`（`provider_failover.go:948`）。也就是说**只要这条 Key 的账号链长度 ≥ 2，第 0…n-2 号账号的每一次 passthrough 流式请求都在"读完全部上游字节"之后才开始回客户端**。三个后果：

1. **首字延迟（TTFT）变成整段回答时长**。一次 60 秒的长回答，客户端前 60 秒收不到一个字节。这是对 `/v1/chat/completions`、Claude `/v1/messages`、Trae/Qoder 所有直连路径的全局退化——多账号是常态配置，等于默认开启。
2. **期间没有心跳**。`startSSEHeartbeat` 只在流式分支里（`passthrough_stream.go:227`），缓冲分支根本没起。Cloudflare 空闲上限 100s → **超过 100 秒的回答必然被切断**，而切断后 `readErr`/`closesUpstreamStream` 又会把它判成中断再重试一遍（重试同样超时）→ 死循环式失败。
3. **内存无上限**。`tee` 有 256KB 上限，但 `streamBody` 是 `io.ReadAll` 的完整结果，长代码/长文档回答会整段驻留内存；并发几十路即可打爆。

对比 magpie：`fallback.go` 的 hold 只缓存"到首个可见增量"为止，从不持有的整段 body。

**修法（二选一）**：
- 回到流式透传 + 只记"是否见到收尾帧"（见 N9-2 的正确解法），**放弃"passthrough 分片后重试"**；
- 或者只缓冲到"判定点"（首个可见 delta）就 flush 并转入直通，中断只补收尾帧、不重试。

### N9-2（P0，沿用 round8 P0-1，未动）判定函数恒 true

```go
// stream_interruption_close.go:21-23
func isUpstreamStreamInterruptedReader(r countingReader) bool {
	return r.r != nil          // 恒 true
}
```

5 处调用（`convert_claude_openai_stream.go:87`、`convert_claude_responses_stream.go:253`、`convert_openai_claude_stream.go:126`、`convert_openai_responses_chat_stream.go:576`、`:815`）都是**循环之前**取值，于是每条正常结束的转换流都会走 `closeInterrupted*Stream` 并返回错误。

叠加 N9-4 的后果链（本轮打通）：`finishConvertedProxy` 返回非 nil err → `executeProtocolFlow` 返回 err → `classifyProviderFailover` 的 `transportErr != nil` 分支 → `failoverSoft` → 3 次即冷却 1 分钟。**即：任何一次正常的转换流式请求都会被计为一次 Provider 软失败。**

红用例（当前实测）：
```
--- FAIL: TestResponsesIncompleteReportsLengthInChatStream
--- FAIL: TestChatLengthClosesResponsesStreamAsIncomplete
--- FAIL: TestClaudeStopSequenceToChatFinishStream（5 子用例）
--- FAIL: TestChatToClaudeStreamReportsStopSequence
--- FAIL: TestFinishConvertedProxyMidStreamDropIsRetryableWhenDeferred
```

### N9-3（P0，沿用 round8 P0-2，未动）转换流仍"先写后重试"

```go
// protocol_proxy.go:619-641
deferBackup := fw.canDefer && !fw.passthrough
if deferBackup {
    usage, streamErr := runStreamConverter(dw, response, model, streamConvert)
    ...
    return response.StatusCode, usage, nil, streamErr, true   // retryable，但不看 dw.Committed()
}
```

`deferredSSEWriter` 不缓冲（`empty_stream_retry.go:158-172`：`Write` 直接 `w.base.Write(p)`），`failoverResponseWriter.WriteHeader(200)` 因 `statusCode < 400` 立刻 `commitHeaders` 并置 `passthrough`（`provider_failover.go:229-247`）。所以第一个 delta 就已经到客户端，`retryable=true` 只是让上层 `Discard()` 拦住**后面的**写。

配合 `finishConvertedProxyWithEmptyStreamRetry`（`protocol_proxy.go:531-570`）的 3 次同 Provider 重试 → **同一段回答被重复 3 次**，再叠加 failover 换账号 → 最多 4 次。对方自己写的 `TestFinishConvertedProxyMidStreamDropIsRetryableWhenDeferred` 就是这条失败证据。

补一条本轮新增的判断：**透传侧已经改成"先缓冲再决定"（N9-1），转换侧却没改**——同一个改造内两套语义，说明作者对"是否能回滚已写字节"这个前提没有统一结论。

### N9-4（P0，沿用 round8 P0-3，未动）多账号链路丢心跳

`startSSEHeartbeat` 在 `protocol_proxy.go:645`，而 `deferBackup` 分支（`:626-641`）两条路径都 `return`。于是"配了备用账号"的转换流完全没有心跳，长思考静默会被 Cloudflare 切断——正好回退 `fix(sse): 长思考静默时发送心跳`。

### N9-5（P1，沿用）真实中断路径（RST）仍不写收尾帧

`passthrough_stream.go:251-253`：`copyErr != nil` 裸返回，不调 `end[0].closeInterruptedStream`。而连接被 reset / `io.Copy` 报错恰恰是最常见的中断形态。收尾帧只在 `copyErr == nil && !closesUpstreamStream(...)` 这条"安静 EOF"窄路径上写。

### N9-6（P1，新）未提交改动覆盖了已修好的 P5

`TestChatLengthClosesResponsesStreamAsIncomplete` 现在报：
```
incomplete_details.reason = "upstream_stream_interrupted", want max_output_tokens
```
`1ad2f50`（截断的 Responses 流以 `response.incomplete` 收尾）的语义被 N9-2 的兜底帧抢走了 —— 真实原因（max_output_tokens）被统一改写成"上游中断"。同类问题在 Claude 侧：`closeInterruptedClaudeStream` 固定 `stop_reason:"max_tokens"`，而原本的 `stop_sequence`/`tool_use` 等真实原因被覆盖。

### N9-7（P1，新）`attemptReasonStreamInterrupted` 实际不可达，且反向误判

```go
// provider_failover.go:1028
noteScheduleAttempt(..., Reason: attemptFailoverReason(respBody), ...)
// provider_failover.go:1074-1080
func attemptFailoverReason(body []byte) string {
	if isUpstreamStreamInterruption(errors.New(string(body))) { return attemptReasonStreamInterrupted }
	return attemptReasonUpstreamError
}
```

- 真正的中断走 `execErr` 分支（`:953`），到不了这条 HTTP 状态分支，因此线上**永远看不到 `stream_interrupted`**，路由解释视图里的原因列对中断仍显示 `transport_error`。
- 反过来，这里把上游**非 2xx 的正文**当 error 字符串做子串匹配：一个 500 响应正文里含 "connection reset" 就会被记成 `stream_interrupted`。这是 round8 P2-d 的同款问题，现在被搬到了新常量上。

### N9-8（P2，新）缓冲分支里两个 `return` 完全同值

```go
// passthrough_stream.go:206-214
if len(bytes.TrimSpace(streamBody)) == 0 {
    // Empty stream: ... worth one more attempt on the same provider
    return response.StatusCode, TokenUsage{}, tee.Bytes(), interrupted
}
return response.StatusCode, TokenUsage{}, tee.Bytes(), interrupted   // 一字不差
```
"空流"与"非空但被截断"的注释承诺了不同处理，实现完全相同（都是 retryable）。按上层语义，两者确实都该重试，但**这段 if 是纯死代码**，读代码的人会被注释误导。

### N9-9（P2，沿用 + 新证据）

| # | 问题 | 位置 | 状态 |
|---|---|---|---|
| a | `streamRetryRecorder` 接口全仓无实现 → 分支死代码 | `protocol_proxy.go:579-581`、`:623` | 仍在 |
| b | `runStreamConverter` 用 `ContentLength > 0 && err == nil` 判中断 → 带长度声明的完整 SSE 被误判 | `protocol_proxy.go:588-590` | 仍在 |
| c | `closeInterruptedClaudeStream` 同时从 `toolIndexes`/`toolOpen` 收集 → 可能重复写 `content_block_stop` | `stream_interruption_close.go:128-131` | 仍在（定时炸弹） |
| d | `closeInterruptedResponsesToChatStream` 丢 usage（`_ = usage`） | `stream_interruption_close.go:221` | 仍在 |
| e | `claudeStreamInterrupter` 可能在**没有 `message_start`** 的流上直接写 `message_delta` → Claude 客户端报事件序列错 | `passthrough_stream.go:26-33` | 新指出 |
| f | `openAIChatStreamInterrupter` 用新生成的 `chatcmpl-<nanos>` id，与流内 id 不一致；`responsesStreamInterrupter` 缺 `response.id`/`sequence_number` | `passthrough_stream.go:42-65` | 新指出 |
| g | `zz_dbg2_test.go`（`TestDbgWriterType` 调试脚手架）仍在 | 新增文件 | 仍在，提交前必须删 |
| h | `closeInterruptedChatStream` 留了未使用的 `_ bool` 形参 | `stream_interruption_close.go:69` | 仍在 |

---

## 三、round8 提出的问题逐项复核

| # | round8 结论 | 本轮状态 | 证据 |
|---|---|---|---|
| P0-1 判定恒 true | 不可合并 | ❌ 未改 | `stream_interruption_close.go:21-23` |
| P0-2 缓冲前提不成立/重复正文 | 不可合并 | ❌ 未改 | `protocol_proxy.go:619-641`；对方测试仍红 |
| P0-3 丢心跳 | 回归 | ❌ 未改 | `protocol_proxy.go:645` |
| P1-1 ContentLength 推断 | 应删 | ❌ 未改 | `protocol_proxy.go:588` |
| P1-2 RST 不写收尾帧 | 反了 | ❌ 未改 | `passthrough_stream.go:251` |
| P1-3 `closesUpstreamStream` 裸 Contains | 弱于 magpie | ❌ 未改 | `stream_interruption_close.go:30-48` |
| P1-4 语义混用 | 需注释 | ❌ 未改 | 同上 |
| P2 a–g | 见清单 | 全部仍在 | 见 N9-9 |
| 5a6f2d1 冷却优先级反了 | 实测 | ❌ 未改 | `provider_failover.go:489-505`（`want = max(retryAfter, resetsAt)`） |
| 5a6f2d1 复探缩回 2m | 实测 | ❌ 未改 | `:1206` 传空 hint |
| 581ea6f 纯 RPM 排队者不唤醒 | 实测 | ❌ 未改 | `provider_lanes.go:161-171`（`max<=0` 直接返回空 release） |
| 6a1256b 误判面 | 观察 | 未动 | — |
| 30519bb pin 落库 | ✅ 正确 | 无回归 | — |
| dc6683b purge 回填 | ✅ 正确 | 无回归 | — |

---

## 四、历史遗留（H / M / N 系列）逐项复核 —— 全部未动

| # | 问题 | 位置（当前） | 状态 |
|---|---|---|---|
| H-1 | `output_config.format` 带 `name` → Claude 400 | `convert_response_format.go:41-43` | ❌ |
| H-2 | `json_object` 原样发 Claude | `convert_response_format.go:19-20` | ❌ |
| H-3 | adaptive 分支整体替换 `output_config`（丢掉 format） | `convert_claude_openai.go:323` | ❌ |
| H-4 | `allowed_tools` 塞进 Claude `tool_choice` | `convert_responses_claude.go:497-509` | ❌ |
| H-5 | 缓存断点白名单不含 `tool_result` | `convert_claude_cache_marks.go:92-105`（只认 text/image/document） | ❌ |
| H-6 | 思考余量补正生产不可达 | `server.go` `rewriteClaudeUpstreamMaxTokens` 先行 | ❌ |
| H-7 | 非 5.2/5.3 的 `glm-*` 回落 `none` | `claude_thinking_off.go:237-238` | ❌ |
| H-8 | `known==false` 被当非 Claude 删 thinking | `claude_thinking_off.go:270-276` | ❌ |
| H-9 | 出图提前拒绝查 `route.ProviderID` 而非 `decision.ProviderID` | `server.go:4204` vs `:4216` | ❌ |
| M-1 | Claude→Chat 无条件 `stopReason="stop"` | `convert_claude_openai.go:484` | ❌ |
| M-19 | 响应正文不脱敏入库 | `server.go:3133/3165/3244/3319` | ❌ |
| N-1 | `workbuddy_ai` 空 `oauth_expires_at` → 永不续期、无 401 兜底 | 续期策略 | ❌ |
| 假测试 | `TestClaudeToolChoiceWireShape` 恒真断言 | `convert_tool_restrictions_test.go:127` | ❌ |

---

## 五、完整排查清单（本轮复验后的总账）

### 5.1 协议转换

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| P1 | Claude→Chat 流式 usage 合并 | ✅ 已修 | round8 复核无回归 |
| P2 | `stop_reason` 全量映射 | ✅ 已修 | — |
| P3 | Chat→Responses 保留工具调用前正文 | ✅ 已修（`a55c35d`） | — |
| P4 | Responses→Chat reasoning 增量 | ✅ 已修 | — |
| P5 | `incomplete`/`max_output_tokens` 透出 | 🟡 已修（`1ad2f50`）但**被未提交改动覆盖**（N9-6） | 合入前必须回归 |
| P6 | Chat→Claude block index / 关闭顺序 | ✅ 已修 | — |
| P7 | 工具 ID 哈希后缀（防碰撞） | ✅ 已修 | — |
| P8 | 合成 `tool_result` | ✅ 已修 | — |
| P9 | `redacted_thinking` / 反向 reasoning | 🟡 部分 | 反向仍丢 |
| P10 | `strict`/`top_k`/`metadata`/`prompt_cache_key` 转发 | 🟡 response_format 侧已有 `strict` 搬运；Claude 工具构造器无 | 低危 |
| P11 | 单一 IR 架构（对齐 magpie） | ❌ | 架构级，暂不动 |
| N9-1 | passthrough 整段缓冲 | 🔴 未提交新引入 | 见第二节 |

### 5.2 路由 / 调度

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| R1 | RPM 限流 | 🟡 Provider 级有（`provider_lanes.rpm`），**无按 Key/按用户** | — |
| R2 | 冷却跟随上游 Retry-After / resets_at | 🟡 已实现但优先级反了 + 复探缩回 | `5a6f2d1` 三缺陷未修 |
| R3 | billing 不算硬额度 | ✅ 已修 | `6a1256b` 误判面待观察 |
| R4 | 截断续传 | 🔴 未提交，当前实现重复正文 | — |
| R5 | session pin 落库 | ✅ 已修（`30519bb`） | 两个小缺口未收 |
| R6 | per-account 闸门 + FIFO | 🟡 per-account 成立；唤醒是 `close(broadcast)` 惊群、非 FIFO；纯 RPM lane 排队者等满超时 | `581ea6f` 缺陷未修 |

### 5.3 横切安全

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| S1 | 管理 API CORS | ✅ 已修（`452edc0`） | — |
| S2 | 请求体大小上限 | ✅ 已修 | 全局 `MaxBytesReader` |
| S3 | 登录/注册/自检/pac token 限流 + setup | 🟡 仅登录有 `authThrottle` | `auth_throttle.go:103` |
| S4 | 请求/响应脱敏 | 🟡 请求侧已做，**响应侧未做**（M-19） | — |
| S5 | 首包后空闲超时 | ❌ `Timeout: 0` 仍在（10 处） | `protocol_proxy.go:144,448`、`server.go:5166…` |

### 5.4 Provider 账号

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| A1 | 刷新令牌加锁 | ✅ 已修（`16c00d3`） | 锁表 3 个瑕疵未收尾 |
| A2 | host 白名单 | ✅ 已修（`8972366`） | — |
| A3 | images 路由 / 全局 Key 回落 | 🟡 路由已收，H-9 判定错位仍在 | — |
| A4 | Claude 额度窗口 + 429 负缓存 | ❌ 无 `seven_day_overage_included`（`claude_oauth_usage.go:90-92`） | — |
| A5 | Cursor 过期提前量 | ❌ 无 leeway | `cursor_oauth.go:236` |

### 5.5 控制台

| 项目 | 状态 | 证据 |
|---|---|---|
| 实时请求流 | ❌ | `web/src` 只有轮询（`main.tsx:1007`） |
| 路由解释聚合视图 | ❌ | `sessionId` 只在 `types.ts:675` 声明，无聚合 |
| 会话视图 | ❌ | 同上 |
| 一键重放 | ❌ | `grep -i replay web/src` 零命中 |
| 日志成本列 | ❌ | `costMicros` 只在 apikeys 页面 |
| 告警未读角标 | ❌ | — |

---

## 六、验收门槛（合入前必须全过）

1. `go test ./internal/gateway/` 全绿（当前 5 红）。
2. 删 `zz_dbg2_test.go`、`streamRetryRecorder` 死接口、`runStreamConverter` 的 ContentLength 推断、N9-8 死分支。
3. **N9-1 必须给出明确结论**：要么放弃 passthrough 重试，要么证明"整段缓冲"不会让 TTFT/心跳/内存退化（当前显然会）。
4. **N9-2 的修法**：删掉 `isUpstreamStreamInterruptedReader`，在转换器里用"是否见到收尾帧"的布尔量（各转换器本来就有 `sawAnyEvent`/`message_stop` 分支）。
5. **N9-3 的修法**：转换输出先进 `bytes.Buffer`，判定后才一次性写给客户端；或只保留"一个字都没写过"时的重试。
6. 心跳必须在任何可能阻塞客户端之前启动。
7. 流中断不得计入 Provider 软失败（`classifyProviderFailover` 需要一条 `failoverNone` 分支，或至少不要 `bumpSoftFailure`）。

## 七、建议顺序（不变，且更急）

1. 先让判定函数正确（N9-2）→ 5 个红用例自然变绿。
2. 决定 N9-1 与 N9-3 的统一方案（两处必须同语义）。
3. 补回心跳（N9-4）。
4. 再把流中断从"Provider 故障"里摘出来。
5. 之后回到老账：H-1…H-9（都是小改动高收益）、A4、控制台 6 项。

## 八、复现

```bash
git diff 1cf920c --stat                  # 空：与 round8 快照一致
go build ./... && go vet ./internal/gateway/
go test ./internal/gateway/              # 5 个顶层用例红
go test ./internal/gateway/ -run 'TestChatLengthClosesResponsesStreamAsIncomplete|TestFinishConvertedProxyMidStreamDropIsRetryableWhenDeferred' -v
ssh root@173.249.207.202 'systemctl show llm-protocol-gateway -p ActiveEnterTimestamp'
```
