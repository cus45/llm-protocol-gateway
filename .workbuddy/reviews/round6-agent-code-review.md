# 第六轮 · 另一 Agent 改动代码审查（round6）

- 审查时间：2026-10-22 22:25–23:15
- 审查范围：`4157aea..16c00d3`，共 30 个提交（含审查进行中对方 agent 于 22:56 提交的 `16c00d3`）
- 对照基准：`C:\guodevelop\demo\magpie`（@4a87c447）
- 基线：`go build ./...`、`go test ./internal/gateway/...` 全绿（37.4s）；工作区干净
- 说明：`round5-agent-code-review.md`（19:58）是另一路未记录的中间审查（针对当时的出图 WIP），本报告为第五次排程审查，为避免覆盖其内容存为 round6
- 方法：四路并行子审查（协议转换 / 流式+thinking / 安全 / Provider+图片+WIP）+ 本人逐一抽验全部高优先级结论（关键项均亲读代码核对）

---

## 结论先说

1. **上轮头号遗留「stop 发进 Responses」已修复且修复彻底**（219ab9c，本地回填 + 截断守卫 + 3 个真实测试）。
2. **上轮 P0「stop_sequence 回填覆盖 tool_use/max_tokens」只修了一半**：Responses 方向有守卫了，但 **Claude→Chat 方向（非流式+流式双路径）仍无保护**，且 9b62841 配套的 5 个测试全部无效（删掉新逻辑照样通过）。
3. 未提交 WIP 曾存在 **P0 级回归**（等锁者重复花新 refresh token，8 并发调 8 次），对方 agent 在提交前自己改掉了，`16c00d3` 最终版逻辑正确、测试全绿——但锁表实现仍留 3 处瑕疵。
4. 本轮新发现 **9 个高危问题**，集中在两处：a5de21a（结构化输出搬运）会把 name/json_object 发给 Claude 必 400、且 format 会被 thinking 覆盖丢失；thinking 关闭五连提交存在「预算补正生产不可达」「glm 回退 460dcec」「未知 Claude 模型 thinking 被删」三个实质缺陷。
5. 调度类（R1–R6）与控制台（web/）**本轮零进展**，仍是清单上最大的整块缺口。

---

## 一、上轮遗留问题复检（每次必查）

| # | 上轮遗留 | 状态 | 核对结果 |
|---|---|---|---|
| 1 | **头号遗留：stop 发进 Responses 上游** | ✅ **已修**（219ab9c） | 两个方向转换器均不再写 stop，改 `rememberConvertedStopSequences` 上下文 + 回包本地匹配回填；补「仅 status=completed 才回填、截断不翻 completed」守卫。全仓 grep 无回归，本人核对 diff 与当前代码 |
| 2 | **Round-4 P0：stop_sequence 回填无条件覆盖** | ⚠️ **半修** | Responses 方向已加守卫（`stop_sequence_fill.go:78-92`）✅。**Claude→Chat 仍无保护**：`convert_claude_openai.go:482-485`（非流式）与 `convert_claude_openai_stream.go:235-238`（流式）仍无条件覆盖 finish_reason="stop"——「正文恰以序列结尾 + max_tokens 截断 / tool_use」被误报为正常结束，与自家守卫原则矛盾 |
| 3 | **4157aea「限 4 条」偏离 magpie** | ✅ 基本缓解 | sanitize 仅作用于非 OAuth 直连上游，注释明确「Anthropic 自身上限也是 4」；>4 静默保留前 4（Anthropic 会 400），取舍已文档化。降为观察项 |
| 4 | **RPM 限流（R1）** | ❌ 未动 | 全仓无任何 limiter 实现；R1–R6 零提交 |
| 5 | redacted_thinking 占位 / 迟到 reasoning | ❌ 未动 | 本轮无一提交涉及 |

---

## 二、逐提交结论（30 提交）

**正确（含真实测试）**：219ab9c（stop→Responses）、628582b（转发头可信代理，本轮质量最高）、452edc0（CORS 默认拒绝）、e20177a（正文上限，中间件全局兜底）、8972366（额度主机名匹配）、a55c35d（工具前前言保留）、1ad2f50（incomplete 收尾）、9a0965c（终态 cached_tokens）、460dcec（去 none）、597e96a（并行工具 block）、7e4aab1（prompt_cache_key/service_tier）、81a5b59（text.format，name 不兜底为低危）、f926fac（工具失败标记）、571d252（日志留原文）、2498433（refresh 串行）、16c00d3（见下）。

**部分正确/有遗留**：b5da0bb（setup 限流死代码）、04dd81b（只脱敏请求侧）、f995154（TOCTOU + 判定面窄）、9b62841（逻辑=与既有非流式一致，但共享 M-1 缺陷，测试全无效）、a5de21a（三处高危）、0a78a76（两处高危三处中危）、f289d91（主线场景失效）、49bd261/cd90176/91f4155/d4f2b77（三个高危）、d8f843d/4c7bed9/e650076（检查对象错位）。

### WIP → 16c00d3 复核（重点）
子审查 22:53 读到的 WIP 版本存在 P0 回归：等锁者拿到路由新凭证后，「令牌已换人」时仍拿新 token 再花一次——8 并发实测 refresh 调 8 次（应 1 次），恰是 A1 要消灭的场景；当时的 `oauth_renew_reread_test.go` 还把错误行为固化成期望。**对方 agent 提交（22:56）前改掉了**：最终逻辑「fresh 不再过期→早退直接用；fresh 仍过期→才用 fresh 再续」，全量测试绿，P0 消除 ✅。
遗留：M-16 锁表 `entry.last` 只在创建时赋值，sweep 实为「创建超 10 分钟即回收」而非空闲回收；M-17 `TryLock` 可在有等待者时成功 → 删表项后互斥降级；M-18 `retireOauthRenewLock` 在 persist 之前调用，存在小窗口双花。**A5 仍未真正修复**：Cursor 读侧无提前量且解析失败 fail-open，5 分钟偏移只在 JWT 写入侧。

---

## 三、本轮新发现问题（按严重度，本人已抽验全部高危项）

### 高
| # | 提交 | 位置 | 问题 |
|---|---|---|---|
| H-1 | a5de21a | `convert_response_format.go:40-44` | 向 Claude `output_config.format` 写 `name` → 必 400「Extra inputs are not permitted」；与本仓 `convert_responses_fields.go:71-72` 已固化约束冲突；Chat 的 name 必填 → 命中率≈100% |
| H-2 | a5de21a | `convert_response_format.go:19-20` | `json_object` 原样发 Claude；Claude 只认 json_schema，应降级宽松 schema（同仓 Responses→Claude 已这么做） |
| H-3 | a5de21a | `convert_claude_openai.go:207-213`+`:323` | `applyOpenAIThinkingToClaudeRequest` 在 adaptive 分支**整体替换** output_config（本人 grep 确认 :323 整体赋值）→ Chat 带 reasoning_effort + adaptive 模型时 format 静默丢失；Responses→Claude 特意 merge-after-thinking，此路径没兜住；测试恰好绕开组合 |
| H-4 | 0a78a76 | `convert_responses_claude.go:496-509` | 给 Claude `tool_choice` 塞 Anthropic 未定义的 `allowed_tools` → 纯 400 风险零收益（narrowClaudeTools 已达成同效果） |
| H-5 | f289d91 | `convert_claude_cache_marks.go:93-105` | 可缓存白名单不含 `tool_result` → agent 多轮「最后一条是 tool 消息」的**主线场景一个断点都不加**，提交主旨失效；magpie 是黑名单制 |
| H-6 | thinking 五连 | `server.go:5439`→`claude_pass_through_normalize.go:161` | **「补思考余量」生产不可达**（本人核对调用链）：`rewriteClaudeUpstreamMaxTokens` 先把 max_tokens 无条件改成 ≥65536，其后 endpoint-thinking-off 的 shortBudget 恒 false；单测绕过该步骤所以全绿 |
| H-7 | 91f4155 | `claude_thinking_off.go:238-239` | 非 5.2/5.3 的 `glm-*` 返回 `reasoning_effort:"none"`，**直接回退 460dcec**（其提交自述「GLM 枚举只到 min，补 none 会被 400」）；GLM-4.6/5.0/5.1 仍中招 |
| H-8 | cd90176 | `claude_thinking_off.go:267-276` | `known==false` 被当「非 Claude」并删 thinking：`claude-3-7-sonnet-20250219`（正则失配）、`claude-sonnet-6`（未来代次）等真 Claude 的 thinking 被静默删；magpie 对 offUnknown 逐字节透传；违反本文件 `:22`「未知模型一律原样透传」自述 |
| H-9 | d8f843d/4c7bed9/e650076 | `server.go:5916-5944`+`user_isolation.go:385-406` | 出图「提前拒绝」检查 `route.ProviderID`，实际发往 `decision.ProviderID`；管理员/legacy Key（OwnerUserID==""）链完全不过滤 → 可落已禁用/已删除/非 OpenAI Chat 协议 Provider。本人已读两函数确认。附带：403 不写请求日志；报文回泄 route.ProviderID |

### 中（择全）
| # | 位置 | 问题 |
|---|---|---|
| M-1 | `convert_claude_openai.go:482-485` / `…_stream.go:235-238` | Claude→Chat stop_sequence 覆盖无守卫（上轮 P0 残留方向）；9b62841 的 5 个测试无一能区分新代码 |
| M-2 | `convert_claude_responses_stream.go:468-469` | `unstremittedArgs` 非前缀时整段重发 full → 客户端拼接出损坏 JSON；magpie 只在是前缀时补尾 + pickArgs 择优 |
| M-3 | `claude_thinking_off.go:306-312` | 8192 只当阈值，增量截断在 2048——d4f2b77「留足 8192」实际只补 2048 |
| M-5 | `claude_thinking_off.go:92,222` | 不处理 `thinking.type:"between_tools"`（magpie 覆盖）；漏一类关思考入口 |
| M-6 | `convert_claude_responses_stream.go:314-319` | payload `type` 优先于 SSE `event:` 行，`{"type":"response",…}` 形态终态不识别；同函数 usage 却无条件读 `chunk["response"]`，内部不一致 |
| M-7 | `convert_response_format.go:58-71` | Claude→Chat name 缺失不兜底 `"response"` → OpenAI 必填 name 400（同仓两处已有范式） |
| M-9 | `convert_tools.go:896` | 向任意 Chat 上游写非标准 `allow_tools`（OpenRouter 私有）→ OpenAI Unknown parameter 400 |
| M-10 | `convert_responses_allowed_tools.go:29-40,55-57` | 白名单只剩内置工具时整段跳过：不收窄不限模式，限制完全失效（magpie 语义相反）；mode 非法静默降级 auto |
| M-11 | `convert_responses_fields.go:181-186` | `any` 守卫丢 `disable_parallel_tool_use`；与 `convert_tools.go:860` 规则不一致；被自家 GLM 实测形状（any+disable 同场）推翻 |
| M-12 | `convert_claude_cache_marks.go:107-121` | 有客户端断点仍叠加可超 Anthropic 4 上限/违 ttl 顺序（magpie 有完整校验）；system/tools 断点仅在显式顶层 cache_control 时才加，普通请求拿不到 |
| M-13/14 | `convert_openai_responses_chat.go:661-676` / `convert_tools.go:1486-1495` | f926fac 未修的同源边：Responses→Chat 仍丢 `is_error`；Chat→Claude 不还原 `[tool_error]` 前缀 |
| M-8 | `admin_auth.go:436-481` | /__auth/setup 限流死代码：只查不记；提交说明「首设同样受保护」未实现 |
| M-15 | `admin_auth.go:441-459,665-677` | setup TOCTOU：无锁读 + setAdminPassword 无原子保护，全新公网实例并发首设谁先写谁拿管理员；「全新」判定面窄（清空 provider/账号即判全新） |
| M-16/17/18 | `oauth_renew_lock.go:48-58,52,67-74` | 锁表三瑕疵（见「二」）；retire 在 persist 前有小窗口 |
| M-19 | `server.go:1400-1406` | **响应正文与 ErrorDescription 未脱敏**：上游 4xx 回显凭据会落库并经控制台回显 |
| M-20 | `access_source.go:66` | 私网即「可信代理」在 Docker/K8s 过宽 → 容器部署 XFF 仍可伪造，限流/配额可绕过 |
| M-21 | `server.go:4146`、`responses_compact.go:201` 等 | 413 只覆盖 4 端点，images/compact/管理端超限仍回 400 |
| M-22 | `deepseek_usage.go:49-53` | 余额探测沿用 BaseURL scheme：`http://` 配置下 Bearer Key 明文外发 |
| M-23 | `server.go:5344-5347` | 以 OutputProtocol 拒绝出图误伤 openai_responses 路由（Codex 同 Key 出图被 502）；状态码/位置/豁免偏离本仓既有约定 |
| M-24 | `server.go:5376-5379` | Provider 无凭证一律 502，误伤 ComfyUI/SD-WebUI 免鉴权本地出图后端 |

### 低（择要）
- `trae.go:383` 临时 OAuth 回调仍回 `ACAO: *`（仅 127.0.0.1 随机端口，可利用性低）
- 脱敏漏网：裸 `token`/`key` 键、`openaiAPIKey` 类连续大写驼峰、`Bearer <JWT>` 无值规则；脱敏先于截断，cap==0 时白做全量解析（可放大 CPU）
- service_tier 缺 `ultrafast`/`scale`（magpie 把 ultrafast 映射 priority）
- `convert_claude_openai_stream.go:174` 无序列时 visibleText 无界累积（O(n²)，多轮未修）
- `claudeEphemeralMark` 共享 map 未 clone；`normalizeClaudeThinkingOffBody`、`writeTooLargeError` 死代码；413 不记日志；`isLocalAdminBypass` 依赖可伪造 Host 头（被 f995154 放大依赖）
- **假测试**：`TestClaudeToolChoiceWireShape`（:127）不调用任何生产代码的自证式测试，应删；`TestClaudeCacheMarksSkipThinkingTail` 未真正覆盖 thinking 跳过；`oauth_renew_reread_test.go` 4 用例全单协程
- `-race` 本机无 gcc 跑不了，oauth 锁改动建议 CI/VPS 补 `go test -race`

---

## 四、原始差距清单总账（60 项对照）

| 项 | 状态 | 说明 |
|---|---|---|
| P1 流式 usage=0 | ✅ | message_start 现取 usage（本人核对 :127-128） |
| P2 stop_reason 映射 | ✅ | 映射表 + 测试 |
| P3 Responses→Chat 正文清空 | ✅ | a55c35d |
| P4 流式 reasoning 丢弃 | ✅ | reasoning_content delta 已发（:884-891） |
| P5 Responses→Chat 忽略 incomplete | ✅ | 读上游 finish_reason；length/filter 处理 |
| P6/P7/P8 | ✅（第 1 轮） | 本轮无回归告警（未逐项复验） |
| P9 redacted_thinking/迟到 reasoning | ⚠️ | 第 1 轮遗留未动 |
| P10 字段转发 | ◐ | prompt_cache_key✅ stop_sequences✅(本地回填) 缓存断点◐(H-5/M-12) structured◐(H-1/2/3) top_k/metadata 未确认 |
| P11 单一 IR | ❌ | 预期内长期项 |
| R1 RPM 限流 | ❌ | 零提交 |
| R2 冷却不看 Retry-After | ❌ | |
| R3 billing 未分类 | ❌ | |
| R4 流中断不续传 | ❌ | |
| R5 粘性不持久化 | ❌ | |
| R6 闸门非每账号 | ❌ | |
| S1 CORS | ✅ | 残留 trae.go:383 |
| S2 body 上限 | ✅ | M-21 遗留 |
| S3 认证限流/setup | ◐ | M-8/M-15 遗留 |
| S4 正文脱敏 | ◐ | M-19 响应侧未脱敏 |
| S5 空闲超时 | ❌ | Timeout:0 未改 |
| A1 续期锁 | ✅ | M-16/17/18 遗留 |
| A2 主机名匹配 | ✅ | M-22 遗留 |
| A3 images 串 Key | ✅ | H-9/M-23/M-24 遗留 |
| A4 Claude 额度窗口/429 负缓存 | ❌ | |
| A5 Cursor 提前量 | ◐ | 写侧偏移 5min；读侧无提前量 + fail-open |
| 控制台 1–6 | ❌ | web/ 本轮零改动 |

---

## 五、建议下一批（按价值排序）

1. **修 H-1/H-2/H-3**：照抄本仓 `convert_responses_fields.go` 既有正确范式（不带 name、json_object 降级、merge-after-thinking），一个提交可全修。
2. **修 H-7/H-8**：glm none 回退是明确自我回退必须撤；unknown 透传对齐 magpie。H-6 需决策：endpoint-thinking-off 挪到 rewrite 之前，或接受仅 key override 短预算生效并删死逻辑。
3. **修 M-1**：Claude→Chat 双路径加与 Responses 一致的守卫（仅 stop_sequence/未知时覆盖），删 9b62841 无效测试、补能区分行为的新测试。
4. **修 H-4**（删 allowed_tools 字段）与 **H-5**（cache marks 白名单改黑名单制，对齐 magpie）。
5. **修 H-9**：imagesRouteRefusal 改查 `decision.ProviderID`，管理员 Key 链补过滤。
6. 调度批（R1–R3）零进展，是清单上最大整块缺口。
7. 删假测试；oauth 锁改动在 CI/VPS 补 `-race`。

---

*本轮未修改任何生产代码；仅审查、运行测试与撰写本报告。*
