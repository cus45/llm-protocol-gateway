import { useMemo } from 'react';
import type { ReactNode } from 'react';
import { BrandIcon } from '../components/ui';
import { formatCostMicros } from '../lib';
import type { MonitorAccount, MonitorAlert, MonitorEvent, MonitorOverview, MonitorSeverity } from './types';
import { hasTime, SEVERITY_TEXT } from './types';
import { detailPairs, displayTime, displayTimeCompact, formatLatency, formatRatio, quotaText, relativeToNow, SEVERITY_TONE } from './format';

export interface OverviewTabProps {
  overview: MonitorOverview | null;
  accounts: MonitorAccount[];
  alerts: MonitorAlert[];
  /** 当前筛选下告警总条数（alerts 只是第一页，右卡标题要报全量）。 */
  alertTotal: number;
  events: MonitorEvent[];
  eventTotal: number;
  kindFilter: string;
  severityFilter: MonitorSeverity | '';
  timezone: string;
  /** 品牌图标键由外层提供（见 AccountsTab 同名说明）。 */
  iconOf?: (providerId: string) => string | undefined;
  loading: boolean;
  error: string;
  onKindFilter: (kind: string) => void;
  onSeverityFilter: (severity: MonitorSeverity | '') => void;
  onLoadMoreEvents: () => void;
  onOpenAccount: (providerId: string) => void;
  onOpenAlert: (alertId: number) => void;
  /** 告警卡「全部」：切到告警页看完整列表（概览右卡只给最近的几条）。 */
  onOpenAlerts: () => void;
  onRetry: () => void;
}

const KIND_CHIPS: Array<{ id: string; label: string }> = [
  { id: '', label: '全部' },
  { id: 'alert', label: '告警' },
  { id: 'renewal', label: '续期' },
  { id: 'quota', label: '额度' },
  { id: 'checkin', label: '签到' },
  { id: 'system', label: '系统' },
];

const SEVERITY_CHIPS: Array<MonitorSeverity | ''> = ['', 'p0', 'p1', 'p2'];

/**
 * 账号行的优先排序：问题账号永远在前。判据依次是活跃告警、额度不可用/极低、
 * 凭证已过期、模型列表连续失败、停用。同档保持名字序，避免每次刷新跳位。
 */
function accountScore(account: MonitorAccount, now: number): number {
  if (account.disabled) return 0;
  let score = 0;
  if (account.activeAlerts > 0) score += 100 + account.activeAlerts;
  if (account.healthStatus === 'unavailable') score += 60;
  if (account.quota?.error || (account.quota && account.quota.ratio < 0)) score += 30;
  else if (account.quota && account.quota.ratio >= 0 && account.quota.ratio < 0.1) score += 25;
  if (hasTime(account.expiresAt) && Date.parse(account.expiresAt as string) < now) score += 40;
  if (account.modelFailures > 0) score += 10;
  return score;
}

/** 账号名过长时保留原词、在两处折行：空白处，以及中文/全角标点之后（断在「订阅（」之前，不把一个词拆成两行）。 */
const NAME_BREAK = new RegExp('(\\s+|(?<=[\\u3000-\\u303f\\uff00-\\uffef]))', 'u');

function breakableName(name: string): ReactNode[] {
  return name
    .split(NAME_BREAK)
    .filter((part) => part !== '')
    .map((part, index) => (
      <span key={index}>
        {part}
        <wbr />
      </span>
    ));
}

/** 告警与事件里只带 ruleKey（如 key_budget_warn）；这里按规则表的中文名直出，未知 key 原样显示。
    固定成前端常量是因为概览页不额外拉规则表；与后端 `/__monitor/rules` 的 name 保持一致。 */
const RULE_LABELS: Record<string, string> = {
  auth_invalid: '凭证失效',
  checkin_failed: '签到连续失败',
  collector_stale: '监控采集器停滞',
  concurrent_ip: 'API Key 并发 IP',
  credit_expiring: '积分即将到期',
  empty_response: '空响应',
  key_budget_exhausted: '密钥额度耗尽',
  key_budget_warn: '密钥额度预警',
  model_unavailable: '模型列表拉取失败',
  multi_ip: 'API Key 多 IP 使用',
  oauth_expiring: '凭证即将过期',
  oauth_refresh_failed: '凭证连续续期失败',
  probe_failed: '轻探测失败',
  provider_down: 'Provider 成功率过低',
  quota_low: '额度不足',
  rate_limit_spike: '上游限流激增',
  server_error_spike: '上游 5xx 激增',
  ttft_regression: '首包延迟回退',
  // 以下三条是历史告警留下的 key（规则表里已不存在，库里还留着旧指纹），按语义给中文名。
  quota_auth_failed: '额度鉴权失败',
  quota_stale: '额度读数滞后',
  renewal_failed: '凭证续期失败',
};

/** 后端旧版 key 用点号（quota.auth_failed），规则表用下划线，两种都认。 */
function ruleLabel(key: string): string {
  return RULE_LABELS[key] || RULE_LABELS[key.replace(/\./g, '_')] || key;
}

const EVENT_SHOWN = 20;

/** 副标题只留一行短语，完整口径放在悬停里（不再占首屏）。 */
const OVERVIEW_NOTE_TITLE = [
  '请求指标（成功率 / P95 / 成本）取自 24h 请求日志；窗口内没有请求时三项显示「—」，不显示 0%。',
  '雷达：百分比 = 各行悬停可见的公式，不是满意度评分；综合分只取有数据维度的均值，无数据的维度标「无数据」。',
  '延迟分布：接口只给 P95，没有逐请求直方图，所以只报 P95 落在哪一档，不报各档次数与占比。',
].join('\n');

/**
 * 雷达 / 频谱的取值纪律。概览接口只给「成功率、P95、成本、账号计数、告警分布」，
 * 因此：
 *
 * - 能算的维度一律列出；算不出来的维度整行标「无数据」并写明缺哪个字段，不补 0、不画 0% 条；
 * - 雷达卡按「够填满卡片」取维度数：可算的不足五维时，后面接「无数据」行占位，
 *   卡片不会因为维度少而空着；
 * - 百分比一律按界面上写明的公式算（悬停可查），不是拍的满意度；
 * - 基线（参考图里「24ms / 基线 50ms」）接口没有，就只写目标，不造基线。
 *
 * 「响应延迟」用 P95 与首包延迟回退规则的默认阈值比（= 目标上限），
 * 「凭证健康」用未过期凭证占比，其余公式见 radarDims。
 */
const TTFT_BUDGET_MS = 3000;

type Tone = 'accent' | 'green' | 'amber' | 'red' | 'blue';

interface RadarDim {
  key: string;
  name: string;
  /** 0..100；null 表示无数据（整行以「无数据」呈现，不画进度条）。 */
  percent: number | null;
  /** 进度条右侧的「当前值 vs 目标」；无数据时为空。 */
  value: string;
  note: string;
  tone: Tone;
  /** 百分比 / 结论是怎么来的（悬停可查，避免读者把评分当满意度）。 */
  formula: string;
}

/** 延迟档位：只给区间语义，不给逐档次数——接口没有直方图，凭空分次数会让合计 ≠ 总数。 */
interface LatencyBand {
  key: 'lt20' | '20to50' | '50to100' | 'gt100';
  label: string;
  hint: string;
  max: number;
  tone: Tone;
}

const LATENCY_BANDS: LatencyBand[] = [
  { key: 'lt20', label: '< 20ms', hint: '极速', max: 20, tone: 'green' },
  { key: '20to50', label: '20 – 50ms', hint: '平稳', max: 50, tone: 'accent' },
  { key: '50to100', label: '50 – 100ms', hint: '偏慢', max: 100, tone: 'amber' },
  { key: 'gt100', label: '> 100ms', hint: '超过 100ms', max: Infinity, tone: 'red' },
];

const KIND_TEXT: Record<string, string> = {
  alert: '告警',
  renewal: '续期',
  probe: '探测',
  checkin: '签到',
  quota: '额度',
  system: '系统',
};

/** 条带每段的颜色：P95 落点用该档色，其余用中性灰（只表示区间，不表示数量）。 */
function inBandTone(index: number, hit: number): string {
  return index === hit ? `hit ${LATENCY_BANDS[index].tone}` : 'idle';
}

function clampPercent(value: number): number {
  return Math.max(0, Math.min(100, value));
}

function pct(value: number): string {
  return `${value.toFixed(value >= 99.95 || value < 10 ? 1 : 0)}%`;
}

/** 量卡右上角的细线图标：与侧栏 NavIcon 同一笔画语言，path 由调用方给。 */
function Glyph({ d }: { d: string }) {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.7} strokeLinecap="round" strokeLinejoin="round" width={16} height={16}>
      <path d={d} />
    </svg>
  );
}

const GLYPH = {
  account: 'M4 20v-1.5A4.5 4.5 0 0 1 8.5 14h7a4.5 4.5 0 0 1 4.5 4.5V20M12 10.5a4 4 0 1 0 0-8 4 4 0 0 0 0 8Z',
  success: 'M12 3.5 19 6v5.5c0 4.3-2.9 7.6-7 9-4.1-1.4-7-4.7-7-9V6l7-2.5ZM8.8 12l2.2 2.2 4.2-4.4',
  alert: 'M12 4 2.8 20h18.4L12 4ZM12 10v4.2M12 17.2h.01',
  cost: 'M12 3v18M16.5 7.2c-.7-1.4-2.3-2.2-4.2-2.2-2.4 0-4.3 1.2-4.3 3.1 0 4.3 8.6 2.3 8.6 6.6 0 1.9-1.9 3.1-4.3 3.1-2 0-3.7-.9-4.4-2.3',
};

function MetaCard({
  label,
  icon,
  tone,
  value,
  unit,
  chip,
  chipTone,
  note,
  noteRight,
}: {
  label: string;
  icon: ReactNode;
  tone: Tone;
  value: string;
  unit?: string;
  chip?: string;
  chipTone?: Tone;
  note: string;
  noteRight?: string;
}) {
  return (
    <div className={`card mon-meta mon-meta-${tone}`}>
      <div className="mon-meta-head">
        <span className="mon-meta-label">{label}</span>
        <span className={`mon-meta-icon ${tone}`} aria-hidden>{icon}</span>
      </div>
      <div className="mon-meta-value-row">
        <span className="mon-meta-value">{value}</span>
        {unit && <span className="mon-meta-unit">{unit}</span>}
        {chip && <span className={`mon-meta-chip ${chipTone ?? tone}`}>{chip}</span>}
      </div>
      <div className="mon-meta-foot">
        <span>{note}</span>
        {noteRight && <span className={`mon-meta-foot-right ${tone}`}>{noteRight}</span>}
      </div>
    </div>
  );
}

export function OverviewTab(props: OverviewTabProps) {
  const {
    overview, accounts, alerts, alertTotal, events, eventTotal, kindFilter, severityFilter, timezone, iconOf,
    loading, error, onKindFilter, onSeverityFilter, onLoadMoreEvents, onOpenAccount, onOpenAlert, onOpenAlerts, onRetry,
  } = props;
  const now = Date.now();

  const ordered = useMemo(
    () =>
      [...accounts].sort((a, b) => {
        const delta = accountScore(b, now) - accountScore(a, now);
        if (delta !== 0) return delta;
        return a.name.localeCompare(b.name, 'zh-CN');
      }),
    // now 只用于「是否已过期」判断，刷新节奏由轮询驱动，不必进依赖。
    [accounts],
  );

  const usable = overview ? Math.max(0, overview.accounts.total - overview.accounts.unavailable - overview.accounts.disabled) : 0;
  const req = overview?.requests;

  // ── 雷达维度：能算的全部列出，算不出的标「无数据」占位 ─────────────────
  const radarDims = useMemo<RadarDim[]>(() => {
    if (!overview) return [];
    const dims: RadarDim[] = [];
    const total = overview.accounts.total;
    const active = total - overview.accounts.disabled;
    const p95 = overview.requests.p95LatencyMs;
    const hasTraffic = overview.requests.total > 0 && p95 > 0;

    // 1) 响应延迟：100 − P95 ÷ 预算 × 50（0ms 满分、预算处 50 分、超预算 0 分）。
    dims.push({
      key: 'latency',
      name: '响应延迟',
      percent: hasTraffic ? clampPercent(100 - (p95 / TTFT_BUDGET_MS) * 50) : null,
      value: hasTraffic ? `P95 ${formatLatency(p95)}` : '',
      note: hasTraffic ? `目标 ≤ ${formatLatency(TTFT_BUDGET_MS)}` : '窗口内没有请求，没有 P95',
      tone: 'accent',
      formula: hasTraffic
        ? `100 − P95 ÷ 预算 × 50，P95 = ${formatLatency(p95)}，预算 = ${formatLatency(TTFT_BUDGET_MS)}（首包延迟回退规则的默认阈值）`
        : '24h 请求为 0，接口不给 P95，也没给逐请求延迟',
    });

    // 2) 容灾冗余：未停用账号 ÷ 全部账号。
    dims.push({
      key: 'redundancy',
      name: '容灾冗余',
      percent: total > 0 ? clampPercent((active / total) * 100) : null,
      value: total > 0 ? `${active}/${total} 个可用` : '',
      note: total > 0 ? `停用 ${overview.accounts.disabled}` : '还没有接入任何账号',
      tone: 'green',
      formula: '未停用账号 ÷ 全部账号',
    });

    // 3) 凭证健康：只算带到期时间的订阅凭证；一个都没有就标无数据，而不是给 100%。
    const withExpiry = accounts.filter((account) => hasTime(account.expiresAt));
    const valid = withExpiry.filter((account) => Date.parse(account.expiresAt as string) > now);
    const renewalFailed = accounts.filter((account) => account.renewal?.known && account.renewal.lastOK === false);
    dims.push({
      key: 'credential',
      name: '凭证健康',
      percent: withExpiry.length > 0 ? clampPercent((valid.length / withExpiry.length) * 100) : null,
      value: withExpiry.length > 0 ? `${valid.length}/${withExpiry.length} 未过期` : '',
      note:
        withExpiry.length > 0
          ? `已过期 ${withExpiry.length - valid.length} · 续期失败 ${renewalFailed.length}`
          : '没有带到期时间的凭证',
      tone: 'blue',
      formula:
        withExpiry.length > 0
          ? '未过期凭证 ÷ 有到期时间的凭证；续期失败数来自 renewal.lastOK'
          : '账号里没有「凭证到期时间」（expiresAt），无从判断',
    });

    // 4) 额度余量：只统计读数可用的账号（ratio ≥ 0 且无 error）。
    const quotaRead = accounts.filter((account) => account.quota && !account.quota.error && account.quota.ratio >= 0);
    const quotaAvg = quotaRead.length > 0
      ? quotaRead.reduce((sum, account) => sum + (account.quota?.ratio ?? 0), 0) / quotaRead.length
      : null;
    dims.push({
      key: 'quota',
      name: '额度余量',
      percent: quotaAvg === null ? null : clampPercent(quotaAvg * 100),
      value: quotaAvg === null ? '' : `均值 ${pct(quotaAvg * 100)}`,
      note: quotaAvg === null ? '没有可用的额度读数' : `读数 ${quotaRead.length}/${accounts.length}`,
      tone: 'amber',
      formula: '有额度读数的账号的剩余比例均值；读数失败（error）与不可用（ratio<0）不计入',
    });

    // 5) 告警健康：当前活跃告警里 P0 越多越差；P0 每条扣 40 分，封底 0。
    const firing = overview.firing;
    dims.push({
      key: 'alerts',
      name: '告警健康',
      percent: firing.total > 0 ? clampPercent(100 - firing.p0 * 40) : firing.total === 0 ? 100 : null,
      value: firing.total > 0 ? `${firing.total} 条活跃` : '',
      note: firing.total > 0 ? `P0 ${firing.p0} · P1 ${firing.p1} · P2 ${firing.p2}` : '当前没有进行中的告警',
      tone: firing.p0 > 0 ? 'red' : firing.total > 0 ? 'amber' : 'green',
      formula: '100 − P0 条数 × 40（封底 0）；P1/P2 在副注里列出，不参与扣分',
    });

    return dims;
  }, [overview, accounts, now]);

  // 综合分 = 有数据维度的均值；无数据维度不参与，免得把「不知道」算成低分。
  const scored = radarDims.filter((dim) => dim.percent !== null);
  const radarScore = scored.length > 0 ? scored.reduce((sum, dim) => sum + (dim.percent ?? 0), 0) / scored.length : null;

  // ── 延迟分布：接口只有 P95，没有逐请求直方图。所以只报「P95 落在哪一档」，
  //    不给每一档编造次数 / 占比（那样各档加起来对不上总数）。 ──
  const spectrum = useMemo(() => {
    if (!req || req.total <= 0 || !(req.p95LatencyMs > 0)) return null;
    const p95 = req.p95LatencyMs;
    const index = LATENCY_BANDS.findIndex((band, i) => p95 <= band.max && (i === 0 || p95 > LATENCY_BANDS[i - 1].max));
    const band = LATENCY_BANDS[index >= 0 ? index : LATENCY_BANDS.length - 1];
    return { p95, band, index };
  }, [req]);

  const latest = events[0];
  const latestStamp = latest ? displayTimeCompact(displayTime(latest.time, timezone).text) : '—';
  const collectorStamp = overview && hasTime(overview.collectorLastSuccess) ? displayTime(overview.collectorLastSuccess, timezone) : null;
  const latestPairs = latest ? detailPairs(latest.detail, 5).map(([key, value]) => `${key}=${value}`).join(' · ') : '';

  return (
    <div className="card panel mon-overview">
      <div className="panel-header">
        <div>
          <h2 className="panel-title">{'监控中心概览'}</h2>
          <p className="panel-desc">
            {`窗口 ${overview ? overview.windowHours : 24} 小时 · 采集器心跳 ${collectorStamp ? collectorStamp.text : '还没有成功采样'}`}
          </p>
          <p className="panel-desc mon-overview-note" title={OVERVIEW_NOTE_TITLE}>{'雷达与延迟分布均由现有接口换算'}</p>
        </div>
        <div className="panel-header-actions">
          <button className="btn btn-sm" onClick={onRetry} disabled={loading}>
            {loading ? '刷新中…' : '立即刷新'}
          </button>
        </div>
      </div>

      {error && (
        <div className="metric">
          <div className="metric-label">{'读取失败'}</div>
          <div className="metric-value err">{error}</div>
        </div>
      )}

      {/* 四张量卡：标签 + 图标 / 超大数字 + 小单位 + 胶囊 / 一行副注。
          契约 §5：「没有数据」不许渲染成 0% / 0 次 / ¥0——请求三项在窗口内零请求时
          统一显示「—」并写明原因，否则会被读成链路全挂。 */}
      <div className="grid-4 mon-meta-row">
        <MetaCard
          label={'可用账号'}
          icon={<Glyph d={GLYPH.account} />}
          tone={overview && overview.accounts.unavailable > 0 ? 'amber' : 'green'}
          value={overview ? String(usable) : '—'}
          unit={overview ? `/ ${overview.accounts.total}` : undefined}
          chip={overview && overview.accounts.total > 0 ? `${pct((usable / overview.accounts.total) * 100)} 可用` : undefined}
          chipTone={overview && overview.accounts.unavailable > 0 ? 'amber' : 'green'}
          note={overview ? `异常 ${overview.accounts.unavailable} · 停用 ${overview.accounts.disabled}` : '等待采集'}
          noteRight={overview && overview.accounts.subscribed > 0 ? `订阅 ${overview.accounts.subscribed}` : undefined}
        />
        <MetaCard
          label={`${overview ? overview.windowHours : 24}h 成功率`}
          icon={<Glyph d={GLYPH.success} />}
          tone={'accent'}
          value={req && req.total > 0 ? formatRatio(req.successRate) : '—'}
          chip={req && req.total > 0 && req.p95LatencyMs > 0 ? `P95 ${formatLatency(req.p95LatencyMs)}` : undefined}
          chipTone={'blue'}
          note={req ? (req.total > 0 ? `${req.total.toLocaleString()} 条 · 成功 ${req.success.toLocaleString()}` : '该窗口内没有请求') : '等待采集'}
        />
        <MetaCard
          label={'活跃告警'}
          icon={<Glyph d={GLYPH.alert} />}
          tone={overview && overview.firing.p0 > 0 ? 'red' : overview && overview.firing.total > 0 ? 'amber' : 'green'}
          value={overview ? String(overview.firing.total) : '—'}
          chip={overview && overview.firing.p0 + overview.firing.p1 > 0 ? '需处理' : undefined}
          chipTone={'red'}
          note={overview ? `P0 ${overview.firing.p0} · P1 ${overview.firing.p1} · P2 ${overview.firing.p2}` : '等待采集'}
          noteRight={overview ? `渠道 ${overview.channels.enabled}/${overview.channels.total}` : undefined}
        />
        <MetaCard
          label={`${overview ? overview.windowHours : 24}h 成本`}
          icon={<Glyph d={GLYPH.cost} />}
          tone={'blue'}
          /* 没有价格行时绝不能显示 $0.00：那看起来像"免费模型"，实际是"没计价"。 */
          value={req && req.priced > 0 ? formatCostMicros(req.costMicros) : '—'}
          chip={req && req.priced > 0 ? `计价 ${pct((req.priced / Math.max(1, req.total)) * 100)}` : undefined}
          chipTone={'green'}
          note={
            req
              ? req.total === 0
                ? '该窗口内没有请求'
                : req.priced > 0
                  ? `计价覆盖 ${req.priced.toLocaleString()}/${req.total.toLocaleString()}`
                  : '这些模型还没有价格行'
              : '等待采集'
          }
        />
      </div>

      {/* 雷达（五维评分）+ 延迟分布。两卡用 subgrid 共享轨道：卡片高度跟随内容多的那张，
          不固定撑满；某张卡内容少时，它的脚注贴底，中间留白而不是把行拉长。 */}
      <div className="mon-duo">
        <div className="card mon-duo-card">
          <div className="mon-card-head">
            <span className="mon-card-title">{'集群健康雷达'}</span>
            {radarScore !== null && <span className="mon-score">{`综合 ${radarScore.toFixed(1)}/100`}</span>}
          </div>
          {radarDims.length === 0 && (
            <div className="mon-dims-empty">
              <span className="mon-dims-empty-title">{'暂无可评估的维度'}</span>
              <span>{'接口没有可换算的数据时这里整段留空，不用 0% 的进度条充数。'}</span>
            </div>
          )}
          <div className="mon-dim-list">
            {radarDims.map((dim) => (
              <div className={`mon-dim${dim.percent === null ? ' is-empty' : ''}`} key={dim.key}>
                <div className="mon-dim-top">
                  <span className="mon-dim-name" title={dim.formula}>{dim.name}</span>
                  {dim.percent === null ? (
                    <span className="mon-dim-empty-tag" title={dim.formula}>{'无数据'}</span>
                  ) : (
                    <span className="mon-dim-right">
                      <span className="mon-dim-value">{dim.value}</span>
                      <span className={`mon-dim-pct ${dim.tone}`}>{pct(dim.percent)}</span>
                    </span>
                  )}
                </div>
                {dim.percent === null ? (
                  // 无数据不画进度条：一条 0% 的空槽会被读成「这项就是 0 分」。
                  <div className="mon-dim-empty-note">{dim.note}</div>
                ) : (
                  <>
                    <div className="mon-bar" role="presentation">
                      <span className={`mon-bar-fill ${dim.tone}`} style={{ width: `${dim.percent}%` }} />
                    </div>
                    <div className="mon-dim-note">{dim.note}</div>
                  </>
                )}
              </div>
            ))}
          </div>
          <div className="mon-card-foot">
            <span>{'综合分 = 有数据维度的均值；无数据维度不参与'}</span>
          </div>
        </div>

        <div className="card mon-duo-card">
          <div className="mon-card-head">
            <span className="mon-card-title">{'请求耗时分布'}</span>
            <span className="mon-card-sub">
              {req && req.total > 0
                ? `24h · ${req.total.toLocaleString()} 次`
                : req ? '该窗口内没有请求' : '等待采集'}
            </span>
          </div>
          {!spectrum && (
            <div className="mon-dims-empty">
              <span className="mon-dims-empty-title">{'该窗口内没有请求'}</span>
              <span>{'没有请求就没有延迟样本，也没有可报告的 P95 档位。'}</span>
            </div>
          )}
          {spectrum && (
            <>
              {/* P95 结论单独一条：接口只有 P95，逐档次数与占比无从计算，所以不列。 */}
              <div className="mon-band-summary">
                <span className="mon-band-p95">{`P95 ${formatLatency(spectrum.p95)}`}</span>
                <span className="mon-band-where">{`落在 ${spectrum.band.label} 区间`}</span>
                <span className={`mon-dim-pct ${spectrum.band.tone}`}>{'5% 的请求 ≥ P95'}</span>
              </div>
              {/* 四个区间只标出 P95 落在哪一档：其余档没有任何计数，不填 0 次 / 0.0%。 */}
              <div className="mon-bar-strip" role="img" aria-label={`P95 ${formatLatency(spectrum.p95)}，落在 ${spectrum.band.label} 区间`}>
                {LATENCY_BANDS.map((band, index) => (
                  <span key={band.key} className={`mon-bar-seg ${inBandTone(index, spectrum.index)}`} title={`${band.label}${index === spectrum.index ? '（P95 落点）' : ''}`} />
                ))}
              </div>
              <div className="mon-band-table" role="table" aria-label="延迟区间">
                {LATENCY_BANDS.map((band, index) => {
                  const inBand = index === spectrum.index;
                  return (
                    <div className={`mon-band${inBand ? ' is-hit' : ''}`} key={band.key} role="row">
                      <span className="mon-band-label" role="cell">
                        <span className={`mon-dot ${inBand ? band.tone : 'muted'}`} aria-hidden />
                        {band.label}
                      </span>
                      <span className="mon-band-state" role="cell">{inBand ? `P95 落点 · ${band.hint}` : '未报告'}</span>
                    </div>
                  );
                })}
              </div>
            </>
          )}
          <div className="mon-card-foot">
            <span>{'接口只给 P95，没有逐请求直方图；各档不报次数与占比'}</span>
          </div>
        </div>
      </div>

      <div className="mon-log-table">
        <div className="card panel m-col-card">
          <div className="m-panel-head">
            <div className="panel-title">{`账号健康（${ordered.length}）`}</div>
            <span className="m-panel-note">{'问题账号排在前面'}</span>
          </div>
          {ordered.length === 0 && <div className="m-empty">{'还没有账号。接入 Provider 后这里会出现状态矩阵。'}</div>}
          {/* 账号多时列表内部滚动：右卡同高，整页不被 26 行拉长。 */}
          <div className="m-scroll-list mon-account-list">
            {ordered.map((account) => {
              const quota = quotaText(account.quota);
              const quotaTone = account.quota?.error
                ? 'has-err'
                : account.quota && account.quota.ratio >= 0 && account.quota.ratio < 0.1
                  ? 'has-warn'
                  : '';
              const troubled = accountScore(account, now) > 0;
              const renewal = hasTime(account.expiresAt) ? `续期 ${relativeToNow(account.expiresAt, now)}` : '不过期';
              return (
                <div className="log-row" key={account.providerId}>
                  <div className="mrow mrow-account-mini">
                    <span className="m-acc-name">
                      <span className={`dot ${troubled ? 'err' : 'ok'}`} aria-hidden />
                      <BrandIcon icon={iconOf?.(account.providerId)} name={account.name} size={18} />
                      <span className="m-acc-label" title={`${account.name}（${account.vendor || '未知厂商'}）`}>
                        {/* 行宽不够时在词与词之间折行，而不是把整句挤成省略号。 */}
                        {breakableName(account.name)}
                      </span>
                    </span>
                    {/* 续期与额度各占一行：半宽卡里横排会把「续期 24 天后」折成「剩余…」，读不出来。 */}
                    <span className="m-acc-facts">
                      <span className="m-acc-fact" title={`凭证到期：${account.expiresAt ?? '无'}`}>
                        <b>{'凭证'}</b>
                        <span>{renewal}</span>
                      </span>
                      <span className="m-acc-fact quota" title={quota.note}>
                        <b>{'额度'}</b>
                        <span className={quotaTone}>{quota.primary}</span>
                      </span>
                    </span>
                    <span className="m-acc-badges">
                      {/* 概览这半宽卡里放不下厂商列；模型列表连续失败才是这里要看的信号。 */}
                      {account.modelFailures > 0 && <span className="badge slate">{`列表 ${account.modelFailures}×`}</span>}
                      {account.activeAlerts > 0 ? (
                        <span className="badge red">{`告警 ${account.activeAlerts}`}</span>
                      ) : account.healthStatus === 'unavailable' ? (
                        // 请求判定为异常但还没有成级告警（例如刚过冷却/未达持续窗口）：
                        // 不能显示「正常」，否则概览说异常、列表说正常，自相矛盾。
                        <span className="badge amber">{'异常'}</span>
                      ) : (
                        <span className="badge green">{'正常'}</span>
                      )}
                      {account.disabled && <span className="badge amber">{'已停用'}</span>}
                    </span>
                    <button className="btn btn-sm m-acc-detail" onClick={() => onOpenAccount(account.providerId)}>{'详情'}</button>
                  </div>
                </div>
              );
            })}
          </div>
        </div>

        <div className="card panel m-col-card">
          <div className="m-panel-head">
            <div className="panel-title">{`当前告警（${alerts.length}）`}</div>
            <div className="m-panel-note" style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              {alertTotal > alerts.length && <span>{`共 ${alertTotal} 条`}</span>}
              {alerts.length > 6 && <button className="mini-btn" onClick={onOpenAlerts}>{'全部'}</button>}
            </div>
          </div>
          {alerts.length === 0 && <div className="m-empty">{'没有进行中的告警。'}</div>}
          {/* 与左卡同高、内部滚动：告警少时不留死白，多时也不把整页拉长。 */}
          <div className="m-scroll-list mon-account-list">
            {alerts.slice(0, 20).map((alert) => (
              <div className="log-row" key={alert.id}>
                <div className="mrow mrow-alert-mini">
                  <span className={`badge ${SEVERITY_TONE[alert.severity]}`} title={SEVERITY_TEXT[alert.severity]}>
                    {alert.severity.toUpperCase()}
                  </span>
                  <span className="m-alert-main">
                    <span className="m-alert-title">{alert.targetName || ruleLabel(alert.ruleKey)}</span>
                    <span className="m-alert-meta" title={alert.ruleKey}>{`${ruleLabel(alert.ruleKey)} · 第 ${alert.count} 次 · ${relativeToNow(alert.lastSeen, now)}`}</span>
                  </span>
                  <button className="btn btn-sm" onClick={() => onOpenAlert(alert.id)}>{'处理'}</button>
                </div>
              </div>
            ))}
          </div>
        </div>
      </div>

      <div className="card panel">
        <div className="m-panel-head has-filters">
          <div className="panel-title">{`事件流（${eventTotal}）`}</div>
          {/* 概览只拉最近一段事件，标题写实际条数，不然「事件流（12）」和列表对不上。 */}
          {eventTotal > events.length && <span className="m-panel-note">{`显示最近 ${events.length} 条`}</span>}
          {/* 不带 flexWrap：隐藏时为空字符串的「刷新中…」，原样式里的 wrap 会留下一个空行撑高。 */}
          <div className="panel-header-actions" style={{ display: 'flex', gap: 6 }}>
            <div className="usage-segments">
              {KIND_CHIPS.map((chip) => (
                <button key={chip.id || 'all'} className={kindFilter === chip.id ? 'active' : ''} onClick={() => onKindFilter(chip.id)}>
                  {chip.label}
                </button>
              ))}
            </div>
            <div className="usage-segments">
              {SEVERITY_CHIPS.map((value) => (
                <button key={value || 'any'} className={severityFilter === value ? 'active' : ''} onClick={() => onSeverityFilter(value)}>
                  {value ? value.toUpperCase() : '任意级别'}
                </button>
              ))}
            </div>
          </div>
        </div>
        {events.length === 0 && !loading && <div className="m-empty">{'这个筛选下还没有事件。'}</div>}
        <div className="mon-log-table">
          {events.slice(0, EVENT_SHOWN).map((event) => {
          const pairs = detailPairs(event.detail);
          const stamp = displayTime(event.time, timezone);
          return (
            <div className="log-row" key={event.id}>
              <div className="mrow mrow-event">
                <span className="m-alert-meta" title={stamp.text}>
                  <span className="m-stamp-full">{stamp.text}</span>
                  <span className="m-stamp-short">{displayTimeCompact(stamp.text)}</span>
                </span>
                <span className={`badge ${SEVERITY_TONE[event.severity]}`}>{event.severity.toUpperCase()}</span>
                <span className="m-event-title" title={event.title}>
                  {event.title}
                  {event.targetId ? (
                    <button className="m-event-link" onClick={() => onOpenAccount(event.targetId as string)}>
                      {event.targetName || event.targetId}
                    </button>
                  ) : null}
                </span>
                <span
                  className="m-event-detail"
                  title={pairs.map(([key, value]) => `${key}=${value}`).join(' · ')}
                >
                  {pairs.length > 0
                    ? pairs.map(([key, value]) => `${key}=${value}`).join(' · ')
                    : event.status === 'resolved' && event.resolutionReason
                      ? `已恢复（${event.resolutionReason}）`
                      : '—'}
                </span>
              </div>
            </div>
          );
        })}
        </div>
        {/* 拉到 20 条后先收住：概览是摘要，继续往后翻是告警页/日志页的事。 */}
        {events.length > EVENT_SHOWN && (
          <div style={{ paddingTop: 8 }}>
            <button className="btn btn-sm" onClick={onLoadMoreEvents} disabled={loading}>{'再看更多'}</button>
          </div>
        )}
      </div>

      {/* 实时状态条：最近一条事件的时间 + 等宽 payload，右侧进入事件流。 */}
      <div className="mon-statusbar">
        <div className="mon-statusbar-main">
          <span className="mon-statusbar-tag">
            <span className="live-dot" aria-hidden />
            {'最新遥测'}
          </span>
          {latest ? (
            <p className="mon-statusbar-text">
              <span className="mon-statusbar-time">{latestStamp}</span>
              <span className="mon-statusbar-kind">{`[${KIND_TEXT[latest.kind] ?? latest.kind}]`}</span>
              <span className="mon-statusbar-title">{latest.title}</span>
              {latestPairs && <span className="mon-statusbar-payload" title={latestPairs}>{`(${latestPairs})`}</span>}
            </p>
          ) : (
            <p className="mon-statusbar-text">{'还没有事件。'}</p>
          )}
        </div>
        <button className="btn btn-sm mon-statusbar-btn" onClick={() => onKindFilter('')}>
          {'进入实时事件流'}
        </button>
      </div>
    </div>
  );
}
