# round34 对抗性代码审查报告

> HEAD `2045543` ｜ `git status --porcelain` **0 行**（工作区干净）｜ `git diff` 空 ｜ `git diff --cached` 空
> 内容指纹：`git diff | sha256sum` = `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`
> 　　　　　`git diff --cached | sha256sum` = `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`
> 增量口径：**无增量（连续第三轮）**——无新提交、无 WIP、无未跟踪文件
> 轮次：**round34**（`roundN` 最大 N=33，+1）；审查时间 2026-10-24 06:32–07:20 (+0800)
> 只读约束：未改任何业务代码，未 commit / push / pull / rebase / reset；生产仅只读命令；探针已删除并留证

---

## 0 审查对象与增量判定

| 项 | 值 |
|---|---|
| `git rev-parse --short HEAD` | `2045543`（`fix(stream): 截断收尾改用协议错误事件并对齐 magpie`，2026-10-24 02:16:58 +0800） |
| `git status --porcelain` | **0 行**（改 0 / 增 0 / 删 0） |
| `git diff --stat` | 空（+0 / −0） |
| 指纹 | `e3b0c442…b855` ×2（空输入哈希） |
| 与 round33 差异 | **逐字节一致**：HEAD 同、双哈希同、reflog 末条仍是 02:16:58 的提交 |
| 与 round31 差异 | HEAD 相同（`2045543` 自 r31 起未变，已连续第 4 轮） |
| 仓库内文件 mtime > 05:20 (+0800) | **0 个**（除 `.workbuddy/` 自身）——作者自 r31 提交后 **约 29 小时** 未动业务代码 |
| 临时 worktree | 无（`git worktree list` 仅主树） |

**判定：无增量。** 按规则三项口径：

1. 第六节照常出**全量清单（97 条，与账本 97 行逐条对齐，脚本校验 missing=[]）**；
2. 复用证据一律写成「继承（代码哈希未变，roundNN 第 E* 号证据）」；
3. **所有未闭环 P0/P1 全部重跑一次**：本轮实跑 21 项（其中 W29-1/N23-1 为新探针 E1 实跑，H-6 用新探针 E1 取到决定性事实），余项逐条重看决定性行号；生产只读全部重跑。

---

## 一 结论先说

**不能上线。** 代码连续第三轮零增量、连续第 4 轮停在同一个 HEAD，唯一 P0 连续第 14 轮未修；线上跑的仍是一个**没有任何 commit 能复现的脏树二进制**。

1. **P0 `W29-1`（透传流全量缓冲）连续第 14 轮未修，本轮新探针 E1 第三次证实。** PROBE-34（同一 HEAD、三个 400 ms 分块）：裸 writer 首字节 **400.4 ms**（两次运行 400.425/400.433）、`writes=3`；换成 `newFailoverResponseWriter` 后 `canDefer=true` 首字节 **1.2010–1.2016 s = 总时长**、`writes=1`，`canDefer=false` 同样 **1.2010–1.2016 s**、`writes=1`。三个零改动成因未变：`passthrough_stream.go:278` 的拦截条件不含 `canDefer`、`writeBufferedStream`(:539) 全程不 `WriteHeader`、`provider_failover.go:347-357` 只落 `buf`、`:368-373` 的 `Flush` 在 `!passthrough` 时是空操作。
2. **生产仍在跑"脏树 + 非 HEAD"（连续第 13 轮）**：sha256 `9a0d86d6…c31f`、PID 911205、启动于 `Fri Oct 23 06:32:38 2026`（= 10-23 21:32 +0800）、`vcs.revision=d93ec02` + **`vcs.modified=true`**；`lane_group` 命中 0、`monitoring snapshot` 命中 0，却含只在工作区存在过的 `without its closing frame`(1)、`idleGuardedBody`(3) ⇒ 既缺 `2045543` 的 R4 修复、也缺 lane 落库，且带了未审计代码。
3. **线上旁证与 W29-1 同向（E4，不作判据）**：24h `pass_through` 1255 条中 **1247 条（99.4%）`latency−ttft ≤ 5 ms` 且均值 0.67 ms**，`convert` 9467 条均值 1153.6 ms。透传流的首字节等于末字节。
4. **本轮唯一实质新证据：H-6(c) 用探针钉死了型号名解析的覆盖洞（新增 N34-1）。** `claude-3.5-sonnet` / `claude-3-5-sonnet-20241022` / `anthropic/claude-3.5-sonnet` 全部被判为**"不是 Claude 型号"**（`known=false`），于是走"非 Claude 端点"分支：`thinking` 被静默删除、`max_tokens` 从 **200 抬到 2248**。上线口径影响：客户端刻意要的 `disabled` 语义还在，但**预算被放大 11 倍**且改写不可见。
5. **P1 十条复核全部原样（带行号）**：H-1/H-2 仍是"同语义两套实现互相打脸"（`convert_response_format.go:22-23/:43` 写 `format.name`，`convert_responses_fields.go:71-72` 注释白纸黑字说 Claude 拒收该字段）；H-3 的 `output_config` 仍被 `applyAdaptiveThinking`(:322-325) 整块赋值覆盖 `format`；H-5 cache 断点白名单仍漏 `tool_result`；H-7 images 拒绝仍查 `route.ProviderID`(:6255)；H-8 管理员 Key 链仍直接 `return chain`(:123-125)；D32-1 `chatStopToClaudeSequences` 仍 0 调用点。
6. **P1 调度侧四项原样**：D29-2 还剩 `server.go:4492` 按行号查 `retryAfterSeconds`；D29-3 `laneFor`(:124-139) 每次 acquire 覆盖 `max/queueCap/rpm`；D29-4 (:556-561) 组内 0/0 行整组绕过；D29-6 (:565-578) 按行号取 `lanes[]` ⇒ 组内恒 0。
7. **本轮关闭一项**：`W29-6`（非 Responses 流扫描无界累积）已由 `stream_end_reader.go:780-795` 的尾部封顶（`>2*passThroughLogBufferMax` 即裁到 `passThroughLogBufferMax`）修掉，🟡→✅（E2）。
8. **治理问题已比代码问题更致命**：`vcs.modified=true` 连续 13 轮 ⇒ 线上任何行为都无法对应到 commit，包括本轮与上轮的所有"已修复"结论。**订单顺序照旧：先让线上可追溯，再谈修 P0。**

---

## 二 基线（build / vet / test / 卫生）

| 项 | 命令 | 结果 |
|---|---|---|
| build | `go build ./...` | **rc=0**（无输出） |
| vet | `go vet ./...` | **rc=0**（无输出） |
| test | `go test ./internal/gateway/ ./internal/store/ ./internal/app/ ./internal/monitoring/` | `gateway 42.424s ok` / `store ok(cached)` / `app 1.498s ok` / `monitoring 0.036s ok` — **0 红** |
| gofmt | `gofmt -l internal/ cmd/` | 6 命中（`provider_lanes.go`/`router.go`/`sqlite.go`/`main.go`/`embeddings_test.go`/`session_pins_test.go`）——`gofmt -d` 显示整文件重写，`file` 确认 **CRLF**，属 Windows 换行假阳性，非格式缺陷（与 r30 结论一致） |
| 调试打印 | `grep -rn 'println(' internal/gateway/*.go \| grep -v _test` | **0 处**（r13/r15 的 `DBG`/`ZZW` 脚手架已清） |
| 测试脚手架入仓 | `git ls-files \| grep -E "zz_\|\.bak$\|\.superpowers"` | `internal/gateway/zz_quota_keeper_e2e_hooks.go`（**W30-1 仍在**）、`.superpowers/…` 5 文件（**D31-7 仍在**）；`.bak` 已清（ed0c139） |

> 说明：`go test` 含本轮临时探针（已删），探针本身 PASS；不含 zz 探针时的四包结果与 r32/r33 一致。

---

## 三 新提交审查

**无新提交。** `git log -1` 与 r31/r32/r33 同一行；`git reflog` 末条仍是 `2045543 HEAD@{2026-10-24 02:16:58 +0800}: commit`。本节无 D 系列新增。

---

## 四 工作区 WIP 审查

**无 WIP。** `git diff`、`git diff --cached` 均为空哈希；仓库内业务文件 mtime 全部 ≤ 2026-10-24 02:16（提交时刻），无未跟踪文件。本节无 W 系列新增。

> 与 r33 相比的唯一差别：`.workbuddy/` 下新增本报告与证据目录（审查产物，不计入业务 WIP）。

---

## 五 生产环境只读核查

| 项 | 读数 | 判定 |
|---|---|---|
| 服务 | `systemctl is-active` = **active**，`MainPID` = **911205** | 在跑 |
| 二进制 | `/proc/911205/exe` → `/opt/llm-protocol-gateway/bin/gateway`，sha256 `9a0d86d67e82c3171b5013e3df3ae0a07ff041ad09c428948a6209eb9ecdc31f` | 与 r33 **逐位一致** |
| 启动时刻 | `Fri Oct 23 06:32:38 2026`（PDT）= **10-23 21:32:38 +0800** | 与 `d93ec02` 提交（21:32:11）相差 27 s |
| 版本归属 | `vcs.revision=d93ec02c9365b20a65d0e92702a92b998b9632d7`、**`vcs.modified=true`** | **脏树，连续第 13 轮** |
| 特征串 | `lane_group`=0、`monitoring snapshot`=0、`connection lost mid-reply`=0、`without its closing frame`=1、`idleGuardedBody`=3 | 缺 lane 落库与监控；含仅工作区存在的串 ⇒ 线上≠任何 commit |
| DB | `/var/lib/llm-protocol-gateway/gateway.db` 2 412 736 512 B(2.41 GB)，以 `file:…?mode=ro` 打开 | 只读，未写 |
| 规模 | providers 50（`disabled=0` 计 50）、api_keys 9、`max_concurrent_requests>0` 仅 `deepseek-gq-api`=1 | lane 实际只对 1 个 Provider 生效 |
| Schema | `providers` **无 `lane_group` 列**（`pragma_table_info` 命中 0） | D29-1 的修复未上线 |
| 监控表 | `monitor_events`/`monitor_alert_states`/`monitor_event_pushes`/`monitor_baselines`/`provider_metric_samples` **全部 0 行**；`checkin_history` 18 行 | 与二进制无 monitoring 串一致：表在、码不在 |
| 24h 流量 | convert 9467（`latency−ttft` 均 1153.6 ms）、pass_through 1255（0.67 ms，1247/1255 ≤5 ms）、rejected 5、test_convert 1；5xx 7 条 | W29-1 旁证 |
| 最新日志 | `2026-10-23T18:47:49Z`（= **10-24 02:47 +0800**），此后 **3.7 小时零请求** | 生产数据自 r33 起冻结，本轮无新增运行时证据 |

> 纠正一处历史口径：早前报告写的"50 Provider / 20 活跃"里，"活跃"应是按"有可用 Key/可用额度"另算；本轮实测 `disabled=0` 为 50（全部未禁用）。本报告不据此下任何结论，只记录差异。

## 六 完整排查清单（全量 97 条，与 `LEDGER.md` 逐行一致）

> 生成方式：脚本直接读 `.workbuddy/reviews/LEDGER.md`（97 行）并按模块归组，保证零遗漏；「本轮变化」列区分 `继承`（代码哈希未变，沿用上轮证据）与 `r34 复验`（本轮实跑/重看）。

### M1 协议转换·请求方向

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| P1 | Claude→Chat 流式丢 prompt_tokens | P1 | ✅ | usage 折叠 | 继承 |
| P2 | stop_reason 全量映射缺项 | P1 | ✅ | 映射表 | 继承 |
| P3 | Chat→Responses 有 tool_calls 清空正文 | P1 | ✅ | 测试 | 继承 |
| P4 | Responses→Chat 丢 reasoning 增量 | P1 | ✅ | 测试 | 继承 |
| P5 | Responses→Chat 忽略 incomplete | P2 | ✅ | 非流 convert_openai_responses_chat.go:870-872 + convert_tools.go:1749-1760；流 convert_openai_responses_chat_stream.go:661-6 | 继承 |
| P7 | sanitizeAnthropicToolUseID 碰撞 | P1 | ✅ | 测试 | 继承 |
| P8 | Responses→Claude 缺合成 tool_result | P1 | ✅ | 测试 | 继承 |
| P9 | redacted_thinking 丢失（占位非回放） | P2 | 🟡 | convert_* | 继承 |
| P10 | strict/top_k/stop_sequences 等不转发 | P2 | 🟡 | convert_* | 继承 |
| P11 | 两两手写转换 vs magpie 单一 IR | P2 | ❌ | 架构未动 | 继承 |
| H-1 | Claude output_config.format 是否该发 name | P1 | ❌ | convert_response_format.go:40-43 写 name；同仓 convert_responses_fields.go:71-72 注释明写「Claude GA 拒收 format.name」 ⇒ 二者必有一错 | E2 重看 `convert_response_format.go:22-23/:43` vs `convert_responses_fields.go:71-72` |
| H-2 | `response_format=json_object` 原样直传 Claude | P1 | ❌ | convert_response_format.go:18-20 直出 {"type":"json_object"}；convert_responses_fields.go:73-80 明说 Claude 无此模式并降级宽松 json_sc | E2 重看 `convert_response_format.go:18-20` |
| H-3 | thinking 覆盖 output_config | P1 | ❌ | 真正覆盖点 convert_claude_openai.go:322-325（applyAdaptiveThinking 整块赋值）与 :394-397；顺序 :208 写 format → :213 thinking。账本原指 claud | E2 重看 `convert_claude_openai.go:322-325`（整块赋值 `output_config`） |
| H-4 | Responses allowed_tools 塞进 Claude tool_choice | P1 | 🟡 | convert_responses_claude.go:496-516 构造该键并收窄 tools；magpie responses.go:502-533 只置 mode+过滤，从不下发 | E2 重看 `convert_responses_claude.go:497-506` |
| H-5 | cache marks 白名单漏 tool_result | P1 | ❌ | convert_claude_cache_marks.go:93-101 只认 text/image/document；markClaudeMessageTail:68-90 找不到即 return false ⇒ agent 续跑断点 0 | E2 重看 `convert_claude_cache_marks.go:93-101`（仅 text/image/document） |
| H-6 | thinking 五连（预算补正/glm none/unknown 模型） | P1 | 🟡 | 仅确认 claude_pass_through_normalize.go:151-169 无条件改写 max_tokens | **E1 新探针 PROBE-34H**：型号名解析表 + 线上报文改写实证（见 N34-1） |
| M-1 | Claude→Chat 无条件 stop | P1 | ✅ | 有条件守卫（r10 纠错） | 继承 |
| D32-1 | Chat→Claude 完全丢弃 `stop`（chatStopToClaudeSequences 是死代码） | P1 | ❌ 新增 | convert_tools.go:1165-1175 仅定义、全包 0 调用；convert_claude_openai.go:177-218 不读 stop | grep 复跑：`chatStopToClaudeSequences` 仍仅定义行、**0 调用点** |
| D32-2 | Claude→OpenAI 回复路径不做 declared 名回填（模型改大小写即 unsupported call） | P2 | ❌ 新增 | convert_tools.go:1272-1311 只试精确/ToLower/OAuth 反向映射，不调 :1337 restoreDeclaredToolName | 继承 |
| D32-3 | thinking 预算与 max_tokens 无钳制（override 小于预算即 400） | P2 | ❌ 新增 | convert_claude_openai.go:325-331 写 16k/32k，:196 写 max_tokens，二者从不比较；magpie anthropic.go:968 有 min(budget,max-1024) | 继承 |
| N34-1 | M1 | Claude 型号名正则漏 3.x 与厂商前缀写法 ⇒ 规则表失效并擅自放大 max_tokens | r34 | **P2** | ❌ 新增 | **r34** | **PROBE-34H（E1）**：`claude-3.5-sonnet`/`claude-3-5-sonnet-20241022`/`anthropic/claude-3.5-sonnet` 全部 `known=false`；`normalizeClaudeEndpointThinkingOffBody` 走 `!knownClaude` 分支删 `thinking` 并把 `max_tokens` 200→**2248**（`claude_thinking_off.go:222-226/:270-277`、正则 `:28`） | 新增（本轮首次实证） |
| D32-6 | safety_identifier 全仓 0 命中（Responses 新字段静默丢失） | P2 | ❌ 新增 | grep -rn safety_identifier internal/ 空；convert_conversation_fields.go:27-36 只搬 cache_key/service_tier | 继承 |

### M2 协议转换·流式

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| P6 | Chat→Claude block index 错乱 | P1 | ✅ | 测试 | 继承 |
| R4 | 流截断不能续/被当正常结束 | P0 | ✅ | 6 处读后判定（convert_claude_openai_stream:216 / convert_claude_responses_stream:430 / convert_openai_claude_stream:297 / conv | 继承 |
| N10-1 | 透传 >256KB 流被判截断（头部 256KB 判据） | P0 | ✅ | 判据改为 `usage.closed()` 全字节 | 继承 |
| N10-2 | 中断收尾帧写裸 w 绕过锁 | P1 | ✅ | 改走 hw | 继承 |
| N11-2 | 心跳 stop 不 join | P1 | ✅ | 有界 join 2s | 继承 |
| N11-3 | 透传 Claude 收尾漏 content_block_stop | P2 | ✅ | openBlocks 逐个 close | 继承 |
| N11-4 | 透传 Responses 收尾帧缺 id/model/seq | P2 | ✅ | responsesStreamIdentity | 继承 |
| N14-2 | 转换路径中断判定恒 false | P0 | ✅ | 读后判定 | 继承 |
| N23-1 | canDefer 在流式路径是死变量 | P1 | ❌ | 与 W29-1 同因（PROBE-33 本轮实测 canDefer 真假首字节均 1.20s、writes 均 1） | **E1 重跑**：canDefer=false 仍 writes=1，与上同因 |
| N23-2/3 | 两套完成判定结论相反 | P2 | 🟡 | 部分统一 | 继承 |
| N24-3 | streamEndReader 只携带 4 字节 | P2 | ✅ | 按 maxClosingMarkerLen-1 | 继承 |
| N24-5 | 两套完成判定无回声守卫 | P3 | 🟡 | 回声守卫仅末 64B | 继承 |
| N25-1 | 中断谓词语义反了 | P1 | ✅ | 已在 isUpstreamStreamInterruption | 继承 |
| N25-3 | scanResponses 非 Responses 流全量累积 | P2 | 🟡 | 未复跑 | 继承 |
| N25-5 | 缓冲路径零心跳 | P2 | ❌ | 探针 flushes=0，心跳落 buf | 继承 |
| W29-1 | 透传流全量缓冲（TTFT=总时长） | P0 | ❌ | PROBE-33（E1 本轮新跑）：failover-canDefer-true firstWrite=1.2018s=total writes=1；canDefer-false 1.2011s writes=1；plain-writer  | **E1 重跑**（PROBE-34）：canDefer 真假首字节均 1.2016s=总、writes=1；裸 writer 400.4ms/writes=3 ⇒ **第 14 轮未修** |
| W29-2 | passthrough 下响应再复制进 fw.body（无上限） | P1 | ❌ | provider_failover.go:359-362 w.body.Write(p[:n]) 无条件；Prefix/streamClosed/StreamLen 零或仅测试调用 | E2 重看 `provider_failover.go:358` 无条件 `w.body.Write` |
| W29-3 | 心跳进缓冲区，流中收不到 ping | P1 | ❌ | 同 W29-1：hw→fw.Flush 在 !passthrough 时是空操作 | E2 重看 `passthrough_stream.go:554-555` 心跳仍包 `fw` |
| W29-6 | scanResponses 非 Responses 流全量累积 | P2 | ✅ | 未复跑 | E2 重看 `stream_end_reader.go:780-795`：尾部封顶 `2*passThroughLogBufferMax` ⇒ **关闭** |
| W29-7 | 回声守卫只看末 64B | P2 | 🟡 | 代码 | 继承 |

### M3 调度/路由/并发/故障转移

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| R1 | 无 RPM 限流 | P1 | ✅ | 8b5542d 滑动窗口 | 继承 |
| R2 | 冷却不看上游 Retry-After | P1 | ✅ | 阶梯可达 | 继承 |
| R3 | billing 归入 hardQuota | P1 | ✅ | 代码 | 继承 |
| R5 | 会话粘性不落库 | P1 | ✅ | session_pins | 继承 |
| R6 | 并发闸门非 FIFO | P1 | 🟡 | D29-3/D29-4 仍在 | 继承（与 D29-3/D29-4 连坐，同因未修） |
| D29-1 | laneGroup 完全不落库 | P0 | ✅ | sqlite.go:239 列 + :641/:766 读写 + `TestProviderLaneGroupRoundTrip`/`TestMigrateAddsLaneGroupToOldDatabase` 绿 | 继承 |
| D29-2 | Retry-After 出口按行号查分组 | P1 | 🟡 | protocol_proxy.go:833、provider_failover.go:1234 已改 retryAfterSecondsFor；server.go:4488 仍 retryAfterSeconds(provider.ID,… | E2 重看 `server.go:4492` 仍按行号 |
| D29-3 | 组内限额取“最后到达那行” | P1 | ❌ | provider_lanes.go:124-139 laneFor 每次 acquire 覆盖 lane.max/queueCap/rpm（重看确认） | E2 重看 `provider_lanes.go:124-139` |
| D29-4 | 组内 0/0 行整组绕过 | P1 | ❌ | provider_lanes.go:556-561 max<=0&&rpm<=0 提前返回空 release，未查组（重看确认） | E2 重看 `provider_lanes.go:556-561` |
| D29-5 | 大小写：注释说不敏感，实现精确匹配 | P2 | ❌ | providerLaneKey:542-547 只 TrimSpace | 继承 |
| D29-6 | providerInFlight 按行号查 | P2 | ❌ | provider_lanes.go:565-578 用原始 providerID 取 lanes[]，分组键为 group:xxx ⇒ 组内恒 0 | E2 重看 `provider_lanes.go:565-578` |
| D29-7 | d93ec02 夹带 failover 行为变更 | P3 | ❌ | 提交粒度 | 继承 |
| D29-8 | lane 只增不删 | P3 | ❌ | 无回收 | 继承 |
| W29-11 | failoverResponseWriter 死代码（StreamLen/streamClosed/recordsForRetry） | P3 | 🟡 | `StreamLen()`/`streamClosed()` 全仓零调用；`Prefix()` 仅测试调用 | 继承 |

### M4 Provider/账号/OAuth/额度

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| A1 | Trae/WorkBuddy/Cursor 刷新无锁 | P1 | ✅ | oauth_renew_lock | 继承 |
| A2 | 子串匹配 host | P1 | ✅ | baseURLHostIn | 继承 |
| A3 | /v1/images 路由误判 | P2 | ❌ | server.go:3099-3105 用 Contains 盲拼（对照 :3110-3116 用 HasSuffix）；带 query 的 BaseURL 会拼错 | 继承 |
| A4 | Claude overage / 负缓存 | P1 | ✅ | 负缓存 5/5 | 继承 |
| A5 | Cursor 配额提前量 | P2 | ✅ | 纠错：已达标 | 继承 |
| A6 | 出图超时被 120s 砍 | P2 | ❌ | server.go:5712/5749 均 Timeout:0 且不走 upstreamHTTPClient；所谓 10min 是 OAuth 登录轮询（cursor_oauth.go:494） | 继承 |
| H-7 | images 拒绝检查 route.ProviderID 而非 decision.ProviderID | P1 | ❌ | server.go:6255 ProviderByID(route.ProviderID)；真正落点 :4287-4301 用 decision.ProviderID；发送侧不校验 Disabled/Protocol | E2 重看 `server.go:6255` 仍 `route.ProviderID` |
| H-9 | workbuddy 11128 校验矩阵 | P1 | ✅ | workbuddy.go:857 覆盖 system+assistant、:861-891 双分支、:972-983 developer→system 且先归一化后 unflag（:997）；workbuddy_test.go:158-17 | 继承 |
| N-1 | workbuddy_ai 凭证 exp 空→永不续期 | P1 | ✅ | 401 自愈关闭 | 继承 |
| D31-1 | 用户暂停的额度保活任务被 keeper 每分钟复活 | P1 | ❌ 新增 | quota_keeper.go:485-489 判据（只判 Enabled==false + 有 live provider）、调用点 :348/:524；注释自称“人工暂停永不被撤销” | E2 重看 `quota_keeper.go:485-489` |
| D31-4 | OAuth 用量缓存 key 不含账号身份（换号 1h 内串额度并参与调度） | P1 | ❌ 新增 | oauth_usage_refresh.go:187/194/234/284、oauth_usage_cache.go:15 | E2 重看 `oauth_usage_refresh.go:284/291/327/338`（键仍 `proto:providerID`，无账号维度） |
| D31-5 | ChatGPT 续期等锁后仍用旧快照 refresh_token | P1 | 🟡 新增 | chatgpt_oauth.go:433-436 有重读但只用于早退；:437 仍花旧 RT、:442 以旧快照为底；对照 claude_oauth.go:548-568 | E2 重看 `chatgpt_oauth.go:433-437`（旧快照 RT 仍被花） |
| D31-6 | 负缓存读侧死代码（lastError/waitForRefresh 零调用，面板能力未实现） | P2 | ❌ 新增 | oauth_usage_cache.go:64-72/116-131 | 继承 |
| D32-5 | 出图按 base64 正文 len/4 估算 token 入账，且价格表无图像行 | P2 | ❌ 新增 | server.go:5755 恒空 usage → :1367-1369 → usage.go:450-455；grep -i image pricing.go 空 | 继承 |
| D32-7 | resumeTasksForProvider 无生产调用点，注释宣称的接线不存在 | P3 | ❌ 新增 | quota_keeper.go:507-528；grep 仅 quota_keeper_test.go:765/777 | 继承 |
| D32-8 | unflagWorkBuddyMessages 的 string 分支丢带 tool_calls 的 assistant 轮 | P3 | ❌ 新增 | workbuddy.go:861-871 vs :878-887（parts 分支有例外、string 分支没有） | 继承 |

### M5 计费/用量/日志

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| M-19 | 错误响应被估算计费 | P1 | ✅ | 已修 | 继承 |
| N11-1 | 长流 usage 归零→估算放大 115× | P0 | ✅ | streamUsageReader 逐字节折叠 | 继承 |
| N12-2 | 日桶用服务器时区 | P2 | ⚪ | 生产 unit TZ=Asia/Shanghai（/proc 实测） | 继承 |
| N13-2 | /__usage-analysis 无上限 | P2 | ❌ | 无 limit/分页；items 全量驻留 + 每页 COUNT(*) + 深 OFFSET；FlushRequestLogs 普通用户可反复触发 | 继承 |
| N23-5 | stream+JSON 回包 usage=0 | P2 | ❌ | 症状更正：usage.go:535-544 在 fold!=nil 时永不回落 parseUsage ⇒ server.go:1367-1369 走 EstimateTokenUsage(len/4) 按真钱入账 | 继承 |
| W29-9 | streamUsageFolder reflect 分派脆 | P3 | ❌ | 代码 | 继承 |
| D31-2 | 监控中心不可停止：StartMonitoring(context.Background()) 无 cancel，Stop 直接 db.Close() | **P2** | ❌ 新增 | app/runtime.go:353；monitoring/service.go:38-51 ctx 永不取消 | 继承 |
| D31-3 | 引擎吞事件写库错误（状态 firing、事件缺行） | P2 | ❌ 新增 | monitoring/engine.go:315-324（:322 `id, _ :=`）；dispatcher.go:274-285 对 ID==0 no-op ⇒ 推送与提醒全丢 | 继承 |
| D31-8 | checkin_history 无 account_key（同日换号覆盖）且 ListCheckinHistory 零调用 | **P3** | ❌ 新增 | store/checkin.go:57-65 PK(provider_id,day)；store/monitoring.go:824 另有零调用、不在事务内的第二套写函数 | 继承 |

### M6 安全/鉴权/隔离

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| S1 | 管理 API `ACAO: *` | P0 | ✅ | cors 白名单 | 继承 |
| S2 | 请求体无上限 | P1 | ✅ | request_body_limit | 继承 |
| S3 | 登录/setup 无限流 | P1 | 🟡 | auth_throttle 死代码/TOCTOU | E2 重看 `admin_auth.go:494-500`：解码仍在限流前、bcrypt 已在限流后 |
| S4 | 日志正文不脱敏 | P1 | ✅ | log_scrub | 继承 |
| S5 | 首包后无空闲超时 | P1 | 🟡 | 守卫有，但心跳被 W29-3 缓冲吞掉 ⇒ 长静默仍可能被 CF 掐 | 继承 |
| H-8 | 管理员 Key 链不过滤 | P1 | ❌ | user_isolation.go:123-125 OwnerUserID=="" 直接 return chain；调用点 provider_failover.go:1094/1492 对 admin 恒 no-op | E2 重看 `user_isolation.go:123-125` |
| N12-3 | 登录先解码后限流 + 全表扫 | P3 | 🟡 | 部分 | 继承 |
| W30-3 | /__monitor 路由无 CSRF/Origin 校验 | P2 | ❌ | monitoring_handlers.go:118-127 全部 requireAdmin；cors.go:61-81 只写响应头不拒请求；admin_auth.go:326-333 回环免鉴权 | 继承 |

### M7 存储/迁移

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| W30-2 | 生产库已建 monitor_* 六表但全空（migration 跑了、采集未跑） | P3 | 新增 | 生产只读：6 表存在，5 表 0 行、checkin_history 18 行；二进制 grep -a 无 monitoring 串 ⇒ 采集未跑（非采集故障） | 继承 |

### M8 前端控制台

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| C1 | 控制台 6 项（回放 UI/sessionId 展示/未读角标/成本列/事件流重放/失败重发）全未实现 | P2 | ❌ 新增（补记账） | grep -rl：EventSource=0、replay=0、costUsd=0、resend=0；sessionId 仅 types.ts:679 类型字段；badge 6 处均为 CSS 类名 | 继承 |

### M9 隧道/系统面

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| N13-1 | `gateway version` 零 flag 解析会起服务 | P2 | ❌ | 复现 | 继承 |
| N13-3 | 容器无 TZ 且 verify 脚本期望 LA | P3 | ❌ | 未改 | 继承 |
| D32-4 | 隧道 token 明文落 cloudflared.log（0644，含 --token 值） | P1 | ❌ 新增 | tunnel/manager.go:300-301 写 strings.Join(args," ")（argv 含 --token，:653），:343 以 0644 建文件 | E2 重看 `tunnel/manager.go:300-301/:343`（argv 含 `--token`，0644） |

### M0 基线与卫生

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| W29-4 | 红测试 TestConverterStreamWriterKeepsTheStatusItWasGiven | P1 | ✅ | 该测试本机绿（r30 起） | 继承（本轮四包 0 红佐证） |
| W29-5 | internal/monitoring 整包未接线 | P2 | ✅ | 已随 2045543 固化：runtime.go:194/353 + 18 条路由 | 继承 |
| W30-1 | zz_quota_keeper_e2e_hooks.go 生产源码里的测试钩子 | P3 | ❌ | `git ls-files` 本轮复跑仍命中该文件 | **r34 复验**：仍在仓内 |
| D31-7 | 提交卫生：.superpowers / 3.9k 行文档 / 128 文件单提交 | P3 | ❌ | `.superpowers/brainstorm/43832-1792745803/{.server.pid,.server-stopped,*.html}` 5 文件仍跟踪 | **r34 复验**：仍在仓内 |

**账本覆盖校验（脚本对 `LEDGER.md` 逐行比对第六节）：ledger=97，missing=[]（全覆盖）。** 首轮生成时有 4 条未被归组（`W29-4`/`W29-5`/`W30-1`/`D31-7`，均属 M0 卫生），已补入上方「M0 基线与卫生」表；本轮新增 `N34-1` 记入 M1，合计 98 行。

---

## 七 回退与假修复

### 7.1 `回归` 项

**本轮无回归。** 逐项与 round33 的状态列比对：97 条账本行中，状态发生变化者仅 1 条且方向为正向（`W29-6` 🟡→✅，代码位置 `stream_end_reader.go:780-795` 的尾部封顶；HEAD 与 r33 相同，该封顶在 r33 时已存在，本轮只是把长期挂"未核查"的判定落地）。其余 96 条状态逐字未变。

> 说明：无新提交、无 WIP ⇒ 逻辑上不存在"改动导致回退"的可能；本轮不存在"连续两轮同一项回归"的升级条件。

### 7.2 `关联回退嫌疑` 项

无（同上理由）。

### 7.3 假修复 / 自述与实现冲突（持续挂账，本轮复验）

| 编号 | 自述 | 实现 | 判定 |
|---|---|---|---|
| W29-1 | `writePassThroughResponse` 注释写「Otherwise the first byte goes straight through」 | 拦截条件 `:278` 只看 `*failoverResponseWriter` + `stream` + `status<400`，**不看 `canDefer`** ⇒ 所有网关 Key 的透传流都进 `writeBufferedStream`，PROBE-34 实测 canDefer 真假都是 `writes=1`、首字节=总时长 | **假修复/注释与实现相反**，P0 第 14 轮 |
| D31-1 | `quota_keeper.go:481-482` 注释「a person's deliberate pause on a healthy task is never undone here」 | `:485-489` 判据是 `len(live)==0` 才 return false ⇒ 有可用 Provider 就复活，而人工暂停的恰恰是健康任务 | **注释与实现相反** |
| D29-3 | `laneFor` 注释「sized to the current configuration」 | `:124-139` 每次 acquire 都用**本次调用方**的 `max/rpm` 覆盖整组 lane ⇒ 组内限额取决于最后一个到达的请求行 | **组限额来源不唯一** |
| H-1 / H-2 | 两条 Chat→Claude 路径对同一语义给出互斥实现 | `convert_response_format.go:22-23` 把 `json_object` 原样发；同仓 `convert_responses_fields.go:73-80` 注释明说 Claude 无此模式并降级 | **必有一错，且错的一侧是硬 400** |
| H-6 / N34-1 | `claude_thinking_off.go:20-22` 注释「未知模型一律原样透传，不猜」 | 型号名不匹配正则时 `known=false`，但 `normalizeClaudeEndpointThinkingOffBody:270-277` 的行为是**删字段 + 抬预算**，不是原样透传 | **注释与实现相反（本轮新增实证）** |

---

## 八 下轮验收门槛（可勾、可机械验证）

> 每条都给出可执行命令或可比对读数；任何一条给不出证据即视为未达标。**顺序即优先级：先 1，再 2，然后 3。**

- [ ] **8.1 线上可追溯（P0 治理）**：`ssh root@173.249.207.202 'grep -a -o "vcs.modified=[a-z]*" /proc/$(systemctl show -p MainPID --value llm-protocol-gateway)/exe'` 输出 `vcs.modified=false`，且 `vcs.revision` 等于 `git rev-parse HEAD`。连续 13 轮为 `true`，这是所有"已修复"结论无法落地的前提。
- [ ] **8.2 P0 W29-1**：`go test ./internal/gateway/ -run TestZZProbe34StreamFirstByte -v`（探针源码见 `.workbuddy/reviews/round34-evidence/zz_probe34_test.go.txt`，用过即删）在 `canDefer=true` 与 `false` 两种情形下均为 **`writes>=3` 且 `firstWrite <= 500ms`**（三块各 400ms）。当前读数：`1.2010–1.2016s / writes=1`（两种都；两次运行读数见 `round34-evidence/PROBE-34.log`）。
- [ ] **8.3 W29-2**：同一次运行里，`fw.body` 长度不得随流长线性增长；或 `StreamLen()`/`Prefix()`/`streamClosed()` 三个方法至少有一个进入生产调用图（`grep -rn "\.StreamLen()" internal/ --include=*.go | grep -v _test` 非空）。
- [ ] **8.4 W29-3 / N25-5**：在 8.2 的探针 writer 上统计 `Flush()` 次数，15s 级心跳下 `flushes >= 1`（当前 `flushes=0`，心跳全落缓冲）。
- [ ] **8.5 H-1/H-2**：`grep -n 'format\["name"\]\|case "json_object"' internal/gateway/convert_response_format.go` 与 `convert_responses_fields.go:71-80` 必须收敛到**同一套**实现（要么都发 name、要么都不发；`json_object` 要么都降级、要么都直传），并附一条对真实 Claude 上游的 200 回放。
- [ ] **8.6 H-3**：`convert_claude_openai.go:322-325` 的 `applyAdaptiveThinking` 改为 merge 而非整块赋值；验收：构造 `{"response_format":{...}} + {"reasoning_effort":"high"}`，断言转换结果里 `output_config` **同时**含 `format` 与 `effort`（写一条 `zz_*_test.go` 断言即可）。
- [ ] **8.7 H-5**：`convert_claude_cache_marks.go:93-101` 的 `claudeCacheableBlock` 需覆盖 `tool_result`（或改为对 magpie `anthropic.go:921-928` 的黑名单语义）；验收：一个"最后一条是 tool_result"的 Messages 请求，`cache_control` 断点数 >0。
- [ ] **8.8 H-7**：`server.go:6255` 的 `router.ProviderByID(route.ProviderID)` 改为 `decision.ProviderID`；验收：同一 route 下两个 provider（一个 Disabled、一个支持 images）时，请求被拒的判定必须落在 decision 上。
- [ ] **8.9 H-8**：`user_isolation.go:123-125` 的 `OwnerUserID==""` 分支不能直接 `return chain`；验收：管理员 Key + 链中含 disabled/deleted/协议不匹配 provider 时，该 provider 不出现在最终链（写 `zz_*_test.go` 断言）。
- [ ] **8.10 D32-1**：`grep -rn "chatStopToClaudeSequences" internal/ --include=*.go | grep -v _test | wc -l` 必须 **≥2**（定义 + 至少 1 个调用点）；验收：Chat 请求带 `stop:["\n\nHuman:"]`，转换结果含 `stop_sequences`。
- [ ] **8.11 D29-2**：`grep -n "retryAfterSeconds(" internal/gateway/server.go` 命中 0（全部改 `retryAfterSecondsFor`）；当前 `:4492` 仍命中。
- [ ] **8.12 D29-3**：`laneFor` 不得用调用方参数覆盖已存在 lane 的 `max/rpm/queueCap`；验收：组内两行 `max=2` 与 `max=10`，交替 acquire ⇒ `providerInFlight` 峰值 ≤2。
- [ ] **8.13 D29-4**：`acquireProviderSlot:556-561` 的 `max<=0 && rpm<=0` 分支必须先解析组内其它行；验收：同一 laneGroup 内一行 0/0 + 一行 2/2，仅打 0/0 那行也必须被限到 2。
- [ ] **8.14 D29-6**：`providerInFlight` 必须用 `providerLaneKey(provider)`；验收：分组 Provider 的 in-flight 在控制台非 0。
- [ ] **8.15 D31-1**：`reviveScheduledTask` 必须能区分"人工暂停"与"全部成员被删"（例如新增 `paused_by_user` 或比对 `paused_at`）；验收：控制台手动暂停一个健康任务，跨过一个 keeper 周期后 `enabled` 仍为 0。
- [ ] **8.16 D31-4**：`oauth_usage_cache` 的 key 必须含账号身份（`oauth_user_id`/`oauth_account_label` 或 token 指纹）；验收：同一 provider 换账号后 1 分钟内面板数字来自新账号（探针可直接断言 key 字符串）。
- [ ] **8.17 D32-4**：`tunnel/manager.go:300-301` 落盘前必须对 argv 中的 `--token` 值打码，`:343` 的文件权限改 `0o600`；验收：`ls -l` 为 `-rw-------`，日志行含 `--token ***`。
- [ ] **8.18 N34-1**：`claudeModelLine` 需覆盖 `claude-3-5-sonnet-*` / `claude-3.5-sonnet` / `anthropic/…` 前缀三种写法（或改为"含 claude 字样但未匹配 ⇒ known=false 但**不改写**"）；验收：PROBE-34H 输出 `known=true`，且 200-token 请求的 `max_tokens` 保持 200。
- [ ] **8.19 卫生**：`git ls-files | grep -E "zz_|\.superpowers"` 输出为空；`gofmt -l` 在 LF 检出下为空。
- [ ] **8.20 报告纪律**：下一轮若仍是零增量，第八节沿用本节并只更新实测读数；若声称修好任何 P0/P1，必须给出 E1（运行时 A/B）或 E2+E3，**不接受"函数存在/注释写了/测试名是绿的"**。

---

## 附 覆盖度声明

| 模块 | 已扫文件数 / 该模块总数 | 本轮实际核查手段 | 未核查项 |
|---|---|---|---|
| M0 基线与卫生 | 全仓（build/vet/test 三件套覆盖全部包；卫生用 `git ls-files`/`gofmt -l`/`grep println`） | 命令 | 无 |
| M1 协议转换·请求方向 | 13 / 336（gateway 包内 `convert_*.go` 与 `claude_thinking_off.go`/`claude_pass_through_normalize.go`/`claude_cache_marks.go` 的相关函数） | E1 探针（PROBE-34H）+ E2 行号 | H-6 的 glm-4.x `reasoning_effort=none` 是否被上游接受（无上游实证）；D32-2 声明名回填仅 r32 结论 |
| M2 协议转换·流式 | 9 / 336（`passthrough_stream.go`、`provider_failover.go` 写侧、`stream_end_reader.go`、`sse_heartbeat.go`、`usage.go` 流内折叠） | **E1 探针（PROBE-34）** + E2 | N25-3、W29-7 未复跑（P2，标记 未核查） |
| M3 调度/路由/并发 | 5 / 336（`provider_lanes.go`、`provider_failover.go`、`protocol_proxy.go` 出口、`provider_cooldown*`） | E2 行号 + grep 调用点 | D29-5 大小写、D29-7/D29-8（P2/P3，继承） |
| M4 Provider/账号/OAuth/额度 | 8 / 336（`claude_oauth*`、`chatgpt_oauth*`、`oauth_usage_*`、`workbuddy*`、`quota_keeper.go`） | E2 行号 | D31-6 负缓存读侧、D32-5/7/8（P2/P3，继承 r33） |
| M5 计费/用量/日志 | 4 / 336 + `internal/store`（28 文件）关键路径 | E4 生产只读 | N23-2/3、N25-3（P2，未核查） |
| M6 安全/鉴权/隔离 | 5 / 336（`admin_auth.go`、`auth_throttle.go`、`access_source.go`、`user_isolation.go`、`cors.go`） | E2 行号 | N12-3 全表扫未复跑 |
| M7 存储/迁移 | `internal/store` 28 文件中 6 个（`sqlite.go`/`monitoring*.go`/`checkin.go` 的列与迁移） | E4 生产 DB schema 只读 | 迁移执行顺序未复跑 |
| M8 前端控制台 | 0 / 19 tsx | 仅 `grep` 计数复核（C1 六项仍 0） | **未核查几何/时区**（时间盒优先 P0/P1，按规则顺延） |
| M9 隧道/系统面 | 1 / 4（`tunnel/manager.go` 的 token 与日志落盘段） | E2 行号 | 隧道只读面其余未扫（未触发真实隧道） |
| magpie 对照 | 只读参考：`internal/gateway/{gateway.go,anthropic.go,responses.go,chat.go}` 的相关函数 | 与历史结论一致，本轮未新增差异 | — |

**未核查清单（必须显式列出，永不等于通过）**：N25-3、W29-7、N23-2/3、D31-6、D32-2、D32-5、D32-7、D32-8、D29-5、D29-7、D29-8、H-6 的 glm-4.x 子项、M8 全部控制台几何/时区验收。

**⚪ 不适用占比**：97 条中仅 `N12-2` 一条（1.0%），远低于 30% 阈值，且有 `/proc` TZ 实测举证。

---

## 给修复 agent 的订单（最小必要改动，按级别）

### P0（不做则整轮无意义，按顺序）

1. **让线上可追溯**：把当前工作区（或任意一份要上线的代码）打成 commit 再部署，部署后 `vcs.modified` 必须为 `false`。这不是代码改动，是流程改动；没有它，下面每一条"修好了"都无法证明。
2. **W29-1 透传流不要在 `failoverResponseWriter` 上无条件缓冲**：`passthrough_stream.go:278` 的拦截条件必须只在"真的还能改投备份"（`canDefer && 还有后备`）时成立；其余情形走 `:288` 之后的直通分支并**先 `WriteHeader(statusCode)` 再 `io.Copy`**。验收 8.2。
   - 若保留缓冲分支（真的需要"客户端零字节时可改投"），则必须给缓冲设上限并在首块到达即提交，否则 TTFT 永远等于总时长。
3. **W29-2/W29-3 随之收敛**：`provider_failover.go:358` 的 `w.body.Write` 只有在"需要记录已送达前缀"时才写，且要有上限；`passthrough_stream.go:554` 的心跳必须挂在**已提交的 writer** 上。验收 8.3/8.4。

### P1

4. H-1/H-2 二选一收敛（8.5）；H-3 改 merge（8.6）；H-5 覆盖 `tool_result`（8.7）；H-7 用 `decision.ProviderID`（8.8）；H-8 管理员链也要过滤（8.9）；D32-1 接上 `chatStopToClaudeSequences`（8.10）。
5. D29-2 最后一处出口（8.11）；D29-3 组限额来源唯一化（8.12）；D29-4 0/0 行先解析组（8.13）；D29-6 用 lane 键（8.14）。
6. D31-1 增加"人工暂停"可判据（8.15）；D31-4 缓存键加账号维度（8.16）。
7. D32-4 日志脱敏 + `0o600`（8.17）。

### P2 / P3

8. N34-1 型号名正则补齐或改为"未知即不改写"（8.18）。
9. 卫生：移出 `zz_quota_keeper_e2e_hooks.go` 与 `.superpowers/**`（8.19）。
10. 控制台 C1 六项：本轮仍 0 实现，无截止期的前提下继续挂账，不占本轮订单优先级。

---

## 收尾自检

- [x] `git status --porcelain` 只剩 `.workbuddy/reviews/` 产物（实测输出为空，探针删除后复核）
- [x] 无临时 worktree 残留（`git worktree list` 仅主树 `C:/guodevelop/llm-protocol-gateway 2045543`）
- [x] 账本已更新，且账本每个未闭环 ID 都在第六节出现（脚本校验 `missing=[]`，97/97 + 新增 N34-1 共 98 行）
- [x] 每个 ✅ 都能指到 E1/E2/E3 之一（本轮变更为 ✅ 的 `W29-6` 为 E2）
- [x] 覆盖度声明已填，`未核查` 项已逐条列举（13 项）
- [x] 无 commit / push / 生产写操作（生产仅 `systemctl is-active`/`ls`/`sha256sum`/`grep -a`/`file:…?mode=ro` 查询）
