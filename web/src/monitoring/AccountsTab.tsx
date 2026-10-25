import {useEffect, useMemo, useState} from 'react';
import {BrandIcon, Modal} from '../components/ui';
import {fetchProbes} from './api';
import type {
  MonitorAccount,
  MonitorProbeAccount,
  MonitorProbeConfig,
  MonitorProbeRun,
  MonitorQuota,
} from './types';
import {hasTime, quotaRatioKnown} from './types';
import {displayTime, formatLatency, quotaText, relativeToNow} from './format';

export interface AccountsTabProps {
  accounts: MonitorAccount[];
  timezone: string;
  /**
   * 品牌图标键。控制台已有 `providerIcon(provider)`（它读完整 Provider 对象），
   * 监控中心只拿到精简视图，所以由外层把真实图标键传进来；不传就退回名字
   * 首字母，绝不自己造图标键（猜错的键会让 mask 图 404 成空洞）。
   */
  iconOf?: (providerId: string) => string | undefined;
  /** 当前 firing 告警数，用来标状态点与「查看告警」。 */
  alertCountOf: (providerId: string) => number;
  busy: string;
  error: string;
  /** 卡片内动作（续期/刷额度）的结果文案，直接显示在动作行下面（抽屉打开时同样可见）。 */
  note: string;
  actionError: string;
  onRenew: (providerId: string) => void;
  onRefreshQuota: (providerId: string) => void;
  onOpenAlerts: (providerId: string) => void;
  onOpenLogs: (providerId: string) => void;
  /** 抽屉里的「立即探测」busy 键（`probe:<providerId>`）。 */
  probeBusy: string;
  /** 「立即探测」的入口（卡片与抽屉共用），返回 null 表示请求本身没发出去。 */
  onRunProbe: (providerId: string) => Promise<unknown> | void;
  /** `/__monitor/probes` 归一后的每账号点阵与指标；不传时点阵退回「无记录」。 */
  probeView?: AccountsProbeView;
  /** 探测全局配置：今日探测下限条在 maxPerDay=0（不限）时借它说明上限来源。 */
  probeConfig?: MonitorProbeConfig | null;
  /** 抽屉受控：概览「详情」与深链 ?account= 都要能直接打开某个账号。 */
  selectedId: string;
  onSelect: (providerId: string) => void;
  onCloseDrawer: () => void;
}

/**
 * 卡片视图的两个输入：`/__monitor/accounts` 的快照与 `/__monitor/probes` 的每账号探测现状，
 * 由 MonitoringPage 把接口原样传进来（页面已按 providerId 归一）。账号页自己不取数，
 * 免得与页面轮询各读一次、两边时点不一致。
 */
export interface AccountsProbeFacts {
  probed: boolean;
  consecutiveFailures: number;
  lastProbeAt?: string;
  /** 最近一发的状态码。 */
  lastStatus?: number;
  /** 点阵：最近 N 发（新→旧），每一发按后端 MonitorProbeRun.ok 落绿/红。 */
  recent: boolean[];
  /** 当日次数 / 每日上限；maxPerDay=0 是「不限」。 */
  usedToday: number;
  maxPerDay: number;
  /** 探测耗时：最近一发未被跳过的探测（与最近一发同为 ok/失败）的 durationMs；没有有效记录时 undefined。 */
  rttMs?: number;
}

export interface AccountsProbeView {
  /** 卡片点阵保留的探测发数。 */
  windowSize: number;
  factsOf: (providerId: string) => AccountsProbeFacts;
}

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

const CAPABILITY_LABELS: Array<{ key: keyof MonitorAccount['capabilities']; label: string }> = [
  { key: 'oauth', label: '凭证续期' },
  { key: 'quota', label: '额度' },
  { key: 'models', label: '模型列表' },
  { key: 'checkin', label: '每日签到' },
];

type AccountState = 'ok' | 'warn' | 'bad' | 'off';

const STATE_TEXT: Record<AccountState, string> = {
  ok: '正常',
  warn: '告警',
  bad: '异常',
  off: '停用',
};

type StateFilter = 'all' | 'problem' | AccountState;
type ViewMode = 'cards' | 'heat';

/**
 * 卡片与筛选共用的账号状态。判定顺序必须与概览页一致（概览说异常、这里说正常就是自相矛盾）：
 * 停用 → 有 firing 告警 → 请求判定异常 → 否则正常。
 */
function stateOf(account: MonitorAccount, alerts: number): AccountState {
  if (account.disabled) return 'off';
  if (alerts > 0) return 'warn';
  if (account.healthStatus === 'unavailable') return 'bad';
  return 'ok';
}

const STATE_RANK: Record<AccountState, number> = { bad: 0, warn: 1, off: 2, ok: 3 };

/**
 * 容灾角色。本项目没有「主路由 / 热备 / 兜底」的持久化字段，也没有分流权重，
 * 所以角色只按能证实的事实推导，并在卡片副注里写明依据（不让推导看起来像配置）：
 * 停用=退出调度；健康且无告警=主路由（当前会被调度）；有告警或判定异常=降级兜底
 * （只作最后手段）；没被判定过健康=待确认（还没被调度验证）。
 */
type ResilienceRole = 'standby' | 'fallback' | 'hold';

const ROLE_TEXT: Record<ResilienceRole, string> = {
  standby: '主路由',
  fallback: '降级兜底',
  hold: '待确认',
};

const ROLE_HINT: Record<ResilienceRole, string> = {
  standby: '健康且无未恢复告警，当前会被调度',
  fallback: '有未恢复告警或请求判定异常，降级使用',
  hold: '还没有被调度验证过健康状态',
};

function roleOf(account: MonitorAccount, alerts: number, state: AccountState, breaker: Breaker): ResilienceRole {
  // 探测已熔断的账号不会被调度去兜底，它是「待确认」，不是「兜底」。
  if (breaker === 'OPEN') return 'hold';
  if (account.healthStatus === 'healthy' && alerts === 0 && state === 'ok') return 'standby';
  if (state === 'warn' || state === 'bad') return 'fallback';
  return 'hold';
}

/**
 * 熔断档位。本项目没有用户可干预的熔断器，只有探测侧的连续失败 streak：
 * 连续失败 ≥ BREAKER_TRIP_STREAK 视为熔断阻断（OPEN），失败但未到阈值是半开试探（HALF_OPEN），
 * 其余闭合（CLOSED）。停用只算 OFF（不参与调度与探测），但停用前已经连续失败至少 BREAKER_WARN_STREAK
 * 次的账号仍标 OPEN：档位是「真实门槛」的事实，不因为停用就伪装成闭合。
 */
const BREAKER_TRIP_STREAK = 5;
/** 卡片右上角徽标的「异常」门槛：连续失败到 2 次就算，比 5 次熔断线更早提醒。 */
const BREAKER_WARN_STREAK = 2;
type Breaker = 'CLOSED' | 'HALF_OPEN' | 'OPEN' | 'OFF';

function breakerOf(account: MonitorAccount, facts: AccountsProbeFacts | undefined): Breaker {
  const streak = facts?.probed ? facts.consecutiveFailures : 0;
  if (streak >= BREAKER_TRIP_STREAK) return 'OPEN';
  if (account.disabled) return 'OFF';
  if (streak > 0) return 'HALF_OPEN';
  return 'CLOSED';
}

const BREAKER_TEXT: Record<Breaker, string> = {
  CLOSED: 'CLOSED',
  HALF_OPEN: 'HALF_OPEN',
  OPEN: 'OPEN',
  OFF: '停用',
};

const BREAKER_HINT: Record<Breaker, string> = {
  CLOSED: `CLOSED：探测闭合——最近一发轻探测通过或还没有记录（本项目不会主动隔离账号）`,
  HALF_OPEN: `HALF_OPEN：连续失败 1~${BREAKER_TRIP_STREAK - 1} 次，未到 ${BREAKER_TRIP_STREAK} 次熔断线`,
  OPEN: `OPEN：连续失败已达 ${BREAKER_TRIP_STREAK} 次（熔断线），本卡判为异常`,
  OFF: '停用：不参与自动调度，也不会发探测',
};

/**
 * 右上角徽标的文案。与 breakerOf 一一对应：
 * OFF→停用，OPEN→异常，HALF_OPEN 且连续失败 ≥2→异常，否则看活跃告警→告警，其余→正常。
 */
type HealthCode = 'normal' | 'alert' | 'abnormal' | 'off';

const HEALTH_TEXT: Record<HealthCode, string> = {
  normal: '正常',
  alert: '告警',
  abnormal: '异常',
  off: '停用',
};

function healthOf(account: MonitorAccount, alerts: number, breaker: Breaker, facts: AccountsProbeFacts | undefined): HealthCode {
  if (breaker === 'OFF') return 'off';
  if (breaker === 'OPEN') return 'abnormal';
  if (facts?.probed && facts.consecutiveFailures >= BREAKER_WARN_STREAK) return 'abnormal';
  if (alerts > 0) return 'alert';
  return 'normal';
}

/** 徽标的判定依据：悬停即可看到每一条门槛的当前值，不让人猜「异常」从哪来。 */
function healthHint(account: MonitorAccount, alerts: number, breaker: Breaker, facts: AccountsProbeFacts | undefined): string {
  const lines =
    breaker === 'OFF'
      ? ['账号已停用 → 停用（不参与调度与探测）']
      : breaker === 'OPEN'
        ? [`探测连续失败 ${facts?.consecutiveFailures ?? 0} 次 ≥ ${BREAKER_TRIP_STREAK} 次熔断线 → 异常`]
        : [
            `未恢复告警 ${alerts} 条${alerts > 0 ? ' → 告警' : ''}`,
            facts?.probed
              ? `探测连续失败 ${facts.consecutiveFailures} 次${
                  facts.consecutiveFailures >= BREAKER_WARN_STREAK ? ` ≥ ${BREAKER_WARN_STREAK} 次 → 异常` : ''
                }`
              : '还没有可用探测记录（不计入连续失败）',
            `请求健康判定：${HEALTH_STATUS_TEXT[account.healthStatus] || account.healthStatus || '未检测'}`,
          ];
  return `${lines.join('\n')}\n判定顺序：停用 → 连续失败 ≥${BREAKER_TRIP_STREAK} 次 → 连续失败 ≥${BREAKER_WARN_STREAK} 次 → 有未恢复告警`;
}

const HEALTH_STATUS_TEXT: Record<string, string> = {
  healthy: '健康',
  unavailable: '不可用',
  unknown: '未检测',
};

type BreakerFilter = 'all' | 'CLOSED' | 'HALF_OPEN' | 'OPEN';

function Onboarding() {
  return (
    <div className="mon-acc-onboard">
      <span className="mon-acc-onboard-icon" aria-hidden="true">
        <SearchIcon />
      </span>
      <div className="mon-acc-onboard-body">
        <p className="mon-acc-onboard-title">{'还没有可监控的账号'}</p>
        <p className="mon-acc-onboard-text">{'接入 Provider 后这里会出现账号卡片：探测点阵、连续失败与额度采样都来自同一个监控采集器。'}</p>
      </div>
      <span className="mon-acc-onboard-badge">{'账号 0'}</span>
    </div>
  );
}

/**
 * 账号名按空白插零宽断点：长名在词与词之间折行，而不是把整段挤成省略号。
 * 中文名没有空格，靠 CSS 的 overflow-wrap: anywhere 在任意字间断。
 */
function NameText({ name }: { name: string }) {
  return (
    <>
      {name.split(/(\s+)/).map((part, index) =>
        /^\s+$/.test(part) ? (
          <span key={index}>
            {part}
            <wbr />
          </span>
        ) : (
          part
        ),
      )}
    </>
  );
}

/** 续期文案：没过期时间就写「不过期」，不把 Go 零值时间当成公元 1 年。 */
function expiryText(account: MonitorAccount, now: number): string {
  if (!hasTime(account.expiresAt)) return '不过期';
  return `续期 ${relativeToNow(account.expiresAt, now)}`;
}

/** 当天已用次数 / 上限；上限为 0 时后端意为「不限」，此时不能写成「x / 0」。 */
function usedTodayText(facts: AccountsProbeFacts): string {
  return facts.maxPerDay === 0 ? `${facts.usedToday} / 不限` : `${facts.usedToday} / ${facts.maxPerDay}`;
}

/** 今日探测进度 0..1；不限次数时返回 null（进度条没有分母，不画假进度）。 */
function todayProgress(facts: AccountsProbeFacts): number | null {
  if (facts.maxPerDay <= 0) return null;
  return Math.min(1, Math.max(0, facts.usedToday / facts.maxPerDay));
}


/** 最近一发的结论。没有状态码就是网络层失败，0 与 200 不是一回事，要分开写。 */
function lastProbeText(facts: AccountsProbeFacts): string {
  if (!facts.probed) return '未探测';
  if (facts.lastStatus) return `HTTP ${facts.lastStatus}`;
  return '网络失败';
}

/** 点阵格子：新→旧排，右侧补灰色占位到 windowSize，点数恒定，卡与卡之间长度一致可比。 */
function dotCells(facts: AccountsProbeFacts, windowSize: number): Array<{ tone: 'ok' | 'err' | 'none'; title: string }> {
  const cells: Array<{ tone: 'ok' | 'err' | 'none'; title: string }> = facts.recent
    .slice(0, windowSize)
    .map((ok, index) => ({
      tone: ok ? 'ok' : 'err',
      title: ok ? `第 ${index + 1} 近发：通过` : `第 ${index + 1} 近发：失败`,
    }));
  while (cells.length < windowSize) {
    cells.push({ tone: 'none', title: '没有更多探测记录' });
  }
  return cells;
}

function successText(facts: AccountsProbeFacts): string {
  const measured = facts.recent.length;
  if (measured === 0) return '暂无记录';
  const okCount = facts.recent.filter(Boolean).length;
  return `${Math.round((okCount / measured) * 100)}% 成功`;
}

function ProbeStrip({ facts, windowSize }: { facts: AccountsProbeFacts; windowSize: number }) {
  const cells = dotCells(facts, windowSize);
  const measured = facts.recent.length;
  return (
    <>
      <div className="mon-acc-probe-head">
        <span className="mon-acc-probe-label">{`最近 ${windowSize} 次轻探测连通`}</span>
        <span className="mon-acc-probe-note">{successText(facts)}</span>
      </div>
      <div className="mon-acc-probe-dots" title={measured > 0 ? `最近 ${measured} 发探测，右为最近` : '还没有探测记录'}>
        {cells.map((cell, index) => (
          <span key={index} className={`mon-acc-dot ${cell.tone}`} title={cell.title} />
        ))}
      </div>
    </>
  );
}

/**
 * 热力图的数据：卡片 / 点阵共用。MonitoringPage 只随页面轮询刷新一次；
 * 这里点一次「轻探测」后要立刻看到这一发的颜色，所以自己再读一次单账号的 probes，
 * 探测进行中时随探测结束重读一次（probeBusy 变化触发）。
 */
function useAccountProbeDetail(providerId: string, probeBusy: string, windowSize: number) {
  const [detail, setDetail] = useState<{ facts: AccountsProbeFacts; runs: MonitorProbeRun[] } | null>(null);
  const reloading = !!providerId && probeBusy === `probe:${providerId}`;
  useEffect(() => {
    if (!providerId) {
      setDetail(null);
      return;
    }
    let live = true;
    fetchProbes(providerId)
      .then((page) => {
        if (!live) return;
        const account: MonitorProbeAccount | undefined = page.accounts.find((item) => item.providerId === providerId);
        const runs = (page.runs ?? [])
          .filter((run) => run.providerId === providerId && !run.skipped)
          .sort((a, b) => Date.parse(b.time) - Date.parse(a.time));
        setDetail({
          runs: runs.slice(0, 30),
          facts: {
            probed: !!account?.probed,
            consecutiveFailures: account?.consecutiveFailures ?? 0,
            lastProbeAt: account?.lastProbeAt,
            lastStatus: account?.lastStatus,
            recent: runs.slice(0, windowSize).map((run) => run.ok),
            usedToday: account?.usedToday ?? 0,
            maxPerDay: account?.maxPerDay ?? 0,
            rttMs: runs[0] && runs[0].durationMs > 0 ? runs[0].durationMs : undefined,
          },
        });
      })
      .catch(() => {
        // 抽屉里已有页面级的轮询数据兜底；单读失败不覆盖、不弹错。
      });
    return () => {
      live = false;
    };
  }, [providerId, reloading, windowSize]);
  return detail;
}

/** 抖动：最近几发耗时的极差。只有一发时无从谈抖动，返回 undefined 而不是 0。 */
function jitterOf(runs: MonitorProbeRun[]): number | undefined {
  const durations = runs.filter((run) => !run.skipped && run.durationMs > 0).slice(0, 5).map((run) => run.durationMs);
  if (durations.length < 2) return undefined;
  return Math.max(...durations) - Math.min(...durations);
}

/**
 * 点阵热力图：行 = 账号，列 = 最近 20 发探测。每格按该发的耗时着色
 * （快/中/慢/失败四档），所以看的是真实分布，不是装饰。
 */
function HeatView({
  rows,
  onProbe,
  probeBusy,
}: {
  rows: Array<{ account: MonitorAccount; runs: MonitorProbeRun[]; facts?: AccountsProbeFacts }>;
  onProbe: (providerId: string) => void;
  probeBusy: string;
}) {
  const CELLS = 20;
  // 固定档位（秒级探测 1~3s 都正常，按 ms 分档会把整屏涂成一片）：<1s 快 / 1~2.5s 正常 / >2.5s 慢。
  const FAST_MS = 1000;
  const SLOW_MS = 2500;
  return (
    <div className="card panel mon-acc-heat">
      <div className="mon-acc-heat-head">
        <div className="mon-acc-heat-title">
          <h2 className="panel-title">{'全账号 SLA 点阵热力图'}</h2>
          <p className="panel-desc">{`${rows.length} 个账号 · 每行最近 ${CELLS} 发轻探测，按耗时着色；空位是窗口里没有记录，不是失败。`}</p>
        </div>
        <div className="mon-acc-heat-legend">
          <span className="mon-acc-heat-key l1">{`< ${formatLatency(FAST_MS)}`}</span>
          <span className="mon-acc-heat-key l2">{`${formatLatency(FAST_MS)}–${formatLatency(SLOW_MS)}`}</span>
          <span className="mon-acc-heat-key l3">{`> ${formatLatency(SLOW_MS)}`}</span>
          <span className="mon-acc-heat-key l4">{'失败'}</span>
          <span className="mon-acc-heat-key l0">{'无数据'}</span>
        </div>
      </div>
      <div className="mon-acc-heat-rows">
        {rows.map(({ account, runs, facts }) => {
          const cellsFrom = runs.slice(0, CELLS).map((run) => ({
            tone: !run.ok ? 'l4' : run.durationMs < FAST_MS ? 'l1' : run.durationMs < SLOW_MS ? 'l2' : 'l3',
            title: `${run.case || '探测'} · ${run.ok ? '通过' : '失败'} · ${formatLatency(run.durationMs)} · ${displayTime(run.time, Intl.DateTimeFormat().resolvedOptions().timeZone).text}`,
          }));
          while (cellsFrom.length < CELLS) cellsFrom.push({ tone: 'l0', title: '窗口内没有记录' });
          const probing = probeBusy === `probe:${account.providerId}`;
          return (
            <div key={account.providerId} className="mon-acc-heat-row">
              <div className="mon-acc-heat-who">
                <button type="button" className="mon-acc-heat-name" onClick={() => onProbe(account.providerId)} title="点名字发一发轻探测">
                  <NameText name={account.name} />
                </button>
                <span className="mon-acc-heat-meta">
                  {`${account.vendor || AUTH_LABELS[account.authType] || 'API Key'} · ${facts?.probed ? `连败 ${facts.consecutiveFailures}` : '未探测'}`}
                </span>
              </div>
              <div className="mon-acc-heat-cells">
                {cellsFrom.map((cell, index) => (
                  <span key={index} className={`mon-acc-heat-cell ${cell.tone}`} title={cell.title} />
                ))}
              </div>
              <button
                type="button"
                className="btn btn-sm mon-acc-heat-probe"
                disabled={probing || account.disabled}
                onClick={() => onProbe(account.providerId)}
                title={account.disabled ? '账号已停用，探测跳过' : '现在就发一发最小对话探测'}
              >
                {probing ? '探测中…' : '轻探测'}
              </button>
            </div>
          );
        })}
      </div>
    </div>
  );
}

export function AccountsTab(props: AccountsTabProps) {
  const {
    accounts, timezone, iconOf, alertCountOf, busy, error, note, actionError,
    onRenew, onRefreshQuota, onOpenAlerts, onOpenLogs, probeBusy, onRunProbe, probeView, probeConfig,
    selectedId, onSelect, onCloseDrawer,
  } = props;
  const [search, setSearch] = useState('');
  const [vendor, setVendor] = useState('all');
  const [stateFilter, setStateFilter] = useState<StateFilter>('all');
  const [breakerFilter, setBreakerFilter] = useState<BreakerFilter>('all');
  // 只保留本项目有真实能力的筛选（状态 / 熔断档位）；参考里的「容灾层级」是 mock 字段，
  // 这里换成按凭证类型筛，过滤的仍是后端真给的 authType。
  const [authFilter, setAuthFilter] = useState('all');
  const [viewMode, setViewMode] = useState<ViewMode>('cards');
  const now = Date.now();

  const vendors = useMemo(() => {
    const seen = new Map<string, number>();
    for (const account of accounts) {
      const label = account.vendor || '未知厂商';
      seen.set(label, (seen.get(label) ?? 0) + 1);
    }
    return [...seen.entries()].sort((a, b) => a[0].localeCompare(b[0], 'zh-CN'));
  }, [accounts]);

  const authTypes = useMemo(() => {
    const seen = new Set<string>();
    for (const account of accounts) seen.add(AUTH_LABELS[account.authType] || account.authType || 'API Key');
    return [...seen].sort((a, b) => a.localeCompare(b, 'zh-CN'));
  }, [accounts]);

  const filtered = useMemo(() => {
    const term = search.trim().toLowerCase();
    return accounts
      .filter((account) => (vendor === 'all' ? true : (account.vendor || '未知厂商') === vendor))
      .filter((account) => {
        const state = stateOf(account, alertCountOf(account.providerId));
        if (stateFilter === 'problem') return state === 'bad' || state === 'warn';
        if (stateFilter !== 'all') return state === stateFilter;
        return true;
      })
      .filter((account) => {
        if (breakerFilter === 'all') return true;
        return breakerOf(account, probeView?.factsOf(account.providerId)) === breakerFilter;
      })
      .filter((account) =>
        authFilter === 'all' ? true : (AUTH_LABELS[account.authType] || account.authType || 'API Key') === authFilter,
      )
      .filter(
        (account) =>
          !term ||
          account.name.toLowerCase().includes(term) ||
          account.providerId.toLowerCase().includes(term) ||
          account.vendor.toLowerCase().includes(term),
      )
      .sort((a, b) => {
        const delta = STATE_RANK[stateOf(a, alertCountOf(a.providerId))] - STATE_RANK[stateOf(b, alertCountOf(b.providerId))];
        if (delta !== 0) return delta;
        return a.name.localeCompare(b.name, 'zh-CN');
      });
  }, [accounts, search, vendor, stateFilter, breakerFilter, authFilter, alertCountOf, probeView]);

  const detail = accounts.find((account) => account.providerId === selectedId) ?? null;
  const detailFacts = detail && probeView ? probeView.factsOf(detail.providerId) : undefined;
  const probing = !!detail && probeBusy === `probe:${detail.providerId}`;
  const windowSize = probeView?.windowSize ?? 10;
  const detailProbe = useAccountProbeDetail(detail?.providerId ?? '', probeBusy, windowSize);
  const detailWindow = detailProbe ?? (detailFacts ? { facts: detailFacts, runs: [] } : null);
  // 能用的功能才出现：停用账号自动调度会跳过，手动探测也只会写一条 skipped，没有排障
  // 价值，所以直接不给入口。其余账号都给——「没配模型」由后端在按下时以 skipped 结果
  // 回给抽屉，界面如实展示那一句，不在前端猜（前端拿不到 model 字段）。
  const canProbe = !!detail && !detail.disabled;
  const detailProbing = detail ? probeBusy === `probe:${detail.providerId}` : false;

  // 热力图行：每行需要该账号最近 20 发，页面级 probeView 只留了 10 发且没有耗时，
  // 所以切到热力图时单独读一次全量 probes（与卡片同一接口，口径一致）。
  const [heatSource, setHeatSource] = useState<MonitorProbePageLite | null>(null);
  useEffect(() => {
    if (viewMode !== 'heat') return;
    let live = true;
    fetchProbes()
      .then((page) => {
        if (live) setHeatSource(page);
      })
      .catch(() => {
        if (live) setHeatSource({ accounts: [], runs: [] });
      });
    return () => {
      live = false;
    };
  }, [viewMode, probeBusy, accounts.length]);
  const heatRows = useMemo(() => {
    if (!heatSource) return [];
    const byAccount = new Map<string, MonitorProbeRun[]>();
    const runs = [...heatSource.runs].filter((run) => !run.skipped).sort((a, b) => Date.parse(b.time) - Date.parse(a.time));
    for (const run of runs) {
      const list = byAccount.get(run.providerId) ?? [];
      list.push(run);
      byAccount.set(run.providerId, list);
    }
    return filtered.map((account) => ({
      account,
      runs: byAccount.get(account.providerId) ?? [],
      facts: probeView?.factsOf(account.providerId),
    }));
  }, [heatSource, filtered, probeView]);

  const empty = accounts.length === 0;

  return (
    <div className="mon-accounts">
      {error && <div className="mon-acc-error" role="alert">{error}</div>}

      <div className="mon-acc-toolbar" role="group" aria-label="账号筛选">
        <div className="mon-acc-toolbar-main">
          <label className="mon-acc-search">
            <SearchIcon />
            <input
              className="monitoring-input"
              placeholder="按账号名、Provider、厂商搜索…"
              value={search}
              onChange={(event) => setSearch(event.target.value)}
              aria-label="搜索账号"
            />
          </label>

          <div className="mon-acc-vendors" role="group" aria-label="厂商">
            <button
              type="button"
              className={`mon-acc-vendor${vendor === 'all' ? ' on' : ''}`}
              aria-pressed={vendor === 'all'}
              onClick={() => setVendor('all')}
            >
              {'全部'}
            </button>
            {vendors.map(([label]) => (
              <button
                key={label}
                type="button"
                className={`mon-acc-vendor${vendor === label ? ' on' : ''}`}
                aria-pressed={vendor === label}
                onClick={() => setVendor(label)}
              >
                {label}
              </button>
            ))}
          </div>
        </div>

        <div className="mon-acc-toolbar-side">
          <label className="mon-acc-select">
            <span className="mon-acc-select-label">{'状态'}</span>
            <select
              className="monitoring-input mon-acc-select-input"
              aria-label="状态"
              value={stateFilter}
              onChange={(event) => setStateFilter(event.target.value as StateFilter)}
            >
              <option value="all">{'全部状态'}</option>
              <option value="problem">{'只看有问题'}</option>
              <option value="warn">{'告警'}</option>
              <option value="bad">{'异常'}</option>
              <option value="off">{'停用'}</option>
            </select>
          </label>
          <label className="mon-acc-select">
            <span className="mon-acc-select-label" title={`按探测连续失败推导：≥${BREAKER_TRIP_STREAK} 次为熔断`}>{'熔断'}</span>
            <select
              className="monitoring-input mon-acc-select-input"
              aria-label="熔断状态"
              value={breakerFilter}
              onChange={(event) => setBreakerFilter(event.target.value as BreakerFilter)}
            >
              <option value="all">{'全部熔断状态'}</option>
              <option value="CLOSED">{'CLOSED 闭合'}</option>
              <option value="HALF_OPEN">{'HALF_OPEN 半开'}</option>
              <option value="OPEN">{'OPEN 熔断'}</option>
            </select>
          </label>
          <label className="mon-acc-select">
            <span className="mon-acc-select-label">{'凭证'}</span>
            <select
              className="monitoring-input mon-acc-select-input"
              aria-label="凭证类型"
              value={authFilter}
              onChange={(event) => setAuthFilter(event.target.value)}
            >
              <option value="all">{'全部凭证'}</option>
              {authTypes.map((label) => (
                <option key={label} value={label}>
                  {label}
                </option>
              ))}
            </select>
          </label>

          <div className="mon-acc-view" role="group" aria-label="视图">
            <button
              type="button"
              className={`mon-acc-view-btn${viewMode === 'cards' ? ' on' : ''}`}
              aria-pressed={viewMode === 'cards'}
              onClick={() => setViewMode('cards')}
            >
              {'集群卡片'}
            </button>
            <button
              type="button"
              className={`mon-acc-view-btn${viewMode === 'heat' ? ' on' : ''}`}
              aria-pressed={viewMode === 'heat'}
              onClick={() => setViewMode('heat')}
            >
              {'SLA 点阵热力图'}
            </button>
          </div>
        </div>
      </div>

      <div className="mon-acc-meta-row">
        <span className="mon-acc-count">{`${filtered.length} / ${accounts.length} 个账号`}</span>
        <span className="mon-acc-meta-note" title="角色按健康与告警推导；熔断档位按探测连续失败推导；本项目没有手工隔离/复位接口">
          {'角色、熔断档位按健康与探测结果推导，不是手工配置'}
        </span>
      </div>

      {empty && <Onboarding />}
      {!empty && filtered.length === 0 && <div className="m-empty">{'没有符合当前筛选的账号。'}</div>}

      {viewMode === 'heat' && !empty && filtered.length > 0 && (
        <HeatView
          rows={heatRows}
          probeBusy={probeBusy}
          onProbe={(providerId) => void onRunProbe(providerId)}
        />
      )}

      <div className="mon-acc-grid">
        {viewMode === 'cards' && filtered.map((account) => {
          const alerts = alertCountOf(account.providerId);
          const state = stateOf(account, alerts);
          const facts = probeView?.factsOf(account.providerId);
          const quota = quotaText(account.quota);
          const quotaTone = account.quota?.error
            ? 'err'
            : account.quota && seriesLow(account.quota.ratio)
              ? 'warn'
              : '';
          const breaker = breakerOf(account, facts);
          const health = healthOf(account, alerts, breaker, facts);
          const role = roleOf(account, alerts, state, breaker);
          const cardProbing = probeBusy === `probe:${account.providerId}`;
          const renewing = busy === `renew:${account.providerId}`;
          const fresh = selectedId === account.providerId;
          const progress = facts ? todayProgress(facts) : null;
          return (
            <article
              key={account.providerId}
              className={`mon-acc-card state-${state}${fresh ? ' is-open' : ''}`}
              aria-label={`账号 ${account.name}`}
            >
              <div className="mon-acc-card-top">
                <span className={`mon-acc-role ${role}`} title={ROLE_HINT[role]}>
                  {ROLE_TEXT[role]}
                </span>
                <span className="mon-acc-card-tags">
                  <span className="mon-acc-ptag">{account.vendor || AUTH_LABELS[account.authType] || 'API Key'}</span>
                  <span
                    className={`mon-acc-breaker ${health}`}
                    title={`${HEALTH_TEXT[health]}\n${healthHint(account, alerts, breaker, facts)}\n\n探测档位 ${BREAKER_TEXT[breaker]}：${BREAKER_HINT[breaker]}`}
                  >
                    <span className="mon-acc-breaker-dot" aria-hidden />
                    {HEALTH_TEXT[health]}
                  </span>
                </span>
              </div>

              <h3 className="mon-acc-name">
                <BrandIcon icon={iconOf?.(account.providerId)} name={account.name} size={18} />
                <span className="mon-acc-name-text" title={account.name}>
                  <NameText name={account.name} />
                </span>
              </h3>

              <div className="mon-acc-metrics">
                <div className="mon-acc-metric">
                  <span className="mon-acc-metric-label">{'最近探测成功率'}</span>
                  <span className={`mon-acc-metric-value${state === 'bad' ? ' bad' : ''}`}>
                    {successText(facts ?? { probed: false, consecutiveFailures: 0, recent: [], usedToday: 0, maxPerDay: 0 })}
                  </span>
                </div>
                <div className="mon-acc-metric">
                  <span className="mon-acc-metric-label">{'探测 RTT'}</span>
                  <span className="mon-acc-metric-value">
                    {facts?.rttMs === undefined ? '—' : formatLatency(facts.rttMs)}
                  </span>
                  <span className="mon-acc-metric-note">
                    {facts && facts.rttMs !== undefined && facts.recent.length > 1 ? '端到端往返' : '无抖动样本'}
                  </span>
                </div>
                <div className="mon-acc-metric">
                  <span className="mon-acc-metric-label">{'连续失败'}</span>
                  <span
                    className={`mon-acc-metric-value${
                      facts && facts.probed && facts.consecutiveFailures > 0 ? ' bad' : ''
                    }`}
                    title={facts && facts.probed ? undefined : '还没有可用的探测记录'}
                  >
                    {facts && facts.probed ? `${facts.consecutiveFailures} 次` : '—'}
                  </span>
                  <span className="mon-acc-metric-note">
                    {facts && facts.probed
                      ? facts.consecutiveFailures >= BREAKER_TRIP_STREAK
                        ? `已达 ${BREAKER_TRIP_STREAK} 次熔断线`
                        : facts.consecutiveFailures > 0 ? '未到熔断线' : '最近一发通过'
                      : '无记录'}
                  </span>
                </div>
              </div>

              {facts ? (
                <div className="mon-acc-probe">
                  <ProbeStrip facts={facts} windowSize={windowSize} />
                </div>
              ) : (
                <div className="mon-acc-probe">
                  <div className="mon-acc-probe-head">
                    <span className="mon-acc-probe-label">{`最近 ${windowSize} 次轻探测连通`}</span>
                    <span className="mon-acc-probe-note">{'读取中'}</span>
                  </div>
                </div>
              )}

              {/* 取代参考图的「分流权重」滑条：本项目调度按失败冷却自动选账号，没有可调的权重，
                  所以这里画的是真实的「今日探测 3/4」额度条（只读，分母 = maxPerDayPerAccount）。 */}
              <div
                className="mon-acc-quota"
                title={
                  facts
                    ? facts.maxPerDay === 0
                      ? `不限次数：探测配置的每账号每日上限为 0${probeConfig ? '（设置 → 探测）' : ''}`
                      : `按投递时区计的当日轻探测次数 / 每账号每日上限（${facts.maxPerDay}）`
                    : '探测现状读取中'
                }
              >
                <div className="mon-acc-quota-row">
                  <span className="mon-acc-quota-label">{'今日探测'}</span>
                  <span className="mon-acc-quota-value">
                    {facts
                      ? `${facts.usedToday}${facts.maxPerDay === 0 ? ' 次 · 不限' : ` / ${facts.maxPerDay}`}`
                      : probeConfig
                        ? `— / ${probeConfig.eventDriven.maxPerDayPerAccount || '不限'}`
                        : '—'}
                  </span>
                </div>
                <div
                  className={`mon-acc-track${progress !== null && progress >= 1 ? ' is-full' : ''}`}
                  role="progressbar"
                  aria-label={`${account.name} 今日探测额度`}
                  aria-valuemin={0}
                  aria-valuemax={facts && facts.maxPerDay > 0 ? facts.maxPerDay : undefined}
                  aria-valuenow={facts ? facts.usedToday : undefined}
                  aria-valuetext={facts ? (facts.maxPerDay === 0 ? `已探测 ${facts.usedToday} 次，不限次数` : `已探测 ${facts.usedToday} 次，上限 ${facts.maxPerDay} 次`) : undefined}
                >
                  <span className={progress === null ? 'is-open' : ''} style={{ width: `${(progress ?? 1) * 100}%` }} />
                </div>
                <p className="mon-acc-quota-note">
                  {facts
                    ? facts.maxPerDay === 0
                      ? '每日探测不限次数（只读额度条）'
                      : facts.usedToday > facts.maxPerDay
                        ? '已超过当前上限：上限是刚改的，今日计数不回溯'
                        : progress !== null && progress >= 1 ? '今日探测次数已用完，今天不再发轻探测' : '只读：按探测配置的每日上限'
                    : '探测现状读取中'}
                </p>
              </div>

              <div className="mon-acc-facts">
                <div className="mon-acc-fact" title={`凭证到期：${account.expiresAt ?? '无凭证或不过期'}`}>
                  <span className="mon-acc-fact-key">
                    <ClockIcon />
                    {'续期'}
                  </span>
                  <span className="mon-acc-fact-value">{expiryText(account, now)}</span>
                </div>
                <div className="mon-acc-fact right" title={quota.note}>
                  <span className="mon-acc-fact-key">{'额度'}</span>
                  <span className={`mon-acc-fact-value ${quotaTone}`}>
                    {quota.primary}
                    {account.quota?.stale && <span className="mon-acc-fact-stale">{'数据过期'}</span>}
                  </span>
                </div>
              </div>

              <div className="mon-acc-actions">
                <button
                  type="button"
                  className="btn btn-sm mon-acc-btn mon-acc-btn-ghost"
                  onClick={() => onSelect(account.providerId)}
                  title="打开账号抽屉：凭证、额度、探测与全部操作"
                >
                  {'详情'}
                </button>
                <button
                  type="button"
                  className="btn btn-sm mon-acc-btn mon-acc-btn-renew"
                  disabled={renewing || !account.capabilities.oauth}
                  onClick={() => onRenew(account.providerId)}
                  title={
                    account.capabilities.oauth
                      ? '按厂商自己的窗口续期；未到窗口不会强行轮换'
                      : '该账号没有可续期凭证'
                  }
                >
                  <RenewIcon />
                  {renewing ? '续期中…' : '续期'}
                </button>
                <button
                  type="button"
                  className="btn btn-sm mon-acc-btn"
                  disabled={cardProbing || account.disabled}
                  onClick={() => void onRunProbe(account.providerId)}
                  title={account.disabled ? '账号已停用，探测跳过' : '现在就发一发最小对话探测'}
                >
                  <ZapIcon />
                  {cardProbing ? '探测中…' : '轻探测'}
                </button>
              </div>
              {selectedId === account.providerId && actionError && (
                <div className="mon-acc-inline err">{actionError}</div>
              )}
              {selectedId === account.providerId && !actionError && note && <div className="mon-acc-inline">{note}</div>}
              {alerts > 0 && (
                <button type="button" className="mon-acc-alert-link" onClick={() => onOpenAlerts(account.providerId)}>
                  {`${alerts} 条未恢复告警，查看 →`}
                </button>
              )}
            </article>
          );
        })}
      </div>

      {detail && (
        <Modal
          title={detail.name}
          description={`${AUTH_LABELS[detail.authType] || detail.authType || 'API Key'} · ${detail.vendor || '未知厂商'} · ${detail.providerId}`}
          onClose={onCloseDrawer}
          size="wide"
        >
          <div className="monitoring-cols">
            <div className="card">
              <div className="panel-title">{'凭证'}</div>
              <div className="metric-label">{'到期时间'}</div>
              <div className="metric-value" title={detail.expiresAt ?? ''}>
                {hasTime(detail.expiresAt)
                  ? `${displayTime(detail.expiresAt, timezone).text}（${relativeToNow(detail.expiresAt, Date.now())}）`
                  : '不适用'}
              </div>
              <div className="metric-label" style={{ marginTop: 10 }}>{'最近续期'}</div>
              <div className="metric-note">
                {detail.renewal?.known
                  ? `${detail.renewal.lastOK ? '成功' : '失败'} · ${relativeToNow(detail.renewal.lastAt, Date.now())}${
                      detail.renewal.consecutive > 0 ? ` · 连续失败 ${detail.renewal.consecutive} 次` : ''
                    }`
                  : '还没有续期记录'}
              </div>
            </div>

            <div className="card">
              <div className="panel-title">{'额度 / 积分'}</div>
              <div className="metric-value">{quotaText(detail.quota).primary}</div>
              <div className="metric-note">
                {detail.quota
                  ? `${quotaText(detail.quota).note} · 采样 ${displayTime(detail.quota.observedAt, timezone).text}`
                  : '该厂商没有额度接口'}
              </div>
              {detail.quota?.error && <div className="metric-note err">{`读取失败：${detail.quota.error}`}</div>}
            </div>

            <div className="card">
              <div className="panel-title">{'轻探测'}</div>
              {detailWindow ? (
                <>
                  <div className={`metric-value ${detailWindow.facts.probed && detailWindow.facts.consecutiveFailures > 0 ? 'err' : ''}`}>
                    {detailWindow.facts.probed
                      ? detailWindow.facts.consecutiveFailures > 0
                        ? `连续失败 ${detailWindow.facts.consecutiveFailures} 次（最近一发：${lastProbeText(detailWindow.facts)}）`
                        : `通过（最近一发：${lastProbeText(detailWindow.facts)}）`
                      : '暂无数据'}
                  </div>
                  <div className="metric-note">
                    {hasTime(detailWindow.facts.lastProbeAt)
                      ? `最近一次执行 ${displayTime(detailWindow.facts.lastProbeAt, timezone).text}（${relativeToNow(detailWindow.facts.lastProbeAt, Date.now())}）`
                      : '还没有探测记录'}
                  </div>
                  <div className="metric-note" title="当日次数 / 每日上限，按投递时区计算">
                    {`当日次数 ${usedTodayText(detailWindow.facts)} · 连续失败 ${detailWindow.facts.consecutiveFailures} 次`}
                  </div>
                  <div className="metric-note" title="最近几发探测耗时的极差；不足两发无从谈抖动">
                    {`探测 RTT ${detailWindow.facts.rttMs === undefined ? '—' : formatLatency(detailWindow.facts.rttMs)} · 抖动 ${
                      jitterOf(detailWindow.runs) === undefined ? '—（样本不足）' : `±${formatLatency(jitterOf(detailWindow.runs) as number)}`
                    }`}
                  </div>
                  <div className="mon-acc-drawer-dots">
                    <ProbeStrip facts={detailWindow.facts} windowSize={windowSize} />
                  </div>
                  {detailWindow.facts.probed && detailWindow.facts.consecutiveFailures > 0 && (
                    <div className={`metric-note ${detailWindow.facts.consecutiveFailures >= BREAKER_TRIP_STREAK ? 'err' : ''}`}>
                      {detailWindow.facts.consecutiveFailures >= BREAKER_TRIP_STREAK
                        ? `连续失败已达 ${BREAKER_TRIP_STREAK} 次，卡片判为熔断（OPEN）`
                        : '探测未跑通，已进入 probe_failed 判定'}
                    </div>
                  )}
                  {detail.disabled && <div className="metric-note">{'账号已停用，自动调度跳过它，也不会发探测。'}</div>}
                </>
              ) : (
                <div className="metric-note">{'探测现状读取中'}</div>
              )}
            </div>

            <div className="card">
              <div className="panel-title">{'健康与能力'}</div>
              <div className="metric-note">
                {`状态 ${detail.healthStatus || '未检测'} · 模型列表连续失败 ${detail.modelFailures} 次 · 活跃告警 ${alertCountOf(detail.providerId)}`}
              </div>
              <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginTop: 8 }}>
                {CAPABILITY_LABELS.map((item) => (
                  <span key={item.key} className={`badge ${detail.capabilities[item.key] ? 'green' : 'slate'}`}>
                    {`${item.label}：${detail.capabilities[item.key] ? '支持' : '不适用'}`}
                  </span>
                ))}
              </div>
            </div>

            <div className="card">
              <div className="panel-title">{'操作'}</div>
              <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                <button
                  className="btn btn-sm"
                  disabled={busy === `renew:${detail.providerId}` || !detail.capabilities.oauth}
                  onClick={() => onRenew(detail.providerId)}
                  title={detail.capabilities.oauth ? '按厂商自己的窗口续期；未到窗口不会强行轮换' : '该账号没有可续期凭证'}
                >
                  {'立即续期'}
                </button>
                <button
                  className="btn btn-sm"
                  disabled={busy === `quota:${detail.providerId}` || !detail.capabilities.quota}
                  onClick={() => onRefreshQuota(detail.providerId)}
                  title={detail.capabilities.quota ? '强制读一次厂商用量接口' : '该账号没有额度接口'}
                >
                  {'刷新额度'}
                </button>
                {canProbe && (
                  <button
                    className="btn btn-sm"
                    disabled={detailProbing}
                    onClick={() => void onRunProbe(detail.providerId)}
                    title={'现在就发一发最小对话探测；同账号同时只跑一发'}
                  >
                    {detailProbing ? '探测中…' : '立即探测'}
                  </button>
                )}
                <button className="btn btn-sm" onClick={() => onOpenAlerts(detail.providerId)}>{'查看告警'}</button>
                <button className="btn btn-sm" onClick={() => onOpenLogs(detail.providerId)}>{'查看请求日志'}</button>
              </div>
              {actionError && <div className="metric-note err" style={{ marginTop: 8 }}>{actionError}</div>}
              {note && !actionError && <div className="metric-note" style={{ marginTop: 8 }}>{note}</div>}
              <div className="metric-note" style={{ marginTop: 8 }}>
                {'监控中心不提供停用、熔断隔离/复位或改配置的按钮；处置只在请求调度里做。'}
              </div>
            </div>
          </div>
        </Modal>
      )}
    </div>
  );
}

type MonitorProbePageLite = { accounts: MonitorProbeAccount[]; runs: MonitorProbeRun[] };

/** 额度低于 10% 才标警告；ratio<0 是「读数不可用」，不是低额度。 */
function seriesLow(ratio: number): boolean {
  return quotaRatioKnown({ ratio } as MonitorQuota) && ratio < 0.1;
}

function SearchIcon() {
  return (
    <svg className="mon-acc-search-icon" viewBox="0 0 24 24" width="14" height="14" aria-hidden="true">
      <circle cx="10.5" cy="10.5" r="6.2" fill="none" stroke="currentColor" strokeWidth="1.9" />
      <path d="M15.2 15.2 20 20" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" />
    </svg>
  );
}

function ClockIcon() {
  return (
    <svg className="mon-acc-fact-icon" viewBox="0 0 24 24" width="12" height="12" aria-hidden="true">
      <circle cx="12" cy="12" r="8.2" fill="none" stroke="currentColor" strokeWidth="1.9" />
      <path d="M12 7.6V12l3 1.9" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" />
    </svg>
  );
}

function RenewIcon() {
  return (
    <svg className="mon-acc-btn-icon" viewBox="0 0 24 24" width="13" height="13" aria-hidden="true">
      <path d="M19.2 12a7.2 7.2 0 1 1-2.4-5.4" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" />
      <path d="M19.5 4.2v3.8h-3.8" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function ZapIcon() {
  return (
    <svg className="mon-acc-btn-icon" viewBox="0 0 24 24" width="13" height="13" aria-hidden="true">
      <path d="M13.2 3 6.5 13h4.6L10 21l7-10.4h-4.6L13.2 3Z" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinejoin="round" />
    </svg>
  );
}
