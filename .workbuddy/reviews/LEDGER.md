# 审查账本（唯一事实源）

> **2026-10-24 12:40 +0800 重建**：`LEDGER.md` 随并发会话误删 `.workbuddy/` 一起丢失且无备份。
> 本轮依据 `round35-agent-code-review.md` 的完整排查清单（97 条）反推恢复全部历史编号，并以 r36/r37 的实证结论覆盖其状态；r36/r37 新增条目（D36-*、D37-*）一并录入。
> r3..r29 的报告与 round36/37 报告均在 `reviews/` 内可查，逐条依据可回溯。
> 规则：编号沿用既有前缀，不重编；每一行每轮都必须出现在报告第六节。
> 状态：✅ 已修复 / 🟡 部分修复 / ❌ 未修复 / ⚪ 不适用；附加标记：回归 / 新增 / 未核查。

| 编号 | 模块 | 一句话问题 | 首见轮次 | 级别 | 当前状态 | 最近验证轮次 | 依据 | 备注 |
|---|---|---|---|---|---|---|---|---|
| P1 | — | Claude→Chat 流式丢 prompt_tokens | r35 前 | P1 | ✅ | r35 | usage 折叠 | 继承（代码哈希未变，r29 结论） |
| P2 | — | stop_reason 全量映射缺项 | r35 前 | P1 | ✅ | r35 | 映射表 | 继承（代码哈希未变，r29 结论） |
| P3 | — | Chat→Responses 有 tool_calls 清空正文 | r35 前 | P1 | ✅ | r35 | 测试 | 继承（代码哈希未变，r29 结论） |
| P4 | — | Responses→Chat 丢 reasoning 增量 | r35 前 | P1 | ✅ | r35 | 测试 | 继承（代码哈希未变，r29 结论） |
| P5 | — | Responses→Chat 忽略 incomplete | r35 前 | P2 | ✅ | r35 | 非流 convert_openai_responses_chat.go:870-872 + convert_tools.go:1749-1760；流 convert_openai_responses_chat_stream.go:661-666→:525-536；stop_reason_mapping_test.go:90-136 可红 | 继承（代码哈希未变，r32 结论） |
| P6 | — | Chat→Claude block index 错乱 | r35 前 | P1 | ✅ | r35 | 测试 | 继承（代码哈希未变，r29 结论） |
| P7 | — | sanitizeAnthropicToolUseID 碰撞 | r35 前 | P1 | ✅ | r35 | 测试 | 继承（代码哈希未变，r29 结论） |
| P8 | — | Responses→Claude 缺合成 tool_result | r35 前 | P1 | ✅ | r35 | 测试 | 继承（代码哈希未变，r29 结论） |
| P9 | — | redacted_thinking 丢失（占位非回放） | r35 前 | P2 | 🟡 | r35 | convert_* | 继承（代码哈希未变，r29 结论） |
| P10 | — | strict/top_k/stop_sequences 等不转发 | r35 前 | P2 | 🟡 | r35 | convert_* | 继承（代码哈希未变，r29 结论） |
| P11 | — | 两两手写转换 vs magpie 单一 IR | r35 前 | P2 | ❌ | r35 | 架构未动 | 继承（代码哈希未变，r29 结论） |
| R1 | — | 无 RPM 限流 | r35 前 | P1 | ✅ | r35 | 8b5542d 滑动窗口 | 继承（代码哈希未变，r21 结论） |
| R2 | — | 冷却不看上游 Retry-After | r35 前 | P1 | ✅ | r35 | 阶梯可达 | 继承（代码哈希未变，r26 结论） |
| R3 | — | billing 归入 hardQuota | r35 前 | P1 | ✅ | r35 | 代码 | 继承（代码哈希未变，r8 结论） |
| R4 | — | 流截断不能续/被当正常结束 | r35 前 | P0 | ✅ | r35 | 6 处读后判定 + 收尾改协议错误帧，测试绿 | 继承（代码哈希未变，r31 结论） |
| R5 | — | 会话粘性不落库 | r35 前 | P1 | ✅ | r35 | session_pins | 继承（代码哈希未变，r24 结论） |
| R6 | — | 并发闸门非 FIFO | r35 前 | P1 | ✅ | r36 | 票据队列 + laneGroup 共享额度 |  |
| S1 | — | 管理 API `ACAO: *` | r35 前 | P0 | ✅ | r35 | cors 白名单 | 继承（代码哈希未变，r24 结论） |
| S2 | — | 请求体无上限 | r35 前 | P1 | ✅ | r35 | request_body_limit | 继承（代码哈希未变，r24 结论） |
| S3 | — | 登录/setup 无限流 | r35 前 | P1 | ✅ | r36 | 限流在口令校验前 |  |
| S4 | — | 日志正文不脱敏 | r35 前 | P1 | ✅ | r35 | log_scrub | 继承（代码哈希未变，r18 结论） |
| S5 | — | 首包后无空闲超时 | r35 前 | P1 | 🟡 | r35 | 守卫有，但心跳被 W29-3 缓冲吞掉 | 同 W29-3 同因（E2），结论不变 |
| A1 | — | Trae/WorkBuddy/Cursor 刷新无锁 | r35 前 | P1 | ✅ | r36 | 五 provider 锁内重读 |  |
| A2 | — | 子串匹配 host | r35 前 | P1 | ✅ | r35 | baseURLHostIn | 继承（代码哈希未变，r24 结论） |
| A3 | — | /v1/images 路由误判 | r35 前 | P2 | ❌ | r35 | server.go:3099-3105 `resolveProviderImageURL` 用 `Contains("/images/generations")` | 重看 Contains 拼接（E2），结论不变 |
| A4 | — | Claude overage / 负缓存 | r35 前 | P1 | ✅ | r35 | 负缓存 5/5 | 继承（代码哈希未变，r26 结论） |
| A5 | — | Cursor 配额提前量 | r35 前 | P2 | ✅ | r35 | 纠错：已达标 | 继承（代码哈希未变，r10 结论） |
| A6 | — | 出图超时被 120s 砍 | r35 前 | P2 | ❌ | r35 | server.go:5712/5749 均 Timeout:0 | 继承（代码哈希未变，r32 结论） |
| H-1 | — | Claude output_config.format 是否该发 name | r35 前 | P1 | ✅ | r36 | 只发 {type,schema} |  |
| H-2 | — | `response_format=json_object` 原样直传 Claude | r35 前 | P1 | ✅ | r36 | 降级为宽松 object schema |  |
| H-3 | — | thinking 覆盖 output_config | r35 前 | P1 | ✅ | r36 | applyAdaptiveThinking 合并 |  |
| H-4 | — | Responses allowed_tools 塞进 Claude tool_choice | r35 前 | P1 | 🟡 | r35 | convert_responses_claude.go:496-516 构造该键并收窄 tools | 重看构造点（E2），结论不变 |
| H-5 | — | cache marks 白名单漏 tool_result | r35 前 | P1 | ✅ | r36 | 黑名单判定，tool_result 可打点 |  |
| H-6 | — | thinking 五连（预算补正/glm none/unknown 模型） | r35 前 | P1 | 🟡 | r35 | PROBE-34H（E1）：claude-3.5-sonnet 等 known=false → 删 thinking + max_tokens 200→2248 | 重看正则 + N34-1（E1 继承），结论不变 |
| H-7 | — | images 拒绝检查 route.ProviderID 而非 decision.ProviderID | r35 前 | P1 | ✅ | r36 | 改为 decision.ProviderID |  |
| H-8 | — | 管理员 Key 链不过滤 | r35 前 | P1 | ✅ | r36 | allowedProviderChainForKey 过滤 | r36 纠正误报 |
| H-9 | — | workbuddy 11128 校验矩阵 | r35 前 | P1 | ✅ | r35 | workbuddy.go:857 覆盖 system+assistant、:972-983 developer→system | 继承（代码哈希未变，r32 结论） |
| M-1 | — | Claude→Chat 无条件 stop | r35 前 | P1 | ✅ | r35 | 有条件守卫（r10 纠错） | 继承（已关闭，勿再报） |
| M-19 | — | 错误响应被估算计费 | r35 前 | P1 | ✅ | r35 | 已修 | 继承（代码哈希未变，r21 结论） |
| N-1 | — | workbuddy_ai 凭证 exp 空→永不续期 | r35 前 | P1 | ✅ | r36 | SignInAt 起算 |  |
| N10-1 | — | 透传 >256KB 流被判截断 | r35 前 | P0 | ✅ | r35 | 判据改为 `usage.closed()` 全字节 | 继承（代码哈希未变，r29 结论） |
| N10-2 | — | 中断收尾帧写裸 w 绕过锁 | r35 前 | P1 | ✅ | r35 | 改走 hw | 继承（代码哈希未变，r29 结论） |
| N11-1 | — | 长流 usage 归零→估算放大 115× | r35 前 | P0 | ✅ | r35 | streamUsageReader 逐字节折叠 | 继承（代码哈希未变，r29 结论） |
| N11-2 | — | 心跳 stop 不 join | r35 前 | P1 | ✅ | r35 | 有界 join 2s | 继承（代码哈希未变，r29 结论） |
| N11-3 | — | 透传 Claude 收尾漏 content_block_stop | r35 前 | P2 | ✅ | r35 | openBlocks 逐个 close | 继承（代码哈希未变，r29 结论） |
| N11-4 | — | 透传 Responses 收尾帧缺 id/model/seq | r35 前 | P2 | ✅ | r35 | responsesStreamIdentity | 继承（代码哈希未变，r29 结论） |
| N12-2 | — | 日桶用服务器时区 | r35 前 | P2 | ⚪ | r35 | 生产 unit TZ=Asia/Shanghai | 继承（已举证不适用） |
| N12-3 | — | 登录先解码后限流 + 全表扫 | r35 前 | P3 | 🟡 | r35 | 部分 | 继承（代码哈希未变，r18 结论） |
| N13-1 | — | `gateway version` 零 flag 解析会起服务 | r35 前 | P2 | ❌ | r35 | 复现 | 继承（代码哈希未变，r29 结论） |
| N13-2 | — | /__usage-analysis 无上限 | r35 前 | P2 | ❌ | r35 | server.go:339 路由，无 limit/分页 | 重看路由无上限（E2），结论不变 |
| N13-3 | — | 容器无 TZ 且 verify 脚本期望 LA | r35 前 | P3 | ❌ | r35 | 未改 | 继承（代码哈希未变，r13 结论） |
| N14-2 | — | 转换路径中断判定恒 false | r35 前 | P0 | ✅ | r35 | 读后判定 | 继承（代码哈希未变，r29 结论） |
| N23-1 | — | canDefer 在流式路径是死变量 | r35 前 | P1 | ✅ | r36 | 随 W29-1 一并消失 | r36 纠正误报 |
| N23-5 | — | stream+JSON 回包 usage=0 | r35 前 | P2 | ❌ | r35 | usage.go:535-544 fold!=nil 时永不回落 | 继承（代码哈希未变，r32 结论） |
| N24-3 | — | streamEndReader 只携带 4 字节 | r35 前 | P2 | ✅ | r35 | 按 maxClosingMarkerLen-1 | 继承（代码哈希未变，r29 结论） |
| N24-5 | — | 两套完成判定无回声守卫 | r35 前 | P3 | 🟡 | r35 | 回声守卫仅末 64B | 继承（代码哈希未变，r29 结论） |
| N25-1 | — | 中断谓词语义反了 | r35 前 | P1 | ✅ | r35 | 已在 isUpstreamStreamInterruption | 继承（代码哈希未变，r29 结论） |
| N25-3 | — | scanResponses 非 Responses 流全量累积 | r35 前 | P2 | 🟡 | r35 | 未复跑 | 继承（代码哈希未变，r30 结论） |
| N25-5 | — | 缓冲路径零心跳 | r35 前 | P2 | ❌ | r35 | 探针 flushes=0，心跳落 buf | 继承（并入 W29-3） |
| D29-1 | — | laneGroup 完全不落库 | r35 前 | P0 | ✅ | r35 | sqlite.go:239 列 + :641/:766 读写 + 两个测试绿 | 继承（代码哈希未变，r30 结论） |
| D29-2 | — | Retry-After 出口按行号查分组 | r35 前 | P1 | ✅ | r36 | 全部出口 retryAfterSecondsFor | r36 纠正误报 |
| D29-3 | — | 组内限额取"最后到达那行" | r35 前 | P1 | ✅ | r36 | laneForProvider 遍历成员取最紧 | r36 纠正误报 |
| D29-4 | — | 组内 0/0 行整组绕过 | r35 前 | P1 | ✅ | r36 | 0/0 成员不投票仍受组约束 | r36 纠正误报 |
| D29-5 | — | 大小写：注释说不敏感，实现精确匹配 | r35 前 | P2 | ❌ | r35 | providerLaneKey:542-547 只 TrimSpace | 重看键构造（E2），结论不变 |
| D29-6 | — | providerInFlight 按行号查 | r35 前 | P2 | ❌ | r35 | provider_lanes.go:565-578 用原始 providerID 取 lanes[] | 重看取 lane（E2），结论不变 |
| D29-7 | — | d93ec02 夹带 failover 行为变更 | r35 前 | P3 | ❌ | r35 | 提交粒度 | 继承（代码哈希未变，r29 结论） |
| D29-8 | — | lane 只增不删 | r35 前 | P3 | ❌ | r35 | 无回收 | 继承（代码哈希未变，r29 结论） |
| W29-1 | — | 透传流全量缓冲（TTFT=总时长） | r35 前 | P0 | ✅ | r36 | E2 passthrough 直写 base；Flush 直通 | r36 纠正：连续 15 轮误报 |
| W29-2 | — | passthrough 下响应再复制进 fw.body（无上限） | r35 前 | P1 | ❌ | r35 | provider_failover.go:359-362 无条件 `w.body.Write(p[:n])`；Prefix 仅测试调用、StreamLen/streamClosed 零调用 | 重看调用点（E2），结论不变 |
| W29-3 | — | 心跳进缓冲区，流中收不到 ping | r35 前 | P1 | ✅ | r36 | E1 转换路径 600ms 内 12 个 ping | r36 纠正误报 |
| W29-4 | — | 红测试 TestConverterStreamWriterKeepsTheStatusItWasGiven | r35 前 | P1 | ✅ | r35 | 该测试本机绿 | 继承（代码哈希未变，r30 结论） |
| W29-5 | — | internal/monitoring 整包未接线 | r35 前 | P2 | ✅ | r35 | 已随 2045543 固化 | 继承（代码哈希未变，r31 结论） |
| W29-6 | — | scanResponses 非 Responses 流全量累积 | r35 前 | P2 | ✅ | r35 | stream_end_reader.go:780-795 尾部封顶 | 继承（代码哈希未变，r34 结论） |
| W29-7 | — | 回声守卫只看末 64B | r35 前 | P2 | 🟡 | r35 | 代码 | 继承（代码哈希未变，r29 结论） |
| W29-9 | — | streamUsageFolder reflect 分派脆 | r35 前 | P3 | ❌ | r35 | 代码 | 继承（代码哈希未变，r29 结论） |
| W29-11 | — | failoverResponseWriter 死代码 | r35 前 | P3 | 🟡 | r35 | Prefix 仅测试调用 | 继承（代码哈希未变，r30 结论） |
| W30-1 | — | zz_quota_keeper_e2e_hooks.go 生产源码里的测试钩子 | r35 前 | P3 | ❌ | r35 | 文件仍在仓库，导出 HandleRestoreProviderForTest | 重看入仓文件（E2），结论不变 |
| W30-2 | — | 生产库已建 monitor_* 表但全空 | r35 前 | P3 | 新增 | r35 | 生产只读：4 张 monitor 表各 0 行；`/__monitor` 串=0 | 继承（r32 观察，r35 复测一致） |
| W30-3 | — | /__monitor 路由无 CSRF/Origin 校验 | r35 前 | P2 | ❌ | r35 | monitoring_handlers.go:118-127 全部 requireAdmin；cors 只写响应头 | 重看 monitorGate（E2），结论不变 |
| D31-1 | — | 用户暂停的额度保活任务被 keeper 每分钟复活 | r35 前 | P1 | ✅ | r36 | enabled := task.Enabled && … |  |
| D31-2 | — | 监控中心不可停止 | r35 前 | P2 | ❌ 新增 | r35 | app/runtime.go:353；monitoring/service.go:38-51 ctx 永不取消 | 继承（代码哈希未变，r32 结论） |
| D31-3 | — | 引擎吞事件写库错误 | r35 前 | P2 | ❌ 新增 | r35 | monitoring/engine.go:315-324 | 继承（代码哈希未变，r32 结论） |
| D31-4 | — | OAuth 用量缓存 key 不含账号身份 | r35 前 | P1 | ❌ 新增 | r35 | oauth_usage_refresh.go:187/194/234/284 `"claude:"+providerID`，无账号维度 | 重看缓存键（E2），结论不变 |
| D31-5 | — | ChatGPT 续期等锁后仍用旧快照 refresh_token | r35 前 | P1 | 🟡 新增 | r35 | chatgpt_oauth.go:433-436 重读仅用于早退；:437 仍花旧 RT | 重看花 RT 处（E2），结论不变 |
| D31-6 | — | 负缓存读侧死代码 | r35 前 | P2 | ❌ 新增 | r35 | oauth_usage_cache.go:64-72/116-131 | 继承（代码哈希未变，r32 结论） |
| D31-7 | — | 提交卫生：.superpowers / 大文档 / 128 文件单提交 | r35 前 | P3 | ❌ 新增 | r35 | `git ls-files .superpowers` 5 文件仍在 | git ls-files（E2），结论不变 |
| D31-8 | — | checkin_history 无 account_key | r35 前 | P3 | ❌ 新增 | r35 | store/checkin.go:57-65 PK(provider_id,day) | 继承（代码哈希未变，r32 结论） |
| D32-1 | — | Chat→Claude 完全丢弃 `stop`（chatStopToClaudeSequences 死代码） | r35 前 | P1 | ❌ 新增 | r35 | convert_tools.go:1165-1175 仅定义，全包 0 调用点 | 重看调用点（E2），结论不变 |
| D32-2 | — | Claude→OpenAI 回复路径不做 declared 名回填 | r35 前 | P2 | ❌ 新增 | r35 | convert_tools.go:1272-1311 不调 :1337 restoreDeclaredToolName | 继承（代码哈希未变，r32 结论） |
| D32-3 | — | thinking 预算与 max_tokens 无钳制 | r35 前 | P2 | ❌ 新增 | r35 | convert_claude_openai.go:325-331 写 16k/32k，:196 写 max_tokens，从不比较 | 继承（代码哈希未变，r32 结论） |
| D32-4 | — | 隧道 token 明文落 cloudflared.log（0644） | r35 前 | P1 | ❌ 新增 | r35 | tunnel/manager.go:300-301 写 `strings.Join(args," ")`（argv 含 `--token`）；:343 `0o644` | 重看 argv 落盘（E2），结论不变 |
| D32-5 | — | 出图按 base64 len/4 估算入账，价格表无图像行 | r35 前 | P2 | ❌ 新增 | r35 | server.go:5755 恒空 usage；`grep -i image pricing.go` 空 | 继承（代码哈希未变，r33 结论） |
| D32-6 | — | safety_identifier 全仓 0 命中 | r35 前 | P2 | ❌ 新增 | r35 | `grep -rn safety_identifier internal/` 空 | 继承（代码哈希未变，r33 结论） |
| D32-7 | — | resumeTasksForProvider 无生产调用点 | r35 前 | P3 | ❌ 新增 | r35 | quota_keeper.go:507-528，仅测试调用 | 继承（代码哈希未变，r33 结论） |
| D32-8 | — | unflagWorkBuddyMessages 的 string 分支丢 tool_calls 轮 | r35 前 | P3 | ❌ 新增 | r35 | workbuddy.go:861-871 vs :878-887 | 继承（代码哈希未变，r33 结论） |
| C1 | — | 控制台 6 项（回放/sessionId/角标/成本列/事件重放/失败重发）全未实现 | r35 前 | P2 | ❌ 新增 | r35 | 本轮 grep：`EventSource=0`、`replay=0`、`costUsd/cost_usd=0`、`resend/retryRequest=0` | 重跑 grep（E2），结论不变 |
| N34-1 | — | Claude 型号名正则漏 3.x 与厂商前缀写法 ⇒ 规则表失效并擅自放大 max_tokens | r35 前 | P2 | ❌ 新增 | r35 | claude_thinking_off.go:28 正则 `(?:claude-)?(opus\ | sonnet\ |
| D36-1 | M1 | allowed_tools 条目缺 type/name | r36 | P1 | ✅ | r36 | d0f3c97 + 红绿测试 |  |
| D36-2 | M3 | 出图不计入 lane；RPM 窗口受时钟回拨影响 | r36 | P1 | ✅ | r36 | 725e4ba + 红绿测试 |  |
| D36-3 | M3 | 出图被限回 502 而非 429 | r36 | P2 | ✅ | r36 | 33c43f5 + E1 |  |
| D36-5 | M5 | 会话视图缺失（sessionId 无处可见） | r36 | P2 | ✅ | r36 | 891fc0e + E1 |  |
| D36-9 | M3 | 出图无超时上限 | r36 | P2 | ✅ | r36 | 0c26215 + E3 |  |
| D36-10 | M3 | 闸门拒绝被当成上游故障冷却账号 | r36 | P1 | ✅ | r36 | 9311b94 + 红绿测试 |  |
| D36-11 | M3 | 组内排队等待只看本行 | r36 | P1 | ✅ | r36 | 9311b94 + 红绿测试 |  |
| D36-12 | M1 | Gemini 并行同名调用 tool id 冲突 | r36 | P1 | ✅ | r36 | 220b06f + 红绿测试 |  |
| D36-13 | M1 | Claude→Chat json_schema 缺 name | r36 | P1 | ✅ | r36 | 220b06f + 红绿测试 |  |
| D37-1 | M4 | 账号切换后旧账号冷却落到新账号 | r37 | P2 | ✅ | r37 | 147293d + 回归测试 | 复核后确为失败晚于切换 |
| D37-2 | M4 | WorkBuddy 过期无 refresh 静默发死令牌 | r37 | P3 | ✅ | r37 | da4bda3 + 回归测试 |  |
| D37-3 | M6 | Proxy-Authorization 头原样落库 | r37 | P1 | ✅ | r37 | f4e8b4a + 红绿测试 | 本轮唯一安全级 |
| D37-4 | M5 | 用量分析翻页漏数/重复 | r37 | P1 | ✅ | r37 | 0ce41c9 + 1200 行回归测试 |  |
| D37-5 | M5 | 脱敏漏厂商私有 opaque token | r37 | P3 | ✅ | r37 | 16b0fdb + 红绿测试 |  |
| D37-6 | M7 | LaneGroup 与三个限额持久化 | r37 | — | ✅ | r37 | 往返测试 + E1 探针 |  |
| D37-7 | M6 | 普通用户占用 admin 身份 | r37 | P2 | ✅ 不成立 | r37 | 50e77f3 双防线测试 |  |
| D37-8 | M4 | 负缓存冻结新账号 | r37 | P2 | ✅ 不成立 | r37 | bind 时已删条目，实测不冻结 |  |
| D37-9 | M5 | 缓存 token 重复计费 | r37 | — | ✅ 无问题 | r37 | E1 探针 660000µ$ 精确 |  |
| D37-10 | M6 | 管理端点仅靠中间件保护 | r37 | P2 | ✅ 契约化 | r37 | d1b0762 白名单契约测试 | 当前默认拒绝正确 |
| D37-11 | M7 | check-in 历史 API 被判死代码 | r37 | P3 | ✅ | r37 | 9361ef6 文档化归属 |  |
| D37-12 | M5 | 成本上限忽略未计价请求 | r37 | P3 | 🟡 | r37 | key_quota.go 文案已注明 |  |
| D37-13 | M7 | schemaVersion 只写不读 | r37 | P3 | 🟡 | r37 | 无降级检测 | 未核查 |
| M5-3 | M5 | 上游报 cache>input 时聚合层二次补加 | r37 | P3 | 未核查 | r37 | 目标文件在并发重构中已移动 | 需重新定位 |
| M4-4 | M4 | 续期锁键语义不一致 | r36 | P3 | 🟡 | r36 | 不影响功能 |  |
| M9-1 | M9 | UI 域名可访问 /openai/v1/models | r38 | P1 | ✅ 不成立 | r38 | a723f3a 契约测试：UI 域名 9 条模型路径全 404 | 复核纠正 |
| D38-1 | M9 | 并发 Start 启动两个 cloudflared | r38 | P2 | ✅ | r38 | a723f3a startMu 串行化三个入口 | |
| D38-2 | M9 | 测试桩仅 POSIX，Windows 恒失败 | r38 | P3 | ✅ | r38 | a723f3a 按 runtime.GOOS 分支，31 测试全过 | |
| D38-3 | M8 | 详情弹窗「会话」按钮重复两次 | r38 | P2 | ✅ | r38 | bf6009c Chrome 实测确认后删除重复块 | |
| D38-4 | M8 | 退出会话筛选只在空结果时可见 | r38 | P2 | ✅ | r38 | bf6009c 常驻筛选条 | |
| D38-5 | M8 | 未计价显示成真实金额 | r38 | P3 | ✅ | r38 | bf6009c 显式「未计价」 | |
| D38-6 | M8 | 自注册 Prompt 仍写「三选一」 | r38 | P3 | ✅ | r38 | bf6009c 改为四选一 | |
| D38-7 | M9 | 并发 Start / 域名绑定点 / hostMetrics 竞争 / 告警串行 / Restore 竞争 / fd 泄漏 / 主机名归一 | r38 | P2-P3 | 🟡 | r38 | 仅 Start 已修，其余未核查 | |
| D38-8 | M8 | 密钥页输出协议筛选无 gemini / 缓存 key 不含筛选 / SSE 摘要 | r38 | P3-P4 | 未核查 | r38 | | |
