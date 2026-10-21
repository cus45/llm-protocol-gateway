# Bearer 令牌 Provider 说明

本文档说明如何在本网关中配置、使用与运维 **Bearer 令牌（API Key）** 类型的
Provider，以及它的鉴权、健康探测、故障转移与"自注册"机制。适用于对接任意
OpenAI Chat / OpenAI Responses / Claude 协议、且用一串密钥做鉴权的上游。

> 与 OAuth 类 Provider（`claude_oauth` / `cursor_oauth` / `chatgpt_oauth`）的区别：
> Bearer Provider 用一个固定密钥直接发往上游，`BaseURL` 可自由设置；OAuth 类
> 由平台托管令牌刷新且 `BaseURL` 被固定到各自上游，不能自注册。

---

## 1. 它是什么

Bearer Provider 对应 `AuthType = "api_key"`（或留空 `""`，等价默认）。核心字段：

| 字段 | 说明 |
| --- | --- |
| `protocol` | 上游实现的协议：`openai_chat` / `openai_responses` / `claude` |
| `baseUrl` | 上游入口 URL（可为你的 Cloudflare 域名、第三方中转等） |
| `apiKeySource` | 密钥来源，见下文格式 |
| `authHeader` | 承载密钥的请求头，默认 `Authorization`；Claude 惯例用 `x-api-key` |
| `defaultModel` | 缺省模型名（客户端未显式指定时兜底） |

对应代码：`internal/domain/types.go`（`Provider` 结构、`AuthTypeAPIKey`）。

---

## 2. 密钥来源 `apiKeySource` 格式

`resolveProviderAuth`（`internal/gateway/server.go`）按前缀解析：

- `literal:<token>` —— 明文密钥，直接落盘保存（当前实现）。
- `env:<ENV_NAME>` —— 从进程环境变量读取，密钥不落盘（推荐用于敏感场景）。
- `keychain:<...>` —— 预留（macOS Keychain，尚未实现，返回空）。
- 无前缀 —— 整个字符串按原始密钥值使用。

示例：

```
apiKeySource = "literal:sk-abc123..."
apiKeySource = "env:MY_UPSTREAM_KEY"
```

---

## 3. 鉴权头如何注入

`applyProviderAuth`（`internal/gateway/server.go`）逻辑：

1. 取 `authHeader`（为空则用 `Authorization`）。
2. 若头是 `Authorization` 且值未以 `bearer ` 开头，自动补 `Bearer ` 前缀。
3. 若 Provider 未配置密钥而客户端请求自带 `Authorization`，则透传客户端的头。

因此：

- OpenAI 系上游：`authHeader = Authorization`，最终发出 `Authorization: Bearer <token>`。
- Claude 上游：`authHeader = x-api-key`，最终发出 `x-api-key: <token>`（不加 Bearer 前缀）。
- 自定义头（如 `X-Api-Key`、`api-key`）也支持，按原样设置、不加前缀。

> 控制台"创建 Provider"表单的默认约定：Claude 协议默认 `x-api-key`，其余默认
> `Authorization`。

---

## 4. BaseURL 与"获取模型"地址推导

拉取模型列表 / 可用性探测会基于 `baseUrl` 推导出一个 `/models` 地址
（`deriveModelsURL`，`internal/gateway/server.go`）：

- host 含 `anthropic.com` → `/v1/models`
- Azure `/deployments/.../chat/completions` → 取 deployments 前缀 + `/models`
- path 含 `/chat/completions` | `/responses` | `/messages` → 就地替换为 `/models`
- 其余情况 → 在 path 末尾追加 `/models`

所以 `baseUrl` 建议直接填到"协议入口"这一层，例如：

```
https://api.example.com/v1/chat/completions   → 探测 https://api.example.com/v1/models
https://api.example.com/v1                     → 探测 https://api.example.com/v1/models
```

---

## 5. 健康探测与"异常"状态（重要）

### 5.1 手动检测
控制台的"获取模型 / 对话测试"按钮，或自注册脚本调用 self-check 接口，会发起一次
真实的连通性+鉴权检查（`fetchProviderModels` / `testProviderChat`）。

### 5.2 被标记为"异常"的条件
真实转发失败时先经 `classifyProviderFailover`（`internal/gateway/provider_failover.go`）分成
**软故障 / 硬故障**；两者都会触发备用 Provider 切换，但控制台"异常"更保守：

**硬故障（立刻标异常）**
- 明确的订阅/账单耗尽文案（`usage limit`、`insufficient_quota`、`credit balance` 等）
- 明确的凭证失效（`invalid_grant`、`revoked`、`invalid api key` 等）
- 对 `claude_oauth`：若缓存的 5h utilization ≥ 95%，上述 usage 文案保持硬故障；**< 95% 则降级为软故障**（避免额度面板未满却整卡标红）

**软故障（连续 3 次 / 60s 才标异常）**
- 传输层错误（连接失败/超时等）
- HTTP 429 / 529 / 502 / 503 / 504
- HTTP 401 / 403（无硬故障关键字时，例如 token 刷新竞态）
- 带短期限流关键字的 400（`rate_limit` / `overloaded` 等）

> 注意：普通的 400（如 thinking 块签名类错误）**不会** failover / 标记异常——它们由 thinking
> 整流器就地修复重试，见 `internal/gateway/thinking_rectifier.go`。
> 打标时 app log 会写 `provider marked unavailable kind=soft|hard reason=...`。

### 5.3 异常后的周期性重探
后台协程 `StartProviderFailoverRecovery`（开机启动，`internal/app/runtime.go`）：

- 每 **2 分钟**一轮（`providerFailoverRecoveryInterval`，首轮开机后 30s）。
- `reprobeUnavailableProviders`：对当前"异常"的 Provider 各发一次 **带 Bearer 的
  `GET /models`**（30s 超时），成功即清除异常标记并停止重探；仍失败则把倒计时顺延
  到下一轮。
- `recoverAPIKeyPreferredProviders`：当某个 Key 已切到备用 Provider 时，探测更高
  优先级的前置 Provider，可用即自动回切。

**关键结论：** 只有"异常期间"的 Provider 才会被后台探测（约每 2 分钟 1 次 `GET
/models`），恢复即停；**健康 Provider 无任何后台探测流量**。控制台的 `nextRetryAt`
即下一次重探时间。

### 5.4 Cloudflare 域名的注意事项
若 `baseUrl` 是你的 Cloudflare 隧道域名：异常期间约 30 次/小时的 `/models` 探测量
很低，不会因频率触发 Cloudflare 侧封禁。真正需要担心的"网络管控封禁"通常是域名/边缘
IP 被 GFW 识别，和请求频率无关。如需降频，可调大 `providerFailoverRecoveryInterval`
（`provider_failover.go`）。另需知悉：网关自身公网隧道健康探测 `healLoop`
（`internal/tunnel/manager.go`，默认每 15s 打 `/__health`）是一直在跑的、频率更高。

---

## 6. 故障转移（多 Provider 备选）

给 API Key 配置 `fallbackProviderIds` 后：主 Provider 失败（同 5.2 判定）会自动切到
下一个备选；前置 Provider 恢复后由 5.3 的回切逻辑自动切回。链路见
`executeProtocolFlowWithFailover`（`internal/gateway/provider_failover.go`）。

备选模型规则按协议区分：

- **与主 Provider 同协议**：无需指定模型，请求里的模型名原样带给备选；若备选的模型目录里
  没有该模型且配置了 `defaultModel`，会用备选自己的默认模型兜底。
- **与主 Provider 跨协议**：必须在 `fallbackModelOverrides` 里为该备选锁定一个模型，
  否则保存会被拒绝；运行时若配置缺失也会跳过该备选，避免把 A 上游的模型名发给 B 上游。

### 6.1 尝试顺序：整条链都可以回头试
一次请求先把链路过滤成**候选集**（`failoverAttemptOrder`，`internal/gateway/provider_schedule.go`）：
被撤销授权的、被禁用的、跨协议却没锁模型的都不进候选；被标"异常"的候选排到最后但**不会被丢掉**
（异常来自一次真实失败或探测，可能已经过期，而它有时是唯一能服务该模型的账号）。

Key 的 `activeProviderId`（粘滞）只决定起点：链被旋转成"粘滞的那条在最前"，因此
既不会每次都回去骚扰首选，又能在粘滞备选也失败时**在同一请求内回头试前序 Provider**。
旧实现是从粘滞下标一直往后走，停在链尾，前序 Provider 要等 5.3 的回切探测（最长 2 分钟）才用得上。

### 6.2 会话粘性（prompt cache 优先于均衡）
`ActiveProviderId` 是 Key 级的，一把 Key 同时跑几个 Agent 时会互相顶掉，
每次换账号都把上游已经建好的 prompt cache 作废。现在按会话记：

- 会话标识取自请求自己报的身份：`Session_id` / `X-Session-Id` / `X-Claude-Code-Session-Id` 头，
  body 的 `session_id`，`metadata.session_id`，Claude Code 的 `metadata.user_id` 里
  `_session_` 之后的后缀，以及 Codex 用来路由缓存的 `prompt_cache_key`（`requestSessionID`，
  `internal/gateway/provider_affinity.go`）。裸 `user_id` 不算会话，它把多个会话合在一起。
- 哈希成 `keyID + session` 的摘要后查 pin 表（纯内存，30 分钟滑动过期，上限 1 万条）。
  谁回答了这一轮，pin 就跟到谁；pin 指向的 Provider 挂了或已不在链上，pin 立刻作废。
- 优先级：**会话 pin > 调度模式 > Key 粘滞**。pin 生效的那一轮不回写 `activeProviderId`，
  否则两个会话会把 Key 级粘滞来回抖动、每请求一次落库写。
- 没带任何会话标识的请求行为不变，仍走 Key 级粘滞。

### 6.3 调度模式 `scheduleMode`
Key 级（也可存进"方案" `KeyProfile`，切方案一起切），空 = 现状：

| 值 | 行为 |
| :-- | :-- |
| `` / `order` | 按链路顺序，粘滞的 Provider 排最前 |
| `rotate` | 每个请求换下一个候选，内存游标，重启从头开始 |
| `least_used` | 在飞请求数最少的先用，其次看近 6 分钟实际处理过的 token（**缓存命中不计**） |
| `quota` | 额度余量多的先用，数据来自额度面板已缓存的快照（Claude / ChatGPT / Cursor / 智谱 / DeepSeek），读不到的排在中位，**绝不在请求路径上打上游** |

非法取值在保存时被拒绝（`normalizeScheduleMode`），不会静默退化成 `order`。

### 6.4 首包超时（假死的上游换掉，而不是拖死）
Provider 上的 `firstPacketTimeoutSec`：网关等这台**响应头**的最长时间，超时即放弃这次尝试、
继续下一个候选。定时器只管到响应头到达为止，所以开播之后思考两分钟的模型不会被误伤
（`doUpstreamHTTP`，`internal/gateway/upstream_timeout.go`；上限 600s，0 = 不限制 = 旧行为）。

实现上它返回的是自定义错误而不是 `context.Canceled`——后者会被 5.2 的判定当成
"客户端已经断开"而停止 failover。专用接口保存：`POST /__providers/{id}/first-packet-timeout`
`{"seconds": N}`（Provider 的 PATCH 是增量的，数字 0 分不清"没传"和"关掉"）。

### 6.5 并发闸门（先由网关拒绝，别让厂商判你异常）
Provider 上的 `maxConcurrentRequests` + `queueWaitSeconds`：

- 超出的请求**在发出前**被拒，返回 429 且带 `Retry-After`；若链上还有别的候选，
  先自动换过去，换不出去才把 429 交给客户端。
- 被闸门挡住**不算 Provider 故障**：不写软故障计数、不标"异常"，忙碌的健康账号不会被误判。
- `queueWaitSeconds > 0` 才排队，等不到位置同样 429；排队用广播唤醒，等待者超时或客户端断开都会退出队列。
- 专用接口：`POST /__providers/{id}/concurrency` `{"maxConcurrentRequests": N, "queueWaitSeconds": N?}`，
  上限 1000 / 排队上限 60s；把上限清 0 时排队秒数一并归零（`internal/gateway/provider_lanes.go`）。

### 6.6 调度轨迹（为什么是它、换了谁）
每次请求把决策过程记进请求日志的 `analysis` 里（与上下文统计同一个 JSON，无需新表）：
`scheduleMode` / `candidates`（排好序的候选）/ `sessionPinned` / `attempts[]`
（每个候选：provider、model、status、`reason`、耗时、`deferred` 回答被丢弃、`skipped` 根本没发出）。
控制台在 API 日志详情的「调度轨迹」里逐条列出；早于该功能的日志没有这些字段，
页面直说"未记录"，不补一个看起来像真相的空链。reason 取值见 `internal/gateway/schedule_trace.go`。

---

## 7. 自注册（Self-Registration）——为轮转 URL 的自建后端准备

当你的上游跑在一个会变动的反代/内网穿透地址后面（如 quick tunnel 每次重启换 URL），
可以让上游自己的脚本在地址变化时回填 `baseUrl` / `apiKeySource`，无需登录控制台。

> 仅支持 `api_key`（Bearer）类型；OAuth 类 Provider 会被明确拒绝。

### 7.1 生成自注册令牌（控制台会话鉴权，owner/admin）
```
POST /__providers/{id}/self-register-token
→ { "token": "<原始令牌，仅此一次可见>", "preview": "....abcd", "createdAt": ... }
```
令牌仅在生成时返回一次，不可再取回；重新生成会使旧令牌立即失效。撤销：
```
POST /__providers/{id}/self-register-token/revoke
```

### 7.2 机器侧回填（仅用自注册令牌鉴权，无 Cookie/CSRF）
```
PATCH /__providers/{id}/self-register
Authorization: Bearer <自注册令牌>
Content-Type: application/json

{
  "baseUrl": "https://new-address.example.com/v1/chat/completions",
  "apiKeySource": "literal:sk-...",     // 可选
  "protocol": "openai_chat",             // 可选：openai_chat|openai_responses|claude
  "authHeader": "Authorization"          // 可选；改 protocol 未给时按惯例推导
}
```
- 四个字段至少提供一个。
- `baseUrl` 有 SSRF 防护（`validateSelfRegisterBaseURL`）：必须 http/https，且不允许
  loopback / 私网 IP / `localhost` / `*.local` / `*.internal` 等内网目标。
- 成功回填后会自动清除该 Provider 的"异常"标记。

### 7.3 协议 conformance（推荐）与旧自检
机器侧用自注册令牌调用：
```
POST /__providers/{id}/self-check/conformance
Authorization: Bearer <自注册令牌>
```
按 Provider 当前 `protocol` 跑一套用例：

| id | 级别 | 含义 |
| --- | --- | --- |
| `models` | required | `/models` 鉴权连通 |
| `nonstream_shape` | required | 非流式 JSON 形状 + 可提取助手文本 |
| `stream_shape` | required | SSE 事件序列（Chat 需 `[DONE]`；Responses/Claude 需对应事件） |
| `usage_fields` | recommended | usage 字段齐全（Responses 需 `input_tokens_details.cached_tokens`，即使为 0） |
| `cache_hit` | recommended | 两轮探测是否回报 cache 命中 |

返回要点：`success`/`passedRequired` 表示必过项全过；`passedAll` 含建议项；`cases[].hint` 给出修复提示。  
脚本应改完再重跑，直到 `success=true`（不要在网关侧重试死循环）。

控制台会话可用：`POST /__providers/{id}/conformance`（owner/admin）。

兼容旧接口（仍可用）：
```
POST /__providers/{id}/self-check/health   # 连通性+鉴权
POST /__providers/{id}/self-check/chat      # 端到端对话（固定 prompt）
```

---

## 8. curl 速查

创建（管理员，示例走控制台内部接口）：
```
POST /__providers
{ "name":"MyUpstream", "protocol":"openai_chat",
  "baseUrl":"https://api.example.com/v1/chat/completions",
  "apiKeySource":"literal:sk-...", "authHeader":"Authorization",
  "defaultModel":"gpt-4o" }
```

测试可用性：
```
POST /__providers/{id}/test
```

机器侧自注册（轮转地址回填）：
```
curl -X PATCH https://<网关公网域名>/__providers/<id>/self-register \
  -H "Authorization: Bearer <自注册令牌>" \
  -H "Content-Type: application/json" \
  -d '{"baseUrl":"https://new.example.com/v1/chat/completions"}'
```

---

## 9. 代码索引

- 结构与常量：`internal/domain/types.go`
- 鉴权注入 / 密钥解析：`internal/gateway/server.go`（`applyProviderAuth` / `resolveProviderAuth`）
- 模型地址推导：`internal/gateway/server.go`（`deriveModelsURL` / `fetchProviderModels`）
- 异常判定与后台重探：`internal/gateway/provider_failover.go`
- 候选排序 / 会话粘性 / 负载与额度 / 并发闸门 / 轨迹：`internal/gateway/provider_schedule.go`、
  `provider_affinity.go`、`provider_load.go`、`provider_lanes.go`、`schedule_trace.go`
- 首包超时：`internal/gateway/upstream_timeout.go`（在 `doHTTPWithTiming` 内生效）
- 后台恢复循环启动：`internal/app/runtime.go`（`StartProviderFailoverRecovery`）
- 自注册 / 自检 / conformance：`internal/gateway/provider_self_register.go`、`provider_conformance.go`
- thinking 签名整流器：`internal/gateway/thinking_rectifier.go`
