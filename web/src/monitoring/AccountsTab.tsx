import {useEffect, useMemo, useState} from 'react';
import {BrandIcon, Modal} from '../components/ui';
import {IconSelect} from '../components/selects';
import type {IconSelectOption} from '../components/selects';
import {fetchProbes, isolateAccount, restoreAccount} from './api';
import type {
  MonitorAccount,
  MonitorIsolationResult,
  MonitorProbeAccount,
  MonitorProbeConfig,
  MonitorProbeRun,
  MonitorQuota,
} from './types';
import {hasTime, quotaRatioKnown} from './types';
import {displayTime, formatLatency, quotaText, relativeToNow} from './format';
import {probeCaseText} from './types';

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
  /**
   * 强制熔断隔离 / 恢复调度成功后由页面把新的账号行交给它，避免等下一轮轮询才变样。
   * busy 键是 `isolate:<providerId>` / `restore:<providerId>`。
   */
  onIsolationChange?: (result: MonitorIsolationResult) => void;
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

export interface AccountsDrawerProps {
  account: MonitorAccount;
  timezone: string;
  alertCount: number;
  drawerError: string;
  note: string;
  busy: string;
  probeBusy: string;
  windowSize: number;
  onRenew: (providerId: string) => void;
  onRefreshQuota: (providerId: string) => void;
  onRunProbe: (providerId: string) => void;
  onOpenAlerts: (providerId: string) => void;
  onOpenLogs: (providerId: string) => void;
  onClose: () => void;
  /** 隔离 / 恢复：由页面执行并在成功后就地刷新账号行。 */
  onIsolate: (account: MonitorAccount) => void;
  onRestore: (account: MonitorAccount) => void;
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

const UNKNOWN_VENDOR = '未知厂商';
// Provider 筛选值：全部 / 某厂商全部账号 / 单个账号。与日志页的二级筛选同一套交互，
// 只是这里多一档「整个厂商」，避免为了看一家账号还得逐个点。
const PROVIDER_FILTER_ALL = 'all';
const VENDOR_FILTER_PREFIX = 'vendor:';
const PROVIDER_FILTER_PREFIX = 'provider:';

function matchesProviderFilter(account: MonitorAccount, filter: string): boolean {
  if (filter === PROVIDER_FILTER_ALL) return true;
  if (filter.startsWith(PROVIDER_FILTER_PREFIX)) {
    return account.providerId === filter.slice(PROVIDER_FILTER_PREFIX.length);
  }
  if (filter.startsWith(VENDOR_FILTER_PREFIX)) {
    return (account.vendor || UNKNOWN_VENDOR) === filter.slice(VENDOR_FILTER_PREFIX.length);
  }
  return true;
}

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
 * 停用（含强制隔离）→ 有 firing 告警 → 请求判定异常 → 否则正常。
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
 * 停用（含隔离）=退出调度；健康且无告警=主路由（当前会被调度）；有告警或判定异常=降级兜底
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
 * 熔断档位。档位按探测侧的连续失败 streak 推导：连续失败 ≥ BREAKER_TRIP_STREAK 视为
 * 熔断阻断（OPEN），失败但未到阈值是半开试探（HALF_OPEN），其余闭合（CLOSED）。
 * 停用（含强制隔离）只算 OFF（不参与调度与探测），但停用前已经连续失败至少
 * BREAKER_WARN_STREAK 次的账号仍标 OPEN：档位是「真实门槛」的事实，不因为停用就伪装成闭合。
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
  CLOSED: `CLOSED：探测闭合——最近一发轻探测通过或还没有记录（没有任何失败阈值会在后端自动隔离账号）`,
  HALF_OPEN: `HALF_OPEN：连续失败 1~${BREAKER_TRIP_STREAK - 1} 次，未到 ${BREAKER_TRIP_STREAK} 次熔断线`,
  OPEN: `OPEN：连续失败已达 ${BREAKER_TRIP_STREAK} 次（熔断线），本卡判为异常`,
  OFF: '停用（含强制隔离）：不参与自动调度，也不会发探测',
};

/**
 * 右上角徽标的文案。与 breakerOf 一一对应：
 * 强制隔离→已隔离，OFF→停用，OPEN→异常，HALF_OPEN 且连续失败 ≥2→异常，否则看活跃告警→告警，其余→正常。
 */
type HealthCode = 'normal' | 'alert' | 'abnormal' | 'off' | 'isolated';

const HEALTH_TEXT: Record<HealthCode, string> = {
  normal: '正常',
  alert: '告警',
  abnormal: '异常',
  off: '停用',
  isolated: '已隔离',
};

function healthOf(account: MonitorAccount, alerts: number, breaker: Breaker, facts: AccountsProbeFacts | undefined): HealthCode {
  if (account.isolated) return 'isolated';
  if (breaker === 'OFF') return 'off';
  if (breaker === 'OPEN') return 'abnormal';
  if (facts?.probed && facts.consecutiveFailures >= BREAKER_WARN_STREAK) return 'abnormal';
  if (alerts > 0) return 'alert';
  return 'normal';
}

/** 徽标的判定依据：悬停即可看到每一条门槛的当前值，不让人猜「异常」从哪来。 */
function healthHint(account: MonitorAccount, alerts: number, breaker: Breaker, facts: AccountsProbeFacts | undefined): string {
  if (account.isolated) {
    const when = hasTime(account.isolatedAt) ? displayTime(account.isolatedAt as string, '').text : '';
    return [
      `已被 ${account.isolatedBy || '管理员'} 强制熔断隔离${when ? `（${when}）` : ''} → 已隔离（不参与调度与探测）`,
      '隔离只有人工「恢复调度」能解除，不会自动恢复',
    ].join('\n');
  }
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
  return `${lines.join('\n')}\n判定顺序：隔离 → 停用 → 连续失败 ≥${BREAKER_TRIP_STREAK} 次 → 连续失败 ≥${BREAKER_WARN_STREAK} 次 → 有未恢复告警`;
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

/** 热力图每行保留的探测发数（/__monitor/probes 单次返回上限 50，足够填满 30 格）。 */
const HEAT_CELLS = 30;

/**
 * 耗时分档（ms）。轻探测是端到端一次最小对话，1~3 秒都属正常，按 30ms 级分档会把整屏
 * 涂成同一个颜色，所以档位取在「秒」级别：<1s 快 / 1~2.5s 正常 / >2.5s 慢 / 失败。
 */
const HEAT_FAST_MS = 1000;
const HEAT_SLOW_MS = 2500;

/**
 * 一格 = 一次真实探测（不是公式生成的装饰）。颜色只看这一发的 `ok` 与 `durationMs`：
 * 失败（ok=false）→ 红；否则 <1s → 绿、<2.5s → 蓝、更慢 → 琥珀。
 */
function heatTone(run: MonitorProbeRun): 'l1' | 'l2' | 'l3' | 'l4' {
  if (!run.ok) return 'l4';
  if (run.durationMs < HEAT_FAST_MS) return 'l1';
  if (run.durationMs < HEAT_SLOW_MS) return 'l2';
  return 'l3';
}

/** 悬停文案：时间 + 状态 + 耗时 + 模型，外加探测用例与错误码（有就写）。 */
function heatTitle(run: MonitorProbeRun, timeZone: string): string {
  const when = displayTime(run.time, timeZone);
  const verdict = run.ok ? `通过${run.status ? ` HTTP ${run.status}` : ''}` : `失败${run.status ? ` HTTP ${run.status}` : '（无响应）'}`;
  const lines = [
    `${when.text}（${when.title}）`,
    `${verdict} · 耗时 ${formatLatency(run.durationMs)}`,
    `模型 ${run.model || '—'} · 用例 ${probeCaseText(run.case)}`,
  ];
  if (!run.ok && run.errorCode) lines.push(`错误：${run.errorCode}`);
  return lines.join('\n');
}

/** 行首的 SLA：窗口内通过数 / 实测数；没有记录时返回 null（不写 100%）。 */
function heatSla(runs: MonitorProbeRun[]): number | null {
  if (runs.length === 0) return null;
  return Math.round((runs.filter((run) => run.ok).length / runs.length) * 100);
}

/**
 * SLA 点阵热力图：行 = 账号（沿用上面的搜索/筛选与排序），列 = 最近 30 发已执行的探测，
 * 左旧右新（越靠右越近）。每个方块都是 /__monitor/probes 里的一条真实记录；
 * 不足 30 发时右侧补灰格，行尾写明「实际 N 次」，没记录的账号整行灰格。
 */
function HeatView({
  rows,
  timezone,
  onProbe,
  probeBusy,
}: {
  rows: Array<{ account: MonitorAccount; runs: MonitorProbeRun[]; facts?: AccountsProbeFacts; loading: boolean }>;
  timezone: string;
  onProbe: (providerId: string) => void;
  probeBusy: string;
}) {
  return (
    <div className="card panel mon-acc-heat">
      <div className="mon-acc-heat-head">
        <div className="mon-acc-heat-title">
          <h2 className="panel-title">{'全集群高密 SLA 采样点阵'}</h2>
          <p className="panel-desc">
            {`${rows.length} 个账号 · 每格是最近 ${HEAT_CELLS} 发真实轻探测之一（左旧右新），按耗时着色；灰格是窗口内没有记录，不是失败。`}
          </p>
        </div>
        <div className="mon-acc-heat-legend" aria-label="耗时图例">
          <span className="mon-acc-heat-key l1">{`< ${formatLatency(HEAT_FAST_MS)}`}</span>
          <span className="mon-acc-heat-key l2">{`${formatLatency(HEAT_FAST_MS)}–${formatLatency(HEAT_SLOW_MS)}`}</span>
          <span className="mon-acc-heat-key l3">{`> ${formatLatency(HEAT_SLOW_MS)}`}</span>
          <span className="mon-acc-heat-key l4">{'失败'}</span>
          <span className="mon-acc-heat-key l0">{'无数据'}</span>
        </div>
      </div>
      <div className="mon-acc-heat-rows">
        {rows.map(({ account, runs, facts, loading }) => {
          const window = runs.slice(0, HEAT_CELLS);
          // runs 是新→旧；点阵左旧右新，所以倒序进位。
          const ordered = [...window].reverse();
          const cells: Array<{ tone: string; title: string }> = ordered.map((run) => ({ tone: heatTone(run), title: heatTitle(run, timezone) }));
          while (cells.length < HEAT_CELLS) {
            cells.unshift({ tone: 'l0', title: `窗口内没有第 ${cells.length + 1} 近的记录` });
          }
          const sla = heatSla(window);
          const latest = ordered[ordered.length - 1];
          const measured = window.length;
          const probing = probeBusy === `probe:${account.providerId}`;
          const full = measured >= HEAT_CELLS;
          const note = loading
            ? '读取探测记录中…'
            : measured === 0
              ? '暂无探测记录'
              : full
                ? `最近 ${HEAT_CELLS} 次`
                : `实际 ${measured} 次`;
          const rtt = facts?.rttMs ?? (latest && latest.durationMs > 0 ? latest.durationMs : undefined);
          return (
            <div key={account.providerId} className="mon-acc-heat-row">
              <div className="mon-acc-heat-who">
                <button type="button" className="mon-acc-heat-name" onClick={() => onProbe(account.providerId)} title="点名字发一发轻探测">
                  <NameText name={account.name} />
                </button>
                <span className="mon-acc-heat-tags">
                  <span className="mon-acc-ptag">{account.vendor || AUTH_LABELS[account.authType] || 'API Key'}</span>
                  <span
                    className="mon-acc-heat-meta"
                    title={sla === null ? '窗口内没有探测记录，无从算 SLA' : `窗口内 ${window.filter((run) => run.ok).length} / ${measured} 发通过`}
                  >
                    {`SLA: ${sla === null ? '—' : `${sla}%`} · RTT: ${rtt === undefined ? '—' : formatLatency(rtt)}`}
                  </span>
                </span>
              </div>
              <div
                className="mon-acc-heat-cells"
                role="img"
                aria-label={`${account.name} 最近 ${HEAT_CELLS} 发探测：${measured === 0 ? '暂无记录' : `${measured} 条，SLA ${sla}%`}`}
              >
                {cells.map((cell, index) => (
                  <span key={index} className={`mon-acc-heat-cell ${cell.tone}`} title={cell.title} />
                ))}
              </div>
              <span className={`mon-acc-heat-note${measured === 0 ? ' empty' : ''}`}>{note}</span>
              <button
                type="button"
                className="btn btn-sm mon-acc-heat-probe"
                disabled={probing || account.disabled}
                onClick={() => onProbe(account.providerId)}
                title={account.isolated ? '账号已强制隔离，探测跳过' : account.disabled ? '账号已停用，探测跳过' : '现在就发一发最小对话探测'}
              >
                {probing ? '探测中…' : '探测'}
              </button>
            </div>
          );
        })}
      </div>
    </div>
  );
}

/*
 * 视图切换记在 URL（?view=heatmap，与页面其它深链同一个来源）并在刷新后保持；
 * 没有参数时再读 localStorage 的会话内偏好。两边都不可用时退回集群卡片。
 */
const HEAT_VIEW_STORAGE_KEY = 'monitor-accounts-view';

function viewFromLocation(): ViewMode {
  let fromUrl = '';
  try {
    fromUrl = new URLSearchParams(window.location.search).get('view') || '';
  } catch {
    fromUrl = '';
  }
  if (fromUrl === 'heatmap' || fromUrl === 'cards') return fromUrl === 'heatmap' ? 'heat' : 'cards';
  try {
    return window.localStorage.getItem(HEAT_VIEW_STORAGE_KEY) === 'heat' ? 'heat' : 'cards';
  } catch {
    return 'cards';
  }
}

function persistView(mode: ViewMode) {
  try {
    window.localStorage.setItem(HEAT_VIEW_STORAGE_KEY, mode);
  } catch {
    // 隐私模式下存储不可用：URL 仍是本次的唯一来源。
  }
  try {
    const url = new URL(window.location.href);
    if (mode === 'heat') url.searchParams.set('view', 'heatmap');
    else url.searchParams.delete('view');
    const next = `${url.pathname}${url.search}${url.hash}`;
    if (next !== `${window.location.pathname}${window.location.search}${window.location.hash}`) {
      window.history.replaceState(window.history.state, '', next);
    }
  } catch {
    // 无 history 环境（极少）：只记 localStorage。
  }
}

/**
 * 账号抽屉。由 AccountsTab 在打开时渲染，关闭即卸载——上一轮的隔离提示、
 * 探测结论都随卸载一起作废，重开同一账号不会「复活」旧结果。
 * 探测现状自己读一次（页面轮询里没有记录明细），已打开时随页面刷新同步。
 */
export function AccountsDrawer(props: AccountsDrawerProps) {
  const {
    account, timezone, alertCount, drawerError, note, busy, probeBusy, windowSize,
    onRenew, onRefreshQuota, onRunProbe, onOpenAlerts, onOpenLogs, onClose, onIsolate, onRestore,
  } = props;
  const detail = account;
  const detailProbe = useAccountProbeDetail(account.providerId, probeBusy, windowSize);
  const detailWindow = detailProbe;
  // 能用的功能才出现：停用或隔离的账号自动调度会跳过，手动探测也只会写一条 skipped，没有排障
  // 价值，所以直接不给入口。其余账号都给——「没配模型」由后端在按下时以 skipped 结果
  // 回给抽屉，界面如实展示那一句，不在前端猜（前端拿不到 model 字段）。
  const canProbe = !detail.disabled;
  const detailProbing = probeBusy === `probe:${detail.providerId}`;

  return (
    <Modal
      title={detail.name}
      description={`${AUTH_LABELS[detail.authType] || detail.authType || 'API Key'} · ${detail.vendor || '未知厂商'} · ${detail.providerId}`}
      onClose={onClose}
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
              {detail.isolated && <div className="metric-note">{'账号已强制熔断隔离：调度跳过它，也不会发探测；恢复调度后才能继续探测。'}</div>}
              {!detail.isolated && detail.disabled && <div className="metric-note">{'账号已停用，自动调度跳过它，也不会发探测。'}</div>}
            </>
          ) : (
            <div className="metric-note">{'探测现状读取中'}</div>
          )}
        </div>

        <div className="card">
          <div className="panel-title">{'健康与能力'}</div>
          <div className="metric-note">
            {`状态 ${detail.healthStatus || '未检测'} · 模型列表连续失败 ${detail.modelFailures} 次 · 活跃告警 ${alertCount}`}
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
                onClick={() => onRunProbe(detail.providerId)}
                title={'现在就发一发最小对话探测；同账号同时只跑一发'}
              >
                {detailProbing ? '探测中…' : '立即探测'}
              </button>
            )}
            {detail.isolated ? (
              <button
                className="btn btn-sm mon-acc-btn-restore"
                disabled={busy === `restore:${detail.providerId}`}
                onClick={() => onRestore(detail)}
                title="解除强制熔断隔离，账号回到调度"
              >
                {busy === `restore:${detail.providerId}` ? '恢复中…' : '恢复调度'}
              </button>
            ) : (
              <button
                className="btn btn-sm mon-acc-btn-isolate"
                disabled={detail.disabled || busy === `isolate:${detail.providerId}`}
                onClick={() => onIsolate(detail)}
                title={detail.disabled ? '该账号已被手工停用，请先在 Providers 页启用' : '临时停用并排除出调度，只有人工恢复才会解除'}
              >
                {busy === `isolate:${detail.providerId}` ? '隔离中…' : '强制熔断隔离'}
              </button>
            )}
            <button className="btn btn-sm" onClick={() => onOpenAlerts(detail.providerId)}>{'查看告警'}</button>
            <button className="btn btn-sm" onClick={() => onOpenLogs(detail.providerId)}>{'查看请求日志'}</button>
          </div>
          {drawerError && <div className="metric-note err" style={{ marginTop: 8 }}>{drawerError}</div>}
          {note && !drawerError && <div className="metric-note" style={{ marginTop: 8 }}>{note}</div>}
          <div className="metric-note" style={{ marginTop: 8 }}>
            {'「强制熔断隔离」= 临时停用账号并排除出调度，带操作者记录、不会自动恢复；「恢复调度」才解除。'}
          </div>
        </div>
      </div>
    </Modal>
  );
}

export function AccountsTab(props: AccountsTabProps) {
  const {
    accounts, timezone, iconOf, alertCountOf, busy, error, note, actionError,
    onRenew, onRefreshQuota, onOpenAlerts, onOpenLogs, probeBusy, onRunProbe, probeView, probeConfig,
    selectedId, onSelect, onCloseDrawer, onIsolationChange,
  } = props;
  const [search, setSearch] = useState('');
  const [providerFilter, setProviderFilter] = useState(PROVIDER_FILTER_ALL);
  const [stateFilter, setStateFilter] = useState<StateFilter>('all');
  const [breakerFilter, setBreakerFilter] = useState<BreakerFilter>('all');
  // 只保留本项目有真实能力的筛选（状态 / 熔断档位）；参考里的「容灾层级」是 mock 字段，
  // 这里换成按凭证类型筛，过滤的仍是后端真给的 authType。
  const [authFilter, setAuthFilter] = useState('all');
  const [viewMode, setViewMode] = useState<ViewMode>(() => viewFromLocation());
  const changeView = (mode: ViewMode) => {
    setViewMode(mode);
    persistView(mode);
  };
  // 浏览器前进/后退：?view= 变了就跟着切（与页面的 popstate 还原同一时机）。
  useEffect(() => {
    const restore = () => setViewMode(viewFromLocation());
    window.addEventListener('popstate', restore);
    return () => window.removeEventListener('popstate', restore);
  }, []);
  const now = Date.now();

  // 隔离 / 恢复：二次确认 → 调接口 → 成功就地把新的账号行交给页面。
  // 隔离 / 恢复自己发请求而不是走页面的 run()，因为结果要回到这里就地显示在卡片上；
  // 失败原因（如「已被手工停用」）由后端给出，原样展示。
  const [pending, setPending] = useState<{ providerId: string; action: 'isolate' | 'restore' } | null>(null);
  const [isolationBusy, setIsolationBusy] = useState('');
  const [isolationNote, setIsolationNote] = useState<Record<string, { tone: 'ok' | 'err'; text: string }>>({});
  useEffect(() => {
    if (!pending) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setPending(null);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [pending]);
  const confirmIsolation = async () => {
    if (!pending) return;
    const { providerId, action } = pending;
    setPending(null);
    setIsolationBusy(providerId);
    try {
      const result = action === 'isolate' ? await isolateAccount(providerId) : await restoreAccount(providerId);
      onIsolationChange?.(result);
      setIsolationNote((prev) => ({
        ...prev,
        [providerId]: {
          tone: 'ok',
          text: action === 'isolate' ? '已强制熔断隔离，调度将跳过该账号' : '已恢复调度，账号回到调度池',
        },
      }));
    } catch (reason) {
      const text = reason instanceof Error && reason.message ? reason.message : '操作失败，请稍后重试';
      setIsolationNote((prev) => ({ ...prev, [providerId]: { tone: 'err', text } }));
    } finally {
      setIsolationBusy('');
    }
  };
  const pendingAccount = pending ? accounts.find((account) => account.providerId === pending.providerId) : undefined;

  const providerOptions = useMemo<IconSelectOption[]>(() => {
    const groups = new Map<string, MonitorAccount[]>();
    for (const account of accounts) {
      const label = account.vendor || UNKNOWN_VENDOR;
      const list = groups.get(label);
      if (list) list.push(account);
      else groups.set(label, [account]);
    }
    const options: IconSelectOption[] = [{ value: PROVIDER_FILTER_ALL, label: '全部 Provider' }];
    for (const [vendor, list] of [...groups.entries()].sort((a, b) => a[0].localeCompare(b[0], 'zh-CN'))) {
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
  }, [accounts, iconOf]);

  // 账号被删除 / 改名后旧筛选值会悬空：回到「全部」，否则列表恒空且看不出为什么。
  useEffect(() => {
    if (providerFilter === PROVIDER_FILTER_ALL) return;
    if (!providerOptions.some((option) => option.value === providerFilter)) setProviderFilter(PROVIDER_FILTER_ALL);
  }, [providerFilter, providerOptions]);

  const authTypes = useMemo(() => {
    const seen = new Set<string>();
    for (const account of accounts) seen.add(AUTH_LABELS[account.authType] || account.authType || 'API Key');
    return [...seen].sort((a, b) => a.localeCompare(b, 'zh-CN'));
  }, [accounts]);

  const filtered = useMemo(() => {
    const term = search.trim().toLowerCase();
    return accounts
      .filter((account) => matchesProviderFilter(account, providerFilter))
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
  }, [accounts, search, providerFilter, stateFilter, breakerFilter, authFilter, alertCountOf, probeView]);

  const detail = accounts.find((account) => account.providerId === selectedId) ?? null;
  const windowSize = probeView?.windowSize ?? 10;

  // 热力图行：每行需要该账号最近 30 发且带耗时，页面级 probeView 只留了 windowSize 发、
  // 也没有记录明细，所以切到热力图时单独读一次 probes（与卡片同一接口，口径一致）。
  const HEAT_FETCH_MS = 5000;
  const [heatSource, setHeatSource] = useState<{ page: MonitorProbePageLite; at: number } | null>(null);
  const [heatTick, setHeatTick] = useState(0);
  // 轮询中即使切到卡片也继续读，切回来时数据是新的而不是停在切走前那一刻。
  useEffect(() => {
    let live = true;
    fetchProbes()
      .then((page) => {
        if (live) setHeatSource({ page, at: Date.now() });
      })
      .catch(() => {
        // 失败保留上一份；第一次失败按「已读过、无记录」处理，不让行一直停在「读取中」。
        if (live) setHeatSource((prev) => prev ?? { page: { accounts: [], runs: [] }, at: Date.now() });
      });
    return () => {
      live = false;
    };
  }, [heatTick, probeBusy, accounts.length]);
  useEffect(() => {
    if (viewMode !== 'heat') return;
    const timer = window.setInterval(() => setHeatTick((value) => value + 1), HEAT_FETCH_MS);
    return () => window.clearInterval(timer);
  }, [viewMode]);
  const heatRows = useMemo(() => {
    // /__monitor/probes 的 runs 是真记录、accounts 里才有 skipped 的发数；这里只画 runs，
    // skipped 的探测没有执行耗可言，不占格。
    const byAccount = new Map<string, MonitorProbeRun[]>();
    const runs = [...(heatSource?.page.runs ?? [])]
      .filter((run) => !run.skipped)
      .sort((a, b) => Date.parse(b.time) - Date.parse(a.time));
    for (const run of runs) {
      const list = byAccount.get(run.providerId) ?? [];
      list.push(run);
      byAccount.set(run.providerId, list);
    }
    return filtered.map((account) => ({
      account,
      runs: byAccount.get(account.providerId) ?? [],
      facts: probeView?.factsOf(account.providerId),
      loading: !heatSource,
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

          <div className="mon-acc-provider-filter">
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

        <div className="mon-acc-toolbar-side">
          <span
            className="mon-acc-count"
            title="角色、熔断档位按健康与探测结果推导；强制隔离由人工发起并需人工恢复"
          >{`${filtered.length} / ${accounts.length} 个账号`}<span className="mon-hint-dot" role="note" title="角色、熔断档位按健康与探测结果推导；强制隔离由人工发起并需人工恢复">{'?'}</span></span>
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
              onClick={() => changeView('cards')}
            >
              {'集群卡片'}
            </button>
            <button
              type="button"
              className={`mon-acc-view-btn${viewMode === 'heat' ? ' on' : ''}`}
              aria-pressed={viewMode === 'heat'}
              onClick={() => changeView('heat')}
            >
              {'SLA 点阵热力图'}
            </button>
          </div>
        </div>
      </div>

      {empty && <Onboarding />}
      {!empty && filtered.length === 0 && <div className="m-empty">{'没有符合当前筛选的账号。'}</div>}

      {viewMode === 'heat' && !empty && filtered.length > 0 && (
        <HeatView
          rows={heatRows}
          timezone={timezone}
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
          const isolationBusyHere = isolationBusy === account.providerId;
          const flash = isolationNote[account.providerId];
          const isolatedAtText =
            account.isolated && hasTime(account.isolatedAt)
              ? `${account.isolatedBy || '管理员'} · ${displayTime(account.isolatedAt as string, timezone).text}`
              : '';
          return (
            <article
              key={account.providerId}
              className={`mon-acc-card state-${state}${fresh ? ' is-open' : ''}${account.isolated ? ' is-isolated' : ''}`}
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

              {/* 参考的首屏分组（徽标 + 名称 + 指标块）到此结束。 */}
              <div className="mon-acc-primary">
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
                <div className="mon-acc-fact" title={isolatedAtText ? `隔离于 ${isolatedAtText}` : `凭证到期：${account.expiresAt ?? '无凭证或不过期'}`}>
                  <span className="mon-acc-fact-key">
                    <ClockIcon />
                    {account.isolated ? '隔离于' : '续期'}
                  </span>
                  <span className={`mon-acc-fact-value${account.isolated ? ' err' : ''}`}>
                    {account.isolated ? isolatedAtText || '已隔离' : expiryText(account, now)}
                  </span>
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
                {account.isolated ? (
                  <button
                    type="button"
                    className="btn btn-sm mon-acc-btn mon-acc-btn-restore"
                    disabled={isolationBusyHere}
                    onClick={() => setPending({ providerId: account.providerId, action: 'restore' })}
                    title="解除强制熔断隔离，账号回到调度"
                  >
                    <PowerIcon />
                    {isolationBusyHere ? '恢复中…' : '恢复调度'}
                  </button>
                ) : (
                  <button
                    type="button"
                    className="btn btn-sm mon-acc-btn mon-acc-btn-isolate"
                    disabled={isolationBusyHere || account.disabled}
                    onClick={() => setPending({ providerId: account.providerId, action: 'isolate' })}
                    title={account.disabled ? '该账号已被手工停用，请先在 Providers 页启用' : '临时停用并排除出调度；只有人工恢复才会解除'}
                  >
                    <SlashIcon />
                    {isolationBusyHere ? '隔离中…' : '强制熔断隔离'}
                  </button>
                )}
                <button
                  type="button"
                  className="btn btn-sm mon-acc-btn mon-acc-btn-ghost mon-acc-btn-detail"
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
                  title={account.isolated ? '账号已强制隔离，探测跳过' : account.disabled ? '账号已停用，探测跳过' : '现在就发一发最小对话探测'}
                >
                  <ZapIcon />
                  {cardProbing ? '探测中…' : '轻探测'}
                </button>
              </div>
              {flash && <div className={`mon-acc-inline ${flash.tone}`}>{flash.text}</div>}
              {fresh && actionError && <div className="mon-acc-inline err">{actionError}</div>}
              {fresh && !actionError && note && <div className="mon-acc-inline">{note}</div>}
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
        <AccountsDrawer
          account={detail}
          timezone={timezone}
          alertCount={alertCountOf(detail.providerId)}
          drawerError={actionError}
          note={note}
          busy={busy}
          probeBusy={probeBusy}
          windowSize={windowSize}
          onRenew={onRenew}
          onRefreshQuota={onRefreshQuota}
          onRunProbe={(providerId) => void onRunProbe(providerId)}
          onOpenAlerts={onOpenAlerts}
          onOpenLogs={onOpenLogs}
          onClose={onCloseDrawer}
          onIsolate={(account) => setPending({ providerId: account.providerId, action: 'isolate' })}
          onRestore={(account) => setPending({ providerId: account.providerId, action: 'restore' })}
        />
      )}

      {pending && pendingAccount && (
        <div className="mon-acc-confirm-layer" role="presentation" onClick={() => setPending(null)}>
          <div
            className="mon-acc-confirm"
            role="dialog"
            aria-modal="true"
            aria-labelledby="mon-acc-confirm-title"
            onClick={(event) => event.stopPropagation()}
          >
            <h3 id="mon-acc-confirm-title" className="mon-acc-confirm-title">
              {pending.action === 'isolate' ? '强制熔断隔离该账号？' : '恢复该账号的调度？'}
            </h3>
            <p className="mon-acc-confirm-body">
              {pending.action === 'isolate'
                ? `「${pendingAccount.name}」将被临时停用，所有 Key 的调度都会跳过它，也不会再发探测。隔离带操作者记录，且不会自动恢复。`
                : `「${pendingAccount.name}」将解除强制熔断隔离，重新回到调度池并可能立刻接到请求。`}
            </p>
            <div className="mon-acc-confirm-actions">
              <button type="button" className="btn btn-sm" onClick={() => setPending(null)}>
                {'取消'}
              </button>
              <button
                type="button"
                className={`btn btn-sm ${pending.action === 'isolate' ? 'mon-acc-btn-isolate' : 'mon-acc-btn-restore'}`}
                onClick={() => void confirmIsolation()}
                autoFocus
              >
                {pending.action === 'isolate' ? '确认隔离' : '确认恢复'}
              </button>
            </div>
          </div>
        </div>
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

/** 隔离：斜杠圆环，表示「把这条线掐掉」。 */
function SlashIcon() {
  return (
    <svg className="mon-acc-btn-icon" viewBox="0 0 24 24" width="13" height="13" aria-hidden="true">
      <circle cx="12" cy="12" r="8.2" fill="none" stroke="currentColor" strokeWidth="1.8" />
      <path d="M6.2 17.8 17.8 6.2" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
    </svg>
  );
}

/** 恢复：电源键，表示「放回调度」。 */
function PowerIcon() {
  return (
    <svg className="mon-acc-btn-icon" viewBox="0 0 24 24" width="13" height="13" aria-hidden="true">
      <path d="M12 3.5v8" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" />
      <path d="M7.4 6.6a7.4 7.4 0 1 0 9.2 0" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" />
    </svg>
  );
}
