import { useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { formatCostMicros } from '../lib';
import { IconSelect } from '../components/selects';
import type { IconSelectOption } from '../components/selects';
import type { MonitorAccount, MonitorEvent, MonitorOverview, MonitorProbeRun } from './types';
import { hasTime } from './types';
import { detailPairs, displayTime, displayTimeCompact, formatLatency, formatRatio } from './format';

export interface OverviewTabProps {
  overview: MonitorOverview | null;
  accounts: MonitorAccount[];
  /** 底部遥测条只取最近一条事件；概览不再内嵌事件列表。 */
  events: MonitorEvent[];
  timezone: string;
  loading: boolean;
  error: string;
  onOpenAccount: (providerId: string) => void;
  /**
   * 概览自己维护的跳转：量卡「调控熔断 →」与拓扑脚注落到账号页，状态条落到实时流。
   * 不传时退回账号详情，保证旧调用方不炸。
   */
  onOpenTab?: (tab: 'accounts' | 'alerts' | 'probes') => void;
  /** 轻探测记录：拓扑的 RTT 用它。概览页不拉一次探测接口（只在探测页拉），
      所以这里通常为空——RTT 缺失时集群行显示「RTT —」，不编造毫秒数。 */
  probeRuns?: MonitorProbeRun[];
  /** 品牌图标键（与账号页同一套，见 AccountsTab 同名说明）；不传就退回名字首字母。 */
  iconOf?: (providerId: string) => string | undefined;
}

type Tone = 'accent' | 'green' | 'amber' | 'red' | 'blue';

const AUTH_LABELS: Record<string, string> = {
  api_key: 'API Key',
  claude_oauth: 'Claude 订阅',
  chatgpt_oauth: 'ChatGPT 订阅',
  cursor_oauth: 'Cursor 订阅',
  qoder_pat: 'Qoder 订阅',
  qoder_cn: 'Qoder CN 订阅',
  workbuddy: 'WorkBuddy 订阅',
  workbuddy_ai: 'WorkBuddy AI 订阅',
  zcode: 'ZCode 订阅',
  trae_cn: 'Trae CN 订阅',
  trae_global: 'Trae Global 订阅',
  opencode_zen: 'OpenCode Zen',
};

const UNKNOWN_VENDOR = '未标注厂商';

// 拓扑筛选值：全部 / 某厂商全部账号 / 单个账号。与日志页、账号页同一套二级筛选，
// 只额外保留「整个厂商」这一档，点一下就能看这家所有节点的分流。
const VENDOR_FILTER_ALL = 'all';
const VENDOR_FILTER_PREFIX = 'vendor:';
const PROVIDER_FILTER_PREFIX = 'provider:';

const STATUS_TEXT: Record<'full' | 'degraded' | 'down', string> = {
  full: '全速运行',
  degraded: '降级运行',
  down: '异常隔离',
};

const CLIENT_VIEWS = [
  { key: 'subscription', label: '订阅制客户端', hint: 'OAuth 续期链' },
  { key: 'api', label: 'API Key 直连', hint: 'Bearer 鉴权' },
  { key: 'offline', label: '停用 / 异常', hint: '需人工处置' },
] as const;

/** 核心底部的能力标签：与参考版式的「路由引擎特性」同一位置，文案按本网关真实能力写。 */
const CORE_TAGS: Array<{ key: string; label: string; tone: Tone }> = [
  { key: 'retry', label: '自适应重试', tone: 'accent' },
  { key: 'halfopen', label: '熔断半开恢复', tone: 'green' },
  { key: 'renew', label: '凭证自动续期', tone: 'blue' },
];

/** 空值一律给「—」：拓扑里占比 / SLA 没有数据时不许写成 0%。 */
function shareText(value: number | null | undefined): string {
  return value === null || value === undefined ? '—' : pct(value * 100);
}

function groupOf(account: MonitorAccount): 'subscription' | 'api' | 'offline' {
  if (account.disabled || account.healthStatus === 'unavailable') return 'offline';
  return account.capabilities.oauth ? 'subscription' : 'api';
}

/** 大数压缩成 k/M，与参考版式的大数字同一种读法；完整条数写在副注里。 */
function compactCount(value: number): string {
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(value >= 10_000_000 ? 0 : 1)}M`;
  if (value >= 10_000) return `${(value / 1_000).toFixed(value >= 100_000 ? 0 : 1)}k`;
  return value.toLocaleString('zh-CN');
}

/** 24h 窗口内的平均吞吐；窗口没有请求时返回「—」而不是 0 req/s。 */
function throughputText(total: number, windowHours: number): string {
  if (!(windowHours > 0) || !(total > 0)) return '—';
  const rps = total / (windowHours * 3600);
  if (rps >= 100) return `${Math.round(rps)} req/s`;
  if (rps >= 1) return `${rps.toFixed(1)} req/s`;
  return `${rps.toFixed(2)} req/s`;
}

/**
 * 实时流量拓扑（参考 MonitoringCenter.tsx:760-1000 的三栏版式）。数据映射：
 *
 * - 调用端：按凭证形态分三组（订阅 OAuth / API Key / 停用异常），占比 = 该组账号数 ÷ 账号总数；
 *   协议层没有逐客户端流量，口径写在卡片脚注里。
 * - 核心：成功率与 P95 用 24h 请求汇总，吞吐 = 24h 请求数 ÷ 窗口秒数；窗口内零请求时写「—」。
 * - 上游集群：按 vendor 聚合账号；RTT = 该集群账号最近一次轻探测耗时的均值；
 *   状态：全部停用 → 异常隔离；有活跃告警或健康度非 healthy → 降级运行；否则全速运行；
 *   SLA 沿用接口给的全局 24h 成功率（行内不加「全局」二字会误导成逐集群值，脚注已写明）；
 *   窗口内没有请求时（SLA 只能是「—」）改报该集群的轻探测成功率，连探测记录都没有就写明「无流量」。
 */
function TopologyCard({
  accounts,
  overview,
  probeRuns,
  timezone,
  loading,
  iconOf,
  onOpenAccount,
  onOpenAllAccounts,
}: {
  accounts: MonitorAccount[];
  overview: MonitorOverview | null;
  probeRuns: MonitorProbeRun[];
  timezone: string;
  loading: boolean;
  iconOf?: (providerId: string) => string | undefined;
  onOpenAccount: (providerId: string) => void;
  onOpenAllAccounts: () => void;
}) {
  // 没有厂商名的账号各自成组会让筛选按钮刷出一长串「未标注厂商」，按 label 折叠成一簇。
  const keyOf = (account: MonitorAccount) => account.vendor || (AUTH_LABELS[account.authType] || account.authType || '').trim() || UNKNOWN_VENDOR;
  const [providerFilter, setProviderFilter] = useState(VENDOR_FILTER_ALL);
  const req = overview?.requests;
  const total = accounts.length;
  const windowHours = overview ? overview.windowHours : 24;

  const vendors = useMemo(() => {
    const map = new Map<string, MonitorAccount[]>();
    for (const account of accounts) {
      const key = keyOf(account);
      const list = map.get(key);
      if (list) list.push(account);
      else map.set(key, [account]);
    }
    return [...map.entries()].sort((a, b) => b[1].length - a[1].length || a[0].localeCompare(b[0], 'zh-CN'));
  }, [accounts]);

  // 每个账号最近一次真正跑过的轻探测耗时：durationMs=0 是「没计时」，不是 0ms。
  const rttByAccount = useMemo(() => {
    const latest = new Map<string, number>();
    for (const run of [...probeRuns].sort((a, b) => Date.parse(b.time) - Date.parse(a.time))) {
      if (run.skipped || !(run.durationMs > 0) || latest.has(run.providerId)) continue;
      latest.set(run.providerId, run.durationMs);
    }
    return latest;
  }, [probeRuns]);

  // 轻探测结论按账号汇总：窗口里真正跑过的发数（skipped 不是结论）与成功数。
  const probeStats = useMemo(() => {
    const map = new Map<string, { ok: number; total: number }>();
    for (const run of probeRuns) {
      if (run.skipped) continue;
      const item = map.get(run.providerId) ?? { ok: 0, total: 0 };
      item.total += 1;
      if (run.ok) item.ok += 1;
      map.set(run.providerId, item);
    }
    return map;
  }, [probeRuns]);

  const clusters = useMemo(
    () =>
      vendors.map(([vendor, list]) => {
        const alerts = list.reduce((sum, account) => sum + account.activeAlerts, 0);
        const offline = list.filter((account) => groupOf(account) === 'offline').length;
        const rtts = list.map((account) => rttByAccount.get(account.providerId)).filter((value): value is number => value !== undefined);
        const rtt = rtts.length > 0 ? rtts.reduce((sum, value) => sum + value, 0) / rtts.length : null;
        // 一个告警都没在报的账号才叫全速；没告警但有停用/异常账号的集群是降级。
        const status = offline === list.length ? 'down' : alerts > 0 || offline > 0 ? 'degraded' : 'full';
        const probed = list.map((account) => probeStats.get(account.providerId)).filter((item): item is { ok: number; total: number } => !!item);
        const probeTotal = probed.reduce((sum, item) => sum + item.total, 0);
        const probeRate = probeTotal > 0 ? probed.reduce((sum, item) => sum + item.ok, 0) / probeTotal : null;
        return { vendor, name: `${vendor} 集群`, nodes: list.length, probeRate, probeTotal, share: total > 0 ? list.length / total : null, alerts, offline, rtt, status, vendorAccounts: list.map((account) => account.providerId) } as const;
      }),
    [vendors, rttByAccount, probeStats, total],
  );

  const providerOptions = useMemo<IconSelectOption[]>(() => {
    const options: IconSelectOption[] = [{ value: VENDOR_FILTER_ALL, label: '全部 Provider' }];
    for (const [vendor, list] of vendors) {
      const groupKey = `${VENDOR_FILTER_PREFIX}${vendor}`;
      options.push({
        value: groupKey,
        label: `全部 ${vendor} 账号`,
        icon: list.map((account) => iconOf?.(account.providerId)).find(Boolean),
        description: `${list.length} 个账号`,
        keywords: `${vendor} 全部`,
        group: vendor,
        groupKey,
        groupAll: true,
      });
      for (const account of [...list].sort((a, b) => a.name.localeCompare(b.name, 'zh-CN'))) {
        options.push({
          value: `${PROVIDER_FILTER_PREFIX}${account.providerId}`,
          label: account.name,
          icon: iconOf?.(account.providerId),
          description: AUTH_LABELS[account.authType] || account.authType || 'API Key',
          keywords: `${account.providerId} ${vendor} ${account.name}`,
          group: vendor,
          groupKey,
        });
      }
    }
    return options;
  }, [vendors, iconOf]);

  // 筛选收窄调用端占比；集群行保留并置灰（参考实现同样保留行）。
  const inScope = (account: MonitorAccount) => {
    if (providerFilter === VENDOR_FILTER_ALL) return true;
    if (providerFilter.startsWith(PROVIDER_FILTER_PREFIX)) {
      return account.providerId === providerFilter.slice(PROVIDER_FILTER_PREFIX.length);
    }
    return keyOf(account) === providerFilter.slice(VENDOR_FILTER_PREFIX.length);
  };
  const scopedAccounts = accounts.filter(inScope);
  const scopeVendor =
    providerFilter === VENDOR_FILTER_ALL
      ? null
      : providerFilter.startsWith(VENDOR_FILTER_PREFIX)
        ? providerFilter.slice(VENDOR_FILTER_PREFIX.length)
        : (() => {
            const account = accounts.find((item) => item.providerId === providerFilter.slice(PROVIDER_FILTER_PREFIX.length));
            return account ? keyOf(account) : null;
          })();
  const scopeName =
    providerFilter === VENDOR_FILTER_ALL
      ? ''
      : providerFilter.startsWith(VENDOR_FILTER_PREFIX)
        ? providerFilter.slice(VENDOR_FILTER_PREFIX.length)
        : accounts.find((account) => account.providerId === providerFilter.slice(PROVIDER_FILTER_PREFIX.length))?.name ?? '该账号';
  const clients = CLIENT_VIEWS.map((view, index) => {
    const group = scopedAccounts.filter((account) => groupOf(account) === view.key);
    const authTypes = [...new Set(group.map((account) => AUTH_LABELS[account.authType] || account.authType || 'API Key'))];
    return {
      ...view,
      count: group.length,
      share: scopedAccounts.length > 0 ? group.length / scopedAccounts.length : null,
      auth: authTypes.length > 0 ? authTypes.slice(0, 2).join(' / ') : '无账号',
      tone: (['accent', 'green', 'amber'] as const)[index],
    };
  });

  const firing = overview?.firing;
  const usable = overview ? overview.accounts.total - overview.accounts.disabled : null;
  const tone: Tone = !overview ? 'accent' : firing && firing.p0 > 0 ? 'red' : firing && firing.total > 0 ? 'amber' : 'green';
  const collector = overview && hasTime(overview.collectorLastSuccess) ? displayTimeCompact(displayTime(overview.collectorLastSuccess, timezone).text) : '';

  const status = !overview
    ? { tone: 'idle', text: loading ? '等待采集' : '暂无数据' }
    : firing && firing.p0 > 0
      ? { tone: 'warn', text: `${firing.p0} 条 P0 待处理` }
      : firing && firing.total > 0
        ? { tone: 'warn', text: `运行中 · ${firing.total} 条活跃告警` }
        : { tone: 'ok', text: '自适应重试已挂载' };

  return (
    <section className={`mon-topo tone-${tone}`} aria-label="实时流量拓扑">
      <div className="mon-topo-head">
        <div className="mon-topo-head-main">
          <span className="mon-topo-ping" aria-hidden />
          {/* 逐词切开允许在词间断行；整句在 1440 下是一行，窄屏折两行而不是被挤成省略号。 */}
          <h3 className="mon-topo-title">
            {`智能协议网关实时流量拓扑与自愈流水线 `}
            <span className="mon-topo-title-en">{'(Live Routing Mesh)'}</span>
          </h3>
          <span className={`mon-topo-badge ${status.tone}`}>{status.text}</span>
        </div>
        <div className="mon-topo-filter">
          <span className="mon-topo-filter-label">{'查看节点分流'}</span>
          <div className="mon-topo-select">
            <IconSelect
              value={providerFilter}
              options={providerOptions}
              onChange={setProviderFilter}
              placeholder="全部 Provider"
              searchable
              renderPortal={false}
            />
          </div>
        </div>
      </div>

      <div className="mon-topo-body">
        <div className="mon-topo-clients">
          <div className="mon-topo-group">
            <Glyph d={GLYPH.key} />
            <span>{'调用端 (CLIENTS)'}</span>
          </div>
          {total === 0 && <div className="mon-topo-empty">{'还没有接入任何账号'}</div>}
          {clients.map((client) => (
            <div key={client.key} className={`mon-client ${client.tone}`} title={`${client.label} · ${client.hint} · ${client.count} 个账号`}>
              <div className="mon-client-top">
                <span className="mon-client-name">{client.label}</span>
                <span className="mon-client-share">{`${shareText(client.share)} 占比`}</span>
              </div>
              <div className="mon-client-id">{`${client.auth} · ${client.count} 账号`}</div>
              {client.share !== null && <div className="mon-progress" role="presentation"><span className="mon-progress-fill" style={{ width: `${clampPercent(client.share * 100)}%` }} /></div>}
            </div>
          ))}
          <div className="mon-topo-note">{'占比 = 该组账号数 ÷ 账号总数'}</div>
        </div>

        <div className="mon-core-wrap">
          <div className="mon-core">
            <span className="mon-core-glow" aria-hidden />
            <div className="mon-core-inner">
              <span className="mon-core-icon" aria-hidden>
                <Glyph d={GLYPH.core} />
              </span>
              <h4 className="mon-core-title">{'智能路由与容灾自愈核心'}</h4>
              <p className="mon-core-version">{`Protocol Gateway Mesh · ${windowHours}h 窗口${collector ? ` · 采样 ${collector}` : ''}`}</p>
              <div className="mon-core-stats">
                <div className="mon-core-stat">
                  <span className="mon-core-stat-label">{'实时吞吐'}</span>
                  <span className={`mon-core-stat-value${req && req.total > 0 ? '' : ' empty'}`}>{throughputText(req?.total ?? 0, windowHours)}</span>
                </div>
                <div className="mon-core-stat">
                  <span className="mon-core-stat-label">{'请求成功率'}</span>
                  <span className={`mon-core-stat-value${req && req.total > 0 ? ' green' : ' empty'}`}>{req && req.total > 0 ? formatRatio(req.successRate) : '—'}</span>
                </div>
                <div className="mon-core-stat">
                  <span className="mon-core-stat-label">{'P95 延迟'}</span>
                  <span className={`mon-core-stat-value${req && req.p95LatencyMs > 0 ? ' accent' : ' empty'}`}>{req && req.p95LatencyMs > 0 ? formatLatency(req.p95LatencyMs) : '—'}</span>
                </div>
              </div>
              <div className="mon-core-tags">
                {CORE_TAGS.map((tag) => (
                  <span key={tag.key} className={`mon-core-tag ${tag.tone}`}>{tag.label}</span>
                ))}
              </div>
            </div>
          </div>
        </div>

        <div className="mon-topo-upstream">
          <div className="mon-topo-group">
            <Glyph d={GLYPH.server} />
            <span>{'上游 PROVIDER 容灾集群'}</span>
          </div>
          <div className="mon-cluster-list">
            {clusters.length === 0 && <div className="mon-topo-empty">{'还没有接入任何厂商'}</div>}
            {clusters.map((cluster) => {
              const dim = scopeVendor !== null && scopeVendor !== cluster.vendor;
              return (
                <div key={cluster.vendor} className={`mon-cluster${dim ? ' is-dim' : ''}`}>
                  <button
                    type="button"
                    className="mon-cluster-hit"
                    aria-label={`查看 ${cluster.name} 的账号`}
                    onClick={() => onOpenAccount(cluster.vendorAccounts[0])}
                  />
                  <div className="mon-cluster-main">
                    <div className="mon-cluster-title">
                      <span className="mon-cluster-name">{cluster.name}</span>
                      <span className="mon-cluster-nodes">{`${cluster.nodes} 节点`}</span>
                    </div>
                    <div className="mon-cluster-meta">
                      <span className={cluster.rtt !== null ? 'accent' : 'muted'} title="该集群账号最近一次轻探测耗时的均值">{cluster.rtt !== null ? `${Math.round(cluster.rtt)}ms` : 'RTT —'}</span>
                      <span aria-hidden>·</span>
                      {/* 有请求：SLA 用全局 24h 成功率；没有请求：退到探测成功率；都没记录：写明「无流量」。 */}
                      <span
                        title={
                          req && req.total > 0
                            ? '接口只给全局 24h 成功率，这里沿用全局值'
                            : cluster.probeRate !== null
                              ? `24h 无请求；探测成功率 = 窗口内 ${cluster.probeTotal} 发轻探测的成功数 ÷ 总数`
                              : '24h 无请求，窗口内也没有该集群的轻探测记录'
                        }
                      >
                        {req && req.total > 0
                          ? `SLA ${formatRatio(req.successRate)}`
                          : cluster.probeRate !== null
                            ? `探测成功率 ${formatRatio(cluster.probeRate)} · ${cluster.probeTotal} 发`
                            : '无流量 · 无探测'}
                      </span>
                    </div>
                  </div>
                  <div className="mon-cluster-side">
                    <span className="mon-cluster-share">{shareText(cluster.share)}</span>
                    <span className={`mon-cluster-status ${cluster.status}`} title={`异常/停用 ${cluster.offline} · 活跃告警 ${cluster.alerts} 条`}>
                      {STATUS_TEXT[cluster.status]}
                    </span>
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      </div>

      <div className="mon-topo-foot">
        <span>
          {providerFilter === VENDOR_FILTER_ALL
            ? `${total} 个账号 · ${clusters.length} 个厂商；RTT 取最近一次轻探测，有请求时 SLA 为全局 24h`
            : `只看「${scopeName}」：${scopedAccounts.length} 个账号；其余集群置灰`}
        </span>
        <button type="button" className="mon-topo-link" onClick={onOpenAllAccounts}>
          {'进入账号矩阵 →'}
        </button>
      </div>
    </section>
  );
}

const RADAR_DIMS = 5;
const RADAR_CENTER = 100;
const RADAR_RADIUS = 76;

const KIND_TEXT: Record<string, string> = {
  alert: '告警',
  renewal: '续期',
  probe: '探测',
  checkin: '签到',
  quota: '额度',
  system: '系统',
};

function radarAngle(index: number): number {
  return ((-90 + (360 / RADAR_DIMS) * index) * Math.PI) / 180;
}

function radarPoint(index: number, ratio: number): [number, number] {
  const angle = radarAngle(index);
  return [
    RADAR_CENTER + Math.cos(angle) * RADAR_RADIUS * ratio,
    RADAR_CENTER + Math.sin(angle) * RADAR_RADIUS * ratio,
  ];
}

const RADAR_GRID = [0.33, 0.66, 1].map((scale) =>
  Array.from({ length: RADAR_DIMS }, (_, index) => radarPoint(index, scale).join(',')).join(' '),
);

const RADAR_AXES = Array.from({ length: RADAR_DIMS }, (_, index) => radarPoint(index, 1));

function clampPercent(value: number): number {
  return Math.max(0, Math.min(100, value));
}

function pct(value: number): string {
  return `${value.toFixed(value >= 99.95 ? 1 : 0)}%`;
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
  server: 'M4 5.5h16v5H4v-5ZM4 13.5h16v5H4v-5ZM7.2 8h.01M7.2 16h.01',
  success: 'M12 3.5 19 6v5.5c0 4.3-2.9 7.6-7 9-4.1-1.4-7-4.7-7-9V6l7-2.5ZM8.8 12l2.2 2.2 4.2-4.4',
  bolt: 'M13 2.5 4.5 13.5H11l-1 8L19.5 10H13l0-7.5Z',
  key: 'M15.2 9.8 20 5M20 5h-3.6M20 5v3.6M15.2 9.8a4.2 4.2 0 1 1-3.5-3.5',
  core: 'M9 3.5h6M9 20.5h6M3.5 9v6M20.5 9v6M7 7h10v10H7V7ZM10.5 10.5h3v3h-3v-3Z',
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
  noteRightTone,
  noteRightTitle,
  action,
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
  noteRightTone?: Tone;
  noteRightTitle?: string;
  action?: { label: string; onClick: () => void };
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
        {action ? (
          <button type="button" className="mon-meta-link" onClick={action.onClick}>{action.label}</button>
        ) : (
          noteRight && <span className={`mon-meta-foot-right ${noteRightTone ?? tone}`} title={noteRightTitle}>{noteRight}</span>
        )}
      </div>
    </div>
  );
}

export function OverviewTab(props: OverviewTabProps) {
  const {
    overview, accounts, events, timezone, loading, error, iconOf, onOpenAccount, onOpenTab, probeRuns = [],
  } = props;
  const now = Date.now();

  const total = overview?.accounts.total ?? accounts.length;
  const disabled = overview?.accounts.disabled ?? 0;
  const unavailable = overview?.accounts.unavailable ?? 0;
  const usable = overview ? Math.max(0, total - unavailable - disabled) : null;
  const req = overview?.requests;
  const hasTraffic = !!req && req.total > 0;
  const windowHours = overview ? overview.windowHours : 24;

  // 实例里没有厂商名时按空值折叠，避免「孤岛厂商」被算成每一个匿名账号。
  const vendors = useMemo(() => {
    const map = new Map<string, number>();
    for (const account of accounts) {
      const key = account.vendor || (AUTH_LABELS[account.authType] || account.authType || '').trim() || UNKNOWN_VENDOR;
      map.set(key, (map.get(key) ?? 0) + 1);
    }
    return map;
  }, [accounts]);
  // 孤岛 = 该厂商只有一个账号：没有同厂商备用节点，单点故障无处切换。
  const islands = [...vendors.values()].filter((count) => count === 1).length;

  // 熔断三态按账号现状换算：闭合 = 可继续调度；半开 = 请求失败后在冷却/试探；
  // 阻断 = 人工强制隔离（isolated）。三者互斥，加起来等于账号总数。
  const isolated = accounts.filter((account) => account.isolated).length;
  const halfOpen = accounts.filter((account) => !account.disabled && account.healthStatus === 'unavailable').length;
  const closed = Math.max(0, total - disabled - halfOpen);
  const breakerTone: Tone = isolated > 0 ? 'red' : halfOpen > 0 ? 'amber' : 'green';

  // ── 雷达维度：能算的全部列出，算不出的标「无数据」占位 ─────────────────
  const radarDims = useMemo<RadarDim[]>(() => {
    if (!overview) return [];
    const dims: RadarDim[] = [];
    const totalAccounts = overview.accounts.total;
    const active = totalAccounts - overview.accounts.disabled;
    const p95 = overview.requests.p95LatencyMs;
    const hasReq = overview.requests.total > 0 && p95 > 0;

    // 1) 响应延迟：100 − P95 ÷ 预算 × 50（0ms 满分、预算处 50 分、超预算 0 分）。
    dims.push({
      key: 'latency',
      name: '响应延迟',
      percent: hasReq ? clampPercent(100 - (p95 / TTFT_BUDGET_MS) * 50) : null,
      value: hasReq ? `P95 ${formatLatency(p95)}` : '',
      note: hasReq ? `目标 ≤ ${formatLatency(TTFT_BUDGET_MS)}` : '窗口内没有请求，没有 P95',
      tone: 'accent',
      formula: hasReq
        ? `100 − P95 ÷ 预算 × 50，P95 = ${formatLatency(p95)}，预算 = ${formatLatency(TTFT_BUDGET_MS)}（首包延迟回退规则的默认阈值）`
        : '24h 请求为 0，接口不给 P95，也没给逐请求延迟',
    });

    // 2) 容灾冗余：未停用账号 ÷ 全部账号。
    dims.push({
      key: 'redundancy',
      name: '容灾冗余',
      percent: totalAccounts > 0 ? clampPercent((active / totalAccounts) * 100) : null,
      value: totalAccounts > 0 ? `${active}/${totalAccounts} 个可用` : '',
      note: totalAccounts > 0 ? `停用 ${overview.accounts.disabled}` : '还没有接入任何账号',
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

    // 5) 告警健康：P0 每条扣 40、P1 扣 15、P2 扣 5，封底 0。
    //    只扣 P0 会让「3 条活跃告警」仍显示 100%，那个读数没人信。
    const firing = overview.firing;
    dims.push({
      key: 'alerts',
      name: '告警健康',
      percent: firing.total > 0 ? clampPercent(100 - firing.p0 * 40 - firing.p1 * 15 - firing.p2 * 5) : 100,
      value: firing.total > 0 ? `${firing.total} 条活跃` : '',
      note: firing.total > 0 ? `P0 ${firing.p0} · P1 ${firing.p1} · P2 ${firing.p2}` : '当前没有进行中的告警',
      tone: firing.p0 > 0 ? 'red' : firing.total > 0 ? 'amber' : 'green',
      formula: '100 − P0 × 40 − P1 × 15 − P2 × 5（封底 0）；每一级的条数写在副注里',
    });

    return dims;
  }, [overview, accounts, now]);

  // 综合分 = 有数据维度的均值；无数据维度不参与，免得把「不知道」算成低分。
  // 只有 1 项可评估时不给「综合 xx/100」：那既不是综合分，又会把其余 4 项空缺掩盖掉。
  const scored = radarDims.filter((dim) => dim.percent !== null);
  const radarScore = scored.length > 0 ? scored.reduce((sum, dim) => sum + (dim.percent ?? 0), 0) / scored.length : null;
  const radarPoints = radarDims
    .map((dim, index) => (dim.percent === null ? null : radarPoint(index, clampPercent(dim.percent) / 100)))
    .filter((point): point is [number, number] => point !== null);
  const radarShape = radarPoints.length >= 3 ? radarPoints.map((point) => point.join(',')).join(' ') : '';

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
  const latestPairs = latest ? detailPairs(latest.detail, 5).map(([key, value]) => `${key}=${value}`).join(' · ') : '';

  return (
    <div className="mon-overview">
      {error && <div className="mon-overview-error" role="alert">{error}</div>}

      {/* 四张量卡：标签 + 图标 / 超大数字 + 小单位 + 胶囊 / 一行副注。
          契约 §5：「没有数据」不许渲染成 0% / 0 次 / ¥0——请求三项在窗口内零请求时
          统一显示「—」并写明原因，否则会被读成链路全挂。 */}
      <div className="grid-4 mon-meta-row">
        <MetaCard
          label={'集群拓扑健康度'}
          icon={<Glyph d={GLYPH.server} />}
          tone={unavailable > 0 ? 'amber' : 'green'}
          value={overview && total > 0 ? `${usable}/${total}` : '—'}
          chip={overview && total > 0 ? `${pct((usable ?? 0) / total * 100)} 连通` : undefined}
          chipTone={unavailable > 0 ? 'amber' : 'green'}
          note={overview ? (total > 0 ? `异常 ${unavailable} · 停用 ${disabled}` : '还没有接入账号') : '等待采集'}
          noteRight={accounts.length > 0 ? `${islands} 孤岛节点` : undefined}
          noteRightTone={islands > 0 ? 'amber' : 'green'}
          noteRightTitle={'孤岛 = 该厂商只有一个账号，没有同厂商备用节点可切换'}
        />
        <MetaCard
          label={'全局 SLA 实时可用率'}
          icon={<Glyph d={GLYPH.success} />}
          tone={'accent'}
          value={hasTraffic ? formatRatio(req.successRate) : '—'}
          chip={hasTraffic && req.p95LatencyMs > 0 ? `P95 ${formatLatency(req.p95LatencyMs)}` : undefined}
          chipTone={'blue'}
          note={req ? (hasTraffic ? `${windowHours}h 请求 ${req.total.toLocaleString()} 条` : '该窗口内没有请求') : '等待采集'}
          noteRight={hasTraffic ? `成功 ${req.success.toLocaleString()}` : undefined}
          noteRightTone={'green'}
        />
        {/* 第 3 张卡右下角带快捷跳转（参考 §6.5）：直接落到账号页调熔断。 */}
        <MetaCard
          label={'熔断保护与降级防线'}
          icon={<Glyph d={GLYPH.bolt} />}
          tone={breakerTone}
          value={overview && total > 0 ? `${closed} 闭合` : '—'}
          chip={overview && total > 0 && halfOpen > 0 ? `${halfOpen} 半开试探` : undefined}
          chipTone={'blue'}
          note={overview ? (total > 0 ? `阻断熔断 ${isolated}` : '还没有接入账号') : '等待采集'}
          action={onOpenTab ? { label: '调控熔断 →', onClick: () => onOpenTab('accounts') } : undefined}
        />
        <MetaCard
          label={`${windowHours}h 请求与成本`}
          icon={<Glyph d={GLYPH.cost} />}
          tone={'blue'}
          /* 没有价格行时绝不能显示 $0.00：那看起来像"免费模型"，实际是"没计价"。 */
          value={hasTraffic ? compactCount(req.total) : '—'}
          unit={hasTraffic ? '次' : undefined}
          chip={hasTraffic && req.priced > 0 ? formatCostMicros(req.costMicros) : undefined}
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

      {/* 拓扑主视觉（契约 §6.1 第 2 段）：调用端 3 栏 / 核心 5 栏 / 上游集群 4 栏。
          占比与状态全部由账号 + 轻探测 + 24h 请求汇总换算，口径写在卡内脚注。 */}
      <TopologyCard
        accounts={accounts}
        overview={overview}
        probeRuns={probeRuns}
        timezone={timezone}
        loading={loading}
        iconOf={iconOf}
        onOpenAccount={onOpenAccount}
        onOpenAllAccounts={() => (onOpenTab ? onOpenTab('accounts') : onOpenAccount(accounts[0]?.providerId ?? ''))}
      />

      {/* 雷达（五维评分）+ 延迟分布。两卡用 subgrid 共享轨道：卡片高度跟随内容多的那张，
          不固定撑满；某张卡内容少时，它的脚注贴底，中间留白而不是把行拉长。 */}
      <div className="mon-duo">
        <div className="card mon-duo-card">
          <div className="mon-card-head">
            <span className="mon-card-title">{'集群自愈成熟度雷达'}</span>
            {radarScore !== null && scored.length >= 2 && <span className="mon-score">{`综合 ${radarScore.toFixed(1)}/100`}</span>}
            {radarScore !== null && scored.length === 1 && <span className="mon-score is-muted">{'仅 1 项可评估'}</span>}
          </div>
          {radarDims.length === 0 && (
            <div className="mon-dims-empty">
              <span className="mon-dims-empty-title">{'暂无可评估的维度'}</span>
              <span>{'接口没有可换算的数据时这里整段留空，不用 0% 的进度条充数。'}</span>
            </div>
          )}
          {radarDims.length > 0 && (
            <div className="mon-radar-body">
              {/* 五边形雷达：只连有数据的维度；无数据维度不画点也不补 0。 */}
              <div className="mon-radar" aria-hidden>
                <svg viewBox="0 0 200 200" className="mon-radar-svg">
                  {RADAR_GRID.map((points, index) => (
                    <polygon key={index} points={points} className="mon-radar-grid" />
                  ))}
                  {RADAR_AXES.map(([x, y], index) => (
                    <line key={index} x1={RADAR_CENTER} y1={RADAR_CENTER} x2={x} y2={y} className="mon-radar-axis" />
                  ))}
                  {radarShape && <polygon points={radarShape} className="mon-radar-shape" />}
                  {radarDims.map((dim, index) => {
                    if (dim.percent === null) {
                      return (
                        <circle key={dim.key} cx={RADAR_CENTER} cy={RADAR_CENTER} r={2.5} className="mon-radar-dot is-empty">
                          <title>{`${dim.name}：无数据`}</title>
                        </circle>
                      );
                    }
                    const [x, y] = radarPoint(index, clampPercent(dim.percent) / 100);
                    return (
                      <circle key={dim.key} cx={x} cy={y} r={3.2} className={`mon-radar-dot ${dim.tone}`}>
                        <title>{`${dim.name} ${pct(dim.percent)}`}</title>
                      </circle>
                    );
                  })}
                </svg>
              </div>
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
            </div>
          )}
          <div className="mon-card-foot">
            <span>{'综合分 = 有数据维度的均值；无数据维度不参与'}</span>
            {radarDims.some((dim) => dim.percent === null) && <span>{'空心点 = 无数据'}</span>}
          </div>
        </div>

        <div className="card mon-duo-card">
          <div className="mon-card-head">
            <span className="mon-card-title">{'请求耗时分布与抖动频谱'}</span>
            <span className="mon-card-sub">
              {req && req.total > 0
                ? `${windowHours}h · ${req.total.toLocaleString()} 次`
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
            <div className="mon-latency-body">
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
            </div>
          )}
          <div className="mon-card-foot">
            <span>{'接口只给 P95，没有逐请求直方图；各档不报次数与占比'}</span>
            {onOpenTab && <button type="button" className="mon-meta-link" onClick={() => onOpenTab('probes')}>{'查看实时事件流 →'}</button>}
          </div>
        </div>
      </div>

      {/* 实时状态条：最近一条事件的时间 + 等宽 payload，右侧进入实时流（参考版式的底部遥测条）。 */}
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
        <button className="btn btn-sm mon-statusbar-btn" onClick={() => (onOpenTab ? onOpenTab('probes') : undefined)}>
          {'进入实时事件流'}
        </button>
      </div>
    </div>
  );
}

// ── 雷达 / 频谱的取值纪律 ──────────────────────────────────────────────
/**
 * 概览接口只给「成功率、P95、成本、账号计数、告警分布」，因此：
 *
 * - 能算的维度一律列出；算不出来的维度整行标「无数据」并写明缺哪个字段，不补 0、不画 0% 条；
 * - 雷达只连有数据的维度：不够三个点时整张图不画多边形，避免把「不知道」读成「接近 0」；
 * - 百分比一律按界面上写明的公式算（悬停可查），不是拍的满意度；
 * - 基线（参考图里「24ms / 基线 50ms」）接口没有，就只写目标，不造基线。
 *
 * 「响应延迟」用 P95 与首包延迟回退规则的默认阈值比（= 目标上限），
 * 「凭证健康」用未过期凭证占比，其余公式见 radarDims。
 */
const TTFT_BUDGET_MS = 3000;

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

/** 条带每段的颜色：P95 落点用该档色，其余用中性灰（只表示区间，不表示数量）。 */
function inBandTone(index: number, hit: number): string {
  return index === hit ? `hit ${LATENCY_BANDS[index].tone}` : 'idle';
}
