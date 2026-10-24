# 第八轮 · 另一 Agent 改动代码审查（round8）

- 审查时间：2026-10-23 01:33–02:05
- 审查范围：`16c00d3..6a1256b`（5 个新提交）+ 未提交工作区（13 改 + 3 新增）
- 对照基准：`C:\guodevelop\demo\magpie`（重点比对 `internal/gateway/fallback.go` 的流终止帧判定）
- 基线：
  - `6a1256b` 干净 HEAD：`go build ./...` / `go vet` 通过；`go test ./internal/...` 仅 `internal/tunnel` 5 个用例因 Windows 无 `/bin/sh` 失败（环境噪声，与本轮改动无关），`internal/gateway` 全绿
  - 未提交工作区：`go build` / `vet` 通过，但 **`go test ./internal/gateway/` 7 个用例失败**（详见第二节）
- 快照：`git stash create` → `1cf920c`（不含未跟踪文件）；验证在独立工作树 `.workbuddy/review-tmp`（HEAD + 拷贝工作区，已清理）
- **本轮未修改任何业务代码，未提交**

---

## 结论先说

1. **5 个已提交的改动方向都对，但 `5a6f2d1`（冷却时长）与 `581ea6f`（lane 排队）各带一个"逻辑反了/永远不生效"的实测缺陷**：前者让上游明说「30 秒后再来」的账号被冷藏 60 分钟，后者让纯 RPM 配额的排队者永远等满超时再被拒。
2. **未提交的"流中断"改造目前不可合并**：核心判定函数 `isUpstreamStreamInterruptedReader` 恒返回 `true`，**每一条正常结束的转换流都会被判成截断**（6 个既有测试 + 1 个自写测试红）。
3. 更关键的是**这条改造的前提不成立**：它假设"有备用账号时这次尝试被缓冲、客户端什么都没看到"，但 `failoverResponseWriter.WriteHeader(200)` 会立刻 commit 并置 `passthrough=true`，第一次 Write 就直达客户端。于是返回"可重试"后，备用账号会把同一段正文再发一遍 —— **客户端看到重复正文**。他们自己新写的测试就是这个失败证据。
4. **顺带打掉一个已上线的修复**：新的 `deferBackup` 分支在 `startSSEHeartbeat` 之前 return，所有"有备用账号"的转换流不再发心跳 → 长思考静默会被 Cloudflare 空闲切断。
5. **上轮 9 个高危（H-1…H-9）与 M-1/M-19/N-1 一项未动**（本轮 5 个提交没有碰任何 `convert_*` / `claude_thinking_off.go` / 出图路径）。调度 R2/R3/R5 有真实进展，R1/R4/R6 部分，安全 S1/S2 已修、S3 半、S4/S5 未动，A4 未动，控制台 6 项全未动。

---

## 一、已提交的 5 个提交

| 提交 | 内容 | 判断 |
|---|---|---|
| `581ea6f` | 限速独有的 Provider 也能排队；Retry-After 跟限速窗口 | ⚠️ 方向对，排队永不唤醒 |
| `dc6683b` | Purge 落库失败把 Provider 放回 | ✅ 正确，位置注释不实 |
| `30519bb` | 会话 pin 落库 | ✅ 正确，有两个小缺口 |
| `5a6f2d1` | 冷却跟随 Retry-After / resets_at；billing 不算硬额度 | ❌ 三处逻辑错误（实测） |
| `6a1256b` | 识别 "exceeded your current quota" | ⚠️ 合理但有误判面 |

### 1.1 `581ea6f`：纯 RPM lane 的排队者是"等死"

`providerQueueCapacity` 从"只看 max"改成"任一预算存在即可排队"，`max=0, rpm>0` 的 lane 现在有 8 个等候位。但唤醒机制没跟上：

- `acquireSlot` 的等待靠 `lane.broadcast`，而 `release()` 只在 `max > 0` 时返回（`provider_lanes.go:161-171`：`if lane.max <= 0 { return func() {} }`），纯限速 lane **没有任何代码会 `close(broadcast)`**；
- 唯一会醒的路径是 `waitFor` 的 timer = 整个 deadline（`provider_lanes.go:205-220`），醒来时循环立刻命中 `!now.Before(deadline)` → 直接报饱和。

实测（`.workbuddy/review-tmp` 探针，窗口本应 80ms 后翻页）：

```
wait=1.501s err=provider is at its concurrent request limit (waited for a free slot)
```

即：**配置了 queue wait 的纯 RPM Provider，请求必然挂满 60s 再 429**，比改动前的"立即 429"更糟。顺带：这条错误文案在 `max=0` 的 lane 上说"concurrent request limit"是错的。

`provider_lane_capacity_test.go:21` 的用例把这种行为当成了期望（只断言"排上了、超时后被拒"），没有覆盖"窗口翻页后被放行"，所以绿灯不代表可用。

### 1.2 `dc6683b`：正确

快照/回填思路正确，DB 半写场景下仍是 best-effort（内存回填后若再次 Save 成功才真正一致），可接受。瑕疵：`RestoreProviderSnapshot` 注释说"包括它的 position"，实现是 `append` 到末尾（`router.go:1358`），控制台里被删的 Provider 复活后会跑到列表最后。

### 1.3 `30519bb`：正确

- `scheduleFlushLocked` 在 `mu` 内调 `time.AfterFunc`，回调里先拿 `flushMu` 再拿 `mu`，不会死锁（读写路径都持 `mu`，实测无 race）；
- 写失败把 key 放回 dirty，但**没有重新排期** —— 要等下一次 pin 变化才会再写，进程退出就丢；注释"下次重试"名不副实（`provider_affinity.go:196-208`）；
- 加载时不裁剪到 `max`（1 万条上限只在 `set` 里执行），脏数据多时启动会带进超量 pin。低危。

### 1.4 `5a6f2d1`：三个实测缺陷

**(a) 上游明说 30 秒，被 resets_at 拉成 60 分钟**（`provider_failover.go:489-495`）

```go
want := hint.retryAfter              // 30s
if !hint.resetsAt.IsZero() {          // 7d 窗口 48h 后重置
    if until := hint.resetsAt.Sub(now); until > want { want = until }  // 48h
}
// → cap 60min
```

探针实测：`Retry-After=30s + 7d 窗口 48h 后重置 → soft=1h0m0s hard=1h0m0s`。

问题有两层：
- **优先级反了**：上游显式给了 `Retry-After`（"1 分钟后再来"），却被"7 天窗口什么时候重置"覆盖。后者跟当前这次限流通常无关（Claude 的 429 多是 5h/分钟级，7d 窗口的 resets_at 可能还在两天后）。
- **soft 路径也用 resets_at**：3 次连续 429 触发 soft 冷却，直接把账号冷藏 1 小时。账号池小的时候这是可用性事故。

建议：`Retry-After` 存在时以它为准；`resets_at` 只在 `hard == true` 且分类原因是额度耗尽（`usage_limit/quota`）时才参与；7d 桶不该无条件参与比较，至少要和 5h 桶取"更近的那个"。

**(b) 复探失败把 1 小时冷却缩回 2 分钟**（`provider_failover.go:1206`）

```go
s.refreshProviderUnavailableCountdown(id, prev.hard, reason, "", nil)
```

注释写的是"vendor-named reset time is kept"，实现却传空 hint 重算 → 回落到硬默认 2 分钟。实测：`首次=1h0m0s 复探失败后=2m0s`。对只有 Retry-After/body 提示（没有 OAuth 用量缓存）的 Provider，等于**每分钟去打一次已经确定凉了的账号**，正是 cooldown cap 想避免的行为。修法：`refresh` 时保留 `prev.nextRetry`（只延不缩），或把 `prev` 里的 hint 一并存下来。

**(c) 死代码 + 注释与常量不符**（`provider_failover.go:499-505`）

`if ttl < providerCooldownFloor*3 { ttl = ... }` —— `providerCooldownFloor = 5s`，`*3 = 15s`，而 `ttl` 至少是 `providerFailoverRecoveryInterval = 2min`，条件永假。注释说的是 "between the soft default and a quarter hour"，常量却写着 15 秒。

附带低危：`parseResetsAt` 把数字一律当**毫秒**（`time.UnixMilli`）。若某家上游给的是秒级 epoch，会算成 1970 年 → `at.After(now)` 为假 → 静默退化（不炸，但"支持秒/毫秒"的说法不成立）。

### 1.5 `6a1256b`：合理，注意误判面

`"exceeded your current quota"` 是 OpenAI `insufficient_quota` 的原句，加进来对。但中转/聚合商大量用它表示"今日额度用完了"这类**会自然恢复**的限额，进 `hardQuotaHints` 后是 `failoverHard` → 立刻整轮冷却。建议观察线上 `reason=http_429_usage` 的分布再决定是否保留；或只在 401/402/403 认这句，429 仍走 soft。

---

## 二、未提交的"流中断"改造（本轮重点，不可合并）

范围：`stream_interruption_close.go`（新）、`empty_stream_retry.go`、`passthrough_stream.go`、`protocol_proxy.go`、5 个转换器、`server.go`/`trae.go`/`qoder_chat.go` 调用点、`stream_interruption_retry_test.go`（新）、`zz_dbg2_test.go`（新，调试脚手架）。

### P0-1 判定函数恒为 true → 所有正常流被判截断

```go
// stream_interruption_close.go:21
func isUpstreamStreamInterruptedReader(r countingReader) bool {
	return r.r != nil          // 永远 true
}
```

调用方在**循环之前**取一次值（`convert_claude_openai_stream.go:87` 等 5 处）：

```go
r := countingReader{r: reader}
interrupted := isUpstreamStreamInterruptedReader(r)
```

于是只要 `reader != nil`，扫描结束后就进 `closeInterruptedChatStream` / `closeInterruptedClaudeStream` / `closeInterruptedChatToResponsesStream`，并返回中断错误。实测 `go test ./internal/gateway/`：

```
--- FAIL: TestClaudeStopSequenceToChatFinishStream (5 个子用例)
        stream: upstream stream ended after 716 bytes without its closing event
--- FAIL: TestChatToClaudeStreamReportsStopSequence
        stream: upstream stream ended after 298 bytes without its closing event
```

夹具里 `message_stop` 齐全也无济于事 —— 因为根本没看内容。生产影响：**每一条转换流式请求**都会多吐一个 `finish_reason:"length"`/`message_delta max_tokens` 收尾帧，并把一次成功请求报成失败（进而触发 failover 的 transport 分支、provider soft 计数 +1，连续 3 次把健康账号标成异常）。

根因是 `countingReader` **按值传参**，函数内既看不到 `n` 也看不到 EOF；正确做法是把"是否见到收尾帧"作为转换器内的布尔量（各转换器本来就有 `sawAnyEvent`/`message_stop` 处理点），或让 `countingReader` 记录 `sawEOF`/`err` 并以指针传递。

### P0-2 "缓冲后可重试"的前提不成立 → 客户端收到重复正文

`protocol_proxy.go:619-641`：

```go
deferBackup := fw.canDefer && !fw.passthrough
if deferBackup {
    usage, streamErr := runStreamConverter(dw, response, model, streamConvert)
    ...
    return response.StatusCode, usage, nil, streamErr, true   // retryable
}
```

前提是"这次尝试被缓冲，客户端什么都没看到"。但 `deferredSSEWriter` 只推迟 `WriteHeader`，**正文照写**；而 `failoverResponseWriter.WriteHeader` 的条件是 `if !w.canDefer || statusCode < 400 { commitHeaders }`（`provider_failover.go:237-247`）—— 200 一定 commit，`passthrough = true`。所以第一次 `dw.Write` 就把正文写给了客户端。

后果链：转换器已把 `xxx` 写给客户端 → 返回 retryable → 上层 `writer.Discard()`（只能拦住之后的写，拦不住已经出去的）→ 备用账号从头再发一遍 → **同一段文字出现两次**。因为他们还给 `closeInterrupted*Stream` 写了收尾帧，客户端会先收到 `[DONE]`/`message_stop` 再收到第二段流，属于协议级错乱。

证据是他们自己新写的 `TestFinishConvertedProxyMidStreamDropIsRetryableWhenDeferred`：

```
stream_interruption_retry_test.go:71: client should not see the interrupted attempt,
got "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
```

同一根因也波及 `finishConvertedProxyWithEmptyStreamRetry`（同 Provider 最多 3 次），会把正文重复 2–3 次。

正确做法二选一：
- 转换器输出先写进 `bytes.Buffer`，成功或"已判定中断"后再一次性 `dw.Write`（内存代价 = 单条回答，可接受，且 pass-through 路径已经是这么做的）；
- 或者不给转换路径做"中断重试"，只做"收尾帧"，重试仅限"一个字都没写过"（`dw.Committed() == false`）。

### P0-3 多账号链路丢掉 SSE 心跳（回归）

`startSSEHeartbeat` 在 `deferBackup` 分支之后（`protocol_proxy.go:643`），而该分支两条路径都 `return`。结果：**只要这个 Key 配了备用账号（`i < len(attempts)-1`），转换流就完全没有心跳**。这正好回退掉早先的 `fix(sse): 长思考静默时发送心跳`：长思考超过 Cloudflare 100s 空闲窗口就会被切，而备用账号是常态配置。

### P1-1 `runStreamConverter` 的 Content-Length 推断会误判

```go
// protocol_proxy.go:586
if err == nil && response.ContentLength > 0 {
	err = newUpstreamStreamInterruption(...)
}
```

`net/http` 已经保证：声明了 Content-Length 的响应如果早断，`Read` 会返回 `io.ErrUnexpectedEOF`，转换器会以 `scanner.Err() != nil` 上报（这些分支都改过了）。所以 `err == nil` 恰恰说明**读完整了**；此条件唯一的效果是把"带 Content-Length 的完整 SSE"（反向代理缓冲、小响应常见）误判为截断。应删除。

### P1-2 真正的中断路径反而不写收尾帧

pass-through 非缓冲路径：

```go
// passthrough_stream.go:244
if copyErr != nil {
	return response.StatusCode, TokenUsage{}, tee.Bytes(), copyErr   // 没有 closeInterruptedStream
}
```

连接被 reset / `io.Copy` 报错是最常见的中断形态，这里直接裸返回，客户端仍停在半句上 —— 与本节要解决的问题背道而驰。收尾帧只在 `copyErr == nil && !closesUpstreamStream(...)` 这一条窄路径上写。

### P1-3 `closesUpstreamStream` 用裸 `strings.Contains`，比 magpie 弱

```go
if strings.Contains(text, "[DONE]") { return true }
for _, marker := range []string{"response.completed", "response.incomplete", "message_stop", ...}
```

magpie 的做法（`fallback.go:1930-1940`）是**锚定到行首/带引号的 JSON 键**，并且注释写明了原因：模型正文里出现 `[DONE]`、`message_stop` 这类字样时，裸 Contains 会把**被截断的流**误判成完整（先写完就被切断 → 不补收尾帧、不重试）。反方向也有风险：不发送任何终止帧、直接关连接的中转商（magpie 用 `holdWriter` 专门兼容这类）在我们这里 100% 被判中断，多账号 Key 上会触发一次无谓的重复计费重试。建议直接移植 magpie 的 `streamEnds` 锚定写法。

### P1-4 语义：把"被切断"报成 `max_tokens` / `length`

`closeInterruptedClaudeStream` 固定写 `stop_reason:"max_tokens"`，Chat 侧写 `finish_reason:"length"`。对 Claude Code 是"接着写"的正确触发，但代价是**成本/截断统计与真实原因脱钩**（额度没超也记一次 length）。可接受，但应在注释里写明这是刻意选择的"可续写"语义，而不是"真实原因"。

### P2 其余

| # | 问题 | 位置 |
|---|---|---|
| a | `streamRetryRecorder` 接口声明了 `recordsForRetry()`，全仓无任何实现 → 该分支是死代码 | `protocol_proxy.go:579,623` |
| b | `closeInterruptedClaudeStream` 同时从 `toolIndexes` 和 `toolOpen` 收集索引，两个都填时会**重复写 `content_block_stop`**（当前唯一调用方只填一个，属定时炸弹） | `stream_interruption_close.go:130-140` |
| c | `closeInterruptedResponsesToChatStream` 把 `usage` 丢掉（`_ = usage`），截断轮的 token 统计为 0 | 同文件 |
| d | `attemptFailoverReason` 把 HTTP 错误体当 error 字符串做关键字匹配，正文里出现 "connection reset" 就会把 500 标成 stream_interrupted | `provider_failover.go:1074-1080` |
| e | `writePassThroughResponse` 的 `if len(trim)==0 / else` 两个分支返回值完全相同（死分支） | `passthrough_stream.go:200-213` |
| f | `zz_dbg2_test.go` 是对方的调试脚手架（`TestDbgWriterType`），提交前必须删 | 新增文件 |
| g | `providerBusyRetryAfterSeconds` 与 `providerRetryAfterSeconds` 都是 1，重复常量 | `provider_lanes.go:37-40` |

---

## 三、上轮遗留复检（逐项，行号=当前工作区）

| # | 上轮结论 | 本轮状态 | 证据 |
|---|---|---|---|
| H-1 | `output_config.format` 带 `name` → Claude 400 | ❌ 未修 | `convert_response_format.go:42` 仍写 `format["name"]` |
| H-2 | `json_object` 原样发 Claude | ❌ 未修 | `convert_response_format.go:19-20` |
| H-3 | adaptive 分支整体替换 `output_config` | ❌ 未修 | `convert_claude_openai.go:323` 整体赋值 `{"effort":…}` |
| H-4 | `allowed_tools` 塞进 Claude `tool_choice` | ❌ 未修 | `convert_responses_claude.go:497-509` |
| H-5 | 缓存断点白名单不含 `tool_result` | ❌ 未修 | `convert_claude_cache_marks.go:93-105` 仍只认 `text/image/document` |
| H-6 | 思考余量补正生产不可达 | ❌ 未修 | `server.go` 的 `rewriteClaudeUpstreamMaxTokens` 仍先行 |
| H-7 | 非 5.2/5.3 的 `glm-*` 回落 `none` | ❌ 未修 | `claude_thinking_off.go:237-238` |
| H-8 | `known==false` 被当非 Claude 删 thinking | ❌ 未修 | `claude_thinking_off.go:266-276` |
| H-9 | 出图提前拒绝查 `route.ProviderID` | ❌ 未修 | `server.go:4204` 用 route 判定，`:4216` 才用 `decision.ProviderID` |
| M-1 | Claude→Chat 无条件 `stopReason="stop"` | ❌ 未修 | 非流式/流式两处守卫仍缺 |
| M-19 | 响应正文不脱敏 | ❌ 未修 | `server.go` 多处 `ResponseBody: string(responseBody)` |
| N-1 | `workbuddy_ai` 空 exp → 永不续期、无 401 兜底 | ⚠️ 调试行已删（工作区干净），根因未修 | 续期策略未动 |
| 假测试 | `TestClaudeToolChoiceWireShape` | ❌ 未删 | `convert_tool_restrictions_test.go:127` |
| 已修项回归 | 流式 usage / stop→Responses | ✅ 无回归 | `convert_claude_openai_stream.go` usage 合并仍在；Responses 编码器无 `stop` |

---

## 四、差距总账（对照原始清单）

| 类别 | 项 | 状态 |
|---|---|---|
| 协议 | P1 流式 usage / P2 stop_reason 映射 / P3 Chat→Responses 正文 / P4 reasoning 增量 / P5 incomplete / P6 block index / P7 工具 ID 哈希 / P8 合成 tool_result | ✅ 已修（本轮无回归） |
| 协议 | P9 redacted_thinking / Responses→Claude reasoning | 🟡 部分（`convert_claude_responses.go:199` 合并处理，反向仍丢） |
| 协议 | P10 strict / metadata / prompt_cache_key / stop_sequences 转发 | ❌ `"strict"` 在 Claude 构造器里零命中 |
| 协议 | P11 两两手写转换（架构） | ❌ 未动 |
| 调度 | R1 RPM 限流 | 🟡 Provider 级 rpm 已有（`provider_lanes`），**无按 Key 限流**；纯 RPM 排队失效（见 1.1） |
| 调度 | R2 冷却跟随上游 | 🟡 已实现但优先级反了 + 复探缩回（见 1.4） |
| 调度 | R3 billing 不算硬额度 | ✅ 已修；新加 "exceeded your current quota" 待观察 |
| 调度 | R4 截断续传 | 🔴 未提交、当前实现会重复正文（见 P0-2） |
| 调度 | R5 session pin 落库 | ✅ 已修（两个小缺口） |
| 调度 | R6 并发闸门 per-account + FIFO | 🟡 Provider 即账号，per-account 成立；唤醒仍是 `close(broadcast)` 惊群，非 FIFO |
| 安全 | S1 管理 API CORS `*` | ✅ 已修（`cors.go`：协议端点通配、管理端同源） |
| 安全 | S2 请求体上限 | ✅ 已修（`request_body_limit.go` 全局 `MaxBytesReader`） |
| 安全 | S3 登录/注册/自检/token 限流 + setup | 🟡 登录有 `authThrottle`；自助注册/自检/pac token 无限流 |
| 安全 | S4 请求/响应脱敏 | 🟡 请求侧已脱敏，**响应侧未做**（M-19） |
| 安全 | S5 首包后无空闲超时 | ❌ `&http.Client{Timeout: 0}` 仍在 |
| 账号 | A1 刷新令牌加锁 | ✅ 已修（`16c00d3`，锁表 3 个瑕疵待收尾） |
| 账号 | A2 host 白名单 | ✅ 已修 |
| 账号 | A3 images 路由 / 全局 Key 回落 | 🟡 路由已收（`e650076`/`4c7bed9`），H-9 的判定错位仍在 |
| 账号 | A4 Claude 额度窗口 + 429 负缓存 | ❌ 无 `seven_day_overage_included`，无负缓存 |
| 账号 | A5 Cursor 过期提前量 | 🟡 续期加了超时，提前量未改 |
| 控制台 | 实时流 / 路由解释视图 / 会话视图 / 一键重放 / 成本列 / 告警角标 | ❌ 6 项全未动（`web/src` 无 `sessionId`/`costMicros` 聚合） |

---

## 五、建议顺序

1. **先做 P0-1**：删掉 `isUpstreamStreamInterruptedReader` 这个 stub，改成转换器内的"见到收尾帧"布尔量；这一步做完 6 个红测试会自己变绿。
2. **决定 P0-2 的方案**：要么给转换流加缓冲（推荐，与 pass-through 一致），要么放弃"分片后重试"，只保留收尾帧。当前形态既重复正文又破坏协议。
3. **补回心跳**：把 `startSSEHeartbeat` 提到 `deferBackup` 之前，或让 `dw` 自己带心跳。
4. 修 `5a6f2d1` 的优先级（Retry-After 优先、resets_at 仅 hard 额度类）与复探保期；修 `581ea6f` 的唤醒（窗口翻页时 broadcast，或给 rate-only lane 一个定时唤醒）。
5. 删 `zz_dbg2_test.go`、`streamRetryRecorder` 死接口、`runStreamConverter` 的 ContentLength 推断。
6. 之后再回到老账：H-1…H-9（都是小改动、高收益）、A4、控制台 6 项。

---

## 六、复现方式

```bash
# 1) 干净 HEAD 基线（gateway 全绿）
git worktree add --detach /tmp/r8 HEAD && cd /tmp/r8 && go test ./internal/gateway/

# 2) 当前工作区（7 红）
go test ./internal/gateway/ -run 'TestClaudeStopSequenceToChatFinishStream|TestChatToClaudeStreamReportsStopSequence|TestFinishConvertedProxy'

# 3) 冷却/排队三探针（本轮实测数字即来自这里）
#    cooldownTTL: Retry-After=30s + 7d 48h → 1h0m0s
#    refreshProviderUnavailableCountdown: 1h → 2m
#    rate-only lane: wait=1.501s 后 429
```

（探针测试文件已在审查后删除，未留在仓库内。）
