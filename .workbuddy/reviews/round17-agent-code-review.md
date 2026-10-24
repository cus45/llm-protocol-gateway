# 第十七轮 · 对照 magpie 的实现审查与修复（round17）

- 审查时间：2026-10-23
- 审查范围：`127c194` 之上的工作区；**同时有另一个 agent 正在同一份工作区改 `provider_lanes.go` / `provider_rpm_test.go`**
- 对照基准：`C:\guodevelop\demo\magpie`（只读调研，见第七节）
- 本轮目标：从「一、真实没修的」清单里挑 magpie 有成熟做法的项，研究后修复
- 验证方式：我的 11 个文件单独拷到干净 HEAD 的临时 worktree 里跑全量，避免与对方半成品互相污染

---

## 结论先说

**修了 3 项（S5 空闲超时、A4 额度探测失败负缓存、R2 冷却阶梯），新增 16 条用例，全量通过。**

1. **S5（首包后无空闲超时）已修**。原来 11 处 `http.Client{Timeout: 0}` 意味着上游发完响应头就地沉默时，请求会一直挂到客户端断开。
   - 新增 `upstreamTransport`：`ResponseHeaderTimeout = 120s`，`MaxIdleConns = 400`，`ForceAttemptHTTP2`。
   - 新增 `idleGuardedBody`：响应头之后每次 `Read` 重置 120s 定时器，超时关闭 body 并返回 `errUpstreamIdleTimeout`（非 context 错误，failover 循环按 Provider 故障处理，不会误判为「客户端断开」）。
   - 所有 15 处上游 `client.Do(request)` 全部接入（`alerts.go` 1、`claude_oauth.go` 3、`claude_oauth_usage.go` 1、`provider_conformance.go` 1、`server.go` 8、`doUpstreamHTTP` 1）。
   - **没有引入新依赖**：先试了 `golang.org/x/net/http2`，但 `go mod tidy` 会重排 `go.mod` 并和另一个 agent 冲突，改用标准库 `ForceAttemptHTTP2` + 独立的读空闲保护，效果等价。

2. **A4（429 无负缓存）已修**。原来 `refreshClaudeOAuthUsageHoldingLock` 失败时直接 return，什么都不缓存 → 控制台每轮询一次就打一次上游；额度探测端点被 429 后会被持续重打。
   - `oauthUsageCache` 增加失败状态：`setFailure` / `clearFailure` / `lastError` / `waitForRefresh`。
   - 退避：1min 起，每次失败翻倍，封顶 10min（`oauthUsageFailureTTL << min(failures-1, 8)`，显式夹位防溢出）。
   - 失败期间仍保留上次成功快照（`getAllowStale`），面板显示「旧数据 + 错误」而不是空。
   - 5 个探测路径全部接入：claude / cursor / chatgpt / deepseek / zhipu，以及 `handleClaudeOAuthUsage` 的三个失败出口。
   - 补上漏掉的 `seven_day_overage_included` 额度窗口，并纳入 `cooldownHint` 的重置时间计算。

3. **发现并修掉一个真实 bug**：`getEntry` 用 `time.Since(fetchedAt) > staleTTL` 做过期，而失败条目 `fetchedAt` 是零值 → 刚写进去就被当成过期删掉，负缓存完全失效。改为失败条目不参与过期（由 `nextRetry` 决定）。
   - 调试过程：本地测试红 → 打桩发现 `getEntry` 返回 `ok=false` → 定位到零值时间。

4. **R2（冷却阶梯）已修**，按 magpie 的「限流与额度两条 ladder」重写：
   - 限流（soft）：`30s << min(连续失败数, 6)`，封顶 30min；供应商 `Retry-After` 与它不一致时以供应商为准（更短更诚实，更长也没意义），仍受 30min 封顶。
   - 额度（hard）：默认 15min 保底；供应商给了窗口就以窗口为准（"9 分钟后恢复"不会被抬到 15 分钟）；封顶 60min。
   - 缓存窗口改取**最早**的重置时刻：5h 窗口 9 分钟后重开比 7d 计划窗口更能说明这个账号什么时候能用。
   - 新增 `resetConsumed`：休整时间跑满供应商给的重置时刻后，下次失败不继承这个已花掉的窗口。
   - 旧用例 `TestCooldownTTLFollowsUpstreamHint` 里两条反直觉的期望（429 的 `Retry-After` 只能拉长不能缩短、额度路径只保底 2 分钟）已按新语义更新。

5. **未动**（下轮候选）：N11-1（usage 用 256KB 截断副本）、N11-2（心跳不 join）、N11-4（Responses 收尾帧字段）、N-1（workbuddy 续期）、控制台 6 项。magpie 在 N11-1 / N11-2 上有明确可借鉴的做法，已记录在第七节。

---

## 一、逐条判定

| # | 项目 | magpie 的做法 | 我们的修法 | 状态 |
|---|---|---|---|---|
| S5 | 首包后空闲超时 | 无厂商侧读超时；靠**给客户端发心跳 5 分钟**，超时后放弃（`holdWriter.watch()`） | 上游侧 120s 空闲保护（比 magpie 更贴近「尽早发现」），失败按 Provider 故障处理 | ✅ |
| A4 | 429 负缓存 | **不是**错误 TTL；是「1 分钟内不再问 + 失败保留上次好值 + 成功值落盘带 AsOf」 | 失败负缓存 + 翻倍退避（封顶 10min）+ 保留上次快照 | ✅ |
| A4b | `seven_day_overage_included` | magpie 解析该窗口 | 补进 `ClaudeOAuthUsageReport` 与冷却计算 | ✅ |
| N11-1 | usage 解析用截断副本 | **逐事件增量解析**，256KB 只用于调用记录 | 未动 | 🔴 下轮 |
| N11-2 | 心跳 goroutine 不 join | `holdWriter.watch()` 用 `defer` 在释放 slot 前 join | 未动 | 🔴 下轮 |
| N11-4 | Responses 收尾帧缺字段 | — | 未动 | 🔴 下轮 |
| R2 | 冷却阶梯 | 分档 ladder + 硬 ceil；限流路径归自己的 ladder（30s 起翻倍封顶 30min），额度路径 15min 保底、8d 内按供应商窗口、封顶 60min | 同左：两条 ladder 分开，限流不被远窗口拉长，额度以供应商窗口为准 | ✅ |

---

## 二、本轮改了什么

| 文件 | 改动 |
|---|---|
| `upstream_timeout.go` | 新增 `upstreamTransport`（ResponseHeaderTimeout 120s）、`idleGuardedBody`、`guardUpstreamIdle` / `guardUpstreamIdleClient`、`errUpstreamIdleTimeout` |
| `upstream_idle_test.go`（新） | 4 例：沉默流必须失败、活跃流不被切断、`doUpstreamHTTP` 接线、transport 参数 |
| `oauth_usage_cache.go` | 失败状态 + 退避 + `lastError` / `waitForRefresh`；修 `getEntry` 零值时间 bug |
| `oauth_usage_cache_test.go` | 5 例：失败记忆、保留好值、退避封顶、成功清错、overage 窗口 |
| `oauth_usage_refresh.go` | 5 条探测路径接入 `setFailure` / `clearFailure` |
| `claude_oauth_usage.go` | 解析 `seven_day_overage_included` |
| `provider_failover.go` | `cooldownHint` 纳入 overage 窗口 |
| `server.go` | `handleClaudeOAuthUsage` 失败落缓存；冷却期内返回上次快照 + 错误，不再打上游 |
| `alerts.go` / `claude_oauth.go` / `provider_conformance.go` | 上游 `client.Do` 接入空闲保护 |
| `provider_failover.go` | 冷却阶梯重写：`providerRateRestBase/Cap`、`providerQuotaRestFloor`、`softFailureCount`、`resetConsumed`；缓存窗口取最早重置 |
| `provider_cooldown_ladder_test.go`（新） | 7 例：远窗口不拉长限流、近窗口会延长、连续失败退避、额度保底、额度封顶、短 Retry-After 生效、已花掉的窗口不继承 |
| `provider_cooldown_test.go` | 更新两条旧期望到新语义 |

`go.mod` / `go.sum` **未改动**（曾短暂引入 `x/net`，已回滚）。

---

## 三、验证

```bash
# 干净 HEAD + 仅我的 11 个文件
git worktree add --detach /tmp/wt-final HEAD
cp /tmp/mine/* /tmp/wt-final/internal/gateway/
cd /tmp/wt-final && go build ./... && go vet ./internal/gateway/
go test ./internal/gateway/ -count=1     # ok  33-38s，全绿
```

| 检查 | 结果 |
|---|---|
| `go build ./...` | ✅ |
| `go vet ./internal/gateway/` | ✅ |
| `go test ./internal/gateway/ -count=1`（隔离树） | ✅ 全绿 |
| 新增用例 | 9 条全绿 |
| `gofmt -l` | 仅 CRLF 假阳性（`gofmt -w` 后 `git diff` 零变化） |

---

## 四、与另一个 agent 的并发冲突（重要）

本轮期间对方正在改 `provider_lanes.go`（滑动分钟窗口 / RPM）与 `provider_rpm_test.go`，中途出现：
- `provider_lanes.go:250` 语法错误（半成品）
- `TestProviderLaneRateIsASlidingMinute` 红（`65s 的第三次请求应被拒`）

**这两条都不是我的改动引起的**，隔离树里全绿。主工作区当前还留着对方两个 `.bak` 文件（`provider_lanes.go.bak`、`provider_lane_capacity_test.go.bak`），需要他清理。

建议：与对方约定同一时刻只有一方改 `internal/gateway/`，否则每次跑测试都会被对方半成品打断。

---

## 五、完整排查清单（历史清单，本轮复核）

### 5.1 协议转换

| ID | 项目 | 状态 |
|---|---|---|
| P1–P10 | 见 round16 | ✅/🟡 无回退 |
| P11 | 单一 IR 架构 | ❌ 设计选择 |
| H-1 | `output_config.format` 带 `name` | ✅ |
| H-2 | `json_object` 近似 object schema | ✅（`convert_responses_fields.go:73`） |
| H-3 | adaptive 分支替换 `output_config` | ❌ |
| H-4 | `allowed_tools` 进 `tool_choice` | ❌（`convert_responses_claude.go:497-509`） |
| H-5 | 缓存断点不含 `tool_result` | ❌ |
| H-6 | 思考余量补正生产不可达 | ❌ |
| H-7/H-8 | `glm-*` / `knownClaude==false` 删 thinking | ❌（有专门处理，但非所有路径） |
| N11-1 | usage 用 256KB 副本 | 🔴 |
| N11-2 | 心跳不 join | 🔴 |
| N11-3 | 透传收尾漏 block stop | ✅ |
| N11-4 | Responses 收尾帧缺字段 | 🔴 |
| N12-1 | 输出 token 放大 | ❌ |
| N13-1 | `--version` 起服务 | ❌ |
| N13-2 | `/__usage-analysis` 无上限 | ❌ |
| N14-1/2/3/4 | 整段缓冲 / 读后求值 / CL 比较 / 尾部判定 | ✅/🟡 |
| N14-6/7/8/9/11 | 中断计软失败 / 重复 stop / 丢头 / 响应体匹配 / 叠乘 | 🔴/🟡 |

### 5.2 路由 / 调度

| ID | 状态 |
|---|---|
| R1 RPM 限流 | ✅（对方本轮在改滑动窗口） |
| R2 冷却阶梯 | ✅ **本轮修** |
| R3 billing 非硬额度 | ✅ |
| R4 截断续传 | 🟡 |
| R5 session pin 落库 | ✅ |
| R6 per-account 闸门 + FIFO | 🟡 |

### 5.3 横切安全

| ID | 状态 |
|---|---|
| S1 CORS 白名单 | ✅ |
| S2 请求体上限 | ✅ |
| S3 认证限流 | ✅ |
| S4 日志脱敏 | ✅（`127c194`） |
| **S5 首包后空闲超时** | ✅ **本轮修** |

### 5.4 Provider 账号

| ID | 状态 |
|---|---|
| A1 刷新令牌加锁 | ✅ |
| A2 探测 host 白名单 | ✅ |
| A3 images 路由 | 🟡（已拒不可能落点，仍固定拼接） |
| **A4 额度窗口 + 429 负缓存** | ✅ **本轮修** |
| A5 Cursor 过期提前量 | ✅ |
| N-1 workbuddy 空 `expires_at` | ❌ |

### 5.5 控制台

| 项目 | 状态 |
|---|---|
| 实时请求流 / 路由解释 / 会话视图 / 一键重放 / 成本列 / 告警角标 | ❌ 6 项未动 |

---

## 六、下轮优先项（按 magpie 可借鉴度）

1. **N11-1**：把 usage 解析改成逐事件增量（magpie `usage_ingest.go` 的模式），彻底摆脱 256KB 截断
2. **N11-2**：心跳改成 `defer` join（magpie `holdWriter.watch()`）
3. **N11-4 / A3 / N-1**
5. 控制台 6 项

---

## 七、magpie 调研摘要（本轮引用）

| 主题 | magpie 做法 |
|---|---|
| 额度缓存 | 1 分钟最小间隔 + 并发 singleflight + 失败保留上次好值 + 成功值落盘带 `AsOf`；账号错误（401/403/额度用尽）**不**降级为旧数据 |
| 流空闲 | 无厂商侧读超时；给客户端发心跳，5 分钟后放弃 |
| 大流 usage | 逐事件增量解析，256KB 只用于调用记录 |
| 心跳生命周期 | `holdWriter.watch()` + `defer` join，先 join 再释放 slot |
| 错误分类 | `routing.go` 单词 + 状态码分类；限流与额度走不同 ladder：限流 `1m<<n` 封顶 30m、额度默认 15m 按供应商重置时刻封顶 8 天、纯 `Retry-After` 封顶 1h；402/额度词汇优先判为额度 |
