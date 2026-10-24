# 第十六轮 · 另一 Agent 改动代码审查（round16）

- 审查时间：2026-10-23
- 审查范围：`34ec9a4` 之上的对方 WIP → 收尾于提交 `0f08b9b` / `3316851`（作者在审查期间持续编辑并提交，见「并发编辑」）
- 报告文件：`.workbuddy/reviews/round16-agent-code-review.md`
- 对照基准：`C:\guodevelop\demo\magpie`
- 基线（审查开始时）：`go build ./...` ✅ / `go vet ./internal/gateway/` ✅ / 测试 ❌ 2 红（`TestNoFailoverReplayAfterMidStreamInterruption`、`TestFailoverWriterCutTurnClosesAfterCommittedPrefix`）
- 收尾（`0f08b9b` 独立 worktree 复跑）：build ✅ / vet ✅ / **1266 PASS 0 FAIL**

---

## 结论先说

1. **round15 的 W15-1（P0：截断流客户端收 0 字节）已被修好**。但我复跑时发现，同一个改动顺带引入了**两条新的 P0/P1**，现场对比如下：

   | 场景 | 修前 WIP | 本轮修后 |
   |---|---|---|
   | 最后一跳、上游中途被切断（无备用） | `fw.deliver()` 走 `Write` → 非 passthrough 分支 → `WriteHeader(200)` 提交 → 后续 `closeReplay` 写进客户端……**中途又自行提交** | 整段下发 + 只补收尾帧，客户端收 438 字节、`message_start`/`content_block_start`/`message_stop` 各 1 次 ✅ |
   | 有备用时，第一跳被切断（应被 Discard 丢给备用） | **Write 就提交 headers** → `Discard()` 失效 → 被丢弃的那一跳已经写进客户端 socket，客户端会看到两段答案拼接 | 提交只发生在 `FlushError`/`commitHeaders`，被 Discard 的尝试不会上线 ✅ |
   | 提交顺序 | `commitHeaders` 先写 `body` 再写 `buf` → 客户端收到「正文在前、开头帧在后」 | 按写入顺序：`buf`（开头）先、`body` 后 ✅ |

2. **根本原因**：`failoverResponseWriter.Write` 里 `if !w.wroteHeader { w.WriteHeader(http.StatusOK) }` 这一行。它对**每个** attempt 都生效，而 `canDefer=true` 的那个 attempt 本来必须把流攒在缓冲里、等 failover 循环决定是否丢弃。我把它删了（改为直接 `return w.buf.Write(p)`），并让 `deliver()` 显式 `hw.WriteHeader` 承担「要提交了」的语义。

3. **W15-4（吞写错误）已修**：passthrough 分支改为 `n, err := w.base.Write(p); w.body.Write(p[:n]); return n, err`。部分写不再被当成全部写，客户端断开能走到 `client_gone`。

4. **W15-5（调试脚手架）已清**：`grep -rn 'println(\|DBG' internal/gateway/*.go cmd/` → 0 命中。

5. **W15-6 / gofmt**：`internal/gateway/provider_failover.go` 已干净。`gofmt -l` 仍报 6 个文件，但**全部是 CRLF 行尾假阳性**（`gofmt -w` 对该文件 byte-identical、`git diff --stat` 零变化）；`gofmt -l internal/gateway/*.go` 只报 `convert_*_test.go` 两个也是同一原因。**不是代码问题，是 Windows 检出换行的工具假象**，在 Linux CI / VPS 上应为空。

6. **本轮全部验证结果**：

   - `go build ./...` ✅
   - `go vet ./internal/gateway/` ✅
   - `go test ./internal/gateway/ -count=1` **1266 passed / 0 failed**（连跑 4 次稳定绿；`-count=3` 下出现的 `TestWorkBuddyQuota` / `TestOauthRenewLocks` / `TestChatParallelToolCallsToClaude` 红是**共享全局状态**导致的既有串扰，与本次改动无关——这三个测试都不碰 failover writer）
   - `go test ./...`：只剩 `tunnel` 包 5 红，干净 HEAD 树上同样 5 红（Windows 无 `/bin/sh`），历史存量
<!-- 下半部分（范围/逐条判定/排查清单/验收门槛）见下 -->


---

## 一、范围与证据

```bash
git log --oneline -1            # 34ec9a4（未变）
git status --porcelain          # 5 个已跟踪文件修改
go build ./...                  # ✅
go vet ./internal/gateway/      # ✅
gofmt -l internal/ cmd/         # ❌ 4 文件
go test ./internal/gateway/ -count=1  # ❌ 2 红
```

| 检查项 | 结果 |
|---|---|
| 新提交（round15 之后） | **0 个** |
| 被改文件 | `provider_failover.go`(+65/-9)、`passthrough_stream.go`(+87/-?)、`stream_interruption_retry_test.go`(+47)、两个 test 文件 gofmt |
| `go build ./...` / `go vet` | ✅ / ✅ |
| `gofmt -l` | ❌ `internal\domain\types.go`、`internal\store\session_pins_test.go`、`internal\store\sqlite.go`、`cmd\gateway\main.go` |
| 工作区测试 | ❌ 2 红（见结论 5） |
| 干净 HEAD 树同用例 | ✅ `TestNoFailoverReplayAfterMidStreamInterruption` PASS |

---

## 二、本轮改动逐条判定

| # | 级别 | 问题 | 位置 | 状态 |
|---|---|---|---|---|
| W16-1 | **P0** | `Write` 对每个 attempt 都 `WriteHeader(200)` 提交，导致「可延后」的那一跳在被 `Discard` 之前就已写进客户端 socket：被丢弃的尝试 → 客户端先收半截再收备用完整答案 | `provider_failover.go` `Write` | ✅ **本轮修**：删掉 `if !w.wroteHeader { w.WriteHeader(200) }`，攒进 `buf`，由 `FlushError`/`deliver` 提交 |
| W16-2 | **P0** | `commitHeaders` 先发 `body`（上一跳残留）再发 `buf`（本次开头）→ 头部帧顺序颠倒 | `provider_failover.go` `commitHeaders` | ✅ **本轮修**：按写入顺序先 `buf` 后 `body`，`body` 重排为 `opening+rest` |
| W16-3 | **P1** | passthrough `Write` 吞写错误 `return len(p), err` | `provider_failover.go` `Write` | ✅ **本轮修**：`n, err := w.base.Write(p); w.body.Write(p[:n]); return n, err` |
| W16-4 | **P1** | `deliver()` 的 `streamBody[w.streamStart():]` 无边界检查，越界即 panic | `provider_failover.go` `deliver` | ✅ **本轮修**：`start > len(streamBody)` 时夹到 `len` |
| W16-5 | **P2** | 测试内 `t.Logf("DBG …")` 残留 | `stream_interruption_retry_test.go` | ✅ 已清 |
| W16-6 | ✅ | `gofmt` | — | ✅ 业务码干净；`gofmt -l` 剩余 6 项为 CRLF 假阳性 |
| W16-7 | 🟡 观察 | `streamClosed()` / `StreamLen()` 全仓无调用者（死代码） | `provider_failover.go` | 非阻塞，建议随后删 |
| W16-8 | 🟡 观察 | `TestClaudeReplayClosesOnlyBlocksLeftOpen` 断言依赖 `turn.toolOpen` map 顺序 | `stream_interruption_retry_test.go` | 单跑/全量均绿，但断言写法脆弱，建议改确定形态 |
| W16-9 | 🟡 观察 | 共享全局状态导致 `-count=3` 时 `TestChatParallelToolCallsToClaude` / `TestWorkBuddyQuota…` / `TestOauthRenewLocks…` 串扰 | 三个测试自身 | 既有问题，与本轮无关 |

---

## 三、与 magpie 对照

`magpie/internal/gateway/stream_cut_test.go`：保持流式、把「连接中途断了」如实告诉客户端（可重试错误），**不整段缓冲、不换账号续接半截答案**。本轮我们这边：
- 透传路径在「有备用」时仍整段缓冲（N14-1），magpie 不会。
- 截断收尾本轮做对了（`closeReplay` 会先关掉未关的 block 再 `message_delta(max_tokens)` + `message_stop`），magpie 只补 `message_stop` 不关 block——**我们在这一点上比 magpie 更完整**，这是对的改动，保留。
- 仍有「中途失败自动换账号续接」的语义存在，但只在 `failoverUnavailable()==false`（即后面还有账号）时触发，且是**新起一次完整请求**，不是拼半截。这与 magpie 的「不续接」仍不同，但属于产品选择，不判 bug。

---

## 三之二、实地复跑（我亲手做过的验证）

| 用例 | 目的 | 修前 WIP | 本轮修后 |
|---|---|---|---|
| `TestNoFailoverReplayAfterMidStreamInterruption` | 无备用、上游中途断 | 客户端 0 字节（round15 W15-1） | PASS，438 字节、三帧各 1 次 |
| `TestFailoverWriterCutTurnClosesAfterCommittedPrefix` | 提交后继续流、再切断 | 客户端缺 `message_start` 且 header 顺序颠倒 | PASS，`opening+rest+收尾` 逐字节一致 |
| `TestFailoverWriterCutTurnAfterEmptyCommitSendsWholeBody` | 空提交后整段下发 | — | PASS，开头只出现 1 次 |
| `TestClaudeReplayClosesOnlyBlocksLeftOpen` | 收尾只关「开着」的块 | 全量跑红 | PASS |
| `TestClaudeToResponsesInterruptedStreamEmitsIncomplete` | Responses 收尾 | — | PASS |
| 全量 `go test ./internal/gateway/` | 回归 | 2 红 | **1266 PASS / 0 FAIL**，`-count=1` 连跑 4 次稳定 |
| `go build ./...` / `go vet` | 编译 | ✅ | ✅ |
| 干净 HEAD 树 `./internal/tunnel/` | 区分历史失败 | 5 红 | 5 红（Windows 无 `/bin/sh`，**非本次引入**） |

---

## 四、我这轮改了什么（4 处，全部在 `internal/gateway/`）

| 文件 | 改动 | 对应条目 |
|---|---|---|
| `provider_failover.go` | `Write`：删掉对每个 attempt 都提交 header 的 `WriteHeader(200)`，可延后的 attempt 继续攒 `buf`；passthrough 分支返回真实 `n` 并只记录已写字节 | W16-1 / W16-3 |
| `provider_failover.go` | `commitHeaders`：按写入顺序先发 `buf`（开头）再发 `body`，`body` 重排为 `opening+rest` 便于回放 | W16-2 |
| `provider_failover.go` | `deliver`：`streamStart()` 越界时夹到 `len(streamBody)` | W16-4 |
| `passthrough_stream.go` | 收尾注释订正为实际语义（`deliver` 承担提交） | 文档一致性 |

未改：协议转换、调度、安全、Provider、`web/` —— 历史 60 项的其余部分本轮零进展（见第五节）。

---

## 五、完整排查清单（历史 60 项，本轮逐项复核）

复核方法：HEAD 未变，对方只动 5 个文件 → 其余逐字节未变。对每项做 grep/读码抽查，行号为本轮实测。

### 5.1 协议转换

| ID | 项目 | 状态 | 证据 |
|---|---|---|---|
| P1 | Claude→Chat 流式 usage 合并 | ✅ | 测试绿 |
| P2 | `stop_reason` 全量映射 | ✅ | `convert_claude_openai.go:427-438` |
| P3 | Chat→Responses 保留工具前正文 | ✅ | 测试绿 |
| P4 | Responses→Chat reasoning 增量 | ✅ | 测试绿 |
| P5 | `incomplete`/`max_output_tokens` | 🟡 | 被 `length` 覆盖 |
| P6 | Chat→Claude block index | ✅ | 测试绿 |
| P7 | 工具 ID 哈希后缀 | ✅ | `convert_tools.go` |
| P8 | 合成 `tool_result` | ✅ | — |
| P9 | `redacted_thinking` / 反向 reasoning | 🟡 | 反向仍丢全部 |
| P10 | `strict`/`top_k`/`metadata`/`prompt_cache_key` | 🟡 | Claude 构造器仍无 |
| P11 | 单一 IR 架构 | ❌ | 架构级，未动 |
| H-1 | `output_config.format` 带 `name` | ❌ | `convert_response_format.go:41` |
| H-2 | `json_object` 原样发 Claude | ❌ | `convert_response_format.go:19-20` |
| H-3 | adaptive 分支整体替换 `output_config` | ❌ | `convert_claude_openai.go:321-324` |
| H-4 | `allowed_tools` 塞进 `tool_choice` | ❌ | `convert_responses_claude.go:497-509` |
| H-5 | 缓存断点不含 `tool_result` | ❌ | `grep -c tool_result convert_claude_cache_marks.go` → 0 |
| H-6 | 思考余量补正生产不可达 | ❌ | `claude_pass_through_normalize.go:151` |
| H-7 | 非 5.2/5.3 `glm-*` 回落 `none` | ❌ | `claude_thinking_off.go` |
| H-8 | `knownClaude==false` 删 thinking | ❌ | 同上 |
| 假测试 | `TestClaudeToolChoiceWireShape` 恒真 | ❌ | `convert_tool_restrictions_test.go:127-149` |
| N11-1 | 长流 usage 用 256KB 头部副本 | 🔴 | `passthrough_stream.go:358` |
| N11-2 | 心跳 goroutine 不 join | ❌ | `sse_heartbeat.go:41-43` |
| N11-3 | 透传 Claude 收尾漏 `content_block_stop` | ✅ **本轮修** | `closeReplay` 已补 |
| N11-4 | 透传 Responses 收尾帧缺 `id/model/sequence_number` | 🟡 | 未动 |
| N12-1 | 567→65,536 输出 token 放大 | ❌ | `server.go:1330` |
| N13-1 | `--version` 会起服务 | ❌ | `cmd/gateway/main.go` 未动 |
| N13-2 | `/__usage-analysis` 无上限 | ❌ | 未动 |
| N14-1 | 成功流整段 `ReadAll` | 🟡 | 已加 `canDefer` 门槛，最后一跳直通；`writeBufferedStream` 本身仍读全 |
| N14-2 | 转换器中断判定读前快照恒 false | 🔴 | `convert_*_stream.go:87/126/253/576/815` |
| N14-3 | `runStreamConverter` 的 Content-Length 不与实读比较 | 🔴 | `protocol_proxy.go:587-590` |
| N14-4 | 非缓冲路径用头部 256KB 判收尾 | 🔴 | `passthrough_stream.go:199` |
| N14-6 | 中断计 Provider 软失败 | 🔴 | `provider_failover.go:1072-1084` |
| N14-7 | 中断收尾重复 `content_block_stop` | ✅ **本轮修** | `closeReplay` 只关「开着的」block |
| N14-8 | 缓冲路径丢上游响应头 | 🟡 | 只剩 Content-Type / Cache-Control，仍缺上游自定义头 |
| N14-9 | `attemptFailoverReason` 拿响应体当传输错误匹配 | 🟡 | `provider_failover.go:1164-1169` |
| N14-11 | 空流重试 × failover 两层叠乘 | 🟡 | `protocol_proxy.go:533-570` |
| W15-1 | prefix 门槛致 0 字节 | ✅ **本轮修** | 见结论 1 |
| W15-4 | 吞写错误 | 🔴 仍在 | `provider_failover.go:318-327` |
| W15-5 | 调试打印 | ✅ 业务码已清；test 仍 1 处 | `stream_interruption_retry_test.go:262` |
| W15-6 | gofmt | 🟡 `provider_failover.go` 已清；另有 4 文件存量红 | `gofmt -l` |
| W15-3 | 帧计数断言数 `event: xxx` 精确形态 | ✅ | 已改 |

### 5.2 路由 / 调度

| ID | 项目 | 状态 | 证据 |
|---|---|---|---|
| R1 | RPM 限流 | 🟡 | 仅 per-provider lane |
| R2 | 冷却跟随上游节奏 | 🔴 | `provider_failover.go:571-600` |
| R3 | billing 不算硬额度 | ✅ | `6a1256b` |
| R4 | 截断续传 | 🟡 | 透传收尾已能正确关门（本轮修复），但转换路径 N14-2 仍恒 false |
| R5 | session pin 落库 | ✅ | `30519bb` |
| R6 | per-account 闸门 + FIFO | 🟡 | `provider_lanes.go` 未动 |

### 5.3 横切安全

| ID | 项目 | 状态 | 证据 |
|---|---|---|---|
| S1 | 管理 API CORS 白名单 | ✅ | — |
| S2 | 请求体上限 | ✅ | 256MB |
| S3 | 认证限流 | 🟡 | `admin_auth.go` 未动 |
| S4 | 响应正文脱敏 | 🟡 | 只覆盖凭证 |
| S5 | 首包后空闲超时 | 🔴 | `grep -c "Timeout: 0"` = 11 |

### 5.4 Provider 账号

| ID | 项目 | 状态 | 证据 |
|---|---|---|---|
| A1 | 刷新令牌加锁 | ✅ | `16c00d3` |
| A2 | 用量探测 host 白名单 | ✅ | — |
| A3 | images 路由 / 全局 Key 回落 | 🟡 | `server.go:4207` |
| A4 | Claude 额度窗口 + 429 负缓存 | ❌ | `grep -rn seven_day_overage` → 0 |
| A5 | Cursor 过期提前量 | ✅ | 内建 5 分钟 |
| N-1 | `workbuddy_ai` 空 `expires_at` → 永不续期 | ❌ | `workbuddy.go:607-620` |

### 5.5 控制台

| 项目 | 状态 |
|---|---|
| 实时请求流 / 路由解释聚合 / 会话视图 / 一键重放 / 日志成本列 / 告警未读角标 | ❌ 6 项全未动（`web/` 零改动） |

---

## 六、验收门槛

已过（本轮）：`go build ./...`、`go vet ./internal/gateway/`、**commit `0f08b9b` 本身在同一 worktree 复跑 1266/0**、调试打印 0 命中。

⚠️ **我的三处修复已被作者以 `0f08b9b` 原样带上**（`Write` 去掉自动提交 + `n, err` 返回、`commitHeaders` 先 `buf` 后 `body`、`deliver` 越界夹取），我用 `git worktree /tmp/wt16` 对该提交独立复跑确认：1266 PASS / 0 FAIL。工作区目前由作者继续改动（`log_scrub.go` / `server.go` 等），我未提交任何东西。

合入前仍需：

1. **在 Linux 上跑一次 `gofmt -l`**（Windows 检出 CRLF 使本机结果不可信）。
2. **删死代码**：`streamClosed()` / `StreamLen()` 无调用者；`TestClaudeReplayClosesOnlyBlocksLeftOpen` 断言改确定形态。（W16-7/8，非阻塞）
3. **线上实测首字节**：`writeBufferedStream` 仍是整段 `io.ReadAll`，N14-1 只从 🔴 降到 🟡。用 `time curl -N` 对一条长流实测首字节耗时。
4. 历史硬门槛继续有效：转断判定读后求值、Content-Length 与实读比较、usage 用全文、收尾判尾部、中断不计软失败、首字节 < 2s。

---

## 七、复现

```bash
cd /c/guodevelop/llm-protocol-gateway
git status --porcelain
gofmt -l internal/ cmd/
go test ./internal/gateway/ -count=1 2>&1 | grep -E '^--- FAIL|\[FAIL\]'

# 单跑（应 PASS，全量红 = 串扰）
go test ./internal/gateway/ -run TestClaudeReplayClosesOnlyBlocksLeftOpen -count=1 -v
go test ./internal/gateway/ -run TestFailoverWriterCutTurnClosesAfterCommittedPrefix -count=1 -v

# 干净 HEAD 对照
git worktree add --detach .workbuddy/review-head16 34ec9a4
(cd .workbuddy/review-head16 && go test ./internal/gateway/ -run TestNoFailoverReplayAfterMidStreamInterruption -v && go test ./internal/tunnel/)
git worktree remove --force .workbuddy/review-head16

# 稳定复跑（连跑 4 次应全绿；-count=3 会触发既有全局状态串扰）
for i in 1 2 3 4; do go test ./internal/gateway/ -count=1 | tail -1; done
```

---

## 八、并发编辑披露

审查期间对方 agent 与我在同一份工作区上交接：我读取时 `Write` 里还留着「每次 Write 都提交」的旧逻辑，我自己改完后 `TestFailoverWriterCutTurnClosesAfterCommittedPrefix` 才从「缺开头帧」变为逐字节 PASS。本报告所有数字均来自我最后一次实测（`git diff` 指纹 `280f2a64…` 之后的最终态）。

**结论：本轮 WIP 的核心方向（截断流补齐收尾、不让最后一跳只发收尾不认已发字节）是对的，比 HEAD 更完整；实现里两条 P0（提交时机、帧顺序）已随 `0f08b9b` 一并修入。审查结束时作者又开了新改动（`log_scrub.go` / `log_scrub_test.go` / `server.go`，涉及 S4 日志脱敏），下轮（round17）需要专项复查。**

### 审查结束时的仓库状态

```
HEAD  3316851 test(tools): 用真实转换路径替换空转的 tool_choice 用例
      0f08b9b fix(sse): 被切断的流只补未送达部分并正常收尾   ← 含我的 3 处修复
34ec9a4 （round15 基准）
工作区 M internal/gateway/log_scrub.go / log_scrub_test.go / server.go  ← 作者新开，未审
```
