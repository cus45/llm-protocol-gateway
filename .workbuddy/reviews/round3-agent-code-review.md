# 第三方 agent 改动审查（第 3 轮）· 2026-10-22 14:20

审查范围：`25a95b2` 之后的工作区未提交改动（4 改 + 1 新增），对照 magpie 验收。
未修改任何代码，未提交。

## 结论速览

| 项 | 状态 |
|---|---|
| 主逻辑（流式 stop_sequence 回填） | 设计正确，可保留 |
| 新增 2 个测试 | **均失败，不可提交** |
| 新增调试文件 `stop_sequence_tail_test.go` | **应删除**（纯日志脚手架） |
| `tailForStopSequences` 空序列分支 | **有性能缺陷**（无 stop 时全量累积 + 每 chunk 转 rune，O(n²)） |
| 上轮头号遗留：stop 发进 Responses | **仍未修，仍在两处** |
| 上轮遗留：误报 stop_sequence 覆盖 length | 仍未修 |
| `convertedStopSequences()` | 死代码（定义后无人调用） |

实测：`go build ./...` 通过；`go test ./internal/gateway/` **2 项失败**（其余 1000+ 通过）。

---

## 一、未提交改动逐项审查

### 1. `internal/gateway/stop_sequence_fill.go` — 新增 `tailForStopSequences`

```go
func tailForStopSequences(text string, sequences []string) string {
	if len(sequences) == 0 {
		return text          // ← 问题
	}
	...
	keep := longest + 1
```

**问题 A（性能，影响主路径）**：`len(sequences)==0` 时原样返回全文。流式里
`visibleText = tailForStopSequences(visibleText+text, clientStopSequences)` 对每个
text delta 都调用一次，而绝大多数请求根本不带停止序列 → `visibleText` 累积整篇正文，
且每个 chunk 都做一次 `[]rune(全文)` 转换 → **总复杂度 O(n²)**，长输出（几十 KB 以上）
会有明显 CPU 与内存开销。这是加在一个本来零成本的热路径上的。

修法：`len(sequences)==0` 时返回 `""`（且流式侧可直接跳过整个跟踪）。

**问题 B（语义）**：`keep = longest + 1` 里的 `+1` 多余但无害；保留 `longest` 即可判定
`HasSuffix`。

### 2. `internal/gateway/convert_openai_claude_stream.go` — 流式回填

签名加 `clientStopSequences ...string`，在 text delta 处维护 `visibleText`，
`message_delta` 用 `claudeStopReasonWithSequence` 输出 `stop_reason`/`stop_sequence`。
**方向正确**：Chat 只有 `finish_reason=stop`，跨协议确实丢掉了 Anthropic 的
`stop_sequence` 语义；只在"转换产生的序列"上回填、不碰直通请求，这个约束也对。

**问题 C（误报，沿用上轮）**：`claudeStopReasonWithSequence` 命中即无条件返回
`"stop_sequence"`，会覆盖 `max_tokens`/`length`。若停止序列较短（如 `"\n\n"`），
被截断的正文末尾恰好命中就会把"超长截断"报成"正常停止"，客户端不再续写。
非流式的 `claudeResponseToOpenAIChat`（convert_claude_openai.go:476-479）同样如此。
修法：仅当原 reason ∈ {`end_turn`, `stop`, `""`} 时才覆盖。

### 3. `internal/gateway/server.go:5162` — 传参

`streamOpenAIChatToClaudeEvents(..., clientStopSequences...)`，调用点与唯一定义匹配，正确。

### 4. 两个新增测试 — **都跑不过**（实测输出见下）

`TestTailForStopSequences`
- 期望 `tailForStopSequences("一二三四五", []string{"\x0a"}) == "二三四五"`（4 字）
- 实际 `"四五"`：longest=1 → keep=2 → `runes[3:]`。
- **期望值写错**，与函数语义自相矛盾（"只留最长序列所需长度"不可能留 4 字）。
  改期望为 `"四五"` 即可。

`TestChatToClaudeStreamReportsStopSequence` — 两处硬伤
- **转义**：SSE 用 Go 原始字符串写 `"content":"1\\n2\\n"`，JSON 解出的是**字面反斜杠+n**
  （6 字符），不是换行。实测输出 `{"delta":{"text":"1\\n2\\n"}}` 可证。要真换行须写 `"1\n2\n"`。
- **序列本身对不上**：客户端序列是 `"\x0a6"`（LF+`6`），正文 `1\n2\n` 结尾只有 LF、没有 `6`，
  即使转义修好也永远不匹配。应改成序列 `"\x0a"`，或把正文写成 `...2\n6`。
- 结果：断言 `"stop_reason":"stop_sequence"` 永不成立，实际输出仍是 `end_turn`。

### 5. `internal/gateway/stop_sequence_tail_test.go`（未跟踪）— 应删除

只有一个 `TestTailKeep8`，里面手抄了一遍求最长序列的逻辑再 `t.Logf` 打印，无断言、无价值，
是排查时的脚手架。留在仓库会污染测试集，且和 `TestTailForStopSequences` 重复。

---

## 二、上轮头号遗留：仍未修 —— `stop` 仍在发往 Responses 上游

实测当前代码两处仍在写 `stop`：

- `internal/gateway/convert_claude_responses.go:283`
  （Claude `stop_sequences` → `responsesReq["stop"]`，`proxyClaudeToResponses` 路径）
- `internal/gateway/convert_openai_responses_chat.go:438`
  （Chat `stop` → `responsesReq["stop"]`，`proxyResponsesToOpenAIChat` 路径）

**对照 magpie 的验收结论（已核对源码）**：
`internal/gateway/responses.go` 的 `buildResponses()`（1089 行起）输出字段为
`text / prompt_cache_key / instructions / max_output_tokens / temperature / top_p /
service_tier / reasoning / client_metadata / access_programs` —— **全文没有 `stop`**。
magpie 只在 `chat.go:487`（Chat Completions 编码器）写 `out["stop"]`。
即：magpie 对 Responses 上游**直接丢弃**停止序列，不是映射。

后果：Responses API 无 `stop` 参数，发出去必 `Unknown parameter: 'stop'`；
而 `sampling_rectifier.go` 的待整流字段只有 `temperature/top_p/top_k`，**不含 stop**，
所以这条路径没有重试兜底 —— 客户端带 stop 打 Responses provider 就是硬 400。

修法（二选一，推荐前者）：
1. 对齐 magpie：两个 Responses 编码器里**删掉** stop 赋值（并保留回填逻辑，让响应侧
   仍能报出 stop_sequence —— 此时序列没发给上游，靠 `rememberConvertedStopSequences`
   记录的原值在响应里判定，语义一致）。
2. 或把 `stop` 加进整流器字段表，靠 400 后重试补救（治标，且多一次往返）。

另：`stop_sequence_fill.go:35` 的 `convertedStopSequences()` 定义后全仓无调用点，是死代码。

---

## 三、下一步最小修复清单（按序）

1. 删 `stop_sequence_tail_test.go`。
2. 修 `TestTailForStopSequences` 期望值 `"四五"`。
3. 修流式测试：正文改用真换行、序列改成 `"\x0a"`（或正文末尾加 `6`）。
4. `tailForStopSequences` 空序列返回 `""`。
5. `claudeStopReasonWithSequence` 与 `claudeResponseToOpenAIChat` 只在
   `end_turn`/`stop` 时才覆盖为 `stop_sequence`。
6. 两个 Responses 编码器删除 `stop` 赋值（对齐 magpie），补一条
   "Chat/Chat 带 stop → Responses provider 不产生 stop" 的测试。
7. 以上全绿后再 `go test ./internal/gateway/...` + 提交 + 上 VPS 验证。
