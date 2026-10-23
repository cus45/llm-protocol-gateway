# 监控中心设计

- 日期：2026-10-23
- 状态：待用户审阅
- 范围：`llm-protocol-gateway` 的告警模块与自检模块合并重构为「监控中心」，覆盖账号健康、续期、额度/签到、链路健康、使用风控、探测调度与多渠道告警推送。

## 1. 背景与问题

现状有两个割裂的模块：

- **告警**：每 2 分钟扫描一次，只有「API Key 多 IP」和「并发 IP」两条规则，规则写死在代码里；推送只支持 Telegram；`alerts` 表是 API Key 专用结构，无法表达账号、额度、链路事件。
- **自检**：手动触发的一次性内存任务，每 provider 跑 6 条 CLI 用例；任务重启即丢，没有调度，也没有和历史健康数据打通。

结果是：账号 token 过期/续期失败、额度耗尽、签到断签、上游错误率突增、空响应等问题只能靠用户自己发现；告警规则少且不实用；两个页面的布局和视觉也不能满足使用。

## 2. 目标与非目标

### 2.1 目标

1. 凭证类问题（token 到期、续期失败）在 2 分钟内发现；请求侧账号失效（401/403）与链路问题（成功率、429、5xx、空响应、延迟劣化）在 5–10 分钟的规则窗口内发现。
2. 账号、额度、链路、风控、探测事件进入同一套事件模型，可按对象、级别、规则、时间回溯。
3. 告警分级：P0 立即推送，P1 聚合推送，P2 只进面板；恢复通知；冷却、去重、抖动抑制。
4. 推送渠道可配置、可扩展：飞书、钉钉、企业微信、Telegram。
5. 自检改为自动调度：事件驱动轻探测 + 每日轻探测 + 每周 CLI 深检；不再要求人工点击。
6. 监控中心单入口、页内 5 个 tab；运维盘气质、跟随明暗主题；三档屏幕可用。
7. 监控只发现和展示，不自动修改 provider/route/key 配置。

### 2.2 非目标

- 不接入 Prometheus/Grafana/Alertmanager 等外部监控栈。
- 不做自动隔离、自动停用、自动切换等自动处置。
- 不做按规则的渠道矩阵（首版只有渠道级最低级别；按规则路由留作后续扩展）。
- 不做 P2 推送（P2 仅面板；恢复通知只对 P0/P1）。
- 不采集真实「流截断」标记（见 §18 风险）；首版用空响应和伪 200 覆盖常见症状。
- 不改动请求代理主链路的行为、重试策略和调度策略。

## 3. 已确认决策

| 主题 | 决策 |
|---|---|
| 主目标 | 全链路健康优先 |
| 数据来源 | 请求日志 + 低频主动探测 |
| 告警范围 | 账号不可用、额度与签到、上游与链路、异常使用风控；多维度 |
| 噪音控制 | 分级 + 聚合 |
| 自检 | 自动检测，规则由人或系统调度 |
| 推送渠道 | 飞书、钉钉、企业微信、Telegram；渠道层可扩展 |
| 首页布局 | A + C 组合：上方总览 + 账号矩阵，下方统一事件流 |
| 视觉 | 运维盘气质 + 跟随当前明暗主题 |
| 导航 | 单入口「监控中心」+ 页内 tabs |
| 规则管理 | 内置模板 + 可调阈值/窗口/级别 |
| 自动处置 | 只告警，不改配置 |
| 探测方式 | 轻探测每日 + CLI 深检每周 |
| 技术路线 | 统一监控子系统（统一事件表 + 规则引擎 + 渠道适配器） |

## 4. 术语

- **账号**：一个 provider 记录；OAuth provider 通常对应一个订阅账号，API Key provider 对应一个中转/官方 Key。
- **对象（target）**：`provider`、`api_key`、`model`、`gateway`。
- **fingerprint**：告警去重键，格式 `rule_key + "|" + target_type + ":" + target_id`。
- **轻探测**：网关内发起的最小 chat/tools 请求，用量极小，带 `monitor_probe` 标记。
- **CLI 深检**：沿用现有自检的 6 条真实客户端用例（OpenCode/Codex/Claude × chat/tools）。
- **P0/P1/P2**：致命/重要/提示；`info` 只进事件流，不参与告警。

## 5. 总体架构

新增 `internal/monitoring` 包，包含纯规则/状态/推送逻辑；`internal/gateway` 负责数据源、探测执行和 HTTP 接线，避免循环依赖。

```
request_logs ─┐
providers ────┼─> Collector ─> Observation ─> RuleEngine ─> monitor_events / states
checkins ─────┤                                   │
quota APIs ───┘                                   v
                                            Dispatcher ─> channel adapters
Prober (light / CLI) ─> monitor_events ──────────┘        (feishu/dingtalk/wecom/telegram)
```

### 5.1 模块职责

- **Collector**
  - 每 60 秒：按 provider 聚合最近 5/10/15 分钟请求窗口（总数、2xx、401/403、429、5xx/timeout、伪 200、空响应、P50/P95 TTFT、P95 延迟、成本），读取 provider token 到期与 health_status；同时刷新采集器心跳。
  - 每 15 分钟：拉取支持厂商的额度/积分快照（带 `observed_at`），写 `provider_metric_samples`；对支持模型列表的 provider 拉一次模型列表并记 probe 事件。
  - 每小时：按过去 7 天同一小时计算每 provider 的 TTFT P95 基线，写 `monitor_baselines`。
  - 每日 09:30（配置时区，默认 Asia/Shanghai）：汇总签到状态，写 `checkin_history` 的当日行（如果当天还未写入）。
  - 采集器只读 SQLite 和厂商只读接口；不阻塞请求路径；单次运行失败只记日志。
- **RuleEngine**
  - 持有 18 条内置规则的默认值与当前配置；每条规则有独立的评估函数和参数。
  - 每次评估产出 `firing / normal / unavailable`；维护 `monitor_alert_states` 的状态、次数、首次/最近时间、通知时间、抑制时间。
  - 只有状态发生 `normal/resolved → firing` 时插入 firing 事件；firing 期间只更新状态；恢复时插入 resolved 事件。
- **Prober**
  - 事件驱动轻探测、每日轻探测、每周 CLI 深检；手动触发作为辅助入口。
  - 结果写 `monitor_events`（kind=probe），并作为 `probe_failed` 规则的输入。
  - 探测请求走临时 route/key，日志 `access_source=monitor_probe`，从账号成功率统计中排除。
- **Dispatcher**
  - 读取 firing 状态，按级别决定立即/摘要/不推送；按渠道最低级别路由；执行冷却、抖动抑制、恢复通知。
  - 调渠道 adapter；每个事件的每个渠道投递结果写 `monitor_event_pushes`；失败重试。
- **HTTP/UI**
  - 新增 `/__monitor/*` 接口，全部管理员可见。
  - 前端新增 `web/src/monitoring/`，替换原「告警」「自检」两个导航项。

### 5.2 并发与生命周期

- 采集器、规则引擎、Dispatcher 各自单 goroutine 串行运行，通过有界 channel 传递观测/事件，避免并发写 SQLite。
- 启动顺序：迁移表 → 迁移旧告警 → 启动监控服务 → 启动采集 ticker。
- 关闭顺序：取消 context → 等待当前 tick 完成（最多 5 秒）→ 关闭数据库。
- 单次 tick 超过 30 秒记 warning；超过 60 秒跳过本轮，避免堆积。

### 5.3 调度作业与观测契约

| 作业 | 频率 | 窗口/范围 | 产出 |
|---|---|---|---|
| 请求窗口统计 | 60 秒 | 5 / 10 / 15 分钟，按 provider + 全局 | `Observation.Windows` |
| provider 状态 | 60 秒 | token 到期、health_status、最近成功时间 | `Observation.Provider` |
| 额度/积分 | 15 分钟 | 支持厂商 | `provider_metric_samples` |
| 模型列表 | 15 分钟 | 支持模型列表的 provider | probe 事件 + `model_unavailable` 输入 |
| TTFT 基线 | 每小时 | 过去 7 天同一小时 | `monitor_baselines` |
| 签到汇总 | 每日 09:30 | 配置时区 | `checkin_history` |
| 每日轻探测 | 每日 04:00–06:00 | 配置时区 | probe 事件 |
| 每周 CLI 深检 | 每周日 05:00 | 配置时区 | probe 事件 |
| 清理 | 每 6 小时 | 事件 90 天、推送记录随事件、样本 30 天、签到 90 天 | 删除过期行 |
| 采集器心跳 | 60 秒 | 最近成功 tick | `collector_stale` 输入 |

规则引擎消费的观测结构：

```go
type WindowStats struct {
    Total, Success, Auth401403, RateLimited, ServerError, Pseudo200, Empty int
    TTFTp50, TTFTp95, LatencyP95 int64 // 毫秒
    CostMicros int64
}

type Observation struct {
    Now              time.Time
    ProviderID       string
    Windows          map[int]*WindowStats // key: 5, 10, 15
    ExpiresAt        time.Time
    HealthStatus     string
    LastSuccessAt    time.Time
    LastProbeAt      time.Time
    Capabilities     Capabilities
    Quota            *QuotaSnapshot
    BaselineTTFTP95  int64 // 来自 monitor_baselines，0 表示无基线
}

type Capabilities struct {
    Quota, Checkin, Models, OAuth bool
}

type QuotaSnapshot struct {
    Source     string    // 额度/积分来源接口标识，不含凭证
    Remaining  float64   // 剩余值，单位随来源
    Limit      float64   // 0 表示来源不提供总量
    Ratio      float64   // Remaining/Limit；Limit=0 时为 -1（不适用）
    ExpiresAt  time.Time // 积分到期时间，零值表示无
    ObservedAt time.Time
    Stale      bool // ObservedAt 超过 quotaStaleMinutes
}
```

每个 tick 用 `time >= now-window` 的 SQL 重新取 5/10/15 分钟窗口，不做只在内存里滚 5 分钟的近似；窗口统计排除 `access_source='monitor_probe'`。

## 6. 数据模型

所有时间字段存 UTC RFC3339Nano；界面按 `monitor_dispatch.timezone`（默认 Asia/Shanghai）展示，悬停显示浏览器本地时间。迁移只新增表/列/索引，不删旧表。

### 6.1 `monitor_events`

```sql
CREATE TABLE IF NOT EXISTS monitor_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    time TEXT NOT NULL,
    kind TEXT NOT NULL,                 -- alert | renewal | probe | checkin | quota | system
    severity TEXT NOT NULL,             -- p0 | p1 | p2 | info
    rule_key TEXT NOT NULL DEFAULT '',
    target_type TEXT NOT NULL,          -- provider | api_key | model | gateway
    target_id TEXT NOT NULL DEFAULT '',
    target_name TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL,
    detail_json TEXT NOT NULL DEFAULT '{}',
    fingerprint TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'firing', -- firing | resolved | info
    count INTEGER NOT NULL DEFAULT 1,
    first_seen TEXT NOT NULL,
    last_seen TEXT NOT NULL,
    resolved_at TEXT NOT NULL DEFAULT '',
    resolution_reason TEXT NOT NULL DEFAULT '', -- recovered | data_unavailable | legacy
    push_status TEXT NOT NULL DEFAULT '',  -- pending | sent | partial | failed | suppressed | skipped
    push_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_monitor_events_time ON monitor_events(time DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_monitor_events_fp ON monitor_events(fingerprint, status);
CREATE INDEX IF NOT EXISTS idx_monitor_events_target ON monitor_events(target_type, target_id, time DESC, id DESC);
```

- `detail_json` 只放统计值、错误码、窗口、模型、样本数、关联 key/route 数；不放 token、API Key、请求/响应正文、webhook 地址。
- `count` 是规则持续评估为真的次数，不是请求数。
- `info` 事件（续期成功、探测通过、签到成功）不进入告警列表，只进事件流。

### 6.2 `monitor_alert_states`

```sql
CREATE TABLE IF NOT EXISTS monitor_alert_states (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    fingerprint TEXT NOT NULL UNIQUE,
    rule_key TEXT NOT NULL,
    severity TEXT NOT NULL,             -- 当前生效级别，可因证据升级
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL DEFAULT '',
    target_name TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,               -- firing | resolved
    disposition TEXT NOT NULL DEFAULT 'open', -- open | acknowledged | ignored
    count INTEGER NOT NULL DEFAULT 0,
    first_seen TEXT NOT NULL,
    last_seen TEXT NOT NULL,
    last_notified_at TEXT NOT NULL DEFAULT '',
    acknowledged_at TEXT NOT NULL DEFAULT '',
    ignored_at TEXT NOT NULL DEFAULT '',
    suppressed_until TEXT NOT NULL DEFAULT '',
    unavailable_since TEXT NOT NULL DEFAULT '',
    resolution_reason TEXT NOT NULL DEFAULT '', -- recovered | data_unavailable
    detail_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_monitor_states_status ON monitor_alert_states(status, severity);
```

### 6.3 `monitor_event_pushes`

```sql
CREATE TABLE IF NOT EXISTS monitor_event_pushes (
    event_id INTEGER NOT NULL,
    channel_id TEXT NOT NULL,
    status TEXT NOT NULL,               -- pending | sent | failed | skipped
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    PRIMARY KEY (event_id, channel_id)
);
```

### 6.4 `checkin_history`

```sql
CREATE TABLE IF NOT EXISTS checkin_history (
    provider_id TEXT NOT NULL,
    day TEXT NOT NULL,
    at TEXT NOT NULL DEFAULT '',
    outcome TEXT NOT NULL DEFAULT '',
    credit REAL NOT NULL DEFAULT 0,
    streak INTEGER NOT NULL DEFAULT 0,
    msg TEXT NOT NULL DEFAULT '',
    valid_until TEXT NOT NULL DEFAULT '',
    offline INTEGER NOT NULL DEFAULT 0,
    tries INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (provider_id, day)
);
```

现有 `checkins` 表保持不变（每 provider 最新一条）；`SaveCheckin` 在同一事务里同时写 `checkins` 和 `checkin_history`。首次升级时把现有 `checkins` 中 `day != ''` 的行复制进历史（幂等）。

### 6.5 `provider_metric_samples`

```sql
CREATE TABLE IF NOT EXISTS provider_metric_samples (
    provider_id TEXT NOT NULL,
    hour TEXT NOT NULL,                 -- UTC 整点，RFC3339
    quota_ratio REAL,                   -- NULL = 不支持/未知
    credits REAL,
    credit_valid_until TEXT NOT NULL DEFAULT '',
    auth_expires_at TEXT NOT NULL DEFAULT '',
    health_status TEXT NOT NULL DEFAULT '',
    source_error TEXT NOT NULL DEFAULT '',
    observed_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (provider_id, hour)
);
```

- 额度接口失败时写 `source_error`，其余字段保留上一次成功值由 UI 读取最新样本并标注 `stale`；不把未知写成 0。
- 只保留 30 天；每日清理任务按 `hour` 删除更早行。

### 6.6 `monitor_baselines`

```sql
CREATE TABLE IF NOT EXISTS monitor_baselines (
    provider_id TEXT NOT NULL,
    hour_of_day INTEGER NOT NULL,       -- 0-23，配置时区
    metric TEXT NOT NULL,               -- ttft_p95
    value REAL NOT NULL DEFAULT 0,
    sample_count INTEGER NOT NULL DEFAULT 0,
    computed_at TEXT NOT NULL,
    PRIMARY KEY (provider_id, hour_of_day, metric)
);
```

每小时从 `request_logs` 的过去 7 天同一小时计算 TTFT P95；样本 <20 时写 `sample_count` 但不写 `value`（值为 0，规则视为无基线）。每 provider 最多 24 行。

### 6.7 配置键（settings 表）

- `monitor_rules`：JSON 数组，每项 `{key, enabled, severity, params, cooldownMinutes}`。一条规则只有一个 `severity`；`key_budget` 拆成 `key_budget_warn`（P2）与 `key_budget_exhausted`（P1）两条，各自独立配置。缺失项用默认值；未知项忽略。
- `monitor_channels`：JSON 数组，每项 `{id, type, name, config, minSeverity, enabled}`；`type` 为 `feishu|dingtalk|wecom|telegram`。`config` 按类型：
  - `feishu` / `dingtalk`：`{webhook, secret}`，`secret` 可空（空则不签名）。
  - `wecom`：`{webhook}`。
  - `telegram`：`{botToken, chatId}`。
- `monitor_probe`：JSON 对象，见 §9。
- `monitor_dispatch`：JSON 对象，`{digestMinutes:15, quietEnabled:true, quietStart:"23:00", quietEnd:"08:00", recoveryNotify:true, timezone:"Asia/Shanghai", baseUrl:""}`；`baseUrl` 用于通知里的深链，为空时见 §10.3。
- `monitor_migration_v1`：迁移步骤标记，JSON `{checkins:bool, alerts:bool, telegram:bool, legacyMaxAlertId:int}`；启动时总是增量导入旧告警 `id > legacyMaxAlertId`，其余步骤只跑未完成项。
- 渠道 secret 只写不读：GET 返回 `secretSet: true`；`PUT` 更新渠道时 `secret`/`webhook`/`botToken` 为空表示保留原值。

### 6.8 请求日志索引与标记

- 新增 `access_source` 取值 `monitor_probe`；探测请求在写日志时显式设置，并跳过 `classifyAccessSource`。
- 新增索引：`CREATE INDEX IF NOT EXISTS idx_request_logs_provider_time ON request_logs(provider_id, time DESC);`
- 账号成功率/TTFT 查询排除 `access_source='monitor_probe'`；事件流可以展示探测事件，但不计入账号健康。

## 7. 指标定义

### 7.1 指标目录

| 指标 | 定义 | 数据源 | 用途 |
|---|---|---|---|
| token 剩余寿命 | `oauth_expires_at - now` | providers | 告警 + 账号详情 |
| 最近续期结果/耗时/提前量 | 续期埋点事件 | monitoring 事件 | 告警 + 账号详情 |
| 401/403 次数与占比 | status ∈ {401,403} | request_logs | 告警 |
| 成功率 | 2xx 且 `error_description=''`，排除探测 | request_logs | 告警 + 看板 |
| 伪 200 | 2xx 且 `error_description!=''` | request_logs | 看板 + 链路告警证据 |
| 429 次数与占比 | status=429 | request_logs | 告警 |
| 5xx/timeout 占比 | status≥500，或 status=0 且 error_description 含 timeout/deadline | request_logs | 告警 |
| 空响应率 | 2xx 且 output_tokens=0，排除 embedding/探测 | request_logs | 告警 |
| TTFT P50/P95 | `ttft_ms` 非零样本 | request_logs | 告警（基线） |
| 延迟 P95、网关开销 P95 | `latency_ms`、`gateway_overhead_ms` | request_logs | 看板 |
| 额度/积分 | 厂商接口返回值 | provider_metric_samples | 告警 + 详情 |
| 积分到期 | `valid_until` | checkin_history / 额度接口 | 告警 |
| 签到结果/连续天数 | outcome、streak | checkin_history | 告警 + 详情 |
| 多 IP / 并发 IP | 现有检测器 | request_logs | 告警 |
| 密钥花费/额度 | 现有配额窗口 | usage_key_window + cost_micros | 告警 |
| 模型列表可用性 | 现有模型拉取能力 | Prober | 告警 |
| 采集器心跳 | 最近成功 tick | monitoring 自身 | 告警 |

请求类指标只统计 `action=proxy` 的行；客户端主动取消（`context canceled`）不计入失败率；`status=0` 且不含 timeout/deadline 的日志忽略。

### 7.2 能力矩阵

能力由 `ProviderCapabilities(provider)` 集中声明；未声明为 true 的指标一律显示“不适用”，规则状态为 `unavailable`，不参与告警。

| 能力 | OAuth 账号（Claude/ChatGPT/Cursor/Qoder/WorkBuddy/ZCode/Trae） | API Key 厂商 |
|---|---|---|
| token 到期与续期 | 有 `oauth_expires_at` 的 provider；没有则显示不适用 | 不适用 |
| 额度/积分 | 已有 quota handler 的厂商；其余不适用 | 视厂商接口；未知则不适用 |
| 积分到期 | Qoder、WorkBuddy（有 `valid_until` 时） | 不适用 |
| 签到 | Qoder CN/Global、WorkBuddy | 不适用 |
| 模型列表 | 有模型拉取能力的 provider | 同左 |
| 请求链路指标 | 所有实际承载请求的 provider | 同左 |

实现时以现有 quota/usage handler 为准补全矩阵；新增厂商只需在能力函数里声明。

### 7.3 账号状态派生

账号 `status` 由数据实时计算，不写回 `provider.health_status`；优先级从高到低：

| status | 条件 |
|---|---|
| `auth_expired` | 凭证已过期且最近一次续期失败，或 `auth_invalid` firing |
| `down` | `provider.health_status=unavailable`，或 `provider_down` / `probe_failed`(P0) firing |
| `degraded` | 其他 provider 级 P0/P1 规则 firing，或 24h 成功率 <95%，或 `quota_low` / `checkin_failed` firing |
| `unknown` | 无凭证、无请求样本，或能力数据缺失，且无上述 firing |
| `healthy` | 其余：凭证有效、无 provider 级 firing、24h 成功率 ≥95% |

- 旧 `health_status` 只作为基线：`healthy`→healthy，`unavailable`→down，`unchecked`/空→unknown。
- 账号列表筛选与概览“可用账号”都使用该派生值；“可用”= `healthy` + `degraded`。

## 8. 规则引擎

### 8.1 规则模板（18 条）

下表的“窗口 / 持续”分别是统计窗口与连续为真进入 firing 的时长；“即时”表示单次评估即可判定（连续次数类规则以次数代替持续）。

| key | 条件（默认参数） | 窗口 / 持续 | 默认级别 | 对象 |
|---|---|---|---|---|
| `oauth_expiring` | 到期 < `leadMinutes=120` 且最近一次续期未成功 | 即时 / 即时 | P0 | provider |
| `oauth_refresh_failed` | 连续 `consecutiveFailures=2` 次刷新失败 | 即时 / 2 次 | P0 | provider |
| `auth_invalid` | 401/403 ≥ `minCount=3` 且占比 ≥ `minRatio=0.5`（样本 ≥ `minSamples=10`） | 5min / 3min | P0 | provider |
| `provider_down` | 成功率 < `maxSuccessRatio=0.8`（样本 ≥ 20） | 5min / 10min | P1 | provider |
| `server_error_spike` | 5xx+timeout 占比 > `maxErrorRatio=0.1`（样本 ≥ 20） | 5min / 5min | P1 | provider |
| `rate_limit_spike` | 429 占比 > `maxRatio=0.2`（样本 ≥ 20）或单 provider 5min ≥ `maxCount=10` 次 | 5min / 5min | P1 | provider |
| `empty_response` | 2xx 且 output=0 占比 > `maxRatio=0.05`（样本 ≥ 10） | 10min / 10min | P1 | provider |
| `probe_failed` | 连续 `consecutiveFailures=2` 次轻探测失败；401/403 时按 `authSeverity=P0` 升级 | 即时 / 2 次 | P1（默认） | provider |
| `checkin_failed` | 连续 `consecutiveDays=2` 个自然日非 success（offline 不计） | 每日 09:30 / 2 天 | P1 | provider |
| `quota_low` | 剩余额度/积分比例 < `maxRatio=0.1`（仅 `Limit>0`；`Ratio<0` 视为 unavailable） | 即时 / 即时 | P1 | provider |
| `credit_expiring` | 积分 >0 且 ≤ `leadHours=72` 到期 | 每日 / 1 天 | P2 | provider |
| `key_budget_warn` | 密钥用量 ≥ `warnRatio=0.8` | 5min / 5min | P2 | api_key |
| `key_budget_exhausted` | 密钥用量 ≥ `criticalRatio=1.0` | 5min / 5min | P1 | api_key |
| `multi_ip` | 10min 内 ≥ `minIPs=5` 个 IP | 10min / 即时 | P0 | api_key |
| `concurrent_ip` | 同时 ≥ `minIPs=4` 个 IP 有在途请求 | 即时 / 即时 | P0 | api_key |
| `ttft_regression` | P95 首包 > max(`floorMs=5000`, `factor=2` × 7 日同时段基线)（样本 ≥ 20；基线缺失即 0 时为 unavailable） | 15min / 15min | P2 | provider |
| `model_unavailable` | 模型列表连续 `consecutiveFailures=3` 次拉取失败 | 15min / 3 次 | P2 | provider |
| `collector_stale` | 采集器 > `maxAgeMinutes=5` 未成功运行 | 即时 / 5min | P2 | gateway |

凭证类规则（`oauth_expiring`、`oauth_refresh_failed`）由 60 秒 tick 评估，满足 §2.1 的 2 分钟口径；请求类规则叠加统计窗口与持续时间，落在 5–10 分钟口径内。`probe_failed` 的配置级别是基础级别（默认 P1），参数 `authSeverity=P0` 表示探测结果为 401/403 时以 P0 写入状态并立即推送；状态内的 `severity` 记录当前生效级别，firing 期间只升不降。`checkin_failed` 的“自然日”按 `monitor_dispatch.timezone` 计算。

### 8.2 评估语义

- 每 60 秒评估请求类规则；额度类每 15 分钟；签到每日 09:30；模型列表每 15 分钟；采集器心跳每 60 秒。
- 比率规则必须样本 ≥ 下限，低流量改用绝对次数；`auth_invalid` 的“占比”分母是该窗口内所有请求。
- 条件连续为真达到 `forMinutes` 才进入 firing；首个 tick 记 `first_seen`，firing 期间每个 tick 更新 `last_seen`、`count`。
- 恢复：条件连续 2 个 tick 为假才进入 resolved；插入 resolved 事件；持续时间 = `resolved_at - first_seen`。
- 冷却：同一 fingerprint 的重复通知间隔 ≥ `cooldownMinutes`（默认 60），首次通知不受冷却；P1→P0 升级时立即推送。
- 抖动抑制：15 分钟内同一 fingerprint 发生 ≥3 次 firing/resolved 翻转 → `suppressed_until = now + 1h`，期间只更新状态和事件，不发通知；抑制结束时发一条摘要（如仍在 firing）。
- 级别映射：旧告警 `warn` → P1，`error`/`critical` → P0；新规则默认级别见上表，用户可改。
- 规则参数校验：`windowMinutes` 只允许 5/10/15（对应 `Observation.Windows` 的聚合桶），阈值 >0，样本下限 1–10000，冷却 5–1440 分钟；非法值拒绝保存并返回 400。

**不可用（unavailable）语义**

- 规则依赖的数据缺失、能力不支持或采集失败时，本次评估结果为 `unavailable`，不改变 firing 计数，也不进入 firing。
- 已 firing 的规则转为 `unavailable` 时进入宽限期：`unavailable_since` 记首次不可用时间，宽限 `max(15 分钟, 2 个该规则评估周期)`（请求类约 2 分钟、额度类约 30 分钟、签到类约 2 天），宽限期内不推送；超过后自动 resolved，`resolution_reason=data_unavailable`，不发恢复通知。
- 从未 firing 的规则持续 `unavailable` 不会产生事件。
- 数据恢复后重新从 inactive 开始累计；若条件仍成立，需重新满足持续窗口才再次 firing。

**状态与处置**

`monitor_alert_states.status` 只有 `firing | resolved`，`disposition` 只有 `open | acknowledged | ignored`，两者正交：处置不改变检测结果，只改变通知与列表呈现。

| 当前 | 事件 | 结果 | 通知 |
|---|---|---|---|
| inactive | 条件持续满足达到窗口 | status=firing，disposition=open | 按 §10.2 |
| firing/open | 条件继续为真 | 更新 last_seen/count | 受冷却限制，不重复 |
| firing/open | 用户确认 | disposition=acknowledged，记 acknowledged_at | 停止重复通知；恢复通知仍发（若此前已通知） |
| firing/open 或 acknowledged | 用户忽略 | disposition=ignored，记 ignored_at | 该 fingerprint 在 resolved 前不再通知；不计入活跃告警数 |
| firing/ignored | 条件继续为真 | 保持 ignored | 无 |
| firing/* | 条件连续 2 tick 为假 | status=resolved，disposition 重置为 open | 若此前通知过且恢复通知开启，发恢复 |
| firing/* | 数据不可用超过宽限期 | status=resolved，resolution_reason=data_unavailable | 不发恢复 |
| resolved | 再次满足持续窗口 | 新 firing 周期，disposition=open，新事件，次数从 1 开始 | 按 §10.2，不受上一周期冷却限制 |
| resolved | 用户确认/忽略 | 400（仅 firing 状态可处置） | 无 |

- 确认与忽略都只影响通知和列表，不停止规则评估；`ignored` 在 resolved 后自动回到 `open`，不记忆到下一周期。
- 每个 firing 周期新增一条 `kind=alert` 事件；状态变化（ack/ignore/resolve）追加到事件 `detail_json.timeline`，不单独建事件。

### 8.3 规则输入契约

| 规则 | 数据来源 |
|---|---|
| `oauth_expiring` / `oauth_refresh_failed` | provider 凭证 `expires_at` + 续期事件（§8.4 埋点） |
| `auth_invalid` / `provider_down` / `server_error_spike` / `rate_limit_spike` / `empty_response` / `ttft_regression` | `Observation.Windows`（5/10/15 分钟聚合，排除探测流量） |
| `probe_failed` | Prober 探测事件 |
| `checkin_failed` | `checkin_history` |
| `quota_low` / `credit_expiring` | `Observation.Quota`（`QuotaSnapshot`）；无额度接口时用最近一次签到 `valid_until` 填 `ExpiresAt` |
| `key_budget_warn` / `key_budget_exhausted` | 现有每密钥额度用量统计 |
| `multi_ip` / `concurrent_ip` | 现有请求日志聚合查询（`internal/store/alerts.go`） |
| `model_unavailable` | 模型列表拉取作业结果 |
| `collector_stale` | Collector 心跳 |

规则只读取上述输入，不直接查 HTTP 上游；所有上游交互由 Collector/Prober 完成。Collector 在额度接口不提供到期时间时，用 `checkin_history.valid_until` 补 `QuotaSnapshot.ExpiresAt`；两者都没有则 `credit_expiring` 为 unavailable。

### 8.4 续期埋点

- 在 `refreshClaudeOAuthToken`、`refreshChatGPTToken`、`refreshCursorOAuthToken`、`refreshWorkBuddyCredential`、`renewQoderCredential`、`traeRenew` 的调用点记录：provider id、开始/结束时间、成功/失败、错误码、提前量（到期时间 - 现在）。
- 写 `monitor_events`（kind=renewal, severity=info/p0）；写失败只记日志。`oauth_refresh_failed` 直接消费最近事件，不重复解析 HTTP 响应。
- 不记录 refresh token/access token。

## 9. 探测系统

### 9.1 配置（`monitor_probe`）

```json
{
  "eventDriven": {"enabled": true, "idleMinutes": 30, "maxPerDayPerAccount": 4, "minIntervalMinutes": 360},
  "daily": {"enabled": true, "windowStart": "04:00", "windowEnd": "06:00"},
  "weeklyCli": {"enabled": true, "weekday": 0, "time": "05:00"},
  "quotaStaleMinutes": 30
}
```

### 9.2 事件驱动轻探测

- 触发条件（全部满足）：
  1. provider 启用且未删除；
  2. 最近 24h 有非探测成功请求；
  3. 最近一次非探测请求距今 ≥ `idleMinutes`；
  4. 距上次探测 ≥ `minIntervalMinutes`；
  5. 当日探测次数 < `maxPerDayPerAccount`；
  6. 当前没有该 provider 的探测在跑。
- 额外触发：同一 provider 连续 2 次请求失败（401/403/429/5xx/timeout），立即探测，但 10 分钟内最多 1 次。
- 探测内容：1 条最小 chat（`max_tokens=16`）+ 1 条带简单工具定义的 tools 请求；超时 30 秒。
- 结果写 `kind=probe` 事件，`detail_json` 含 case、HTTP 状态、耗时、模型、错误码；失败进入 `probe_failed`。

### 9.3 每日轻探测

- 每天 04:00–06:00（配置时区，默认 Asia/Shanghai）对每个启用 provider 跑一次与 §9.2 相同的两个 case；串行，单 provider 并发 1。
- provider 数多时按 15 分钟一批分批；未跑完的顺延到窗口结束，不跨日堆积。
- 每日轻探测与事件驱动探测共用当日次数上限。

### 9.4 每周 CLI 深检

- 每周日 05:00（配置时区，默认 Asia/Shanghai；`weeklyCli.weekday` 0=周日）跑现有 6 条用例（OpenCode/Codex/Claude × chat/tools），复用 `selfcheck.go` 的执行器与临时 route/key。
- CLI 不存在或不可执行：记录 `kind=probe, detail.skipped="cli_missing"`，不判失败、不告警。
- 每个 provider 顺序执行；单用例超时 90 秒；整轮超过 2 小时则停止并记录剩余为 skipped。
- 结果进入事件流和探测页，不改变 provider 配置。

### 9.5 手动入口

- `POST /__monitor/probes/run` 支持指定 provider 和 `light|cli`；用于排障，不是日常必需。
- 手动与自动互斥：同一 provider 同时只允许一个探测任务。

## 10. 通知与渠道

### 10.1 渠道模型

渠道列表可增删改、单独开关、单独测试；每个渠道有 `id/type/name/config/minSeverity/enabled`，`config` 按类型不同：

| type | config 字段 |
|---|---|
| `feishu` | `{webhook, secret}` |
| `dingtalk` | `{webhook, secret}` |
| `wecom` | `{webhook}`（key 在 URL 中） |
| `telegram` | `{botToken, chatId}` |

`webhook` 与 `botToken` 都按 secret 处理（只写不读）。新增平台只需实现 `ChannelAdapter.Send(ctx, Notification) error`，规则引擎不感知渠道类型；前端按类型渲染对应字段。

### 10.2 分级投递

| 级别 | 投递 | 内容 |
|---|---|---|
| P0 | 立即，逐条（同 provider 1 分钟内多条合并） | 卡片：标题、对象、级别、关键证据、深链 |
| P1 | 每 `digestMinutes=15` 聚合 | Markdown 摘要表，按规则/对象分组；超过 20 行折叠为“还有 N 条” |
| P2 | 不推送 | 只进事件流和规则页 |
| 恢复 | P0/P1 恢复时推送 | 持续时长、峰值、恢复时间；<5 分钟自愈不发 |

- 渠道 `minSeverity` 过滤：例如某渠道只收 P0，则 P1 摘要不发给它。
- 静默时段（默认 23:00–08:00，按 `monitor_dispatch.timezone` 解释，默认 `Asia/Shanghai`）：P0 立即发；P1 摘要顺延到 08:00 后合并发送；`quietEnabled=false` 可整体关闭，也可改时间段。
- 恢复通知可关闭（`recoveryNotify`）。

### 10.3 消息格式

统一内部模型：

```json
{
  "fingerprint": "provider_down|provider:claude-pro",
  "severity": "p1",
  "title": "Claude Pro 成功率跌破 80%",
  "target": "claude-pro",
  "markdown": "- 5 分钟成功率 62%\n- 影响 3 条路由",
  "url": "https://<console>/monitoring?tab=accounts&account=claude-pro",
  "at": "2026-10-23T01:42:00Z"
}
```

- 飞书：P0/P1 用 interactive card 2.0；摘要用 markdown 文本。签名放 body：`timestamp` 秒 + `sign = base64(HMAC-SHA256(key = ts + "\n" + secret, data = ""))`；无 secret 时不加签。
- 钉钉：P0/P1 用 actionCard；摘要用 markdown。签名放 query：毫秒 `timestamp` + `sign = urlencode(base64(HMAC-SHA256(key = secret, data = timestamp + "\n" + secret)))`；`atMobiles` 默认空。
- 企业微信：markdown 消息，webhook key 在 URL；不支持卡片颜色，用 `[P0]` 标题前缀。
- Telegram：沿用现有 Markdown/HTML 消息，`botToken` + `chatId` 取自渠道 `config`；迁移时把旧设置转成一个 `type=telegram` 渠道。
- 深链 URL：优先 `monitor_dispatch.baseUrl`；未配置且控制台可经 LAN 访问时用 `http://<LAN IP>:<port>`（复用 `netutil` 的地址探测）；都不可用时省略 `url`，消息正文仍包含账号/告警标题。
- 所有消息不含 token、API Key、请求/响应正文、webhook 地址；错误样本只保留错误码和归一化描述。

### 10.4 生命周期、去重、重试

- 状态机与处置：`inactive → firing → resolved`，确认/忽略只改 `disposition`（见 §8.2）；每个 fingerprint 一条 `monitor_alert_states`。
- 同一 fingerprint 的首次通知立即/按摘要发出；若持续未恢复，超过 `cooldownMinutes`（默认 60）后允许再提醒一次（P0 立即、P1 进下一次摘要），由 `last_notified_at` 控制；级别 P1→P0 升级立即推送，不受冷却限制。
- 抖动抑制见 §8.2；抑制结束后如仍 firing，发一条“仍在持续”的摘要。
- 每个事件对每个渠道的投递结果写 `monitor_event_pushes`；失败按 1s/5s/30s 重试 3 次；仍失败则 `status=failed`，面板可单独重试该渠道。
- 渠道限流：飞书 100 条/分、钉钉 20 条/分、企业微信 20 条/分；Dispatcher 每个渠道一个令牌桶，超限排队而不是丢弃。
- 测试发送：`POST /__monitor/channels/{id}/test` 发固定测试卡片，返回 HTTP 状态、耗时、平台错误码；不返回 webhook/secret。

## 11. API 设计

全部接口仅管理员可用，返回 camelCase JSON；列表分页默认 50、最大 200。

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/__monitor/overview?range=24h` | KPI、账号摘要、链路健康、当前告警、最近事件 |
| GET | `/__monitor/accounts?query=&status=&vendor=&authType=` | 账号列表 |
| GET | `/__monitor/accounts/{providerId}` | 账号详情（凭证、额度、签到、健康、最近错误、关联路由/密钥） |
| POST | `/__monitor/accounts/{providerId}/refresh` | 立即续期 |
| POST | `/__monitor/accounts/{providerId}/quota/refresh` | 立即拉取额度/积分（同步等待上游，超时 15 秒） |
| POST | `/__monitor/accounts/{providerId}/probe` | 立即轻探测 |
| GET | `/__monitor/events?kind=&severity=&targetType=&targetId=&status=&from=&to=&page=` | 事件流 |
| GET | `/__monitor/alerts?status=&disposition=&severity=&rule=&targetType=&targetId=&from=&to=&page=` | 告警列表（`status`=firing/resolved，`disposition`=open/acknowledged/ignored） |
| GET | `/__monitor/alerts/{id}` | 告警详情（`id` 为 `monitor_alert_states.id`） |
| POST | `/__monitor/alerts/{id}/ack` | 确认告警 |
| POST | `/__monitor/alerts/{id}/ignore` | 忽略告警 |
| POST | `/__monitor/events/{eventId}/retry-push` | 重试某个事件在指定渠道的失败投递，body `{channelId}` |
| GET | `/__monitor/rules` | 规则配置 + 默认值 |
| PUT | `/__monitor/rules` | 保存规则配置 |
| POST | `/__monitor/rules/reset` | 恢复默认 |
| GET | `/__monitor/channels` | 渠道列表（secret 脱敏） |
| POST | `/__monitor/channels` | 新增渠道 |
| PUT | `/__monitor/channels/{id}` | 修改渠道 |
| DELETE | `/__monitor/channels/{id}` | 删除渠道 |
| POST | `/__monitor/channels/{id}/test` | 测试发送 |
| GET | `/__monitor/dispatch` | 聚合间隔、静默时段、恢复通知、时区、baseUrl |
| PUT | `/__monitor/dispatch` | 保存调度配置 |
| GET | `/__monitor/probes` | 探测配置、覆盖、最近结果、运行历史 |
| PUT | `/__monitor/probes/settings` | 保存探测配置 |
| POST | `/__monitor/probes/run` | 手动探测 |

- 枚举：`range` ∈ `24h|7d|30d`；账号 `status` ∈ `healthy|degraded|down|auth_expired|unknown`；`vendor` 用与前端图标一致的厂商键；事件 `kind` ∈ `alert|probe|renewal|checkin|quota|system`。
- 告警列表/详情返回 `latestEventId`、`pushStatus`（取该周期最近事件）、`relatedEventIds`，便于展示推送状态和调用重试接口。
- 旧的 `/__alerts*`、`/__selfcheck*` HTTP 路由在阶段 2 移除；`selfcheck.go` 的执行函数保留给 Prober 调用。
- 账号详情里的“重新授权”复用现有 OAuth 授权流程，不在监控模块新开一套。
- 所有写接口返回 400 时给字段级错误；404 用于对象不存在；409 用于并发探测冲突。

## 12. 前端设计

### 12.1 导航与文件

- 侧边栏新增 `monitoring`，移除 `alerts`、`self-check`。
- 新增 `web/src/monitoring/`：
  - `MonitoringPage.tsx`（tab 容器、轮询、深链）
  - `OverviewTab.tsx`、`AccountsTab.tsx`、`ProbesTab.tsx`、`AlertsTab.tsx`、`SettingsTab.tsx`
  - `AccountDrawer.tsx`、`AlertDetail.tsx`、`ChannelForm.tsx`、`RuleRow.tsx`
  - `api.ts`、`types.ts`（仅监控中心类型）
- 复用现有 `ui.tsx`、`selects.tsx`、`charts.tsx`；新增样式追加到 `styles.css` 的监控中心小节，不另起一套主题。

### 12.2 概览（A + C）

- 顶部 KPI：可用账号（x/y）、活跃告警（P0/P1 拆分）、24h 成功率、今日花费（含计价覆盖率）。
- 左「账号健康」：按问题优先排序；每行状态点 + 账号标签 + 凭证类型、续期倒计时、额度/积分、签到、24h 成功率 sparkline、最近错误；异常行给“处理”深链。
- 右「链路健康」：成功率大数字、429/5xx/空响应/P95 首包、24h 柱状；下方当前告警前 5 条。
- 底部「事件流」：筛选 chips（全部/P0/P1/账号/链路/风控/探测/签到），每行时间、级别、对象、摘要、证据、深链；15 秒刷新，加载更多。

### 12.3 账号

- 可搜索/筛选的账号表（厂商、状态、凭证类型、签到）；行点击开右侧抽屉。
- 抽屉分区：凭证（到期、最近续期、7 日续期历史）、额度/积分（来源、`observed_at`、手动刷新）、签到（连续天数 + 7/30 日格子）、健康（24h 成功率/TTFT、最近错误）、关联（密钥/路由数量）。
- 额度样本超过 `staleMinutes=30`（`monitor_probe.quotaStaleMinutes`）未更新时显示“数据过期 + 最后更新时间”，并展示手动刷新按钮；刷新失败保留旧值并显示错误码。
- 操作：重新授权、立即续期、刷新额度、立即探测、查看请求日志；不提供停用/隔离/改配置按钮。

### 12.4 探测

- 三块调度卡：事件驱动、每日轻探测、每周 CLI 深检；显示下一次运行时间与开关。
- 覆盖表：每 provider 最近一次轻探测/CLI 深检结果、耗时、跳过原因。
- 运行历史：时间、类型、provider、case 结果、耗时；支持“立即探测”（选 provider + 类型）。

### 12.5 告警

- 筛选：状态（firing/resolved）、处置（open/acknowledged/ignored）、级别、规则、对象、时间。
- 列表：级别、标题、对象、次数、首次/最近、推送状态。
- 详情：证据（窗口指标、错误码样本、受影响 key/route 数）、时间线（触发 → 通知 → 恢复）、操作（确认/忽略；时间线中的失败投递可按渠道重试）、深链到账号和请求日志。

### 12.6 规则与渠道

- 规则表：开关、级别、关键参数（窗口/阈值/样本下限/冷却）、恢复默认；参数即时校验。
- 渠道：列表 + 新增/编辑/删除；类型选择后显示对应字段；测试发送显示结果；secret 只显示“已配置”。
- 调度与静默：聚合间隔、静默时段、恢复通知开关。

### 12.7 交互与视觉

- 轮询：概览/告警 15 秒；页面隐藏时暂停；切回立即刷新一次；无全页闪动（保留上一次数据，仅更新变化区域）。
- 状态：骨架屏、空态、错误态、`不适用`、`数据过期` 文案齐全。
- 视觉：运维盘气质；沿用现有主题 tokens；行高 36–40px；等宽数字；1px 边框；6–8px 圆角；状态点 + 文本（不只靠颜色）。
- 响应式：≥1280 两栏；768–1280 单栏；<768 卡片化，表格横向滚动，抽屉全屏。
- 无障碍：键盘可达、focus ring、aria-label；状态色不作为唯一信息。
- 深链：`/monitoring?tab=accounts&account=<providerId>`、`/monitoring?tab=alerts&alert=<stateId>`（`stateId` 为 `monitor_alert_states.id`）；告警 tab 另支持 `&target=<providerId>` 过滤。沿用现有 pathname 路由：`navIDFromPath` / `navPathForID` 增加 `/monitoring` 映射，`pushState` 写入 tab 与对象参数，`popstate` 与首次加载都解析 `tab/account/alert/target` 并定位到对应行；刷新后仍停留在同一 tab 与抽屉。

## 13. 迁移

1. 启动迁移新增表、索引、`request_logs` 新索引。
2. `checkins` → `checkin_history`：复制 `day != ''` 的最新记录，幂等。
3. 旧 `alerts` → `monitor_events`：读取全部旧行，映射：
   - `rule`：`apikey_multi_ip`→`multi_ip`，`apikey_concurrent_ip`→`concurrent_ip`；未知值保留原值并加 `legacy_rule=true`；
   - `kind=alert`；`target_type=api_key`，`target_id=api_key_id`，`target_name=api_key_name`；
   - `severity`：`error`/`critical`→p0，`warn`→p1；
   - `title`：多 IP 为“API Key 多 IP 使用”，并发 IP 为“API Key 并发 IP”；
   - `detail_json` 含 ips、ipCount、windowMinutes、requestCount、concurrentAt，以及 `legacy_status`（原 unread/read/ignored）；
   - 全部导入为 `status=resolved`、`resolution_reason='legacy'`，**不创建** `monitor_alert_states`，避免把历史记录变成当前活跃告警；
   - `push_status`：`sent`→`sent`，`failed`→`failed`，`disabled`/空→`skipped`；`push_error` 保留；   - 旧阈值/冷却写死在代码里，不迁移，新规则使用默认参数。
4. 旧 Telegram 设置 → `monitor_channels`：若旧设置含 bot token 与 chat id，生成一个 `type=telegram` 渠道（`config={botToken,chatId}`，默认 `minSeverity=p1`，enabled 跟随旧开关）；旧设置保留不删。
5. 迁移标记 `monitor_migration_v1` 存 JSON `{checkins:bool, alerts:bool, telegram:bool, legacyMaxAlertId:int}`：`checkins/telegram` 只跑未完成项；`alerts` 每次启动都按 `id > legacyMaxAlertId` 增量导入并更新游标，重复启动不重复导入；渠道按固定 id `legacy-telegram` 写入，避免重复创建。
6. 旧 `alerts` 表、旧 Telegram 设置保留；不删除。
7. 旧前端页面和 HTTP 路由在阶段 2 移除；旧接口不再写入。
8. 回滚：旧代码不读新表；新表不影响旧逻辑；旧页面需回滚旧前端 bundle。发布前保留上一版二进制与 web/dist。

## 14. 失败处理与降级

- **采集器**：panic recover；SQLite busy 重试一次；失败只记日志；连续 5 分钟未成功 → `collector_stale` P2。
- **额度接口**：失败保留上次样本并写 `source_error`；UI 显示“数据不可用/已过期”；超过 `quotaStaleMinutes` 标记过期，不因额度失败判定账号不可用。
- **续期埋点**：写库失败只记日志，不影响续期。
- **推送**：按渠道独立重试；失败不影响事件落库；面板可重试；渠道限流排队不丢弃。
- **探测**：单 provider 并发 1；超时 30s（轻）/90s（CLI）；CLI 缺失跳过；探测失败只影响 `probe_failed` 规则，不直接改 provider 状态。
- **时间**：UTC 落库；每日调度、自然日与静默时段统一按 `monitor_dispatch.timezone`（默认 `Asia/Shanghai`）计算，不依赖进程 TZ；前端按同一时区展示，悬停显示浏览器本地时间。
- **未知能力**：显示不适用，不参与告警，不拿 0 冒充。
- **数据库增长**：事件 90 天、样本 30 天、签到历史 90 天；`monitor_event_pushes` 随所属事件一起删除；维护 ticker 每 6 小时清理；`monitor_alert_states` 常驻但可按 resolved 且 90 天前清理（保留最近 1000 条 resolved 状态）。

## 15. 安全与隐私

- 渠道 secret、Telegram token 只写不读；GET 返回 `secretSet`；日志、事件、推送错误中脱敏。
- 监控消息只含账号标签/provider id/错误码/统计值；不含 token、API Key、请求正文、响应正文、webhook。
- 所有 `/__monitor/*` 仅管理员；沿用现有会话鉴权。
- 错误样本展示前做长度截断和敏感字段过滤；不展示原始请求体。
- 探测使用临时 key/route，结束即清理；临时 key 不出现在密钥列表。
- 迁移和备份不包含 secret 明文到日志。

## 16. 测试策略

### 16.1 Go 单测

- 规则引擎表驱动：每条规则的触发/不触发边界、样本下限、持续窗口、滞回恢复、冷却、抖动抑制、级别升级。
- Collector：窗口聚合、探测流量排除、空响应/伪 200 分类、P95 计算、能力缺失返回 unavailable。
- Dispatcher：P0 立即、P1 即时（阶段 1–3）/摘要（阶段 4）、P2 不推、恢复通知、静默时段、渠道 minSeverity、重试状态。
- 渠道 golden：飞书/钉钉/企微/Telegram 的 payload 与签名；错误码处理；secret 脱敏。
- 迁移：旧 alerts 导入幂等、checkin 历史复制幂等、保留策略清理。
- 探测：事件驱动条件、每日窗口、CLI 缺失跳过、并发互斥。

### 16.2 集成（本地/scratch）

- scratch 实例 + mock 上游 + mock webhook 接收器：模拟 401/403/429/5xx/timeout/空响应/多 IP/令牌过期/签到失败/额度失败。
- 验证：事件落库、状态机、推送 payload、签名、去重、恢复、面板接口。
- 不调用真实厂商；不消耗订阅额度。
- 迁移测试使用旧库副本（脱敏）。

### 16.3 前端

- 1440/1024/390 三档；明暗主题；tab 切换；轮询暂停；深链恢复；空态/错误态/不适用。
- 无控制台报错、无布局跳动；`npm run build` 通过。
- 截图检查：概览、账号抽屉、告警详情、规则与渠道、移动端。

### 16.4 VPS 验收

- 部署后确认采集器心跳、规则状态、真实账号只读信息、接口无密钥泄漏。
- 用 VPS 上的 mock webhook 接收器验证签名与重试。
- 真实飞书/钉钉/企微测试需要用户提供测试群 webhook（见 §18）。
- 真实 Claude 相关测试只在 VPS 进行，禁止本地使用真实凭证。

### 16.5 负载

- 30 个 provider、10 万条日志：概览接口 P95 < 300ms、事件列表 < 200ms；采集 tick < 1s；无 SQLite 锁超时。
- 1000 条事件 + 100 条状态：规则评估 < 100ms。

## 17. 发布计划与验收

### 17.1 分四步发布（每步部署 VPS 并验收）

1. **后端基础**：表结构、采集器、规则引擎、四渠道推送、`/__monitor/*` API；旧数据迁移；单测 + mock 集成。本阶段运行 16 条规则：凭证（`oauth_expiring`、`oauth_refresh_failed`、`auth_invalid`）、额度（`quota_low`、`credit_expiring`、`key_budget_warn`、`key_budget_exhausted`）、链路（`provider_down`、`server_error_spike`、`rate_limit_spike`、`empty_response`、`ttft_regression`、`model_unavailable`）、风控（`multi_ip`、`concurrent_ip`）与 `collector_stale`；`probe_failed`、`checkin_failed` 随阶段 3 的数据源上线。
2. **监控中心壳**：导航替换、概览/账号页接真实数据、告警列表/详情；移除旧入口；前端构建 + 浏览器验收。
3. **探测**：事件驱动/每日轻探测、每周 CLI 深检、签到历史、探测页；VPS 上验证调度与跳过逻辑。
4. **配置与收尾**：规则/渠道/调度设置界面、聚合摘要、恢复通知展示、全量回归与性能验收（静默与冷却的服务端行为在阶段 1 已实现）。

实施计划只覆盖阶段 1（表结构、采集器、规则引擎、渠道、API、迁移、测试）；阶段 2–4 在阶段 1 验收通过后分别编写计划，避免单份计划跨四个交付面。

旧扫描器切换：阶段 1 启动新引擎后，`StartAlertScan` 继续写旧 `alerts` 表供旧页面读取，但关闭其 Telegram 推送（临时开关），避免与 Dispatcher 双推；阶段 2 删除旧扫描器、旧 `/__alerts*` 路由与旧页面。删除前先执行一次增量导入（`alerts.id > legacyMaxAlertId`），把阶段 1 期间旧扫描器写入的行补进事件流，然后冻结旧表。

P1 投递：阶段 1–3 先逐条即时发送（仍受冷却与静默约束），阶段 4 上线聚合摘要后切换为 15 分钟摘要；静默时段的顺延行为从阶段 1 起生效（使用默认配置），阶段 4 才提供设置界面；通知内容模型与渠道适配不变。

阶段 1 实现 `/__monitor/*` 的子集：`overview`、`accounts`（列表/详情/续期/额度刷新）、`events`（列表）、`alerts`（列表/详情/ack/ignore/重试推送）、`channels`（CRUD/test）、`dispatch`（GET/PUT）。`accounts/{id}/probe`、`probes*` 随阶段 3 上线，`rules*` 随阶段 4 上线；阶段 1 规则使用内置默认参数，不提供编辑接口。

### 17.2 验收标准

- 坏号在 2 分钟内（token/续期）或 5–10 分钟内（请求链路）被发现；凭证类规则 60 秒 tick，请求类规则按各自窗口 + 持续时间生效。
- 探测流量不污染账号成功率；P0/P1 去重、冷却、恢复符合预期；确认/忽略不停止检测。
- 四渠道测试通过；消息无密钥泄漏；推送失败可单独重试。
- 18 条规则均可开关/改阈值；不支持的能力显示“不适用”。
- 概览/账号/探测/告警/规则渠道五页在 1440/1024/390、明暗主题下可用；无控制台报错。
- 旧 alerts 完整迁移；旧页面入口移除；`go test ./...` 与 `npm run build` 通过。
- VPS 部署后采集器稳定运行 30 分钟，无锁超时、无请求路径影响。

## 18. 依赖与风险

- **真实 webhook**：飞书/钉钉/企微最终验收需要用户提供测试群 webhook；在此之前用 mock 接收器验证签名与格式。
- **CLI 可用性**：VPS 必须安装 OpenCode/Codex/Claude 三个 CLI 才能跑每周深检；缺失时跳过并记录，不告警。实现前先检查。
- **流截断**：首版不新增统一流截断埋点；用空响应/伪 200 覆盖常见症状。阶段 3 评估统一的 `stream_incomplete` 标记，涉及多个转换器，暂不作为验收项。
- **额度接口覆盖**：各厂商接口能力不同；能力矩阵以现有 handler 为准，未覆盖的显示不适用。
- **SQLite 写竞争**：监控写事件/状态独立于请求写日志；用短事务、批量提交；压测验证。
- **包边界**：`internal/monitoring` 不 import `internal/gateway`；数据源、探测执行、渠道发送通过接口注入，避免循环依赖。
- **计划边界**：规格覆盖四个阶段，实施计划只写阶段 1；阶段 2–4 在本阶段验收后单独出计划，接口与 UI 以本规格为准。
- **生产数据**：迁移前备份 `gateway.db`；迁移只增不删；回滚保留上一版二进制和前端 bundle。
