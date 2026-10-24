# round35 — 对抗性代码审查报告

**审查对象**：`C:\guodevelop\llm-protocol-gateway` @ `2045543`（`fix(stream): 截断收尾改用协议错误事件并对齐 magpie`）
**工作区指纹**：`git diff` = `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`（空）｜`git diff --cached` = `e3b0c442…b855`（空）｜拼接指纹 = `e3b0c442…|e3b0c442…`
**git status --porcelain**：0 行（0 改 / 0 增 / 0 删）
**git diff --stat**：无输出（+0 / −0）
**与上一轮（r34）比对**：HEAD 相同、双哈希相同、status 0 行 ⇒ **无增量（连续第 4 轮）**
**复审时间**：2026-10-24 07:40–08:2x +0800｜**只读**：未改任何业务代码，未 commit/push，未写生产

---

## 一 结论先说

1. **不能上线。** 唯一的 P0 缺陷（W29-1 透传流全量缓冲）连续第 **15** 轮未修，且生产二进制仍是 `vcs.modified=true` 的未提交构建（连续第 **14** 轮）——即使现在修好，也没有可回滚、可复现的部署物。
2. **W29-1 本轮用 E1 实跑复验，结论不变**：经 `failoverResponseWriter` 的透传流首字节 = 末字节（`firstWrite=1.2013306s` / `total=1.2013306s` / `writes=1`），`canDefer=true` 与 `false` **完全无差异**；同一份数据用裸 writer 是 `400.5ms` 首字节 / `writes=3`。三个数放在一起证明瓶颈 100% 来自 writer 包装，而不是上游、不是心跳、不是采样抖动。
3. **修复方已实质停摆**：最近一次业务代码 mtime 是 `2026-10-24 02:13:16`（`protocol_proxy.go`，且内容与 HEAD 逐字节一致，只是被 touch），此后约 5.5 小时零提交、零编辑、零 WIP。98 项清单**无一项状态变化**。
4. **生产与仓库的差距比"落后一个提交"更糟**：`vcs.revision=d93ec02`（非 HEAD `2045543`），`vcs.modified=true` ⇒ 线上是「d93ec02 + 若干未提交改动」的混合体。实测：`lane_group` 串 = 0（缺 D29-1 落库修复）、`/__monitor` = 0（监控路由未接线）、`idleGuardedBody` = 3（WIP 独有符号）。**能证明的只有"线上不是任何一个提交"。**
5. **纠错一条历史记录**：r32/r34 记载「生产二进制 monitoring 串 = 0」，本轮实测 `grep -ao` 命中 `internal/monitoring` ×2、`monitor_events` ×7、`collector_stale` ×1。二进制 sha256 与 r34 记录一致（`9a0d86d6…c31f`），**说明是当年测量方法失误（未加 `-a`）而非代码变化**。但 `/__monitor` 路由 = 0 且 5 张 monitor 表全空 ⇒ W30-2「migration 跑了、采集没跑」的结论不变。
6. **新 E4 观察（未定级）**：24h 内 `action='pass_through'` 且 `ttft_ms>0` 的请求 1942 条，其中 1423 条（73.3%）`latency-ttft ≤ 5ms`，另有 519 条差距 >5ms（最大 305 s）。若全量缓冲对所有透传流生效，应接近 100% ⇒ **说明生产里存在不走缓冲的透传路径**（可能是非 SSE 的 images/embeddings 类回包被计入 `pass_through`）。该项**未核查**，不作为 W29-1 的减弱证据，只作为"E4 指标不能分辨流式与非流式"的自证。
7. **回归检测**：本轮零代码变化，因此不可能出现"✅→非✅"的代码回归；逐项比对确认**无回归、无关联回退嫌疑**。上一轮标记为 ✅ 的 22 项全部原样保持。
8. **订单顺序维持 r33 的判断不变**：先让部署可追溯（`vcs.modified=false`），再修 P0。顺序颠倒的话，修完仍然无法证明线上生效。

---

## 二 基线

| 项 | 命令 | 结果 |
|---|---|---|
| 编译 | `go build ./...` | **rc=0**，无输出 |
| 静态检查 | `go vet ./...` | **rc=0**，无输出 |
| 单测（强制重跑） | `go test -count=1 ./internal/gateway/ ./internal/store/ ./internal/app/ ./internal/monitoring/` | `gateway 42.468s ok` / `store 1.372s ok` / `app 1.501s ok` / `monitoring 0.035s ok`，**0 红** |
| 格式 | `gofmt -l internal/ cmd/` | 6 命中：`embeddings_test.go`、`provider_lanes.go`、`router.go`、`session_pins_test.go`、`sqlite.go`、`cmd/gateway/main.go`。**全部为 CRLF 假阳性**——`git show HEAD:internal/gateway/provider_lanes.go \| gofmt -d` 输出为空，`file` 确认 `with CRLF line terminators` |
| 调试残留 | `grep -rn "println(" internal/ cmd/ \| grep -v _test.go` | 0 命中 |
| 测试钩子入仓 | `ls internal/gateway/zz_*.go` | `zz_quota_keeper_e2e_hooks.go` **仍在仓库中**（W30-1，第 6 轮未清） |
| 备份文件 | `find . -name "*.bak"` | `internal/gateway/` 下 5 个（chatgpt_oauth / claude_oauth / cursor_oauth / qoder_pat / qoder_pat_handlers） |
| 提交夹带 | `git ls-files .superpowers` | 5 个文件仍被跟踪，含 `.server.pid` / `.server-stopped`（D31-7） |

> 基线全绿是本轮的**唯一正面事实**，但它证明的是"现有测试没覆盖 P0"，不是"P0 不存在"——PROBE-35 与全绿测试同时成立，就是这个矛盾的直接证据。

---

## 三 新提交审查（D* 问题表）

**本轮无新提交。** `HEAD` 仍为 r31 的 `2045543`（128 文件 / +20673 / −875），该提交的结论已在 r32 完成并固化于账本：
- 做对：R4 真闭环（6 处读后判定 + `cleanEOFInterruption` + 收尾改协议错误帧，对齐 magpie `gateway.go:4940 streamFailure`）。
- 遗留：D31-1…D31-8 八项、W30-1 测试钩子入仓、D31-7 提交卫生。

本轮不重复出 D 表（无新增提交，D 表沿用 r32；账本中 D31/D32 系列各行"本轮变化"列已标注继承来源）。

---

## 四 工作区 WIP 审查

**本轮无 WIP。** `git diff` / `git diff --cached` 双空哈希，`git status --porcelain` 0 行。

唯一需要澄清的噪声：`internal/gateway/protocol_proxy.go` 的 mtime 是 `2026-10-24 02:13:16`（晚于 r34 记录的 02:00 快照时刻），但 `git diff --stat -- internal/gateway/protocol_proxy.go` 无输出 ⇒ **内容与 HEAD 完全一致，属于 touch/编辑器打开重存，不是编辑**。其余业务文件最新 mtime 为 `01:51 / 01:50 / 01:49 / 01:44`，全部早于 r34 快照。

结论：**W 表本轮为空**（无新 WIP 问题）。上一轮 W 系列（W29-x / W30-x）全部转为账本常规项，见第六节。

---

## 五 生产环境只读核查（`173.249.207.202`）

**只读手段**：`systemctl is-active` / `systemctl show -p MainPID` / `ls -l` / `sha256sum` / `grep -ao` / `journalctl` / `sqlite3` 以 `file:…?mode=ro` 打开。**未 restart、未 deploy、未写库、未触发任何上游请求。** 未执行 `gateway version`（N13-1 陷阱）。

### 5.1 部署物

| 项 | 读数 |
|---|---|
| 服务 | `active`，MainPID `911205`（与 r33/r34 同一进程，未重启） |
| 二进制 | `/opt/llm-protocol-gateway/bin/gateway`，16,498,850 B，mtime `2026-10-23 06:32:24 -0700`（= 10-23 21:32 +0800） |
| sha256（前 24） | `9a0d86d67e82c3171b5013e3`（与 r33/r34 完全相同 ⇒ 零部署） |
| 版本归属 | `vcs.revision=d93ec02c9365b20a65d0e92702a92b998b9632d7`，**`vcs.modified=true`** |

**`vcs.modified=true` 连续第 14 轮。** 含义：线上二进制由脏工作区编译，`d93ec02` 只是构建时 HEAD 的标签，**二进制内容与任何提交都不相等**。

### 5.2 符号级差异（证明"线上≠仓库"）

| 特征串 | 命中 | 判读 |
|---|---|---|
| `lane_group` | **0** | 缺 D29-1 的 laneGroup 落库修复（该修复在 d93ec02 之后） |
| `/__monitor` | **0** | 监控路由未接线（W29-5 的修复未上线） |
| `internal/monitoring` | 2 | 包被编译进来（仅供迁移/配置），**不代表路由可用** |
| `monitor_events` | 7 | 仅表名字符串（`store/monitoring_schema.go`） |
| `idleGuardedBody` | 3 | **WIP 独有符号** ⇒ 线上含未提交代码 |
| `shares account lane` | 1 | d93ec02 的 d29 特性存在 |

### 5.3 数据面（只读）

| 项 | 读数 |
|---|---|
| DB | `/var/lib/llm-protocol-gateway/gateway.db`，2,412,736,512 B（2.41 GB），mtime `2026-10-23 12:32`，权限 `-rw-------` |
| providers | 50 行，`disabled=1` 的 **0** 行；`providers` 表**无 `lane_group` 列** |
| api_keys | 9 |
| 限额启用 | `max_concurrent_requests>0` 的 Provider **1 个** ⇒ lane 已生效（但该 Provider 的分组因无列而不可用） |
| monitor 表 | 存在 4 张（`monitor_events` / `monitor_alert_states` / `monitor_event_pushes` / `monitor_baselines`），**每张 0 行**；`checkin_history` 18 行 ⇒ 确认"迁移跑了、采集没跑"（W30-2 不变） |
| 24h 流量 | `convert` 13747 条（`avg(latency−ttft)=1118.8ms` ⇒ 转换路径真流式）｜`pass_through` 1972 条（`ttft>0` 的 1942 条中 1423 条 gap≤5ms，519 条 >5ms，max 304960ms）｜5xx 7 条 |
| 最后一条请求 | `2026-10-23T18:47:49Z` = `2026-10-24 02:47 +0800`，此后约 5 小时无流量 |
| panic（24h） | `journalctl \| grep -ci panic` = **0** |
| 最近日志 | `claude oauth token refreshed proactively`（04:05）、`api key recovered higher-priority provider`（02:23）——均为常规 INFO |

### 5.4 生产侧结论

- **线上没有本轮的修复**（也没有 r31 的 R4 修复、没有 D29-1 的落库修复、没有 monitor 路由），**同时含 WIP 代码**——"既缺修复又带未审计改动"。
- 侥幸未爆的仍是同一条：真正配了限额的 Provider 只有 1 个，lane 相关缺陷（D29-3/4/6）影响面极小。
- `pass_through` 的 73.3%/519 条分裂**未核查**，不下结论（见第一节第 6 条）。

---

## 六 完整排查清单（98 项，全部来自账本，逐项在场）

> 生成方式：脚本从 `.workbuddy/reviews/LEDGER.md` 直接取 98 行打印，未手工转录（脚本与产物见 `round35-evidence/gen_checklist.py`、`checklist-table.md`）。
> 「本轮变化」列：`重跑/重看` = 本轮主 agent 亲自复核（E1 实跑或决定性 file:line）；`继承` = 代码哈希未变，复用上一轮证据（按任务书允许，且注明来源轮次）。

| 编号 | 问题 | 级别 | 状态 | 依据 | 本轮变化 |
|---|---|---|---|---|---|
| P1 | Claude→Chat 流式丢 prompt_tokens | P1 | ✅ | usage 折叠 | 继承（代码哈希未变，r29 结论） |
| P2 | stop_reason 全量映射缺项 | P1 | ✅ | 映射表 | 继承（代码哈希未变，r29 结论） |
| P3 | Chat→Responses 有 tool_calls 清空正文 | P1 | ✅ | 测试 | 继承（代码哈希未变，r29 结论） |
| P4 | Responses→Chat 丢 reasoning 增量 | P1 | ✅ | 测试 | 继承（代码哈希未变，r29 结论） |
| P5 | Responses→Chat 忽略 incomplete | P2 | ✅ | 非流 convert_openai_responses_chat.go:870-872 + convert_tools.go:1749-1760；流 convert_openai_responses_chat_stream.go:661-666→:525-536；stop_reason_mapping_test.go:90-136 可红 | 继承（代码哈希未变，r32 结论） |
| P6 | Chat→Claude block index 错乱 | P1 | ✅ | 测试 | 继承（代码哈希未变，r29 结论） |
| P7 | sanitizeAnthropicToolUseID 碰撞 | P1 | ✅ | 测试 | 继承（代码哈希未变，r29 结论） |
| P8 | Responses→Claude 缺合成 tool_result | P1 | ✅ | 测试 | 继承（代码哈希未变，r29 结论） |
| P9 | redacted_thinking 丢失（占位非回放） | P2 | 🟡 | convert_* | 继承（代码哈希未变，r29 结论） |
| P10 | strict/top_k/stop_sequences 等不转发 | P2 | 🟡 | convert_* | 继承（代码哈希未变，r29 结论） |
| P11 | 两两手写转换 vs magpie 单一 IR | P2 | ❌ | 架构未动 | 继承（代码哈希未变，r29 结论） |
| R1 | 无 RPM 限流 | P1 | ✅ | 8b5542d 滑动窗口 | 继承（代码哈希未变，r21 结论） |
| R2 | 冷却不看上游 Retry-After | P1 | ✅ | 阶梯可达 | 继承（代码哈希未变，r26 结论） |
| R3 | billing 归入 hardQuota | P1 | ✅ | 代码 | 继承（代码哈希未变，r8 结论） |
| R4 | 流截断不能续/被当正常结束 | P0 | ✅ | 6 处读后判定 + 收尾改协议错误帧，测试绿 | 继承（代码哈希未变，r31 结论） |
| R5 | 会话粘性不落库 | P1 | ✅ | session_pins | 继承（代码哈希未变，r24 结论） |
| R6 | 并发闸门非 FIFO | P1 | 🟡 | D29-3/D29-4 仍在 | 经 D29-3/D29-4 确认（E2），结论不变 |
| S1 | 管理 API `ACAO: *` | P0 | ✅ | cors 白名单 | 继承（代码哈希未变，r24 结论） |
| S2 | 请求体无上限 | P1 | ✅ | request_body_limit | 继承（代码哈希未变，r24 结论） |
| S3 | 登录/setup 无限流 | P1 | 🟡 | auth_throttle 死代码/TOCTOU；setup 在 :453 早于解码，login 在 :502 晚于 :496 解码 | 重看限流与解码顺序（E2），结论不变 |
| S4 | 日志正文不脱敏 | P1 | ✅ | log_scrub | 继承（代码哈希未变，r18 结论） |
| S5 | 首包后无空闲超时 | P1 | 🟡 | 守卫有，但心跳被 W29-3 缓冲吞掉 | 同 W29-3 同因（E2），结论不变 |
| A1 | Trae/WorkBuddy/Cursor 刷新无锁 | P1 | ✅ | oauth_renew_lock | 继承（代码哈希未变，r24 结论） |
| A2 | 子串匹配 host | P1 | ✅ | baseURLHostIn | 继承（代码哈希未变，r24 结论） |
| A3 | /v1/images 路由误判 | P2 | ❌ | server.go:3099-3105 `resolveProviderImageURL` 用 `Contains("/images/generations")` | 重看 Contains 拼接（E2），结论不变 |
| A4 | Claude overage / 负缓存 | P1 | ✅ | 负缓存 5/5 | 继承（代码哈希未变，r26 结论） |
| A5 | Cursor 配额提前量 | P2 | ✅ | 纠错：已达标 | 继承（代码哈希未变，r10 结论） |
| A6 | 出图超时被 120s 砍 | P2 | ❌ | server.go:5712/5749 均 Timeout:0 | 继承（代码哈希未变，r32 结论） |
| H-1 | Claude output_config.format 是否该发 name | P1 | ❌ | convert_response_format.go:40-43 写 name；convert_responses_fields.go:71-72 注释明写「Claude GA 拒收 format.name」 | 重看两处互斥注释（E2），结论不变 |
| H-2 | `response_format=json_object` 原样直传 Claude | P1 | ❌ | convert_response_format.go:18-20 直出 `{"type":"json_object"}`；convert_responses_fields.go:73-80 明说无此模式并降级 | 重看两处互斥实现（E2），结论不变 |
| H-3 | thinking 覆盖 output_config | P1 | ❌ | convert_claude_openai.go:322-325 整块赋值；顺序 :208 写 format → :213 thinking | 重看覆盖顺序（E2），结论不变 |
| H-4 | Responses allowed_tools 塞进 Claude tool_choice | P1 | 🟡 | convert_responses_claude.go:496-516 构造该键并收窄 tools | 重看构造点（E2），结论不变 |
| H-5 | cache marks 白名单漏 tool_result | P1 | ❌ | convert_claude_cache_marks.go:93-101 只认 text/image/document | 重看白名单（E2），结论不变 |
| H-6 | thinking 五连（预算补正/glm none/unknown 模型） | P1 | 🟡 | PROBE-34H（E1）：claude-3.5-sonnet 等 known=false → 删 thinking + max_tokens 200→2248 | 重看正则 + N34-1（E1 继承），结论不变 |
| H-7 | images 拒绝检查 route.ProviderID 而非 decision.ProviderID | P1 | ❌ | server.go:6255 `router.ProviderByID(route.ProviderID)` | 重看落点（E2），结论不变 |
| H-8 | 管理员 Key 链不过滤 | P1 | ❌ | user_isolation.go:123-125 `OwnerUserID==""` 直接 return chain | 重看提前返回（E2），结论不变 |
| H-9 | workbuddy 11128 校验矩阵 | P1 | ✅ | workbuddy.go:857 覆盖 system+assistant、:972-983 developer→system | 继承（代码哈希未变，r32 结论） |
| M-1 | Claude→Chat 无条件 stop | P1 | ✅ | 有条件守卫（r10 纠错） | 继承（已关闭，勿再报） |
| M-19 | 错误响应被估算计费 | P1 | ✅ | 已修 | 继承（代码哈希未变，r21 结论） |
| N-1 | workbuddy_ai 凭证 exp 空→永不续期 | P1 | ✅ | 401 自愈关闭 | 继承（代码哈希未变，r20 结论） |
| N10-1 | 透传 >256KB 流被判截断 | P0 | ✅ | 判据改为 `usage.closed()` 全字节 | 继承（代码哈希未变，r29 结论） |
| N10-2 | 中断收尾帧写裸 w 绕过锁 | P1 | ✅ | 改走 hw | 继承（代码哈希未变，r29 结论） |
| N11-1 | 长流 usage 归零→估算放大 115× | P0 | ✅ | streamUsageReader 逐字节折叠 | 继承（代码哈希未变，r29 结论） |
| N11-2 | 心跳 stop 不 join | P1 | ✅ | 有界 join 2s | 继承（代码哈希未变，r29 结论） |
| N11-3 | 透传 Claude 收尾漏 content_block_stop | P2 | ✅ | openBlocks 逐个 close | 继承（代码哈希未变，r29 结论） |
| N11-4 | 透传 Responses 收尾帧缺 id/model/seq | P2 | ✅ | responsesStreamIdentity | 继承（代码哈希未变，r29 结论） |
| N12-2 | 日桶用服务器时区 | P2 | ⚪ | 生产 unit TZ=Asia/Shanghai | 继承（已举证不适用） |
| N12-3 | 登录先解码后限流 + 全表扫 | P3 | 🟡 | 部分 | 继承（代码哈希未变，r18 结论） |
| N13-1 | `gateway version` 零 flag 解析会起服务 | P2 | ❌ | 复现 | 继承（代码哈希未变，r29 结论） |
| N13-2 | /__usage-analysis 无上限 | P2 | ❌ | server.go:339 路由，无 limit/分页 | 重看路由无上限（E2），结论不变 |
| N13-3 | 容器无 TZ 且 verify 脚本期望 LA | P3 | ❌ | 未改 | 继承（代码哈希未变，r13 结论） |
| N14-2 | 转换路径中断判定恒 false | P0 | ✅ | 读后判定 | 继承（代码哈希未变，r29 结论） |
| N23-1 | canDefer 在流式路径是死变量 | P1 | ❌ | 与 W29-1 同因（PROBE-35 本轮实测 canDefer 真假首字节均 1.2013s、writes 均 1） | 同 W29-1 同因（E1），结论不变 |
| N23-2/3 | 两套完成判定结论相反 | P2 | 🟡 | 部分统一 | 继承（代码哈希未变，r29 结论） |
| N23-5 | stream+JSON 回包 usage=0 | P2 | ❌ | usage.go:535-544 fold!=nil 时永不回落 | 继承（代码哈希未变，r32 结论） |
| N24-3 | streamEndReader 只携带 4 字节 | P2 | ✅ | 按 maxClosingMarkerLen-1 | 继承（代码哈希未变，r29 结论） |
| N24-5 | 两套完成判定无回声守卫 | P3 | 🟡 | 回声守卫仅末 64B | 继承（代码哈希未变，r29 结论） |
| N25-1 | 中断谓词语义反了 | P1 | ✅ | 已在 isUpstreamStreamInterruption | 继承（代码哈希未变，r29 结论） |
| N25-3 | scanResponses 非 Responses 流全量累积 | P2 | 🟡 | 未复跑 | 继承（代码哈希未变，r30 结论） |
| N25-5 | 缓冲路径零心跳 | P2 | ❌ | 探针 flushes=0，心跳落 buf | 继承（并入 W29-3） |
| D29-1 | laneGroup 完全不落库 | P0 | ✅ | sqlite.go:239 列 + :641/:766 读写 + 两个测试绿 | 继承（代码哈希未变，r30 结论） |
| D29-2 | Retry-After 出口按行号查分组 | P1 | 🟡 | server.go:4492 仍 `retryAfterSeconds(provider.ID,…)` | 重看剩余调用点（E2），结论不变 |
| D29-3 | 组内限额取"最后到达那行" | P1 | ❌ | provider_lanes.go:124-139 laneFor 每次 acquire 覆盖 max/queueCap/rpm | 重看 laneFor（E2），结论不变 |
| D29-4 | 组内 0/0 行整组绕过 | P1 | ❌ | provider_lanes.go:556-561 max<=0&&rpm<=0 提前返回空 release | 重看提前返回（E2），结论不变 |
| D29-5 | 大小写：注释说不敏感，实现精确匹配 | P2 | ❌ | providerLaneKey:542-547 只 TrimSpace | 重看键构造（E2），结论不变 |
| D29-6 | providerInFlight 按行号查 | P2 | ❌ | provider_lanes.go:565-578 用原始 providerID 取 lanes[] | 重看取 lane（E2），结论不变 |
| D29-7 | d93ec02 夹带 failover 行为变更 | P3 | ❌ | 提交粒度 | 继承（代码哈希未变，r29 结论） |
| D29-8 | lane 只增不删 | P3 | ❌ | 无回收 | 继承（代码哈希未变，r29 结论） |
| W29-1 | 透传流全量缓冲（TTFT=总时长） | P0 | ❌ | PROBE-35（E1）：canDefer 真假均 firstWrite=1.2013s=total、writes=1；裸 writer 400.5ms、writes=3。成因 passthrough_stream.go:278-283 无 canDefer、:539 无 WriteHeader、provider_failover.go:347-357 只落 buf、:368-373 Flush 空操作 | **重跑 PROBE-35（E1），结论不变（连续第 15 轮）** |
| W29-2 | passthrough 下响应再复制进 fw.body（无上限） | P1 | ❌ | provider_failover.go:359-362 无条件 `w.body.Write(p[:n])`；Prefix 仅测试调用、StreamLen/streamClosed 零调用 | 重看调用点（E2），结论不变 |
| W29-3 | 心跳进缓冲区，流中收不到 ping | P1 | ❌ | 同 W29-1：hw→fw.Flush 在 !passthrough 时是空操作 | 同 W29-1 同因（E2），结论不变 |
| W29-4 | 红测试 TestConverterStreamWriterKeepsTheStatusItWasGiven | P1 | ✅ | 该测试本机绿 | 继承（代码哈希未变，r30 结论） |
| W29-5 | internal/monitoring 整包未接线 | P2 | ✅ | 已随 2045543 固化 | 继承（代码哈希未变，r31 结论） |
| W29-6 | scanResponses 非 Responses 流全量累积 | P2 | ✅ | stream_end_reader.go:780-795 尾部封顶 | 继承（代码哈希未变，r34 结论） |
| W29-7 | 回声守卫只看末 64B | P2 | 🟡 | 代码 | 继承（代码哈希未变，r29 结论） |
| W29-9 | streamUsageFolder reflect 分派脆 | P3 | ❌ | 代码 | 继承（代码哈希未变，r29 结论） |
| W29-11 | failoverResponseWriter 死代码 | P3 | 🟡 | Prefix 仅测试调用 | 继承（代码哈希未变，r30 结论） |
| W30-1 | zz_quota_keeper_e2e_hooks.go 生产源码里的测试钩子 | P3 | ❌ | 文件仍在仓库，导出 HandleRestoreProviderForTest | 重看入仓文件（E2），结论不变 |
| W30-2 | 生产库已建 monitor_* 表但全空 | P3 | 新增 | 生产只读：4 张 monitor 表各 0 行；`/__monitor` 串=0 | 继承（r32 观察，r35 复测一致） |
| W30-3 | /__monitor 路由无 CSRF/Origin 校验 | P2 | ❌ | monitoring_handlers.go:118-127 全部 requireAdmin；cors 只写响应头 | 重看 monitorGate（E2），结论不变 |
| D31-1 | 用户暂停的额度保活任务被 keeper 每分钟复活 | P1 | ❌ 新增 | quota_keeper.go:485-489 判据只判 `Enabled`/`len(live)==0`，注释自称"人工暂停永不被撤销" | 重看判据（E2），结论不变 |
| D31-2 | 监控中心不可停止 | P2 | ❌ 新增 | app/runtime.go:353；monitoring/service.go:38-51 ctx 永不取消 | 继承（代码哈希未变，r32 结论） |
| D31-3 | 引擎吞事件写库错误 | P2 | ❌ 新增 | monitoring/engine.go:315-324 | 继承（代码哈希未变，r32 结论） |
| D31-4 | OAuth 用量缓存 key 不含账号身份 | P1 | ❌ 新增 | oauth_usage_refresh.go:187/194/234/284 `"claude:"+providerID`，无账号维度 | 重看缓存键（E2），结论不变 |
| D31-5 | ChatGPT 续期等锁后仍用旧快照 refresh_token | P1 | 🟡 新增 | chatgpt_oauth.go:433-436 重读仅用于早退；:437 仍花旧 RT | 重看花 RT 处（E2），结论不变 |
| D31-6 | 负缓存读侧死代码 | P2 | ❌ 新增 | oauth_usage_cache.go:64-72/116-131 | 继承（代码哈希未变，r32 结论） |
| D31-7 | 提交卫生：.superpowers / 大文档 / 128 文件单提交 | P3 | ❌ 新增 | `git ls-files .superpowers` 5 文件仍在 | git ls-files（E2），结论不变 |
| D31-8 | checkin_history 无 account_key | P3 | ❌ 新增 | store/checkin.go:57-65 PK(provider_id,day) | 继承（代码哈希未变，r32 结论） |
| D32-1 | Chat→Claude 完全丢弃 `stop`（chatStopToClaudeSequences 死代码） | P1 | ❌ 新增 | convert_tools.go:1165-1175 仅定义，全包 0 调用点 | 重看调用点（E2），结论不变 |
| D32-2 | Claude→OpenAI 回复路径不做 declared 名回填 | P2 | ❌ 新增 | convert_tools.go:1272-1311 不调 :1337 restoreDeclaredToolName | 继承（代码哈希未变，r32 结论） |
| D32-3 | thinking 预算与 max_tokens 无钳制 | P2 | ❌ 新增 | convert_claude_openai.go:325-331 写 16k/32k，:196 写 max_tokens，从不比较 | 继承（代码哈希未变，r32 结论） |
| D32-4 | 隧道 token 明文落 cloudflared.log（0644） | P1 | ❌ 新增 | tunnel/manager.go:300-301 写 `strings.Join(args," ")`（argv 含 `--token`）；:343 `0o644` | 重看 argv 落盘（E2），结论不变 |
| D32-5 | 出图按 base64 len/4 估算入账，价格表无图像行 | P2 | ❌ 新增 | server.go:5755 恒空 usage；`grep -i image pricing.go` 空 | 继承（代码哈希未变，r33 结论） |
| D32-6 | safety_identifier 全仓 0 命中 | P2 | ❌ 新增 | `grep -rn safety_identifier internal/` 空 | 继承（代码哈希未变，r33 结论） |
| D32-7 | resumeTasksForProvider 无生产调用点 | P3 | ❌ 新增 | quota_keeper.go:507-528，仅测试调用 | 继承（代码哈希未变，r33 结论） |
| D32-8 | unflagWorkBuddyMessages 的 string 分支丢 tool_calls 轮 | P3 | ❌ 新增 | workbuddy.go:861-871 vs :878-887 | 继承（代码哈希未变，r33 结论） |
| C1 | 控制台 6 项（回放/sessionId/角标/成本列/事件重放/失败重发）全未实现 | P2 | ❌ 新增 | 本轮 grep：`EventSource=0`、`replay=0`、`costUsd/cost_usd=0`、`resend/retryRequest=0` | 重跑 grep（E2），结论不变 |
| N34-1 | Claude 型号名正则漏 3.x 与厂商前缀写法 ⇒ 规则表失效并擅自放大 max_tokens | P2 | ❌ 新增 | claude_thinking_off.go:28 正则 `(?:claude-)?(opus\|sonnet\|…)-(\d{1,2})` 要求家族名在数字前 | 继承 PROBE-34H（E1，代码哈希未变） |

**清单自检**：账本 98 行 / 报告 98 行，`missing=[]`（脚本比对）。⚪ 占比 1/98 = 1.0%，远低于 30% 阈值。

---

## 七 回退与假修复

- **`回归` 项：无。** 本轮 HEAD 与工作区均与 r34 逐字节相同（双空哈希），代码层面不存在"被改回去"的可能；22 项 ✅ 全部原样。
- **`关联回退嫌疑`：无。**
- **假修复/失效修复（跨轮累计，非本轮新增）**，仅列仍生效者：
  - `chatStopToClaudeSequences`（D32-1）：函数存在、注释齐全、**零调用点**——典型的"看起来修了"。
  - `resolveProviderImageURL`（A3）：有函数、被调用，但 `Contains` 判据在带 query 的 BaseURL 上拼错。
  - `monitoring`：整包编译进二进制、迁移会建表，但 `/__monitor` 路由 = 0 ⇒ 线上能力不存在（W29-5 的 ✅ 只在源码层面成立，部署层面仍 ✅→未生效）。
- **本轮新证据推翻了什么**：只推翻了一条**测量结论**（r32/r34「生产二进制无 monitoring 串」），不推翻任何缺陷判定。

---

## 八 下轮验收门槛（逐条可机械验证）

| # | 断言 | 验证命令/方法 | 通过判据 |
|---|---|---|---|
| 1 | 部署可追溯 | `ssh … grep -ao "vcs.modified=[a-z]*" /opt/…/gateway` | 输出 `vcs.modified=false` |
| 2 | 线上即 HEAD | `vcs.revision` 前缀 == `git rev-parse HEAD` 前 12 位 | 完全相等 |
| 3 | W29-1 已修（E1） | 复跑 PROBE-35 | `failover-canDefer-{true,false}` 的 `firstWrite < total/2` **且** `writes ≥ 3` |
| 4 | 首字节不被缓冲（E1 补充） | 同上，`canDefer=false` 也应接近 400ms | `firstWrite ≤ 600ms`（3×400ms 分块） |
| 5 | 心跳能出网 | 在 PROBE-35 里统计 `flushes` | `flushes ≥ 1`（1.2s 流、15s 间隔可用更短间隔的派生探针） |
| 6 | fw.body 不再无界 | `grep -n "w.body.Write" internal/gateway/provider_failover.go` + 周边是否有上限/条件 | 要么条件化（仅 passthrough 且需要回放时），要么有 cap |
| 7 | D29-3 修复 | `grep -n "lane.max = max" internal/gateway/provider_lanes.go` 的上下文 | 组 lane 尺寸只由组级配置决定，不被单行覆盖 |
| 8 | D29-4 修复 | `grep -n "max <= 0 && rpm <= 0" internal/gateway/provider_lanes.go` | 该提前返回前必须先解析分组键并取组限额 |
| 9 | D29-2 收尾 | `grep -n "retryAfterSeconds(" internal/gateway/server.go` | 无按行号调用残留 |
| 10 | D31-4 修复 | `grep -n "cacheKey := " internal/gateway/oauth_usage_refresh.go` | 键含账号身份（token 指纹/账号 ID） |
| 11 | H-1/H-2 一致 | 对同一 schema 分别走 `convert_response_format.go` 与 `convert_responses_fields.go` 的出参比对测试 | 两处产出一致；若决定不写 `name`，则 `:40-43` 与 `:71-72` 注释同步 |
| 12 | H-3 修复 | `grep -n "output_config" internal/gateway/convert_claude_openai.go` | 写 `effort` 时不得整块覆盖已有 `format` |
| 13 | H-5 修复 | 构造 tool_result 结尾的 Claude 请求 | 断点落在 tool_result 上（或明确记录为不支持并给出替代） |
| 14 | H-8 修复 | 构造 admin Key + 含 disabled/deleted Provider 的链 | 链被过滤 |
| 15 | D32-1 修复 | `grep -rn "chatStopToClaudeSequences(" internal/` | ≥1 个非测试调用点 |
| 16 | D32-4 修复 | `grep -n "Join(args" internal/tunnel/manager.go`；检查日志文件权限 | 不落 token；权限 ≤0600 |
| 17 | 卫生 | `ls internal/gateway/zz_*.go internal/gateway/*.bak`；`git ls-files .superpowers` | 全部为空 |
| 18 | S3 修复 | `sed -n '490,505p' internal/gateway/admin_auth.go` | `rejectIfAuthThrottled` 行号 < `Decode` 行号 |
| 19 | 控制台 C1 | 4 条 grep（EventSource/replay/costUsd/resend） | 至少回放与成本列有实现 |
| 20 | 本轮前提 | `git status --porcelain` / `git diff \| sha256sum` | 修复必须落在**提交**上，不再以 WIP 交付 |

---

## 附 覆盖度声明

| 模块 | 本模块文件总数 | 本轮直接重看/重跑 | 覆盖说明 |
|---|---|---|---|
| M0 基线与卫生 | 全仓 | build/vet/test/gofmt/卫生 5 类命令全跑 | 本轮全覆盖（基线 100%） |
| M1 协议转换·请求 | `internal/gateway/*.go` 非测试 142 | 6（`convert_response_format.go`、`convert_responses_fields.go`、`convert_claude_openai.go`、`convert_claude_cache_marks.go`、`convert_tools.go`、`convert_responses_claude.go`） | 其余**继承 r34 覆盖**；未逐行重扫 |
| M2 协议转换·流式 | 同上 | 3（`passthrough_stream.go`、`provider_failover.go`、`stream_end_reader.go` 相关段）+ PROBE-35 实跑 | W29-1/2/3 与 N23-1 有 E1/E2；其余继承 |
| M3 调度/路由/并发 | 同上 | 1（`provider_lanes.go` 全关键段） | D29-3/4/5/6 逐行确认；lane 排队时序未重跑探针 |
| M4 Provider/账号/OAuth | 同上 | 4（`chatgpt_oauth.go`、`oauth_usage_cache.go`、`oauth_usage_refresh.go`、`quota_keeper.go`） | D31-1/4/5 确认；A6/A3 仅看落点 |
| M5 计费/用量/日志 | `internal/gateway` + `internal/store` 28 | 0 直接重看 | **全部继承 r32/r33 结论（未核查本轮）** |
| M6 安全/鉴权/隔离 | `internal/gateway` | 3（`admin_auth.go`、`auth_throttle.go` grep、`user_isolation.go`） | S3/H-8 确认；限流 TOCTOU 未压测 |
| M7 存储/迁移 | `internal/store` 28 | 0 | 继承（本轮仅生产库只读校验 `lane_group` 列不存在） |
| M8 前端控制台 | `web/src` 22 ts/tsx | 0 文件（仅 4 条 grep） | **未核查**：未跑 measure/shot，未做几何与时区实测 |
| M9 隧道/系统面 | `internal/tunnel` 4 | 1（`manager.go` 关键段） | D32-4 确认；未触发真实隧道 |

**明确未核查**：
1. M8 前端全部（无代码变化，且本轮时间盒优先 P0/P1）——故 C1 只有 grep 级 E2，**未做几何验证**。
2. M5 计费/用量的运行时行为（N23-5、N11-1 的回归风险）——继承结论。
3. 生产 `pass_through` 73.3% / 519 条 gap>5ms 的分裂成因（需区分 SSE 与非 SSE 回包，DB 无 stream 列）。
4. `internal/monitoring` 的运行期行为（线上路由未接，无法观测）。
5. H-4 的上游 schema 实证（仍缺）。

---

## 收尾自检

- [x] `git status --porcelain` 只剩 `.workbuddy/reviews/` 产物（结束时复核，0 行业务残留）
- [x] 无临时 worktree 残留（`git worktree list` 仅主树）
- [x] 账本已更新，账本 98 个 ID 全部出现在第六节（脚本比对 `missing=[]`）
- [x] 每个 ✅ 都能指到 E1/E2/E3 之一（本轮无新建 ✅，历史 ✅ 均带原证据轮次）
- [x] 覆盖度声明已填，未核查项已列举（5 条）
- [x] 无 commit / push / 生产写操作发生

---

## 给修复 agent 的订单

> 只描述改哪里、改成什么语义、用什么证据验收。不写代码。顺序即优先级，**不要跳步**。

### P0-1（治理，必须先做）部署可追溯
- 改哪里：构建与部署流程（不是业务代码）。当前 `vcs.modified=true` 已连续 14 轮。
- 改成什么语义：**只有 `git status --porcelain` 为空时才允许产出部署二进制**；产物必须 `vcs.modified=false` 且 `vcs.revision` == 部署时 HEAD。
- 验收：#1、#2 两条门槛同时通过；并在报告里贴出 `vcs.revision` 与 `git rev-parse HEAD` 的对照。

### P0-2 透传流恢复真流式（W29-1 / N23-1 / W29-3 / S5）
- 改哪里：`internal/gateway/passthrough_stream.go:278-283`（分派条件）、`:539` 起的 `writeBufferedStream`、`internal/gateway/provider_failover.go:347-373`（`Write`/`Flush`）。
- 改成什么语义：**默认直通**。仅当"确实还需要回放/重试"时才进缓冲；进缓冲时也必须**先提交响应头**（`WriteHeader` 或等价的 commit），让 `Flush` 有东西可 flush。三件事必须同批完成：① 首字节立即发出；② 心跳能真正到达客户端；③ `fw.body` 有上限（只保留可回放所需的前缀，或设定明确 cap）。
- 约束：改的是**一条路径**，不要为每个协议各写一套；这套语义要对 Chat/Claude/Responses 三种透传同时成立。
- 验收：#3、#4、#5、#6。**#3 是硬门槛**——`canDefer` 真/假两种情况下首字节都必须早于总时长的一半。

### P0-3 补齐 lane 分组语义（D29-3 / D29-4 / D29-2 / D29-6）
- 改哪里：`provider_lanes.go:124-139`（`laneFor` 覆盖）、`:556-561`（0/0 提前返回）、`:565-578`（`providerInFlight`）、`server.go:4492`。
- 改成什么语义：**组限额只能由组级来源决定**，任何单行 acquire 不得覆盖；0/0 的行不能成为"整组免检"的通行证；所有对外读数（Retry-After、in-flight）必须用**同一个键**取 lane。
- 验收：#7、#8、#9。

### P1-1 协议字段一致性（H-1 / H-2 / H-3 / H-5 / D32-1）
- 改哪里：`convert_response_format.go:18-43`、`convert_responses_fields.go:71-80`、`convert_claude_openai.go:208/213/322-325`、`convert_claude_cache_marks.go:93-101`、`convert_tools.go:1165-1175`。
- 改成什么语义：同一份语义在两条路径上必须产出**同一个** Claude 请求体——`format` 是否带 `name` 只能有一个答案（并同步改注释）；`json_object` 要么降级为宽松 schema（与 `convert_responses_fields.go` 一致），要么显式记录为不支持；写 `thinking` 时**合并** `output_config` 而不是整块替换；缓存断点白名单补 `tool_result`；`stop` 要么真转发（接上死函数），要么在文档与注释里删掉承诺。
- 验收：#11、#12、#13、#15。

### P1-2 账号与密钥边界（H-7 / H-8 / D31-4 / D31-1 / D31-5 / D32-4）
- 改哪里：`server.go:6255`、`user_isolation.go:123-125`、`oauth_usage_refresh.go:187/194/234/284`、`quota_keeper.go:485-489`、`chatgpt_oauth.go:433-442`、`tunnel/manager.go:300-301/343`。
- 改成什么语义：拒绝判断必须看**实际要发的那个 Provider**（`decision.ProviderID`）；管理员 Key 的链也要过 `disabled/deleted/协议匹配` 过滤；OAuth 用量缓存键必须含账号身份；"人工暂停"必须能与"额度恢复"区分（加显式状态，而不是靠 `len(live)` 推断）；续期必须花**锁后重读**的那份 RT；隧道 token 不得进日志，日志权限 ≤0600。
- 验收：#10、#14、#16。

### P2 体验与一致性
- S3：把 `rejectIfAuthThrottled` 挪到 `Decode` 之前（#18）。
- A3：`resolveProviderImageURL` 改用 `HasSuffix`/URL 解析，与 `resolveProviderEndpointURL` 同判据。
- D29-5：键构造加 `ToLower`（或改注释承认精确匹配）。
- C1 控制台 6 项：至少先补"事件回放"与"成本列"（#19）。

### P3 卫生
- 删 `internal/gateway/zz_quota_keeper_e2e_hooks.go`（改为 `_test.go` 内的等价物或 build tag）。
- 删 5 个 `*.bak`；`git rm -r --cached .superpowers` 并加 `.gitignore`。
- 清 `gofmt` CRLF 假阳性来源（统一 `.gitattributes` 的 `eol=lf`），让 `gofmt -l` 能当门禁用。
- 验收：#17、#20。

**最后一句话**：本轮 98 项里没有任何一项因为"又审了一轮"而改变。真正拦路的不是缺陷数量，而是**交付方式**——连续 14 轮 `vcs.modified=true` 意味着每一次"修好了"都无法被证明。先把这件事解决，再谈 P0。
