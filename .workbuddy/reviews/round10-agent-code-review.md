# 第十轮 · 另一 Agent 改动代码审查（round10）

- 审查时间：2026-10-23 03:57–04:10
- 审查范围：`6a1256b`（HEAD）+ 未提交工作区（13 改 + 3 新增）
- 对照基准：`C:\guodevelop\demo\magpie`
- **本轮结论：HEAD 未动、工作区未动 —— 这是连续第三轮"零新代码"**（round8 起 5 小时 29 分无提交、2 小时 27 分无编辑）
- 基线：`go build ./...` ✅ / `go vet ./internal/gateway/` ✅ / `go test ./internal/gateway/` ❌ **5 个顶层用例红（7 条子用例）**
- VPS（173.249.207.202:18093）：`active`，`ActiveEnterTimestamp=2026-10-22 10:29:24 PDT`（=01:29:24 +0800，紧跟 `6a1256b`），`/__health` 200 → 线上仍是 HEAD，未提交改造没有上线
- **本轮未修改任何业务代码，未提交**（临时探针已从仓库移除，副本存 `.workbuddy/reviews/round10-evidence/`）

---

## 结论先说

1. **又被放了一次鸽子。** HEAD `6a1256b`、工作区 16 个文件，mtime 全部停在 **01:33–01:38**；全仓 `-newermt '2026-10-23 01:40'` 零命中。round9（02:52）之后没有任何新的编辑或提交，因此"改动是否正确"这个问题本轮**没有新判断对象**。
2. **本轮仍有两条新发现**（都是前两轮漏掉的，且都能实测）：
   - **N10-1（P0）**：透传流"收尾帧靠 `tee` 判断"，而 `tee` 只保留**前 256KB**。于是**任何超过 256KB 的完整 SSE 回答都会被判成"上游中断"** —— 客户端收到一个**假的截断帧**（Chat 多一个 `finish_reason=length` + 第二个 `[DONE]`），同时这次请求被计入 Provider 软失败。已用探针坐实（716KB 完整流 → `upstream stream interrupted`，客户端 `[DONE] x2`）。
   - **N10-2（P1）**：中断收尾帧直接写给**裸 `w`**，绕过了 `sseHeartbeatResponseWriter` 的互斥锁，与后台心跳 goroutine 并发写同一个 `ResponseWriter`（15s 一次心跳，长流必撞）。SSE 帧可能被撕开。
3. **两条历史结论需要纠正**（我上几轮记错了，本轮逐行复查）：
   - **A5（Cursor 过期提前量）实际已达标**，不算未修 —— `cursorTokenExpiry()` 在解码 JWT 时已经 `.Add(-cursorTokenExpirySkew)`（5 分钟），`ExpiresAt` 落库的就是提前 5 分钟的时刻，`cursorTokenNeedsRefresh` 的 `now > ExpiresAt` 天然带 leeway。
   - **M-1（Claude→Chat 无条件 stop）实际已修** —— 现在只有"客户端自己的停止序列命中正文"时才覆写为 `stop`（`convert_claude_openai.go:482-485`），其余走映射表。
4. **上轮 9 个高危（H-1…H-9）+ M-19 + N-1 + 假测试 + R1/R4/R6 + S3/S4/S5 + A4 + 控制台 6 项，全部原样未动**（无提交 ⇒ 不可能被修，且逐项带行号复核）。
5. **合入门槛依旧是那一条**：`go test ./internal/gateway/` 全绿。当前 5 红全由「判定恒 true」单一根因导致，改一处即可全绿 —— 但改了它之后 N10-1 会暴露得更明显。

---

## 一、范围与证据

```bash
git log -1 --format='%h %ci %s'      # 6a1256b 2026-10-23 01:28:25 +0800
git status --porcelain               # 13 M + 3 ??
ls -l --time-style=+%F_%T <16 files> # 全部 01:33–01:38
find internal cmd web/src -newermt '2026-10-23 01:40' -type f   # 空
```

| 检查项 | 结果 |
|---|---|
| 新提交 | 0（round8 之后累计仍是 5 个：`30519bb`…`6a1256b`） |
| 工作区变更 | 与 round8 快照 `1cf920c` 逐字节一致（round9 已验证，mtime 未变） |
| `go build ./...` | 通过 |
| `go vet ./internal/gateway/` | 无输出 |
| `go test ./internal/gateway/` | 5 顶层 / 7 子用例红，37.9s |
| VPS 部署 | `6a1256b`，`active`，health 200 |

红用例（本轮实测，与 round9 完全一致）：

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

## 二、本轮新发现

### N10-1（P0）完整长流被判"中断"：`tee` 只留头部 256KB

`passthrough_stream.go:242`：

```go
_, copyErr := io.Copy(writer, response.Body)
if copyErr == nil && isUpstreamStreamInterruptedResponse(response, closesUpstreamStream(tee.Bytes())) {
    if len(end) > 0 && end[0] != nil && tee.Total() > 0 {
        end[0].closeInterruptedStream(w)          // ← 给一个已经完整结束的回答补"截断帧"
    }
    return ..., newUpstreamStreamInterruption(io.ErrUnexpectedEOF, int(tee.Total()))
}
```

`tee` 是 `limitedBuffer{max: 256*1024}`，而 `limitedBuffer.Write` 只 `append` 到满为止（`passthrough_stream.go:76-90`）——**留的是头部，不是尾部**。`closesUpstreamStream` 的判断逻辑本身也只回看末尾 4096 字节，但拿到的 `tee.Bytes()` 在长流上根本不含末尾。

**实测（探针，716KB 完整 SSE，末尾 `[DONE]`）**：

```
body size = 716144 bytes, cap = 262144
client got 716329 bytes, [DONE] x2, synthetic length frame: true
COMPLETE stream reported as interrupted: upstream stream interrupted after 716144 bytes: unexpected EOF
```

三个后果：

1. **客户端看到"假截断"**。Chat：完整答案之后再补 `finish_reason=length` + 第二个 `[DONE]` → agent 认为被截断、继续续写；Claude：在真正的 `message_stop` 之后再发 `message_delta(max_tokens)` + `message_stop`（事件序列违规）；Responses：`response.incomplete / upstream_stream_interrupted`。
2. **健康 Provider 被记软失败**：`execErr != nil` → `classifyProviderFailover` 的 `transportErr != nil` 分支 → `failoverSoft` → 60s 内 3 次即冷却 1 分钟（`provider_failover.go:117-123`）。即：**一次长回答 = 一次 Provider 软失败**。
3. 触发面：`canDefer == false` 的分支才走到这里，也就是**单账号配置（最常见）或链路最后一跳**。多账号时走的是"整段缓冲"分支（N9-1），那里的 256KB 问题不成立，但换来的是 TTFT 退化。

**判据本身就是错的**：`bufio.Scanner` 正常读到 EOF 时 `scanner.Err() == nil`，与"上游被切断"完全同形；唯一可靠的证据是转换器**有没有见到收尾事件**（`[DONE]` / `response.completed|incomplete` / `message_stop`），而这个布尔量每个转换器本来就有（`sawAnyEvent` / `messageStarted` / `[DONE]` 分支）。

**修法**：删掉 `tee` 参与判定；让转换器直接返回"是否见到收尾帧"，透传侧用一个只记尾巴的最小 ring（或直接在 `io.Copy` 前用 `bufio.Reader` 扫尾）。

### N10-2（P1）中断收尾帧绕过心跳锁 —— 与心跳 goroutine 并发写同一个 ResponseWriter

```go
hw := newSSEHeartbeatResponseWriter(w)          // passthrough_stream.go:230-232
hw.WriteHeader(response.StatusCode)
stopHeartbeat := startSSEHeartbeat(hw, sseHeartbeatInterval)   // 15s 一次
defer stopHeartbeat()
...
end[0].closeInterruptedStream(w)                // :246 ← 写的是裸 w，不是 hw
```

`sseHeartbeatResponseWriter` 的唯一作用就是**用 `mu` 把正常写出与后台心跳串起来**（`sse_heartbeat.go:83-107`）。收尾帧直接写 `w`，等于在心跳还在跑的时候，两个 goroutine 同时写同一个 `http.ResponseWriter`（net/http 明确不支持并发写）。长流（>15s）里心跳 tick 与中断收尾帧撞上的概率不低，后果是 SSE 帧被撕开 —— 客户端看到坏行，或 `message_delta` 拼在注释中间。

**修法**：`end[0].closeInterruptedStream(hw)`；先把 `hw` 传下去，其它什么都不用改。

### N10-3（纠错）A5 其实已达标

```go
// cursor_oauth.go:178-181
return time.Unix(claims.Exp, 0).Add(-cursorTokenExpirySkew)   // 5 分钟
// cursor_oauth.go:306-317
expiresAt, _ := time.Parse(time.RFC3339, cred.ExpiresAt)
return time.Now().After(expiresAt)                            // 天然提前 5 分钟
```

`ExpiresAt` 只有两个来源（`cursorTokenExpiry`，两条路径都用它），所以 leeway 是内建的。round6–round9 把它记成"无 leeway"是看了 `:236` 那个 poll 函数看错了。**A5 从"未修"改为"已达标"**，不必再提。

唯一的边角：JWT 解析失败时 `ExpiresAt = now+1h`，此时没有真实过期时间，属于可接受兜底。

### N10-4（纠错）M-1 其实已修

```go
// convert_claude_openai.go:480-485
stopReason := mapClaudeStopReasonToOpenAI(stringValue(payload["stop_reason"]))
if len(clientStopSequences) > 0 && matchedStopSequence(openAIAssistantVisibleText(assistantMessage), clientStopSequences) != "" {
    stopReason = "stop"
}
```

覆写被 `len(clientStopSequences) > 0 && matched…` 守住了，语义正确（客户端自己的 stop sequence 命中 → Chat 只认 `stop`）。**从"❌"改为"✅"**。

顺带确认 P2 的映射表是完整的、没有 `default: return reason` 漏值：

```go
// convert_claude_openai.go:427-438
"" / end_turn / stop_sequence / refusal / pause_turn → "stop"
max_tokens / model_context_window_exceeded          → "length"
tool_use                                            → "tool_calls"
default                                             → "stop"   // 兜底，不再原样漏给客户端
```

---

## 三、未提交「流中断」改造：既有 P0 复验（全部仍在）

### P0-1 判定函数恒 true（5 处调用，全部在循环之前取值）

```go
// stream_interruption_close.go:21-23
func isUpstreamStreamInterruptedReader(r countingReader) bool {
    return r.r != nil          // 恒 true
}
```

调用点：`convert_claude_openai_stream.go:87`、`convert_claude_responses_stream.go:253`、`convert_openai_claude_stream.go:126`、`convert_openai_responses_chat_stream.go:576`、`:815`，以及 `convert_responses_claude_stream.go` 的 `state.interrupt` 路径。

**用户可见症状（这是本轮补充的、比"测试红"更有说服力的表述）**：每条**正常结束**的转换流都会在收尾前被拦下，改写成一个"截断"终态——

| 转换方向 | 正常回答被强行改写成 |
|---|---|
| Claude → Chat | `finish_reason:"length"`（`closeInterruptedChatStream` 把 `stop` 映射成 `length`） |
| Chat → Claude | `message_delta{stop_reason:"max_tokens"}` + `message_stop` |
| Chat → Responses | `response.incomplete / upstream_stream_interrupted` |
| Responses → Chat | `finish_reason:"length"`（有工具调用时 `tool_calls`） |
| Claude → Responses | `response.incomplete / upstream_stream_interrupted` |

叠加"每个 err 都计入软失败"→ **正常回答既被报成截断，又把 Provider 计成故障**。

### P0-2 "已缓冲可重试"前提不成立（会重复正文）

`protocol_proxy.go:620-640` 的 `deferBackup` 分支注释写「the attempt was buffered, the client has seen nothing」，但：

- `deferredSSEWriter.Write` 直接 `w.base.Write(p)`（`empty_stream_retry.go:164-176`，不缓冲）；
- `failoverResponseWriter.WriteHeader(200)` 因为 `statusCode < 400` 立刻 `commitHeaders` → `passthrough = true`（`provider_failover.go:236-262`）。

所以第一个 delta 就已经到客户端，`retryable=true` 只是让上层 `Discard()` 拦住**后面**的写。这条不需要论证——作者自己写的 `TestFinishConvertedProxyMidStreamDropIsRetryableWhenDeferred` 就是失败证据（`got content:"partial"`），`zz_dbg2_test.go` 也是同一个探针（`retryable=true body="partial"`）。

### P0-3 多账号链路丢心跳

`startSSEHeartbeat` 在 `protocol_proxy.go:645`，而 `deferBackup` 分支两条 `return`（`:628-640`）都在它之前 —— 配了备用账号的转换流完全没有心跳，正好回退 `fix(sse): 长思考静默时发送心跳`。

### P1 其余（沿用，逐项仍在）

| # | 问题 | 位置 | 状态 |
|---|---|---|---|
| a | `runStreamConverter` 用 `err == nil && ContentLength > 0` 判中断 → **任何带 Content-Length 的完整转换流都被判截断**（注释说"比 Content-Length 短"，实现里根本没比） | `protocol_proxy.go:586-592` | 仍在（与 N10-1 同类，且更直白） |
| b | 真实 RST（`copyErr != nil`）不写收尾帧，裸返回 | `passthrough_stream.go:251-253` | 仍在（判据反了） |
| c | `closesUpstreamStream` 裸 `Contains`（正文里出现 `[DONE]` 就认完） | `stream_interruption_close.go:30-48` | 仍在 |
| d | `closeInterruptedClaudeStream` 无条件 `stop_reason:"max_tokens"`，覆盖真实的 `stop_sequence`/`tool_use` | `:148-152` | 仍在 |
| e | `streamRetryRecorder` 接口全仓无实现 → 死代码 | `protocol_proxy.go:578-581`、`:623` | 仍在 |
| f | `countingReader.n` 只用来拼错误消息，判定从不读它 | `empty_stream_retry.go:114-124` | 仍在 |
| g | 缓冲分支"空流"与"非空截断"两个 `return` 一字不差，if 是死代码 | `passthrough_stream.go:204-213` | 仍在 |
| h | `attemptReasonStreamInterrupted` 不可达（真中断走 `execErr` 分支，到不了 `attemptFailoverReason`）；且拿非 2xx 正文做子串匹配会误判 | `provider_failover.go:1074-1080` | 仍在 |
| i | `closeInterruptedResponsesToChatStream` 丢 usage（`_ = usage`） | `stream_interruption_close.go:221` | 仍在 |
| j | `zz_dbg2_test.go` 调试脚手架（`TestDbgWriterType`）随提交上车 | 新增文件 | 仍在，必须删 |

---

## 四、历史遗留逐项复核（H / M / N 系列）

| # | 问题 | 当前证据 | 状态 |
|---|---|---|---|
| H-1 | `output_config.format` 带 `name` → Anthropic 400 | `convert_response_format.go:41-43`（`format["name"] = name`）；magpie `anthropic.go:957` 明确只发 `{type, schema}` | ❌ |
| H-2 | `json_object` 原样发 Claude（Anthropic 只吃 schema） | `convert_response_format.go:19-20`；magpie 走 `Format.instruction()` 进 system prompt | ❌ |
| H-3 | adaptive 分支整体替换 `output_config`，丢掉 format | `convert_claude_openai.go:323` | ❌ |
| H-4 | `allowed_tools` 塞进 Claude `tool_choice` | `convert_responses_claude.go:497-509` | ❌ |
| H-5 | 缓存断点白名单只认 text/image/document，不含 `tool_result` | `convert_claude_cache_marks.go:92-105` | ❌ |
| H-6 | 思考余量补正生产不可达 | `server.go` `rewriteClaudeUpstreamMaxTokens` 先行 | ❌ |
| H-7 | 非 5.2/5.3 的 `glm-*` 回落 `none`（GLM 只认 min/low/medium/high/max） | `claude_thinking_off.go:237-238` | ❌ |
| H-8 | `knownClaude == false` 被直接当"非 Claude"删 thinking | `claude_thinking_off.go:270-276` | ❌ |
| H-9 | 出图拒绝检查 `route.ProviderID` 而非 `decision.ProviderID` | `server.go:4204` 先 `imagesRouteRefusal(route)`，`:4216` 才 `imagesEffectiveProvider`；`route.ProviderID=""` 或落在 fallback 上时会被误拒 | ❌ |
| M-1 | Claude→Chat 无条件 `stop` | 已被 stop-sequence 条件守住 | ✅ **已修**（本轮纠正） |
| M-19 | 响应正文不脱敏入库 | `redact*` 只覆盖 Provider 凭证；请求/响应 body 原文入库 | ❌ |
| N-1 | `workbuddy_ai` 空 `oauth_expires_at` → 永不续期、无 401 兜底 | 续期策略未变 | ❌ |
| 假测试 | `TestClaudeToolChoiceWireShape` 只 `json.Marshal` 自己 unmarshal 的 map，恒真 | `convert_tool_restrictions_test.go:126-140` | ❌ |

---

## 五、完整排查清单（本轮复验后的总账）

### 5.1 协议转换

| ID | 项目 | 状态 | 备注 / 证据 |
|---|---|---|---|
| P1 | Claude→Chat 流式 usage 合并 | ✅ | `message_start` + `message_delta` 合并，round8 复核 |
| P2 | `stop_reason` 全量映射 | ✅ | 映射表完整，`default → stop` |
| P3 | Chat→Responses 保留工具调用前正文 | ✅ | `a55c35d` |
| P4 | Responses→Chat reasoning 增量 | ✅ | — |
| P5 | `incomplete` / `max_output_tokens` 透出 | 🟡 | `1ad2f50` 修好，但**被未提交改造覆盖**（N9-6，测试报 `upstream_stream_interrupted`） |
| P6 | Chat→Claude block index / 关闭顺序 | ✅ | — |
| P7 | 工具 ID 哈希后缀防碰撞 | ✅ | — |
| P8 | Responses→Claude 合成 `tool_result` | ✅ | — |
| P9 | `redacted_thinking` / 反向 reasoning | 🟡 | 反向仍丢全部 reasoning |
| P10 | `strict`/`top_k`/`metadata`/`prompt_cache_key` | 🟡 | `strict` 已搬（response_format 侧）；Claude 工具构造器无 |
| P11 | 单一 IR 架构（对齐 magpie `ir.go`） | ❌ | 架构级，暂不动 |
| N10-1 | >256KB 完整长流判"中断" + 假截断帧 | 🔴 **新** | `passthrough_stream.go:242`；探针实证 |
| N10-2 | 收尾帧绕过心跳锁，并发写 ResponseWriter | 🔴 **新** | `passthrough_stream.go:246` |
| P0-1/2/3 | 未提交改造三条 P0 | 🔴 | 见第三节 |

### 5.2 路由 / 调度

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| R1 | RPM 限流 | 🟡 | 有 Provider 级 `provider_lanes.rpm`；无按 Key / 按用户 |
| R2 | 冷却跟随 Retry-After / resets_at | 🟡 | `provider_failover.go:491-492` 仍取 `max(retryAfter, resetsAt)` → 30s 被拉成 60min；`:1206` 复探传空 hint 把 1h 缩回 2m |
| R3 | billing 不算硬额度 | ✅ | `6a1256b`，`hardQuotaHints` 已剔除 billing |
| R4 | 截断续传 | 🔴 | 未提交实现会重复正文（P0-2） |
| R5 | session pin 落库 | ✅ | `30519bb`，两个小缺口未收 |
| R6 | per-account 闸门 + FIFO | 🟡 | per-account 成立；`close(broadcast)` 惊群非 FIFO；纯 RPM lane 排队者 `max<=0` 直接返回空 release，等满 wait 才 429（`provider_lanes.go:154-160`） |

### 5.3 横切安全

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| S1 | 管理 API CORS | ✅ | 已迁到 `cors.go`，白名单；`trae.go:383` 仍有一条页面级 `*`（非管理 API） |
| S2 | 请求体大小上限 | ✅ | `request_body_limit.go:48` 全局 `MaxBytesReader` |
| S3 | 登录/注册/自检/pac/setup 限流 | 🟡 | 仅登录有 `authThrottle`；`admin_auth.go:448` setup 对已用实例要求 loopback（TOCTOU 未收） |
| S4 | 请求/响应脱敏 | 🟡 | 请求侧已做；响应侧未做（M-19） |
| S5 | 首包后空闲超时 | ❌ | `Timeout: 0` 仍有 10 处（`protocol_proxy.go:144,448`、`server.go:5166…5652`） |

### 5.4 Provider 账号

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| A1 | 刷新令牌加锁 | ✅ | `16c00d3`；锁表 3 瑕疵未收（`last` 只在创建时赋值 / `TryLock` 可破坏互斥 / retire 早于 persist） |
| A2 | host 白名单 | ✅ | `zhipu_usage.go:30` `baseURLHostIn(baseURL, ...)` |
| A3 | images 路由 / 全局 Key 回落 | 🟡 | 路由已收；H-9 判定错位仍在 |
| A4 | Claude 额度窗口 + 429 负缓存 | ❌ | `claude_oauth_usage.go:88-92` 仍无 `seven_day_overage_included` |
| A5 | Cursor 过期提前量 | ✅ | **本轮纠正**：`cursorTokenExpiry` 内建 5 分钟 |

### 5.5 控制台（能力缺口）

| 项目 | 状态 | 证据 |
|---|---|---|
| 实时请求流 | ❌ | `web/src` 只有轮询 |
| 路由解释聚合视图 | ❌ | `analysis.attempts` 已存，无聚合页 |
| 会话视图 | ❌ | `sessionId` 只声明未展示 |
| 一键重放 | ❌ | `grep -ri replay web/src` 零命中 |
| 日志成本列 | ❌ | `costMicros` 只在密钥页 |
| 告警未读角标 | ❌ | — |

---

## 六、验收门槛（合入前必须全过）

1. `go test ./internal/gateway/` 全绿（当前 5 顶层 / 7 子用例红）。
2. 删 `zz_dbg2_test.go`、`streamRetryRecorder` 死接口、`countingReader.n` 无效计数、`runStreamConverter` 的 ContentLength 推断、`passthrough_stream.go:204-213` 死分支。
3. **判定改成"见到收尾帧"**：删 `isUpstreamStreamInterruptedReader`，各转换器用自己已有的 `sawAnyEvent / messageStarted / [DONE]` 布尔量。
4. **N10-1**：`tee` 不得参与完成性判定（只留头 256KB 必然误判）；若一定要看尾巴，用独立 ring buffer 或 `bufio.Reader` 尾扫。
5. **N10-2**：收尾帧写给 `hw` 而不是 `w`。
6. **P0-2**：转换输出先进 `bytes.Buffer`，判定完再一次性下发；或只保留"一个字都没写过"时的重试。
7. 心跳必须在任何可能阻塞客户端的分支之前启动（P0-3）。
8. 流中断不得计入 Provider 软失败（`classifyProviderFailover` 需要 `failoverNone` 分支，或至少不 `bumpSoftFailure`）。

## 七、建议顺序

1. 先修判定（门槛 3）→ 5 个红用例自然变绿。
2. 修 N10-2（一行）→ 消掉并发写。
3. 修 N10-1（判据换掉）→ 消掉长流假截断。
4. 统一"缓冲/重试"语义（P0-2 与 N9-1 必须同语义），补回心跳（P0-3）。
5. 把流中断从"Provider 故障"里摘出来。
6. 回到老账：H-1…H-9（小改动高收益）、A4、控制台 6 项。

## 八、复现

```bash
git log -1 --format='%h %ci %s'                 # 6a1256b 01:28:25
ls -l --time-style=+%F_%T internal/gateway/{passthrough_stream,protocol_proxy}.go
go build ./... && go vet ./internal/gateway/
go test ./internal/gateway/                     # 5 顶层红
go test ./internal/gateway/ -run 'TestChatLengthClosesResponsesStreamAsIncomplete|TestFinishConvertedProxyMidStreamDropIsRetryableWhenDeferred' -v
ssh root@173.249.207.202 'systemctl show llm-protocol-gateway -p ActiveEnterTimestamp --value'
```

N10-1 探针源码副本：`.workbuddy/reviews/round10-evidence/zz_round10_probe_test.go.txt`
