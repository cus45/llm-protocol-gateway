/**
 * 监控中心类型。字段以 VPS 上 `/__monitor/*` 的真实响应为准（2026-10-24 抓取核对），
 * 不是凭设计文档推测——尤其注意：
 *
 * - 时间一律是字符串（Go 侧 RFC3339），零值会以 `0001-01-01T00:00:00Z` 出现，
 *   必须当「没有」处理，不能显示公元 1 年。
 * - 额度只有比率被持久化（provider_metric_samples 只有 quota_ratio/credits 两列），
 *   `remaining`/`limit` 在列表里恒为 0，属「不知道」而不是「剩 0」，渲染前必须判。
 * - `ratio < 0` 表示额度读数不可用（含上游失败），同样不是 0。
 */

export type MonitorSeverity = 'p0' | 'p1' | 'p2' | 'info';

export type MonitorEventKind = 'alert' | 'renewal' | 'probe' | 'checkin' | 'quota' | 'system';

export type MonitorAlertStatus = 'firing' | 'resolved' | 'info';
export type MonitorDisposition = 'open' | 'acknowledged' | 'ignored';
export type MonitorPushStatus = 'sent' | 'failed' | 'partial' | 'pending' | 'skipped' | 'suppressed';

/** 后端只回分类，绝不回上游原文（原文可能带 token）。 */
export type MonitorErrorKind = 'auth_failed' | 'rate_limited' | 'network' | 'unknown';

export interface MonitorCapabilities {
  quota: boolean;
  checkin: boolean;
  models: boolean;
  oauth: boolean;
}

export interface MonitorQuota {
  source: string;
  remaining: number;
  limit: number;
  /** 剩余比例 0..1；<0 表示不可用。 */
  ratio: number;
  expiresAt: string;
  observedAt: string;
  stale: boolean;
  error?: MonitorErrorKind;
}

export interface MonitorRenewalOutcome {
  known: boolean;
  lastAt: string;
  lastOK: boolean;
  consecutive: number;
  lastError?: string;
}

export interface MonitorAccount {
  providerId: string;
  name: string;
  vendor: string;
  authType: string;
  disabled: boolean;
  healthStatus: string;
  capabilities: MonitorCapabilities;
  expiresAt?: string;
  quota?: MonitorQuota | null;
  renewal?: MonitorRenewalOutcome | null;
  modelFailures: number;
  activeAlerts: number;
}

/** 时间线里的一次人工处置（后端把它挂在最新事件的 detail.timeline 上）。 */
export interface MonitorTimelineEntry {
  action: string;
  time: string;
  actor?: string;
}

export interface MonitorEvent {
  id: number;
  time: string;
  kind: MonitorEventKind;
  severity: MonitorSeverity;
  ruleKey?: string;
  targetType: string;
  targetId?: string;
  targetName?: string;
  title: string;
  detail: Record<string, unknown>;
  fingerprint?: string;
  status: MonitorAlertStatus;
  count: number;
  firstSeen: string;
  lastSeen: string;
  resolvedAt?: string;
  resolutionReason?: string;
  pushStatus?: MonitorPushStatus;
  pushError?: string;
}

export interface MonitorAlert {
  id: number;
  fingerprint: string;
  ruleKey: string;
  severity: MonitorSeverity;
  targetType: string;
  targetId: string;
  targetName: string;
  status: MonitorAlertStatus;
  disposition: MonitorDisposition;
  count: number;
  firstSeen: string;
  lastSeen: string;
  lastNotifiedAt: string;
  acknowledgedAt: string;
  ignoredAt: string;
  suppressedUntil: string;
  unavailableSince: string;
  resolutionReason?: string;
  detail: Record<string, unknown>;
  latestEventId: number;
  pushStatus?: MonitorPushStatus;
  pushError?: string;
  relatedEventIds: number[];
}

/** 告警详情：后端把最新事件一并带回，时间线在 latestEvent.detail.timeline 里。 */
export interface MonitorAlertDetail {
  alert: MonitorAlert;
  latestEvent: MonitorEvent | null;
}

export interface MonitorOverview {
  generatedAt: string;
  windowHours: number;
  requests: {
    total: number;
    success: number;
    successRate: number;
    p95LatencyMs: number;
    costMicros: number;
    /**
     * 有价格行的请求数。costMicros==0 且 priced==0 是「没计价」，不是「免费」——
     * 与用量页同一条硬规则：未计价绝不显示成 $0.00。
     */
    priced: number;
  };
  accounts: { total: number; subscribed: number; unavailable: number; disabled: number };
  firing: { total: number; p0: number; p1: number; p2: number };
  channels: { total: number; enabled: number };
  collectorLastSuccess: string;
  recentEvents: MonitorEvent[];
}

export interface MonitorPage<T> {
  items: T[];
  total: number;
  page: number;
  pageSize: number;
}

/**
 * 渠道在链路上永远只有 *Set 布尔：后端不回 webhook/secret/botToken 明文，
 * 表单留空即「保留原值」（与后端 PUT 语义一致）。
 */
export interface MonitorChannelView {
  id: string;
  type: string;
  name: string;
  minSeverity: MonitorSeverity;
  enabled: boolean;
  webhookSet?: boolean;
  secretSet?: boolean;
  tokenSet?: boolean;
  chatId?: string;
}

export interface MonitorChannelInput {
  type: string;
  name: string;
  minSeverity: MonitorSeverity;
  enabled: boolean;
  webhook?: string;
  secret?: string;
  botToken?: string;
  chatId?: string;
}

export interface MonitorDispatchConfig {
  digestMinutes: number;
  quietEnabled: boolean;
  quietStart: string;
  quietEnd: string;
  recoveryNotify: boolean;
  timezone: string;
  baseUrl: string;
}

export interface MonitorRenewalResult {
  ok: boolean;
  providerId: string;
  vendor: string;
  /** 未到续期窗口时 false——这不是失败，文案要分开。 */
  renewed: boolean;
  expiresAt?: string;
  errorKind?: MonitorErrorKind;
}

export interface MonitorQuotaResult {
  ok: boolean;
  quota?: MonitorQuota;
  errorKind?: MonitorErrorKind;
}

export interface MonitorChannelTestResult {
  ok: boolean;
  channelId: string;
  status: MonitorPushStatus;
  latencyMs: number;
  error?: string;
  pushStatus?: MonitorPushStatus;
}

/** Go 零值时间会以字符串出现，必须当成「无」。 */
export function hasTime(raw?: string | null): boolean {
  if (!raw) return false;
  return !raw.startsWith('0001-01-01');
}

/** 额度绝对值没有被采样保存，limit=0 是「不知道」而不是「用完了」。 */
export function quotaAbsoluteKnown(quota?: MonitorQuota | null): boolean {
  return !!quota && quota.limit > 0;
}

/** ratio<0 表示额度读数不可用（含上游失败）。 */
export function quotaRatioKnown(quota?: MonitorQuota | null): boolean {
  return !!quota && quota.ratio >= 0;
}

export const SEVERITY_TEXT: Record<MonitorSeverity, string> = {
  p0: 'P0 严重',
  p1: 'P1 重要',
  p2: 'P2 提醒',
  info: '信息',
};

export const ERROR_KIND_TEXT: Record<MonitorErrorKind, string> = {
  auth_failed: '登录凭证已失效，需重新授权',
  rate_limited: '上游限流',
  network: '网络不可达',
  unknown: '未知错误',
};

// ── 探测（阶段 3）───────────────────────────────────────────────────────

/** 一个账号的探测现状。probed=false 是「还没探过」，不是「探测全过」。 */
export interface MonitorProbeAccount {
  providerId: string;
  name: string;
  vendor?: string;
  disabled: boolean;
  probed: boolean;
  consecutiveFailures: number;
  lastProbeAt?: string;
  lastStatus?: number;
  skipped?: string;
  usedToday: number;
  maxPerDay: number;
}

export interface MonitorProbeRun {
  providerId: string;
  kind: string;
  case: string;
  ok: boolean;
  status: number;
  skipped?: string;
  durationMs: number;
  model?: string;
  errorCode?: string;
  day: string;
  time: string;
}

export interface MonitorProbeConfig {
  eventDriven: { enabled: boolean; idleMinutes: number; maxPerDayPerAccount: number; minIntervalMinutes: number };
  daily: { enabled: boolean; windowStart: string; windowEnd: string };
  weeklyCli: { enabled: boolean; weekday: number; time: string };
  quotaStaleMinutes: number;
}

export interface MonitorProbePage {
  accounts: MonitorProbeAccount[];
  runs: MonitorProbeRun[];
  config: MonitorProbeConfig;
}

/**
 * 一轮探测返回里每个 case 的结果（后端 probeCaseResult 的 JSON 视图）。
 * 没有 json tag（解析时大小写不敏感），实测字段名是 `Case`/`OK`/`Status`/`Duration`……，
 * 所以这里按真实响应写；小写键是将来补了 tag 之后的形态，两种都接。
 */
export interface MonitorProbeCaseResult {
  Case?: string;
  OK?: boolean;
  Status?: number;
  Duration?: number;
  Model?: string;
  Error?: string;
  case?: string;
  ok?: boolean;
  status?: number;
  durationMs?: number;
  model?: string;
  error?: string;
}

/**
 * 手动探测的结果。HTTP 409 是「同账号已有探测在跑」（`skipped:"in_flight"`），
 * 不是失败；400 的 `errorKind:"kind_unavailable"` 是请求了未上线的 kind。
 */
export interface MonitorProbeRunResult {
  ok: boolean;
  providerId: string;
  kind: string;
  at?: string;
  skipped?: string;
  errorKind?: string;
  cases?: MonitorProbeCaseResult[];
}

/** 把后端两种命名形态收敛成界面用的一个形状。 */
export function normalizeProbeCase(raw: MonitorProbeCaseResult): {
  key: string;
  ok: boolean;
  status: number;
  durationMs: number;
  model: string;
  error: string;
} {
  return {
    key: raw.case ?? raw.Case ?? '',
    ok: raw.ok ?? raw.OK ?? false,
    status: raw.status ?? raw.Status ?? 0,
    durationMs: raw.durationMs ?? raw.Duration ?? 0,
    model: raw.model ?? raw.Model ?? '',
    error: raw.error ?? raw.Error ?? '',
  };
}

/** 跳过原因的界面文案。没跑成不等于跑失败，尤其 cli_missing 绝不能显示成失败。 */
export const PROBE_SKIP_TEXT: Record<string, string> = {
  cli_missing: 'CLI 未安装，未判定失败',
  in_flight: '已有探测在进行中',
  no_model: '没有可用模型，未发出请求',
  provider_disabled: '账号已停用，未探测',
  provider_missing: '账号不存在或已删除',
  // CLI 深检的整轮上限（2 小时）到了，剩余用例没跑；not_run 是兜底原因。
  // 两者都不是失败，只是「没跑完」。
  round_budget: '整轮超过 2 小时上限，剩余用例未跑',
  not_run: '整轮结束时仍未跑',
  running: '深检正在进行中',
};

export function probeSkipText(skipped: string | undefined): string {
  if (!skipped) return '';
  return PROBE_SKIP_TEXT[skipped] ?? `已跳过（${skipped}）`;
}

/** 探测 kind 文案：轻探测 / CLI 深检。 */
export function probeKindText(kind: string): string {
  if (kind === 'light') return '轻探测';
  if (kind === 'cli') return 'CLI 深检';
  return kind || '—';
}

/** 深检 caseID 形如 `<providerId>|<client>|<kind>`，映射成「客户端 · chat/tool」。 */
const SELFCHECK_CLIENT_TEXT: Record<string, string> = {
  opencode: 'OpenCode',
  codex: 'Codex',
  claude: 'Claude',
};

export function probeCaseText(key: string): string {
  if (key === 'chat') return '最小对话';
  if (key === 'tools') return '工具调用';
  if (key === 'prepare' || key === 'cli') return '深检准备';
  if (key.startsWith('selfcheck:')) {
    const caseId = key.slice('selfcheck:'.length);
    const parts = caseId.split('|');
    if (parts.length === 3) {
      const client = SELFCHECK_CLIENT_TEXT[parts[1]] ?? parts[1];
      return `${client} · ${parts[2]}`;
    }
    return caseId || '—';
  }
  return key || '—';
}
