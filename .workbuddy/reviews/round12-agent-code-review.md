# 第十二轮 · 另一 Agent 改动代码审查（round12）

- 审查时间：2026-10-23 06:05–06:25（+0800）
- 审查范围：`HEAD = 6a1256b`（未变）+ 未提交工作区（13 改 + 3 新增）
- 对照基准：`C:\guodevelop\demo\magpie`
- **本轮结论：连续第五轮「零新代码」**；但本轮换到「已部署代码」的另一条路径上，挖到 **1 条比上轮更严重的 P1**（用量不是归零，而是被兜底估算放大 115 倍），以及 1 条时区语义缺陷
- 基线：`go build ./...` ✅ / `go vet ./internal/gateway/` ✅ / `go test ./internal/gateway/` ❌ **5 顶层用例红（7 子用例）**，与 round9/10/11 完全一致
- VPS（173.249.207.202:18093）：`active`，`/__health` 200，`ActiveEnterTimestamp=2026-10-22 10:29:24 PDT`（=10-23 01:29 +0800），线上仍跑 `6a1256b`，WIP 未上线
- **本轮未修改任何业务代码、未提交**；探针已跑完并从仓库删除，副本存 `.workbuddy/reviews/round12-evidence/`

---

## 结论先说

1. **没有新代码可审。** `git diff HEAD | sha256sum = 5dfa6dfe9619b5d87f553c036f0f6b3096590d62805a872c5b5b7404e0772596`，与 round11 **逐字节一致**；16 个文件 mtime 全部 ≤ `01:38:58`，已停 4 小时 26 分。所以"另一个 agent 这轮改得对不对"没有新判断对象——**上一轮报的 9 条高危、3 条 P0、全部遗留原样**。

2. **本轮把 N11-1（长流 usage）追到底，结论比上轮更坏**：

   - 上轮结论是"usage 归零"。实测**不是归零**：`recordRequestLogEx` 在 `usage.InputTokens==0 && usage.OutputTokens==0` 时会用 `EstimateTokenUsage` 兜底（`server.go:1330-1332`），而兜底的"响应体"正是被截到 256KB 的那份 `tee.Bytes()`。

   - **探针实证**（440,771 字节的 Chat 流，真实用量 `1234/567`）：

     ```
     full body=440771 bytes, tee=262144 bytes
     real usage: in=1234 out=567
     tee usage : in=0  out=0
     logged usage: in=7 out=65536     ← 兜底估算 = 262144/4
     ```

   - 后果方向与上轮相反：**不是少算，而是把 567 输出 token 记成 65,536（放大 115×），输入却塌成 7**。这条直接进 `CostMicros`、`KeyWindowUsage`（Key 日/月额度）、日用量表，等于用户问一个长回答就把当天配额烧掉一大截。上轮报告的"低估"只在 Claude 方向成立（`in=800/out=1`，非零不触发兜底）。

3. **新发现一条时区语义缺陷（P2，已在线上）**：日用量分桶、Key 日/月额度窗口、以及控制台"今日"，全部用**服务器进程时区**（`usage_accumulator.go:216 event.Time.Local()`、`key_quota.go:91 now.Local()`）。VPS 实测 `Time zone: America/Los_Angeles`，所以**北京时间的用户看到"今日"是洛杉矶的一天，Key 日额度在北京时间 15:00 重置**。代码里没有任何 TZ 配置项。

4. **WIP 仍不可合入**：三条 P0 一条没修（判定恒 true / 假缓冲重试 / 多账号丢心跳），5 个测试还在红，`zz_dbg2_test.go` 调试脚手架还在车上。

5. **合入门槛不变**：`go test ./internal/gateway/` 全绿。当前 5 红同一根因，改一处即可全绿，但必须先修 N10-1/N12-1，否则修完判定就把"长流用量爆炸"放到线上。

---

## 一、范围与证据

```bash
git log -1 --format='%h %ci %s'      # 6a1256b 2026-10-23 01:28:25 +0800
git status --porcelain               # 13 M + 3 ??
git diff HEAD | sha256sum            # 5dfa6dfe…（与 round11 相同）
stat -c '%y %n' <13 modified>        # 01:33:51 – 01:38:58
go build ./... && go vet ./internal/gateway/
go test ./internal/gateway/          # 5 顶层红
```

| 检查项 | 结果 |
|---|---|
| 新提交（round11 之后） | **0** |
| 工作区变更 | 与 round11 逐字节一致（指纹相同） |
| `go build ./...` | 通过 |
| `go vet ./internal/gateway/` | 无输出 |
| `go test ./internal/gateway/` | 37.9s，5 顶层 / 7 子用例红 |
| VPS | `active`，health 200，TZ = America/Los_Angeles，仍是 `6a1256b` |

红用例（与 round9/10/11 逐字一致）：

```
--- FAIL: TestResponsesIncompleteReportsLengthInChatStream
    unexpected error: upstream stream ended after 526 bytes without its closing event
--- FAIL: TestChatLengthClosesResponsesStreamAsIncomplete
    incomplete_details.reason = "upstream_stream_interrupted", want max_output_tokens
--- FAIL: TestClaudeStopSequenceToChatFinishStream        (5 子用例全红)
--- FAIL: TestChatToClaudeStreamReportsStopSequence
--- FAIL: TestFinishConvertedProxyMidStreamDropIsRetryableWhenDeferred
    client should not see the interrupted attempt, got ..."content":"partial"...
```

---

## 二、本轮新发现

### N12-1（P1，**HEAD 已存在、线上正在跑**）长流用量被兜底估算放大：567 → 65,536

三个既有事实叠加成一个放大器：

```go
// 1) 解析用的是被截断的副本（passthrough_stream.go:137-139，HEAD）
usage = parseUsage(tee.Bytes())        // tee 上限 256KB（:13）

// 2) 解析不出东西就"估算"（server.go:1330-1332）
if usage.InputTokens == 0 && usage.OutputTokens == 0 {
    usage = EstimateTokenUsage(requestBody, responseBody)   // len(body)/4
}

// 3) 传进来的 responseBody 还是那份 256KB（passthrough_stream.go:140）
return status, usage, tee.Bytes(), nil
```

`EstimateTokenUsage`（`usage.go:341-346`）把 `len(responseBody)/4` 当输出 token。响应体被截到 262,144 字节 → **65,536**。

**探针实测**（`.workbuddy/reviews/round12-evidence/zz_round12_probe_test.go.txt`）：

```
full body=440771 bytes, tee=262144 bytes
real usage: in=1234 out=567
tee usage : in=0 out=0
logged usage: in=7 out=65536     ← 输入塌成 7，输出放大 115×
```

**影响面**（`writePassThroughResponse` 共 7 个调用点：`protocol_proxy.go:172`、`qoder_chat.go:73`、`server.go:5289/5353/5493`、`trae.go:1838`，另 `:5446` 无 usage）：

- Claude Code 读大文件 / 长回答这类 300KB+ 的流，**每一跳都按 6.5 万输出 token 记帐**；
- 该数字进 `CostMicros`（成本）、`KeyWindowUsage`（Key 日/月额度）、`/__usage-analysis` 日聚合；
- 与 N11-1 的差别：Claude 方向是 `in>0` 不触发兜底 → **低估**；Chat/Responses 方向是 `0/0` → **高估到离谱**。两个方向都错，且互相掩盖（总量看起来"差不多"）。

**同一放大器的第二条入口**：`copyErr != nil` 时直接 `return status, TokenUsage{}, tee.Bytes(), copyErr`（`passthrough_stream.go:134`，HEAD）。**任何被 RST / 客户端断开的流，用量也是 0/0 → 同样被估算成 256KB/4**。而 WIP 的"流中断识别"如果上线，被判中断的流会更多，这条会从"偶发"变成"常态"。

**修法**：usage 解析必须用完整 body（缓冲分支用 `streamBody`，直通分支加尾部 ring，见 round11 门槛 3）；`EstimateTokenUsage` 不得在"有流但解析失败"时启用，宁可记 0 并打 `usage=unparsed` 标记（magpie 的 usage 缺失就是缺失，不猜）。

### N12-2（P2，**HEAD 已存在、线上正在跑**）「日」的边界跟随服务器时区，代码里不可配

```go
// usage_accumulator.go:216 —— 日桶键用进程本地时区
dayKey := event.Time.Local().Format("2006-01-02")
// key_quota.go:91 —— 日/月额度窗口同样
local := now.Local()
start := time.Date(local.Year(), local.Month(), local.Day(), 0,0,0,0, local.Location())
```

VPS 实测：

```
Time zone: America/Los_Angeles (PDT, -0700)
```

于是：

| 现象 | 实际 |
|---|---|
| 控制台「今日」 | 洛杉矶的今天（北京 06:10 时显示 `2026-10-22`，而用户日历是 10-23） |
| Key 日额度重置 | 北京时间 **15:00**（不是 00:00） |
| 按天趋势图 | 每天 09:00 北京时间为界切换 |

代码里没有任何 `TZ` / `time.Local` 赋值 / 设置项（`grep -rn "time.Local\s*=|TZ=" internal cmd deploy` 零命中），只有 `quota_keeper` 的定时任务时区。magpie 侧同一件事用显式时区参数。**修法**：加一个 `usage.location` 设置（默认 `Asia/Shanghai` 或 `time.Local`），dayKey 与 quotaWindow 都从它取；文档写明换算。

### N12-3（P3）登录限流的两个小瑕疵

- `handleAuthLogin` 先 `json.Decoder.Decode`（`admin_auth.go:496`）再 `rejectIfAuthThrottled`（`:502`）—— 已被限流的来源仍能做一次完整 JSON 解码；setup 正好相反（先限流后解码）。顺序应统一为"先限流"。
- `authThrottle.fail` 在条目数 ≥1024 时**每次失败都全表扫一遍**（`auth_throttle.go:65-71`），且**条目总量无上限**：窗口内 8 次失败/账号，攻击者换账号即可在 10 分钟内堆任意多条（每条 ~100B）。属慢速内存/CPU 放大，非紧急。

---

## 三、未提交「流中断」改造：既有 P0 复验（全部仍在，带行号）

| # | 问题 | 本轮证据 |
|---|---|---|
| P0-1 | 判定函数**恒 true** | `stream_interruption_close.go:21-23` `return r.r != nil`；5 个调用点全部在循环之前取值 |
| P0-2 | 「已缓冲可重试」前提不成立，会**重复正文** | `empty_stream_retry.go:166-177` `deferredSSEWriter.Write` 直接 `w.base.Write(p)`；作者自己的 `TestFinishConvertedProxyMidStreamDropIsRetryableWhenDeferred` 报 `got "partial"` |
| P0-3 | 多账号链路**丢心跳** | `protocol_proxy.go:628-645`：`deferBackup` 分支 `return` 在 `startSSEHeartbeat` 之前 |
| a | `runStreamConverter` 用 `ContentLength > 0` 判中断 | `protocol_proxy.go:586-592` |
| b | 真实 RST 不写收尾帧 | `passthrough_stream.go:250-253` |
| c | `closesUpstreamStream` 裸 `Contains` | `stream_interruption_close.go:39-46` |
| d | 无条件 `stop_reason:"max_tokens"` | `stream_interruption_close.go:148-152`（覆盖真实 `stop_sequence`/`tool_use`） |
| e | `streamRetryRecorder` 无实现 | `protocol_proxy.go:578-581`、`:623` |
| f | `countingReader.n` 只用于错误消息 | `empty_stream_retry.go:120-124` |
| i | `closeInterruptedResponsesToChatStream` 丢 usage | `stream_interruption_close.go:221` |
| j | `zz_dbg2_test.go` 调试脚手架在车上 | 仍在（`TestDbgWriterType`） |

---

## 四、历史遗留逐项复核（无提交 ⇒ 不可能被修，逐条带行号确认）

| # | 问题 | 当前证据 | 状态 |
|---|---|---|---|
| H-1 | `output_config.format` 带 `name` → Anthropic 400 | `convert_response_format.go:42` | ❌ |
| H-2 | `json_object` 原样发 Claude | `convert_response_format.go:19-20`、`:59-60` | ❌ |
| H-3 | adaptive 分支整体替换 `output_config` | `convert_claude_openai.go:321-324` | ❌ |
| H-4 | `allowed_tools` 塞进 Claude `tool_choice` | `convert_responses_claude.go:497-509` | ❌ |
| H-5 | 缓存断点白名单不含 `tool_result` | `grep -c tool_result convert_claude_cache_marks.go` → **0** | ❌ |
| H-6 | 思考余量补正生产不可达 | `claude_pass_through_normalize.go:151` | ❌ |
| H-7 | 非 5.2/5.3 的 `glm-*` 回落 `none` | `claude_thinking_off.go:239-241` | ❌ |
| H-8 | `knownClaude==false` 直接删 thinking | `claude_thinking_off.go:269-282` | ❌ |
| H-9 | 出图拒绝检查 `route.ProviderID` | `server.go:4206`（`imagesEffectiveProvider` 在 `:4216`） | ❌ |
| M-19 | 响应正文不脱敏入库 | `redact*` 只覆盖凭证 | ❌ |
| N-1 | workbuddy 空 `expires_at` → 永不续期 | `workbuddy.go:611-613` `return false` | ❌ |
| 假测试 | `TestClaudeToolChoiceWireShape` 恒真 | `convert_tool_restrictions_test.go:126-140` | ❌ |
| M-1 | Claude→Chat 无条件 stop | 已被条件守住 | ✅（round10 纠正） |
| A5 | Cursor 过期提前量 | 内建 5 分钟 | ✅（round10 纠正） |

---

## 五、完整排查清单（本轮复验后的总账）

### 5.1 协议转换

| ID | 项目 | 状态 | 备注 / 证据 |
|---|---|---|---|
| P1 | Claude→Chat 流式 usage 合并 | ✅ | `message_start` + `message_delta` 合并 |
| P2 | `stop_reason` 全量映射 | ✅ | `convert_claude_openai.go:427-438` |
| P3 | Chat→Responses 保留工具前正文 | ✅ | `a55c35d` |
| P4 | Responses→Chat reasoning 增量 | ✅ | — |
| P5 | `incomplete`/`max_output_tokens` | 🟡 | `1ad2f50` 修好，**被 WIP 覆盖**（测试报 `upstream_stream_interrupted`） |
| P6 | Chat→Claude block index / 关闭顺序 | ✅ | — |
| P7 | 工具 ID 哈希后缀 | ✅ | `convert_tools.go:113-135`（幂等：已合法 id 不再改） |
| P8 | Responses→Claude 合成 `tool_result` | ✅ | — |
| P9 | `redacted_thinking` / 反向 reasoning | 🟡 | 反向仍丢全部 reasoning |
| P10 | `strict`/`top_k`/`metadata`/`prompt_cache_key` | 🟡 | Claude 工具构造器仍无 |
| P11 | 单一 IR 架构（对齐 magpie `ir.go`） | ❌ | 架构级 |
| N11-1 | 长流 usage 解析用 256KB 头部副本 | 🔴 已部署 | `passthrough_stream.go:137-140` |
| **N12-1** | **同源放大：567 → 65,536 输出 token** | 🔴 **新·已部署** | `server.go:1330-1332` + `:134`；探针实证 |
| N11-2 | 心跳 goroutine 不 join | 🔴 已部署 | `sse_heartbeat.go:38-44` |
| N11-3 | 透传 Claude 收尾漏 `content_block_stop` | 🔴 WIP | `passthrough_stream.go:24-34` |
| N11-4 | 透传 Responses 收尾帧字段残缺 | 🟡 WIP | 缺 `id/model/sequence_number` |
| N10-1 | >256KB 完整流被判"中断" | 🔴 WIP | `passthrough_stream.go:242` |
| N10-2 | 收尾帧绕过心跳锁 | 🔴 WIP | `:246` |
| P0-1/2/3 | WIP 三条 P0 | 🔴 | 见第三节 |

### 5.2 路由 / 调度

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| R1 | RPM 限流 | 🟡 | 仅 Provider lane，无按 Key/用户 |
| R2 | 冷却跟随 Retry-After / resets_at | 🟡 | `provider_failover.go:489-493` 取 `max` → 30s 被拉成 60min；`:1206` 复探传空 hint 把 1h 缩回 2m |
| R3 | billing 不算硬额度 | ✅ | `6a1256b` |
| R4 | 截断续传 | 🔴 | WIP 会重复正文（P0-2） |
| R5 | session pin 落库 | ✅ | `30519bb`，两个小缺口 |
| R6 | per-account 闸门 + FIFO | 🟡 | `provider_lanes.go:155-157` 纯 RPM lane `max<=0` 直接放行；`close(broadcast)` 非 FIFO |
| **N12-2** | **日桶/日额度跟随服务器时区（LA）** | 🔴 **新·已部署** | `usage_accumulator.go:216`、`key_quota.go:91`；VPS TZ 实测 |

### 5.3 横切安全

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| S1 | 管理 API CORS | ✅ | 已白名单；`trae.go:383` 页面级 `*` 仍在（非管理 API） |
| S2 | 请求体大小上限 | ✅ | `request_body_limit.go:14`（256MB）+ 中间件 |
| S3 | 登录/注册/自检/pac/setup 限流 | 🟡 | 仅登录/setup/改密有；**N12-3** 顺序与 sweep 两个小瑕疵；`admin_auth.go:448` TOCTOU |
| S4 | 请求/响应脱敏 | 🟡 | 请求侧已做，响应侧未做（M-19） |
| S5 | 首包后空闲超时 | ❌ | `Timeout: 0` 现 **11 处**（`protocol_proxy.go:144,448`、`request_timing.go:291`、`server.go:5166,5284,5334,5414,5440,5563,5652`、`trae.go:1722`） |

### 5.4 Provider 账号

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| A1 | 刷新令牌加锁 | ✅ | `16c00d3`；锁表 3 瑕疵未收 |
| A2 | host 白名单 | ✅ | `zhipu_usage.go:30` |
| A3 | images 路由 / 全局 Key 回落 | 🟡 | 路由已收；H-9 判定错位仍在 |
| A4 | Claude 额度窗口 + 429 负缓存 | ❌ | `claude_oauth_usage.go:86-92` 无 `seven_day_overage_included` |
| A5 | Cursor 过期提前量 | ✅ | round10 纠正 |

### 5.5 控制台（能力缺口，本轮 grep 复核）

| 项目 | 状态 | 证据 |
|---|---|---|
| 实时请求流 | ❌ | `EventSource` / `/__stream` 在 `web/src` 零命中 |
| 路由解释聚合视图 | ❌ | `analysis.attempts` 已存未 surface |
| 会话视图 | ❌ | `sessionId` 仅 `types.ts:675` 声明 |
| 一键重放 | ❌ | `grep -ri replay web/src` 零命中 |
| 日志成本列 | ❌ | `traffic_log.tsx` 只有 in/out/cache 四列，`costMicros` 未用 |
| 告警未读角标 | ❌ | 无 `unread` 逻辑 |

---

## 六、验收门槛（合入前必须全过）

1. `go test ./internal/gateway/` 全绿（当前 5 顶层 / 7 子用例红）。
2. 判定改成"见到收尾帧"：删 `isUpstreamStreamInterruptedReader`，各转换器用自己已有的 `sawAnyEvent / messageStarted / [DONE]` 布尔量。
3. **N11-1 + N12-1**：usage 解析不得用头部截断的 `tee.Bytes()`；且"有流但解析失败"时**不得**用 `EstimateTokenUsage` 兜底（记 0 + 标记）。
4. N11-2：`stop()` 必须等 goroutine 退出（`<-exited`）。
5. N11-3/N11-4：收尾帧统一构造器，Claude 先 `content_block_stop`；Responses 带 `id/model/sequence_number`。
6. **N12-2**：引入可配置统计时区，dayKey、quotaWindow、前端日标签同源。
7. P0-2：转换输出先入 buffer、判定后一次下发，或只保留"一个字都没写过"的重试。
8. P0-3：心跳在任何可能阻塞客户端的分支之前启动；流中断不计 Provider 软失败。
9. 删 `zz_dbg2_test.go`、`streamRetryRecorder`、`countingReader.n`、`ContentLength` 推断、缓冲分支死 if。
10. 修 `TestClaudeToolChoiceWireShape`（恒真 = 没测）。

## 七、建议顺序

1. **N11-1 + N12-1 一起修**（纯统计/结算 bug，风险最低、收益最高，线上正在错，且方向相反地骗人）。
2. N11-2（一行等待）。
3. 修判定（门槛 2）→ 5 个红用例转绿。
4. 收尾帧族（N10-1/N10-2/N11-3/N11-4）一起做。
5. 统一缓冲/重试语义（P0-2）+ 补回心跳（P0-3）+ 流中断不计软失败。
6. N12-2 时区 + 老账：H-1…H-9、A4、控制台 6 项。

## 八、复现

```bash
git log -1 --format='%h %ci %s'                  # 6a1256b 01:28:25
git diff HEAD | sha256sum                        # 5dfa6dfe…
go build ./... && go vet ./internal/gateway/
go test ./internal/gateway/                      # 5 顶层红
# 用量兜底探针（复制到 internal/gateway/ 后跑，跑完删除）
cp .workbuddy/reviews/round12-evidence/zz_round12_probe_test.go.txt \
   internal/gateway/zz_round12_probe_test.go
go test ./internal/gateway/ -run TestZZRound12ProbeUsageFallback -v
# 时区
ssh root@173.249.207.202 'timedatectl | head -4'
```
