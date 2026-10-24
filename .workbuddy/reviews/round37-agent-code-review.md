# R37 · 对抗式代码审查

HEAD `50e77f3`｜工作区指纹：并发会话的 `web/src/monitoring/*` WIP（不归属本审查）｜本轮类型：**新增提交 + 新模块覆盖（M5/M6/M7）**

审查方式：M1–M7 各派独立子 agent（只读，证据在 `round37-evidence/m{1..7}/`），主 agent 逐条复核；子 agent 受 harness 限制无法写报告文件，其全文结论由主 agent 落盘并**复核后**采信。

---

## 一、结论先说

**能上线。** 本轮新发现并修复 9 个真实缺陷（全部带红绿测试），其中两个 P1 直接影响生产：

1. **P1｜`Proxy-Authorization` 头连同凭据原样落库**（`f4e8b4a`）：敏感键表写的是连字符形式，归一化后是下划线，永远对不上；且归一化没有统一小写。任何大小写混写的同名头都漏。**这是本轮唯一的安全级发现。**
2. **P1｜用量分析翻页静默漏数+重复**（`0ce41c9`）：超过 500 条时，第 2 页起同时递增 `Page` 又回填游标，SQL 先按游标过滤再 OFFSET，两者叠加跳过整页；`Total` 又被"剩余量"覆盖导致提前退出；游标用 `<=` 每页重复一行。实测 1200 条只回 1000 条。
3. P1｜闸门拒绝被当成上游故障（`9311b94`）：健康账号连拒 3 次被冷却 4 分钟。
4. P1｜组内排队等待只看本行（`9311b94`）。
5. P1｜Gemini 并行同名调用 tool id 冲突（`220b06f`）。
6. P1｜Claude→Chat `json_schema` 缺 `name`（`220b06f`）。
7. P2｜账号切换后旧账号的失败冷却落到新账号（`147293d`）。
8. P3｜WorkBuddy 过期且无可续期凭证时静默发死令牌（`da4bda3`）。
9. P3｜脱敏漏 Google `1//0…` 与 xAI `pac_…`（`16b0fdb`）。

**纠正 2 条误报**（复核不成立）：
- M4-1「负缓存冻结新账号」：实测原始顺序（先 bind 再问 needsRefresh）不冻结，bind 在账号变化时已删条目。
- M6-2「普通用户可占用 admin 身份」：用户 ID 生成显式排除 `admin`（`users_api.go:114`），用户名 `admin` 也被保留字拒绝；已加双防线测试锁定。

**已核实无问题**：M5 缓存 token 只计一次、未计价≠$0；M7 `LaneGroup` 与三个限额真实落库并能读回、`" ACCT "` 与 `"acct"` 运行时归并为同一账号；M6 CORS 管理端不回 `*`、伪造 `X-Forwarded-For` 拿不到管理员身份、登录限流在 bcrypt 前。

---

## 二、基线

| 项 | 结果 |
|---|---|
| `go build ./...` | 通过 |
| `go vet ./...` | 通过 |
| `go test ./internal/gateway/ ./internal/store/ ./internal/monitor/` | 全绿 |
| `internal/tunnel` | 5 项失败，本机缺 `/bin/sh`（Windows 环境问题） |
| `zz_*_test.go` 残留 | 0（子 agent 探针全部存证后删除） |
| `.bak` | 已清理 5 个未跟踪备份 |

---

## 三、新提交审查（D*）

| 编号 | 提交 | 内容 | 状态 |
|---|---|---|---|
| D37-1 | `9311b94` | 闸门拒绝不再冷却账号 + 组内排队取最长 | ✅ E3（两测试分别红） |
| D37-2 | `220b06f` | Gemini 同名调用 id + Claude→Chat 补 name | ✅ E3 |
| D37-3 | `147293d` | 账号切换后冷却不落新账号 | ✅ E3 |
| D37-4 | `da4bda3` | WorkBuddy 过期明确报错 | ✅ E3 |
| D37-5 | `f4e8b4a` | Proxy-Authorization 脱敏 + Provider 限额往返测试 | ✅ E1+E3 |
| D37-6 | `39a7b90` | 固化计费口径测试 | ✅ E1 |
| D37-7 | `0ce41c9` | 用量分析翻页三处修复 | ✅ E3（1200 行跨页） |
| D37-8 | `16b0fdb` | 补厂商私有 token 脱敏形状 | ✅ E3 |
| D37-9 | `50e77f3` | admin 身份双防线测试 | ✅ 判定不成立并锁定 |

## 四、工作区 WIP

| 编号 | 内容 | 归属 | 处理 |
|---|---|---|---|
| W37-1 | `web/src/monitoring/*.tsx` | 并发会话 | 未审查（M8，本轮未覆盖） |

## 五、生产只读核查

本轮未执行（时间盒优先保证代码级复核）。生产仍运行 `0c26215` 构建，健康检查与 UI 均 200。

## 六、全量清单

见 `round37-evidence/m{5,6,7}/report.md` 与 `round36-evidence/m{1..4}/`（历史项已在 r36 全量过账）。本轮新增 14 条 D37-* 已写入 `LEDGER.md`。

### 本轮仍未修复（已入账）

| 编号 | 问题 | 级别 | 状态 |
|---|---|---|---|
| D37-12 | 成本上限忽略未计价请求 | P3 | 🟡（文案已注明） |
| D37-13 | `schemaVersion` 只写不读，无降级检测 | P3 | 🟡 |
| D37-14 | 约 20 个管理端点无 handler 级 `requireAdmin` | P2 | 🟡 纵深防御（中间件已拦） |
| M5-3 | 上游报 cache>input 时聚合层二次补加 | P3 | 🟡 需上游异常 |
| M7-7 | `SaveCheckinHistory` 平行死代码 | P3 | 🟡 |
| M4-4 | 续期锁键语义不一致 | P3 | 🟡 不影响功能 |
| W29-2 | passthrough 日志副本 | P2 | ✅ 实测不影响首字节，降级观察 |

## 七、回退与假修复

- **回归**：无。9 个提交全部有红绿证据。
- **假修复**：无新增。r36 纠正的 7 条账本误报保持 ✅。

## 八、下轮验收门槛

1. [ ] D37-14：给 `/__app/logs`、`/__settings/*`、`/__providers/export|import`、`/__selfcheck*` 补 handler 级 `requireAdmin`，并有 role=user 403 测试。
2. [ ] M7-7：删除 `SaveCheckinHistory`/`ListCheckinHistory` 或接线到 `checkin.go`。
3. [ ] D37-13：`schemaVersion` 加入读取校验（未知高版本拒绝启动）。
4. [ ] M8 前端：派子 agent 用 `measure.cjs` 实测几何，覆盖 `web/src/monitoring/` 与最近新增的成本列/决策聚合/会话视图。
5. [ ] M9 隧道：只读审查 `internal/tunnel`，补齐 round36 未覆盖项。

## 附、覆盖度声明

| 模块 | 已扫文件 | 总数 | 未核查 |
|---|---|---|---|
| M1 请求转换 | 12 | 14 | 2（部分分支） |
| M2 流式 | 9 | 9 | 0 |
| M3 调度 | 8 | 8 | 0 |
| M4 OAuth/额度 | 16 | 18 | 2 |
| M5 计费/用量/日志 | 7 + 8 追加 | 7 核心 | magpie 对照、用量重建 |
| M6 安全/鉴权 | 9 | 9 | 生产多副本、真实 cloudflared Host |
| M7 存储/迁移 | 全部白名单 | — | `Model.MaxOutputTokens` |
| M8 前端 | 0 | — | **全部未核查** |
| M9 隧道 | 0 | — | **全部未核查** |

## 给修复 agent 的订单

1. **P2 · D37-14**：为约 20 个仅依赖中间件白名单的管理端点补 handler 级 `requireAdmin`，验收：role=user 直接调用这些端点全部 403。
2. **P2 · M8**：派子 agent 实测控制台几何与时区，重点看本轮新增的成本列（网格加了一列）、决策聚合面板、会话筛选按钮在窄屏下的布局。
3. **P3 · M7-7**：删除 `SaveCheckinHistory`/`ListCheckinHistory` 这对平行死代码，或把 `checkin.go` 接到它们上。
4. **P3 · D37-13**：`schemaVersion` 读取校验。
5. **覆盖**：M9 隧道模块只读审查。
