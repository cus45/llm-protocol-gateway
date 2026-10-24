# 第三十轮审查（round30）— 另一 agent 改动核查

- 时间：2026-10-24 01:44–02:30 (+0800)
- 审查对象：`d93ec02`（无新提交）+ 工作区 126 文件（+20199 / −900）
- 对照基准：`C:\guodevelop\demo\magpie`
- 未修改任何业务代码；探针源码已复制进证据目录并删除；未创建 worktree；生产仅只读

---

## 0 审查对象与增量判定

| 项 | 值 |
|---|---|
| HEAD | `d93ec02`（=round29，**无新提交**） |
| git status --porcelain | **126** 条：M 83 / A 39 / D 1 / ?? 1（另有 3 条 AM） |
| git diff HEAD --stat | **126 files changed, 20199 insertions(+), 900 deletions(-)** |
| 指纹（未暂存） | `sha256(git diff)` = `25feda628bf45af03e8993a6e54fe29ba43033f40893c03128d1c0f3d7606fd3` |
| 指纹（已暂存） | `sha256(git diff --cached)` = `5e268b3ff014c6582f6824dc0232d09fa7eb727cf17006189dfd1621a47b9cb9` |
| 与上轮比对 | round29 末次快照 `bf1ce3d3…`，本轮 `25feda62…`+`5e268b3f…` ⇒ **仅 WIP 变化**（且规模翻倍：63 文件 → 126 文件，+3209 → +20199） |

增量性质：**不是零改动**。上轮之后新增了整套「监控中心」落地（`internal/monitoring` 9 个源文件 + `internal/store/monitoring*.go` 4 个 + `internal/gateway/monitoring_*.go` 5 个 + 17 条路由 + 前端），并回填了 D29-1（laneGroup 落库）、W29-4（红测试）、W29-5（接线）。

---

## 一 结论先说

**不能上线：透传流的「假流式」仍然是 P0 且仍在生产上（连续第 10 轮），本轮新增的两万行监控中心则把管理面攻击面扩大了一倍。**

1. **P0 · W29-1 未修**：`writeBufferedStream` 从不在 `io.Copy` 前提交响应头，`failoverResponseWriter.Write` 在 `!passthrough` 时只写内存 ⇒ 所有 API Key 的透传流仍是「整段生成完一次性吐出」。探针 PROBE-30STREAM：`canDefer=true` 首字节 **400.56ms = 末字节 400.56ms，writes=1，flushes=0**；`canDefer=false` 同样。生产 A/B（E4）：12 小时内 1063 条 `pass_through` 的 `avg(latency−ttft)=0.7ms`、`max=19ms`，而 `convert` 路径是 1159.5ms ⇒ 线上透传路径**没有任何流式**。
2. **P1 · W29-3 同因**：心跳 `: ping` 也走同一个 `fw.Write` 进缓冲，`flushes=0` ⇒ 长静默期客户端收不到心跳，Cloudflare ~100s 空闲剪断风险回归（S5 名义修好、实际失效）。
3. **P1 · D29-3 / D29-4 未修**：组内限额仍取「最后到达那行」的配置（`laneFor` 每次 acquire 覆盖 `lane.max/rpm`），且组内任一行填 0/0 就整组绕过 lane（`acquireProviderSlot` 提前 return 空 release）——恰好是「只在一行填限额」这个真实运营场景。
4. **D29-1 ✅ 真修好**：`lane_group` 有列（sqlite.go:239）、有迁移、INSERT/SELECT 双写双读（:641/:766/:787），`TestProviderLaneGroupRoundTrip` 与 `TestMigrateAddsLaneGroupToOldDatabase` 本机绿。
5. **W29-5 ✅ 接线完成**：`MigrateMonitoringV1()` 在 `app/runtime.go:194`、`StartMonitoring` 在 `:353`、17 条 `/__monitor/*` 路由在 `server.go:358-375`，且全部走 `monitorGate`→`requireAdmin`。
6. **R4 ✅ 本轮确认闭环**：6 处 `cleanEOFInterruption`（convert_*_stream.go:216/430/297/677/1036/796），`TestConvertersTreatCleanEOFWithoutClosingFrameAsInterruption` 覆盖 4 条腿全绿。
7. **P1 治理（连续第 9 轮）**：生产二进制 `vcs.revision=d93ec02` + **`vcs.modified=true`**，mtime 21:32:24 +0800 与 round29 完全相同 ⇒ 线上是「提交 + 一批未知 WIP」的混合体，无 commit 可复现；且该二进制**不含** `lane_group`（grep -a 计数 0）却**已建** `monitor_*` 六张表 ⇒ 线上既缺本轮修复、又带了未审计的新代码。
8. **W30-1 / W30-3（新）**：`zz_quota_keeper_e2e_hooks.go` 是非 `_test` 的生产源码（`*Server` 上的 `RunQuotaKeeperTickForTest`/`HandleRestoreProviderForTest`，还 import `net/http/httptest`），会随二进制发布；新增的 17 条管理路由只有 session 校验，无 CSRF/Origin 防护，攻击面比 round29 扩大一倍。

⚪ 占比：账本 67 项中 ⚪ 1 项（N12-2），1.5%，未用 ⚪ 当逃生口。

---

## 二 基线

| 对象 | 结果 |
|---|---|
| `go build ./...` | 绿（3s） |
| `go vet ./...` | 绿，无输出 |
| `go test ./internal/gateway/ ./internal/store/ ./internal/app/ ./internal/monitoring/` | **全绿**：gateway 42.769s / store cached / app 1.531s / monitoring 0.057s |
| 上轮唯一红（`TestConverterStreamWriterKeepsTheStatusItWasGiven`） | **已转绿**（`-v` 实测 PASS） |
| gofmt | 命中 6 文件，抽查 4 个（protocol_proxy.go / provider_lanes.go / router.go / sqlite.go）**全部整文件 CRLF**（`grep -c $'\r'` = 行数）⇒ 假阳性，非真实格式问题 |
| 卫生 | `zz_quota_keeper_e2e_hooks.go`（生产源码，见 W30-1）；无 `println("DBG…")`；无 `.bak`（round29 的 `config.go.bak` 已清） |
| web | 本轮未跑 `tsc -b`（时间盒，**未核查**） |

---

## 三 新提交审查

**本轮无新提交**（HEAD 未变）。第三节按规则无内容。

---

## 四 工作区 WIP 审查（126 文件，+20199/−900）

### 4.1 做对的（本轮实质进展）

| 项 | 证据 | 强度 |
|---|---|---|
| D29-1 `lane_group` 真落库 | sqlite.go:239 建列、:641/:650 写、:766/:787 读；两个迁移/回环测试本机绿 | E2+E3 |
| W29-4 红测试清零 | `-run TestConverterStreamWriter…` PASS，四个包全绿 | E3 |
| W29-5 监控中心接线 | runtime.go:194/:353、server.go:358-375、monitorGate 要求 admin | E2 |
| 监控中心安全基线合理 | 渠道密钥只回 `secretSet` 布尔（monitoring_handlers.go:43、:511-517）、adapter 错误用 `Secret` 替换为 `***`（channel.go:133-134）、`PruneMonitoringData` 有调用（runtime.go:322/:390）并按表设保留期（monitoring_migration.go:217-235） | E2 |
| R4 转换路径中断判定 | 6 处读后判定 + 4 腿测试绿 | E2+E3 |
| D29-2 部分修好 | protocol_proxy.go:833 与 provider_failover.go:1229 改用 `retryAfterSecondsFor` | E2 |

### 4.2 问题表

| # | 级别 | 问题 | 证据 |
|---|---|---|---|
| W29-1 | **P0（生产）** | 透传流全量缓冲，TTFT = 总生成时间 | PROBE-30STREAM（`round30-evidence/M2/probe-stream-passthrough.txt`）：`canDefer=true headerAt=400.563ms firstWrite=400.563ms lastWrite=400.563ms writes=1 flushes=0`；`canDefer=false` 同。根因：`passthrough_stream.go:447-477` 在 `io.Copy` 前**没有** `hw.WriteHeader`，`provider_failover.go:351-362` 未 passthrough 时只 `buf.Write`，:368-374 `Flush` 在 `!passthrough` 时是空操作 | 
| W29-3 | P1 | 心跳落缓冲，流中零 ping | 同上 `flushes=0`；`sse_heartbeat.go:127-133` → `fw.Flush` 空操作 |
| W29-2 | P1 | 每字节再复制进 `fw.body`（`provider_failover.go:359-362`、commitHeaders:398-404 各一份）且无人读 ⇒ 8MB 流常驻 2 份 | 代码 + `StreamLen()`/`streamClosed()` 全仓零调用（W29-11） |
| D29-2 | P1 | `server.go:4492` 仍 `retryAfterSeconds(provider.ID, …)` ⇒ 分组账号在该出口退回 1s 常量 | grep 三出口 |
| D29-3 | P1 | 组内限额取最后到达那行 | `provider_lanes.go:133-138` |
| D29-4 | P1 | 组内 0/0 行整组绕过 | `provider_lanes.go:559` |
| D29-5 | P2 | 组名大小写敏感（注释说不敏感） | `provider_lanes.go:542-546` |
| D29-6 | P2 | `providerInFlight` 按行号查 ⇒ 分组行恒 0，`leastUsedChain` 反复优先 | `provider_lanes.go:565-578` |
| W30-1 | P3 | 测试钩子写进生产源码（`zz_quota_keeper_e2e_hooks.go`，含 `httptest` 依赖） | 文件头 1-34 行 |
| W30-3 | P2 | 17 条新管理路由只校验 session，无 CSRF/Origin 校验（与既有端点同风格，但攻击面翻倍；Webhook/secret 可被跨站改写） | monitoring_handlers.go:118-127 + server.go:358-375 |
| W30-2 | P3 | 生产库已建 `monitor_events/alert_states/event_pushes/provider_metric_samples/baselines` 五表**全 0 行**（checkin_history 18 行）⇒ 线上 migration 已执行、采集未生效 | 生产只读查询 |

### 4.3 与 magpie 对照（M2 焦点）

magpie 的流式路径是单次 `io.Copy` 直发、不做 buffer-then-replay（`internal/gateway/gateway.go` 流式分支；`stream_cut_test.go` 在切断时**如实上报**而不回溯重放）。我们为了「截断可交给备用账号」引入了「先把整段收进 buf，最后再交出去」，代价就是 TTFT = 总时长。二者不是实现差异，是**目标冲突**：要么放弃「客户端零感知重试」，要么像 magpie 一样接受「首字节即承诺」。当前代码两头都没做到——既缓冲了，又保留了 `canDefer=false` 分支的语义混乱。

---

## 五 生产环境只读核查（173.249.207.202）

| 项 | 事实 |
|---|---|
| 服务 | `llm-protocol-gateway.service` active (running) since 2026-10-23 06:32:39 PDT，PID 911205，Tasks 21，Memory 1.4G |
| 二进制 | `/opt/llm-protocol-gateway/bin/gateway`，16,498,850 B，mtime `2026-10-23 06:32:24 PDT`（= 21:32:24 +0800，**与 round29 完全一致，未重新部署**） |
| 版本 | `vcs.revision=d93ec02c9365b20a65d0e92702a92b998b9632d7`，**`vcs.modified=true`** ⇒ 无 commit 可复现（连续第 9 轮） |
| 特征串 | `shares account lane`=1（d93ec02）、`upstream stream ended without its closing frame`=1（仅 WIP）、`lane_group`=**0**、`monitor_alert_states`=2 |
| 流式 A/B | `pass_through` 12h n=1063，`avg(latency−ttft)=0.7ms`、max 19ms；`convert` 12h n=8846，`avg=1159.5ms` |
| 监控表 | 五张 `monitor_*`/`provider_metric_samples` 均 0 行；`checkin_history` 18 行 |
| 数据 | 仅 `file:gateway.db?mode=ro` 打开做聚合查询与 count，**未写库** |

---

## 六 完整排查清单（按模块，全量；由 LEDGER.md 生成）

### M1 协议转换·请求

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| P1–P4, P6–P8 | 历史转换缺陷 | P1 | ✅ | 见账本 | 无 |
| P5 | Responses→Chat 忽略 incomplete | P2 | 🟡 | 部分字段 | 无 |
| P9 | redacted_thinking 占位非回放 | P2 | 🟡 | convert_* | 无 |
| P10 | strict/top_k 等不转发 | P2 | 🟡 | convert_* | 无 |
| P11 | 两两手写转换 vs 单一 IR | P2 | ❌ | 架构未动 | 无 |
| H-1 | Provider `name` 直发 Claude | P1 | 🟡 未核查 | convert_openai_claude_stream.go:248 有 restoreDeclaredToolName | 未复跑 |
| H-2 | json_object 直传 Claude | P1 | 🟡 | 新增 convert_response_format.go:19-20 映射 output_config.format | 见转换层（未运行验证） |
| H-3 | thinking 覆盖 output_config | P1 | 🟡 | claude_pass_through_normalize.go:89-94 | 未运行验证 |
| H-4 | allowed_tools 进 Claude tool_choice | P1 | 🟡 | convert_responses_allowed_tools.go | 未运行验证 |
| H-5 | cache marks 漏 tool_result | P1 | 🟡 | convert_openai_claude.go:150-151 仅 text | 未运行验证 |
| M-1 | Claude→Chat 无条件 stop | P1 | ✅ | r10 已纠错关闭 | 无 |

### M2 协议转换·流式

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| R4 | 流截断被当正常结束 | P0 | ✅ | 6 处 cleanEOFInterruption + 4 腿测试绿 | **转 ✅** |
| N14-2 | 转换路径中断判定恒 false | P0 | ✅ | 读后判定 | 无 |
| N10-1 | >256KB 流误判截断 | P0 | ✅ | 判据改 `usage.closed()` | 无 |
| N11-1 | 长流 usage 归零→放大 | P0 | ✅ | streamUsageReader | 无 |
| N11-2/3/4, N25-1, N24-3 | 心跳 join / 收尾帧 / 4B 携带 | P1–P2 | ✅ | 见账本 | 无 |
| **W29-1** | **透传流全量缓冲（TTFT=总时长）** | **P0** | **❌** | PROBE-30STREAM + 生产 A/B | **连续第 10 轮** |
| W29-2 | 响应再复制进 fw.body | P1 | ❌ | provider_failover.go:359-362,398-404 | 无 |
| W29-3 / N25-5 | 心跳落缓冲，流中无 ping | P1 | ❌ | 探针 flushes=0 | 无 |
| W29-4 | 红测试与实现矛盾 | P1 | ✅ | 本机 PASS | **转 ✅** |
| N25-3 / W29-6 | 非 Responses 流全量累积 | P2 | 🟡 未核查 | — | 未复跑 |
| W29-7 / N24-5 | 回声守卫只看末 64B | P2 | 🟡 | stream_end_reader.go:114-143 | 无 |
| N23-1 | canDefer 是死变量 | P1 | ❌ | 并入 W29-1 | 无 |
| N10-2 | 收尾写裸 w | P1 | ✅ | 走 hw | 无 |

### M3 调度/路由/并发

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| R1/R2/R3/R5 | 限流/冷却/billing/粘性 | P1 | ✅ | 见账本 | 无 |
| R6 | 闸门非 FIFO + 分组一致性 | P1 | 🟡 | D29-3/4 仍在 | 无 |
| **D29-1** | **laneGroup 不落库** | P0 | **✅** | sqlite.go:239/641/766 + 两个测试绿 | **转 ✅** |
| D29-2 | Retry-After 分三出口 | P1 | 🟡 | 2/3 修好，server.go:4492 未改 | 部分 |
| D29-3 | 组内限额取最后行 | P1 | ❌ | provider_lanes.go:133-138 | 无 |
| D29-4 | 0/0 行整组绕过 | P1 | ❌ | provider_lanes.go:559 | 无 |
| D29-5/D29-6 | 大小写 / providerInFlight | P2 | ❌ | 同上 | 无 |
| D29-7/D29-8 | 提交粒度 / lane 不回收 | P3 | ❌ | — | 无 |
| W29-11 | failover writer 死代码 | P3 | 🟡 | 仅 `stream()` 在用 | 略好 |

### M4 Provider/账号/OAuth

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| A1/A2/A4/A5/A6, N-1 | 锁/host/overage/提前量/出图/续期 | P1 | ✅ | 见账本 | 无 |
| A3 | images 路由 | P2 | 🟡 | server.go:4277 已接拒绝，:3099 仍盲拼 | 无 |
| H-6/H-7/H-9 | thinking 五连 / images 判定源 / 11128 矩阵 | P1 | 🟡 未核查 | — | 时间盒 |

### M5 计费/用量/日志

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| M-19 | 错误响应估算计费 | P1 | ✅ | 已修 | 无 |
| N11-1 | 长流 usage | P0 | ✅ | streamUsageReader | 无 |
| N23-5 | stream+JSON usage=0 | P2 | 🟡 | — | 无 |
| N13-2 | /__usage-analysis 无上限 | P2 | ❌ | 未改 | 无 |
| W29-9 | streamUsageFolder reflect 脆 | P3 | ❌ | — | 无 |
| W29-5 | monitoring 未接线 | P2 | ✅ | runtime.go:194/353 | **转 ✅** |

### M6 安全/鉴权/隔离

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| S1/S2/S4 | CORS / body 上限 / 脱敏 | P0–P1 | ✅ | 见账本 | 无 |
| S3 | 登录限流死代码/TOCTOU | P1 | 🟡 | 未改 | 无 |
| S5 | 首包后空闲超时 | P1 | 🟡 | 守卫在，心跳被 W29-3 吞 | 无 |
| H-8 | 管理员 Key 链不过滤 | P1 | 🟡 未核查 | — | 时间盒 |
| **W30-3** | **17 条新管理路由无 CSRF/Origin** | P2 | **新增** | monitoring_handlers.go:118-127 | 新 |

### M7 存储/迁移

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| D29-1 | lane_group 列 | P0 | ✅ | sqlite.go:239 + 迁移测试 | 转 ✅ |
| W29-5 | MigrateMonitoringV1 无调用者 | P2 | ✅ | runtime.go:194 | 转 ✅ |
| W30-2 | 生产 monitor 表 0 行 | P3 | 新增 | 生产只读 | 新 |

### M8 前端控制台

| 项 | 状态 | 依据 |
|---|---|---|
| 实时流（EventSource） | ❌ 未核查本轮 | 时间盒，未跑 grep/tsc |
| 路由决策聚合 / 会话视图 / 一键重放 / 成本列 | ❌ | 上轮实证零命中 |
| 监控中心前端（新增） | 未核查 | `web/src/components/` 有改动 24 行 main.tsx、usage/pricing/quota_keeper |
| 告警未读角标 | 🟡 | 侧边栏无角标 |

### M0 基线与卫生 / M9 系统面

| 编号 | 问题 | 级别 | 状态 | 依据 |
|---|---|---|---|---|
| N13-1 | `gateway version` 会起服务 | P2 | ❌ 未改 | 本轮未复现（已改用 grep -a） |
| N13-3 | 容器无 TZ | P3 | ❌ | 未改 |
| W30-1 | 测试钩子进生产源码 | P3 | 新增 | zz_quota_keeper_e2e_hooks.go |
| W30-2 | 生产 monitor 表空 | P3 | 新增 | 生产只读 |

---

## 七 回退与假修复

| 项 | 结论 |
|---|---|
| 回归 | **本轮无状态从 ✅ 变非 ✅**（D29-1/W29-4/W29-5/R4 四项均向前） |
| 关联回退嫌疑 | 无。W29-1/W29-3 是「一直没修」，不是回退 |
| 假修复（历史遗留） | D29-2「提交说明称协议代理与 failover 两出口都改了」——实际改了两处但漏了 `server.go:4492`，仍属**声称范围 > 实际范围**，维持 🟡 |
| 连续两轮同一项 | W29-1 已连续两轮（r29、r30）报 P0 且未动；按规则升级表述并点名，见「结论先说」第 1 条 |

---

## 八 下轮验收门槛（可机械验证）

1. `go build ./... && go vet ./...` 绿；`go test ./internal/gateway/ ./internal/store/ ./internal/app/ ./internal/monitoring/` 0 红。
2. 透传流首字节 < 末字节 50%：在 `writeBufferedStream` 的 `io.Copy` **之前**调用 `hw.WriteHeader(response.StatusCode)`（或等价提交），复跑 PROBE-30STREAM 须满足 `firstWrite ≪ lastWrite 且 writes ≥ 2`。
3. 生产 `request_logs` 里 `action='pass_through'` 的 `avg(latency−ttft)` 从 0.7ms 升到 >200ms（同一查询，12h 窗口）。
4. 流式期间客户端能收到 `: ping`：探针把 `sseHeartbeatInterval` 调到 20ms、上游静默 100ms，客户端须收到至少 1 个 `: ping`（当前 `flushes=0`）。
5. `fw.body` 在请求结束后不再持有整段：≥8MB 流跑完后断言 `fw.body.Len()==0`（或删掉这条复制）。
6. `provider_lanes.go` 组限额来源唯一：同组 A(max=2)、B(max=10) 交替请求，`inflight` 上限恒为 2（或恒为 10），不得抖动；组内 0/0 行请求必须被组的 lane 限制。
7. `server.go:4492` 改用 `retryAfterSecondsFor`；分组 provider 三出口返回同一数值。
8. `vcs.modified=false` 且 `vcs.revision` = 已推送提交（连续 9 轮未达标）。
9. 监控：`zz_quota_keeper_e2e_hooks.go` 移入 `_test.go`；`/__monitor/*` 至少对 POST/PUT/DELETE 加 Origin 同源校验。
10. 生产重启后 `monitor_events` 24h 内出现非零行（验证采集真的在跑），否则视为「接了个空壳」。

---

## 附 覆盖度声明

| 模块 | 已扫（细读/实测） | 该模块文件总数 | 未核查 |
|---|---|---|---|
| M0 基线卫生 | build/vet/4 包 test/gofmt/zz/bak/println | 全仓 | web `tsc -b` |
| M1 协议转换·请求 | 4 个 grep 命中点 | 26 个 convert_*.go | H-1..H-9 语义未逐用例复跑 |
| M2 流式 | **实测**：passthrough_stream.go 全文、sse_heartbeat.go 全文、provider_failover.go 200-440 | 12 | stream_end_reader 非 Responses 累积（N25-3/W29-6） |
| M3 调度 | provider_lanes.go 全文关键段、三处 retryAfter 出口 | 9 | 队列竞态未跑 -count=10 |
| M4 账号 | grep 抽样 | 31 | H-6/H-7/H-9 未复跑 |
| M5 计费 | 生产 SQL 聚合 + grep | 14 | 缓存 token 重复计数未复核 |
| M6 安全 | monitorGate、路由表、CSRF 缺失 | 11 | CORS 逐端点未复跑 |
| M7 存储 | sqlite.go 列/读写、monitoring_migration.go 保留期 | 13 | 迁移在超大库上的耗时未测 |
| M8 前端 | 未扫 | 22 | **全部未核查**（时间盒） |
| M9 系统面 | 生产 systemd/二进制/只读 DB | — | 隧道未触发 |

未核查项已在第六节逐条标注，**不视为通过**。

---

## 收尾自检

- [x] `git status --porcelain` 只剩 `.workbuddy/reviews/` 产物 + 作者自己的改动（我的 `zz_probe30_test.go` 已删，源码存证于 `round30-evidence/M2/`）
- [x] 无临时 worktree 残留（本轮未创建；`git worktree list` 仅主树）
- [x] 账本已建（`LEDGER.md`，67 行），每个未闭环 ID 都在第六节出现
- [x] 每个 ✅ 均指到 E1/E2/E3（D29-1=E2+E3、W29-4=E3、W29-5=E2、R4=E2+E3）
- [x] 覆盖度声明已填，未核查项已列举
- [x] 无 commit / push / pull / reset；生产仅 systemctl status / ls / sha256sum / grep -a / sqlite ro

---

## 给修复 agent 的订单（最小必要改动）

**P0（一条，改完这一条才算能上线）**
1. `internal/gateway/passthrough_stream.go` `writeBufferedStream`：在 `io.Copy` 之前提交响应头并置 passthrough（例如 `hw.WriteHeader(response.StatusCode)` 后 `hw.Flush()`），使首块字节立即下发；随后再决定截断/重试语义。若坚持「零感知重试」，就把 `canDefer` 真正接进 `writePassThroughResponse` 的判断（末次尝试不缓冲），而不是让它当死变量。验收：门槛 2 + 3。

**P1**
2. `sse_heartbeat` 在 passthrough 提交后必须真 flush（与 1 同因）；验收门槛 4。
3. `provider_failover.go` 去掉 `w.body.Write(p[:n])`，或为它加硬上限并让 `Prefix()/StreamLen()/streamClosed()` 真正被使用，否则删掉整条状态；验收门槛 5。
4. `provider_lanes.go`：组限额来源改成「组内首个非零配置」或独立组级设置；`acquireProviderSlot` 在 `providerGrouped` 时不因本行 0/0 提前返回；`server.go:4492` 改用 `retryAfterSecondsFor`。验收门槛 6 + 7。

**P2**
5. `providerLaneKey` 统一 `ToLower`；`providerInFlight` 走 `providerLaneKey`。
6. `/__monitor/*` 的写操作加 Origin 同源校验。

**P3**
7. `zz_quota_keeper_e2e_hooks.go` → `_test.go`；补一次带 `vcs.modified=false` 的正式构建并部署（连续 9 轮的治理债）。
