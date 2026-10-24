# 第二十八轮审查报告（round28）

- 审查时间：2026-10-23 20:28–21:0x（+0800）
- HEAD：`80558d4`（与 round27 相同，**零新提交**）
- 工作区：64 文件 `+3650/−622` + 6 个未跟踪新文件；`sha256(git diff HEAD)=f94f259d…`，与 round27 末次快照**逐字节一致**
- 本轮唯一实质新增：**两份设计文档 + 3 份 HTML 稿**（全部未跟踪）
  - `docs/superpowers/specs/2026-10-23-monitoring-center-design.md`（762 行）
  - `docs/superpowers/plans/2026-10-23-monitoring-center-phase-1.md`（3046 行）
  - `.superpowers/brainstorm/43832-1792745803/{layout-home,overview-final,visual-style}.html`

---

## 一、结论先说

1. **代码没有变**：没有再提交，工作区里 64 个文件的改动与 round27 审查时的快照字节一致。所以 round27 的全部结论（P0-1 透传缓冲、P1-1 cleanEOF 缺一腿、2 个红测试、生产脏构建）**原样成立**，本轮已逐条带行号复核。
2. **本轮要看的新东西是设计文档**。质量意外地高：我核对了 20 多处"计划里引用的代码事实"，**行号 100% 命中**，SQL 列名/过滤值/函数签名/helper 存在性基本都对。
3. **但不能按现状开工**：文档里有 **3 处编译级不自洽**、**2 处验收命令在目标环境必然失败**，而执行前提是「64 文件脏工作区 + 2 红测试 + 生产跑着 `vcs.modified=true` 的脏构建」——计划开篇还明确要求"不要新建 worktree，直接在当前工作区做"。
4. 生产：`ActiveState=active`，二进制 `2026-10-23 04:20:51 PDT`（=19:20:51 +0800），`vcs.revision=80558d40…` 且 **`vcs.modified=true`** ⇒ 线上就是"HEAD + 未提交 WIP"，**连续第 7 轮无 commit 可回滚**。
5. 基线：`go build ./...` / `go vet ./...` 绿；`go test ./internal/gateway/ ./internal/store/ ./internal/app/` = **2 红**（`TestRateOnlyLaneAdmitsWhenItsWaitExpires`、`TestRateOnlyLaneWaitsOutItsBudgetBeforeRefusing`），与 round27 同一对。

---

## 二、监控中心 spec / plan 审查

### 2.1 事实性核对（计划的断言 vs 代码实测）

| # | 计划/spec 的断言 | 实测结果 | 结论 |
|---|---|---|---|
| 1 | Go 1.26 | `go.mod` → `go 1.26.1` | ✅ |
| 2 | `request_logs` 有 provider_id/action/protocol_flow/output_tokens/ttft_ms/latency_ms/cost_micros/error_description/access_source | `store/request_logs.go:483-509` 全有 | ✅ |
| 3 | 排除条件 `protocol_flow='embeddings'` | `server.go:4487-4528` 写的就是字面量 `"embeddings"` | ✅ |
| 4 | `action` 只有 convert / pass_through（正常代理） | `router.go:1999-2004` | ✅ |
| 5 | `status<>499` 排除客户端断开 | `server.go:1286 statusClientClosedRequest=499` | ✅ |
| 6 | 迁移 `INSERT OR IGNORE … SELECT provider_id,day,at,outcome,credit,streak,msg,valid_until,offline,tries` | `store/checkin.go:16-28` 列存在且与 §6.4 目标列顺序一致 | ✅（但丢列，见 D28-9） |
| 7 | 旧告警规则键 `apikey_multi_ip` / `apikey_concurrent_ip` | `domain/types.go:80,86` | ✅ |
| 8 | 旧告警"每 2 分钟、两条规则、只推 Telegram" | `alerts.go:22-25`（2min）、`:284 pushAlert(alert, telegram)` | ✅ |
| 9 | 自检"每 provider 跑 6 条 CLI 用例" | `selfcheck.go:382-383` = 3 CLI × chat/tools | ✅ |
| 10 | 在 `migrate()` 末尾加 `ensureMonitoringSchema(tx)` 能作用于存量库 | `sqlite.go:114-360` 每次 Open 无条件执行，非版本门控 | ✅ |
| 11 | `settings` 具备 `Setting/SetSetting` | `sqlite.go:1232/1237` | ✅ |
| 12 | `stateSaver[0]` 不能裸索引（现有代码有守卫） | `server.go:211-213` 已有 `len(stateSaver) > 0` | ✅ |
| 13 | 续期调用点 8 处、行号 437/559/343/627/173/175/735/673 | 逐一 grep 命中，**行号全对** | ✅（漏 1 处，见 D28-11） |
| 14 | 额度 fetcher 签名（`fetchClaudeOAuthUsage(ctx,token)`、`fetchChatGPTOAuthUsage(ctx,provider)`、`qoderQuotaFor → (provider,quota)`、`workBuddyQuota`） | 全部匹配 | ✅ |
| 15 | Claude 报告有 `SevenDayOverageIncluded` | `claude_oauth_usage.go:33` | ✅ |
| 16 | `writeOpenAIError` / `requireAdmin` / `startStorageMaintenance` 存在（且无 `writeError`） | `server.go:6174`、`users_api.go:15`、`runtime.go:353` | ✅ |
| 17 | `deploy-linux.sh --skip-web` | `deploy-linux.sh:74` | ✅ |
| 18 | 服务名 `llm-protocol-gateway`、DB `/var/lib/llm-protocol-gateway/gateway.db` | systemd 实测 | ✅ |
| 19 | 规则数 18 / 阶段 1 为 16（`probe_failed`、`checkin_failed` 留阶段 3） | spec §8.1 表 18 行；§17.1 与 plan 均写 16 | ✅ 自洽 |
| 20 | 概览布局"上方总览+账号矩阵，下方事件流" | `overview-final.html` 含 可用账号/活跃告警/成功率/账号健康/链路健康/事件流 | ✅ |

这一节结论：**可以放心把它当技术依据**。问题集中在下面 2.2/2.3 的实现细节。

### 2.2 P1：照抄即编译失败 / 验收必然失败

| # | 问题 | 位置 | 证据 |
|---|---|---|---|
| D28-1 | 计划自相矛盾：`if rules, err := UnmarshalRules(rawRules); err == nil`，而 Task 2 自己的定义是 `func UnmarshalRules(raw string) []RuleConfig`（单返回值） | plan:2209 vs plan:756 | 照抄 → `assignment mismatch: 2 variables but UnmarshalRules returns 1 value` |
| D28-2 | `s.engine.Rules()` 不存在：Task 6 的 Engine 只有 `NewEngine/Evaluate/SetRules/EvaluateCollectorHealth` | plan:2219 vs plan:1599-1610 | 照抄 → `s.engine.Rules undefined` |
| D28-3 | 全部验收命令用 `sqlite3`，**VPS 实测 `sqlite3: MISSING`**（只有 python3 3.45.1） | plan:2946/2947/2955/2956/2991-3021 | Task 14 的 Step 3/4/6 每一条都会 `command not found`（round24 已踩过同一个坑） |
| D28-4 | 演练数据直接写**生产库** `request_logs`：16+14+30 条 `action='convert'`、`path='monitor-selftest'`，靠事后 DELETE 清理 | plan:2990-3026 | 规则 SQL 不过滤 path/access_source ⇒ 演练期间这批假 401 计入真实账号窗口，可能顶掉真告警或凭空造 P0；事后 DELETE 又会让已 firing 的状态缺样本，转 `data_unavailable`。**spec §6.8 已经定义了 `access_source='monitor_probe'` 并在规则 SQL 排除——演练应该复用它，而不是 path** |
| D28-5 | Task 13 把 `go test ./...` PASS 当验收门槛 | plan:2919-2920 | ① Windows 下 `internal/tunnel` 依赖 `/bin/sh`（round20 已判环境性失败）本机不可能 PASS；② 当前工作区本来就 2 红，而计划**没有"先把基线清零"的前置任务**，从 Task 1 起就在红基线上叠加 |

### 2.3 P2：语义 / 设计缺口

| # | 问题 | 说明 |
|---|---|---|
| D28-6 | `collector_stale` 双评估 | Task 6 的 `Evaluate` 按 provider/**key/gateway** 生成 evaluations，Task 9 又另起 watchdog goroutine 调 `EvaluateCollectorHealth` ⇒ 同一 fingerprint 每分钟可能被两个 goroutine 各写一次状态与事件（Upsert 竞争 + 重复通知）。Task 6 注释写了"不能用普通 Evaluate"，但没有规定 `Evaluate` 必须跳过 `collector_stale` |
| D28-7 | 能力声明与 fetcher 不同源 | `providerHasQuotaHandler`（计划新建）若按"有额度面板"判断，会把 Zhipu/DeepSeek（确有 `zhipu_usage.go` / `deepseek_usage.go`）算成支持；但 `quotaForProvider` 的 switch 只覆盖 Claude/ChatGPT/Cursor/Qoder/WorkBuddy ⇒ 这两个账号会「能力=支持、规则=unavailable」，`quota_low` 永不生效 |
| D28-8 | `oauth_expiring` 永不告警的陷阱 | 条件要求 `Renewal.Known && !Renewal.LastOK`；从未续期过（新接入/长期未到期）的账号 `Known=false` ⇒ 到期前 2 小时也不会告警，与 spec §2.1「凭证问题 2 分钟内发现」直接冲突。应改为"到期 < lead 且（续期未知 **或** 最近一次失败）" |
| D28-9 | `checkin_history` 丢 `account_key` | `checkins` 有 `account_key`（`checkin.go:18`），§6.4 建表与迁移 SELECT 都没有它；PK 是 `(provider_id, day)` ⇒ 同一天换绑账号时历史互相覆盖，`checkin_failed` 的"连续自然日"会串号 |
| D28-10 | 采集查询没规定走只读连接池 | Store 是**单写连接** `SetMaxOpenConns(1)`（`sqlite.go:52`）+ `readDB` 4 连接（`:68`，注释明写"防止重型查询饿死热路径写入"）。计划里每分钟的窗口聚合、每小时基线重算、overview 24h 汇总都没说明用 `reader()`；若直接用 `s.db`，采集 tick 会和请求日志写入抢唯一写连接 |
| D28-11 | 续期埋点片段类型不符 + 漏一处 | `s.recordRenewal(refreshed, "claude", …)` 而 `recordRenewal` 收 `domain.Provider`，此处 `refreshed` 是 `domain.ClaudeOAuthCredential`（plan:2706）；下文虽补了"成功后用 updated provider"，4 个低层调用点照抄前者即编译失败。另：`trae.go:468` 也是 `traeRenew` 的调用点（重新交换凭证），计划表只列了 `:735`，会漏埋点 |
| D28-12 | 计划依赖不存在的子技能 + 未跟踪目录归属未定 | 头部写 `REQUIRED SUB-SKILL: superpowers-subagent-driven-development / executing-plans`，本机 user/project 技能清单里都没有这两个；`.superpowers/` 与 `docs/superpowers/` 仍未纳入版本控制（round27 已提示"无关目录不入库"，现在它变成主要交付物，需要先定：入库 or `.gitignore`） |

### 2.4 与控制台 6 项缺口的关系

监控中心解决的是**发现**（账号/额度/链路/风控），**不解决排障**。本轮再次 grep 复核：

- `EventSource` 命中 **0**（spec §12.2 明确写 15 秒轮询）→ 实时流仍缺
- `sessionId` 在 `.tsx` 命中 **0** → 会话视图仍缺
- 日志表无成本列（`panels.tsx` 里 `cost` 命中 0；`costMicros` 只出现在用量分析卡）
- 无重放按钮
- 侧栏无未读角标（`main.tsx` 的 `unread` 只是告警页内筛选）

⇒ 原清单的"第 4 批：控制台排障链路"必须保留为独立批次，不要指望阶段 2 的"监控中心壳"覆盖它。

---

## 三、历史问题复检（本轮抽验，均带行号）

| 项 | 状态 | 实证 |
|---|---|---|
| P0-1 透传流全量缓冲 | ❌ **仍在** | `passthrough_stream.go:245-249` 只要 w 是 `*failoverResponseWriter` 且 stream 就进 `writeBufferedStream`；该函数 386-470 **全程无 WriteHeader**，只在末尾 `FlushError()` 一次性落网。对照转换路径 `protocol_proxy.go:695` 有 `w.WriteHeader(http.StatusOK)` ⇒ 同一 writer 两条路径两种行为 |
| P1-1 Chat→Responses 缺 cleanEOF | ❌ **仍在** | `convert_openai_responses_chat_stream.go:666` 只有 `isUpstreamStreamInterruptedReader(r)`；其余 4 腿都是 `… \|\| cleanEOFInterruption(r, closed)`（`:216`/`:430`/`:297`/`:1021`） |
| 2 个 rate-only lane 测试 | ❌ **仍红** | `provider_rpm_test.go:226`、`provider_lane_capacity_test.go:218`，与 round27 P2-1「acquire 的 wait=0 立即拒绝 vs 测试期望等满窗口」同一冲突，30+ 小时未处理 |
| 控制台 6 项 | ❌ 全部未动 | 见 §2.4 grep 证据 |
| 生产脏构建 | ❌ 连续第 7 轮 | `vcs.revision=80558d4` + `vcs.modified=true`，二进制 19:20:51+0800 |
| H-1..H-9、M-19、N-1、P1-2..P1-8、P2-2..P2-15 | 无代码变动 ⇒ 原样 | 工作区 tracked 指纹与 round27 逐字节一致（新增物只有 5 个未跟踪文档/稿） |

**本轮不重复报已修正项**：round27 判定的 `cleanEOFInterruption` 覆盖 4/5 腿、长流 usage、R2 阶梯可达、响应体脱敏、CORS/body 上限、`LaneGroup`/`SignInAt` 之外的令牌锁等仍然成立。

---

## 四、建议的推进顺序

0. **先清场（不做这步，后面全是叠加伤害）**
   - 把 64 文件 WIP 落成若干提交（或 stash），让 `go test ./internal/gateway/` 归零；
   - 谈定 rate-only lane 的语义（实现 vs 测试二选一，别再漂移）；
   - 生产重新按 commit 构建，验收 `vcs.modified=false`。
1. **协议/流式三连修**（都只是一到几行）：
   - P0-1：`io.Copy` 前显式 `hw.WriteHeader(response.StatusCode)`（转换路径已经这么做，照抄即可）；
   - P1-1：`:666` 补 `closed` 跟踪 + `cleanEOFInterruption`；
   - P1-2：WorkBuddy 未识别 400 的 `restore()` 兜底。
2. **设计文档侧修 5 条 P1**（D28-1..D28-5）再开工：两处 API 签名、sqlite3→python3、演练改用 `access_source='monitor_probe'`、验收命令换成 `go test ./internal/...`（Windows 排除 tunnel）+ 前置"基线清零"任务。
3. **阶段 1 实施**按 Task 1→14 顺序；每个 Task 结束跑 `go test ./internal/monitoring/... ./internal/store/... ./internal/gateway/...`，不要跑 `./...`。
4. **D28-6..D28-12 进 spec 修订**（尤其 D28-7 能力同源、D28-8 oauth_expiring 条件、D28-10 只读池），这些不改也能跑，但会在上线后变成"规则永远不响"的哑火项。
5. 控制台排障链路（实时流/路由解释/会话/重放/成本列/角标）仍然独立排期。

---

## 五、验收门槛（本轮新增，合入前必须满足）

1. `go build ./...` + `go vet ./...` 绿。
2. `go test ./internal/gateway/ ./internal/store/ ./internal/app/ -count=1` **0 红**（当前 2 红）。
3. 工作区 clean 之后再开始 phase 1；phase 1 的每个 Task 只 `git add` 自己涉及的文件。
4. `writeBufferedStream` 首字节前提交（PROBE：`firstWrite << total`），且 `canDefer` 两条分支行为一致。
5. 5 条转换腿全部覆盖 clean-EOF（当前缺 Chat→Responses）。
6. 计划内示例代码必须能整体编译：修正 `UnmarshalRules` 调用、补 `Engine.Rules()`（或改用 `SetRules` 后的本地缓存）。
7. 所有 VPS 验证命令在本机先跑通一次（`sqlite3` 不存在 ⇒ 换 `python3 -c`）。
8. 演练数据必须带 `access_source='monitor_probe'` 且在规则 SQL 中排除；不允许用 `path` 做隔离。
9. `monitor_events`/`monitor_alert_states` 的写入路径不得与请求热路径争用唯一写连接（采集读走 `reader()`）。
10. 生产部署来自 commit：`vcs.modified=false`，`git rev-parse HEAD` 与部署记录一致。
11. 能力函数与 `quotaForProvider` 分支同源（一个 switch 派生 `Capabilities.Quota`）。
12. 旧的 `/__alerts*`、`/__selfcheck*` 在阶段 1 保持可用，且旧 Telegram 推送只由一处发出（不双推）。

---

## 附注

本轮**未修改任何业务代码**，未提交，未部署。基线命令均在本机执行；VPS 只用只读命令（`systemctl show`、`ls -l`、`grep -a`、`command -v`），未触碰生产库，未启动服务。
