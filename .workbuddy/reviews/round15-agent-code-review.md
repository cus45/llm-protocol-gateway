# 第十五轮 · 另一 Agent 改动代码审查（round15）

- 审查时间：2026-10-23 09:40–09:50（+0800）
- 审查范围：**无新提交**（HEAD 仍 `34ec9a4`）+ 工作区 WIP（审查期间对方**正在实时调试**）
- WIP 指纹演变：`4436363…`（09:41）→ `07485d1f…`（09:45）→ `8106e999…`（09:47:14，本轮定稿快照）
- 对照基准：`C:\guodevelop\demo\magpie`
- 基线：`go build ./...` ✅ / `go vet ./internal/gateway/` ✅ / **`gofmt -l` ❌（provider_failover.go）** / 测试 **2 红**（干净 HEAD 树同用例 0 红）
- VPS（173.249.207.202:18093）：`active`，二进制 mtime `2026-10-22_17:14:49`、**仍是 `34ec9a4`**，WIP 未上线
- **本轮未修改任何业务代码、未提交**；探针文件已删除、临时 worktree 已移除

---

## 结论先说

1. **本轮 WIP 不正确，并且它把一条本来能用的行为改坏了。** 同一条「上游在回答中途被切断、没有备用账号」的请求：

   | | 客户端收到 |
   |---|---|
   | HEAD `34ec9a4` | **438 字节**：开头两帧 + `"once"` 正文 + `message_delta(max_tokens)` + `message_stop` ✅ |
   | 本轮 WIP | **0 字节**（`rec.Body==""`），随后由 handler 回一个 502 JSON ❌ |

   这就是 `TestNoFailoverReplayAfterMidStreamInterruption` 从绿变红的原因（`client saw the text 0 times: ""`），而不是测试写错了。

2. **根因：`prefix`（"已经发给客户端的前缀"）这个模型在本路径上不存在。** `writeBufferedStream` 是「先 `ReadAll` 整段、再下发」，出错的时刻客户端一个字节都没拿到；`commitHeaders` 里唯一可能把字节搬进 `prefix` 的分支（`w.buf.Len() > 0`）在流式路径上恒为假 —— `WriteHeader(200)` 在**任何 Write 之前**就提交了，`buf` 必然为空。对方自己的探针已经打出来了：

   ```
   WRITE passthrough 0     ← 第一次 Write 时 buf 就是空的
   PL 0
   DBG pass=true plen=0 slen=101 preflen=0 prestart=0 buf="<整段正文>"
   prefix="" want "event: message_start\ndata: {}\n\n"   ← 自测红
   ```

   因此 `passthrough_stream.go:327` 新加的 `&& len(fw.Prefix()) > 0` 把**唯一**能给客户端收尾的分支变成了死代码。

3. **新增/改写的 3 条自测里 2 条红，其中 1 条的断言本身不可能成立**：Claude 的每一帧里类型名出现两次（`event: message_start` + `data: {"type":"message_start"`），所以 `strings.Count(body,"message_start") != 1` 即使实现完全正确也必挂（HEAD 实测 = 2）。`message_stop`、`content_block_start` 同理。

4. **调试脚手架又回来了，而且这次连格式都不过**：`provider_failover.go:314/320` 两行 `println`（**每个 chunk 一行**）、自测里一行 `t.Logf("DBG …")`、`commitHeaders` 里 `w.buf.Reset()` 缩进错乱 → `gofmt -l` 报该文件。这是 round8 / round13 / round14 已报三次的同类问题。

5. **生产未受影响、但也未修**：HEAD 没动、VPS 仍是 `34ec9a4`，所以 round14 的两条线上 P0（整段缓冲导致零流式 / usage 记成 0）**原样在线**。本轮工作区改动既没修它们，也没法上线。

6. **历史 60 项零进展**（第四节逐项复核，带行号与命令）。本轮只有 5 个文件被动过，其余文件逐字节未变。

---

## 一、范围与证据

```bash
git log --oneline -1            # 34ec9a4 fix(sse): 流中断后可交给备用 Provider 续流（未变）
git status --porcelain          # 5 个已跟踪文件修改 + 期间出现又消失的 zz_dbg_test.go
git diff HEAD | sha256sum       # 09:41 4436363… → 09:45 07485d1f… → 09:47 8106e999…
```

| 检查项 | 结果 |
|---|---|
| 新提交（round14 之后） | **0 个** |
| 被改文件 | `provider_failover.go`(+58)、`passthrough_stream.go`(+6)、`stream_interruption_retry_test.go`(+51)、两个 test 文件纯 gofmt 对齐 |
| `go build ./...` / `go vet` | ✅ / ✅ |
| `gofmt -l internal/gateway/` | ❌ `internal\gateway\provider_failover.go` |
| 工作区测试 | ❌ 2 红（`TestNoFailoverReplayAfterMidStreamInterruption`、`TestFailoverWriterCutTurnClosesAfterCommittedPrefix`） |
| 干净 HEAD 树（worktree `34ec9a4`）同用例 | ✅ PASS（两条都通过 / 不存在），**证实红是 WIP 引入** |
| 线上 | `active`，二进制 `2026-10-22_17:14:49`、无 `prefixStart` 串 → **= `34ec9a4`，WIP 未部署** |

### 本轮 WIP 改了什么（意图 → 实际）

| 位置 | 作者意图 | 实际 |
|---|---|---|
| `failoverResponseWriter` 新增 `prefix/prefixStart/prefixLen/streamLen` | 区分「已发给客户端的开头」与「还没发的剩余」 | 字段在流式路径恒为 0（见结论 2） |
| `Write` passthrough 分支改为先写 buf 再下发 | 记住本次尝试的流长度 | 每次 Write 都 `buf.Reset()`，只剩最后一个 chunk；**并吞掉了 base 的写错误** |
| `commitHeaders` 把 buf 追加进 `prefix` | 把「随头下发的开头」记成前缀 | 只有**错误响应体**会走到这里，与流无关 |
| `WriteHeader` 加 `statusCode > 0 &&` | 防御 `WriteHeader(0)` | 全仓 `WriteHeader(0)` 调用 0 处 → 无效改动 |
| `writeBufferedStream` 加 `&& len(fw.Prefix()) > 0` | 避免重复下发开头 | **分支变死代码 → 客户端 0 字节** |
| `streamClosed()` 改看 `w.stream()` | — | 该函数**全仓无调用者**（死代码），且 passthrough 下它只剩最后一个 chunk |
| `recordsForRetry()` 注释删除 | — | `streamRetryRecorder` 接口 + 方法全仓无任何断言使用 → 死代码 |

---

## 二、本轮改动逐条判定

| # | 级别 | 问题 | 位置 | 证据 |
|---|---|---|---|---|
| W15-1 | **P0（回归）** | `&& len(fw.Prefix()) > 0` 让最后尝试的收尾分支不可达 → 截断流客户端收到 0 字节（HEAD 收 438 字节 + 收尾帧） | `passthrough_stream.go:327` | 探针：WIP `bytes=0`，HEAD `bytes=438 / once=1`；`TestNoFailoverReplayAfterMidStreamInterruption` 红「client saw the text 0 times」 |
| W15-2 | **P0（模型错）** | `prefix` 前提不成立：`WriteHeader(200)` 在首次 `Write` 之前提交，`buf` 必空 → prefix 永空；作者自测红 | `provider_failover.go:351-367` | `DBG pass=true plen=0 preflen=0 buf=<整段>`；`prefix="" want "event: message_start…"` |
| W15-3 | **P1（测试不可达）** | 新增断言按**子串出现次数**数帧：每帧类型名出现 2 次 → `==1` 永远不成立 | `stream_interruption_retry_test.go:265-276` | HEAD 同请求实测 `message_start=2 / message_stop=2`；应改数 `event: message_start` |
| W15-4 | **P1（新 bug）** | passthrough `Write` 吞掉 base 写错误并把部分写当成全部写：`_, _ = w.base.Write(p); return len(p), nil` | `provider_failover.go:322-325` | HEAD 为 `return w.base.Write(p)`；客户端断开不再中断上游读取/不再 `client_gone` |
| W15-5 | **P1（脚手架）** | 热路径调试打印：09:47 快照 2 行 → **09:48 快照已增至 5 行**（`WRITE passthrough` / `PL` / `SL` / `AFTER` / `SETSTART`，每 chunk 4 行直连 stdout），另有自测 `t.Logf("DBG…")`/`println("LOCAL"…)` | `provider_failover.go:314,320,322,324,371`、test:401-405 | 测试输出实测刷屏；round8/13/14 同类问题第 4 次，且每次「清理」后又加回来 |
| W15-6 | **P2** | `gofmt` 不过：`commitHeaders` 内 `w.buf.Reset()` 多缩进一层 | `provider_failover.go:364` | `gofmt -l` 报该文件 |
| W15-7 | **P2** | `streamBody[fw.streamStart():]` 无边界检查 → 一旦 prefix 真被填上而错误体更短即 panic | `passthrough_stream.go:335` | 静态；当前因 W15-1 不可达 |
| W15-8 | **P3** | 三处新增死状态：`streamClosed()` 无调用者；`StreamLen()` 仅测试用；`streamRetryRecorder`/`recordsForRetry()` 无断言；`streamLen` 只记第一次 Write 长度却文档写「整段长度」；passthrough `Write` 每次 `buf.Reset()` 使 `BufferedBody()`/`streamClosed()` 语义退化为「最后一个 chunk」 | `provider_failover.go:245-274,309-319` | `grep -rn "streamClosed\|StreamLen\|recordsForRetry"` 仅定义与测试 |

### 与 HEAD 的差异实测（同一探针，两侧各跑一次）

```
HEAD 34ec9a4 : status=200 err="upstream stream interrupted after 248 bytes: unexpected EOF"
               client bytes=438 once=1 message_start=2 message_stop=2
WIP  8106e999: status=200 err="…" client bytes=0   once=0 message_start=0 message_stop=0
```

> 结论：这次改动**不是修 bug，是把「半截答案 + max_tokens 收尾」换成了「什么都没有」**。真要消掉「开头重复下发」的疑虑，先证明它存在 —— 在 `writeBufferedStream` 里不存在（客户端在此分支之前收不到任何正文）。

---

## 三、与 magpie 对照（同一场景的第三方答案）

`magpie/internal/gateway/stream_cut_test.go`（另有 `claude_cut_stream_test.go` / `gemini_*_cut_test.go`）的做法是：**保持流式，把「连接中途断了」如实告诉客户端**（"lost the connection"，属可重试错误），让客户端自己重试；**不整段缓冲、不换账号续接半截答案**。

我们这边两条路都试过了：round14 的「整段缓冲换可重试」（线上 P0：零流式）、本轮的「前缀拼接」（P0：零输出）。**方向建议**：透传流保持边收边发；被切断时补协议收尾帧并明确标记截断（`max_tokens` / `finish_reason=length` / `response.incomplete`），failover 只在**首字节之前**发生；把「流中断」与「Provider 故障」分开计数，不参与冷却。

---

## 四、完整排查清单（历史 60 项，本轮逐项复核）

复核方法：HEAD 未变 + 本轮只动 5 个文件（清单见第一节）→ 其余文件逐字节未变；对下文每一项做 grep/读码抽查，行号为本轮实测。

### 4.1 协议转换

| ID | 项目 | 状态 | 证据 |
|---|---|---|---|
| P1 | Claude→Chat 流式 usage 合并 | ✅ | 干净树测试绿 |
| P2 | `stop_reason` 全量映射 | ✅ | `convert_claude_openai.go:427-438` |
| P3 | Chat→Responses 保留工具前正文 | ✅ | 测试绿 |
| P4 | Responses→Chat reasoning 增量 | ✅ | 测试绿 |
| P5 | `incomplete`/`max_output_tokens` | 🟡 | 与中断收尾语义重叠（都被 `length` 覆盖） |
| P6 | Chat→Claude block index | ✅ | 测试绿 |
| P7 | 工具 ID 哈希后缀 | ✅ | `convert_tools.go` |
| P8 | 合成 `tool_result` | ✅ | — |
| P9 | `redacted_thinking` / 反向 reasoning | 🟡 | 反向仍丢全部 |
| P10 | `strict`/`top_k`/`metadata`/`prompt_cache_key` | 🟡 | Claude 构造器仍无 |
| P11 | 单一 IR 架构 | ❌ | 架构级，未动 |
| H-1 | `output_config.format` 带 `name` | ❌ | `convert_response_format.go:41` |
| H-2 | `json_object` 原样发 Claude | ❌ | `convert_response_format.go:19-20` |
| H-3 | adaptive 分支整体替换 `output_config` | ❌ | `convert_claude_openai.go:321-324` |
| H-4 | `allowed_tools` 塞进 Claude `tool_choice` | ❌ | `convert_responses_claude.go:497-509`（`:506` 仍是 `"allowed_tools":{…}`） |
| H-5 | 缓存断点不含 `tool_result` | ❌ | `grep -c tool_result convert_claude_cache_marks.go` → **0** |
| H-6 | 思考余量补正生产不可达 | ❌ | `claude_pass_through_normalize.go:151` |
| H-7 | 非 5.2/5.3 `glm-*` 回落 `none` | ❌ | `claude_thinking_off.go` |
| H-8 | `knownClaude==false` 删 thinking | ❌ | 同上 |
| 假测试 | `TestClaudeToolChoiceWireShape` 恒真（只 round-trip 自己的字面量，不碰任何转换函数） | ❌ | `convert_tool_restrictions_test.go:127-149` |
| N11-1 | 长流 usage 用 256KB 头部副本 | 🔴 未修 | `passthrough_stream.go:358` 仍 `tee.parse(parseUsage)` |
| N11-2 | 心跳 goroutine 不 join | ❌ | `sse_heartbeat.go:41-43` 只 `close(done)` |
| N11-3 | 透传 Claude 收尾漏 `content_block_stop` | 🔴 | `claudeStreamInterrupter` 只补 `message_delta`+`message_stop` |
| N11-4 | 透传 Responses 收尾帧缺 `id/model/sequence_number` | 🟡 | `responsesStreamInterrupter` |
| N12-1 | 567 → 65,536 输出 token 放大 | ❌ | `server.go:1330` 区域 |
| N13-1 | `--version` 会起服务（零 flag 解析） | ❌ | `cmd/gateway/main.go` 未动 |
| N13-2 | `/__usage-analysis` 无上限 | ❌ | 未动 |
| N14-1 | **成功流整段 `ReadAll`，线上零流式（P0）** | 🔴 在线上 | `passthrough_stream.go:155-158` 无 `canDefer` 门槛 |
| N14-2 | 转换器中断判定读前快照恒 false | 🔴 | `convert_*_stream.go:87/126/253/576/815` 全部仍在 |
| N14-3 | `runStreamConverter` 的 Content-Length 判定不与实读字节比较 | 🔴 | `protocol_proxy.go:587-590` |
| N14-4 | 非缓冲路径用头部 256KB 判收尾（409KB 完整流误判 + 重复收尾） | 🔴 | `passthrough_stream.go:199` |
| N14-6 | 「中断」直接计 Provider 软失败 → 3 次冷却 | 🔴 | `provider_failover.go:1072-1084` 走 transport 分支 |
| N14-7 | 中断收尾重复 `content_block_stop`（同 index 两次） | 🔴 | `stream_interruption_close.go:127-131` |
| N14-8 | 缓冲路径丢上游响应头（只剩 Content-Type/Cache-Control） | 🔴 | `passthrough_stream.go:311-316` |
| N14-9 | `attemptFailoverReason` 拿响应体当传输错误匹配 | 🟡 | `provider_failover.go:1164-1169` |
| N14-11 | 空流重试 × failover 两层叠乘 | 🟡 | `protocol_proxy.go:533-570` |
| **W15-1..8** | **本轮新增（见第二节）** | 🔴 | — |

### 4.2 路由 / 调度

| ID | 项目 | 状态 | 证据 |
|---|---|---|---|
| R1 | RPM 限流 | 🟡 | 仅 per-provider lane（`provider_lanes.go` 未动） |
| R2 | 冷却跟随上游节奏 | 🔴 | `provider_failover.go:571-600` 仍取 `max(默认, Retry-After/resets_at)` 并封顶 60min（短 429 被 7d resets 拉长）；`:1293` 复探失败仍把时长缩回 1–2 分钟 |
| R3 | billing 不算硬额度 | ✅ | `6a1256b` |
| R4 | 截断续传 | 🔴 | 透传靠整段缓冲（N14-1）、转换判定恒 false（N14-2）、本轮把透传收尾也弄丢了（W15-1） |
| R5 | session pin 落库 | ✅ | `30519bb` |
| R6 | per-account 闸门 + FIFO | 🟡 | `provider_lanes.go` 未动 |

### 4.3 横切安全

| ID | 项目 | 状态 | 证据 |
|---|---|---|---|
| S1 | 管理 API CORS 白名单 | ✅ | — |
| S2 | 请求体上限 | ✅ | 256MB |
| S3 | 认证限流（先解码后限流、表无上限） | 🟡 | `admin_auth.go` 未动 |
| S4 | 响应正文脱敏 | 🟡 | `redact*` 只覆盖凭证（M-19） |
| S5 | 首包后空闲超时 | 🔴 | `grep -c "Timeout: 0"` = **11**，未动；N14-1 把「首包」变成「整段」，问题被放大 |

### 4.4 Provider 账号

| ID | 项目 | 状态 | 证据 |
|---|---|---|---|
| A1 | 刷新令牌加锁 | ✅ | `16c00d3`（锁表 3 瑕疵未收） |
| A2 | 用量探测 host 白名单 | ✅ | — |
| A3 | images 路由 / 全局 Key 回落 | 🟡 | `server.go:4207` 仍用 `route.ProviderID` |
| A4 | Claude 额度窗口 + 429 负缓存 | ❌ | `grep -rn seven_day_overage` → 0 命中 |
| A5 | Cursor 过期提前量 | ✅ | 内建 5 分钟（round10 已纠错） |
| N-1 | `workbuddy_ai` 空 `expires_at` → 永不续期 | ❌ | `workbuddy.go:607-620`：`expiresAt=="" → return false`；且无 401→续期→重放 |

### 4.5 控制台

| 项目 | 状态 |
|---|---|
| 实时请求流 / 路由解释聚合 / 会话视图 / 一键重放 / 日志成本列 / 告警未读角标 | ❌ 6 项全未动（`web/` 本轮零改动） |

---

## 五、验收门槛（本轮更新，合入/上线前必须全过）

1. **回滚本轮 WIP 的 `prefix` 机制**（`Prefix/streamStart/StreamLen/stream/*/streamClosed/recordsForRetry` 整段删除），`writeBufferedStream` 恢复「无 prefix 门槛」的收尾分支。（W15-1/2/8）
2. 恢复 `Write` passthrough 的 `return w.base.Write(p)` 语义，写错误必须上抛。（W15-4）
3. 清空所有调试打印（`println(` / `fmt.Printf(` / `t.Logf("DBG…")`），并加 CI 守卫：`internal/gateway` 非 `_test.go` 文件出现 `println(` 即失败。（W15-5）
4. `gofmt -l` 必须为空（AGENTS.md 硬要求）。（W15-6）
5. 帧计数断言一律数 `event: xxx` / `"type":"xxx"` 精确形态，禁止裸子串计数。（W15-3）
6. 若要保留「已发出前缀」概念，必须先有**真实**产生前缀的路径与用例（当前 0 处），否则不要引入状态。（W15-2）
7. 历史硬门槛继续有效：转断判定读后求值、Content-Length 与实读比较、usage 用全文、收尾判尾部、中断不计软失败、回滚整段缓冲、首字节 < 2s。（N14-1/2/3/4/5/6）

---

## 六、复现

```bash
cd /c/guodevelop/llm-protocol-gateway
git status --porcelain          # 5 个已改文件，无新提交
gofmt -l internal/gateway/      # → internal\gateway\provider_failover.go
go test ./internal/gateway/ 2>&1 | grep -E '^--- FAIL'
#   TestNoFailoverReplayAfterMidStreamInterruption
#   TestFailoverWriterCutTurnClosesAfterCommittedPrefix

# 作者自测（红色 + 调试输出即为「prefix 恒空」的铁证）
go test ./internal/gateway/ -run TestFailoverWriterCutTurnClosesAfterCommittedPrefix -v

# 与 HEAD 对比（临时 worktree）
git worktree add --detach .workbuddy/review-head15 34ec9a4
(cd .workbuddy/review-head15 && go test ./internal/gateway/ -run TestNoFailoverReplayAfterMidStreamInterruption -v)  # PASS

# 线上只读
ssh root@173.249.207.202 'systemctl is-active llm-protocol-gateway; \
  ls -l --time-style=+%F_%H:%M:%S /opt/llm-protocol-gateway/bin/gateway; \
  grep -c "prefixStart" /opt/llm-protocol-gateway/bin/gateway'   # active / 17:14:49 / 0
```

---

## 七、副作用披露

1. 本地建过一次性 worktree `.workbuddy/review-head15`（已 `git worktree remove --force`），未改动工作区业务文件。
2. 建过一次性探针 `internal/gateway/zz_r15_probe_test.go`（1 例，**已删除**；`git status` 只剩对方自己的 5 个改动）。
3. 对 VPS 只读：`systemctl is-active`、`ls -l`、`grep -c`。未重启、未部署、未改配置。
4. 审查期间对方 agent 仍在编辑：指纹 09:41 `4436363…` → 09:45 `07485d1f…` → 09:47 `8106e999…` → 09:48 `07c7800c…`（期间 `zz_dbg_test.go` 出现又删除；`println` 从 2 处增到 5 处）。本报告正文以 **09:47:14 快照**为准；结论（2 红、prefix 恒空、客户端 0 字节）在四个快照上一致，未受后续调试改动影响。
