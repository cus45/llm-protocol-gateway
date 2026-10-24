# 第七轮 · 另一 Agent 改动代码审查（round7）

- 审查时间：2026-10-23 00:27–00:45
- 审查范围：`16c00d3`（HEAD，10-22 22:56）之后 **零新提交** + 1 处未提交改动（`internal/gateway/workbuddy.go`，审查期间被改了三次，快照 `00:31:33`，+3 行）
- 对照基准：`C:\guodevelop\demo\magpie`（协议语义、结构化输出形状、tool_choice/缓存断点写法）
- 基线：`go build ./...` 通过、`go vet ./internal/gateway/...` 无告警、`go test ./internal/gateway/...` 全绿（37.2s，855 个顶层用例 + 子用例，含未提交调试行）
- 工作区：`M internal/gateway/workbuddy.go`（+2 行：`os` import + 调试输出）；`stash@{0}` 为 20:52 的历史遗留
- 方法：先取 git 事实（提交/时间戳/stash），再逐一复核上轮高危及遗留项（带当前行号），最后针对「对方正在调试的 WorkBuddy 续期」做定点深挖

---

## 结论先说

1. **本轮没有可评审的提交**：HEAD 仍是 `16c00d3`，所以上轮判定的问题在已提交代码里**不可能被修**——事实也的确一项未动（下面逐项带行号复核，不靠推断）。
2. **唯一的新代码是两行调试输出，且正在泄漏凭据**：`workbuddy.go:658` 与 `:684` 把完整 `AccessToken` / `RefreshToken` 打进日志。对方在审查窗口内把它从「应用日志（入库 SQLite）」改成「stderr」——**方向对，但仍然是把生产令牌写进 journald/日志文件**，且第二处在刷新失败时打印的是旧令牌，容易误导读数。不能提交。P0。
3. **上轮 9 个高危全部仍在**（H-1…H-9），另加 M-1（Claude→Chat stop 覆盖无守卫）、R1（无限流）、S5（无空闲超时）、M-19（响应正文不脱敏）——一条都没修。
4. **本轮新增 3 条**：N-1 `workbuddy_ai` 凭证 `expires_at` 为空 → 永不续期且无 401 兜底（正好是调试行在查的方向，给出根因假设 + 验证命令）；N-2 一个 20:52 的 stash 是 `597e96a` 的旧副本；N-3 线上二进制 = `16c00d3` 已部署，但调试行未上线，生产日志**未被污染**。
5. 线上近 12h 无 WorkBuddy 续期/401 异常（journalctl 计数 0），说明这行调试属**探索性排查**而非救火，可以按正常节奏删掉、不要在主线追。

---

## 一、未提交改动逐行评审（唯一的新代码）

审查期间对方仍在实时编辑，同一函数被改了三次；以 `00:31:33` 快照为准（+3 行）：

| 版本 | 位置 | 形态 | 问题 |
|---|---|---|---|
| 00:27 版 | `workbuddy.go:657` | `s.logs.AddApp("warn","DBG ensureFresh", … at=%q rt=%q …)` | **最坏**：`warn` 级会进控制台「应用日志」并**持久化到 SQLite**，等于把 access/refresh token 写进数据库与所有备份 |
| 00:30 版 | `workbuddy.go:658` | 同上改为 `fmt.Fprintf(os.Stderr, …)` | 不进 DB 了 ✅，但 stderr 在 VPS 上由 systemd 收进 journald（`journalctl` 可读、轮转留存），且**每个转发请求**都打一次 → 令牌明文 + 日志洪泛 |
| 00:31 版（当前） | `+ workbuddy.go:684` | 新增 `fmt.Fprintf(os.Stderr, "DBG refresh endpoint=… err=… access=%q refresh=%q exp=%q")` | 同一泄漏问题 ×2；另外**这一行有诊断陷阱**：`refreshWorkBuddyCredential` 出错时返回的 `refreshed` 就是**入参旧凭证**，所以失败场景打印出来「access/refresh 没变」，很容易被误读成「服务端没轮换」，而真正原因是请求根本没成功 |

结论：**半成品，不可提交**（`go build`/`go test` 能过，正因为能过才更容易被误提交）。

- 正确做法：调试期用 `//go:build debugworkbuddy` 临时文件或本地临时分支；确认根因后整段删除。当前最小动作：`git checkout -- internal/gateway/workbuddy.go`（其余工作区干净，不会连带丢别人的改动）。
- 顺带核对：全仓只有这一处新增凭据泄漏（`grep -rn "logs.AddApp\|log.Printf" | grep -i "token\|key\|secret"` 其余命中都是「不含值」的标题式日志，合规）。

---

## 二、上轮遗留复检（逐项，行号为当前代码）

| # | 上轮遗留 | 状态 | 本轮核对证据 |
|---|---|---|---|
| H-1 | `output_config.format` 带 `name` → Claude 400 | ❌ 未修 | `convert_response_format.go:40-44` 仍写 `format["name"]`；同仓 `convert_responses_fields.go` 的既有约束未被复用 |
| H-2 | `json_object` 原样发 Claude | ❌ 未修 | `convert_response_format.go:19-20` 直接返回 `{"type":"json_object"}`，未降级宽松 schema |
| H-3 | adaptive 分支整体替换 `output_config` | ❌ 未修 | `convert_claude_openai.go:322-324` `applyAdaptiveThinking` 仍整体赋值 `{"effort":…}`，format 静默丢失 |
| H-4 | `allowed_tools` 塞进 Claude `tool_choice` | ❌ 未修 | `convert_responses_claude.go:497-509` 仍在写 `"allowed_tools"` |
| H-5 | 缓存断点白名单不含 `tool_result` | ❌ 未修 | `convert_claude_cache_marks.go:93-105` `claudeCacheableBlock` 只认 `text/image/document` |
| H-6 | 思考余量补正生产不可达 | ❌ 未修 | `server.go:5440` 仍是 `rewriteClaudeUpstreamMaxTokens` 先行，`claude_pass_through_normalize.go:161` 的 shortBudget 判定永远为假 |
| H-7 | 非 5.2/5.3 的 `glm-*` 回落到 `none` | ❌ 未修 | `claude_thinking_off.go:238-239` 仍 `return "none"`（与 460dcec 自述「GLM 不收 none」自相矛盾） |
| H-8 | `known==false` 被当非 Claude 删 thinking | ❌ 未修 | `claude_thinking_off.go:266-276` `if !knownClaude { removeClaudeThinkingOff }`，`claude-3-7-sonnet-20250219` 等仍中招 |
| H-9 | 出图提前拒绝查 `route.ProviderID` | ❌ 未修 | `server.go:4204` 用 `route.ProviderID` 判定并回显，`:4216` 才用 `decision.ProviderID` 取 provider——判定与执行对象仍错位 |
| M-1 | Claude→Chat stop 覆盖无守卫 | ❌ 未修 | 非流式 `convert_claude_openai.go:481-485`、流式 `…_stream.go:234-238` 仍无条件 `stopReason="stop"` |
| M-19 | 响应正文/错误描述不脱敏 | ❌ 未修 | `server.go:1394-1396` `ResponseBody` / `ErrorDescription` 直接 `truncateForLog`，只有请求侧走了 `scrubSensitiveRequestBody`（`:1385`） |
| R1 | 无 RPM 限流 | ❌ 未修 | `grep -rln "rate.Limiter\|x/time/rate\|limiter" internal/` 无命中 |
| S5 | 上游返回头后无空闲超时 | ❌ 未修 | `protocol_proxy.go:143,447` 仍 `&http.Client{Timeout: 0}` |
| 假测试 | `TestClaudeToolChoiceWireShape` 自证式 | ❌ 未删 | `convert_tool_restrictions_test.go:127` 仍在（不调用任何生产代码） |
| 已修项回归 | stop→Responses / P1 流式 usage | ✅ 无回归 | `convert_claude_openai_stream.go:120-129` 仍取 message_start usage；Responses 编码器内无 `stop`（`convert_openai_responses_chat.go:757` 的 `chatReq["stop"]` 是 Chat 请求方向，合法） |

---

## 三、本轮新增发现

### N-1（中高）`workbuddy_ai` 令牌无过期时间 → 永不续期，且没有 401 兜底

- 证据链：
  1. 本地库 `providers` 里 `workbuddy-ai-订阅`（`auth_type=workbuddy_ai`）的 `oauth_expires_at` 是**空串**，而同库 `workbuddy-订阅` 是 `2026-11-11T05:26:10Z`；
  2. `workBuddyTokenNeedsRefresh`（`workbuddy.go:608-620`）对空串 `return false`（注释「信任令牌直到被拒」）；
  3. `ensureFreshWorkBuddyToken` 在 `!needsRefresh` 时直接返回（`:659-661`），refresh token 为空时也静默放行（`:663`）；
  4. 转发主路径**没有** 401→强制续期→重放的逻辑：全仓只有 `workbuddy_checkin.go:137` 把 401 当自身轮询的可重试码，`protocol_proxy.go` / `provider_chat.go` 均无。
- 后果：intl 站若 token 端点不回 `expiresAt`/`expiresIn`（`toCredential` 只认这两个 int 字段，`:331-336`），令牌过期后**每次请求 401 直到人工重新登录**，而日志里只会看到普通上游 401，看不出「该续期没续」。
- 建议（选一，倾向后者）：
  - 空 exp 且有 refreshToken 时按保守 TTL（例如 30 分钟）续期一次，用续期返回的 exp 校准；
  - 上游 401 且存在 refreshToken 时，强制走一次 `refreshWorkBuddyCredential` 并重放一次请求（这也是 A1 锁真正发挥作用的场景）。
- 验证命令（VPS 无 sqlite3，需 `apt install sqlite3` 或用控制台接口）：`select id,auth_type,oauth_expires_at from providers where auth_type like 'workbuddy%';` —— 若线上 intl 账号同样为空，即坐实。
- 这条正好解释对方在 `ensureFresh` 里打 `exp=%q needs=%v` 的动机；建议先做这一步定性，再决定改续期策略，别先提交调试行。

### N-2（低，卫生）`stash@{0}` 是 `597e96a` 的过期副本

- 内容：`convert_claude_responses_stream.go`（+87/-29）+ `convert_tool_restrictions_test.go`（+27），时间 20:52，名称 `review-baseline-5418`。
- 该逻辑已由 `597e96a`（21:08）以更完整的形态提交进 HEAD；stash 里还带着那个假测试。属上轮为跑「干净基线」而暂存、忘记恢复的残留。
- 建议：确认 `597e96a` 覆盖后 `git stash drop`，避免以后 `git stash pop` 把旧实现复活。

### N-3（信息）生产未被污染，部署与提交同步

- VPS `systemctl is-active` = active；`/opt/llm-protocol-gateway/bin/gateway` mtime = 10-22 22:56（服务器 PDT），换算北京时间 10-22 22:56 = `16c00d3` 提交时刻 → **线上跑的是最新提交**，调试行尚未构建上线（该文件 00:27 才被改）。
- 近 12h `journalctl` 中匹配 `workbuddy.*(fail|401|expired)` 的行数 = **0**。（注意：应用级日志走 SQLite `logs.AddApp`，不进 journal，故此项仅作旁证。）

---

## 四、原始差距清单总账（60 项，本轮更新）

| 项 | 状态 | 说明 |
|---|---|---|
| P1 流式 usage=0 | ✅ | 无回归 |
| P2 stop_reason 映射 | ✅ | 映射表 + 测试 |
| P3 Responses→Chat 正文清空 | ✅ | a55c35d |
| P4 流式 reasoning 丢弃 | ✅ | reasoning_content delta 已发 |
| P5 忽略 incomplete | ✅ | 1ad2f50 |
| P6/P7/P8 | ✅ | 第 1 轮修，无回归 |
| P9 redacted_thinking / 迟到 reasoning | ⚠️ | 三轮未动 |
| P10 字段转发 | ◐ | stop_sequences✅(本地回填) prompt_cache_key✅ 缓存断点◐(H-5/M-12) 结构化◐(H-1/2/3) top_k/metadata 未确认 |
| P11 单一 IR | ❌ | 长期项，两手中转架构未动 |
| R1 RPM 限流 | ❌ | 零实现 |
| R2 冷却不看 Retry-After/resets_at | ❌ | 2min/1min 两档硬编码未动 |
| R3 billing 未分类 | ❌ | |
| R4 流中断不续传 | ❌ | |
| R5 粘性不持久化 | ❌ | |
| R6 闸门非每账号 | ❌ | |
| S1 CORS | ✅ | 残留 trae.go:383 临时回调 |
| S2 body 上限 | ✅ | 413 仅覆盖 4 端点（M-21） |
| S3 认证限流/setup | ◐ | M-8 死代码、M-15 TOCTOU 仍在 |
| S4 正文脱敏 | ◐ | 请求侧✅ 响应侧❌（M-19，本轮复核仍在） |
| S5 空闲超时 | ❌ | `Timeout: 0` 两处 |
| A1 续期锁 | ✅ | M-16/17/18 三瑕疵仍在 |
| A2 主机名匹配 | ✅ | M-22 明文 scheme 仍在 |
| A3 images 串 Key | ✅ | H-9 判定对象错位仍在 |
| A4 Claude 额度窗口/429 负缓存 | ❌ | |
| A5 Cursor 提前量 | ◐ | 读侧仍无提前量 + fail-open |
| A6（新）WorkBuddy 空 exp 死锁 | ❌ | N-1，本轮新增 |
| 控制台 1–6（实时流/路由解释/会话/重放/成本列/告警角标） | ❌ | `web/` 本轮仍零改动 |

---

## 五、下一批建议（按价值，前 3 条与上轮一致但更紧迫）

1. **删掉 `workbuddy.go` 调试行**（`git checkout -- internal/gateway/workbuddy.go`），任何提交前必做。
2. **修 H-1/H-2/H-3**：照抄本仓 `convert_responses_fields.go` 的既有范式（不带 name、json_object 降级、merge-after-thinking），一个提交可全修，附带能命中的组合测试。
3. **修 H-7/H-8**：撤 `glm-*` 的 `none` 回退；`known==false` 改回无条件透传（对齐 magpie `offUnknown`）。
4. **定性并修 N-1**：先查线上 `oauth_expires_at`，再补「401→强制续期→重放 1 次」，这条比 H-6 更贴近真实可用性。
5. **修 H-9 + M-1**：判定对象改 `decision.ProviderID`；Claude→Chat 双路径加与 Responses 一致的 stop 守卫，删 9b62841 的无效测试。
6. **调度批 R1–R3** 仍是最大整块缺口，建议排进下一个独立批次（限流器 → 冷却阶梯 → 错误分类），别再被协议细节无限推后。
7. 卫生项：删假测试、`git stash drop`、把调试代码迁到 build-tag 文件，避免下次审查把它当成「待评审改动」。

---

*本轮未修改任何生产代码；仅审查、运行构建/测试、读取只读数据（本地 SQLite + VPS journal/文件时间戳）并撰写本报告。*
