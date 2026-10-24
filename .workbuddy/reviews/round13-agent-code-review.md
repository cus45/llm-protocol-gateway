# 第十三轮 · 另一 Agent 改动代码审查（round13）

- 审查时间：2026-10-23 07:12–07:52（+0800）
- 审查范围：`HEAD = 6a1256b`（无新提交）+ 工作区 WIP（**审查期间对方恢复编辑**，见第二节）
- 定格快照：`git diff HEAD | sha256sum = cc9ea3f9…`，副本在 `.workbuddy/reviews/round13-evidence/snapshot-final/`
- 对照基准：`C:\guodevelop\demo\magpie`
- 基线：`go build ./...` ✅ / `go vet` ✅ / `go test ./internal/gateway/` ❌ **35 个用例红**；HEAD 干净树 **0 红**
- VPS（173.249.207.202:18093）：`active`、`/__health` 200、线上 `6a1256b`、`TZ=Asia/Shanghai`
- **本轮未修改任何业务代码、未提交**；副作用见第九节

---

## 结论先说

1. **HEAD 没有新提交，但工作区在我审查期间被改了 5 次。** 07:12 开始时指纹 `5dfa6dfe…`（与 round11/12 逐字节一致），07:19 起对方 agent 重新开始编辑，指纹依次变为 `c43a1602… → 26216d65… → 71378abe… → cc9ea3f9…`。所以本轮**不是"零新代码"**——有一批 07:19 之后的新改动要判，判完的结论是：**方向对，实现仍不能合入，且新引入了两个比原来更糟的问题**（第 3、4 条）。

2. **纠正前四轮的测量错误：红的不是 5 个用例，是 33–35 个。** 用干净 worktree 对照：

   | 树 | 结果 |
   |---|---|
   | `HEAD = 6a1256b`（干净 worktree） | `ok … 37.9s`，**0 红** |
   | 工作区 WIP（07:12 快照） | **33 红** |
   | 工作区 WIP（07:52 快照） | **35 红**（对方又加了 2 个测试，也红了） |

   红的是**四个方向的全部 SSE 转换测试**。前几轮记的"5 顶层 / 7 子用例"来自过滤子集运行，让工作量看起来只有实际的 1/7。**这个数字必须改，否则接手人会严重低估。**

3. **新改动把 P1/P3/P4/P5/P6 已验收的修复一起打红了，其中 P3 是"输出 0 个 item"这种静默吞正文**：

   ```
   StreamClaudeToOpenAIUsageKeepsPromptTokens        (P1) upstream stream ended after 544 bytes without its closing event
   ChatStreamToResponsesKeepsPreambleBeforeToolCall  (P3) output has 0 items, want message + function_call
   ResponsesStreamToChatEmitsReasoningDeltas         (P4) upstream stream ended after 949 bytes
   ChatToResponsesStreamEmitsIncompleteEvent         (P5) 被中断判定抢占
   ChatStreamToClaudeClosesToolBlocksInOrder         (P6) 同上
   ```

4. **新缓冲设计自己的测试三条全红，且症状互相矛盾**（同一份代码，三条路径三种错法）：

   ```
   TestFinishConvertedProxyMidStreamDropIsRetryableWhenDeferred
       client should not see the interrupted attempt, got "…content":"partial"…
   TestFinishConvertedProxyMidStreamDropRetryableOnLastAttempt
       buffered attempt leaked to the client: "data: partial\n\n"
   TestNoFailoverReplayAfterMidStreamInterruption
       client saw the text 0 times: ""
   ```

   前两条说"缓冲没兜住、正文泄给了客户端"，第三条说"客户端一个字都没收到"。也就是说：**它既没做到"只在完整时才下发"，也没做到"中断时重试一次"**，而是按路径随机落到两种坏结果之一。

5. **纠正 round12 的 N12-2（时区）**：生产**没有**"日额度 15:00 重置"。systemd 单元显式 `TZ=Asia/Shanghai`，进程实测 `/proc/832506/environ` 含 `TZ=Asia/Shanghai`，启动日志 `+08:00`。round12 只看 `timedatectl`（系统 LA）就下了结论，漏了服务级覆盖。降级为"仅 Docker/K8s/裸跑成立"，见 N13-3。

6. 另外新增三条（与 WIP 无关，HEAD 就有）：
   - **N13-1（P2）** `gateway --version` **不打印版本，直接启动服务**——`cmd/gateway/main.go` 零 flag 解析。线上实测它打开了默认库并写入 226KB WAL。
   - **N13-2（P3）** `/__usage-analysis` 无上限全量拉取：7 天 = 14,045 条 / 13.3MB / 2.26s，线性放大。
   - **N13-3（P3）** 部署层没有 TZ 兜底，且 `verify-vps-env.sh` 期望的是 `America/Los_Angeles`——按它的提示操作会把刚修好的时区弄坏。

---

## 一、范围与证据

```bash
git log -1 --format='%h %ci %s'   # 6a1256b 2026-10-23 01:28:25 +0800（无新提交）
git diff HEAD | sha256sum         # 07:12 5dfa6dfe… → 07:19 c43a1602… → 26216d65… → 71378abe… → 07:52 cc9ea3f9…
git worktree add .workbuddy/review-head 6a1256b
(cd .workbuddy/review-head && go test ./internal/gateway/)   # ok，0 红
go test ./internal/gateway/                                   # 33 红(07:12) → 35 红(07:52)
```

| 检查项 | 结果 |
|---|---|
| 新提交（round12 之后） | **0** |
| 工作区 | 审查期间被改 5 次，仍在编辑 |
| `go build ./...` | 通过（每个快照都通过） |
| `go test`（工作区） | **35 红** |
| `go test`（HEAD 干净树） | **0 红** |
| VPS | `active`，health 200，`TZ=Asia/Shanghai`，`6a1256b` |

---

## 二、07:19 之后的改动：新增了什么

| 文件 | 改动 | 意图 |
|---|---|---|
| `empty_stream_retry.go` | 新增 `isUpstreamStreamInterruption`、`isUpstreamStreamInterruptedResponse`、`upstreamStreamInterruptionError`、`countingReader`；`deferredSSEWriter.Write` 改为"有备用链路时写进 failover 缓冲" | 让转换流也能在中断时重试 |
| `provider_failover.go` | 新增 `deferring()`、`recordsForRetry()`、`attemptFailoverReason()`；`deferBackup` 判定改用类型断言 | 同上 |
| `protocol_proxy.go` | `finishConvertedProxy` 增加 `deferBackup` 分支：中断则 `return …, true`（retryable），不提交/不 flush | 同上 |
| `stream_interruption_retry_test.go` | 新增 `TestNoFailoverReplayAfterMidStreamInterruption`、`TestFinishConvertedProxyMidStreamDropRetryableOnLastAttempt` | 自证 |
| `zz_dbg2_test.go` | **已删除**（round7–12 一直在报的调试脚手架） | ✅ 这条算修了 |

方向是对的：转换流确实需要"先攒、确认完整再下发"才能安全重试。**但落地有 6 个问题，其中 D-1/D-2 是阻塞级。**

---

## 三、新改动的判断（逐条）

### D-1（P0，阻塞）`deferBackup` 被类型断言做成了恒真，最后一次尝试也谎称"可重试"

```go
// protocol_proxy.go:619-624
deferBackup := false
if fw, ok := w.(*failoverResponseWriter); ok {
	deferBackup = fw.canDefer && !fw.passthrough
}
if _, ok := w.(streamRetryRecorder); ok {   // ← 恒成立
	deferBackup = true
}
```

`failoverResponseWriter` 自己实现了空方法 `recordsForRetry()`（`provider_failover.go:227`），所以这个断言对**任何** failover 写入器都成立，与 `canDefer` 无关；`canDefer` 的定义恰恰是 `i < len(attempts)-1`（`provider_failover.go:959`），最后一次尝试是 `false`。

而"能不能缓冲"是由 `WriteHeader` 决定的：

```go
// provider_failover.go:250-257
if !w.canDefer || statusCode < 400 {
	w.commitHeaders(statusCode)   // → passthrough = true，此后再也丢不掉
	return
}
```

于是**最后一次尝试**：`canDefer=false` → 立刻 `commitHeaders` → `passthrough=true` → 字节直通客户端；但上面又把 `deferBackup` 置 true、返回 `retryable=true`，让 failover 循环去重试。这就是作者自己那条测试的报错：

```
buffered attempt leaked to the client: "data: partial\n\n"
```

**修法**：`deferBackup` 只能由"这个写入器此刻真的还能丢弃"决定（`fw.canDefer && !fw.passthrough && !fw.discarded`），删掉 `recordsForRetry()` 这个恒真标记；"最后一次尝试也能重试"要靠显式的 `writer.ResetForRetry()`，不能靠类型断言假装。

### D-2（P0，阻塞）"已缓冲、客户端什么都没看到"的前提在首条路径上也不成立

`TestFinishConvertedProxyMidStreamDropIsRetryableWhenDeferred` 报 `client should not see the interrupted attempt, got "…partial…"`。这条测的正是 `canDefer=true` 的主路径——说明即使 `deferring()` 为真，仍有字节写到了 base。

两个可疑点：

- `deferredSSEWriter.Write` 在 buffering 分支里先 `w.copyHeadersLocked(buffered.Header())` 再 `buffered.Write(p)`；而 `failoverResponseWriter.Write` 在 `!w.wroteHeader && !w.passthrough` 时会调 `WriteHeader(200)` —— 一旦哪次 `wroteHeader` 被重置（`Discard()` 会重置），`canDefer=false` 的写入器就会走 `commitHeaders`。
- 反之第三条测试 `TestNoFailoverReplayAfterMidStreamInterruption` 报 `client saw the text 0 times: ""`——说明重试路径把该发的东西也吞了。

**结论**：缓冲与"能否丢弃"目前是两套独立状态（`deferredSSEWriter.wroteHeader` + `failoverResponseWriter.passthrough`），没有单一真源。必须先合并成一个状态机（例如 `attemptState{open|buffered|committed|discarded}`），再谈重试。

### D-3（P1）`deferBackup` 分支**完全不起心跳**

`startSSEHeartbeat` 仍只在 else 分支（`protocol_proxy.go:646`）：

```go
if deferBackup {
	usage, streamErr := runStreamConverter(dw, response, model, streamConvert)  // 无心跳
	...
}
stopHeartbeat := startSSEHeartbeat(dw, sseHeartbeatInterval)   // 只有这条路径有
```

有备用账号的转换流在长思考期间**一个字节都不下发**（缓冲模式下本来也无字节可发），Cloudflare / 反代 100s 空闲直接切断。这正是 round9 报的 P0-3，**没修**，而且新缓冲把它从"偶尔"变成"每个有备用链路的请求"。心跳必须放在缓冲之外（往 base 直接写 SSE 注释，或让缓冲层支持"旁路心跳"）。

### D-4（P1）`isUpstreamStreamInterruptedResponse` 注释与代码相反

```go
// 注释：The length is deliberately not trusted
if resp.ContentLength < 0 {
	return !sawClosingFrame
}
return false          // ← ContentLength >= 0 时"信任长度"，直接判为未中断
```

注释说长度不可信，代码在 `ContentLength >= 0` 时恰恰**信了长度**。两个分支里必有一个是错的；对被截断但有 Content-Length 的响应，这里会返回"没中断"。

### D-5（P2）`attemptFailoverReason` 拿 HTTP 错误正文当传输错误匹配

```go
func attemptFailoverReason(body []byte) string {
	if isUpstreamStreamInterruption(errors.New(string(body))) {
		return attemptReasonStreamInterrupted
	}
	return attemptReasonUpstreamError
}
```

`body` 是上游返回的 **HTTP 错误响应体**，不是传输错误。若错误正文里恰好含 `"unexpected eof"`、`"connection reset"` 之类的文字（中转服务的报错很常见），这个尝试就会被标成 `stream_interrupted`，污染 `analysis.attempts`。而且 `string(body)` 对几百 KB 的错误页是整块拷贝。此函数对"流被截断"这个真实场景**根本不会被调用到**（那条路径走的是 `streamErr`），属于为了填字段硬造的函数。

### D-6（P2）热路径仍留 3 处 `println("DBG...")`

07:19 时有 7 处，对方随后删了 4 处，**定格快照仍剩 3 处**：

```
internal/gateway/passthrough_stream.go:187   println("DBG3 direct path", fmt.Sprintf("%T", w))
internal/gateway/provider_failover.go:260    println("DBG6 write", w.passthrough, len(p))
internal/gateway/provider_failover.go:282    println("DBG6 commit", statusCode, w.canDefer, len(w.buf.Bytes()))
```

`Write` 每帧都打一次，直连 stdout、绕过 slog、不受日志级别控制。这是"提交前必须清干净"的清单项（round8–12 已报过两次同类问题，`zz_dbg2_test.go` 这次删了，但 `println` 又留在生产文件里）。

---

## 四、本轮纠错（三条）

### C-1（测量）WIP 红用例：5 → **33/35**

前四轮记录的"5 顶层 / 7 子用例红"是过滤子集运行的结果。全量运行：四个转换方向的流式用例全红。**影响**：round12 验收门槛写的"改一处即可全绿"是错的，实际要先处理 35 个用例 + 5 条已验证修复的回归。

### C-2（结论）N12-2 时区在日常生产**不成立**

```bash
$ tr "\0" "\n" < /proc/832506/environ | grep ^TZ=
TZ=Asia/Shanghai
$ systemctl show llm-protocol-gateway -p Environment
Environment=... TZ=Asia/Shanghai ...
```

启动日志 `time=2026-10-23T01:29:24.463+08:00`。所以 `usage_accumulator.go:216`、`key_quota.go:91` 在当前部署解析为北京时区，日桶、Key 日额度、"今日"都对。

**降级为**：代码缺可配置项 + 部署层缺兜底，风险在 Docker/K8s/裸跑（N13-3）。不是"线上正在错账"。

### C-3（工具）`verify-vps-env.sh` 与单元文件互相打脸

```bash
$ grep -n EXPECTED_TZ scripts/verify-vps-env.sh
20:EXPECTED_TZ="${EXPECTED_TZ:-America/Los_Angeles}"
68:  warn "系统时区 = $TZ_NOW，预期 $EXPECTED_TZ — 修复: sudo timedatectl set-timezone $EXPECTED_TZ"
```

脚本默认期望 LA、发现不一致就提示"改成 LA"——**照做就把刚被 `TZ` 修好的日额度弄回错误状态**。

---

## 五、本轮新发现（与 WIP 无关，HEAD 已存在）

### N13-1（P2）`--version` 会启动整个服务

`cmd/gateway/main.go` 全文 41 行，**零 flag 解析**。线上实测 `gateway --version`：

```
msg="database store" path=/root/.config/llm-protocol-gateway/gateway.db
msg="config loaded" path=... providers=0 routes=0 models=0 apiKeys=0
msg="endpoint advertise" lanHost=173.249.207.202 port=18093 webExposed=true
```

副作用实测：

```
-rw------- 4096 2026-10-08 23:37:44 gateway.db
-rw-r--r-- 226632 2026-10-22 16:17:20 gateway.db-wal   ← 本次探测写入
```

风险：巡检脚本习惯性 `--version` → 没有版本输出反而起服务；若环境变量指向生产库，等于对正在服务的库跑迁移并抢端口；且**二进制不含 `vcs.revision`**，线上到底跑哪个提交只能靠 mtime 猜（round7 就是这么干的）。

**修法**：`flag` 解析 + `--version` 打印 `debug.ReadBuildInfo()` 的 `vcs.revision/vcs.time` + `-buildvcs=true`；未知参数直接报错退出。

### N13-2（P3）`/__usage-analysis` 无上限

```go
// usage_analysis.go:104-135
for query.Page = 1; ; query.Page++ {   // PageSize 500，无条数上限
	page = s.requestLogStore.QueryRequestLogs(query)
	items = append(items, page.Items...)
}
writeJSONGzip(w, r, 200, map[string]any{"items": items, ...})
```

生产实测（7 天，`Accept-Encoding: identity`）：

```
items=14045 total=14045   http=200  time=2.264s  size=13,321,037 bytes
analysis avg=366B/行
```

今天不算事故（约 50–70MB 瞬时堆），但线性放大：14 万条/7 天就是 20s+ 与数百 MB，而管理页一打开就会打这个接口。retention 只限天数不限行数。**修法**：聚合下推到日桶/SQL，明细分页，接口加硬上限。

### N13-3（P3）时区没有部署兜底

| 位置 | TZ |
|---|---|
| `Dockerfile` | 无 |
| `docker-compose.yml` | 无 |
| `deploy/k8s.yaml` | 无 |
| `deploy/deploy-linux.sh` | 不建 unit，不设 TZ |
| VPS systemd unit（手工） | `TZ=Asia/Shanghai` ✅ |
| `scripts/verify-vps-env.sh:20` | 期望 `America/Los_Angeles` ❌ |

**唯一让生产正确的东西是一个不在仓库里的手工 unit 设置。** 重建 unit、上容器、或按脚本建议改时区，日额度与"今日"都会错。**修法**：`usage.location` 设置项（默认 `Asia/Shanghai`）+ 容器显式 `TZ` + 修脚本预期。

---

## 六、历史遗留复验（抽验行号，均仍在）

| # | 问题 | 本轮证据 | 状态 |
|---|---|---|---|
| H-1 | `output_config.format` 带 `name` → 400 | `convert_response_format.go:41` | ❌ |
| H-2 | `json_object` 原样发 Claude | `convert_response_format.go:19-20` | ❌ |
| H-3 | adaptive 分支整体替换 `output_config` | `convert_claude_openai.go:321-324` | ❌ |
| H-4 | `allowed_tools` 塞进 `tool_choice` | `convert_responses_claude.go:497-509` | ❌ |
| H-5 | 缓存断点不含 `tool_result` | `grep -c tool_result convert_claude_cache_marks.go` → **0** | ❌ |
| H-6 | 思考余量补正生产不可达 | `claude_pass_through_normalize.go:151` | ❌ |
| H-7 | 非 5.2/5.3 的 `glm-*` 回落 `none` | `claude_thinking_off.go:239-241` | ❌ |
| H-8 | `knownClaude==false` 删 thinking | `claude_thinking_off.go:269-282` | ❌ |
| H-9 | 出图拒绝检查 `route.ProviderID` | `server.go:4207` vs `:4216` | ❌ |
| M-19 | 响应正文不脱敏 | `redact*` 只覆盖凭证 | ❌ |
| N-1 | workbuddy 空 `expires_at` → 永不续期 | `workbuddy.go:611-612` | ❌ |
| 假测试 | `TestClaudeToolChoiceWireShape` 恒真 | `convert_tool_restrictions_test.go:127`：只 Marshal 一份字面量，不经任何转换器 | ❌ |
| M-1 | Claude→Chat 无条件 stop | 已被条件守住 | ✅（round10 纠正） |
| A5 | Cursor 提前量 | 内建 5 分钟 | ✅（round10 纠正） |
| N12-2 | 日桶跟随服务器时区 | 生产由 unit `TZ` 覆盖 | 🟡（本轮降级） |
| zz_dbg2_test.go | 调试脚手架 | **已删除** | ✅（本轮修掉） |

---

## 七、完整排查清单（本轮复验后的总账）

### 7.1 协议转换

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| P1 | Claude→Chat 流式 usage 合并 | ✅ | **WIP 下回归** |
| P2 | `stop_reason` 全量映射 | ✅ | `convert_claude_openai.go:427-438` |
| P3 | Chat→Responses 保留工具前正文 | ✅ | **WIP 下输出 0 item** |
| P4 | Responses→Chat reasoning 增量 | ✅ | **WIP 下回归** |
| P5 | `incomplete`/`max_output_tokens` | 🟡 | **被 WIP 覆盖** |
| P6 | Chat→Claude block index | ✅ | **WIP 下回归** |
| P7 | 工具 ID 哈希后缀 | ✅ | `convert_tools.go:113-135` |
| P8 | 合成 `tool_result` | ✅ | — |
| P9 | `redacted_thinking` / 反向 reasoning | 🟡 | 反向仍丢全部 |
| P10 | `strict`/`top_k`/`metadata`/`prompt_cache_key` | 🟡 | Claude 构造器仍无 |
| P11 | 单一 IR 架构 | ❌ | 架构级 |
| N11-1 | 长流 usage 用 256KB 头部副本 | 🔴 已部署 | `passthrough_stream.go:137-140` |
| N12-1 | 567 → 65,536 输出 token 放大 | 🔴 已部署 | `server.go:1330-1332` + `:134` |
| N11-2 | 心跳 goroutine 不 join | 🔴 已部署 | `sse_heartbeat.go:38-44` |
| N11-3 | 透传 Claude 收尾漏 `content_block_stop` | 🔴 WIP | — |
| N11-4 | 透传 Responses 收尾帧残缺 | 🟡 WIP | 缺 `id/model/sequence_number` |
| N13-0 | **WIP 全量红：33→35** | 🔴 **本轮纠正** | 干净树 0 红 |
| D-1 | `deferBackup` 恒真，最后一次尝试谎称可重试 | 🔴 **新** | `protocol_proxy.go:619-624` |
| D-2 | 缓冲状态双真源，正文泄漏 / 全吞 | 🔴 **新** | 3 条自测互斥报错 |
| D-3 | `deferBackup` 分支无心跳 | 🔴 **新** | `protocol_proxy.go:646` 只在 else |
| D-4 | `isUpstreamStreamInterruptedResponse` 注释反代码 | 🟡 **新** | `empty_stream_retry.go` |
| D-5 | `attemptFailoverReason` 拿响应体当传输错误 | 🟡 **新** | `provider_failover.go` |
| D-6 | 热路径 `println("DBG…")` ×3 | 🔴 **新** | 见第三节 |

### 7.2 路由 / 调度

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| R1 | RPM 限流 | 🟡 | 仅 Provider lane |
| R2 | 冷却跟随 Retry-After / resets_at | 🟡 | 取 `max` → 30s 被拉成 60min；复探把 1h 缩回 2m |
| R3 | billing 不算硬额度 | ✅ | `6a1256b` |
| R4 | 截断续传 | 🔴 | D-1/D-2 |
| R5 | session pin 落库 | ✅ | 两个小缺口 |
| R6 | per-account 闸门 + FIFO | 🟡 | `provider_lanes.go:157` 纯 RPM lane 直接放行；非 FIFO |
| N12-2 | 日桶/额度跟随服务器时区 | 🟡 | 生产已有 `TZ`；风险在容器/裸跑 |

### 7.3 横切安全

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| S1 | 管理 API CORS | ✅ | `trae.go:383` 页面级 `*` 仍在（非管理 API） |
| S2 | 请求体上限 | ✅ | `request_body_limit.go:14`（256MB） |
| S3 | 认证限流 | 🟡 | 顺序与 sweep 瑕疵；`admin_auth.go:448` TOCTOU |
| S4 | 脱敏 | 🟡 | 响应侧未做（M-19） |
| S5 | 首包后空闲超时 | ❌ | `Timeout: 0` 仍 11 处 |
| N13-1 | `--version` 起服务、版本不可追溯 | 🔴 新 | main.go 零 flag |
| N13-2 | `/__usage-analysis` 无上限 | 🟡 新 | 13.3MB/2.26s@14k 行 |

### 7.4 Provider 账号

| ID | 项目 | 状态 | 备注 |
|---|---|---|---|
| A1 | 刷新令牌加锁 | ✅ | `16c00d3`；锁表 3 瑕疵未收 |
| A2 | host 白名单 | ✅ | `zhipu_usage.go:30` |
| A3 | images 路由 / 全局 Key 回落 | 🟡 | H-9 判定错位仍在 |
| A4 | Claude 额度窗口 + 429 负缓存 | ❌ | 无 `seven_day_overage_included` |
| A5 | Cursor 提前量 | ✅ | round10 纠正 |

### 7.5 控制台

| 项目 | 状态 | 证据 |
|---|---|---|
| 实时请求流 | ❌ | 无 `EventSource` / `/__stream` |
| 路由解释聚合视图 | ❌ | `analysis.attempts` 已存未 surface |
| 会话视图 | ❌ | `sessionId` 仅类型声明 |
| 一键重放 | ❌ | grep `replay` 零命中 |
| 日志成本列 | ❌ | `traffic_log.tsx` 无 cost 列 |
| 告警未读角标 | ❌ | 无 `unread` |

### 7.6 部署 / 运维

| ID | 项目 | 状态 |
|---|---|---|
| N13-3 | 时区无容器兜底 + 巡检脚本方向反了 | 🟡 新 |
| 版本可追溯 | ❌ | N13-1 |

---

## 八、验收门槛（合入前必须全过）

1. `go test ./internal/gateway/` **35 红 → 0 红**（以干净树为基线，不要再按子集报数）。
2. **D-1**：删 `recordsForRetry()` 恒真标记；`deferBackup` 只由 `fw.canDefer && !fw.passthrough && !fw.discarded` 决定；最后一次尝试的重试走显式 `ResetForRetry()`。
3. **D-2**：把 `deferredSSEWriter.wroteHeader` 与 `failoverResponseWriter.passthrough` 合并成单一状态机，且"已提交"后禁止再声明可重试。
4. **D-3**：心跳必须在缓冲之外启动（旁路 SSE 注释），deferBackup 分支也要有。
5. **D-4/D-5**：删或重写 `isUpstreamStreamInterruptedResponse`、`attemptFailoverReason`；不要用响应体字符串匹配传输错误。
6. **D-6**：清空 `println("DBG…")`（快照剩 3 处），并加 `go vet` 之外的 CI grep 守卫。
7. 判定改成"见到收尾帧"：删 `isUpstreamStreamInterruptedReader`（现为 `return r.r != nil`），各转换器用自己已有的 `sawAnyEvent / messageStarted / [DONE]` 布尔量。
8. N11-1 + N12-1：usage 不得用截断的 `tee.Bytes()`；"有流但解析失败"不得用 `EstimateTokenUsage` 兜底。
9. N11-2：`stop()` 必须 `<-exited`。
10. N11-3/N11-4：收尾帧统一构造器。
11. **N13-1**：flag 解析 + `--version` + `-buildvcs=true`。
12. **N13-3**：`usage.location` + 容器 `TZ` + 修 `verify-vps-env.sh`。

---

## 九、本轮副作用披露

1. 为取实证在 VPS 执行了**一次** `gateway --version`（当时不知道它没有 flag 解析）。它打开了 `/root/.config/llm-protocol-gateway/gateway.db`（**2026-10-08 遗留的 4KB 空库**，与生产库 `/var/lib/llm-protocol-gateway/gateway.db` 无关），写入 **226,632 字节 WAL**。生产库未被触碰。该 WAL **未删除**（远程删库文件属高风险，留运维判断）；建议确认该目录无用后整体移除。
2. 读取 `/__usage-analysis` 一次（7 天区间，单次只读，2.26s）；`/tmp/ua.json` 已删除。
3. 本地对照 worktree `.workbuddy/review-head` 已移除；工作区文件未被我修改。

---

## 十、复现

```bash
# 树是否变了（本轮指纹变过 5 次）
git log -1 --format='%h %ci %s'
git diff HEAD | sha256sum          # 定格 cc9ea3f9…

# 基线对照：HEAD 干净树 0 红 vs 工作区 35 红
git worktree add .workbuddy/review-head 6a1256b
(cd .workbuddy/review-head && go test ./internal/gateway/)
go test ./internal/gateway/ 2>&1 | grep -c '^--- FAIL'

# 新缓冲自测（三条互斥报错）
go test ./internal/gateway/ -run 'TestFinishConvertedProxyMidStreamDrop|TestNoFailoverReplayAfterMidStreamInterruption' -v

# D-1：deferBackup 恒真
sed -n '615,630p' internal/gateway/protocol_proxy.go
sed -n '250,260p' internal/gateway/provider_failover.go

# D-3：心跳只在 else 分支
grep -n startSSEHeartbeat internal/gateway/protocol_proxy.go

# D-6：残留调试打印
grep -rn 'println("DBG' --include=*.go internal/gateway/ | wc -l

# N13-1
grep -n 'flag\.\|os.Args' cmd/gateway/main.go      # 零命中

# 时区（纠正 round12）
ssh root@173.249.207.202 'tr "\0" "\n" < /proc/832506/environ | grep ^TZ='
ssh root@173.249.207.202 'grep -n EXPECTED_TZ /opt/llm-protocol-gateway/scripts/verify-vps-env.sh 2>/dev/null || true'
```
