# 第十四轮 · 另一 Agent 改动代码审查（round14）

- 审查时间：2026-10-23 08:26–09:05（+0800）
- 审查范围：**新提交 `34ec9a4`**（15 文件 +1196/−45）+ 工作区 WIP（审查期间仍在编辑）
- WIP 指纹：`git diff | sha256sum = 4f18fe4c…`（08:26）→ `c95030a0…`（08:58，期间对方又改了一次）
- 对照基准：`C:\guodevelop\demo\magpie`
- 基线：`go build ./...` ✅ / `go vet` ✅ / **干净 worktree（34ec9a4）`go test ./internal/gateway/` ✅ 0 红**；**工作区 WIP 2 红**（都是本轮新加的自测）
- VPS（173.249.207.202:18093）：`active`、**线上二进制 = 34ec9a4**（见下）、生产已受影响
- **本轮未修改任何业务代码、未提交**；探针文件已删除，临时 worktree 已移除

---

## 结论先说（先止损，再谈代码）

1. **`34ec9a4` 已经部署到生产，并且它把「边收边发」改成了「整段收完再发」。** 线上二进制 mtime `2026-10-22 17:14:49 PDT` = 北京时间 **08:14:49**，正好是提交时间 08:14:39 之后 10 秒；二进制里含新代码串 `upstream stream interrupted`（2 处）。也就是说：

   - **所有 SSE 流式请求（Chat / Claude / Responses 透传，以及全部协议转换）在客户端侧都是「零输出 → 一次性吐出全文」**；
   - 客户端看到的首字节时间 = 整条回答的生成时间，长思考/长输出会直接撞客户端与中间层的空闲超时；
   - 缓冲无上限（`io.ReadAll`），并发长流按「全文 × 并发数」占内存；
   - 首包前 **没有心跳**（心跳仍是老路径才有），CF/反代 100s 空闲会切断。

   探针实测（`writePassThroughResponse` + 慢速 Body：第一块已产出、后续阻塞）：**第一块到达时客户端字节数 = 0**。

   **建议：立刻把 VPS 回滚到 `6a1256b`**，再讨论中断重试怎么做。这个提交不是「修得不彻底」，是**用「流式」换「可重试」**，方向本身要重议（magpie 的选择见第三节末）。

2. **提交说明里「首包之后被切断的流可交给备用 Provider」只对透传成立，对协议转换完全不成立。** 五个转换器里的中断判定是**循环前对值拷贝取快照**：

   ```go
   r := countingReader{r: reader}
   scanner := bufio.NewScanner(&r)
   interrupted := isUpstreamStreamInterruptedReader(r)   // ← 此刻 r.err 必为 nil
   for scanner.Scan() { … }
   if interrupted { … }                                   // ← 恒 false，死代码
   ```

   探针实测：截断的 Claude 流 → Chat 客户端收到 `finish_reason:"stop"` + `[DONE]`，返回 `err=nil`。**客户端以为是正常结束，failover 层也看不到错误**，`closeInterrupted*` 三个函数在转换路径上是不可达代码。

3. **`runStreamConverter` 的「短于 Content-Length」判定写成了「只要 Content-Length > 0 就是中断」**，不与实际读取字节数比较。凡是上游（或中转/反代）给 SSE 带 `Content-Length` 的，**每一个完整响应都会被判成截断**：先白白重试一次上游（烧配额），最后一次再补一个假的 `max_tokens` 收尾，并把 Provider 计入软失败（3 次 → 冷却 1–2 分钟）。

4. **`N11-1`（长流 usage 丢在 256KB 之外）没修**：新缓冲路径虽然把全文读进了 `streamBody`，但解析仍走 `tee.parse()`，而 `limitedBuffer` 只保留**头部** 256KB。探针实测 400KB 完整 Chat 流，真值 1234/567 → 记为 **0/0**。

5. **语义正确性新增两处错**：中断收尾会给同一 `content_block_stop` 写两次（`index` 重复）；中断收尾不关闭已打开的 `content_block`（`N11-3` 换个位置继续存在）。

6. **工作区 WIP 目前不能提交**：`provider_failover.go:319-320` 两个热路径调试打印（`fmt.Printf("ZZW2…")` / `println("ZZW3…")`，每次 `Write` 打一行，直连 stdout）；`prefix/stream` 拆分抽象与自测互相矛盾（`TestFailoverWriterCutTurnClosesAfterCommittedPrefix`：`prefix "" is not the start of …`）。

---

## 一、范围与证据

```bash
git log -1 --format='%h %ci %s'   # 34ec9a4 2026-10-23 08:14:39 +0800
git diff | sha256sum              # 08:26 4f18fe4c… → 08:58 c95030a0…
git worktree add --detach .workbuddy/review-head14 34ec9a4
(cd .workbuddy/review-head14 && go test ./internal/gateway/)   # ok 38.8s，0 红
go test ./internal/gateway/                                     # 工作区 WIP：2 红（均为新加自测）
```

| 检查项 | 结果 |
|---|---|
| 新提交（round13 之后） | **1 个**：`34ec9a4`（15 文件） |
| `go build ./...` / `go vet` | ✅ / ✅ |
| 已提交树测试 | ✅ 0 红（干净 worktree 实测） |
| 工作区测试 | ❌ 2 红（`TestNoFailoverReplayAfterMidStreamInterruption`、`TestFailoverWriterCutTurnClosesAfterCommittedPrefix`） |
| 线上服务 | `active`，pid 849914，listen `*:18093` |
| 线上二进制 | `2026-10-22 17:14:49 PDT`，含 `upstream stream interrupted` ×2 → **= 34ec9a4** |

### 改动清单（提交 34ec9a4）

| 文件 | 意图 |
|---|---|
| `passthrough_stream.go` +242 | 新增 `writeBufferedStream`（整段 `ReadAll` 后下发）、`streamInterrupter` 三实现、`interceptingStreamWriter` |
| `stream_interruption_close.go` +221（新） | 各协议的中断收尾帧构造 |
| `empty_stream_retry.go` +162 | `isUpstreamStreamInterruption`、`upstreamStreamInterruptionError`、`deferredSSEWriter` 缓冲分支 |
| `protocol_proxy.go` +61 | `runStreamConverter`、转换流拦截重放 |
| `provider_failover.go` +53 | `deferring/failoverUnavailable/streamClosed/recordsForRetry`、`attemptFailoverReason` |
| 5 个转换器 +69 | 接入 `countingReader` 与中断收尾 |
| `server.go`/`qoder_chat.go`/`trae.go` | 调用点补 `streamInterrupter`（纯传参） |

---

## 二、新改动逐条判定

### N14-1（P0，已上线）整段缓冲：流式退化为「最后一次性下发」

```go
// passthrough_stream.go:154-158
if fw, ok := w.(*failoverResponseWriter); ok && stream && response.StatusCode < 400 {
	return writeBufferedStream(fw, response, parseUsage, end...)
}
// :317
streamBody, readErr := io.ReadAll(io.TeeReader(response.Body, tee))
```

- 判定没有 `canDefer` 门槛：**有没有备用账号都缓冲**；
- `executeProtocolFlowWithFailover` 每次尝试都用 `newFailoverResponseWriter`（`provider_failover.go:1032`），所以线上**无一例外**；
- 转换路径更彻底：`interceptingStreamWriter` 先把整条转换流攒进 `records`，`replay` 时才写（`protocol_proxy.go:622-632`）；
- 缓冲期间**不写头、不发心跳**：TTFT 指标（`wrapTTFTWriterWithTiming`）会从「首包毫秒」变成「整段毫秒」，这也是运维侧最容易观察到这一变化的信号；
- 内存无上限，长流按全文驻留（`records` + `bodyLog` 两份拷贝）。

**探针（已复现并删除）**：慢速 Body 第一块产出后阻塞 120ms，`rec.Body.Len() == 0`。

**修法方向**：缓冲只可能用于「首字节之前」的失败；一旦决定流式就不能回头。若一定要重试，只能做 magpie 那种「把切断显式告诉客户端」或「首包前重试」。

### N14-2（P0）转换器中断判定恒 false —— R4 对转换路径完全没生效

`stream_interruption_close.go:21` 的 `isUpstreamStreamInterruptedReader(r countingReader)` 按值接收；各转换器在**读之前**取值（`convert_claude_openai_stream.go:87`、`convert_openai_claude_stream.go:126`、`convert_openai_responses_chat_stream.go:576/815`、`convert_claude_responses_stream.go:253`、`convert_responses_claude_stream.go:723`），此后 `r.err` 由 `&r` 更新，但快照永远停在 `nil`。

**探针实测**：截断 Claude 流（只有 `message_start` + 一个 delta）→ Chat 侧输出 `finish_reason:"stop"` + `[DONE]`，`err=nil`。

连带后果：`closeInterruptedChatStream` / `closeInterruptedClaudeStream` / `closeInterruptedChatToResponsesStream` 在转换路径上是**死代码**，只有单测能调到。

### N14-3（P1）`runStreamConverter` 的 Content-Length 判定恒真

```go
// protocol_proxy.go:586-591
usage, err := streamConvert(w, response.Body, model)
if err == nil && response.ContentLength > 0 {
	err = newUpstreamStreamInterruption(fmt.Errorf("body shorter than its Content-Length %d", …), …)
}
```

没有比较「读了 N 字节 vs 声明 M 字节」。**探针实测**：Content-Length=33、正文恰好 33 字节、转换器正常结束 → 仍判中断。

影响：给 SSE 带 `Content-Length` 的上游/中转（把流缓冲后转发的那种最常见）**每个请求**都会被判截断 → 白重试一次同 Provider 或直接切备用（烧配额），并在 `applyProviderFailoverOutcome(…, 0, nil, "", execErr)` 里按 `err != nil` 计入软失败（`provider_failover.go:851-874`，3 次 → 冷却）。

### N14-4（P1）非缓冲路径用「头部 256KB」判定收尾 → 长流误判 + 重复收尾帧

```go
// passthrough_stream.go 非缓冲分支
if copyErr == nil && isUpstreamStreamInterruptedResponse(response, closesUpstreamStream(tee.Bytes())) {
	if … && tee.Total() > 0 { end[0].closeInterruptedStream(w) }
```

`limitedBuffer` 只留**头** 256KB（`passthrough_stream.go:73-90`），而收尾帧在**尾**。**探针实测**：409,850 字节的完整 Chat 流（尾帧 `[DONE]`）→ 判为中断，客户端额外收到 185 字节（第二个 `finish_reason` + 第二个 `[DONE]`），且该请求被算作 Provider 软失败。

### N14-5（P1）长流 usage 仍是 0/0（N11-1 未修）

`writeBufferedStream` 已经把全文拿在 `streamBody` 里，但解析仍 `tee.parse(parseUsage)`（`passthrough_stream.go:340`），tee 是 256KB 头部副本。**探针实测**：400KB Chat 流，尾部 `usage {1234, 567}` → 记为 `0/0`。

### N14-6（P1）「中断」直接换 Provider 软失败冷却（自伤型级联）

中断以 `execErr` 返回 → `provider_failover.go:1073-1080` 走传输错误分支 → `classifyProviderFailover(status=0, err)` 必给 not-none → 每次中断 `bumpSoftFailure`；与 N14-3 叠加时，**上游只要给 SSE 带 Content-Length，3 个请求就能把健康账号打成「异常」并冷却 1–2 分钟**，随后流量全压到备用账号。

### N14-7（P2）中断收尾重复关闭同一 content_block

```go
// stream_interruption_close.go:127-131
for _, index := range turn.toolOpen { indexes = append(indexes, index) }
```
`toolIndexes` 与 `toolOpen` 会含同一 index。**探针实测**：`index:0` 的 `content_block_stop` 写了 **2 次**。

同一函数还缺「已打开但未记录在 `toolOpen` 的块」的关闭（`N11-3` 原样），`claudeStreamInterrupter`（透传用）更是完全不发 `content_block_stop`，只补 `message_delta` + `message_stop`。

### N14-8（P2）缓冲路径丢上游响应头

`writeBufferedStream` 只手工搬 `Content-Type` / `Cache-Control`，不再走 `copyPassThroughHeaders`：`x-request-id`、`openai-processing-ms`、限速剩余额度等头在流式请求上全部消失，排障少了关键线索。

### N14-9（P2）`attemptFailoverReason` 把响应体当传输错误匹配

`provider_failover.go:1109-1114`：`isUpstreamStreamInterruption(errors.New(string(body)))`，`body` 是上游 HTTP 错误正文。中转报错里出现 `connection reset` / `unexpected eof` 这类字面就会把 429/400 标成 `stream_interrupted`，污染 `analysis.attempts`；真正的流中断走的是 `execErr`，**这个分支不会被调用到**。

### N14-10（P2，WIP）调试打印 + `prefix/stream` 抽象不成立

```go
// provider_failover.go:319-320（工作区，未提交）
fmt.Printf("ZZW2 fw=%p pl=%d buf=%d\n", w, w.prefixLen, w.buf.Len())
println("ZZW3", w.streamLen, w.buf.Len(), w.prefixLen, w.canDefer, w.passthrough)
```

- 每个 `Write` 一行、直连 stdout，跑测试时把输出全刷满；这是 round8/13 已报两次的同类问题（`println("DBG…")` 改成 `ZZW` 又回来了）。
- `Write` 现在**无条件** `w.buf.Write(p)`，passthrough 时再 `w.base.Write(p)` —— 流式响应会把全文再囤一份在 `buf` 里（与 N14-1 叠加，内存翻倍）。
- `prefixLen` 在「首次 Write 且 hasHeader」时置为当前 `buf.Len()`，而那时 buf 必为空 → `prefix` 恒空；自测 `TestFailoverWriterCutTurnClosesAfterCommittedPrefix` 报 `prefix ""`。前后两条自测（`…PrefixTracksCommittedTurn` / `…CutTurnClosesAfterCommittedPrefix`）与实现互相矛盾，说明这套「前缀随响应头发出」的模型还没想清楚。

### N14-11（P3）重复重试叠乘

`finishConvertedProxyWithEmptyStreamRetry` 自己有 `maxUpstreamEmptyStreamAttempts` 层重试（`protocol_proxy.go:533-570`），外面 failover 循环还有一层。中断被识别成 retryable 时两层会叠乘（最多 N×M 次上游调用），配额消耗放大。

---

## 三、与 magpie 的对照（同一问题的两种解法）

magpie 有 `internal/gateway/stream_cut_test.go`，处理同一个场景（上游连接在回答中途被切断），做法是**保持流式、把「连接断了」如实告诉客户端**，让客户端按可重试错误处理；它**不做**「已经流出一半再换账号重来」，也不整段缓冲（`grep io.ReadAll` 在它的流路径上无命中）。

本提交的选择相反：为了能换账号，把整条流压进内存直到确认完整。代价是**所有请求都失去流式**，而收益只覆盖「首包前就断」这一小类（首包前断本来就能靠空流重试覆盖）。

**结论**：`R4`（截断续传）这件事，magpie 的答案是「如实上报 + 让客户端重试」；我们若要保留服务端重试，只能在**首字节之前**做，不能靠整段缓冲。

---

## 四、上轮（round13）遗留复检

### 4.1 上一轮的三条 P0（D-1/D-2/D-3）

| # | 上轮问题 | 本轮结论 |
|---|---|---|
| D-1 | `recordsForRetry()` 恒真使 `deferBackup` 恒真 | 🟡 改法变了：现在用 `fw.canDefer` + 拦截器，不再靠类型断言撒谎；但拦截器对**所有**尝试生效（包括最后一次），语义从「谎称可重试」变成「一律缓冲」→ 见 N14-1 |
| D-2 | 缓冲状态双真源，正文泄漏 / 全吞 | 🟡 三条互斥报错没了（干净树 0 红），但状态源仍有两套（`deferred.wroteHeader` + `fw.passthrough`），WIP 又加了 `prefixLen/streamLen/hasHeader` 第三套 |
| D-3 | `deferBackup` 分支无心跳 | ❌ 仍在：`writeBufferedStream` 与拦截器路径都没有 `startSSEHeartbeat` |

### 4.2 历史问题总表（本轮复验）

判定方法：`34ec9a4` 只改了 15 个文件（清单见第一节），**不在改动清单里的问题文件 = 逐字节未变**；改动过的文件逐条 grep/读码复验。

#### 协议转换

| ID | 项目 | 状态 | 证据 |
|---|---|---|---|
| P1 | Claude→Chat 流式 usage 合并 | ✅ | 干净树测试绿 |
| P2 | `stop_reason` 全量映射 | ✅ | `convert_claude_openai.go:427-438` |
| P3 | Chat→Responses 保留工具前正文 | ✅ | 测试绿 |
| P4 | Responses→Chat reasoning 增量 | ✅ | 测试绿 |
| P5 | `incomplete`/`max_output_tokens` | 🟡 | 测试绿，但与新中断收尾语义重叠（都被 `length` 覆盖） |
| P6 | Chat→Claude block index | ✅ | 测试绿 |
| P7 | 工具 ID 哈希后缀 | ✅ | `convert_tools.go` |
| P8 | 合成 `tool_result` | ✅ | — |
| P9 | `redacted_thinking` / 反向 reasoning | 🟡 | 反向仍丢全部 |
| P10 | `strict`/`top_k`/`metadata`/`prompt_cache_key` | 🟡 | Claude 构造器仍无 |
| P11 | 单一 IR 架构 | ❌ | 架构级 |
| H-1 | `output_config.format` 带 `name` | ❌ | `convert_response_format.go:41`（文件未改） |
| H-2 | `json_object` 原样发 Claude | ❌ | `convert_response_format.go:19-20` |
| H-3 | adaptive 分支整体替换 `output_config` | ❌ | `convert_claude_openai.go:321-324` |
| H-4 | `allowed_tools` 塞进 `tool_choice` | ❌ | `convert_responses_claude.go:497-509` |
| H-5 | 缓存断点不含 `tool_result` | ❌ | `grep -c tool_result convert_claude_cache_marks.go` → 0 |
| H-6 | 思考余量补正生产不可达 | ❌ | `claude_pass_through_normalize.go:151` |
| H-7 | 非 5.2/5.3 `glm-*` 回落 `none` | ❌ | `claude_thinking_off.go` |
| H-8 | `knownClaude==false` 删 thinking | ❌ | 同上 |
| 假测试 | `TestClaudeToolChoiceWireShape` 恒真 | ❌ | `convert_tool_restrictions_test.go:127` |
| N11-1 | 长流 usage 用 256KB 头部副本 | 🔴 **未修 + 探针实证 0/0** | `passthrough_stream.go:340` |
| N11-2 | 心跳 goroutine 不 join | ❌ | `sse_heartbeat.go:38-44`（文件未改） |
| N11-3 | 透传 Claude 收尾漏 `content_block_stop` | 🔴 仍在 | `claudeStreamInterrupter` |
| N11-4 | 透传 Responses 收尾帧残缺 | 🟡 仍在 | `responsesStreamInterrupter` 缺 `id/model/sequence_number` |
| N12-1 | 567 → 65,536 输出 token 放大 | ❌ | `server.go:1330` 区域未被本提交触及 |

#### 路由 / 调度

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| R1 | RPM 限流 | 🟡 | 仅 per-provider lane |
| R2 | 冷却跟随 Retry-After / resets_at | ❌ | 本提交只加了 `attemptFailoverReason`，取 max / 复探缩回未动 |
| R3 | billing 不算硬额度 | ✅ | `6a1256b` |
| R4 | 截断续传 | 🔴 | 透传靠整段缓冲换来（N14-1）；转换路径判定恒 false（N14-2） |
| R5 | session pin 落库 | ✅ | — |
| R6 | per-account 闸门 + FIFO | 🟡 | `provider_lanes.go` 未改 |

#### 横切安全

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| S1 | 管理 API CORS | ✅ | — |
| S2 | 请求体上限 | ✅ | 256MB |
| S3 | 认证限流 | 🟡 | 先解码后限流、表无上限（`admin_auth.go` 未改） |
| S4 | 脱敏 | 🟡 | `redact*` 只覆盖凭证，响应正文未脱敏（M-19） |
| S5 | 首包后空闲超时 | ❌ | `Timeout: 0` 仍 11 处；本提交让「首包」直接变成「整段」，问题被放大 |
| N13-1 | `--version` 起服务 | ❌ | `cmd/gateway/main.go` 仍零 flag |
| N13-2 | `/__usage-analysis` 无上限 | ❌ | 未改 |
| N13-3 | 时区无容器兜底 | ❌ | 未改 |

#### Provider 账号

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| A1 | 刷新令牌加锁 | ✅ | `16c00d3`；锁表 3 瑕疵未收 |
| A2 | host 白名单 | ✅ | — |
| A3 | images 路由 / 全局 Key 回落 | 🟡 | H-9 判定错位（`server.go:4207` 用 `route.ProviderID`） |
| A4 | Claude 额度窗口 + 429 负缓存 | ❌ | `seven_day_overage` 命中 0 |
| A5 | Cursor 提前量 | ✅ | 内建 5 分钟 |
| N-1 | workbuddy 空 `expires_at` 永不续期 | ❌ | `workbuddy.go` 未改 |

#### 控制台

| 项目 | 状态 |
|---|---|
| 实时请求流 / 路由解释聚合 / 会话视图 / 一键重放 / 日志成本列 / 告警未读角标 | ❌ 6 项全未动（`web/` 本提交零改动） |

---

## 五、验收门槛（合入/上线前必须全过）

1. **回滚或还原流式**：`writePassThroughResponse` 的成功流不得走 `io.ReadAll` 全量缓冲；若保留缓冲，必须由显式开关控制，且默认关闭。（N14-1）
2. 心跳在「首包前」必须存在（旁路 SSE 注释），缓冲/拦截路径同样要有。（D-3）
3. 转换器的中断判定必须在**读完之后**求值（`interrupted := r.err == io.EOF && !sawClosingEvent`），并删掉 `isUpstreamStreamInterruptedReader` 这种「读前快照」写法。（N14-2）
4. `runStreamConverter` 的 Content-Length 判定必须比较实际读取字节数；否则删除该判定。（N14-3）
5. 收尾帧判定一律用**全文/尾部**，禁止对 `limitedBuffer`（头部副本）做收尾检查。（N14-4）
6. usage 解析必须用全文（`streamBody`），不得用 `tee.Bytes()`。（N14-5）
7. 「流被切断」不得直接计入 Provider 软失败；应单列原因并设阈值/不冷却。（N14-6）
8. `closeInterruptedClaudeStream` 去重 index，并按真实打开的块补 `content_block_stop`；透传路径同理。（N14-7 / N11-3）
9. 清空 `fmt.Printf("ZZW…")` / `println("ZZW…")`，并加 CI grep 守卫（`println(` / `fmt.Printf(` 在 `internal/gateway` 非 test 文件）。（N14-10）
10. 工作区 2 条自测必须先绿；`prefix/stream` 抽象要么实现成「真正随头下发的字节」，要么整段删除。
11. 端到端验证：用一条 60s 以上的真实长流，断言**客户端首字节 < 2s**（这是本提交最直接的回归指标）。

---

## 六、复现

```bash
# 1) 线上到底跑的是哪个提交
ssh root@173.249.207.202 'ls -l --time-style=+%F_%H:%M:%S /opt/llm-protocol-gateway/bin/gateway; \
  grep -c "upstream stream interrupted" /opt/llm-protocol-gateway/bin/gateway'

# 2) 已提交树 vs 工作区
git worktree add --detach .workbuddy/review-head14 34ec9a4
(cd .workbuddy/review-head14 && go test ./internal/gateway/)   # ok 38.8s
go test ./internal/gateway/ 2>&1 | grep -E '^--- FAIL'

# 3) 关键代码位置
sed -n '150,160p' internal/gateway/passthrough_stream.go   # 无条件走缓冲
sed -n '310,345p' internal/gateway/passthrough_stream.go   # io.ReadAll + tee.parse
sed -n '584,592p' internal/gateway/protocol_proxy.go       # Content-Length 恒真
sed -n '84,90p'   internal/gateway/convert_claude_openai_stream.go  # 读前快照
sed -n '315,322p' internal/gateway/provider_failover.go    # WIP 调试打印

# 4) 中断判定探针（临时文件，已删；重建方式）
#    streamClaudeToOpenAIChatEvents(rec, strings.NewReader(截断SSE), "m", nil)
#    → err=nil, finish_reason=stop, [DONE]  ⇒ 判定失效
```

---

## 七、副作用披露

1. 本地建过一次性 worktree `.workbuddy/review-head14`（已 `git worktree remove --force`），未改动工作区业务文件。
2. 建过一次性探针测试 `internal/gateway/zz_r14_probe_test.go`（6 个用例，**已删除**，`git status` 无残留）。
3. 对 VPS 只读：`systemctl show`、`ls -l`、`grep -c` 二进制、`journalctl`（无匹配输出）。未重启、未部署、未改配置。
4. 期间对方 agent 仍在编辑工作区（指纹 08:26 `4f18fe4c…` → 08:58 `c95030a0…`），本报告以 08:58 快照为准。
