# 第四轮 · 另一 agent 改动代码审查

审查范围：`903d923`（流式 Chat→Claude 回填 stop_sequence）、`4157aea`（直通上游 stop_sequences 整流），
外加第三轮遗留问题的复检与 RPM 限流（28c7034 系列）复核。
对照基准：`C:\guodevelop\demo\magpie`。
基线：`go build ./...` 通过，`go test ./internal/gateway/...` 全绿。
本轮未修改仓库任何代码（临时验证测试已运行后删除，工作区除 agent 自身在途文件外干净）。

---

## 结论先说

1. 两个新提交**方向正确、测试到位**，但各带一个真实缺陷。
2. **新发现一个功能级 bug（比第三轮头号遗留更常见）**：正文以客户端停止序列结尾后又发起工具调用时，
   `stop_reason` 被从 `tool_use` 改写成 `stop_sequence` → Claude 客户端不会执行工具调用。已实测复现。
3. **第三轮头号遗留仍未修**：`stop` 仍然被发进 Responses 上游。已用临时测试实锤，并再次对照 magpie 坐实。
4. `4157aea` 的「上限 4 条」是**偏离 magpie 的自创规则**，且作用范围把真实 Anthropic api_key Provider 也圈进去了。
5. agent 正在实时编辑两个未提交文件（Claude thinking-off 整流），其 port 与 magpie `thinkingOffOf` 逐条对齐，尚未完整。

---

## 一、903d923 流式 Chat→Claude 回填 stop_sequence

### 正确的部分
- 只在流式正文尾部保留 `longest+2` rune 的窗口，避免整段累积（意图正确）；
  `tailForStopSequences` 按 rune 计算、不会切断多字节字符（`TestTailForStopSequences` 覆盖中文）。
- 跨 delta 拆分到达的序列能正确拼出（`"1\n2\n6"` + 序列 `"\n6"` 用例）。
- 没有客户端序列时保持原 `end_turn`（`stop_sequence: null`）。
- 心跳、块关闭顺序（P6）等既有修复未被破坏。

### 缺陷 1（高，功能级）：无条件覆盖 stop_reason

位置：`convert_openai_claude_stream.go:314`、`convert_openai_claude.go:382`

```go
stopReason, stopSequenceValue = claudeStopReasonWithSequence(stopReason, visibleText, clientStopSequences)
```

`stopReason` 此时已由 `mapOpenAIFinishReasonToClaude` 得到（`tool_calls→tool_use`、`length→max_tokens`、
`content_filter→refusal`、`pause_turn→pause_turn`），而 `claudeStopReasonWithSequence` 只要尾部命中就把
**任何**原因改写成 `stop_sequence`。

实测（临时测试，已删）：Chat 流 = 正文 `"done END"` + 一个 tool_call，`finish_reason=tool_calls`，客户端序列 `["END"]`：

```
event: message_delta
data: {"delta":{"stop_reason":"stop_sequence","stop_sequence":"END"}, ...}
```

正文里明明有 `tool_use` 块，报出来却是「按停止序列正常结束」。Claude Code 会据此认为回合已结束，
**不会执行工具调用**。同一测试在 `finish_reason=length` 下也把 `max_tokens` 改成了 `stop_sequence`，
截断被伪装成正常结束（等于把 P2/P5 修好的东西又开了一个口子）。

修复（一行）：只在 `stopReason == "end_turn"`（上游确属正常结束）时才改写，其余原因原样保留。

### 缺陷 2（中，性能）：无停止序列时仍然全量累积

`tailForStopSequences` 的 `if len(sequences) == 0 { return text }` 返回的是**全文**；
调用点是 `tailForStopSequences(visibleText+text, ...)`。绝大多数请求不带停止序列，
于是每个 chunk 都做一次「旧全文 + 新 chunk」的字符串拼接 → 整条流 O(n²) 拷贝 + 常驻全量正文。
（第三轮已指出，本轮未改。）

修复：`len(sequences) == 0` 时返回 `""`。

### 缺陷 3（低，沿用 25a95b2）：incomplete 被强行改成 completed

`stop_sequence_fill.go` 的 `fillNonStreamStopSequence`：Responses 答案只要尾部命中就把
`payload["status"]` 置为 `completed`。若上游本就是 `incomplete`（`max_output_tokens` 截断）
而正文恰好以停止序列结尾，截断会被报成完成 —— 与缺陷 1 同类。建议同样只在 `completed` 时回填。

---

## 二、4157aea 直通上游 stop_sequences 整流

### 正确的部分
- 剔除非字符串项、空串，避免对象项让整个请求 400、空串让流在第一个 token 前就停 —— 动机合理。
- `sanitizeClaudeStopSequencesBody` 只在「确有改写」时才重新 marshal（`before == after` 直接返回原 body），
  干净请求零开销、零字节变化；非 JSON / 字段不存在时原样返回。单测覆盖五种形态，质量好。
- 作用点选在 `sendClaudeMessagesUpstream`（`/v1/messages` 直通的唯一发送口），没有漏发路径。

### 缺陷 1（中）：作用范围与「4 条上限」偏离参考实现

- 条件是 `!isOAuth`，即**所有非 Claude-OAuth 的 Claude Provider 都被整流**，包含
  `authType=api_key` 且 base URL 指向 `api.anthropic.com` 的真实 Anthropic 账号。
- 「超过 4 条被直接拒绝」：magpie `anthropic.go:961` 是 `if len(r.Stop) > 0 { out["stop_sequences"] = r.Stop }`，
  **原样透传、不设上限**；Anthropic 官方文档对 `stop_sequences` 也无明确条数上限
  （vLLM 那边的 4 条限制来自它内部转成 Chat 的实现，不是 Anthropic 的契约）。
  静默丢弃第 5 条会改变语义：模型不再在客户端指定的位置停下，且客户端无从得知。

建议：把「上限 4」限定到确需的非 Anthropic 上游（按 host 判定，或改为配置），
真实 Anthropic 保持透传；或干脆去掉上限，只保留「剔除非字符串 / 空串」（这部分无损且有用）。

### 缺陷 2（低）：整体 json 重排的副作用

一旦触发改写，整个 body 走 `Unmarshal → Marshal`：`键序变为字典序`、`&<>` 被转义成 `\u0026` 等、
超过 float64 精度的整数会失真。上游解码后语义等价，属可接受代价；但如果上游对 body 做签名校验
（AWS Bedrock / Vertex 的 SigV4）就会直接失败 —— 目前 Claude 直通不走这两家，记录备查。

---

## 三、第三轮头号遗留：仍未修 —— stop 被发进 Responses 上游

位置（两个都是实时请求路径）：

| 位置 | 触发 |
|---|---|
| `convert_claude_responses.go:283-290`（`claudeToResponsesRequestDirect`，经 `protocol_proxy.go:300` 调用） | Claude 请求带 `stop_sequences` 路由到 Responses Provider |
| `convert_openai_responses_chat.go:438-439`（`openAIChatToResponsesRequest`，经 `protocol_proxy.go:269` 调用） | Chat 请求带 `stop` 路由到 Responses Provider |

实测（临时测试，已删）：

```
claude→responses must not send stop, got "END"
```

magpie 对照（决定性）：`responses.go:1089 buildResponses` **全文没有 `stop` 字段**；
同一份 `r.Stop` 在 `chat.go:486`（`out["stop"]`，Chat 合法）和 `anthropic.go:961`
（`out["stop_sequences"]`，Anthropic 合法）都发，唯独 Responses 不发。
OpenAI Responses 收到 `stop` 必报 `Unknown parameter: 'stop'`，且我们的采样参数整流器
（`sampling_rectifier.go`，只处理 temperature/top_p/top_k）不覆盖 `stop`。

**这是连续第三轮标记、仍未修的最高优先级项。** 修复量很小：删掉这两处赋值，
把序列照样记进 `rememberConvertedStopSequences`（已有机制），回填逻辑不受影响。

---

## 四、RPM 限流（28c7034 / 3a2ba70 / 562878e / 7c6f8df）

R1 已落地，方向对（每 Provider 每分钟预算 + 并发闸门 + 队列等待 + 控制台可配 + 落库），
但 `provider_lanes.go:177-192 admitLocked` 是**固定窗口**：

```go
if lane.windowStart.IsZero() || now.Sub(lane.windowStart) >= time.Minute {
    lane.windowStart = now; lane.used = 0
}
```

跨窗口边界可放行 2× 预算（12:00:59 打满 + 12:01:00 再打满），而上游（Anthropic/OpenAI）普遍按
滑动窗口或令牌桶计 —— 边界突发正是最容易吃 429 的时刻。建议改成滑动窗口计数或令牌桶。
另：只被 RPM 挡住的请求不会在窗口翻转时被唤醒（`waitFor` 只等 release 广播），
只能等 deadline 超时失败，而不是等窗口重置后放行。

---

## 五、agent 在途未提交文件（本轮只做只读核对）

| 文件 | 状态 |
|---|---|
| `internal/gateway/claude_thinking_off.go`（新增，181 行） | 未提交、仍在编辑 |
| `internal/gateway/claude_pass_through_normalize.go`（已改，+4 行） | 未提交 |

`claudeThinkingOffOf` 是 magpie `anthropic.go:537 thinkingOffOf` 的移植，逐条核对**忠实**：

| 规则 | magpie | 我们 |
|---|---|---|
| fable / mythos 5.x | offNever | claudeOffNever ✅ |
| major < 5 | offTaken | claudeOffTaken ✅ |
| 5.0 opus | offUpToHigh | claudeOffUpToHigh ✅ |
| 5.0 sonnet / haiku | offTaken | claudeOffTaken ✅ |
| 5.5 sonnet | offBetweenTools | claudeOffBetweenTools ✅ |
| 5.5 opus | offNever | claudeOffNever ✅ |
| 5.5 haiku | offUpToHigh | claudeOffUpToHigh ✅ |
| 其余 / 未知 | offUnknown（原样透传） | claudeOffUnknown ✅ |

`classifierRoom = 2048` 也对得上 magpie 的同名常量。`go build ./...` 在当前在途状态下通过。
未完成部分（`normalizeClaudeThinkingOff` 的 between_tools / never 分支）建议等提交后再审。

---

## 六、建议修复顺序（每项单独提交 + 单测 + 上 VPS）

| 优先级 | 项 | 位置 | 规模 |
|---|---|---|---|
| P0 | 回填不得覆盖 `tool_use` / `max_tokens` / 其它非 end_turn 原因 | `stop_sequence_fill.go:121`、`convert_openai_claude.go:382`、`convert_openai_claude_stream.go:314` | 1 行 + 2 用例 |
| P0 | 删除发往 Responses 的 `stop`（序列改记 context） | `convert_claude_responses.go:283-290`、`convert_openai_responses_chat.go:438-439` | 删 2 段 + 2 用例 |
| P1 | 无停止序列时 `tailForStopSequences` 返回空串 | `stop_sequence_fill.go:165` | 1 行 |
| P1 | `fillNonStreamStopSequence` 仅在 `completed` 时回填 | `stop_sequence_fill.go:78-83` | 3 行 |
| P2 | RPM 固定窗口 → 滑动窗口 / 令牌桶；窗口翻转唤醒等待者 | `provider_lanes.go:177-192` | 中 |
| P2 | stop_sequences 整流收窄到非 Anthropic 上游，去掉「4 条上限」 | `stop_sequence_fill.go:194-220` | 小 |
| — | Claude thinking-off 整流提交后复审 | `claude_thinking_off.go` | 待提交 |
