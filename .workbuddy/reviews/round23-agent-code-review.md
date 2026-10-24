# 第二十三轮：另一 agent 代码审查（round23）

- 审查时间：2026-10-23 15:12 → 15:40 (+0800)
- 基线 HEAD：`8b5542d`（`fix(lanes): RPM 改为滑动分钟窗口，纯限流 Provider 可排队`）
- 范围：工作区未提交改动 **47 改 + 6 新增**（+2424 / −401），指纹 `sha256(git diff HEAD)=f86d25eb…`
- **与 round22 完全一致**：文件集合、行数、指纹、最新 mtime（`provider_lanes.go` 13:35:13）逐项相同 ⇒ **本轮无新提交、无新改动**（静默 1h37m）
- 未修改任何业务代码；探针文件 `zz_probe23_test.go` 已删除，工作区回到审查前状态（53 条 git status，指纹复原为 `f86d25eb…`）

---

## 一、结论

**没有新代码可审，所以结论与上轮同向，但本轮补充了四类此前只有"代码阅读"证据的缺陷，全部用探针坐实。**

1. **治理问题连续第 5 轮**：生产跑的是 13:24:57 的未提交 WIP（`isMyTurnLocked` PRESENT / `strikeLocked` ABSENT），既无 commit 可回滚，退回 HEAD 又会丢掉一整批修复。
2. **真流式只发生在"最后一次尝试"**（N23-1）：只要身后还有备用链路，整段答案会被 `io.ReadAll` 缓冲完才写给客户端——TTFT 等于完整生成时间，且期间没有任何心跳帧。
3. **收尾判定有两套实现，结论会相反**（N23-2/N23-3）：`closesUpstreamStream` 与 `streamEndReader.closed()` 对同一段流一个判"完整"、一个判"截断"，谁生效取决于这次请求有没有备用链路；而且两套都没有正文回声守卫。
4. **`streamEndReader` 至今零测试**（N23-8）：V22-4/V22-5 这类分块缺陷因此只能靠探针发现，round22 的验收门槛 6 一条没落地。

基线：`go build ./...` 绿、`go vet ./...` 静默、`go test ./internal/gateway/` **37.97s ok**、lane/守卫/收尾专项 `-count=10` 5.92s ok、`web npx tsc -b` 0 error、`gofmt -l` 仅 `internal/store/{sqlite.go,session_pins_test.go}`（非本轮文件）、热路径无 `println` 残留。

---

## 二、本轮新发现

### N23-1（P1，体验/架构）有备用链路 ⇒ 客户端一个字节也收不到，直到整段答案生成完

`passthrough_stream.go:246-250`：

```go
if fw, ok := w.(*failoverResponseWriter); ok && stream && response.StatusCode < 400 {
    return writeBufferedStream(fw, response, parseUsage, end...)   // io.ReadAll 整段
}
```

`writeBufferedStream`（`:407`）是 `io.ReadAll(io.TeeReader(response.Body, …))`，读满才 `hw.Write(streamBody)`。而 `provider_failover.go:1166` 是 `canDefer := i < len(attempts)-1` ⇒ **只要这次请求不是最后一个尝试，它就是缓冲路径**。

后果（本机 44 个 Provider，多数路由有 2 条以上链路，即主路径）：

- TTFT = 上游完整生成时间（长答案 30~60s 静默）；
- 缓冲期 headers 未提交 ⇒ 心跳 goroutine 无事可做 ⇒ 网关侧 0 帧保活；
- 单请求内存 = 整段答案（无上限），并发一上来就是 GB 级峰值；
- round14 那条 P0 没有消失，只是从"无条件"收窄成"除最后一次尝试之外"。

修法（与 magpie `stream_cut_test.go` 的方向一致：保持真流式、把切断如实报给客户端）：把"可重试"限制在**首 N 字节**（如 64KB）或首 2 秒，超过就直接放流；或给一个 per-provider 开关，默认不做整段延迟。

### N23-2（P2）两套收尾判定并存，同一段流结论相反

| 实现 | 位置 | 判定方式 |
|---|---|---|
| `closesUpstreamStream` | `stream_interruption_close.go:31-46` | 取**最后 4096 字节**，`strings.Contains` 5 个标记 |
| `streamEndReader.closed()` | `stream_end_reader.go:62-92` | 扫**全部字节**，`[DONE]` 需落在尾部 64 字节内 |

缓冲路径用前者（`:408`），真流式路径用后者（`:293`）。探针：

```
PROBE-23D len=302 closesUpstreamStream=true streamEndReader=false
          （[DONE] 之后还有 300 字节注释行）
```

⇒ **同一段流，有备用链路时判"完整"，没有时判"截断"**：客户端要么少一帧收尾，要么平白多一帧 `finish_reason=length` + 第二个 `[DONE]`，并计 Provider 软失败。修法：删掉 `closesUpstreamStream`，缓冲路径也用 `streamEndReader`（它本来就是按全字节判定，正是缓冲路径想要的能力）。

### N23-3（P2）两套都没有正文回声守卫 ⇒ 真截断被判完整，客户端永远等不到收尾帧

`markersRejectingEcho` 只有 `[DONE]`（`stream_end_reader.go:44`），`message_stop` / `response.completed` 只要在流里出现就算收尾——包括出现在**答案正文里**。探针：

```
PROBE-23E len=383 closesUpstreamStream=true streamEndReader=true
          正文含 "the stream ends with message_stop"，随后流被切断
```

Claude 客户端等的是**事件** `type=message_stop`，不是字符串。判定为"已完整"⇒ 不补收尾帧 ⇒ 客户端一直挂到自己的读超时。现实命中率低（要模型自己聊到 SSE），但代价是整轮挂死，且修法明确：Claude 侧改成"解析到 `type==message_stop` 的事件"，而不是子串匹配。

### N23-4（P2，V22-4 原样）收尾标记跨块只携带 4 字节，完整流被误判截断

`stream_end_reader.go:19` `tail [4]byte`。探针（同一条完整 Claude 流，只改切分点）：

```
PROBE-23A split=71 prevTail="\"typ"  closed=true
PROBE-23A split=73 prevTail="ype\""  closed=true
PROBE-23A split=76 prevTail="\":\"m"  closed=true
PROBE-23A split=81 prevTail="ssag"   closed=false   ← 标记 "message_stop" 被切开，只剩 5 字节在上一块
```

后果：完整收尾的流被判截断 ⇒ 客户端多收一帧 `max_tokens` + 计 Provider 软失败（3 次进冷却）。修法：`tail` 至少 16 字节（最短标记 6 字节，留一倍余量）。

### N23-5（P2，V22-6 原样）`stream=true` 但上游回 JSON 时 usage 仍是 0

`streamUsageReader.event()` 里那段 `primed` 分支（"第一个 payload 是整个 JSON body"）**永远到不了**：`consider()` 先要求行首是 `d`/空格/制表符且以 `data:` 开头（`usage.go:571-583`），JSON body 行首是 `{`，直接 return。

```
PROBE-23B stream-reader={0 0 0 0}  whole-body={1234 567}
```

影响调用点不变（`qoder_chat.go:73`、`trae.go:1838`、`server.go:5395/5459`）；随后被 `recordRequestLogEx` 的估算兜底放大成 `len/4`。修法：`final()` 在折叠结果为空时回落 `parseUsage(body)`（一行）。

### N23-6（P3）首包超时开着和没开，走的是两套 transport

`upstream_timeout.go:222-225` `doUpstreamHTTP` 在 `timeout > 0` 时**直接** `client.Do(request)`；`timeout <= 0` 时才走 `guardUpstreamIdleClient` → `upstreamHTTPClient`（换上 `upstreamTransport`）。也就是说：配了 `FirstPacketTimeoutSec` 的 Provider 用回 `http.DefaultTransport`（无 `ResponseHeaderTimeout` 兜底、无 400 连接上限、无 `ForceAttemptHTTP2`），没配的才有。修法：`doUpstreamHTTP` 也走 `upstreamHTTPClient(client)`。

### N23-7（P3）全局价遇到 Provider 的 `*` 默认行会静默失效

`pricing.go:105-118` 的查找顺序是「provider 精确 → **provider `*`** → 全局精确 → 全局 `*`」。而界面（`pricing.tsx:222`）把未计价模型提示成「设全局价」：如果该 Provider 曾存过一条 `*` 兜底价，用户按提示设了全局精确价，**这条价对该 Provider 不生效**（被它的 `*` 压过），新请求继续按兜底价/未计价走，用户看不出原因。要么把顺序改成「provider 精确 → 全局精确 → provider `*` → 全局 `*`」，要么在 UI 上标出"该 Provider 存在默认行，全局价不会生效"。

### N23-8（P3）`streamEndReader` 零测试

`grep` 全仓 `*_test.go`：只有 `usage_test.go:213` 直接构造过 `&streamEndReader{}`（用于 usage 折叠），**没有任何**对 `closed()` / `cut()` 的分块矩阵断言。round22 门槛 6（512B/4KB/32KB/单次写 × 完整/截断）一条没做。本轮 N23-2/3/4 三条都是分块或回声边界问题，正好落在没有测试覆盖的区域。

### N23-9（P3）零散

- `gofmt -l internal/` 仍报 `internal/store/{sqlite.go,session_pins_test.go}`；仓库仍留 5 个 `.bak`（chatgpt_oauth / claude_oauth / cursor_oauth / qoder_pat / qoder_pat_handlers）。
- `upstream_timeout.go:93` `reading bool` 死字段（`:108`/`:121` 只写不读）。
- `idleGuardedBody.Read` 每次读都新建一个 `time.AfterFunc`（`:110`），热路径上千帧就是上千个 timer 分配；改成单个 `Timer.Reset` 更省。
- `scanClaude` 对每条 data 行做 `json.Unmarshal`（`stream_end_reader.go:122`），Claude 流每帧一次完整解码——热路径成本，可先用 `bytes.Contains(line, []byte("\"type\""))` 前置过滤。

---

## 三、本轮做对的（逐条复核）

| # | 改动 | 复核结论 |
|---|---|---|
| ✅ | 价格表全局 scope（`pricing.go:87-120`、`handleSavePrice:227`、`pricing.tsx` 范围选择器） | 查找链路实现与注释一致；`lookupPrice` 的 `scope == ""` 跳过处理正确；`invalidatePrices` 在保存/删除后都调了（`:243`/`:268`） |
| ✅ | `pruneModelPrices` 跳过 `PriceWildcard`（`model_prices.go:107-112`） | 真 bug 确认修好：否则一次 state 保存（令牌刷新即可触发）就删光共用价 |
| ✅ | WorkBuddy 签到 401 自愈（`workbuddy_checkin.go:66-86`、`checkin.go:291-306`） | `loginRefused` 只在"账号自己的令牌被拒"时置位，端点拒绝当日领取不触发刷新——分类正确；刷新后的凭证回传并回写 `refreshed.WorkBuddy`，`AccountKey` 重算一致 |
| ✅ | `workBuddyUnauthorized` 改用 `errors.As`（`:151-158`） | 比原来的 `workBuddyErrorCode` 更严：同时看 `status` 与 `code`，HTML 页面场景仍有 `HTTP 401` 兜底 |
| ✅ | `guardUpstreamIdleClient` 覆盖面扩大（13 处：alerts/claude_oauth×3/claude_usage/conformance/server×7） | 这些点位确实都读过 body，包裹位置正确（都在 `Do` 之后立刻包） |
| ✅ | 新增 5 个测试文件 | 非空转：`TestUpstreamIdleGuardFailsASilentStream`、`TestRateLimitRestIgnoresAFarOffReset`、`TestSSEHeartbeatStopJoinsTheGoroutine` 等都能在实现回退时变红 |
| ✅ | 热路径无 `println` 调试残留 | 与 round22 一致，前几轮的老毛病没有复发 |

---

## 四、上轮遗留复核（V22-x）

| 编号 | round22 判定 | 本轮现状（逐条带行号复验） |
|---|---|---|
| V22-1 生产=未提交 WIP（死锁版） | P0 | ❌ **原样，且是连续第 5 轮**。`/opt/.../gateway` mtime `2026-10-22 22:24:57 -0700`（=13:24:57 +0800）；`isMyTurnLocked` PRESENT / `strikeLocked` ABSENT / `streamEndReader` PRESENT / `idleGuardedBody` PRESENT；service active |
| V22-2 新请求可插队 | P2 | ❌ 原样：`provider_lanes.go:195` `if !queued \|\| lane.turnLocked(ticket)`；`provider_lane_fifo_test.go` 四个用例仍只验证"已排队者之间"的顺序 |
| V22-3 idle 守卫 nil timer | P2 | ❌ 原样，但**崩溃点在 `Read` 不是 `Close`**（本轮更正上轮表述）：`Close`（`:136-143`）已有 `if b.timer != nil`；`Read`（`:120-124`）出锁后再进锁 `b.timer.Stop()` 无守卫 ⇒ Close 插在两次加锁之间即 nil 解引用 |
| V22-4 收尾标记 4 字节携带 | P2 | ❌ 原样，探针复现（PROBE-23A split=81 closed=false） |
| V22-5 Responses 收尾 id/sequence 算错 | P2 | ❌ 原样：`stream_end_reader.go:99-101` 一旦 `id`/`model` 齐了就 `return`，`tailBytes` 冻结 ⇒ `cut()` 只能看到开头那点事件 |
| V22-6 `stream=true`+JSON → usage 0 | P2 | ❌ 原样，探针复现（PROBE-23B）；`primed` 分支被 `consider()` 的门槛挡死 |
| V22-7 收尾帧写裸 `w` | P2 | ❌ 原样：`passthrough_stream.go:297` 仍是 `closeInterruptedStream(w, …)`，而心跳写在 `hw` 上 |
| V22-8 中断判定恒 false（5 处） | P2 | ❌ 原样：`convert_claude_openai_stream.go:87`、`convert_claude_responses_stream.go:253`、`convert_openai_claude_stream.go:126`、`convert_openai_responses_chat_stream.go:576`/`:815` 全部"读前按值快照" |
| V22-9 图片被 120s idle 砍 | P2 | ❌ 原样：`server.go:5552-5554` `Timeout: 0` + `guardUpstreamIdleClient`（Cursor 分支 `:5527` 仍是 10min） |
| V22-10 401+hardAuth 15min | P2 | ❌ 原样：`provider_failover.go:159` 归 `failoverHard`，`:658` 硬下限 `providerQuotaRestFloor=15min` |
| V22-11 刷新失败换掉 401 | P3 | ❌ 原样（`workbuddy.go` 未改） |
| V22-12 心跳 stop 可阻塞 | P3 | ❌ 原样：`sse_heartbeat.go:44` `<-exited` 无写超时 |
| V22-13 零散（回声守卫/哨兵/热路径/queueCap/.bak/价格顺序） | P3 | 🟡 `[DONE]` 之外仍无回声守卫（本轮升级为 N23-3）；`resetConsumed` 仍用 `ttl == providerSoftUnavailableTTL` 当哨兵（`:716`）；`queueCap = rpm`；5 个 `.bak`；价格顺序见 N23-7 |

---

## 五、历史总账（截至 round23）

图例：✅ 已修并复核 ｜ 🟡 部分 ｜ ❌ 原样 ｜ ⛔ 回调/新伤

### 协议转换

| # | 问题 | 状态 |
|---|---|---|
| P1 | Claude→Chat 流式 usage 丢 0 | ✅ |
| P2 | stop_reason 不全量映射 | ✅ |
| P3 | Chat→Responses 有 tool_calls 时正文清空 | ✅ |
| P4 | Responses→Chat 丢 reasoning 增量 | ✅ |
| P5 | Responses incomplete 当正常结束 | 🟡 透传侧修，转换侧受 V22-8 影响 |
| P6 | Chat→Claude block index 错乱 | ✅ |
| P7 | tool_use ID 归一化碰撞 | ✅ |
| P8 | Responses→Claude 缺合成 tool_result | ✅ |
| P9 | redacted_thinking / reasoning 丢失 | 🟡 占位仍在 |
| P10 | strict/top_k/stop_sequences/metadata 不转发 | ✅ 大部分；stop 进 Responses 已堵 |
| P11 | 两两手写转换 vs magpie 单一 IR | ❌ 架构性，不做 |
| P12 | 中断收尾帧 identity/sequence 不可靠 | ⛔ V22-5（N23 未修） |
| P13 | 完整流被误判截断（4B 携带） | ⛔ V22-4（PROBE-23A 复现） |
| P14 | `stream=true`+JSON body → usage 0 | ⛔ V22-6（PROBE-23B 复现） |
| **P15（新）** | **两套收尾判定结论相反** | ⛔ N23-2（PROBE-23D） |
| **P16（新）** | **收尾标记无回声守卫，真截断被判完整** | ⛔ N23-3（PROBE-23E） |

### 路由 / 调度

| # | 问题 | 状态 |
|---|---|---|
| R1 | 无 RPM 限流 | ✅ `8b5542d` 滑动窗口 |
| R2 | 冷却不看上游 | ✅ 阶梯 + Retry-After/resets_at；401+hardAuth 15min 偏长 🟡 |
| R3 | billing 归进硬额度 | ✅ |
| R4 | 流截断无法续流 | 🟡 透传侧生效；转换侧因 V22-8 无效；且续流代价见 N23-1 |
| R5 | 会话粘性只在内存 | ✅ 落库 |
| R6 | 并发闸门非 FIFO / 非每账号 | 🟡 已排队者 FIFO；新请求可插队（V22-2）；仍非每账号 |

### 安全（S1–S5）

| # | 问题 | 状态 |
|---|---|---|
| S1 | 管理 API `ACAO: *` | ✅ `cors.go` 白名单 |
| S2 | 请求体无上限 / 无 ReadTimeout | ✅ `request_body_limit.go` |
| S3 | 登录/注册/自检无限流、setup 首个调用者可设密码 | 🟡 有限流 + 实例在用守卫，仍靠 Host 判回环 |
| S4 | 正文不脱敏入库 | ✅ 请求+响应双向 |
| S5 | 上游头返回后无空闲超时 | 🟡 覆盖面够；120s 硬编码 + 图片被砍（V22-9）+ transport 分叉（N23-6） |

### Provider 账号

| # | 问题 | 状态 |
|---|---|---|
| A1 | Trae/WorkBuddy/Cursor 刷新无锁 | ✅ 统一 renewLock |
| A2 | 子串匹配 host 泄露 Key | ✅ 白名单 |
| A3 | `/v1/images/*` 回落全局 Key | ✅ |
| A4 | Claude 漏 overage / 429 无负缓存 | ✅ 两项齐（5/5 路） |
| A5 | Cursor 无过期提前量 | ✅（round10 已纠错，勿再报） |
| A6 | workbuddy_ai 永不过期 | ✅ 401 自愈 + SignInAt |

### 控制台 6 项

实时请求流 ❌ ｜ 路由聚合视图 ❌ ｜ 会话视图 ❌ ｜ 一键重放 ❌ ｜ 日志成本列 🟡（仅详情弹窗）｜ 告警未读角标 ❌

本轮 `web/` 只动了价格面板（全局 scope）与 Claude manual 授权，**6 项零进展**。

### 调度/流式专项（W21/W22 系列）

| 编号 | 状态 |
|---|---|
| W21-1 lane 自死锁 | ✅ 重写修好（专项 `-count=10` 绿）；线上仍是旧版（V22-1） |
| W21-2 幽灵占满 queueCap | ✅ |
| W21-3 idle 守卫 nil timer | ❌ V22-3 |
| W21-4 Responses 收尾 sequence | ❌ V22-5 |
| W21-5 转换路径中断判定 | ❌ V22-8 |
| W21-6 收尾帧写裸 w | ❌ V22-7 |
| W21-7 120s 砍长图 | 🟡 仅 Cursor 分支 |
| W21-8 零散 | 🟡 见 V22-13 |
| **N23-1 整段缓冲（round14 P0 的现行版）** | ❌ 只要身后有备用链路就不流式 |
| **N23-8 streamEndReader 零测试** | ❌ |

---

## 六、验收门槛（下一轮逐条打勾，不勾不许部署）

1. `git diff --quiet HEAD` 为真，或部署脚本硬拒未提交状态（**已连续 5 轮未落地**）。
2. 部署后二进制带上 commit：`-ldflags -X main.commit=$(git rev-parse --short HEAD)`，`gateway --version` 能对账。
3. `go test ./internal/gateway/ -timeout 300s` 在 ~40s 内 `ok`（当前 37.97s，达标）。
4. **删掉 `closesUpstreamStream`**，缓冲路径改用 `streamEndReader`；补一条"同一段流两条路径结论必须相同"的用例（N23-2）。
5. `streamEndReader` 补分块矩阵：512B / 4KB / 32KB / 单次写 × 完整 / 截断 / 标记跨块，断言 `closed()`、`cut().id/model/lastSequence`（N23-4/N23-5/N23-8）。
6. 正文含 `message_stop` / `response.completed` 的截断流必须判"截断"（N23-3）。
7. `stream=true` + JSON 回包的 usage 断言 ≥ 报文字段（N23-5）。
8. `doUpstreamHTTP` 两条分支都断言 `Transport == upstreamTransport`（N23-6）。
9. `idleGuardedBody` 并发 Close/Read 用 `-race` 覆盖（需先打开 `CGO_ENABLED`）。
10. 4 条转换路径各写一个"上游半路截断"用例，断言客户端收到 `finish_reason=length` / `response.incomplete`（V22-8）。
11. 中断收尾帧改走 `hw`，SSE 用例跑 `-race`（V22-7）。
12. 图片路径用 180s 静默的上游桩测一次，不得被封（V22-9）。
13. **给出真流式的决策**：要么把"可重试窗口"限制在首 64KB（超过即放流），要么明确写下"多链路场景放弃流式"的取舍并在 UI/文档标注（N23-1）。
14. 拆提交：协议 / 调度 / 安全 / provider / 价格 UI 分开，每个提交自带用例。

---

## 七、复现命令与探针输出

```bash
# 基线
go build ./... && go vet ./...                    # 绿
go test ./internal/gateway/ -count=1              # ok 37.971s
go test ./internal/gateway/ -run 'TestProviderLane|TestRateOnlyLane|TestUpstreamIdle|TestStreamEndReader' -count=10   # ok 5.922s
(cd web && npx tsc -b)                            # 0 error
gofmt -l internal/                                # internal/store/{sqlite.go,session_pins_test.go}
ls internal/gateway/*.bak | wc -l                 # 5

# 零漂移证据
git rev-parse --short HEAD                        # 8b5542d
git diff HEAD | sha256sum                         # f86d25eb…（与 round22 相同）
ls -l --time-style=full-iso internal/gateway/provider_lanes.go   # 13:35:13

# 线上版本（只读）
ssh root@173.249.207.202 'ls -l --time-style=full-iso /opt/llm-protocol-gateway/bin/gateway; systemctl is-active llm-protocol-gateway; \
  for s in isMyTurnLocked strikeLocked streamEndReader idleGuardedBody; do printf "%s => " "$s"; \
  grep -qaF "$s" /opt/llm-protocol-gateway/bin/gateway && echo PRESENT || echo ABSENT; done'
# -rwxr-xr-x 16408738 2026-10-22 22:24:57 -0700   active
# isMyTurnLocked => PRESENT / strikeLocked => ABSENT / streamEndReader => PRESENT / idleGuardedBody => PRESENT
```

探针（临时 `internal/gateway/zz_probe23_test.go`，跑完即删）：

```
PROBE-23A split=71 prevTail="\"typ"  closed=true
PROBE-23A split=73 prevTail="ype\""  closed=true
PROBE-23A split=76 prevTail="\":\"m"  closed=true
PROBE-23A split=81 prevTail="ssag"   closed=false      ← 完整流被判截断
PROBE-23B stream-reader={0 0 0 0}  whole-body={1234 567}
PROBE-23C endsWithMarker=true streamEndReader=true      ← 正文回声即判完整
PROBE-23D len=302 closesUpstreamStream=true streamEndReader=false   ← 两套判定相反
PROBE-23E len=383 closesUpstreamStream=true streamEndReader=true    ← 截断被判完整
```
