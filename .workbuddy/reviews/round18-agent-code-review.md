# 第 18 轮审查与修复报告

范围：续接第 17 轮遗留清单（N11-1 / N11-2 / N11-4 / N-1 / A3 / 控制台 6 项），并复核另一个 agent 并发改动的 `provider_lanes.go`。
基线：`b1e935c`。验证：`go build ./... && go vet ./...` 干净，`go test ./internal/gateway/` **1298 passed / 0 failed / 0 skipped**。

## 1. 本轮结论

| 项 | 结论 |
| --- | --- |
| N11-1 usage 被 256KB 截断 | **已修**：改为逐事件增量解析，长流尾部 usage 不再丢失 |
| N11-2 心跳 goroutine 未 join | **已修**：`stop()` 等待 goroutine 退出后才返回（`<-exited`） |
| N11-4 Responses 收尾帧缺 id/model/sequence_number | **已修**：从流内回读三者，序号严格 +1 |
| N-1 workbuddy 无 `expires_at` 永不续期 | **已修**：新增 `signInAt`，无过期时间时按签入时刻推算寿命 |
| A3 `/v1/images/*` 硬拼 `/images/generations` | **暂缓**（见 §5，需产品决策，不擅自改对外行为） |
| 控制台 6 项 | **未开始**（独立一批，下一轮） |
| 另一个 agent 的 `provider_lanes.go` | **有真 bug，已修**：固定窗口误改滑动窗口 + 准入前就 trim 导致窗口只剩 1 条 |

## 2. 逐项修复说明

### N11-1 流式 usage 增量解析（`usage.go` / `passthrough_stream.go`）

旧实现把整条 SSE 流缓存到 256KB 上限的 `limitedBuffer`，流结束后从这份**被截断的副本**里找 usage。后果：超过 256KB 的流，`message_delta` / 最后的 usage chunk 全在截断之外，usage 记 0 或只记到首包的值。

新实现 `streamUsageReader`：

- 作为 `io.MultiWriter` 的一环，**跟着字节流走**，看到多少算多少（`passthrough` 与 `buf` 两条路径都挂上了）；
- 按行缓冲，被 chunk 边界切断的 SSE 行留到下一块补齐，同一个流不管怎么切块结果一致；
- 每行先做两个子串探测（`"usage"` 是否存在），答案正文本身（占流 99%）不解码；
- 只有带 usage 的事件才进 `json.Decoder`，并 **skip 掉正文/增量字段**，只取 `type` 与 `usage`；
- 折叠语义：OpenAI / Responses 取"最后一个上报 usage 的完整事件"（OpenAI 终包、Responses `*.completed`）；Claude 保留原始字段到流结束再归一（逐事件求和会把 cache 重复计一遍）；
- 整包 JSON（错误体）不会被当成流事件：错误体里常引用一份 usage，逐事件折叠会把它记成已花 token。

回归测试（`usage_test.go`）：超过 256KB 后 usage 仍正确、chunk 边界无关、错误体不计费、OpenAI 尾部 usage chunk 不丢。

### N11-2 心跳 goroutine join（`sse_heartbeat.go`）

`stop()` 过去只 `close(done)` 就返回：调用方随即释放 provider lane 并结束响应，而心跳协程可能还握着写锁写 `: ping`。现在 `stop()` 发完信号后 `<-exited`，保证返回时已无心跳在写。幂等性保持不变。

回归测试（`sse_heartbeat_join_test.go`）：stop 返回时进行中的心跳数必须为 0；对不支持心跳的 writer，stop 不会阻塞。

### N11-4 Responses 收尾帧（`passthrough_stream.go`）

`response.incomplete` 收尾帧过去只有 `type` + 极简 `response`：

- 没有 `sequence_number`：SDK 以序号对事件去重，缺号/重号直接抛错；
- `response` 里没有 `id` / `model`：客户端无法把截断帧挂回原来的那个响应。

现在从已转发的那段流里回读最后一个事件（带容错地裁掉未写完的半帧），取 `id` / `model` / 最大 `sequence_number`，收尾帧用 `sequence_number+1`；流在首帧前就被切断时生成新的 `resp_<nano>` 并给 0 号。

回归测试：`TestResponsesClosingFrameRepeatsStreamIdentity`。

### N-1 WorkBuddy 无过期时间永不续期（`workbuddy.go` / `domain/types.go`）

`workBuddyTokenNeedsRefresh` 在 `expiresAt == ""` 时返回 `false`，注释写着"信任到对方拒绝为止"。问题是 WorkBuddy 的 access token 寿命只有小时级，而记录里的 `expiresAt` 实际上是 refresh token 的一个月寿命——两件事都没法触发提前续期，只剩 401 后强制刷新这一条路，每次都要白烧一个请求。

修法：`WorkBuddyCredential` 新增 `signInAt`，令牌签发/刷新时写入；无 `expiresAt` 时按 `signInAt + workBuddyAccessTokenLifetime(30m)` 判断，既不认识 `expiresAt` 也不认识 `signInAt` 时仍回退到"等 401"。

`signInAt` 不是密钥，出网对象里只有时间串。回归测试：`TestWorkBuddyTokenRefreshWithoutExpiry`。

401 → 刷新 → 重放单次：**已存在**（`sendWorkBuddyChat` 里 `forceRefreshWorkBuddyToken` + 一次重发），本轮只补了主动续期。

### 另一个 agent 的 `provider_lanes.go`（并发改动）

他本意是把 RPM 的"整分钟固定窗口"改成"最近一分钟滑动窗口"（方向对，固定窗口在两分钟交界会放行 2×rpm）。但实现有两个真 bug：

1. **`admitLocked` 在追加后立刻 `trimLocked(now)`**：`trimLocked` 的裁剪条件是 `<= cutoff`，刚追加的那条（等于 now）在 `now.UnixNano()` 精度下**当场被裁掉**。窗口里永远只剩 1 条，`rpm >= 2` 的限速完全失效。
2. 他自己的测试 `TestProviderLaneRateIsASlidingMinute` 的期望是错的。他按固定窗口推演：0s、50s 两条准入，65s 时"只剩第一条在窗口里"——但 50s 那条到 65s 只过了 15 秒，仍在滑动窗口内，所以 65s 时是 2/2 满，第三条必被拒；反过来，他"第一条滑出后又放行一条"的时点写成 61s，那时 50s 那条也在窗口里，同样过不了。

修复：

- `admitLocked` 不再 trim（trim 由调用方在判定前统一做）；
- `acquire` 对纯限速 lane（`max<=0 && wait<=0`）改为等到最老一条滑出窗口再重试（上限 `providerQueueWaitCap`），即"限速满 ≠ 服务不可用"；`acquireSlot` 的等待改成**按墙钟判定**的忙等（碰到 `wait=0` 立即判定），不再用 `time.Now()` 覆盖调用方传入的时刻；
- 测试改为用可推演的时序：5s/50s 两条准入 → 65s 时 50s 那条仍在窗口内、第三条必被拒；5s 那条在 65s 滑出 → 放行一条；
- 修了 `provider_rpm_test.go` 里被 GBK 乱码污染的 4 行中文注释（文件此前被别的工具以错误编码写入）。

## 3. 验证

```
go build ./...                                  # OK
go vet ./...                                    # OK
go test ./internal/gateway/ -count=1            # 1298 passed, 0 failed, 0 skipped
```

新增测试：`TestStreamUsageReader*`(4)、`TestResponsesClosingFrameRepeatsStreamIdentity`、`TestSSEHeartbeatStopJoinsTheGoroutine`、`TestSSEHeartbeatStopWithoutSupport`、`TestWorkBuddyTokenRefreshWithoutExpiry`；修正：`TestProviderLaneRateIsASlidingMinute`。

并发改动期间的编译失败（`workBuddySender` 签名、`workBuddyQuotaFor` 签名、`claudeOAuthRefreshRequiresRelogin` 未定义）已确认全部是另一个 agent 在同一包里的半成品，等其落盘后复验通过，不是本轮改动引入。

## 4. 历史问题复核（完整清单）

### 4.1 已修复并验证（累计）

| 编号 | 问题 | 状态 |
| --- | --- | --- |
| R16-P0-1 | `Write` 提前 commit 导致 `Discard()` 失效 | 已修 |
| R16-P0-2 | `commitHeaders` 帧序颠倒（body 先于 buf） | 已修 |
| R16-1 | 写错误被吞 | 已修 |
| R16-2 | 缺边界检查 | 已修 |
| S5 | 上游无空闲读超时（HTTP/2 无读 deadline） | 第 17 轮已修，15 处调用点全覆盖 |
| A4 | OAuth 用量探测失败无负缓存 / 指数退避 | 第 17 轮已修 |
| R2 | 冷却只有一把梯子，未区分限速与配额 | 第 17 轮已修（magpie 双梯子） |
| N11-1 | usage 被日志缓冲截断 | **本轮** |
| N11-2 | 心跳协程未 join | **本轮** |
| N11-4 | Responses 收尾帧缺 id/model/seq | **本轮** |
| N-1 | workbuddy 无过期时间永不续期 | **本轮**（主动续期部分） |
| L1 | 滑动分钟限速被实现成固定窗口 | **本轮**（对方改动引入的两个 bug 一并修） |

### 4.2 仍未处理（按优先级）

| 优先级 | 项 | 说明 |
| --- | --- | --- |
| 中 | A3 `/v1/images/*` 只支持 `POST /v1/images/generations` | 需要先定对外契约（见 §5） |
| 中 | 控制台 6 项 | 实时请求流、路由解释视图、会话视图、一键重放、日志成本列、告警未读角标 |
| 低 | 短 `Retry-After`（如 1s）是否加 5s 硬下限 | 第 17 轮遗留的产品口径问题，当前按厂商值 1s 走 |
| 低 | `providerQueueWaitCap`(60s) vs 限速窗口(60s) 的边缘 | 纯限速 lane 最坏要等"最老一条滑出"= 接近 60s，等上限 60s 再 +1ms，极端时序下会被上限截在窗口之外、多一次 429。建议把这一档单独放宽到 `providerQueueWaitCap + 1s` |

## 5. 需要拍板的两件事

1. **A3 images 路径**：现在硬拼 `<base>/images/generations`，只能代理"文生图"。要想支持 `/v1/images/edits`（图生图）等，需要定：是给 Provider 加可配置的上游路径模板，还是按请求子路径透传（`/v1/images/edits` → `<base>/images/edits`）。后者会让 `/v1/images/*` 变成一个通用的"透传到任意子路径"的口子，是安全上的扩大，不适合默认开。建议加 Provider 级 `imagePath` 可选字段，默认行为不变。
2. **短 `Retry-After`**：厂商回 `Retry-After: 1` 时，当前 1 秒后就会重试同一 Provider。要不要加 5 秒硬下限，换个稳（但慢）的体验。
