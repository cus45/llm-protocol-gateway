# 第 25 轮审查 — 另一 agent 的改动是否正确

- 审查时间：2026-10-23 16:40 (+0800)
- 范围：HEAD `8b5542d` + 未提交工作区（47 改 / 6 新增）
- 对照项目：`C:\guodevelop\demo\magpie`
- 结论：**仍不可合入**。本轮零提交、工作区与 round24 逐字节一致；但首次拿到「线上 A/B 实证」——**部署后同协议透传的流式应答被整段缓冲**，`lat-ttft` 从部署前的 2509ms 掉到 **0ms**，最长一条 20.8s 的答案一次性吐出。同时纠正 round24 两处结论：触发条件不是「配了备用链」，范围也没有「全部 Key」那么宽（转换路径仍真流式）。

---

## 一、基线与「零改动」证据

| 项 | 结果 |
|---|---|
| HEAD | `8b5542d` fix(lanes): RPM 改为滑动分钟窗口，纯限流 Provider 可排队（13:00:11 +0800） |
| 工作区 | 47 改 / 6 新增；未暂存 `+2424 / −401`，暂存 6 个新文件 `+842` |
| 指纹 `sha256(git diff)` | `f86d25eb…2a5ebebc` — **与 round24 逐字节一致** |
| 最新 mtime | 13:35:34（`provider_lane_fifo_test.go`），静默 **3h03m** |
| `go build ./...` / `go vet ./internal/...` | 绿 |
| `go test ./internal/gateway/ -count=1` | **ok 39.044s，0 红** |
| `gofmt -l` | `store/sqlite.go`、`store/session_pins_test.go`、`cmd/gateway/main.go`；`main.go` 与 HEAD 无差异 ⇒ 工作区 CRLF 所致，非排版问题 |
| 仓库卫生 | `internal/gateway/*.bak` 仍是 5 个（chatgpt_oauth / claude_oauth / cursor_oauth / qoder_pat / qoder_pat_handlers） |
| 未跟踪 | 仅 `.superpowers/brainstorm/…`（16:56 由宿主工具生成，与本仓库无关） |

生产侧（只读检查，未发起真实模型请求）：

| 项 | 结果 |
|---|---|
| 服务 | `llm-protocol-gateway.service` **active**，`ActiveEnterTimestamp` = 2026-10-22 22:39:45 PDT |
| 二进制 | `/opt/llm-protocol-gateway/bin/gateway`，mtime `2026-10-22 22:24:57 PDT` = **13:24:57 +0800** |
| 特征串 | `isMyTurnLocked` PRESENT / `streamEndReader` PRESENT / `upstream_stream_interrupted` PRESENT / `strikeLocked` **ABSENT** ⇒ **线上是 13:24 那版未提交 WIP，13:35 的 lane 重写没上线**（连续第 7 轮） |
| 规模 | Provider `deleted=0` **18 个**（round24 记的 44 是含回收站的总行数，本轮回滚该数字）；API Key 9 个，其中 **5 个配了备用链**（luca-glm / work buddy / wb / wb-luoxiwen-claude / luca-workbuddy） |
| 限流是否启用 | 18 个 Provider 的 `requests_per_minute` / `max_concurrent_requests` **全为 0** ⇒ lane 未启用，死锁版仍未有机会爆 |

---

## 二、本轮核心取证：透传流式在线上被整段缓冲（A/B 实证）

### 2.1 生产数据（`request_logs`，只读 SQL）

筛选：`status=200`、`request_body like '%"stream":true%'`、`ttft_ms>0`。`ttft_ms` 由最外层 `ttftResponseWriter` 在**首次写客户端**时打点（`ttft_writer.go:30-34`），心跳 SSE 注释被排除，因此 `latency−ttft` 就是「首字节之后又等了多久」。

| 分组 | 部署前（< 13:30 +0800） | 部署后（≥ 13:30 +0800） |
|---|---|---|
| `action='pass_through'` | n=81，avg(lat−ttft)=**2509ms** | n=24，avg(lat−ttft)=**0ms**，单条最长 20776ms |
| `action='convert'`（workbuddy-订阅-8） | — | n=413，avg(lat−ttft)=**832ms** |

**读法**：部署前透传是真流式（首字节后再陆续吐 2.5s）；部署后 24 条**无一例外**首字节等于末字节（`lat−ttft` 恒 0），最长的一条 20.8s 答案在最后一次性发出。转换路径（workbuddy 适配器自写 SSE）没受影响，avg 832ms。

### 2.2 本地复现（探针已删，指纹已复原）

| 探针 | 路径 | 结果 |
|---|---|---|
| 25K | 假上游本身 | `firstRead=1ms` ⇒ 上游确实在流 |
| 25L | 直接调 `proxyClaudeMessages`（无 failover writer） | `total=645ms firstByte=2ms writes=9` ⇒ **真流式** |
| 25M | 走完整 `handleClaudeMessages` + 真实 TCP 连接 | `firstRead=647ms`，一次性 64B 起 ⇒ **整段缓冲** |
| 25N | Claude 客户端 ← Chat Provider（转换路径） | `firstRead=2ms` ⇒ **转换路径仍真流式** |

### 2.3 结论与两处纠正

- **代码位置（两处，均在未提交工作区与 HEAD 中都有）**
  - `passthrough_stream.go:246` — `if fw, ok := w.(*failoverResponseWriter); ok && stream && response.StatusCode < 400 { return writeBufferedStream(...) }`，不判 `canDefer`；`:407` 直接 `io.ReadAll(io.TeeReader(...))`。
  - `writeBufferedStream` 全程**不启心跳**（只在整段读完、`:434` 才建 `hw`），也不设上限。
- **纠正 round24 的触发条件**：不是「配了备用链」。`apiKeyProviderChain`（`router.go:1133-1152`）**总是**把首选 Provider 放进链里 ⇒ `len(chain) ≥ 1` 恒成立 ⇒ 任何用网关 Key 认证的请求都走 failover 循环，`writer` 恒为 `*failoverResponseWriter`，于是**是否配备用链无关**。round24 的 25J 等价场景（无备用链）同样被缓冲。
- **纠正 round24 的影响面**：不是「所有 Key 的流式都废了」。转换路径仍然流式（25N 2ms；生产 convert 832ms），受影响的是**同协议透传**（`action='pass_through'`，如 Claude 订阅账号、Claude API-key 中转）。
- **残留影响**：TTFT=完整生成；生成期零心跳；内存 ≈ 整段×2（`io.ReadAll` + 下游再缓冲）；20.8s 单请求已在线上发生。
- **magpie 对照**：`gateway.go:1396` 是 `io.Copy` 直通，不做 buffer-then-replay；其 `stream_cut_test.go` 也坚持把切断如实报给客户端。

---

## 三、本轮新发现

### N25-1（P1）中断判定谓词语义反了：`io.EOF` 是「正常结束」

`stream_interruption_close.go:21-23`：

```go
func isUpstreamStreamInterruptedReader(r countingReader) bool { return r.err == io.EOF }
```

- `countingReader.err` 记录的是 Read 返回的错误（`empty_stream_retry.go:127-133`）。HTTP body 读到**正常结束**就是 `io.EOF`；真正被中途切断（chunked 缺终结块）是 `io.ErrUnexpectedEOF`。
- 所以这个谓词表达的是「流正常结束」，名字与用法却取反。round24 只报了「调用点在读取前按值快照 ⇒ 恒 false」；本轮补充：**就算把调用点移到循环之后，它会变成恒 true**，5 个调用点会一起把正常流判成截断。正确写法应是 `errors.Is(r.err, io.ErrUnexpectedEOF)`（并保留 `err != nil && err != io.EOF` 的传输错误分支）。
- 5 个调用点（均在 `for scanner.Scan()` 之前）：`convert_claude_openai_stream.go:87`、`convert_claude_responses_stream.go:253`、`convert_openai_claude_stream.go:126`、`convert_openai_responses_chat_stream.go:576`、`:815`。

### N25-2（P1）同一个问题，两个函数给出相反结论

- `runStreamConverter`（`protocol_proxy.go:590-592`）：只要 `response.ContentLength > 0` 且转换器没报错，就**判为截断**，从不与实读字节比较。
- `isUpstreamStreamInterruptedResponse`（`empty_stream_retry.go:88-96`）：`ContentLength >= 0` 时**判为完整**。

探针 PROBE-25C：body 85 字节、`Content-Length: 85`（**完全相等**）⇒ `runStreamConverter` 返回 `upstream stream interrupted after 85 bytes: body shorter than its Content-Length 85`。即完整流被判截断、错误文案还自相矛盾。同一个问题在全仓有两套互斥判定，必须合并成一个（建议以「读到 `io.ErrUnexpectedEOF`」或「无收尾帧」为唯一依据）。

### N25-3（P2）`streamEndReader` 在非 Responses 流上全量累积 + 每块重扫

`stream_end_reader.go:98-109`：`scanResponses` 只在拿到 `id` 和 `model` 后早退，而这两个值只从 `response.created` 取。Claude / Chat 透传流永远不会出现该事件 ⇒

- `tailBytes` 一直 append 到 `2*256KB` 上限，**每个 chunk 都重新 `bytes.Contains` 最多 512KB**；
- `scanClaude` 还会对**每条 data 行**做一次 `json.Unmarshal`（Chat 流也照做）。

PROBE-25A（1KB 分块喂入）：Chat 1289KB → `tailBytes=267342`、60ms；Claude 1933KB → `tailBytes=401023`、79ms。CPU 尚可接受，但这是每条透传流的固定额外内存（~0.5MB）+ 平方级扫描，属于「为了 Responses 的收尾 identity，让所有协议都付钱」。建议按 `Content-Type`/首帧协议分派 scanner，而不是三个 scanner 全跑。

### N25-4（P2）流在 `message_start` 之前被切断 → 发出非法事件序

`passthrough_stream.go:33-43`：`!cut.messageStarted` 时直接写 `message_delta` + `message_stop`。Anthropic 流要求 `message_start` 先行，SDK 的 `MessageStream` 在没收到 `message_start` 时会因无法组装 message 而报错。此时客户端本来就只收到 0 个事件，正确做法是补一个最小 `message_start`（空 content、`stop_reason` 后续在 delta 里给），或者干脆不发收尾帧只断开。同类问题在 Responses 侧不存在（`response.incomplete` 自解释）。

### N25-5（P2）两条缓冲路径全程零心跳

- `writeBufferedStream`（`passthrough_stream.go:392-441`）：只在 `:434` 建 `hw`，**从未 `startSSEHeartbeat`**（对比非缓冲分支 `:281`）。
- `finishConvertedProxy` 的拦截分支（`protocol_proxy.go:623-643`）：同样没有心跳，心跳只存在于 `:650` 的 else 分支。

后果：只要走缓冲，长思考/长生成期间客户端与中间代理（Cloudflare、nginx `proxy_read_timeout`）看到的是纯静默。当前 VPS 是直连 18093 暂时没被切，但这条路径一旦经 CF 暴露就是必然的超时断连。

### N25-6（P3，并纠正 N24-3）收尾标记的携带窗口只有 4 字节，失败点比 round24 报的窄

`stream_end_reader.go:19`（`tail [4]byte`）+ `:90-91`：跨块时只带 4 字节前缀。PROBE-25E 精确扫描 `[DONE]`（6 字节）的 8 种切分：

| 第二块前缀 | `closed()` |
|---|---|
| `\n` / `\n\n` / `E]` / `NE]` / `ONE]` / `DONE]` / `[DONE]` … | true |
| **`]\n\n`（第一块刚好以 `[DONE` 结尾）** | **false** |

即**只有「第一块正好停在标记最后一个字符前」这一种对齐会漏判**（对 12 字节的 `message_stop` 同理，1/12 对齐）。round24 记的「5/9/12B→false」来自不同构造，量级偏大；真实概率 ≈ 每次跨越标记 1/标记长度，属 P3 而非 P2。修法很简单：`tail` 长度取 `len(最长标记)-1`（11 字节），或直接把 hold 提到 64。

### N25-7（P2，复核加强）完整流被判截断的「回声」问题在 Claude/Responses 侧仍在

`closingMarkers` 里 `message_stop` / `response.completed` / `response.incomplete` **没有回声守卫**（只有 `[DONE]` 有 `markersRejectingEcho`）。PROBE-25F：一个**被真正截断**的 Claude 流，只要已输出的正文里出现 "the stream ends with message_stop"，`closed()` 就返回 true ⇒ 不补收尾帧，客户端挂在半截。PROBE-25D 同样证明 `response.completed` 出现在正文里会被判完整。`closesUpstreamStream`（`stream_interruption_close.go:36-44`，末 4096 字节 `strings.Contains`）有同样问题，且与 `streamEndReader.closed()` 结论可以相反。

---

## 四、前 24 轮问题复核清单（本轮实测）

判定依据：**零提交 + 指纹与 round24 逐字节一致** ⇒ 未被我重新取证的条目，代码不可能变化；下表凡是标 ❌ 的都已在本轮重新定位行号。

### 4.1 协议转换

| # | 问题 | 状态 | 本轮证据 |
|---|---|---|---|
| P1 | Claude→Chat 流式 usage 丢 0 | ✅ | 25G 探针：usage=1234/1 正确回填 |
| P2 | stop_reason 不全量映射 | ✅ | — |
| P3 | Chat→Responses 有 tool_calls 时正文清空 | ✅ | — |
| P4 | Responses→Chat 丢 reasoning | ✅ | — |
| P5 | incomplete/max_output_tokens 当正常结束 | 🟡 | 透传侧修；转换侧受 N25-1 影响仍漏 |
| P6 | Chat→Claude block index 错乱 | ✅ | — |
| P7 | tool_use ID 归一化碰撞 | ✅ | — |
| P8 | Responses→Claude 缺合成 tool_result | ✅ | — |
| P9 | redacted_thinking / reasoning 丢失 | 🟡 | 占位仍在 |
| P10 | strict/top_k/stop_sequences/metadata 不转发 | ✅ 多数 | `stop` 进 Responses 已堵 |
| P11 | 两两手写转换 vs magpie 单一 IR | ❌ | 架构性，未动 |
| P12 | 中断收尾帧 identity/sequence 不可靠 | ❌ | N25-3 同源 |
| P13 | 完整流误判截断（4B 携带） | ❌ | N25-6 精确化 |
| P14 | `stream=true` + JSON 回包 usage=0 | ❌ | 本轮未复测，代码未变 |
| **N25-1** | 中断判定谓词语义反了 | ❌ 新 | `stream_interruption_close.go:21-23` |
| **N25-2** | 两套 Content-Length 判定互相打架 | ❌ 新 | 25C 探针实测相等长度也判截断 |
| **N25-7** | 收尾标记无回声守卫 | ❌ 新 | 25D / 25F |

### 4.2 路由 / 调度

| # | 问题 | 状态 | 备注 |
|---|---|---|---|
| R1 | 无 RPM 限流 | ✅ | `8b5542d` 滑动窗口（生产 rpm 全 0，未启用） |
| R2 | 冷却不看上游 | 🟡 | 阶梯可达；`401+hardAuth` 15min 偏长 |
| R3 | billing 归进硬额度 | ✅ | — |
| R4 | 流截断无法续流 | ❌ | 25G/25H：截断流对客户端是 `finish_reason=stop`+`[DONE]` / `end_turn`+`message_stop`，`err=nil` ⇒ 不重试、不计失败 |
| R5 | 会话粘性只在内存 | ✅ | `session_pins.go` 落库 |
| R6 | 并发闸门非 FIFO / 非每账号 | 🟡 | 已排队者 FIFO，新到可插队 |

### 4.3 安全

| # | 问题 | 状态 | 备注 |
|---|---|---|---|
| S1 | 管理 API `ACAO: *` | ✅ | `cors.go` 白名单 |
| S2 | 请求体无上限 | ✅ | `request_body_limit.go` |
| S3 | 登录/注册/setup 无限流、首个远程可设密码 | 🟡 | 有限流 + 实例守卫，仍靠 Host 判回环 |
| S4 | 正文不脱敏入库 | ✅ | 请求+响应双向 |
| S5 | 上游头返回后无空闲超时 | 🟡 | 覆盖面 20 处；120s 硬编码；`upstream_timeout.go:120-124` nil timer panic 仍在（round22 V22-3） |

### 4.4 Provider 账号

| # | 问题 | 状态 |
|---|---|---|
| A1 | Trae/WorkBuddy/Cursor 刷新无锁 | ✅ 统一 `oauth_renew_lock.go` |
| A2 | 子串匹配 host 泄露 Key | ✅ `baseURLHostIn` 白名单 |
| A3 | `/v1/images/*` 回落全局 Key | 🟡 拒绝逻辑已加（`server.go:4312`），`resolveProviderImageURL` 仍盲拼 |
| A4 | Claude 漏 overage / 429 无负缓存 | ✅ |
| A5 | Cursor 无过期提前量 | ✅（round10 已纠错） |
| A6 | workbuddy_ai 永不过期 | ✅ 401 自愈 + `SignInAt` |

### 4.5 round21 的九项高危（H-1…H-9，抽样复核）

| # | 问题 | 状态 | 证据 |
|---|---|---|---|
| H-1 | `output_config.format` 带 `name` → Anthropic 400 | ❌ 仍在 | `convert_response_format.go:41-43` 仍 `format["name"] = name`（magpie `anthropic.go:957` 只发 `{type,schema}`） |
| H-2 | `json_object` 原样发 Claude | 🟡 | 现映射进 `output_config.format{type:json_object}`（`convert_response_format.go:19-20`），仍非 schema |
| H-3 | adaptive 分支整体替换 `output_config` | ❌ 未复核（代码未变） | `convert_claude_openai.go:323` |
| H-4 | `allowed_tools` 塞进 Claude `tool_choice` | ❓ | `convert_responses_claude.go:497-509` 仍落 `tool_choice.allowed_tools`；新增 `convert_responses_allowed_tools.go` 声称同构，需对真实 Claude 端点验证 |
| H-5 | 缓存断点白名单漏 `tool_result` | ❌ 未复核（代码未变） | `convert_claude_cache_marks.go:92-105` |
| H-6 | 思考余量补正生产不可达 | ❌ 未复核 | `rewriteClaudeUpstreamMaxTokens` 先行 |
| H-7 | 非 5.2/5.3 的 `glm-*` 回落 `none` | ❌ 仍在 | `claude_thinking_off.go:238-240`（`case strings.Contains(id,"glm-"): return "none"`） |
| H-8 | `knownClaude==false` 直接删 thinking | ❌ 仍在 | `claude_thinking_off.go:263-271`（`if !knownClaude { removeClaudeThinkingOff… }`） |
| H-9 | 出图拒绝看 `route.ProviderID` | 🟡 | `server.go:4312` 仍传 `route`；需核对是否另有 decision 侧检查 |

### 4.6 控制台 6 项

实时请求流 ❌ ｜ 路由聚合视图 ❌ ｜ 会话视图 ❌ ｜ 一键重放 ❌ ｜ 日志成本列 🟡（仅详情弹窗）｜ 告警未读角标 ❌ —— 本轮 `web/` 仅价格表相关改动，6 项零进展。

---

## 五、magpie 对照（本轮新增）

| 维度 | magpie | 本仓库 |
|---|---|---|
| 流式透传 | `gateway.go:1396` `io.Copy` 直通，不缓冲 | `passthrough_stream.go:246/407` 有 failover writer 就整段 `io.ReadAll` |
| 中断判定 | 以「没有收尾帧」为唯一依据，切断如实上报 | `isUpstreamStreamInterruptedReader`（err==io.EOF）+ `closesUpstreamStream`（Contains）+ `isUpstreamStreamInterruptedResponse`（ContentLength）三套并存且结论可相反 |
| 429 | 请求内重试 pause 1/2/4s，Retry-After cap 8s | 账号级长冷却（已按 Retry-After 调整，方向不同） |
| 协议转换 | 单一 IR + 每协议编码器 | 两两手写，11 条腿 |

---

## 六、验收门槛（不勾不许部署，沿用并收紧）

1. `git diff --quiet HEAD` 为真，或部署脚本硬拒未提交状态（**连续 7 轮未落地**）。
2. 二进制内嵌 commit（`-ldflags -X main.commit=…`），线上可反查；本轮线上仍是 13:24 WIP。
3. `go test ./internal/gateway/ -timeout 300s` 在 ~40s 内 `ok`。
4. **新增**：`pass_through` 流式端到端用例——上游分 8 块吐、断言客户端首个 chunk 到达时间 < 总时长/2（本轮 25M 探针即为此用例的现成骨架）。
5. **新增**：`isUpstreamStreamInterruptedReader` 单测四态（正常 EOF / `io.ErrUnexpectedEOF` / 传输错误 / nil），并断言"正常 EOF 不算截断"。
6. **新增**：`runStreamConverter` 与 `isUpstreamStreamInterruptedResponse` 合并为一个判定，且 `Content-Length == 实读` 必须判完整（25C）。
7. `streamEndReader` 分块矩阵：512B/4KB/32KB/单次写 × 完整/截断 × 三种协议，断言 `closed()` 与 `cut()` 的 id/model/lastSequence；并覆盖 25E 的 8 种对齐。
8. 回声守卫：正文含 `message_stop` / `response.completed` / `[DONE]` 的**截断流**必须判截断（25D/25F）。
9. 4 条转换路径各写「上游半路截断」用例，断言客户端拿到 `finish_reason=length` / `response.incomplete` / `max_tokens`（25G/25H 已给出当前错误输出的基线）。
10. 缓冲路径加心跳：断言 30s 静默期间客户端至少收到 1 个 SSE 注释帧（25-5）。
11. `idleGuardedBody` 并发 Close/Read `-race` 覆盖，禁止 nil timer。
12. 中断收尾帧改走 `hw`，SSE 用例 `-race`。
13. 5 个 `*.bak` 入回收站；`gofmt` 在 LF 工作区全绿。

---

## 七、建议的最小修复（按性价比）

1. **一行止血**：`passthrough_stream.go:246` 改 `... && canDefer()`（把 `canDefer` 暴露成方法），无备份链时立刻恢复真流式；有备份链时保留可重试。若要彻底流式，则改成「首块到达即提交，之后不可重试」——与 magpie 同构。
2. `isUpstreamStreamInterruptedReader` 改判 `errors.Is(r.err, io.ErrUnexpectedEOF)`，并把 5 处调用移到 scan 循环之后。
3. 合并 Content-Length 判定（删掉 `runStreamConverter` 里那条，保留字节级比较）。
4. `streamEndReader.tail` 扩到 64 字节 + 给 Claude/Responses 标记加回声守卫。
5. `scanResponses` 只在首帧判定为 Responses 协议后启用。
6. 缓冲路径补 `startSSEHeartbeat`。

---

## 八、复现命令（本轮实际执行）

```bash
# 基线
go build ./... && go vet ./internal/...                 # 绿
go test ./internal/gateway/ -count=1                    # ok 39.044s
git diff | sha256sum                                    # f86d25eb…（= round24）

# 生产只读
ssh root@173.249.207.202 'systemctl is-active llm-protocol-gateway.service; \
  ls -l --time-style=+%Y-%m-%dT%H:%M:%S /opt/llm-protocol-gateway/bin/gateway'
# A/B 查询（python3 sqlite3 只读）
#   pass_through 流式请求的 avg(latency_ms - ttft_ms)，按部署时间切分 → 2509ms / 0ms
```

探针文件（`zz_probe_r25*.go`）已全部删除，工作区指纹已复原为 `f86d25eb…`。**未改动任何业务代码。**
