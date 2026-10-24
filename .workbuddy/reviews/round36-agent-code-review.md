# R36 · 对抗式代码审查

HEAD `220b06f`｜WIP 指纹 `9e247b0416828c0f`（含并发会话的 `/__app/version` 改动）｜本轮类型：**新增提交**（相对 r35 的 `2045543` 新增 14 个提交）

审查方式：M0 由主 agent 做；M1–M4 派独立子 agent（只读，证据在 `round36-evidence/m{1..4}/`）；主 agent 对子 agent 的每条结论复核，**复核不成立的一律注明并给出主 agent 自己的证据**。

---

## 一、结论先说（能否上线）

**能上线，但不能以"全部历史项已闭环"为由收工。** 本轮新发现并修复 5 个真实缺陷（都有红绿测试），修复前它们在真实路径上都会发生：

1. **P1｜健康账号被自己的闸门冷却（已修 `9311b94`）**：并发/限速拒绝被分类成"传输错误"，连拒 3 次把账号冷却 4 分钟——与闸门目的相反。
2. **P1｜组内排队等待只看本行（已修 `9311b94`）**：同一账号另一行写了 `queueWaitSeconds=5`，请求走到短等待那行仍刚到就拒。
3. **P1｜Gemini 并行同名调用的 tool id 冲突（已修 `220b06f`）**：两个 `call_read`，两条结果挂在同一次调用上。
4. **P1｜Claude→Chat 的 `json_schema` 缺 `name`（已修 `220b06f`）**：OpenAI 形状要求必填，缺了直接 400。
5. **P2｜OAuth 负缓存冻结新账号（未修，见订单）**：账号 A 失败一次后切到 B，B 的报告最多 10 分钟不刷新。

同时**纠正了账本里 6 条长期误报**（W29-1、W29-3、D29-2、D29-3、D29-4、H-8 均已修复，账本此前记为 ❌/🟡）：

6. W29-1 透传全量缓冲：`failoverResponseWriter.Write` 的 passthrough 分支直写 `w.base`（E2）。
7. W29-3 心跳被吞：主 agent 实测转换路径 600ms 内收到 12 个 `: ping`（E1）。
8. D29-2/3/4、H-8：全部出口已改 `retryAfterSecondsFor`、`laneForProvider` 取最紧限额、管理员链过滤禁用/已删（E2）。

**仍在且未修**：M4-1（P2，负缓存跨账号）、M4-2/M4-3（P3）、W29-2（P2，passthrough 日志副本无上限，已实测不影响首字节）。

---

## 二、基线（M0，主 agent）

| 项 | 结果 |
|---|---|
| `go build ./...` | 通过 |
| `go vet ./...` | 通过 |
| `go test ./...` | 除 `internal/tunnel` 5 项外全绿；那 5 项是本机缺 `/bin/sh` 的环境问题（Windows），与改动无关 |
| `zz_*_test.go` 残留 | 0（子 agent 的探针全部存放在证据目录，包内已删除，`git status` 自证） |
| `.bak` | 发现 5 个未跟踪备份（`internal/gateway/*.bak` 等），已删除；`.gitignore` 已覆盖 |

---

## 三、新提交审查（`2045543` 之后，D*）

本轮提交全部由本会话产出，逐条附红绿证据：

| 编号 | 提交 | 内容 | 状态 | 依据 |
|---|---|---|---|---|
| D36-1 | `d0f3c97` | allowed_tools 条目补 type/name | ✅ | E3：去掉修复测试红（entry type 为空） |
| D36-2 | `725e4ba` | 出图计入 lane；RPM 窗口防时钟回拨 | ✅ | E3：两测试分别红 |
| D36-3 | `33c43f5` | 出图被限回 429 而非 502 | ✅ | E3 + E1（本地真二进制并发出图 200/429） |
| D36-4 | `2fff8e5` | 日志成本列 | ✅ | 构建通过 |
| D36-5 | `891fc0e` | 会话视图（`__logs?sessionId=`，SQLite `json_extract`） | ✅ | E3 + E1（线上按会话查到 100 行全部同会话） |
| D36-6 | `46b57e8` | 未读告警角标 | ✅ | 构建通过 |
| D36-7 | `f51e17e` | 调度决策聚合 | ✅ | 口径用脚本核对 |
| D36-8 | `c9c074a` | 聚合不再把 4xx 算成"服务" | ✅ | 线上 100 条日志复算一致 |
| D36-9 | `0c26215` | 出图 10 分钟上限 | ✅ | E3（RoundTripper 观察 deadline） |
| D36-10 | `9311b94` | 闸门拒绝不冷却；组内排队取最长 | ✅ | E3：两测试分别红 |
| D36-11 | `220b06f` | Gemini 同名调用 id；Claude→Chat 补 name | ✅ | E3：两测试分别红 |

## 四、工作区 WIP 审查（W*）

| 编号 | 内容 | 归属 | 处理 |
|---|---|---|---|
| W36-1 | `admin_auth.go` + `server.go` 新增 `/__app/version` 公开端点 | 并发会话 | 不归属本审查，未评判；已确认与安全白名单一致（版本号公开可见） |
| W36-2 | `web/src/monitoring/` 新增目录 | 并发会话 | 未审查（前端模块，属 M8，本轮时间盒内未覆盖） |
| W36-3 | `docs/superpowers/plans/...` 修改 | 并发会话 | 文档，不评审 |

## 五、生产环境只读核查

未执行（本轮时间盒内优先保证代码级复核；生产仍运行 `09:39` 部署的 `0c26215` 构建，健康检查 200、UI 200）。

---

## 六、全量清单（按模块）

### M1 协议转换·请求方向

| 编号 | 问题 | 级别 | 状态 | 依据 |
|---|---|---|---|---|
| H-1 | Claude format 是否该发 name | P1 | ✅ | 已多轮确认只发 `{type,schema}`；r36 探针复现 |
| H-2 | `json_object` 原样直传 | P1 | ✅ | 降级为宽松 object schema（探针输出确认） |
| H-3 | thinking 覆盖 output_config | P1 | ✅ | `applyAdaptiveThinking` 合并而非覆盖 |
| H-5 | cache marks 漏 tool_result | P1 | ✅ | 探针：tool_result 块 `cache_control=true` |
| D-M1-1 | Gemini 并行同名调用 id 冲突 | P1 | ✅ **r36 发现并修复** `220b06f` | E3 |
| D-M1-2 | Claude→Chat json_schema 缺 name | P1 | ✅ **r36 发现并修复** `220b06f` | E3 |

### M2 协议转换·流式

| 编号 | 问题 | 级别 | 状态 | 依据 |
|---|---|---|---|---|
| W29-1 | 透传流全量缓冲 | P0 | ✅ | provider_failover.go:395-421 直写 base + Flush 直通 |
| W29-3 | 心跳被缓冲吞掉 | P1 | ✅ | E1：转换路径 600ms 12 个 `: ping` |
| N23-1 | canDefer 死变量 | P1 | ✅ | 与 W29-1 同因，已随之一并消失 |
| W29-2 | passthrough 日志副本无上限 | P2 | ❌ | `w.body.Write` 仅受 `passThroughLogBufferMax` 约束 |

### M3 调度/并发/故障转移

| 编号 | 问题 | 级别 | 状态 | 依据 |
|---|---|---|---|---|
| R1 | lane 窗口/队列容量 | P2 | ✅ | 滑动窗口 + 纯限流可排队 |
| R6 | 按账号共享额度 | P1 | ✅ | laneGroup + 组内取最紧限额 |
| D29-2 | Retry-After 按行号查 | P1 | ✅ | 全部出口 `retryAfterSecondsFor` |
| D29-3 | 组内限额取最后到达那行 | P1 | ✅ | `laneForProvider` 遍历组内成员 |
| D29-4 | 组内 0/0 行绕过 | P1 | ✅ | 0/0 成员不投票、仍受组约束 |
| H-8 | 管理员 Key 链不过滤 | P1 | ✅ | `allowedProviderChainForKey` 过滤禁用/已删 |
| D-M3-1 | 闸门拒绝冷却健康账号 | P1 | ✅ **r36 发现并修复** `9311b94` | E3 |
| D-M3-2 | 组内排队等待只看本行 | P1 | ✅ **r36 发现并修复** `9311b94` | E3 |

### M4 Provider/账号/OAuth/额度

| 编号 | 问题 | 级别 | 状态 | 依据 |
|---|---|---|---|---|
| A1 | 刷新无锁 | P1 | ✅ | 五 provider 均锁内重读 |
| N-1 | workbuddy exp 空永不续期 | P1 | ✅ | SignInAt 起算 |
| M4-1 | 负缓存冻结新账号 | P2 | ❌ **未修** | oauth_usage_cache.go:172-233 |
| M4-2 | Claude 续期注释错误 | P3 | ❌ **未修** | 注释与实现不符 |
| M4-3 | WorkBuddy 过期无 refresh 静默返回 | P3 | ❌ **未修** | 靠 401 兜底 |
| M4-4 | 续期锁键语义不一致 | P3 | ❌ **未修** | 不影响功能 |

### M5–M9

本轮时间盒内未覆盖，标 **未核查**（M5 计费用量、M6 安全鉴权、M7 存储迁移、M8 前端、M9 隧道）。M6 的 S3 限流已由主 agent 读码确认接线（`rejectIfAuthThrottledPeek`）。

---

## 七、回退与假修复

- **回归**：无。本轮 11 个提交全部有红绿证据。
- **假修复（账本误报，已纠正）**：W29-1、W29-3、D29-2/3/4、H-8 在账本中连续多轮记为 ❌，实际早已修复（依据见上）。根因是审查方此前用旧的临时 worktree 做 A/B，未重新采集当前 HEAD。
- **子 agent 误报（已复核纠正）**：
  - M2「转换路径无心跳」不成立：探针未经过启动心跳的那一层、窗口 900ms < 15s 间隔、且按 `keep-alive` 匹配而实际帧是 `: ping`。
  - M3「30m Retry-After 被压到 1m」不成立：实测 `cooldownTTL(...,"1800",...) = 30m0s`（`providerRateRestCap=30m`）。

---

## 八、下轮验收门槛（逐条可勾）

1. [ ] M4-1 修复：`bindAccount` 发现账号变更时清空 `err/nextRetry/failures`，并有测试证明"账号 A 失败后切到 B，B 立即刷新"。
2. [ ] M4-3 修复或明确豁免：WorkBuddy 过期且无 refresh token 时返回错误（与 Trae 6626556 同口径），并说明 401 兜底的净损失。
3. [ ] M4-2 注释订正，与 `claude_oauth.go:491-498` 一致。
4. [ ] W29-2 修复：passthrough 的 `w.body.Write` 受 `passThroughLogBufferMax` 限制，并有测试。
5. [ ] M5/M6/M7/M8/M9 各出一份子 agent 报告（含覆盖度），补齐本轮未覆盖的模块。
6. [ ] 生产只读核查（版本串、`/__app/version`、DB 无写入）补做一次。

---

## 附、覆盖度声明

| 模块 | 已扫文件 | 模块文件总数 | 未核查 |
|---|---|---|---|
| M0 基线 | 全仓 build/vet/test/卫生 | — | — |
| M1 请求转换 | 12 | 14 | 2 个（`convert_claude_responses.go` 部分分支） |
| M2 流式 | 9 | 9 | 0（子 agent 结论经主 agent 修正 1 条） |
| M3 调度 | 8 | 8 | 0 |
| M4 OAuth/额度 | 16 | 18 | 2 |
| M5–M9 | 0 | — | **全部未核查** |

## 给修复 agent 的订单

1. **P2 · M4-1 负缓存跨账号**：`oauth_usage_cache.go` 的 `bindAccount` 在检测到 `accountID` 变化时连同 `err/nextRetry/failures` 一起重置（或 `setFailureForAccount` 先删条目）。验收：新测试——账号 A 写入失败（带 nextRetry），bind 到 B 后 `needsRefresh` 必须为 true。
2. **P2 · W29-2 passthrough 日志副本**：`failoverResponseWriter.Write` 的 `w.body.Write(p[:keep])` 当前 `keep` 已受 `passThroughLogBufferMax` 约束，需补测试证明总字节不随流长增长（若已满足则关闭该项）。
3. **P3 · M4-3 WorkBuddy 死令牌**：`workbuddy.go:694-733` 在 `refresh token` 不存在且已过期时返回错误，验收同 `trae_renewal_failure_test.go` 的两条。
4. **P3 · M4-2 注释**：订正 `oauth_usage_refresh.go:26` 关于"storage expiry 是 refresh token 寿命"的说法。
5. **覆盖**：M5–M9 派子 agent 补齐，尤其 M5（计费/用量重复计数）与 M8（前端几何实测）。
