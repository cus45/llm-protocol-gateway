# 第二十六轮审查：另一个 agent 的改动（round26）

- 审查时间：2026-10-23 17:29–17:55 (+0800)
- 基线 HEAD：起审时 `ed0c139`；17:39 对方又提交了 `f178e88`（embeddings），两版均已单独验证
- 本轮新增提交 4 个：`0a4bf87` / `97f497f` / `ed0c139` / `f178e88`
- 未提交工作区：49 改 7 新增，`git diff HEAD` 指纹在审查期内 `d9a5fe08…` → `b61e7b4d…` → `647eb2ea…`（对方仍在实时编辑）
- 参考实现：`C:\guodevelop\demo\magpie`
- 本轮未修改任何业务代码，探针与临时 worktree 已清理

---

## 一、结论先说

1. **四个新提交整体方向正确**：`97f497f`（读写/空闲超时）、`ed0c139`（删 .bak）、`f178e88`（embeddings 透传）**可以合入**（后者有 3 个 P3 记账/URL 口径问题）；`0a4bf87`（lane 票号队列）修掉了此前的自死锁与幽灵计数，但**没有实现它声称的"先到先得"**——新到请求仍可越过排队队头，探针 **200/200 轮全部插队成功**。
2. **未提交 WIP 不可提交**：我方快照（`+2483/−525`）跑出 **2 个红测试**，都是作者自己新写的、针对"中断收尾帧复用流身份"的用例；作者自带的探针 `TestResponsesStreamIdentityFromReadsItsOwnStream` 到 17:36 仍失败（`id=""`）。根因是逐字节扫描函数取不到嵌套 `response` 对象里的 id。
3. **两个新 P1 实证**：空闲守卫 `idleGuardedBody` 在"Read 进行中 Close"时 **nil pointer panic 仍在**（round24 N24-2 未修，探针复现）；转换器路径对"干净 EOF 但没有收尾帧"的截断 **仍判为正常结束**（探针：`err=nil`，客户端收到 `finish_reason=stop`＋`[DONE]`）。
4. **治理问题升级**：线上二进制 `vcs.revision=0a4bf87` + **`vcs.modified=true`**（17:13:10 构建）——即"提交 0a4bf87 + 一堆未提交改动"的混合体，**没有任何一个 commit 能复现线上**，回滚只能重新编译。二进制里同时存在新旧两套流路径符号（`interceptingStreamWriter` 与 `streamEndReader`/`idleGuardedBody`），坐实"线上跑的是半成品"。
5. 历史 60 项：协议侧基本收官（P1–P8 ✅，P9/P10 🟡，P11 ❌），调度 R4 与 R6 仍未真正解决，安全面 S3/S5 🟡，控制台 6 项 **依旧零进展**（grep 全仓无 `EventSource`/重放/sessionId UI/成本列/告警角标）。

---

## 二、范围与方法

| 项目 | 值 |
|---|---|
| 新增提交 | `0a4bf87` fix(lanes) 票号队列 17:12 / `97f497f` fix(server) 超时 17:25 / `ed0c139` chore 删 .bak 17:28 / `f178e88` feat(embeddings) 17:39 |
| HEAD 干净树 | 独立 worktree：`ed0c139` 与 `f178e88` 均 `go build ./...` ✅、`go vet` ✅、`go test ./internal/gateway/ ./internal/app/` **0 红**（38.4s / 1.5s） |
| 工作区 | `go build ./...` ✅、`go vet ./internal/gateway ./internal/app` ✅；`go test ./internal/gateway/` 17:30 时 **1 红**（作者探针） |
| WIP 快照 | patch `sha256:8063603…`，在 `/tmp/wip26` 独立 worktree 复跑：**2 红** |
| 探针 | 5 个（lane 插队、idle 守卫竞态、转换器干净 EOF、incomplete usage、收尾标记回声）均在隔离 worktree 内运行，已删除 |
| 格式 | `gofmt -l` 命中的 `internal/app/runtime.go`、`internal/store/sqlite.go`、`cmd/gateway/main.go` 在**干净 HEAD 上同样命中** ⇒ CRLF 行尾，不是真实格式问题（延续 round24 的纠正） |

---

## 三、四个新提交逐条审查

### 3.1 `0a4bf87` fix(lanes)：排队改为先到先得的票号队列 — 🟡 部分正确

做对的（前几轮的高危确实消失）：

- `retire`/`retireLocked` 拆成持锁/不持锁两版，`release` 不再在持锁时重新 Lock ⇒ **round21 的自死锁（W21-1）确认已消除**。
- `waiters` 计数换成 `queue []uint64` ⇒ round21 的"幽灵占满 queueCap"（W21-2）一并消除。
- 唤醒后 `now = time.Now()`（`:265`）修掉了"每次广播都把计时器重置成满预算"的老问题，等待总时长现在真的受 budget 约束。
- `passTurnLocked` 只在 `admitLocked` 成功且拿到槽位时调用，票号不会泄漏。

**新问题 L26-1（P1，探针实证）**：`:195` 的条件是 `if !queued || lane.turnLocked(ticket)`。一个**尚未入队**的新请求直接跳过 turn 校验，只要此刻 `inflight < max`（或 rpm 有余量）就拿到槽位——排队的队头被绕过。探针 `TestProbeFIFOBypassByNewArrival`（max=1、队头已入队、释放后立刻有新请求到达）：**200/200 轮新请求抢走了刚释放的槽位**。即"FIFO 只在没有空位时生效"，持续流量下队头仍会饿死，与提交信息宣称的目标不符。正确写法应是"队列非空时，未持票者不得直接准入"（`len(lane.queue) == 0 || lane.turnLocked(ticket)`）。

**L26-2（P3）**：每次准入（含 `passTurnLocked`）都 `close(wake)` 唤醒全部等待者，高并发下是 O(n²) 惊群；当前 rpm/max 全 0 未启用，属潜在成本。

### 3.2 `97f497f` fix(server)：补齐请求读取与空闲连接超时 — ✅ 正确

- `ReadHeaderTimeout 10s / ReadTimeout 5min / IdleTimeout 60s`，`WriteTimeout` 明确不设（流式回答会被从 token 中间切断），注释与测试都写清了理由，测试是**真断言**（不是空转）。
- 顺带确认：这条补的是 round26 之前 S2 的尾巴——`request_body_limit.go` 管的是体积上限，`ReadTimeout` 管的是"慢速滴灌"，两者互补，没有重复。
- 唯一提醒：`ReadTimeout` 对**整个请求**（含 body）生效，附件上传走 5 分钟足够了。

### 3.3 `ed0c139` chore：删 .bak — ✅

- 删除 5 个误提交的备份文件 + `.gitignore` 补规则，正确。
- 但 `git status` 仍有未跟踪 `.superpowers/`（brainstorm 目录）与 `zz_probe_test.go`/`zz_probe2_test.go`（后者是 `println` 脚手架），**提交前必须清掉或改名**（`zz_probe_test.go` 本身是有效用例，只是名字带 zz_）。

### 3.4 `f178e88` feat(embeddings)：透传 `/v1/embeddings` — ✅ 主体正确，记账有缺口

- 路由/Key/模型映射/额度/Provider 闸门/请求日志都复用了 chat 的路径，`OutputProtocol` 不是 OpenAI Chat 的路由会被 `writeRouteProtocolMismatch` 拒绝；上游凭证只用 Provider 自己的（`resolveProviderAuth`），**不把客户端网关 Key 转给厂商**——这几点都对。
- 测试是可信的：真 handler + 假上游，断言了上游路径 `/v1/embeddings`、`Authorization: Bearer <provider key>`、模型映射生效、响应原样回传。
- **E26-1（P3）**：转发复用 `proxyOpenAIImagesUpstream`，它把 `parseUsage` 写死成 `func([]byte) TokenUsage { return TokenUsage{} }` ⇒ 上游明明返回 `usage.prompt_tokens`，网关一律记 0，随后 `recordRequestLogEx` 走**字节估算**（`len/4`）兜底。结果：embeddings 的 token/成本是估出来的，与 chat 路径不一致。修法：把 `ParseOpenAIUsage` 传进去（该函数能读 `prompt_tokens`）。
- **E26-2（P3）**：额度准入用 `quotaPromptEstimate(body)`，而 embeddings 的输入字段是 `input` 不是 `messages`，估算大概率为 0 ⇒ 令牌额度对 embeddings 基本不设防。
- **E26-3（P3）**：`resolveProviderEmbeddingsURL` 只在 BaseURL 已以 `/embeddings` 结尾时原样使用；BaseURL 若写成 `.../v1/chat/completions` 这类完整端点会拼出错误路径。与 `resolveProviderImageURL` 同款问题，建议统一成"已含版本段则只补端点"的规则。


---

## 四、未提交 WIP 审查（快照 `8063603…`，49 改 7 新增，+2483/−525）

### 4.1 方向做对的（值得保留）

| 项 | 证据 |
|---|---|
| **N24-1 缓冲已消除** | `passthrough_stream.go:275-286` 直接 `io.Copy(MultiWriter(flushWriter, tee, usage), response.Body)`；`protocol_proxy.go` 里 `interceptingStreamWriter` 的整段拦截被 `converterStreamWriter` 取代（首字节提交、零字节仍可 defer） |
| **N11-1 长流 usage** | `streamUsageReader` 嵌入 `streamEndReader`，见字节即解析，不再吃 256KB 头部缓冲；`409KB` 类长流可正确记账 |
| **R2 阶梯可达** | `providerSoftFailureWindow 60s → 10min`，否则"30s→1m→2m→4m"永远从 1m 重来 |
| **A4 overage** | `cooldownHint` 纳入 `SevenDayOverageIncluded`，且按"真正耗尽的窗口"取 reset（`exhaustedReset`），不再把周额度耗尽报成 9 分钟 |
| **负缓存 5/5** | `oauthUsageCache` 加 `err/nextRetry/failures`，`lastError` 把失败挂在最后一次好值旁边；5 个 handler 全部接入 |
| **错误响应不再估算计费** | `recordRequestLogEx`：`treatAsFailure` 时跳过 `EstimateTokenUsage`（错误体估出 $1 级假成本已修） |
| **全局价格行** | `lookupPrice` 四级回退 + `pruneModelPrices` 放过 `*`（此前每次保存 state 都会删掉全局价，真 bug）；UI 与顺序一致 |
| **图片超时** | Cursor 桥接改 10 分钟（但见 D26-7） |

### 4.2 新增问题（全部本轮实证）

| 编号 | 级别 | 问题 | 证据 |
|---|---|---|---|
| **D26-1** | **P0** | `responsesStreamIdentityFrom` 取不到嵌套 `response` 里的 id：从最后一个 `{` 起截取，遇到嵌套对象时把外层事件已经越过的 `}` 裁掉，之后 `payloadAt` 只能拿到不平衡前缀 | 作者自带 `TestResponsesStreamIdentityFromReadsItsOwnStream` **FAIL**（`id="" model="" last=1`，17:36 仍在）；快照里 `TestResponsesClosingFrameRepeatsStreamIdentity`、`TestWritePassThroughResponseClosesLongStreamWithItsIdentity` 亦 **FAIL** ⇒ 截断收尾帧仍会伪造 `resp_<nanos>` id（N11-4 只修了 sequence，没修 id） |
| **D26-2** | **P0** | 生产代码残留调试打印：`stream_end_reader.go` 内 `println("DBGSEQ"…)`、`println("DBGNEXT"…)`（17:30 还有 `println("DBGMAP", fmt.Sprint(event))`），每次中断收尾都会往 stderr 打全部事件 map | `grep -n println(` 命中；这是第 5 次脚手架回归（round13/15/19/20/26） |
| **D26-3** | **P1** | `idleGuardedBody.Read` 在读返回后才 `b.timer.Stop()`（`upstream_timeout.go:122`），并发 `Close()`（`:141`）把 `timer` 置 nil ⇒ **nil pointer panic**（round24 N24-2 未修） | 探针 `PROBE-IDLE panic in Read after concurrent Close: runtime error: invalid memory address or nil pointer dereference`；且该代码**已在线上二进制里**（`idleGuardedBody` 符号命中） |
| **D26-4** | **P1** | 转换器路径仍无法识别"干净 EOF 但无收尾帧"的截断：`isUpstreamStreamInterruptedReader` 现在是 `err != nil && !errors.Is(err, io.EOF)`（`stream_interruption_close.go:22`），而 `scanner.Err()==nil` 时 `r.err` 恒为 `io.EOF` ⇒ 分支不可达 | 探针 `PROBE-CLEANEOF err=<nil> interrupted=false body=…finish_reason:stop…data: [DONE]`：截断被包装成"正常结束"发给客户端，R4 对这类截断仍无效 |
| **D26-5** | **P2** | `response.incomplete` 的 usage 被丢：`foldRunningTotalEvent` 只接受 `eventType` 为空或以 `.completed` 结尾（`usage.go:610`），而 `max_output_tokens` 截断的终结事件正是 `response.incomplete` | 探针 `PROBE-INCOMPLETE usage={0,0}`（应为 1234/567）⇒ 截断流记 0 token |
| **D26-6** | **P2** | 收尾标记回声守卫仍可被绕过：`closingEchoWindow=64` 只挡住"标记出现在更靠前的正文里"；正文最后一句话里若出现 `[DONE]`/`message_stop`，仍判为已收尾 | 探针 `PROBE-ECHO closesUpstreamStream=true`（该流实为截断） |
| **D26-7** | **P2** | 图片主路径仍被 120s 卡：`imageGenerationTimeout`（10min）只给了 Cursor 桥；`proxyOpenAIImagesUpstream` 仍是 `Timeout: 0` + `guardUpstreamIdleClient`（⇒ `ResponseHeaderTimeout=120s`） | round19 遗留，未改 |
| **D26-8** | **P2** | `converterStreamWriter.WriteHeader` 是空实现（`passthrough_stream.go:342`）：转换器若想写非 200，会被静默改成 200；`Committed()` 只对 `hw != nil` 成立，语义上"提交"实际由首字节决定，与注释表述一致但与接口习惯不同 | 代码阅读 |
| **D26-9** | **P2** | 心跳 `stop()` 现在 `<-exited` 等 goroutine 退出（`sse_heartbeat.go:47-51`）。方向正确（修掉 handler 返回后仍在写 ResponseWriter），但**没有上限**：客户端读得极慢、TCP 缓冲写满时，心跳写阻塞 ⇒ `stop()` 永不返回 ⇒ handler 不返回、Provider lane 槽位不释放 | 代码阅读（round20 提的"join 可阻塞"从可能性变成必然路径） |
| **D26-10** | **P3** | `streamUsageFolder` 仍靠 `reflect.ValueOf(parseUsage).Pointer()` 分派；`fold==nil` 时回落 `parseUsage(tee.Bytes())`，而 tee 只有 256KB 头部 | `usage.go:460` 附近 |
| ~~D26-11~~ | — | 已被 `f178e88` 提交覆盖，改记为 E26-1/E26-3（见 §3.4） | — |
| **D26-12** | **P3** | `applyOpenCodeZenHeaders(request, provider, …)` 在多账号链里仍用外层 `provider`，同函数内其他头都用 `sendProvider`（`server.go:5586` vs `doOpenAIProviderRequest` 全量替换） | 代码阅读，行为差异待确认 |
| **D26-13** | **P3** | `ErrorDescription` 仍未脱敏：`entry.ErrorDescription = responseErrorMessage`（`server.go:1411`）不经 `scrubSensitiveRequestBody`，上游 401 回显的 `sk-…` 会原样落库并在日志 UI 展示（round18 W18-1 未修） | 代码阅读 |

### 4.3 需要作者立即处理的两件事

1. `stream_end_reader.go` 的 `responsesStreamIdentityFrom` 改成**从事件起点做花括号配平扫描**（找到 `data:` 行首的 `{` 后按 depth 走完整个对象），不要"从最后一个 `{` 往回退"；同时删掉两处 `println`。
2. 修完再谈提交：当前快照有 2 个自测红，说明功能未闭合；提交前把 `zz_probe_test.go` 改成正式测试名、删掉 `zz_probe2_test.go`（println 脚手架）。

---

## 五、历史问题复检总表（"+60 项"总账）

符号：✅ 已修 / 🟡 部分 / ❌ 未动（括号内为本次复检依据）

### 5.1 协议转换（第一轮 P1–P11）

| # | 项 | 状态 | 依据 |
|---|---|---|---|
| P1 | Claude→Chat 流式丢 prompt_tokens | ✅ | 探针 `PROBE-CLEANEOF` 里 `prompt_tokens:10` 正确 |
| P2 | stop_reason 映射不全 | ✅ | `convert_claude_openai.go` 映射表 |
| P3 | Chat→Responses 有 tool_calls 时正文被清空 | ✅ | 前轮已验 |
| P4 | Responses→Chat 流式丢 reasoning 增量 | ✅ | 前轮已验 |
| P5 | incomplete/max_output_tokens 忽略 | 🟡 | 转换器侧已修；**usage 侧新漏**（D26-5） |
| P6 | Chat→Claude block index 错乱 | ✅ | 前轮已验 |
| P7 | tool_use id 归一化碰撞 | ✅ | 哈希后缀 |
| P8 | Responses→Claude 缺合成 tool_result | ✅ | 前轮已验 |
| P9 | redacted_thinking 丢失 | 🟡 | 仅占位 |
| P10 | strict/top_k/stop_sequences 等不转发 | 🟡 | 部分转发（stop 已进 Responses 修复链） |
| P11 | 架构性：两两手写转换无 IR | ❌ | 仍无统一 IR；`protocol_proxy.go` 中转腿结构未变 |

### 5.2 路由 / 调度

| # | 项 | 状态 | 依据 |
|---|---|---|---|
| R1 | 无 RPM 限流 | ✅ | `8b5542d` 滑动窗口（生产 rpm 全 0 未启用） |
| R2 | 冷却不看上游 | 🟡 | 阶梯可达（10min 窗口）+ reset 取"真耗尽窗口"；`Retry-After=1s` 仍会把冷却压到 1s，`401+hardAuth` 仍吃 15min |
| R3 | billing 算硬额度 | ✅ | 已移出 |
| R4 | 流截断无法续流 | ❌ | 传输层报错类已能识别；**干净 EOF 类仍判正常**（D26-4） |
| R5 | 会话粘性只在内存 | ✅ | `session_pins.go` |
| R6 | 并发闸门非 FIFO / 非每账号 | 🟡 | 已入队者 FIFO ✅；**新到可插队 200/200**（L26-1）；仍非每账号 |

### 5.3 横切安全

| # | 项 | 状态 | 依据 |
|---|---|---|---|
| S1 | 管理 API `ACAO: *` | ✅ | `cors.go` 白名单 |
| S2 | 请求体无上限 / 无读超时 | ✅ | `request_body_limit.go` + `97f497f` 三档超时 |
| S3 | 登录/注册/setup 无限流、首远程可设密码 | 🟡 | 限流 + 实例守卫 + 回环判断，仍靠 Host |
| S4 | 正文不脱敏入库 | 🟡 | 请求/响应正文 ✅；**error_description 漏**（D26-13） |
| S5 | 上游头返回后无空闲超时 | 🟡 | 覆盖面大幅扩大（20+ 处接 `guardUpstreamIdleClient`）；但 120s 硬编码、图片路径被误伤、**panic 竞态未修**（D26-3） |

### 5.4 Provider 账号

| # | 项 | 状态 |
|---|---|---|
| A1 | Trae/WorkBuddy/Cursor 刷新无锁 | ✅ 统一 `oauth_renew_lock.go` |
| A2 | 子串匹配 host 泄露 Key | ✅ `baseURLHostIn` |
| A3 | `/v1/images/*` 回落全局 Key / 盲拼 URL | 🟡 拒绝逻辑在；`resolveProviderImageURL:3163` 仍盲拼（只判是否已含 `/images/generations`） |
| A4 | Claude 漏 overage / 429 无负缓存 | ✅ |
| A5 | Cursor 无过期提前量 | ✅（round10 已纠错） |
| A6 | workbuddy_ai 永不过期 | ✅ |

### 5.5 控制台能力缺口（6 项）

| 项 | 状态 | 依据 |
|---|---|---|
| 实时请求流 / 应用日志流 | ❌ | `grep -rn "EventSource" web/src` 零命中 |
| 路由决策聚合视图（surface `analysis.attempts`） | ❌ | 无 |
| 会话视图（`sessionId` 聚合） | ❌ | 仅 `types.ts:676` 有字段，无 UI |
| 一键重放 | ❌ | `grep -i "replay|重放"` 零命中 |
| 日志表成本列 | ❌ | `costMicros` 只出现在密钥配额相关组件 |
| 告警未读角标 | ❌ | 只有通用 `Badge` 组件 |

### 5.6 前几轮 WIP 专项（抽样复检）

| 编号 | 项 | 状态 |
|---|---|---|
| N23-1/N24-1 | 流式名存实亡（整段缓冲） | ✅ WIP 已改真流式（未提交、未上线） |
| N24-2 | idle 守卫 nil timer panic | ❌ D26-3（探针复现） |
| N24-3 | 4 字节携带致误判 | ✅ 换成 `streamLineFramer` + 全字节 `streamEndReader` |
| N24-4 | 转换器 interrupted 恒 false | 🟡 传输错误类 ✅；干净 EOF 类 ❌（D26-4） |
| N23-3 | 两套完成判定结论相反 | ✅ 统一到 `streamEndReader`（`closesUpstreamStream` 变成它的 body-at-once 包装） |
| N23-4 | stream+JSON 回包 usage=0 | 🟡 待复验（`primed` 分支仍在） |
| W18-1 | error_description 不脱敏 | ❌ D26-13 |
| V22-1 | lane 新到插队 | ❌ L26-1 |

---

## 六、生产状态（本轮实测）

| 项 | 值 |
|---|---|
| 服务 | `llm-protocol-gateway.service` **active**，ActiveEnter 17:13:23 (+0800) |
| 二进制 | `/opt/llm-protocol-gateway/bin/gateway` mtime `2026-10-23 02:13:10 PDT` = **17:13:10 +0800** |
| 内嵌版本 | `vcs.revision=0a4bf87…`、**`vcs.modified=true`** |
| 符号 | 同时含 `interceptingStreamWriter`(旧缓冲) 与 `streamEndReader`/`responsesStreamIdentityFrom`/`idleGuardedBody`(新 WIP) |
| 数据 | 44 Provider / 9 Key / 24h 请求 10,113 条 / 429 14 条 / 5xx 4 条 |
| 限流配置 | `max_concurrent_requests>0` 的 Provider **0 个**，rpm 未配置 ⇒ lane 未启用，L26-1 暂未在生产暴露 |
| 与仓库的差距 | 线上**不含** `97f497f`（读写/空闲超时）与 `f178e88`（`/v1/embeddings` 未上线）；却含未提交 WIP（`idleGuardedBody`、`streamEndReader`） |
| 流式指标 | `pass_through` 24h n=1233，`avg(latency−ttft)=4536ms`、max 305s；`convert` n=8850，avg 1077ms |

**G26-1（P1，治理）**：线上是"`0a4bf87` + 未提交改动"的编译产物（`vcs.modified=true`）。它既不是 HEAD，也不对应任何 commit：**回滚没有 commit 可回滚，只能重新编译**；更麻烦的是它把尚未通过的 WIP（`idleGuardedBody` 的 panic 竞态、`responsesStreamIdentityFrom` 的错误实现）带上了生产。建议：立即用最新提交 `f178e88` 重新构建部署一次，把生产拉回可复现状态；之后 `deploy-linux.sh` 加一道 `git diff --quiet` 守卫。

**G26-2（P3，卫生）**：`git worktree list` 里还挂着 `C:/gw-head-wt`（指向 `34ec9a4`），是遗留的审查/构建 worktree，建议 `git worktree remove`。未跟踪 `.superpowers/`、`zz_probe_test.go`、`zz_probe2_test.go` 待处理。

---

## 七、验收门槛（合并前逐条勾）

1. `git diff --quiet` 为空（提交的是完整状态，不含 `println`）。
2. 干净 worktree `go build ./... && go vet ./... && go test ./internal/gateway/ ./internal/app/` 全绿。
3. `grep -rn "println(" internal/` 零命中。
4. `responsesStreamIdentityFrom` 能在**嵌套 response 对象**的事件上取回 id（用作者的 `TestResponsesStreamIdentityFromReadsItsOwnStream` 作门槛）。
5. 截断流（无收尾帧）在**转换器路径**上也判为中断，且客户端收到合法收尾帧。
6. `idleGuardedBody` 的 Read/Close 并发不 panic（加探针用例固化）。
7. `response.incomplete` 的 usage 被记账（1234/567 级断言）。
8. 收尾标记回声守卫有正向/反向两个用例（正文含 `[DONE]` 但流已截断仍判截断）。
9. 心跳 `stop()` 有上限或可被 ctx 打断。
10. lane 新到请求在队列非空时**不得**越过队头（把 L26-1 探针反过来写成断言）。
11. 图片路径的超时与 `imageGenerationTimeout` 一致（长图不再被 120s 砍）。
12. 部署脚本构建前校验 `vcs.modified=false`。

---

## 八、建议动作顺序

1. **先救生产**：用 `f178e88` 重建部署（撤掉带上线的 WIP panic/错实现），再按 §七 收口 WIP。
2. **WIP 收口顺序**：D26-2（删 println）→ D26-1（id 恢复，带测试转绿）→ D26-3（timer 竞态）→ D26-4（干净 EOF 判定）→ D26-5/D26-6（usage/回声守卫）→ D26-9（心跳 join 上限）。
3. **然后才是新功能**：L26-1（FIFO 补严）→ 控制台 6 项（实时流优先，排障价值最高）→ 能力扩展（embeddings 已起步，需确认复用的转发函数语义）。

---

*本轮未修改任何业务代码；探针文件已删，`/tmp/rev26`、`/tmp/wip26` worktree 已移除。*
