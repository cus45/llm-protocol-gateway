# 第五轮 · 另一 agent 改动代码审查

审查范围：工作区未提交改动（出图授权选择）＋ 审计清单全部历史项复检 ＋ 本轮并发改动后的最终快照。
对照基准：`C:\guodevelop\demo\magpie`。
基线：`go build ./...`、`go vet ./...` 通过；`go test ./internal/gateway/...` 全绿；
`internal/tunnel` 5 个失败为 Windows 缺 `/bin/sh` 的环境问题，与本轮改动无关。
本轮未修改仓库任何代码，未提交。

---

## 结论先说

1. **未提交的出图授权改动不能验收**：授权重校验本身自洽、5 个单测全绿，但只做了「按 Key 链 ∩ 用户授权」这一半；
   A3（图片 URL 固定拼 `/images/generations`、全局 `OPENAI_API_KEY` 回退、把客户端网关 Key 当上游凭证）原样未动，
   且新增「链外 ActiveProviderID 被采用」的路由问题。测试还删掉了覆盖该洞的用例。
2. **历史清单进度**：已修 9 项（P1/P2/P3 原方向/P4/P6/P7/P8/A2/S1）；部分 10 项（P5/P9/P10/P11/R1/A1/S2/S3/S4/A5）；
   未修 9 项（R2/R3/R4/R5/R6/S5/A3/A4 及控制台主体）；第 5 批仅 Provider 导出导入/恢复。
3. **两个 P0 连续多轮未修**：Claude→Responses 仍把 `stop` 发给 Responses 上游（必 400）；
   `stop_sequence` 回填仍会把 `tool_use` / `max_tokens` 改写成 `stop_sequence`（工具不被执行、截断被伪装成正常结束）。
4. **新发现 4 个协议级回归**：incomplete 仍发 `response.completed` 事件；Responses→Chat 非流式仍丢前言；
   Responses→Claude 流式并行工具调用索引重复/参数丢失；Claude→Responses 最终 usage 缺 `cached_tokens`。
5. 仓库卫生：5 个 `.bak` 文件仍被 git 跟踪；新增的 `TestClaudeToolChoiceWireShape` 只测 `encoding/json`，无覆盖价值。

---

## 一、未提交改动逐文件判定

| 文件 | 改动 | 判定 |
|---|---|---|
| [server.go](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/server.go#L4190) | 命中网关 Key 时用 `imagesEffectiveProvider` 选 Provider，空则 403；否则走 `Decide` | **部分正确**：授权重校验、403 是对的；但模型仍按路由默认 Provider 解析（[server.go:4209](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/server.go#L4209)，实际 Provider 在 4224），alias/映射/日志会错位；仍未进 RPM/并发闸门；请求体在鉴权前 `io.ReadAll`（[server.go:4145](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/server.go#L4145)） |
| [user_isolation.go](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/user_isolation.go#L386) | 新增 `imagesEffectiveProvider`：链 ∩ 用户授权，优先 active | **部分正确**：∩ 授权与 chat 路径一致；但 `apiKeyProviderChain(key.ActiveProviderID, …)` 把 active 无条件放到链头（[389](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/user_isolation.go#L389)），链外 active 会被采用（[399-404](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/user_isolation.go#L399)）。chat 路径 `rotateChainAt` 对链外 active 保持原链（[provider_schedule.go:85](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/provider_schedule.go#L85)），注释「同 chat」不成立 |
| [images_routing_test.go](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/images_routing_test.go#L55) | 5 个纯函数用例 | **当前 5/5 PASS**，但早先快照有 2 个 FAIL；`HonoursOwnerGrants` 被删除（新语义与 chat 一致，可接受），`FollowsActiveProvider` 因实现改动转绿；**没有链外 active、disabled/deleted、403 响应、上游 URL/Authorization 的用例** |
| [convert_tool_restrictions_test.go](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_tool_restrictions_test.go#L125) | 新增 `TestClaudeToolChoiceWireShape` | **无效测试**：手写 JSON 的 Unmarshal→Marshal 往返，测的是 `encoding/json`，不经过任何网关代码；注释称「GLM 端点接受」，但本仓库无对应代码路径或实测记录 |

补充：`DecideForProvider` 只校验 route/provider 存在（[router.go:1952](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/router.go#L1952)），不查 `Disabled`、`route.Enabled`、图片端点挂载；出图没有 failover 兜底，管理员 Key 可命中禁用 Provider 直接 502。

---

## 二、完整排查清单

### 协议转换 P1–P11

| # | 状态 | 证据 | 说明 |
|---|---|---|---|
| P1 | ✅ 已修 | [convert_claude_openai_stream.go:120](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_claude_openai_stream.go#L120)、[202](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_claude_openai_stream.go#L202) | `message_start` 读 usage，`message_delta` 合并，最终输出 prompt/cache 计数 |
| P2 | ✅ 已修 | [convert_claude_openai.go:427](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_claude_openai.go#L427) | `refusal`/`pause_turn`→`stop`，`model_context_window_exceeded`→`length`，不再原样漏出 |
| P3 | ✅ 已修（原方向） | [convert_openai_responses_chat.go:916](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_openai_responses_chat.go#L916) | Chat→Responses 有 tool_calls 时保留前言；**反向 Responses→Chat 仍丢前言**，见新发现 N7 |
| P4 | ✅ 已修 | [convert_openai_responses_chat_stream.go:645](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_openai_responses_chat_stream.go#L645)、[888](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_openai_responses_chat_stream.go#L888) | reasoning 以 `reasoning_content` 下发 |
| P5 | ⚠️ 部分 | [convert_openai_responses_chat_stream.go:524](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_openai_responses_chat_stream.go#L524)、[529](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_openai_responses_chat_stream.go#L529)；[convert_responses_claude_stream.go:642](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_responses_claude_stream.go#L642) | 非流式已按 `incomplete` 映射；流式 body 里 `status=incomplete`，事件名却仍是 `response.completed`（两处），应发 `response.incomplete` |
| P6 | ✅ 已修 | [convert_openai_claude_stream.go:39](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_openai_claude_stream.go#L39)、[238](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_openai_claude_stream.go#L238) | 当前块 index 统一推进，工具块按 call 记录 index，关闭顺序稳定 |
| P7 | ✅ 已修 | [convert_tools.go:114](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_tools.go#L114) | 非法字符归一后加哈希后缀，`a_b`/`a-b` 不再碰撞 |
| P8 | ✅ 已修 | [convert_responses_claude.go:287](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_responses_claude.go#L287)、[465](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_responses_claude.go#L465) | 合成缺失的 tool_result，避免 Anthropic 400 |
| P9 | ⚠️ 基本修复 | [convert_responses_claude_stream.go:547](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_responses_claude_stream.go#L547)；[convert_claude_responses_stream.go:291](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_claude_responses_stream.go#L291)；[convert_responses_claude.go:386](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_responses_claude.go#L386) | 签名 thinking/redacted 经 `encrypted_content` 往返已通；边界：对话末尾仅有 redacted_thinking 时被替换成空 thinking 占位，ciphertext 丢失 |
| P10 | ⚠️ 部分 | [convert_claude_responses.go:283](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_claude_responses.go#L283)；[convert_claude_openai.go:177](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_claude_openai.go#L177)；[convert_responses_claude.go:484](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_responses_claude.go#L484) | Responses→Claude 已转 top_k/stop/user；Chat→Claude 仍丢 stop/user/top_k；Claude→Responses 仍把 stop 发给 Responses（非法）且不转 top_k |
| P11 | ⚠️ 部分 | [protocol_proxy.go:300](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/protocol_proxy.go#L300) | 新增 Claude↔Responses 直连、去掉 Chat 中转腿；仍是两两手写转换，无 magpie 式单一 IR |

### 路由 / 调度 R1–R6

| # | 状态 | 证据 | 说明 |
|---|---|---|---|
| R1 | ⚠️ 已落地但有缺陷 | [provider_lanes.go:181](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/provider_lanes.go#L181)；[protocol_proxy.go:693](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/protocol_proxy.go#L693) | RPM 固定窗口，跨分钟可 2× 突发；时钟回拨不重置；出图不进 lane；空流重试不计数；重启清零；`max<1` 时排队容量 0（[91-97](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/provider_lanes.go#L91)），纯 RPM 配置无法等待 |
| R2 | ❌ 未修 | [provider_failover.go:39](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/provider_failover.go#L39)、[319](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/provider_failover.go#L319) | 仍固定软 1 分钟 / 硬 2 分钟，不解析 `Retry-After` / `resets_at` / 额度窗口 |
| R3 | ❌ 未修 | [provider_failover.go:52](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/provider_failover.go#L52) | `"billing"` 仍在 `hardQuotaHints`，计费文案错误会被当成永久额度死亡 |
| R4 | ❌ 未修 | [empty_stream_retry.go:26](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/empty_stream_retry.go#L26) | 仍只重试空流/纯 thinking；首包后任何中断即终局，备用链路不接管 |
| R5 | ❌ 未修 | [provider_affinity.go:38](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/provider_affinity.go#L38) | 会话 pin 纯内存（TTL 30 分钟、上限 10000），重启即丢，部署后 prompt cache 全量重烧 |
| R6 | ❌ 未修 | [provider_lanes.go:43](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/provider_lanes.go#L43) | 仍是每 Provider（非每账号）闸门，唤醒非 FIFO；注释明确保留该设计 |

### 横切安全 S1–S5

| # | 状态 | 证据 | 说明 |
|---|---|---|---|
| S1 | ✅ 已修 | [cors.go:28](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/cors.go#L28) | 管理端只允许与 Host 同源，带 `Vary: Origin`；公共模型端点仍 `*`。残留：`withAdminAuth` 包在 CORS 外层（[server.go:483](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/server.go#L483)），回环管理员绕过（[admin_auth.go:326](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/admin_auth.go#L326)）下仍有 no-cors 本地 CSRF 面（现代浏览器 PNA 预检可缓解） |
| S2 | ⚠️ 部分 | [request_body_limit.go:14](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/request_body_limit.go#L14)、[48](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/request_body_limit.go#L48)；[runtime.go:261](file:///C:/guodevelop/llm-protocol-gateway/internal/app/runtime.go#L261) | 全局 256MiB 上限已挂；但上限过大、仍未设 `ReadTimeout`/`IdleTimeout`；images/import 等路径超限回 400 而非 413；部分处理器先读正文后鉴权 |
| S3 | ⚠️ 部分 | [admin_auth.go:420](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/admin_auth.go#L420)；[auth_throttle.go:102](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/auth_throttle.go#L102)；[access_source.go:27](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/access_source.go#L27) | 登录/注册/setup 已有 8 次/10 分钟限流并带 Retry-After；`/__auth/setup` 仍对首个远程调用者开放；IP 无条件信任 `CF-Connecting-IP`/`X-Forwarded-For`/`X-Real-IP`，换头即绕过并可锁受害者账号 |
| S4 | ⚠️ 部分 | [log_scrub.go:88](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/log_scrub.go#L88)；[server.go:1384](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/server.go#L1384) | 仅请求体入库前脱敏；响应体仍原样落库；裸 `token`、`secretKey`/`accessKey`、JWT 未覆盖 |
| S5 | ❌ 未修 | [protocol_proxy.go:143](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/protocol_proxy.go#L143)、[442](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/protocol_proxy.go#L442) | 上游已回头、正文停顿时仍无空闲超时，可挂到客户端断开 |

### Provider 账号 A1–A5

| # | 状态 | 证据 | 说明 |
|---|---|---|---|
| A1 | ⚠️ 部分 | [oauth_renew_lock.go:19](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/oauth_renew_lock.go#L19)；[trae.go:729](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/trae.go#L729)；[cursor_oauth.go:316](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/cursor_oauth.go#L316) | 已按 provider+refresh token 加锁、锁内重读；但重读后仍需刷新时用的是锁前的旧 token 变量；锁表永不清理；Cursor 续期用 `http.DefaultClient` 无超时（[254](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/cursor_oauth.go#L254)）且持锁；Trae 续期失败吞错返回 nil（[730-737](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/trae.go#L730)），等待者会逐个重花同一枚令牌 |
| A2 | ✅ 已修 | [base_url_host.go:13](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/base_url_host.go#L13) | 按 `Hostname()` 精确/子域匹配，端口、大小写、userinfo、路径伪装均拒；Zhipu/DeepSeek 前后台均走同一判断 |
| A3 | ❌ 未修 | [server.go:3052](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/server.go#L3052)、[5343](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/server.go#L5343)、[5357](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/server.go#L5357) | 仍固定拼 `<base>/images/generations`（Claude/ChatGPT 等必 404）；Cursor 失败回落全局 `OPENAI_API_KEY`；Provider 无自身凭证时把客户端网关 Key 当上游 `Authorization` 发出（泄露给厂商） |
| A4 | ❌ 未修 | [claude_oauth_usage.go:90](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/claude_oauth_usage.go#L90)；[oauth_usage_refresh.go:299](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/oauth_usage_refresh.go#L299) | 仍只有 five_hour/seven_day/opus/sonnet，缺 `seven_day_overage_included`；429/5xx 无负缓存，每次轮询都打上游 |
| A5 | ⚠️ 已修（边界） | [cursor_oauth.go:180](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/cursor_oauth.go#L180)、[283](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/cursor_oauth.go#L283) | 写入 ExpiresAt 时已减 5 分钟；但空/坏过期时间返回 `false`（永不主动刷新），应 fail-closed |

### 控制台能力缺口

| # | 状态 | 证据 | 说明 |
|---|---|---|---|
| 1 实时流 | ⚠️ 部分 | [main.tsx:1174](file:///C:/guodevelop/llm-protocol-gateway/web/src/main.tsx#L1174) | API 日志仅第 1 页轮询；应用日志无周期刷新；仍无 SSE/WS 实时流 |
| 2 路由决策聚合 | ❌ 未做 | [traffic_log.tsx:105](file:///C:/guodevelop/llm-protocol-gateway/web/src/components/traffic_log.tsx#L105) | `analysis.attempts` 只在单条日志详情里出现，无聚合视图 |
| 3 Session 视图 | ❌ 未做 | `sessionId` 仅存在于类型定义，无任何展示 |
| 4 一键重放 | ❌ 未做 | 只有复制 curl（[apikeys.tsx:319](file:///C:/guodevelop/llm-protocol-gateway/web/src/components/apikeys.tsx#L319)），无重放按钮/端点 |
| 5 日志成本列 | ❌ 未做 | 详情弹窗有成本（[main.tsx:7095](file:///C:/guodevelop/llm-protocol-gateway/web/src/main.tsx#L7095)），表格无成本列 |
| 6 告警未读角标 | ⚠️ 部分 | 告警页内有未读筛选与计数（[main.tsx:6096](file:///C:/guodevelop/llm-protocol-gateway/web/src/main.tsx#L6096)），导航项无角标（[lib.tsx:337](file:///C:/guodevelop/llm-protocol-gateway/web/src/lib.tsx#L337)） |

### 第 5 批能力扩展

| 能力 | 状态 | 证据 |
|---|---|---|
| embeddings/rerank/audio 透传 | ❌ 未做 | [server.go:472](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/server.go#L472) 起无相关路由 |
| Copilot 账号 | ❌ 未做 | 仓库内无 Copilot provider |
| Gemini 原生协议 | ❌ 未做 | 仅 `openai_chat` / `openai_responses` / `claude` 三种协议（[protocol.go:11](file:///C:/guodevelop/llm-protocol-gateway/internal/domain/protocol.go#L11)） |
| web search 工具 | ❌ 未做 | 无服务端执行/回传路径 |
| 备份恢复 | ⚠️ 部分 | 仅有 Provider 级 export/import/restore（[server.go:361](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/server.go#L361)、[437](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/server.go#L437)），无整库备份恢复 |

---

## 三、新发现问题（按严重度）

### P0

1. **Claude→Responses 仍把 `stop` 发给 Responses 上游**：[convert_claude_responses.go:283-290](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_claude_responses.go#L283)。
   Responses 不接受该参数（magpie `buildResponses` 全程无 `stop`），会直接 400 `Unknown parameter: 'stop'`。
   连续第 4 轮标记仍未修；正确做法是本地记住序列、不发给上游。
2. **stop_sequence 回填覆盖正确原因**：[stop_sequence_fill.go:121](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/stop_sequence_fill.go#L121) 只要尾部命中就改写任意 `stopReason`，
   包括 `tool_use`/`max_tokens`：工具调用不被执行、截断被伪装成正常结束；[78-80](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/stop_sequence_fill.go#L78) 还会把 `incomplete` 覆盖成 `completed`。
3. **首个远程调用者可设管理员密码**：[admin_auth.go:420-458](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/admin_auth.go#L420) 仅限流，不回环限制、不需一次性 token；
   且限流键可信任意转发头（[access_source.go:27-44](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/access_source.go#L27)），换头即绕过。
4. **出图把网关 Key 发给上游厂商**：[server.go:5357-5360](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/server.go#L5357)；
   叠加固定 URL 与全局 Key 回退，A3 的用户 Key 串用/404/泄露三类问题全部还在。

### P1

5. **incomplete 仍以 completed 事件下发**：[convert_openai_responses_chat_stream.go:529-533](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_openai_responses_chat_stream.go#L529)、
   [convert_responses_claude_stream.go:642-646](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_responses_claude_stream.go#L642)。客户端把截断当完成，不会续写。
6. **Responses→Claude 流式并行工具调用损坏**：[convert_claude_responses_stream.go:148-179](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_claude_responses_stream.go#L148) 打开工具块后不推进 `blockIndex`，
   并行调用得到重复 index；[312-328](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_claude_responses_stream.go#L312) 用 `item_id` 去匹配 `call_id` 键，多个工具时参数直接丢弃；
   [331-340](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_claude_responses_stream.go#L331) 只在单工具时关闭。
7. **Responses→Chat 非流式仍丢前言**：[convert_openai_responses_chat.go:849-851](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_openai_responses_chat.go#L849) 有 tool_calls 就把 `content` 置 nil。
8. **Claude→Responses 最终 usage 缺缓存命中**：[convert_responses_claude_stream.go:632-636](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_responses_claude_stream.go#L632) 只写 input/output/total；
   反向读取了 `cached_tokens`（[convert_claude_responses_stream.go:262-264](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_claude_responses_stream.go#L262)），计费口径不对称。
9. **出图链外 active / 模型错位**：[user_isolation.go:389](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/user_isolation.go#L389) 把链外 active 放到链头并直接返回；
   [server.go:4209](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/server.go#L4209) 用路由默认 Provider 解析模型，与实际 Provider（4224）不一致。
10. **A1 残留**：锁内重读仍用旧 token 变量（[trae.go:729](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/trae.go#L729)、workbuddy.go:674、cursor_oauth.go:316）；
    锁表不回收；Cursor 续期无超时；Trae 失败吞错。

### P2

11. **`allowed_tools` 线上形状待验证**：[convert_responses_claude.go:506](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/convert_responses_claude.go#L506) 在 `tool_choice` 里塞 `allowed_tools` 对象。
    Anthropic 官方 `tool_choice` 只定义 auto/any/none/tool；magpie 解析 Responses 的 `allowed_tools` 但从不转发给 Claude。
    收窄 `tools` 列表本身是安全且有效的，未知字段应删除或先在真实 Anthropic 上游实测。
12. **RPM 配置边缘**：`max<1` 时排队容量 0，纯 RPM 配置一旦打满立即报队列满；`Retry-After` 恒为 1 秒（[provider_lanes.go:33](file:///C:/guodevelop/llm-protocol-gateway/internal/gateway/provider_lanes.go#L33)）。
13. **CORS/管理端顺序**：`withAdminAuth` 在 CORS 外层，跨域预检先吃 401；回环管理员绕过叠加 no-cors 请求构成本地 CSRF 面，建议把 CORS 前置并校验 Origin。
14. **仓库卫生**：`chatgpt_oauth.go.bak`、`claude_oauth.go.bak`、`cursor_oauth.go.bak`、`qoder_pat.go.bak`、`qoder_pat_handlers.go.bak` 仍被跟踪。
15. **测试质量**：出图新测试只测纯函数，未断言 403、上游 URL、Authorization、disabled/deleted；`TestClaudeToolChoiceWireShape` 无覆盖价值。

---

## 四、建议修复顺序

| 优先级 | 项 | 位置 | 规模 |
|---|---|---|---|
| P0 | 删除发往 Responses 的 `stop` | convert_claude_responses.go:283-290 | 删 1 段 + 用例 |
| P0 | 回填只在 `end_turn`/`completed` 时改写 | stop_sequence_fill.go:78、121 | 2 处判断 + 用例 |
| P0 | 出图不再回退/转发客户端 Key；按 Provider 拼图片端点 | server.go:3052、5343、5357 | 中 |
| P0 | setup 仅回环或一次性 token；IP 取 RemoteAddr + 可信代理白名单 | admin_auth.go:420、access_source.go:27 | 小 |
| P1 | incomplete 发 `response.incomplete` | 两处 stream finalize | 小 |
| P1 | 并行工具流按 item_id 建索引并推进 blockIndex | convert_claude_responses_stream.go:148、312 | 中 |
| P1 | Responses→Chat 保留前言；Claude→Responses 补 cached_tokens | convert_openai_responses_chat.go:849；convert_responses_claude_stream.go:632 | 小 |
| P1 | 出图 active 只允许链内；模型按 decision.ProviderID 解析 | user_isolation.go:389；server.go:4209 | 小 |
| P2 | R2/R4/R5、S4 响应脱敏、A4 窗口与负缓存、控制台 2–5 | — | 中 |

---

## 五、验证记录

```
go build ./...                                   # PASS
go vet ./...                                     # PASS
go test ./...                                    # gateway/app/monitor/store 等 PASS
                                                 # internal/tunnel 5 FAIL：Windows 无 /bin/sh（环境问题）
go test ./internal/gateway -run TestImagesEffectiveProvider -v -count=1
                                                 # 5/5 PASS（早先快照为 2 FAIL，期间实现与测试被并发修改）
```

审查期间工作区被并发修改（约 19:48–19:54）；以上结论基于最终快照。
本轮审查未修改任何代码、未提交。
