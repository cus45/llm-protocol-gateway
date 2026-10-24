# round33 对抗性代码审查报告

> HEAD `2045543` ｜ `git status --porcelain` **0 行**（工作区干净）｜ `git diff` 空 ｜ `git diff --cached` 空
> 内容指纹：`git diff | sha256sum` = `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`
> 　　　　　`git diff --cached | sha256sum` = `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`
> 增量口径：**请求增量**（含新提交 / 仅 WIP 变化 / 无增量）→ 本轮判定 **无增量（第三类）**
> 轮次：**round33**（`.workbuddy/reviews/` 下最大 N=32，+1）；审查时间 2026-10-24 05:25–06:00 (+0800)
> 只读约束：未改任何业务代码，未 commit / push / pull / rebase / reset，未重启或写生产库

---

## 0 审查对象与增量判定

| 项 | 值 |
|---|---|
| `git rev-parse --short HEAD` | `2045543`（`fix(stream): 截断收尾改用协议错误事件并对齐 magpie`） |
| `git status --porcelain` | **0 行**（改 0 / 增 0 / 删 0） |
| `git diff --stat` | 空（+0 / −0） |
| 指纹 | `e3b0c442…b855` + `e3b0c442…b855`（空输入哈希 ×2） |
| 与 round32 差异 | **逐字节一致**。HEAD 相同、双哈希相同、无新提交、无 WIP |
| 与 round31 差异 | HEAD 相同（`2045543` 自 r31 起未变，已连续第 3 轮） |
| 临时 worktree | 无（`git worktree list` 仅主树） |

**判定：无增量。** 本轮三项口径全部走"继承"路径：

1. 全量清单照常出（第六节 **97 条**，账本 96 条 + 本轮补记的控制台合并项 C1）；
2. 允许复用的证据一律写成「继承（代码哈希未变，roundNN 第 E* 号证据）」；
3. **所有未闭环 P0/P1 全部重跑一次验证**。本轮实际重跑：
   - **W29-1（唯一 P0）：新写 PROBE-33 探针本地 A/B 实跑（E1）**，不是读代码；
   - H-1/H-2/H-3/H-5/H-7/H-8（P1）：决定性行逐条重看；
   - D29-2/D29-3/D29-4/D29-6（P1）：lane 与 Retry-After 出口重看 + 重 grep 全部调用点；
   - D31-1/D31-4（P1）：判据与 cache key 重看；
   - D32-1/D32-4（P1）：重 grep 调用点 / 重看写入行；
   - 生产只读：二进制 sha256 + vcs 串 + 特征串 + DB 结构与计数 + 24h 流量分布（全部重跑，读数与 r32 完全一致）。

**本轮另补 round32 因时间盒挂"未复核"的 4 条（D32-5/D32-6/D32-7/D32-8）+ 长期挂"未核查"的 H-8**，结论见第三节与第六节。

---

## 一 结论先说

**不能上线。** 这是"零增量"的第二个连续轮次：代码一行没动，唯一 P0 已连续第 13 轮未修，生产上跑的仍不是任何一个 commit。

1. **P0 W29-1（透传流全量缓冲）连续第 13 轮未修，本轮用新探针再证一次。** PROBE-33 在同一 HEAD 上跑三个 400 ms 分块：裸 writer 首字节 **400.6 ms**、`writes=3`；换成 `newFailoverResponseWriter` 后，`canDefer=true` 首字节 **1.2018 s = 总时长**、`writes=1`，`canDefer=false` 同样 **1.2011 s**、`writes=1`。成因没变：`passthrough_stream.go:278` 的拦截条件里没有 `canDefer`，`writeBufferedStream` 全程不 `WriteHeader`，`failoverResponseWriter.Write` 在提交前只落 `buf`、`Flush` 是空操作。
2. **生产仍在跑"脏树 + 非 HEAD"。** 二进制 sha256 `9a0d86d6…c31f`、mtime `2026-10-23 21:32 +0800`、`vcs.revision=d93ec02` 且 **`vcs.modified=true`（连续第 12 轮）**；`lane_group` 命中 0、`monitoring snapshot failed` 命中 0 ⇒ 既缺 `2045543` 的 R4 修复、也缺 `lane_group` 落库；却含仅工作区存在的 `without its closing frame`(1) 与 `idleGuardedBody`(3)。**没有任何 commit 能复现线上行为。**
3. **线上旁证与 W29-1 一致（E4，不作判据）。** 24h `pass_through` 1163+80 条里 **99.4%（1247/1255）的 `latency − ttft ≤ 5 ms`**，而 `convert` 9467 条平均 1153.6 ms。透传流首字节等于末字节。库上无 `stream` 列，非流式也会得到 gap=0，所以这条只作旁证。
4. **P1 H-1/H-2 仍是"同语义两套实现互相打脸"。** `convert_response_format.go:18-20` 把 `json_object` 原样发给 Claude、`:40-43` 写 `format.name`；同仓 `convert_responses_fields.go:71-72` 的注释白纸黑字写「Claude GA structured outputs reject format.name ("Extra inputs are not permitted")」，`:73-80` 还把 `json_object` 降级成宽松 `json_schema`。错在 Chat 侧就是硬 400。
5. **P1 D32-1 复核成立且很容易被误当已修。** `chatStopToClaudeSequences` 有完整实现（`convert_tools.go:1165-1176`），但 `grep -rn` 全仓**只有定义行、0 调用点**；`openAIChatToClaudeRequest` 从不读 `stop` ⇒ Claude 客户端设了停止序列也无效，一路生成到 `max_tokens`。
6. **P1 新增确认（上轮未复核）：隧道 token 明文落盘。** `tunnel/manager.go:300-301` 把 `strings.Join(args," ")` 写进 `cloudflared.log`，而 args 含 `--token <token>`；`:343` 以 `0o644` 建文件 ⇒ 同机任意用户可读并接管公网域名。
7. **P1 D31-1 是"注释与实现相反"的教科书案例。** `quota_keeper.go:481-485` 注释写「a person's deliberate pause on a healthy task is never undone here」，`:486` 的实际判据是 `len(live)==0` 才 return false ⇒ 只要有可用 Provider 就复活，而人工暂停的恰恰是健康任务。
8. **本轮唯一正向变化：证据面扩大，缺陷面零变化。** 补核 5 条（D32-5/6/7/8、H-8）全部**成立**，无一被推翻；`H-8` 从未核查转 ❌——`user_isolation.go:123-125` 对 `OwnerUserID==""` 的管理员 Key 直接 `return chain`，兜底链可指向任意 Provider（含 disabled / 回收站 / 协议不匹配）。

> ⚪ 占比：97 条中 ⚪ 仅 1 条（N12-2，已举证），**1.0%**，不存在拿"不适用"当逃生口。

---

## 二 基线（build / vet / test / 卫生）

| 项 | 结果 |
|---|---|
| `go build ./...` | **通过**（rc=0，无输出） |
| `go vet ./...` | **通过**（rc=0，无输出） |
| `go test ./internal/gateway/ ./internal/store/ ./internal/app/ ./internal/monitoring/ -count=1` | **全绿**：gateway `42.236s`、store `1.388s`、app `1.507s`、monitoring `0.042s` |
| 工作区卫生 | `git status --porcelain` 0 行；`.bak` 0 个；热路径 `println` 0 处 |
| 生产源码里的测试钩子 | `internal/gateway/zz_quota_keeper_e2e_hooks.go` **仍在且已入仓**（W30-1，第 3 轮挂账） |
| 提交卫生 | `.superpowers/` 5 个文件仍被跟踪（含 `.server.pid`/`.server-stopped`）（D31-7） |
| 前端类型/构建 | 继承 r31 的 `tsc -b` 0 error（零增量，不重复跑） |
| `gofmt` | 不跑：CRLF 检出会产出假阳性，且只读约束下无法 `-w` 回写 |

> 基线全绿这件事本身要说清楚：**测试绿 ≠ 修好了**。W29-1 在测试全绿的情况下连续 13 轮存在，原因就是**没有任何一条测试断言"首字节时延"**（见第八节门槛 2）。

---

## 三 新提交审查

**本轮无新提交。** `2045543` 已在 r31 审完（R4 闭环、删除 `interceptingStreamWriter`、收尾改协议错误帧对齐 magpie `gateway.go:4940 streamFailure`），本轮复核结论不变。

问题表：无新增 D* 行。既有 D29-*/D31-*/D32-* 见第六节。

### 3.1 本轮补核的四条（上轮时间盒未复核，全部成立）

| 编号 | 补核结论 | 决定性证据 |
|---|---|---|
| D32-5（P2） | **成立** | `server.go:5755` 出图返回 `writePassThroughResponse(..., false, usageStreamUnfoldable, func([]byte) TokenUsage { return TokenUsage{} })` ⇒ usage 恒空；`grep -in image pricing.go` **0 命中**（无图像价格行）。下游 `server.go:1367-1369` 必然回落 `EstimateTokenUsage`（`len/4`）。 |
| D32-6（P2） | **成立** | `grep -rn safety_identifier internal/ web/src` **0 命中**。Responses 的 `user` 已 deprecated，新字段整条静默丢失。 |
| D32-7（P3） | **成立** | `grep -rn resumeTasksForProvider internal/` → 只有定义 `quota_keeper.go:510` 与测试 `quota_keeper_test.go:765/777`，**无生产调用点**。注释宣称的"Provider 出回收站时恢复任务"不存在。 |
| D32-8（P3） | **成立** | `workbuddy.go:861-871` 的 `case string:` 分支在文本被清空后直接 `continue`（整条 assistant 轮丢弃）；`:878-887` 的 `case []any:` 分支有 `if _, hasCalls := message["tool_calls"]; hasCalls` 例外。两分支语义不一致。 |

### 3.2 H-8 从未核查转确认（连续 3 轮挂"未核查"）

| 编号 | 结论 | 证据 |
|---|---|---|
| H-8（P1） | **❌ 成立** | `user_isolation.go:122-124`：`if strings.TrimSpace(key.OwnerUserID) == "" { return chain }` —— 管理员 Key 直接返回原链，不经过 `allowedProviderIDsForUser` 过滤。调用点 `provider_failover.go:1094/1492` 因此对管理员恒等于 no-op。后果：管理员 Key 的 `fallback_provider_ids` 可指向 disabled、已进回收站、或协议不匹配的 Provider，链路走到即报错/超时，而控制台不会有任何提示。 |

---

## 四 工作区 WIP 审查

**无 WIP。** `git diff` 为空哈希，`git status --porcelain` 0 行，`find internal web -newermt "2026-10-24 05:00"` 无命中 ⇒ 相对 r32 进展为 0，作者自 r31 提交后近 25 小时未再动代码。

问题表：无新增 W* 行。历史 W29-1/W29-2/W29-3/W29-6/W29-7/W29-9/W29-11/W30-1/W30-2/W30-3 见第六节。

> 说明：本轮我曾创建一次性探针 `internal/gateway/zz_probe33_test.go`，源码已复制进 `round33-evidence/zz_probe33_test.go.txt`，运行后立即删除，`git status --porcelain` 回到 0 行（见收尾自检）。

---

## 五 生产环境只读核查（173.249.207.202）

只用了 `systemctl is-active` / `systemctl show`、`ls -l`、`sha256sum`、`grep -a -c`、`python3 sqlite3`（`file:…?mode=ro`）。**未 restart、未写库、未触发上游计费请求、未运行 `gateway version`**（该子命令会起服务，属已知坑 N13-1）。

| 项 | 读数 | 与 r32 对比 |
|---|---|---|
| 服务 | `active`，MainPID `911205` | 同 |
| 二进制 | `/opt/llm-protocol-gateway/bin/gateway`，16498850 B，mtime `2026-10-23 06:32 PDT` = `21:32 +0800`，sha256 `9a0d86d6…c31f` | **完全同** |
| 版本归属 | `vcs.revision=d93ec02…`，**`vcs.modified=true`（连续第 12 轮）** | 同 |
| 特征串 | `without its closing frame`=1、`idleGuardedBody`=3（**仅工作区存在**）；`lane_group`=0、`monitoring snapshot failed`=0 | 同 |
| 库 | `gateway.db` 2412736512 B（2.41 GB），mtime `2026-10-23 12:32` | 同 |
| Provider | 50 行，`deleted=0` 的 **20 行**；`api_keys` 9 | 同 |
| lane 配置 | `providers` 表**无 `lane_group` 列**；`max_concurrent_requests>0` 仅 1 行（`DeepSeek-gq-api` = 1 / rpm 0）；`requests_per_minute>0` **0 行** | 同 |
| monitor 表 | 6 张都在；`monitor_events`/`monitor_alert_states`/`monitor_event_pushes`/`monitor_baselines`/`provider_metric_samples` 全 **0 行**；`checkin_history` 18 行 | 同 |
| 24h 流量 | convert（OpenAI Chat→Claude）9467 条，avg `latency−ttft` **1153.6 ms**；pass_through（Claude→Claude）1163 条 **0.7 ms**；pass_through 全体 gap≤5ms 占 **1247/1255 = 99.4%** | 量级同 |

**三条硬事实给运维：**
1. 生产二进制**不含 `2045543`**（无 `lane_group`/monitoring 串却含 WIP 串）⇒ R4 的协议错误帧修复**未上线**，线上仍是"截断当正常结束"的旧行为；
2. 生产**已生效** per-provider 并发闸门（`DeepSeek-gq-api` max=1）⇒ lane 代码在跑，但**无法配组**（无 `lane_group` 列），D29-3/4/6 处于"潜伏"状态；
3. `vcs.modified=true` 意味着**回滚只能重编**，无法用 commit 追溯当前线上缺陷集合。这一条不解决，前 96 项修了也无法证明生效。

---

## 六 完整排查清单（账本全量 96 条 + 补记 1 条 = 97）

图例：✅ 已修复（满足第四节门槛 E1/E2/E3）｜🟡 部分修复｜❌ 未修复（含假修复）｜⚪ 不适用｜标记：`回归` `新增` `未核查`

### M0 基线与卫生

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| N13-1 | `gateway version` 零 flag 解析会真起服务并写 WAL | P2 | ❌ | 继承（r13 复现；r27 远程误触复证） | 无 |
| W30-1 | `zz_quota_keeper_e2e_hooks.go` 生产源码里的测试钩子（导出伪造 admin） | P3 | ❌ | `git ls-files` 命中；`internal/gateway/zz_quota_keeper_e2e_hooks.go:15-26` | 第 3 轮挂账 |
| D31-7 | 提交卫生：`.superpowers/`（含 `.server.pid`）+ 3.9k 行文档 + 128 文件单提交 | P3 | ❌ | `git ls-files` 5 个 `.superpowers/**` 仍在 | 无 |

### M1 协议转换·请求方向

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| P1 | Claude→Chat 流式丢 prompt_tokens | P1 | ✅ | 继承（r29 usage 折叠，代码哈希未变） | 无 |
| P2 | stop_reason 映射缺项 | P1 | ✅ | 继承（r29 映射表） | 无 |
| P3 | Chat→Responses 有 tool_calls 清空正文 | P1 | ✅ | 继承（r29 测试） | 无 |
| P4 | Responses→Chat 丢 reasoning 增量 | P1 | ✅ | 继承（r29 测试） | 无 |
| P5 | Responses→Chat 忽略 incomplete | P2 | ✅ | 非流 `convert_openai_responses_chat.go:870-872`；流 `…_stream.go:661-666`→`:525-536`；`stop_reason_mapping_test.go:90-136` 可红 | 继承（r32 闭合） |
| P6 | Chat→Claude block index 错乱 | P1 | ✅ | 继承（r29 测试） | 无 |
| P7 | sanitizeAnthropicToolUseID 碰撞 | P1 | ✅ | 继承（r29 测试） | 无 |
| P8 | Responses→Claude 缺合成 tool_result | P1 | ✅ | 继承（r29 测试） | 无 |
| P9 | redacted_thinking 丢失/占位非回放 | P2 | 🟡 | 已通 `convert_claude_responses.go:205`、`convert_responses_claude.go:24-78/241`；仍丢 `convert_openai_claude.go:73-75/139-141`、`convert_responses_claude.go:380-394` | 无 |
| P10 | strict/top_k/stop_sequences 等不转发 | P2 | 🟡 | strict 已通；stop 3/4 方向通，Chat→Claude 缺（=D32-1）；top_k 仅 `convert_responses_claude.go:484` | 无 |
| P11 | 两两手写转换 vs magpie 单一 IR | P2 | ❌ | 18 个非测试 `convert_*.go`，无 `ir.go`；magpie `ir.go:154` + `buildAnthropic:839`/`buildChat:268`/`buildResponses:1089` | 架构未动 |
| H-1 | `output_config.format.name` 该不该发（同仓两套实现矛盾） | P1 | ❌ | `convert_response_format.go:40-43` 写 name；反证 `convert_responses_fields.go:71-72` 注释明写 Claude 拒收 | **重看确认（E2）** |
| H-2 | `response_format=json_object` 原样直传 Claude | P1 | ❌ | `convert_response_format.go:18-20` 直出；`convert_responses_fields.go:73-80` 降级宽松 json_schema | **重看确认（E2）** |
| H-3 | thinking 覆盖 `output_config`（format 被抹） | P1 | ❌ | `convert_claude_openai.go:322-325` 整块赋值、`:394-397`；顺序 `:207-209` 写 format → `:213` thinking | **重看确认（E2）** |
| H-4 | Responses `allowed_tools` 塞进 Claude `tool_choice` | P1 | 🟡 | `convert_responses_claude.go:496-516`；magpie `responses.go:502-533` 从不下发该键。缺上游 schema 实证 | 无 |
| H-5 | cache marks 白名单漏 `tool_result` | P1 | ❌ | `convert_claude_cache_marks.go:92-103` 只认 text/image/document；magpie 用黑名单 `anthropic.go:921-928` | **重看确认（E2）** |
| H-6 | thinking 五连（预算补正 / glm none / unknown 模型） | P1 | 🟡 | `claude_pass_through_normalize.go:152-170` 无条件改写 `max_tokens`（本轮重看确认）；另三子项仍未逐条核 | 无（未核查子项保留） |
| D32-1 | Chat→Claude 完全丢 `stop`（映射函数是死代码） | P1 | ❌ | `convert_tools.go:1165-1176` 仅定义；`grep -rn chatStopToClaudeSequences internal/` **0 调用点**；`convert_claude_openai.go:177-218` 不读 stop | **重 grep 确认** |
| D32-2 | Claude→OpenAI 回复路径不回填 declared 工具名 | P2 | ❌ | `convert_tools.go:1272-1311` 不调 `:1337 restoreDeclaredToolName` | 继承 |
| D32-3 | thinking 预算与 `max_tokens` 无钳制 | P2 | ❌ | `convert_claude_openai.go:325-331` vs `:196`；magpie `anthropic.go:968` 有 `min(budget, max-1024)` | 继承 |
| D32-6 | `safety_identifier` 全仓 0 命中 | P2 | ❌ 新增 | `grep -rn safety_identifier internal/ web/src` = 0 | **本轮补核确认** |
| D32-8 | `unflagWorkBuddyMessages` 的 string 分支丢带 `tool_calls` 的 assistant 轮 | P3 | ❌ | `workbuddy.go:861-871`（清空即 `continue`）vs `:878-887`（有 `hasCalls` 例外） | **本轮补核确认** |
| M-1 | Claude→Chat 无条件 stop | P1 | ✅ | 继承（r10 纠错：已有条件守卫） | 已关闭，勿再报（保留在册） |

### M2 协议转换·流式

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| R4 | 流截断不能续/被当正常结束 | P0 | ✅ | 继承（r31：6 处读后判定 + `stream_interruption_close.go:38/94/157`，对齐 magpie `gateway.go:4940`；`convert_clean_eof_test.go` 绿） | **未上线**（生产无此代码） |
| N10-1 | 透传 >256KB 流被判截断（头部判据） | P0 | ✅ | 继承（r29 `usage.closed()` 全字节） | 无 |
| N10-2 | 中断收尾帧写裸 `w` 绕过锁 | P1 | ✅ | 继承（r29 改走 hw） | 无 |
| N11-2 | 心跳 stop 不 join | P1 | ✅ | 继承（r29 有界 join 2s） | 无 |
| N11-3 | 透传 Claude 收尾漏 content_block_stop | P2 | ✅ | 继承（r29 openBlocks 逐个 close） | 无 |
| N11-4 | 透传 Responses 收尾帧缺 id/model/seq | P2 | ✅ | 继承（r29 responsesStreamIdentity） | 无 |
| N14-2 | 转换路径中断判定恒 false | P0 | ✅ | 继承（r29 读后判定；r31 六处全改） | 无 |
| N23-1 | `canDefer` 在流式路径是死变量 | P1 | ❌ | **PROBE-33 实测：canDefer=true/false 首字节均 1.20s、writes 均 1** | 并入 W29-1 |
| N23-2/3 | 两套完成判定结论相反 | P2 | 🟡 | 继承（r29 部分统一） | 无 |
| N24-3 | `streamEndReader` 只携带 4 字节 | P2 | ✅ | 继承（r29 按 `maxClosingMarkerLen-1`） | 无 |
| N24-5 | 两套完成判定无回声守卫 | P3 | 🟡 | 继承（回声守卫仅末 64B） | 无 |
| N25-1 | 中断谓词语义反了 | P1 | ✅ | 继承（r29 `isUpstreamStreamInterruption`） | 无 |
| N25-3 | `scanResponses` 非 Responses 流全量累积 | P2 | 🟡 | 继承（r30 未复跑） | 未核查 |
| N25-5 | 缓冲路径零心跳 | P2 | ❌ | 并入 W29-3（`fw.Flush` 在未提交时是空操作） | 无 |
| W29-1 | **透传流全量缓冲（TTFT=总时长）** | **P0** | ❌ | `passthrough_stream.go:278-283` 无条件缓冲、`:539` 函数体无 `WriteHeader`、`provider_failover.go:347-357` 只落 buf、`:368-373` Flush 空操作；**PROBE-33（E1）首字节 1.2018s=total、writes=1 vs 裸 writer 400.6ms/3 writes** | **连续第 13 轮（E1 重跑）** |
| W29-2 | 透传下响应再复制进 `fw.body`（无上限） | P1 | ❌ | 同 W29-1 未提交路径下 `body` 累积；`Prefix/StreamLen/streamClosed` 零或仅测试调用 | 未修 |
| W29-3 | 心跳进缓冲区，流中收不到 ping | P1 | ❌ | 同 W29-1：`hw→fw.Flush` 在未提交时是空操作 | 未修 |
| W29-4 | 红测试 `TestConverterStreamWriterKeepsTheStatusItWasGiven` | P1 | ✅ | 继承（r30：该测试本机绿） | 已清（保留在册） |
| W29-6 | `scanResponses` 非 Responses 流全量累积 | P2 | 🟡 | 继承（r30 未复跑） | 未核查 |
| W29-7 | 回声守卫只看末 64B | P2 | 🟡 | 继承（r29 代码） | 无 |
| W29-9 | `streamUsageFolder` reflect 分派脆 | P3 | ❌ | 继承（r29 代码） | 无 |
| W29-11 | `failoverResponseWriter` 死代码（StreamLen/streamClosed/recordsForRetry） | P3 | 🟡 | `StreamLen()`/`streamClosed()` 全仓零调用；`Prefix()` 仅测试调用 | 无 |

### M3 调度 / 路由 / 并发 / 故障转移

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| R1 | 无 RPM 限流 | P1 | ✅ | 继承（r21 `8b5542d` 滑动窗口） | 无 |
| R2 | 冷却不看上游 Retry-After | P1 | ✅ | 继承（r26 阶梯可达，窗口 10min） | 无 |
| R3 | billing 归入 hardQuota | P1 | ✅ | 继承（r8） | 无 |
| R5 | 会话粘性不落库 | P1 | ✅ | 继承（r24 `session_pins`） | 无 |
| R6 | 并发闸门非 FIFO | P1 | 🟡 | 票号队列已入（0a4bf87），但新到未入队请求仍跳过 turn 校验（`provider_lanes.go:195`） | 无 |
| D29-1 | `laneGroup` 完全不落库 | P0 | ✅ | 继承（r30 `sqlite.go:239` 列 + `:641/:766` 读写 + 回环测试绿） | 无（但生产无此列） |
| D29-2 | Retry-After 出口按行号查分组 | P1 | 🟡 | `protocol_proxy.go:844` 已传 provider；**`server.go:4492` 仍 `retryAfterSeconds(provider.ID,…)`** | 2/3，剩 1 处 |
| D29-3 | 组内限额取"最后到达那行" | P1 | ❌ | `provider_lanes.go:124-139` `laneFor` 每次 acquire 覆盖 `lane.max/queueCap/rpm` | **重看确认** |
| D29-4 | 组内 0/0 行整组绕过 | P1 | ❌ | `provider_lanes.go:556-558` `max<=0&&rpm<=0` 提前返回空 release，未查组 | **重看确认** |
| D29-5 | 大小写：注释说不敏感，实现精确匹配 | P2 | ❌ | `providerLaneKey:542-547` 只 `TrimSpace` | 无 |
| D29-6 | `providerInFlight` 按行号查 | P2 | ❌ | `provider_lanes.go:565-571` 用原始 `providerID` 取 `lanes[]`（组键 `group:xxx`）⇒ 组内恒 0 | **重看确认** |
| D29-7 | `d93ec02` 夹带 failover 行为变更 | P3 | ❌ | 提交粒度 | 无 |
| D29-8 | lane 只增不删 | P3 | ❌ | 无回收 | 无 |

### M4 Provider / 账号 / OAuth / 额度

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| A1 | Trae/WorkBuddy/Cursor 刷新无锁 | P1 | ✅ | 继承（r24 `oauth_renew_lock`） | 无 |
| A2 | 子串匹配 host | P1 | ✅ | 继承（r24 `baseURLHostIn`） | 无 |
| A3 | `/v1/images` 路由误判 | P2 | ❌ | `server.go:3099-3105` 用 Contains 盲拼（`:3110-3116` 用 HasSuffix） | 无 |
| A4 | Claude overage / 负缓存 | P1 | ✅ | 继承（r26 负缓存 5/5） | 无 |
| A5 | Cursor 配额提前量 | P2 | ✅ | 继承（r10 纠错：已达标） | 无 |
| A6 | 出图超时被 120s 砍 | P2 | ❌ | `server.go:5712/5749` 均 `Timeout: 0` 且不走 `upstreamHTTPClient`（r32 纠正，非回归） | 无 |
| H-7 | images 拒绝检查 `route.ProviderID` 而非 `decision.ProviderID` | P1 | ❌ | `server.go:4277` 拒绝先于 `:4287 imagesEffectiveProvider`；`:6255` 用 `route.ProviderID` | **重看确认** |
| H-9 | workbuddy 11128 校验矩阵 | P1 | ✅ | 继承（r32 `workbuddy.go:857/861-891/972-983`，测试可红） | 无 |
| N-1 | workbuddy_ai 凭证 exp 空→永不续期 | P1 | ✅ | 继承（r20 401 自愈） | 无 |
| D31-1 | 用户暂停的额度任务被 keeper 每分钟复活 | P1 | ❌ | `quota_keeper.go:481-485` 注释 vs `:486` 判据 `len(live)==0` | **重看确认** |
| D31-4 | OAuth 用量缓存 key 不含账号身份 | P1 | ❌ | `oauth_usage_refresh.go:187/194/234/284`、`oauth_usage_cache.go:15` | **重看确认** |
| D31-5 | ChatGPT 续期等锁后仍用旧快照 refresh_token | P1 | 🟡 | `chatgpt_oauth.go:433-436` 重读结果只用于早退；`:437` 仍花旧 RT | 无 |
| D31-6 | 负缓存读侧死代码 | P2 | ❌ | `oauth_usage_cache.go:64-72/116-131` | 无 |
| D32-5 | 出图按 base64 `len/4` 估算入账，无图像价格 | P2 | ❌ 新增 | `server.go:5755` 恒空 usage → `:1367-1369` → `usage.go:450-455`；`grep -in image pricing.go` 空 | **本轮补核确认** |
| D32-7 | `resumeTasksForProvider` 无生产调用点 | P3 | ❌ 新增 | `quota_keeper.go:510`；`grep` 仅测试 `:765/:777` | **本轮补核确认** |

### M5 计费 / 用量 / 日志

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| M-19 | 错误响应被估算计费 | P1 | ✅ | 继承（r21） | 无 |
| N11-1 | 长流 usage 归零→估算放大 115× | P0 | ✅ | 继承（r29 `streamUsageReader` 逐字节折叠） | 无 |
| N12-2 | 日桶用服务器时区 | P2 | ⚪ | 生产 unit TZ=Asia/Shanghai（r13 `/proc` 实测） | 已举证 |
| N13-2 | `/__usage-analysis` 无上限、深 OFFSET | P2 | ❌ | 无 limit/分页；`FlushRequestLogs` 普通用户可反复触发 | 无 |
| N23-5 | `stream+JSON` 回包 usage=0（`fold!=nil` 时不回落） | P2 | ❌ | `usage.go:535-544` → `server.go:1367-1369` → `EstimateTokenUsage` | 无 |
| D31-2 | 监控中心不可停止（`context.Background()` 无 cancel） | P2 | ❌ | `app/runtime.go:353`；`monitoring/service.go:38-51` | 无 |
| D31-3 | 引擎吞事件写库错误 | P2 | ❌ | `monitoring/engine.go:315-324`；`dispatcher.go:274-285` 对 ID==0 no-op | 无 |
| D31-8 | `checkin_history` 无 account_key、`ListCheckinHistory` 零调用 | P3 | ❌ | `store/checkin.go:57-65` PK(provider_id,day) | 无 |
| W29-5 | `internal/monitoring` 整包未接线 | P2 | ✅ | 继承（r31：`runtime.go:194/353` + 18 条 `/__monitor` 路由） | **生产未上线** |
| W30-2 | 生产已建 monitor_* 六表全空 | P3 | 新增 | 生产只读：6 表存在，5 表 0 行、`checkin_history` 18 行；二进制无 monitoring 串 | **本轮重跑同结果** |

### M6 安全 / 鉴权 / 隔离

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| S1 | 管理 API `ACAO: *` | P0 | ✅ | 继承（r24 `cors.go` 白名单） | 无 |
| S2 | 请求体无上限 | P1 | ✅ | 继承（r24 `request_body_limit.go`） | 无 |
| S3 | 登录/setup 无限流 | P1 | 🟡 | `auth_throttle` 死代码/TOCTOU（r18） | 未核查 |
| S4 | 日志正文不脱敏 | P1 | ✅ | 继承（r18 `log_scrub`） | 无 |
| S5 | 首包后无空闲超时 | P1 | 🟡 | 守卫有，但心跳被 W29-3 吞进缓冲 ⇒ 长静默仍可能被 CF 掐 | 无 |
| N12-3 | 登录先解码后限流 + 全表扫 | P3 | 🟡 | 继承（r18 部分） | 无 |
| W30-3 | `/__monitor` 路由无 CSRF/Origin 校验 | P2 | ❌ | `monitoring_handlers.go:118-127` 全 `requireAdmin`；`cors.go:61-81` 只写响应头不拒请求；`admin_auth.go:326-333` 回环免鉴权 | 无 |
| H-8 | 管理员 Key 链不过滤 | P1 | ❌ | `user_isolation.go:123-125` `OwnerUserID==""` 直接 `return chain`；调用点 `provider_failover.go:1094/1492` | **从未核查→确认** |

### M7 存储 / 迁移

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| D29-1 | `laneGroup` 不落库（存储面） | P0 | ✅ | 同 M3（`sqlite.go:239/641/766`） | 无 |

### M8 前端控制台

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| C1 | 控制台 6 项（请求回放 UI / `sessionId` 展示 / 未读角标 / 成本列 / 事件流重放 / 失败重发）全未实现 | P2 | ❌ 新增（补记账） | `grep -rl`：`EventSource`=0、`replay`=0、`costUsd`=0、`resend`=0；`sessionId` 仅 `types.ts:679` 类型字段无 UI 使用；`badge` 6 处命中均为通用 CSS 类名 | **grep 重跑，同结论** |

> 说明：C1 是历史 6 项（r3 起多轮记为"控制台 6 项全未动"）的**合并记账**，此前从未在账本中占行。本轮补入账本以保证"每项每轮都出现"。

### M9 隧道 / 系统面

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| N13-3 | 容器无 TZ 且 verify 脚本期望 LA | P3 | ❌ | 继承（r13） | 无 |
| D32-4 | 隧道 token 明文落 `cloudflared.log`（0644） | P1 | ❌ 新增 | `tunnel/manager.go:300-301` `strings.Join(args," ")`（args 含 `--token`，`:653`）；`:343` `os.OpenFile(..., 0o644)` | **重看确认** |

---

## 七 回退与假修复

### 7.1 回归项：**无**

逐项比对 round32 状态：W29-1 ❌→❌、H-1/H-2/H-3/H-5 ❌→❌、D29-2 🟡→🟡、D31-1/D31-4 ❌→❌、D32-1/D32-4 ❌→❌、R4 ✅→✅、P5 ✅→✅、H-9 ✅→✅、D29-1 ✅→✅、W29-5 ✅→✅。**没有任何一项从 ✅ 掉下来**（本轮零增量，结构上不具备回归条件）。

### 7.2 关联回退嫌疑：无

本轮无代码变化，不存在"新问题落在已修模块内"的因果链。

### 7.3 假修复 / 假声明清单（本轮新增取证标 ★）

| 项 | 假在哪 | 依据 |
|---|---|---|
| ★ W29-1 | 注释（`passthrough_stream.go:271-277`）宣称「Otherwise the first byte goes straight through」，代码在 `:278` 无条件拦截 ⇒ 永远走缓冲 | `:278-283` + PROBE-33 |
| ★ D32-1 | 函数有完整实现、有清晰注释，**但零调用点**——"看起来像修复"的典型：任何人 grep 到函数就以为已支持 | `convert_tools.go:1165-1176` + `grep` 0 调用 |
| ★ D32-7 | 注释写「runs when a provider leaves the trash」，`grep` 只有测试调用 | `quota_keeper.go:507-528` |
| D31-1 | 注释写「never undone here」，代码 `len(live)>0` 就复活 | `:481-485` vs `:486` |
| H-1/H-2 | 同一语义两套实现互相否定，`convert_responses_fields.go:71-72` 的注释直接反驳 `convert_response_format.go:40-43` 的行为 | 两文件并读 |
| D31-5 | 注释宣称「Re-read after acquiring the lock」，实际重读只用于早退 | `chatgpt_oauth.go:431-437` |
| W30-1 | 文件名以 `zz_` 开头却无 build tag、无 `_test.go` 后缀，会被编进生产二进制 | `git ls-files` + `go build` 成功 |

### 7.4 部署面假信号（不属代码回归，但必须记）

`vcs.modified=true` 连续第 12 轮，意味着"二进制内嵌 revision"这个唯一的版本归属手段**本身失效**：`vcs.revision=d93ec02` 只是编译时 HEAD，**不代表二进制内容等于该 commit**。本轮用特征串反证：二进制含 `idleGuardedBody`(3)、`without its closing frame`(1)（两者仅存在于工作区 WIP），同时缺 `lane_group`/monitoring ⇒ 内容既非 `d93ec02` 也非 `2045543`。

---

## 八 下轮验收门槛（逐条可勾、可机械验证）

1. [ ] `git rev-parse --short HEAD` ≠ `2045543`，且 `git status --porcelain` 为空。
2. [ ] **W29-1 首字节**：新增断言时延的测试（不是断言 body），`newFailoverResponseWriter` 包装下 `slowBody{delay:400ms}` 三个分块必须 `firstWrite < 200ms` 且 `writes >= 2`。命令：`go test ./internal/gateway/ -run 'TestZZProbeStreamingFirstByte' -v`。**当前实测 1.2018s / writes=1 ⇒ 必红。**
3. [ ] **W29-1 修法**：`writeBufferedStream` 在 `!fw.canDefer` 时先 `copyPassThroughHeaders` + `fw.WriteHeader(response.StatusCode)`（`fw.WriteHeader` 在 `canDefer=false` 分支已会 `commitHeaders`）再逐块写；对照 `protocol_proxy.go:695` 的既有正确写法。验收同门槛 2。
4. [ ] **W29-2**：8MB 流跑完后 `len(fw.body) <= passThroughLogBufferMax`；`Prefix/StreamLen/streamClosed` 要么接线要么删除（`go vet` 无 unused）。
5. [ ] **W29-3**：同一条流测试断言 stream 期间底层 writer 收到 ≥2 次 `Flush`。
6. [ ] **H-1/H-2**：`convert_response_format.go` 的 `claudeStructuredFormat` 不再写 `name`，`json_object` 复用 `convert_responses_fields.go` 的宽松 `json_schema`；同步改 `convert_response_format_test.go:19-23/29/107`（否则测试会把修复判红）。
7. [ ] **H-3**：`applyAdaptiveThinking` 只写 `output_config["effort"]`（merge 不替换）；显式 thinking 分支同样 merge。验收：同时带 `response_format` + `reasoning_effort` 的请求，`output_config` 同时含 `format` 与 `effort`。
8. [ ] **H-5**：`claudeCacheableBlock` 改黑名单（排 `thinking`/`redacted_thinking`）；新增用例：末条为 `tool_result` 时必须打上 `cache_control`。
9. [ ] **D32-1**：`openAIChatToClaudeRequest` 接上 `chatStopToClaudeSequences`；验收 `stop=["\n\n"]` 的 Chat 请求转出的 Claude body 含 `stop_sequences`，旧实现该用例必红。
10. [ ] **D32-4**：`tunnel/manager.go` 记录 argv 时把 `--token` 后的值替换 `***`（或只记 `bin` + 参数个数），文件权限 `0600`；验收 `grep -a "token" cloudflared.log` 无明文。
11. [ ] **D31-1**：`scheduled_tasks` 增 `pause_reason`（`user|keeper`），`reviveScheduledTask` 只复活 `pause_reason='keeper'`；验收：控制台暂停后连续 3 个 tick 仍 `enabled=0`。
12. [ ] **D31-4**：用量 cache key 加账号维度（`AccountLabel`/`ChatGPTAccountID`/RT 指纹），并在 `SetProviderXxxOAuth` 写入时 invalidate；验收：换号后 `cachedClaudeFiveHourUtilization` 返回新值。
13. [ ] **D29-2 收口**：`grep -n "retryAfterSeconds(provider.ID" internal/gateway/server.go` 输出为空。
14. [ ] **D29-3/D29-4/D29-6**：组内限额取"组内各行的严格最小非零值"；0/0 行必须查组；`providerInFlight` 用 `providerLaneKey`。各一个用例。
15. [ ] **H-8**：`allowedProviderChainForKey` 对管理员 Key 也要剔除 `Disabled`/`Deleted`/协议不匹配的 Provider（不是按 owner 过滤，而是按可用性过滤）；验收：管理员 Key + 含回收站 Provider 的兜底链，链被裁掉该项。
16. [ ] **D32-5**：出图路径写入真实 usage 或显式标注"无 usage，不计费"；`pricing.go` 增图像行；验收 `grep -in image pricing.go` 非空且出图 `input_tokens/output_tokens` 不再等于 `len/4`。
17. [ ] **部署治理（前置条件）**：发布自检必须包含 `grep -a -o "vcs.modified=[a-z]*" bin/gateway` == `false`，且 `vcs.revision` == 发布 commit；同时用特征串反证（WIP 串命中数应为 0）。**这一条不修，上面 16 条修了也无法证明线上生效。**
18. [ ] **W30-1**：`zz_quota_keeper_e2e_hooks.go` 移到 `_test.go` 或加 `//go:build e2e`；验收 `git ls-files | grep zz_` 为空。

---

## 附 覆盖度声明

| 模块 | 已扫文件数 / 总数 | 未核查项 |
|---|---|---|
| M0 基线与卫生 | 全仓 `build`/`vet`/4 包 `test` + 提交元数据 + 卫生 grep（zz_/bak/println/superpowers） | `gofmt`（CRLF 假阳性 + 只读无法回写） |
| M1 协议转换·请求 | 5/18 个非测试 `convert_*.go`（`convert_response_format.go`、`convert_responses_fields.go`、`convert_claude_openai.go`、`convert_claude_cache_marks.go`、`convert_tools.go` 关键段） | 其余 13 个 `convert_*.go` 本轮未重读（零增量，继承 r32）；H-6 三个子项 |
| M2 流式 | `passthrough_stream.go`（255-300/539-600）、`provider_failover.go`（225-380）、PROBE-33 实跑 | `sse_heartbeat.go` 全文、N25-3/W29-6 未复跑 |
| M3 调度 | `provider_lanes.go`（118-142/550-580）、`server.go:4485-4500`、`protocol_proxy.go:844` | `provider_cooldown*`、`upstream_timeout.go` 未读 |
| M4 Provider/OAuth | `quota_keeper.go`（474-500/507-528）、`oauth_usage_refresh.go`（183-198）、`workbuddy.go`（855-890）、`chatgpt_oauth.go` 引用段 | `checkin*`、`cursor_oauth.go` 全文；本模块运行时 A/B 为 0 |
| M5 计费/用量 | `server.go:5745-5760`、`pricing.go`（image grep）、生产库计数 | `request_log_batcher.go`、`usage_analysis.go` 未逐行 |
| M6 安全 | `user_isolation.go:122-160`、`cors.go`/`admin_auth.go` 引用段 | S3 的 TOCTOU 未复跑 |
| M7 存储 | `sqlite.go` lane_group 读写、生产 `pragma table_info` | 其余迁移未逐条走查 |
| M8 前端 | **仅 grep**（EventSource/replay/costUsd/resend/sessionId/badge） | **整模块未做几何实测（连续 2 轮顺延，时间盒）** |
| M9 隧道 | `tunnel/manager.go`（296-306/338-346/653 引用） | 未触发真实隧道（按约束只读）；`internal/daemon` 类对照未做 |

**magpie 对照结论（本轮无变化）**：`streamFailure`（`internal/gateway/gateway.go:4940`）与我们的 `stream_interruption_close.go` 同语义（R4 已对齐，**但未上线**）；`unknownOptionalField`（`:5536`，重试在 `:4564-4577`）只对顶层白名单字段的 unknown/unsupported 400 生效，显式跳过 located/unsupported_value（`:5610-5615`）与嵌套路径（`:5576-5582`）⇒ **救不了 H-1/H-2/H-3/H-4/H-5 任何一条**；流式交付在 magpie 是 `gateway.go:1396` 的 `io.Copy` 直发（对照 W29-1 的 buffer-then-replay）；账号身份在 magpie 是数据模型实体（`internal/access/accounts.go:15-57`），我们的"自由文本 provider 行 + 无账号维度缓存"正是 D31-1/D31-4 的结构性根因。

---

## 收尾自检

- [x] `git status --porcelain` 0 行（`.workbuddy/reviews/` 被 `.gitignore` 忽略）；一次性探针 `internal/gateway/zz_probe33_test.go` 已删除，`git status` 复位为 0
- [x] 无临时 worktree 残留（`git worktree list` 仅主树 `C:/guodevelop/llm-protocol-gateway 2045543`）——本轮未创建 worktree
- [x] 账本已更新（96 → 97 行，新增 C1）；账本每个未闭环 ID 都在第六节出现
- [x] 每个 ✅ 都能指到 E1/E2/E3 之一（本轮新增 ✅：**无**；唯一 P0 仍为 ❌）
- [x] 覆盖度声明已填，未核查项逐条列举（M8 整模块顺延已注明）
- [x] 无 commit / push / 生产写操作发生（生产仅 `systemctl is-active`/`show`、`ls -l`、`sha256sum`、`grep -a`、sqlite `mode=ro`）

---

## 给修复 agent 的订单

> 原则：本轮零增量，订单与 r32 基本一致，但**顺序必须改**——先解决部署可追溯，再修 P0，否则修了也无从证明。

**P0（阻断上线）**

1. **先让发布可追溯**：用干净树 `git archive` 或 `go build` 于无未提交改动的目录产出二进制，发布后自检 `vcs.modified=false` 且 `vcs.revision == <release commit>`。当前线上是"脏树 d93ec02 + WIP"，没有任何 commit 能复现。
2. **W29-1 透传流全量缓冲**（`passthrough_stream.go:278-283`、`:539`；`provider_failover.go:347-373`）
   - 改哪里：`:278` 的条件加 `&& fw.canDefer`；`!canDefer` 时进"直发"分支——先 `copyPassThroughHeaders(fw, response)` + `fw.WriteHeader(response.StatusCode)`（此调用会 `commitHeaders` 并把 `fw.passthrough` 置真），再 `io.Copy` 逐块写。
   - 改成什么语义：一旦开始向客户端交付首字节，本次尝试不可重试；未交付前保持可 defer。心跳必须走直发路径才真正触达客户端。
   - 验收：门槛 2、3、5。当前实测 1.2018s / `writes=1` ⇒ 门槛 2 必须能红。
3. **W29-2 随之解决**：直发分支下 `fw.body` 不再无上限累积（只保留日志用截断副本）。

**P1（按"线上在跑 + 影响面"排序）**

4. **D32-4 隧道 token 脱敏**（`tunnel/manager.go:300-301` + `:343`）：日志只记 `bin` 与参数个数，或 `--token` 后替换 `***`；文件 `0600`。这是唯一"密钥明文落盘"类缺陷，优先级高于协议问题。
5. **H-1/H-2**（`convert_response_format.go:18-20/40-43`）：删 `name`；`json_object` 降级宽松 `json_schema`（复用 `convert_responses_fields.go:73-80`）；同步改 `convert_response_format_test.go:19-23/29/107`。
6. **H-3**（`convert_claude_openai.go:322-325`、`:394-397`）：`output_config` 用 merge 而非整块赋值，保住 `format`。
7. **H-5**（`convert_claude_cache_marks.go:92-103`）：白名单改黑名单（排 `thinking`/`redacted_thinking`），让 `tool_result` 可作断点。
8. **D32-1**（`convert_claude_openai.go:177-218`）：接上 `chatStopToClaudeSequences` 写 `stop_sequences`。
9. **D31-1**（`quota_keeper.go:486`）：加 `pause_reason` 字段区分 user/keeper，keeper 只复活自己的。
10. **D31-4**（`oauth_usage_refresh.go:187/194/234/284`）：cache key 加账号维度 + 写入侧 invalidate。
11. **H-8**（`user_isolation.go:123-125`）：管理员 Key 也要按"Provider 可用性"过滤兜底链（disabled/deleted/协议不匹配），不要用 owner 维度。
12. **H-7**（`server.go:4277` vs `:4287`）：refusal 判定移到 `imagesEffectiveProvider` 之后，用 `decision.ProviderID`。
13. **D29-2 收口**（`server.go:4492`）：改 `retryAfterSecondsFor(provider, …)`。
14. **W29-3**：随 W29-1 解决；若 W29-1 暂不修，至少让 `fw.Flush()` 在 `canDefer=false` 时透传到底层 `Flusher`。

**P2**

15. D29-3/D29-4/D29-6：组内限额取严格最小非零、0/0 查组、`providerInFlight` 用组键。
16. D32-5：出图写真实 usage 或显式"不计费"标注 + 价格表补图像行。
17. D32-3：`min(budget, max_tokens-1024)` 钳制。
18. N23-5：明确"fold 为 nil 时回落 parseUsage"的边界。
19. C1（控制台）：**先不要动**，等 P0/P1 清完再排期。

**P3**

20. W30-1（`zz_*` 移出生产源码，验收 `git ls-files | grep zz_` 为空）、D31-7（清理 `.superpowers/`）、D31-8、D32-6、D32-7、D32-8、N13-1、N13-3。

**一句话**：本轮**没有任何代码需要"返工"——需要的是"开始动"**。唯一的 P0 已 13 轮未动，而生产跑的是一个谁都复现不了的脏树二进制；这两件事不解决，后面 95 项全绿也没有意义。
