# 第二十七轮审查报告（round27）

- 审查时间：2026-10-23 19:05–19:30 (+0800)
- 审查范围：`f178e88..80558d4`（2 个新提交）+ 未提交工作区（60 改 7 新增，+3084 / −615）
- 审查方式：全量 diff 阅读 + 四路并行深挖（协议/调度/账号/控制台）+ **本地探针实测**（探针文件已删除，未改任何业务代码）
- 工作区快照指纹：`sha256(git diff HEAD) = f94f259d…`（19:26）；审查期间对方仍在实时编辑（最新 mtime 19:24:14）

---

## 一、结论

**不可合入、更不可上线。** 三个理由，按严重度：

1. **P0 流式名存实亡**：透传路径（所有带 Key 的 `/v1/*`、`/anthropic/*` 请求都走它）先全量缓冲再一次性下发，首字节 = 整段生成时间，与备用链无关。本轮用探针实测（旧轮只有静态推断）。
2. **两个测试红**：`TestRateOnlyLaneWaitsOutItsBudgetBeforeRefusing`、`TestRateOnlyLaneAdmitsWhenItsWaitExpires` 与新引入的「wait=0 立即拒绝」语义直接冲突，测试未同步。
3. **生产又在跑未提交代码**：VPS 二进制 19:20:51(+0800) 构建，含仅存在于工作区的符号 ⇒ 线上无 commit 可复现，回滚只能重编。

两个新提交本身（`f3b3ccb` 前端把 `/v1/embeddings` 纳入模型协议路径、`80558d4` 上游 4xx 记 warn 日志）**逻辑正确、范围干净**，可保留。

基线（当前工作区）：`go build ./...` 绿、`go vet ./...` 绿、`go test ./internal/gateway/` 43.7s **2 红**（store/app 绿）、`web npx tsc -b` **0 错误**、`gofmt -l` 命中的 7 个文件全部是 CRLF 行尾假阳性（逐文件 `gofmt -d` 验证，diff 是整文件行尾而非缩进）。

---

## 二、P0（阻断上线）

### P0-1 透传流全量缓冲：TTFT = 整段生成，心跳也一起被吞

**探针证据（PROBE-A，本地实测）：**

```
canDefer=true   firstWrite=601ms lastWrite=601ms total=601ms bytes=216 err=<nil>
canDefer=false  firstWrite=601ms lastWrite=601ms total=601ms bytes=216 err=<nil>
```

上游分 3 段、每段间隔 300ms，客户端在**最后一段结束后**才拿到全部 216 字节。

**成因（代码路径已逐行核对）：**

1. `passthrough_stream.go:245-249`：只要 `w` 是 `*failoverResponseWriter` 就走 `writeBufferedStream`，**不再看 canDefer**。
2. `provider_failover.go:340-359`：`failoverResponseWriter.Write` 在 `!passthrough` 时只写 `w.buf`，不落网络。
3. `passthrough_stream.go:401-412`：`writeBufferedStream` 全程**不调用 `hw.WriteHeader`/`fw.WriteHeader`**，只有 `io.Copy` 结束后才 `fw.FlushError()` → `commitHeaders` → 一次性 `base.Write(buf)`。

所以 `hw.Committed()`（`:409`）在第一次写时就为 true，但那只是「写进了 fw 的缓冲」，不代表到了客户端；`delivered` 判定因而也失真（后续 `closeInterruptedStream` 会把收尾帧写给一个已经在缓冲里的 writer）。

**副作用三连：**

- 生成期间**零心跳到客户端**：heartbeat goroutine 写的是同一个 `hw`→`fw`，全部进缓冲（Cloudflare ~100s 空闲切断风险回归）。
- 内存 ≈ 2× 整段（fw.buf + tee），且无上限。
- TTFT 指标退化：`requestTiming` 的首字节时间会记在结尾。

**对照 PROBE-E（转换路径是对的）：**

```
firstWrite=0s lastWrite=501ms total=751ms bytes=42
```

原因：`converterStreamWriter.Write` 里调了 `s.hw.WriteHeader(http.StatusOK)`，而 `failoverResponseWriter.WriteHeader` 在 `statusCode < 400` 时直接 `commitHeaders` → `passthrough=true` → 后续写直达客户端。**同一份 writer，两条路径两种行为**，这就是修复的钥匙：透传路径也需要在首字节前显式提交（或者给 `writeBufferedStream` 一个 `canDefer` 门槛，二者取一，取决于产品要「真流式」还是「可重试」）。

### P0-2 生产运行未提交代码（连续第 6 轮）

```
ActiveEnterTimestamp = Fri 2026-10-23 04:21:15 PDT  (= 19:21:15 +0800)
binary mtime         = 2026-10-23 04:20:51 PDT  (= 19:20:51 +0800)
GATEWAY_DB=/var/lib/llm-protocol-gateway/gateway.db   GATEWAY_ADDR=0.0.0.0:18093
```

二进制含 `streamEndReader`（仅未跟踪新文件）、`retireLocked`（仅工作区）、`LaneGroup`（仅工作区）、`responsesUsageFromUsageMap`（WIP 新增函数）⇒ **线上 = 19:20 的工作区快照**。此时工作区还有 2 个红测试，且 P0-1 在线生效。

---

## 三、P1（功能性错误）

| # | 问题 | 位置 | 证据 / 影响 |
|---|---|---|---|
| P1-1 | **Chat→Responses 流式仍把「干净 EOF 无 [DONE]」当正常结束**（R4 只修了 5 条腿中的 4 条） | `convert_openai_responses_chat_stream.go:666` | 同文件 `:1021`、`convert_openai_claude_stream.go:297`、`convert_claude_openai_stream.go:216`、`convert_claude_responses_stream.go:430`、`convert_responses_claude_stream.go:794` 都改成 `isUpstreamStreamInterruptedReader(r) \|\| cleanEOFInterruption(r, closed)`，只有这条腿没有 `closed` 跟踪也没有 `cleanEOFInterruption`。上游正常 FIN 截断 ⇒ 客户端收 `response.completed`，不重试、不计失败 |
| P1-2 | **WorkBuddy 未识别的 400 丢响应体**（相对 HEAD 是回归） | `workbuddy.go:1078-1113` | `:1080` 已 `io.ReadAll`+`Close`；三个 case 都不匹配时直接落到 `:1113` 而不 `restore()`。HEAD 是 `if readErr != nil \|\| !afterToolsRefused { return restore() }`。后果：参数错误/上下文超长/模型名错等 400 变成**空 body 400**，且 `classifyProviderFailover(400, nil)` 读不到 body 里的 quota 字样 → 该软失败转移的不转移 |
| P1-3 | **`LaneGroup` 完全没落库** | `domain/types.go:355` / `provider_lanes.go:506` vs `internal/store` | `grep -rn "LaneGroup\|lane_group" internal/store/*.go` = **0 命中**；对照 `requests_per_minute` 在 `sqlite.go` 有 4 处（ALTER/INSERT/SELECT/Scan）。重启后分组成员各自恢复满额度，正是该功能要防的「把同一账号打出 429」 |
| P1-4 | **`SignInAt` 没落库** | `domain/types.go:509` / `workbuddy.go:331,630` vs `internal/store` | 同样 0 命中。`ExpiresAt==""` 的账号靠 `SignInAt` 决定主动续期，重启后恒 `false`，退回「只能等 401」。新测试只测内存规则，覆盖不到 |
| P1-5 | **价格面板默认「全局 + `*`」，随手保存即写全局兜底价** | `web/src/components/pricing.tsx:74-75`、`:166-173` | 打开面板就是 `providerId="*" / modelId="*"`，四档留空按 0 提交；`pricing.go:233` 只拒负数 ⇒ 一条 `*/*` 全 0 行把所有未设价模型变成「已计价 $0」，用量页「未计价」信号消失。改动前默认是第一个 Provider，误伤面从单 Provider 扩到全账本 |
| P1-6 | **配额守护按「软删除」永久剪枝任务成员，恢复 Provider 后不恢复** | `quota_keeper.go:392-404`、`store/quota_keeper.go:174` | 已核对 `server.go:440` 存在 `POST /__providers/{id}/restore`（软删除可恢复）。keeper 每 tick 把不可解析的 id 从 `provider_ids` 删掉并在空时 `enabled=false` 落库；恢复 Provider 后任务不会复原，也没有撤销入口 |
| P1-7 | **出图重新被 120s 砍** | `server.go:5709` `proxyOpenAIImagesUpstream` | 该路径 `client Timeout:0` + `guardUpstreamIdleClient`，共享 transport 又有 `ResponseHeaderTimeout: 120s`（`upstream_timeout.go:47`）。而 Cursor 分支专门设了 `imageGenerationTimeout = 10min`（`:5664`）并注释「图片几分钟不出字」。OpenAI 兼容出图多数是「渲染完才回头」，会在 120s 被砍。**待厂商确认**，但两条路径自相矛盾 |
| P1-8 | **心跳 `stop()` 无条件 join，慢客户端可长期占住 handler 与 lane 槽位** | `sse_heartbeat.go:43-48` | 原为缓解「handler 返回后心跳仍写 ResponseWriter」。但心跳 goroutine 可能阻塞在 `base.Write`（TCP 背压），`stop()` 在 `defer` 里等 `<-exited`，handler 不返回 ⇒ `acquireProviderSlot` 的 release 不执行。无 `WriteTimeout` 兜底 |

---

## 四、P2（次要 / 一致性 / 测试）

1. **两个红测试是「新语义 vs 旧断言」**，不是实现 bug：`acquire()`（`provider_lanes.go:165-176`）明确写了「wait=0 一律立即拒绝」，而两个测试都传 `0` 却期望等满窗口（`provider_lane_capacity_test.go:216`、`provider_rpm_test.go:224`）。二者必须同步：要么测试改传 `QueueWaitSeconds>0`，要么恢复「rate-only 零等待也排一个窗口」。顺带 `providerQueueCapacity` 的注释（`:142-144`「rate-only lane 仍需要队列，等窗口是它唯一的服务方式」）与新语义互相打脸。
2. **回声守卫仍可被正文尾部标记骗过**：PROBE-C `echoTail closed=true`——正文里出现 `[DONE]` 且落在最后 64 字节内，即使没有真正的结束帧也判完整 ⇒ 真截断不补收尾帧。
3. **转换路径 `firstWrite=0ms` 与透传路径 `601ms` 并列存在**：说明 `writeBufferedStream` 的「提交时机」是唯一变量，修 P0-1 时不要把转换路径也改坏。
4. **价格回退顺序把作用域放在精确度之前**（`pricing.go:104-116`：provider 精确 → provider `*` → 全局精确 → 全局 `*`）。历史故障形态，`pricing_test.go:126` 已把它写死；前端文案也这么承诺 ⇒ 属产品取舍，**不要单方面改**，但要在文档里写明「Provider 的 `*` 会盖住全局精确价」。
5. **`laneFor` 每次调用覆盖组上限**（`provider_lanes.go:134-138`）：同组两行限值不一致时，共享 lane 的上限随「最后进来的是谁」跳动。建议保存时校验同组一致，或取首次值。
6. **分组成员自身未配限值时完全绕过组额度**（`:520-526` 只看本行 `max/rpm`）；UI 未提示「每一行都要填」。
7. **`LaneGroup` 大小写敏感但注释说不敏感**（`domain/types.go:352` vs `provider_lanes.go:507` 只 TrimSpace）；前后端长度校验一个是字符一个是字节（`provider_lanes.go:575`）。
8. **分组 provider 仍有两处按 `provider.ID` 找 lane**：`server.go:4495`（embeddings）、`provider_lanes.go:529`（`providerInFlight`）⇒ 分组时调度器看到的 inflight 恒 0。
9. **负缓存只接了 Claude 一个 handler**：`server.go:2677/2683/2688` 有 `setFailure`；Cursor/ChatGPT/Zhipu/DeepSeek 的失败分支没有 ⇒ 这几个面板仍按轮询频率打上游（与 `oauth_usage_cache.go:21` 的注释不符）。
10. **持锁后不复查冷却**：`server.go:2666` 只查 `get()`（失败条目一律返回 false），刚写下的失败条目拦不住已在等锁的标签页 ⇒ 并发 N 个标签页各打一次。
11. **配额守护静默窗口**：`quotaQuietWindow` 允许负值直接关闭保护（`quota_keeper.go:297`，API 只裁上界 `quota_keeper_api.go:439`）；定点去重用的是同一个 60 分钟窗口（`quota_keeper.go:544`）⇒ 09:00 与 09:30 两个定点每天必然吃掉后一个，用户手点「立即执行」也会吃掉后续定点。
12. **价格内存表不随 store 剪枝失效**（`pricing.go:274` 只在改价/删价时 reload；`store/model_prices.go:110` 在 `Save` 时剪枝）⇒ `GET /__prices` 可能列出 DB 里已不存在的行。
13. **调试脚手架仍在**：`internal/gateway/zz_dbg40_test.go`（纯 t.Logf 探针）入库前必须删；另有 `.superpowers/`、`docs/superpowers/` 两个未跟踪目录与本次功能无关。
14. **假测试 3 处**：`TestClaudeUsageParsesTheOverageWindow`（自造 map 再断言解析，改坏 key 仍绿）；`claude_oauth_callback_test.go:92` 的 `Contains(body,"<script>alert")` 恒 false；`TestQuietWindowOnlyCountsSuccessfulTraffic` 只断言传参。
15. **权限覆盖缺口**：`quota_keeper_permissions_test.go` 只测「删除/读 run」，`handleUpdateScheduledTask`、`handleRunScheduledTask` 越权路径没测；`/__prices` 的非管理员拒绝路径没测。

---

## 五、本轮做对的地方（要保留）

- **长流 usage 真修好了**：PROBE-B `665,925` 字节流 → `{Input:1234 Output:567}`（旧轮是 0/0 或估算放大 115×）。`streamUsageReader` 逐字节 + `pending` 兜末尾 + `closed()` 统一判定，方向对。
- **转换路径真流式**：PROBE-E 首字节 0ms；`converterStreamWriter` 只在首次写时提交，转换器失败且未写字节时仍可 failover。
- **R4 在 4/5 条腿上落地**：PROBE-D `noCloser err=upstream stream ended after 206 bytes without its closing event`。
- **`isUpstreamStreamInterruptedReader` 语义纠正**（`r.err != nil && !errors.Is(EOF)`）且 5 处调用都移到了读循环之后——历史「读前快照恒 false」的问题消失。
- **请求体上限、CORS 白名单、响应体脱敏**保持完好（`cors.go`/`request_body_limit.go` 本批未动）。
- **令牌刷新收敛**：WorkBuddy/Claude 都只走带锁 `refresh…Locked`，锁内重读路由，成功后 `retire`、失败不动内存；401 只刷一次、只重放一次，且重放带新 token（`protocol_proxy.go:113-141` 三处 sender 已改用参数里的 provider）。
- **`pruneModelPrices` 跳过 `*`**（`store/model_prices.go:107`）：修掉「一次 state save 把全局价删光」的真 bug，且有真实 SQLite 测试。
- **错误响应不再按 body 长度估算 token**（`server.go:1329`）：429/5xx 不再凭空记账。
- **R2 阶梯现在可达**：`providerSoftFailureWindow=10min` > 任何 rest（base 30s×2^n，cap 30min），`exhaustedReset` 取「真正用满的窗口」。

---

## 六、历史清单复查（60 项总账摘要）

| 域 | 结论 | 说明 |
|---|---|---|
| 协议 P1–P8 | ✅ | 本轮 P8/P5/P7 未回退；转换器 4/5 腿补上 clean-EOF |
| P9 redacted_thinking | 🟡 | `convert_claude_responses.go:199` 仍只做占位，未回放签名块 |
| P10 strict/top_k/metadata | 🟡 | `strict` 已双向支持；`top_k`/`metadata` 未转发 |
| P11 单一 IR 架构 | ❌ | 仍两两手写转换 |
| R1 RPM | ✅ | 滑动窗口（8b5542d）保留 |
| R2 冷却阶梯 | ✅ | 窗口 10min 使阶梯可达 |
| R3 分类 | ✅ | billing 已移出 hardQuotaHints |
| R4 流中断续传 | 🟡 | 转换路径 4/5 腿 ✅（P1-1 漏 Chat→Responses）；透传路径因 P0-1 全量缓冲，续传逻辑实际不触发 |
| R5 会话粘性 | ✅ | `session_pins.go` 落库 |
| R6 并发闸门 | 🟡 | 票号队列 + LaneGroup 分组，但 `LaneGroup` 不落库、零等待语义与测试冲突 |
| S1 CORS / S2 body 上限 / S4 脱敏 | ✅ | 本批未回退 |
| S3 认证限流/setup | 🟡 | loopback 守卫在（`admin_auth.go:332`），限流仍是最初版本 |
| S5 空闲超时 | 🟡 | 新增 `idleGuardedBody`（120s），nil timer panic 已修；但出图被它砍（P1-7）、心跳 join 可阻塞（P1-8） |
| A1–A6 | ✅/🟡 | A1 锁、A2 host 白名单、A3 images 拒绝检查、A4 overage、A5 leeway 都在；A4 负缓存覆盖不全（P2-9） |
| 控制台 6 项 | ❌ | `grep -r EventSource web/src` = 0 命中；无重放按钮、无 sessionId 视图、无成本列、无未读角标 |
| N10-1（>256KB 流误判） | ✅ | `closed()` 走全字节；PROBE-B 665KB 流 usage 正确 |
| N24-2 空闲守卫 nil panic | ✅ | timer 在锁内 Stop/置 nil |
| N24-3 4 字节携带漏判 | ✅ | 改为 `hold = maxClosingMarkerLen-1` 前缀拼接 |
| N24-1 流式 | ❌→**P0-1** | 换了个形态回归：不再 `io.ReadAll`，但 fw 缓冲等价 |
| N13-1 `--version` 起服务 | ❌ | 本轮复现（见附注） |

---

## 七、与 magpie 对照（关键差异）

- **流式**：magpie `gateway.go:1396` 是 `io.Copy` 直发、不做 buffer-then-replay；我们为了「中断可换号」把整段吞进 fw.buf，代价是 TTFT 归零收益全丢。magpie 的选择是「切断如实报给客户端」（`stream_cut_test.go`），不做重放。
- **429**：magpie 是**请求内重试**（pause 1/2/4s，Retry-After cap 8s），我们是账号级长冷却。短线限流应该用重试消化，不该把账号标异常——本轮 `cooldownTTL` 已允许 Retry-After 缩短冷却，方向接近，但仍会打「异常」标记。
- **协议**：magpie 单一 IR 解析 → 每协议编码器；我们 11 个转换文件两两配对，P1-1 这种「5 条腿修了 4 条」的漏项是结构必然。

---

## 八、验收门槛（合入前必须全绿）

1. `go build ./...` + `go vet ./...` 绿。
2. `go test ./internal/gateway/ ./internal/store/ ./internal/app/ -count=1` **0 红**（当前 2 红）。
3. PROBE-A 复跑：`firstWrite` 必须显著小于 `total`（透传流式生效），且 `canDefer=false` 与 `true` 行为一致。
4. 生成期间客户端能收到心跳（可用 15s 间隔的 transform 测试断言）：`bytes` 在 `io.Copy` 结束前增长。
5. 长流（>256KB）usage 仍为真实值（PROBE-B 基线 1234/567）。
6. 5 条转换腿全部覆盖 clean-EOF 用例（当前缺 Chat→Responses）。
7. WorkBuddy 未识别 400 的 body 必须原样回给客户端；补一条测试。
8. `LaneGroup`、`SignInAt` 必须在 SQLite 有列且重启可读回；补 migrate + 读写测试。
9. 两个 rate-only lane 测试与实现语义一致（0 红）。
10. 删除 `zz_dbg40_test.go`；`.superpowers/` 等无关目录不入库。
11. **生产部署必须来自 commit**：`vcs.modified=false`，且 `git rev-parse HEAD` 与部署记录一致。
12. 出图路径与 Cursor 分支超时口径一致（≥10min 或按模型配置）。

---

## 九、建议的最小修复顺序

1. **P0-1**：`writeBufferedStream` 在 `io.Copy` 前显式 `hw.WriteHeader(response.StatusCode)`（先提交再流），并决定 canDefer 是否还保留「整段缓冲以便换号」的语义；若保留，必须只在 `fw.canDefer` 时缓冲、末次尝试直发。
2. **两个红测试**与 `acquire` 语义对齐，恢复全绿。
3. **P1-1** 给 Chat→Responses 补 `closed` 跟踪 + `cleanEOFInterruption`。
4. **P1-2** restore 兜底；**P1-3/P1-4** 补 SQLite 列；**P1-5** 价格默认回到具体 Provider 或对 `*/*` 全 0 直接拒绝。
5. 回滚生产到 `80558d4` 干净构建，重新按 commit 部署。

---

## 附注：本轮审查的一个操作副作用（如实记录）

核对线上版本时执行了 `/opt/llm-protocol-gateway/bin/gateway version`，该二进制**不解析 flag**（N13-1 未修），于是启动了一次默认配置的服务实例，读 `/root/.config/llm-protocol-gateway/gateway.db`（`-wal` mtime 变成 04:07:22 PDT），因端口被占随即失败、随 ssh 会话退出，**未触碰生产库 `/var/lib/llm-protocol-gateway/gateway.db`**，也未留下进程（复核 `ps` 只有 905743 一个 gateway）。这条同时是 N13-1 的复现证据：任何 `gateway --version` / `gateway -h` 都会起服务并写库。
