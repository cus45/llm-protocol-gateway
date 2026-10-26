import type {ReactNode} from 'react';
import {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import {Modal, NavIcon} from '../components/ui';
import {navPathForID} from '../lib';
import type {NavItemID} from '../types';
import {
    acknowledgeAlert,
    createChannel,
    deleteChannel,
    fetchAccounts,
    fetchAlert,
    fetchAlerts,
    fetchChannels,
    fetchDispatch,
    fetchEvents,
    fetchOverview,
    fetchProbeConfig,
    fetchProbes,
    fetchRules,
    ignoreAlert,
    MonitorAPIError,
    refreshQuota,
    renewAccount,
    retryPush,
    runProbe,
    saveProbeConfig,
    testChannel,
    updateChannel,
    updateDispatch,
} from './api';
import type {
    MonitorAccount,
    MonitorAlert,
    MonitorAlertDetail,
    MonitorChannelInput,
    MonitorChannelView,
    MonitorDispatchConfig,
    MonitorEvent,
    MonitorOverview,
    MonitorProbeAccount,
    MonitorProbeConfig,
    MonitorProbeRun,
    MonitorProbeRunResult,
    MonitorProbeStreamMode,
    MonitorRule,
    MonitorSeverity,
} from './types';
import {hasTime} from './types';
import {displayTime, relativeToNow} from './format';
import type {AccountsProbeView} from './AccountsTab';
import {AccountsTab} from './AccountsTab';
import {AlertsTab} from './AlertsTab';
import {OverviewTab} from './OverviewTab';
import {PROBE_KIND_LIGHT, ProbesTab} from './ProbesTab';
import {RulesTab} from './RulesTab';
import {SettingsTab} from './SettingsTab';

type TabID = 'overview' | 'accounts' | 'alerts' | 'probes' | 'rules' | 'settings';
/** 页签图标直接借侧边栏那套 16px 线性 SVG（NavIcon），不再另拉一套图标库。 */
type TabIconID = 'monitoring' | 'input-providers' | 'quota-keeper' | 'traffic-tokens' | 'output-providers' | 'settings';

const TABS: Array<{ id: TabID; label: string; icon: TabIconID }> = [
  { id: 'overview', label: '概览', icon: 'monitoring' },
  { id: 'accounts', label: '集群容灾与 SLA 矩阵', icon: 'input-providers' },
  { id: 'alerts', label: '告警中心', icon: 'quota-keeper' },
  { id: 'probes', label: '事件流 (实时流式)', icon: 'traffic-tokens' },
  { id: 'rules', label: '规则引擎', icon: 'output-providers' },
  { id: 'settings', label: '设置与调度', icon: 'settings' },
];

type BadgeTone = 'plain' | 'amber' | 'live' | 'paused';

/** 页签徽标：普通灰 / 告警 amber / 探测实时（绿点脉冲）与暂停。 */
function TabBadge({ tone, children }: { tone: BadgeTone; children: ReactNode }) {
  if (tone === 'live' || tone === 'paused') {
    return (
      <span className={`mon-tab-badge mon-tab-badge-${tone}`}>
        <span className="mon-tab-badge-dot" aria-hidden="true" />
        {children}
      </span>
    );
  }
  return <span className={`mon-tab-badge${tone === 'amber' ? ' mon-tab-badge-amber' : ''}`}>{children}</span>;
}

/**
 * 顶栏的动作按钮图标（与 NavIcon 同一条 24 网格、1.7 描边），补的只是侧边栏没有的
 * 刷新 / 续期 / 密钥三枚。
 */
function HeaderActionIcon({ id }: { id: 'refresh' | 'renew' | 'key' | 'mute' }) {
  const common = {
    fill: 'none',
    stroke: 'currentColor',
    strokeWidth: 1.9,
    strokeLinecap: 'round' as const,
    strokeLinejoin: 'round' as const,
  };
  if (id === 'mute') {
    // 喇叭 + 斜杠：「告警静音」的开关态由按钮底色表达，图标本身不变。
    return (
      <svg className="mon-header-btn-icon" viewBox="0 0 24 24" aria-hidden="true" {...common}>
        <path d="M4 9.5h3.2L12 6v12l-4.8-3.5H4z" />
        <path d="M16.2 9.2 20.6 14.8M20.6 9.2 16.2 14.8" />
      </svg>
    );
  }
  if (id === 'key') {
    return (
      <svg className="mon-header-btn-icon" viewBox="0 0 24 24" aria-hidden="true" {...common}>
        <circle cx="8" cy="12" r="4.2" />
        <path d="M12.2 12H21M17.5 12v3M20.8 12v2.2" />
      </svg>
    );
  }
  if (id === 'renew') {
    // 续期：环形箭头 + 下方延长线，和「刷新」区分开。
    return (
      <svg className="mon-header-btn-icon" viewBox="0 0 24 24" aria-hidden="true" {...common}>
        <path d="M19.5 12a7.5 7.5 0 1 1-2.6-5.7" />
        <path d="M19.8 3.6v4.2h-4.2" />
        <path d="M12 15.5V9.5M9.6 11.9 12 9.5l2.4 2.4" />
      </svg>
    );
  }
  return (
    <svg className="mon-header-btn-icon" viewBox="0 0 24 24" aria-hidden="true" {...common}>
      <path d="M19.6 8.2a8 8 0 0 0-13.8-2.6L3.6 7.8" />
      <path d="M3.8 3.9v4h4" />
      <path d="M4.4 15.8a8 8 0 0 0 13.8 2.6l2.2-2.2" />
      <path d="M20.2 20.1v-4h-4" />
    </svg>
  );
}

/** 顶栏「账号续期」只看 24 小时内到期（含已过期）的登录凭证。 */
const RENEW_WINDOW_MS = 24 * 60 * 60_000;

type RenewStage = 'idle' | 'running' | 'ok' | 'skipped' | 'failed';

interface RenewRow {
  providerId: string;
  name: string;
  vendor: string;
  expiresAt: string;
  stage: RenewStage;
  message?: string;
}

const POLL_MS = 15_000;
/**
 * 监控读取的重试节奏。网关重启 / 反代 502 这类故障通常一两秒内自愈，
 * 一次失败就整页报「链路读取失败」会让人误以为监控坏了。
 */
const FETCH_RETRY_DELAYS = [400, 1200];
/** 仍在失败时的补拉间隔：比 15 秒轮询快，故障窗口过去后尽快恢复，不用等下一轮。 */
const FETCH_FAILED_RETRY_MS = 4_000;/**
 * 单发轻探测的后端上限是 30 秒，期间「正在探测」的按钮要一直亮着；
 * 这个节拍只比轮询密，别让它比探测本身先停。
 */
const PROBE_POLL_MS = 3_000;
/** 账号卡片点阵保留的探测发数（与 /__monitor/probes 单次返回的 50 发上限配套）。 */
const ACCOUNT_DOT_WINDOW = 20;
const EVENT_PAGE = 30;
const ALERT_PAGE = 50;

function tabFromLocation(): TabID {
  const value = new URLSearchParams(window.location.search).get('tab');
  const found = TABS.find((item) => item.id === value);
  return found ? found.id : 'overview';
}

function paramFromLocation(key: string): string {
  return new URLSearchParams(window.location.search).get(key) || '';
}

export interface MonitoringPageProps {
  /** 只用于品牌图标：外层把控制台已有的 providerIcon 结果传进来。 */
  iconOf?: (providerId: string) => string | undefined;
  /** 「查看请求日志」：外层把 API 日志页按该 provider 过滤并切过去。 */
  onOpenAccountLogs?: (providerId: string) => void;
}

export function MonitoringPage({ iconOf, onOpenAccountLogs }: MonitoringPageProps) {
  const [tab, setTab] = useState<TabID>(() => tabFromLocation());
  // 页签条两侧的滚动渐隐：只有真能横滚时才挂 fade 类，桌面下不出现假的边缘阴影。
  const tabsRef = useRef<HTMLDivElement | null>(null);
  const [tabsScroll, setTabsScroll] = useState({ start: false, end: false });
  const [overview, setOverview] = useState<MonitorOverview | null>(null);
  const [accounts, setAccounts] = useState<MonitorAccount[]>([]);
  const [alerts, setAlerts] = useState<MonitorAlert[]>([]);
  const [alertTotal, setAlertTotal] = useState(0);
  // 筛选条括号里的合计要全量口径：另外两个状态各查一次（pageSize=1 只要 total）。
  const [alertCounts, setAlertCounts] = useState({ firing: 0, resolved: 0 });
  const [events, setEvents] = useState<MonitorEvent[]>([]);
  const [channels, setChannels] = useState<MonitorChannelView[]>([]);
  const [dispatch, setDispatch] = useState<MonitorDispatchConfig | null>(null);
  const [probeAccounts, setProbeAccounts] = useState<MonitorProbeAccount[]>([]);
  const [probeRuns, setProbeRuns] = useState<MonitorProbeRun[]>([]);
  // 探测页「清空」过列表：下一次 /probes 响应的 runs 丢弃一次，之后照常跟随服务端。
  const probeRunsClearedRef = useRef(false);
  const [probeConfig, setProbeConfig] = useState<MonitorProbeConfig | null>(null);
  const [probeRunBusy, setProbeRunBusy] = useState('');
  /*
   * 正在跑的探测用 ref 记：连续点两下时，第一发的 finally 不能提前清掉第二发的 busy，
   * 否则按钮在探测途中就恢复成可点，让人以为没生效再点一次。
   */
  const probeRunSeq = useRef(0);
  // 弹层里的探测结果按 providerId 存：轮询换新数据时结果不跟着丢，新的探测覆盖旧的。
  const [probeResult, setProbeResult] = useState<{ providerId: string; data: MonitorProbeRunResult } | null>(null);
  // 页签徽标跟随事件流自身的状态（暂停 / 轮询降级都算非 LIVE），而不是只看采集器时间。
  const [probeStreamMode, setProbeStreamMode] = useState<MonitorProbeStreamMode>('live');
  /*
   * 配置表单的编辑草稿。只在「首次拿到后端配置」与「保存成功」时重置，
   * 之后的 15 秒轮询不覆盖未保存的编辑；null = 跟随后端值。
   */
  const [probeDraft, setProbeDraft] = useState<MonitorProbeConfig | null>(null);
  // 配置表单只在「后端配置变了（首次加载/保存成功）」时重置，定时轮询不覆盖未保存的编辑。
  const [probeSettingsError, setProbeSettingsError] = useState('');
  const [probeSettingsNotice, setProbeSettingsNotice] = useState('');
  const [alertDetail, setAlertDetail] = useState<MonitorAlertDetail | null>(null);
  const [selectedAccount, setSelectedAccount] = useState<string>(() => paramFromLocation('account'));
  // 探测页的记录筛选直接取自深链 ?account=，与 popstate 还原同一个来源，不另存一份状态。
  const probeFilter = tab === 'probes' ? selectedAccount : '';
  const [statusFilter, setStatusFilter] = useState('firing');
  const [severityFilter, setSeverityFilter] = useState<MonitorSeverity | ''>('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [settingsError, setSettingsError] = useState('');
  // 规则页的保存只写规则；成功/失败都由页面内部提示，失败更不刷新（否则响应会把编辑冲掉）。
  const [rulesNotice, setRulesNotice] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState('');
  // 动作结果条会自动消失，避免「点了没反应」或提示一直挂在页头。
  useEffect(() => {
    if (!notice) return;
    const timer = window.setTimeout(() => setNotice(''), 10_000);
    return () => window.clearTimeout(timer);
  }, [notice]);
  // 顶栏「账号续期」：null = 未打开；数组 = 弹窗里的续期计划（空数组表示没有到期账号）。
  const [renewRows, setRenewRows] = useState<RenewRow[] | null>(null);
  const [renewBusy, setRenewBusy] = useState(false);
  // 抽屉里的动作要就地给反馈：账号页在 Modal 打开时，面板顶部的错误行会被抽屉挡住。
  const [drawerNote, setDrawerNote] = useState('');
  const [drawerError, setDrawerError] = useState('');
  // 顶栏副标题与「规则」页签徽标要的是全量口径：告警列表可能被状态/级别筛选过，不能拿来数。
  const [rules, setRules] = useState<MonitorRule[]>([]);
  const [lastLoadedAt, setLastLoadedAt] = useState('');
  // alertPagesShownRef 是告警列表当前展开到第几页：轮询按它重新取，不让「加载更多」
  // 在 15 秒后被悄悄收回。
  const alertPagesShownRef = useRef(1);
  // 顶部同步胶囊在窄屏（1024 下只有约 100px）装不下完整时间，改用相对时间「3 分钟前」：
  // 绝对时间与完整口径都留在 title 里，不再靠省略号把「10/26 03:21:5…」切掉。
  const [nowMs, setNowMs] = useState(() => Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNowMs(Date.now()), 30_000);
    return () => window.clearInterval(timer);
  }, []);

  const generation = useRef(0);
  // 失败后的补拉定时器与「当前最新一次 loadVisible」：4 秒补拉不能重新建一个闭包，
  // 否则会拿着旧的筛选条件去取数。
  const retryTimerRef = useRef<number | null>(null);
  const loadVisibleRef = useRef<() => void>(() => undefined);
  const timezone = dispatch?.timezone || 'Asia/Shanghai';

  const goTab = useCallback((next: TabID, params: Record<string, string> = {}) => {
    setTab(next);
    const search = new URLSearchParams();
    search.set('tab', next);
    for (const [key, value] of Object.entries(params)) {
      if (value) search.set(key, value);
    }
    window.history.pushState({}, '', `${window.location.pathname}?${search.toString()}`);
  }, []);

  // 深链：首屏与浏览器前进/后退都还原到同一 tab 与对象。
  useEffect(() => {
    const restore = () => {
      setTab(tabFromLocation());
      setSelectedAccount(paramFromLocation('account'));
    };
    window.addEventListener('popstate', restore);
    return () => window.removeEventListener('popstate', restore);
  }, []);

  const loadVisible = useCallback(async () => {
    const mine = ++generation.current;
    setLoading(true);
    try {
      // 探测接口失败不能拖垮整页刷新（探测页照常显示账号与告警，只是缺记录），所以单独兜底。
      const probesPromise = fetchProbes(tab === 'probes' ? probeFilter : undefined).catch((reason: unknown) => {
        if (reason instanceof MonitorAPIError && (reason.status === 401 || reason.status === 403)) throw reason;
        return null;
      });
      const [nextOverview, nextAccounts, nextAlerts, nextEvents, nextChannels, nextDispatch, nextProbes, nextRules] = await Promise.all([
        fetchWithRetry(fetchOverview),
        fetchWithRetry(fetchAccounts),
        fetchWithRetry(() => fetchAlerts({
          status: statusFilter || undefined,
          severity: severityFilter || undefined,
          range: '7d',
          pageSize: ALERT_PAGE,
        })),
        fetchWithRetry(() => fetchEvents({
          range: '24h',
          pageSize: EVENT_PAGE,
        })),
        // 告警详情里的「重试推送」要选渠道，所以告警页也要拿一次渠道（脱敏视图）。
        tab === 'settings' || tab === 'alerts' ? fetchWithRetry(fetchChannels) : Promise.resolve(null),
        dispatch ? Promise.resolve(null) : fetchWithRetry(fetchDispatch),
        probesPromise,
        // 规则表只给顶栏副标题和页签计数用；读失败不影响页面，等下一轮再试。
        fetchRules().catch(() => null),
      ]);
      if (mine !== generation.current) return;
      setOverview(nextOverview);
      setAccounts(nextAccounts.accounts);
      // 用户点过「加载更多」时，轮询不能把列表缩回第一页：按已展开的页数重新取，
      // 让正在看的那几条留在原处，而不是每 15 秒被收回去一次。
      const wantedAlertPages = Math.min(alertPagesShownRef.current, Math.max(1, Math.ceil(nextAlerts.total / ALERT_PAGE)));
      if (tab === 'alerts' && wantedAlertPages > 1) {
        void fetchAlerts({ status: statusFilter || undefined, severity: severityFilter || undefined, range: '7d', page: wantedAlertPages, pageSize: ALERT_PAGE })
          .then((last) => {
            if (mine !== generation.current) return;
            setAlerts(mergeByID(nextAlerts.items, last.items));
            setAlertTotal(last.total);
          })
          .catch(() => setError('告警加载失败'));
      } else {
        alertPagesShownRef.current = 1;
        setAlerts(nextAlerts.items);
        setAlertTotal(nextAlerts.total);
      }
      setEvents(nextEvents.items);
      if (nextChannels) setChannels(nextChannels);
      if (nextDispatch) setDispatch(nextDispatch);
      if (nextProbes) {
        setProbeAccounts(nextProbes.accounts);
        // 探测页自己清空过列表时，旧轮次的响应不再把记录带回来（账号现状照常更新）。
        setProbeRuns((prev) => (probeRunsClearedRef.current ? prev : nextProbes.runs));
        probeRunsClearedRef.current = false;
        setProbeConfig(nextProbes.config);
      }
      if (nextRules) setRules(nextRules);
      setLastLoadedAt(nextOverview.generatedAt);
      // 成功一轮就取消挂起的补拉，避免多打一次。
      if (retryTimerRef.current !== null) {
        window.clearTimeout(retryTimerRef.current);
        retryTimerRef.current = null;
      }
      setError('');
    } catch (reason) {
      if (mine !== generation.current) return;
      setError(reason instanceof MonitorAPIError ? reason.message : '监控数据读取失败');
      const authFailure = reason instanceof MonitorAPIError && (reason.status === 401 || reason.status === 403);
      // 旧数据保留在页面上；4 秒后再拉一次，故障窗口过去就自动恢复，不用等 15 秒轮询。
      // 鉴权失败重试也没用（401/403 要么登录态过期，要么没有权限）。
      if (!authFailure && retryTimerRef.current === null) {
        retryTimerRef.current = window.setTimeout(() => {
          retryTimerRef.current = null;
          if (!document.hidden) loadVisibleRef.current();
        }, FETCH_FAILED_RETRY_MS);
      }
    } finally {
      if (mine === generation.current) setLoading(false);
    }
  }, [dispatch, probeFilter, severityFilter, statusFilter, tab]);

  useEffect(() => {
    loadVisibleRef.current = () => {
      void loadVisible();
    };
    return () => {
      if (retryTimerRef.current !== null) {
        window.clearTimeout(retryTimerRef.current);
        retryTimerRef.current = null;
      }
    };
  }, [loadVisible]);

  useEffect(() => {
    void loadVisible();
  }, [loadVisible]);

  // 页签条横滚渐隐：宽度不够（手机档 / 加宽字体的 1024）时两端才出渐隐，
  // 并且滚到哪端就收哪端，让人知道后面还有页签。
  useEffect(() => {
    const el = tabsRef.current;
    if (!el) return;
    const update = () => {
      const max = el.scrollWidth - el.clientWidth;
      setTabsScroll({ start: el.scrollLeft > 1, end: max > 1 && el.scrollLeft < max - 1 });
    };
    update();
    el.addEventListener('scroll', update, { passive: true });
    const observer = new ResizeObserver(update);
    observer.observe(el);
    if (el.firstElementChild) observer.observe(el.firstElementChild);
    return () => {
      el.removeEventListener('scroll', update);
      observer.disconnect();
    };
  }, []);

  // 激活页签滚入视野：1024 下「规则引擎」超出右缘、390 下靠后的页签完全在屏外。
  // 直接改滚动容器的 scrollLeft，不用 scrollIntoView——后者会把祖先（含整页）一起滚动，
  // 深链 ?tab=rules 打开时页面会莫名跳到中间。只动横向、竖直方向绝不碰。
  useEffect(() => {
    const el = tabsRef.current;
    const btn = el?.querySelector<HTMLElement>('.mon-tab.active');
    if (!el || !btn) return;
    const pad = parseFloat(getComputedStyle(el).paddingLeft) || 0;
    // 右端多留 4px（渐隐伪元素 24px 只有边缘轻轻压过来）：贴边时最后一个页签的右侧圆角/边框
    // 刚好压在渐隐起点上，实测 390 下右边界 365 vs 可见 364 差 1px。
    const padR = (parseFloat(getComputedStyle(el).paddingRight) || 0) + 4;
    const left = btn.offsetLeft - pad;
    const right = btn.offsetLeft + btn.offsetWidth + padR;
    if (left < el.scrollLeft) el.scrollLeft = left;
    else if (right > el.scrollLeft + el.clientWidth) el.scrollLeft = right - el.clientWidth;
  }, [tab, tabsScroll.end]);

  // 探测配置：只在设置页读一次 `/probe-config`（写入要走 PUT）。失败不拖垮整页：
  // 设置页的其它卡片照常可用，探测配置卡片不出现，等下一轮再试。
  useEffect(() => {
    if (tab !== 'settings' || probeConfig) return;
    let cancelled = false;
    void fetchProbeConfig()
      .then((config) => {
        if (!cancelled) setProbeConfig(config);
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, [tab, probeConfig]);

    // 告警页的两段合计（进行中 / 已恢复）不受状态筛选影响，所以单独取一次；
  // 级别筛选照常带上，括号里的数字与当前列表同一口径。只在告警页且数据变化时重取。
  useEffect(() => {
    if (tab !== 'alerts') return;
    let cancelled = false;
    const count = (status: string) =>
      fetchAlerts({ status, severity: severityFilter || undefined, range: '7d', pageSize: 1 })
        .then((page) => page.total)
        .catch(() => 0);
    void Promise.all([count('firing'), count('resolved')]).then(([firing, resolved]) => {
      if (!cancelled) setAlertCounts({ firing, resolved });
    });
    return () => {
      cancelled = true;
    };
  }, [tab, severityFilter, alerts, alertTotal]);

  // 15 秒轮询；只有定时任务在页面隐藏时跳过，手动/操作后的刷新必须真的取数。
  useEffect(() => {
    const timer = window.setInterval(() => {
      if (!document.hidden) void loadVisible();
    }, POLL_MS);
    const onVisible = () => {
      if (!document.hidden) void loadVisible();
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [loadVisible]);

  /** 探测中每 3 秒刷新一次现状（账号行显示「探测中…」时结果来得比 15 秒轮询快）。 */
  // 探测不走 run()，busy 里永远不会出现 probe: 前缀，所以只看 probeRunBusy（审查 O1）。
  const probeBusyActive = !!probeRunBusy;
  useEffect(() => {
    if (!probeBusyActive) return;
    const timer = window.setInterval(() => {
      if (tab === 'probes' || tab === 'accounts') void loadVisible();
    }, PROBE_POLL_MS);
    return () => window.clearInterval(timer);
  }, [probeBusyActive, loadVisible, tab]);

  /** 统一动作包装：busy 标记、错误文案、成功后刷新（保留旧数据不闪）。 */
  const run = useCallback(
    async (
      key: string,
      action: () => Promise<unknown>,
      successText?: string,
      routeError?: (message: string) => void,
    ) => {
      setBusy(key);
      try {
        await action();
        if (successText) setNotice(successText);
        await loadVisible();
      } catch (reason) {
        const message = reason instanceof MonitorAPIError ? reason.message : '操作失败，请稍后重试';
        if (routeError) routeError(message);
        else setError(message);
      } finally {
        setBusy('');
      }
    },
    [loadVisible],
  );

  /**
   * 顶栏「账号续期」：只把已进入续期窗口的账号排进计划。窗口外的账号不排队——
   * 提前轮换 refresh token 可能把还能用的凭证作废，这是各厂商自己的续期窗口
   * 决定的，前端不越权。
   */
  const openRenewPlan = useCallback(() => {
    const now = Date.now();
    const due = accounts
      .filter(
        (account) =>
          account.capabilities.oauth &&
          hasTime(account.expiresAt) &&
          Date.parse(account.expiresAt as string) - now <= RENEW_WINDOW_MS,
      )
      .sort((left, right) => Date.parse(left.expiresAt as string) - Date.parse(right.expiresAt as string))
      .map<RenewRow>((account) => ({
        providerId: account.providerId,
        name: account.name,
        vendor: account.vendor,
        expiresAt: account.expiresAt as string,
        stage: 'idle',
      }));
    setRenewRows(due);
  }, [accounts]);

  const runRenewPlan = useCallback(async () => {
    if (!renewRows || renewRows.length === 0 || renewBusy) return;
    setRenewBusy(true);
    const mark = (providerId: string, patch: Partial<RenewRow>) => {
      setRenewRows((prev) => prev?.map((row) => (row.providerId === providerId ? { ...row, ...patch } : row)) ?? prev);
    };
    let rotated = 0;
    let skipped = 0;
    let failed = 0;
    for (const row of renewRows) {
      mark(row.providerId, { stage: 'running', message: undefined });
      try {
        const result = await renewAccount(row.providerId);
        if (!result.ok) throw new MonitorAPIError(502, `续期失败：${result.errorKind ?? 'unknown'}`);
        if (result.renewed) {
          rotated += 1;
          mark(row.providerId, { stage: 'ok', message: '凭证已轮换' });
        } else {
          skipped += 1;
          mark(row.providerId, { stage: 'skipped', message: '未到厂商续期窗口，凭证保持不变' });
        }
      } catch (reason) {
        failed += 1;
        mark(row.providerId, {
          stage: 'failed',
          message: reason instanceof MonitorAPIError ? reason.message : '请求失败，请稍后重试',
        });
      }
    }
    setRenewBusy(false);
    setNotice(`续期完成：轮换 ${rotated} · 未到窗口 ${skipped} · 失败 ${failed}；结果已写入事件流。`);
    await loadVisible();
  }, [loadVisible, renewBusy, renewRows]);

  /**
   * 账号页的探测点阵：账号现状（consecutiveFailures 等）来自 `/__monitor/probes` 的 accounts，
   * 点阵与耗时来自同一响应的 runs（按时间新→旧）。接口一次只回最近 50 发、且是全部账号混排，
   * 所以点阵显示的是「窗口里实际有记录的发数」，不足 ACCOUNT_DOT_WINDOW 时右侧补灰点，
   * 不拿空位冒充成功；跳过（skipped）的探测不是结论，不进点阵。
   */
  const accountProbeView = useMemo<AccountsProbeView>(() => {
    const runsByAccount = new Map<string, MonitorProbeRun[]>();
    for (const run of probeRuns) {
      if (run.skipped) continue;
      const list = runsByAccount.get(run.providerId);
      if (list) list.push(run);
      else runsByAccount.set(run.providerId, [run]);
    }
    const accountsById = new Map(probeAccounts.map((item) => [item.providerId, item]));
    return {
      windowSize: ACCOUNT_DOT_WINDOW,
      factsOf: (providerId: string) => {
        const account = accountsById.get(providerId);
        // 后端按时间倒序返回；这里再排一次，避免将来换排序时点阵左右颠倒。
        const runs = [...(runsByAccount.get(providerId) ?? [])].sort((a, b) => Date.parse(b.time) - Date.parse(a.time));
        return {
          probed: !!account?.probed,
          consecutiveFailures: account?.consecutiveFailures ?? 0,
          lastProbeAt: account?.lastProbeAt,
          lastStatus: account?.lastStatus,
          recent: runs.slice(0, ACCOUNT_DOT_WINDOW).map((run) => run.ok),
          usedToday: account?.usedToday ?? 0,
          maxPerDay: account?.maxPerDay ?? 0,
          // RTT 只认真正跑过的一发：durationMs=0 是「没计时」，显示成 0ms 会被当成极快。
          rttMs: runs[0] && runs[0].durationMs > 0 ? runs[0].durationMs : undefined,
        };
      },
    };
  }, [probeAccounts, probeRuns]);

  const alertCountOf = useCallback(
    (providerId: string) => alerts.filter((alert) => alert.targetId === providerId && alert.status === 'firing').length,
    [alerts],
  );

  /**
   * 手动探测：两个入口（探测页行内、账号抽屉）共用。结果返回给调用处就地显示，
   * 这里只管 busy 与刷新；返回 null 表示请求本身失败（网络/权限），由调用处给文案。
   */
  const runProbeFor = useCallback(
    async (providerId: string): Promise<MonitorProbeRunResult | null> => {
      const seq = ++probeRunSeq.current;
      setProbeRunBusy(`probe:${providerId}`);
      // 上一次的结果先收起来：新一轮探测还在跑时，抽屉里不该还挂着上一轮「通过」。
      setProbeResult((current) => (current?.providerId === providerId ? null : current));
      try {
        const result = await runProbe(providerId, PROBE_KIND_LIGHT);
        setProbeResult({ providerId, data: result });
        await loadVisible();
        return result;
      } catch (reason) {
        const message = reason instanceof MonitorAPIError ? reason.message : '探测请求失败，请稍后重试';
        setError(message);
        return null;
      } finally {
        if (seq === probeRunSeq.current) setProbeRunBusy('');
      }
    },
    [loadVisible],
  );

  /** 探测记录按账号筛选：同一个 providerId 同时决定深链 ?account=，刷新能还原。 */
  const filterProbes = useCallback(
    (providerId: string) => {
      setSelectedAccount(providerId);
      goTab('probes', { account: providerId });
    },
    [goTab],
  );

  const openAccount = useCallback(
    (providerId: string) => {
      setSelectedAccount(providerId);
      setDrawerNote('');
      setDrawerError('');
      // 抽屉里的结果是「刚刚在抽屉里点出来的」，不能把探测页或别的账号的结果带进来。
      setProbeResult(null);
      goTab('accounts', { account: providerId });
    },
    [goTab],
  );

  const openAlert = useCallback(
    (id: number) => {
      goTab('alerts', { alert: String(id) });
      void fetchAlert(id).then(setAlertDetail).catch(() => setError('告警详情读取失败'));
    },
    [goTab],
  );

  // 深链 ?alert= 直接打开详情（从飞书/钉钉卡片的链接进来就是这个形态）。
  useEffect(() => {
    const raw = paramFromLocation('alert');
    if (!raw || tab !== 'alerts') return;
    const id = Number(raw);
    if (!Number.isFinite(id) || id <= 0) return;
    void fetchAlert(id).then(setAlertDetail).catch(() => setError('告警详情读取失败'));
  }, [tab]);

  const loadMoreAlerts = () => {
    void fetchAlerts({
      status: statusFilter || undefined,
      severity: severityFilter || undefined,
      range: '7d',
      page: Math.floor(alerts.length / ALERT_PAGE) + 1,
      pageSize: ALERT_PAGE,
    })
      .then((page) => {
        alertPagesShownRef.current = Math.floor(alerts.length / ALERT_PAGE) + 1;
        setAlerts((current) => mergeByID(current, page.items));
        setAlertTotal(page.total);
      })
      .catch(() => setError('告警加载失败'));
  };

  const selectTab = (next: TabID) =>
    goTab(next, (next === 'accounts' || next === 'probes') && selectedAccount ? { account: selectedAccount } : {});

  // 顶栏与页签徽标的统计口径：全部来自概览/账号/规则接口，不受告警页筛选影响。
  const accountTotal = overview?.accounts.total ?? accounts.length;
  const p0Count = overview?.firing.p0 ?? alerts.filter((alert) => alert.status === 'firing' && alert.severity === 'p0').length;
  const firingCount = overview?.firing.total ?? alerts.filter((alert) => alert.status === 'firing').length;
  const ruleTotal = rules.length;
  const rulesEnabled = rules.filter((rule) => rule.enabled).length;
  // 副标题「N 个厂商」只数真正有厂商名的账号；厂商名解析不到的单独写成「未标注」，
  // 不把它们并进某个厂商，也不凭空造厂商名。
  const vendorLabels = new Set(accounts.map((item) => item.vendor.trim()).filter(Boolean));
  const vendorless = accounts.filter((item) => !item.vendor.trim()).length;
  const liveProbes = probeAccounts.reduce((sum, item) => sum + item.usedToday, 0);
  const loadedAt = lastLoadedAt ? displayTime(lastLoadedAt, timezone).text : '';
  // 顶栏同步胶囊的「几分钟前」：nowMs 每 30 秒走一次，跨分钟时文案自动更新。
  const loadedAgoSec = lastLoadedAt ? Math.max(0, Math.round((nowMs - Date.parse(lastLoadedAt)) / 1000)) : 0;
  const relativeLoaded = !lastLoadedAt
    ? ''
    : loadedAgoSec < 45 ? '刚刚'
    : loadedAgoSec < 3600 ? `${Math.round(loadedAgoSec / 60)} 分钟前`
    : `${Math.round(loadedAgoSec / 3600)} 小时前`;
  // 完整口径留给悬停提示：用户要看的是「哪一秒」，胶囊上只显示新鲜度。
  const syncHint = lastLoadedAt ? `（${lastLoadedAt}，${timezone}）` : '';

  // 告警静音：直接开关投递配置的 quietEnabled（PUT /__monitor/dispatch 的返回值就是新的完整配置）。
  // 静默时段内 P1 摘要延后、非 P0 通知排队，P0 仍即时发——这是后端 dispatcher 的真实语义。
  const quiet = dispatch?.quietEnabled ?? false;
  const toggleQuiet = () => {
    if (!dispatch) return;
    setError('');
    void run('quiet', async () => setDispatch(await updateDispatch({ quietEnabled: !dispatch.quietEnabled })));
  };
  // 静默窗口按投递时区判断，不在浏览器时区里算：后端就是按 cfg.dispatch.Location() 切的。
  const inQuietWindow = (() => {
    if (!dispatch?.quietEnabled) return false;
    const parse = (raw: string) => {
      const match = /^(\d{1,2}):(\d{2})$/.exec(raw.trim());
      return match ? Number(match[1]) * 60 + Number(match[2]) : null;
    };
    const start = parse(dispatch.quietStart);
    const end = parse(dispatch.quietEnd);
    if (start === null || end === null || start === end) return false;
    let minutes: number;
    try {
      const parts = new Intl.DateTimeFormat('en-GB', {
        timeZone: dispatch.timezone || 'Asia/Shanghai',
        hour: '2-digit',
        minute: '2-digit',
        hourCycle: 'h23',
      }).formatToParts(new Date());
      const hour = Number(parts.find((part) => part.type === 'hour')?.value ?? '0');
      const minute = Number(parts.find((part) => part.type === 'minute')?.value ?? '0');
      minutes = hour * 60 + minute;
    } catch {
      return false;
    }
    return start < end ? minutes >= start && minutes < end : minutes >= start || minutes < end;
  })();
  const quietWindowText = dispatch
    ? `${timezone} ${dispatch.quietStart || '—'}–${dispatch.quietEnd || '—'}`
    : '读取中';
  const quietTitle = dispatch
    ? !quiet
      ? `告警静音未开启：所有级别通知即时投递。点击开启后，${quietWindowText} 内非 P0 通知延后、P1 进摘要（P0 始终即时）。`
      : inQuietWindow
        ? `已静音，当前正处静默时段（${quietWindowText}）：非 P0 通知延后，P0 仍即时。点击关闭。`
        : `已静音，静默时段（${quietWindowText}）之外的通知照常即时投递，所以现在不会延后。点击关闭。`
    : '告警静音状态读取中';

  // 状态胶囊：采集器读不到 / 实时数据读失败 = 异常；采集器成功时间过期 = 告警；
  // 其余按 P0 严重告警切换「运行中 / 需要关注」。
  const collector = collectorState(overview?.collectorLastSuccess);
  const chainState =
    error || collector === 'down'
      ? { tone: 'danger' as const, text: error ? '监控链路数据读取失败' : '监控采集器已停止上报' }
      : collector === 'stale'
        ? { tone: 'warn' as const, text: '监控采集器数据滞后' }
        : p0Count > 0
          ? { tone: 'warn' as const, text: `监控链路运行中 · ${p0Count} 条 P0 告警` }
          : { tone: 'ok' as const, text: '监控链路运行中' };

  // 徽标：探测页看「有没有实时数据在来」，规则看已启用条数，告警只看未处理。
  const probeStale = !overview || !hasTime(overview.collectorLastSuccess) || Date.now() - Date.parse(overview.collectorLastSuccess) > STALE_MS;
  const badges: Record<TabID, { tone: BadgeTone; text: string } | null> = {
    overview: null,
    accounts: { tone: 'plain', text: `${accountTotal} 节点` },
    alerts: firingCount > 0 ? { tone: 'amber', text: String(firingCount) } : null,
    probes: tab === 'probes'
      ? probeStreamMode === 'live' ? { tone: 'live', text: 'LIVE' } : { tone: 'paused', text: 'PAUSED' }
      : probeStale ? { tone: 'paused', text: 'PAUSED' } : { tone: 'live', text: 'LIVE' },
    rules: ruleTotal > 0 ? { tone: 'plain', text: `${rulesEnabled} 条` } : null,
    settings: null,
  };

  return (
    <div className={`monitoring-page${tab === 'probes' ? ' mon-stream-fill' : ''}`}>
      {/* 1. 顶栏：徽章 + 标题（右挂同步时间）+ 状态胶囊 ｜ 业务摘要 ｜ 动作按钮组；与页签栏用 1px 边框分隔。
          参考 `MonitoringCenter.tsx:520-618`：第一行标题+胶囊，第二行业务摘要，时间由 tooltip 承载。 */}
      <header className="mon-header">
        <div className="mon-header-id">
          <span className="mon-header-badge" aria-hidden="true">
            <span className="mon-header-badge-glyph"><NavIcon id="monitoring" /></span>
          </span>
          <div className="mon-header-text">
            <div className="mon-header-headline">
              <h1 className="mon-header-title">协议网关监控中心</h1>
              <span
                className={`mon-chain mon-chain-${chainState.tone}`}
                role="status"
                aria-live="polite"
                title={`采集器最近一次成功上报：${hasTime(overview?.collectorLastSuccess) ? displayTime(overview!.collectorLastSuccess as string, timezone).text : '无记录'} · 页面数据生成于 ${loadedAt || '—'}`}
              >
                <i className="mon-chain-dot" aria-hidden="true" />
                <span className="mon-chain-text">{chainState.text}</span>
              </span>
              {/* 读失败时不能还显示「刚刚同步」：页面上的数字可能已经过期，胶囊要说清
                  最后一次成功是什么时候，而不是继续报告新鲜度。 */}
              <span className={`mon-header-sync${error && !loading ? ' is-stale' : ''}`} title={loadedAt ? `${loadedAt} 同步${syncHint}` : undefined}>
                <i className="live-dot" aria-hidden="true" />
                {loading ? '同步中…' : error ? (loadedAt ? `数据过期 · 最后成功 ${relativeLoaded}` : '读取失败') : loadedAt ? `${relativeLoaded} 同步` : '等待首轮同步'}
              </span>
            </div>
            <p
              className="mon-header-sub"
              title={`覆盖 ${accountTotal} 个账号（${vendorLabels.size} 个厂商${vendorless > 0 ? `，另 ${vendorless} 个未标注` : ''}）· ${firingCount} 条未处理告警 · ${rulesEnabled}/${ruleTotal} 条规则已启用 · 今日探测 ${liveProbes} 发`}
            >
              {/* 副标题只留三段（对齐参考的「覆盖 20 容灾节点 · 熔断防御已就绪 · 实时流式遥测中」）。
                  原先五段加起来 432px，把 Header 挤成两行（1280 下 116px 高）；其余口径进 title。 */}
              <span>{`覆盖 ${accountTotal} 个账号`}</span>
              <span className="mon-dot" aria-hidden="true">·</span>
              <span>{firingCount > 0 ? `${firingCount} 条未处理告警` : '暂无未处理告警'}</span>
              <span className="mon-dot" aria-hidden="true">·</span>
              <span>{`${rulesEnabled}/${ruleTotal} 条规则已启用`}</span>
            </p>
          </div>
        </div>
        <div className="mon-header-btns">
          <button
            type="button"
            className={`mon-header-btn mon-header-mute${quiet ? ' is-on' : ''}`}
            aria-pressed={quiet}
            onClick={toggleQuiet}
            disabled={!dispatch || busy === 'quiet'}
            title={quietTitle}
          >
            <HeaderActionIcon id="mute" />
            <span>{busy === 'quiet' ? '保存中…' : quiet ? (inQuietWindow ? '已静音' : '静音中') : '告警静音'}</span>
          </button>
          <button type="button" className="mon-header-btn mon-header-refresh" onClick={() => void loadVisible()} disabled={loading}>
            <HeaderActionIcon id="refresh" />
            <span>{loading ? '刷新中' : '刷新指标'}</span>
          </button>
          <button type="button" className="mon-header-btn is-green" onClick={openRenewPlan}>
            <HeaderActionIcon id="renew" />
            <span>账号续期</span>
          </button>
          <a className="mon-header-btn is-accent" href={navPathForID('api-keys')} onClick={(event) => {
            // 键盘/修饰键点击留给浏览器，普通点击走 SPA 路由，整页不重载。
            if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
            event.preventDefault();
            window.history.pushState({ nav: 'api-keys' }, '', navPathForID('api-keys'));
            window.dispatchEvent(new PopStateEvent('popstate', { state: { nav: 'api-keys' } }));
          }}>
            <HeaderActionIcon id="key" />
            <span>管理 API 密钥</span>
          </a>
        </div>
      </header>

      {notice && tab !== 'settings' && (
        <div className="mon-notice" role="status" aria-live="polite">
          <span>{notice}</span>
          <button type="button" className="mon-notice-close" onClick={() => setNotice('')} aria-label="关闭提示">
            ×
          </button>
        </div>
      )}

      {/* 2. 子页签：图标 + 文字 + 数量徽标；激活 = 底部 2px 强调线 + 强调色文字。整条横向可滚。 */}
      <div
        ref={tabsRef}
        className={`mon-tabs${tabsScroll.start ? ' has-fade-start' : ''}${tabsScroll.end ? ' has-fade-end' : ''}`}
        role="tablist"
        aria-label="监控中心"
      >
        <div className="mon-tabs-track">
          {TABS.map((item) => {
            const active = tab === item.id;
            const badge = badges[item.id];
            return (
              <button
                key={item.id}
                type="button"
                role="tab"
                id={`mon-tab-${item.id}`}
                aria-selected={active}
                aria-controls="mon-tab-panel"
                className={`mon-tab${active ? ' active' : ''}`}
                onClick={() => selectTab(item.id)}
              >
                <span className="mon-tab-icon" aria-hidden="true"><NavIcon id={item.icon as NavItemID} /></span>
                <span className="mon-tab-label">{item.label}</span>
                {badge && <TabBadge tone={badge.tone}>{badge.text}</TabBadge>}
              </button>
            );
          })}
        </div>
      </div>

      <div id="mon-tab-panel" role="tabpanel" aria-labelledby={`mon-tab-${tab}`} className="mon-tab-panel">

      {tab === 'overview' && (
        <OverviewTab
          overview={overview}
          accounts={accounts}
          timezone={timezone}
          loading={loading}
          error={error}
          iconOf={iconOf}
          onOpenAccount={openAccount}
          onOpenTab={(next) => goTab(next, {})}
          probeRuns={probeRuns}
        />
      )}

      {tab === 'accounts' && (
        <AccountsTab
          accounts={accounts}
          timezone={timezone}
          iconOf={iconOf}
          alertCountOf={alertCountOf}
          busy={busy}
          error={error}
          selectedId={selectedAccount}
          onSelect={openAccount}
          note={drawerNote}
          actionError={drawerError}
          probeView={accountProbeView}
          probeConfig={probeConfig}
          probeBusy={probeRunBusy}
          onRunProbe={(providerId) => {
            setDrawerError('');
            // 真实原因已由 runProbeFor 写到面板顶部错误行，这里只补一句「在哪没成」。
            void runProbeFor(providerId).then((result) => {
              if (!result) setDrawerError('探测没有发出：账号或上游已不可用，原因见页面顶部提示');
            });
          }}
          onCloseDrawer={() => {
            setSelectedAccount('');
            setDrawerNote('');
            setDrawerError('');
            // 关掉就作废旧结果：重开同一账号时不该「复活」上一次的探测结论。
            setProbeResult(null);
            goTab('accounts', {});
          }}
          onRenew={(providerId) => {
            void run(
              `renew:${providerId}`,
              async () => {
                const result = await renewAccount(providerId);
                if (!result.ok) throw new MonitorAPIError(502, `续期失败：${result.errorKind ?? 'unknown'}`);
                setDrawerNote(result.renewed ? '凭证已轮换，已写入事件流' : '未到续期窗口，凭证保持不变（已记录到事件流）');
              },
              undefined,
              setDrawerError,
            );
          }}
          onRefreshQuota={(providerId) => {
            void run(
              `quota:${providerId}`,
              async () => {
                const result = await refreshQuota(providerId);
                if (!result.ok) throw new MonitorAPIError(502, `额度读取失败：${result.errorKind ?? 'unknown'}`);
                setDrawerNote('额度已刷新');
              },
              undefined,
              setDrawerError,
            );
          }}
          onOpenAlerts={(providerId) => {
            goTab('alerts', { target: providerId });
            setSelectedAccount('');
            void fetchAlerts({ targetId: providerId, status: statusFilter || undefined, range: '7d' })
              .then((page) => {
                setAlerts(page.items);
                setAlertTotal(page.total);
              })
              .catch(() => setError('告警读取失败'));
          }}
          onOpenLogs={onOpenAccountLogs ?? (() => undefined)}
          onIsolationChange={(result) => {
            // 隔离 / 恢复立即反映到卡片与概览计数，不等下一轮轮询；后台再整体刷新一次兜底。
            const next = result.account;
            setAccounts((prev) => prev.map((account) => (account.providerId === next.providerId ? next : account)));
            void loadVisible();
          }}
        />
      )}

      {tab === 'probes' && (
        <ProbesTab
          probeAccounts={probeAccounts}
          providerFilter={probeFilter}
          timezone={timezone}
          busy={probeRunBusy}
          error={error}
          iconOf={iconOf}
          probeResult={
            probeResult && probeAccounts.some((item) => item.providerId === probeResult.providerId) ? probeResult : null
          }
          onDismissResult={() => setProbeResult(null)}
          onRunProbe={runProbeFor}
          onFilter={filterProbes}
          onModeChange={setProbeStreamMode}
        />
      )}

      {tab === 'alerts' && (
        <AlertsTab
          alerts={alerts}
          total={alertTotal}
          firingTotal={alertCounts.firing}
          resolvedTotal={alertCounts.resolved}
          statusFilter={statusFilter}
          severityFilter={severityFilter}
          timezone={timezone}
          channels={channels}
          detail={alertDetail}
          busy={busy}
          error={error}
          onStatusFilter={(value) => {
            setStatusFilter(value);
            setAlertDetail(null);
          }}
          onSeverityFilter={setSeverityFilter}
          onOpen={openAlert}
          onClose={() => {
            setAlertDetail(null);
            goTab('alerts', {});
          }}
          onAck={(id) => {
            void run(`ack:${id}`, async () => setAlertDetail(await acknowledgeAlert(id)));
          }}
          onIgnore={(id) => {
            void run(`ignore:${id}`, async () => setAlertDetail(await ignoreAlert(id)));
          }}
          onRetryPush={(eventId, channelId) => {
            void run(`retry:${eventId}`, () => retryPush(eventId, channelId), '重试推送已提交');
          }}
          onOpenAccount={openAccount}
          onLoadMore={loadMoreAlerts}
        />
      )}

      {tab === 'rules' && (
        <RulesTab
          onNotice={(text) => setRulesNotice(text)}
          error={error}
          timezone={timezone}
          notice={tab === 'rules' ? rulesNotice : ''}
        />
      )}

      {tab === 'settings' && (
        <SettingsTab
          channels={channels}
          dispatch={dispatch}
          busy={busy}
          error={settingsError}
          notice={notice}
          onCreateChannel={(input: MonitorChannelInput) => {
            void run('channel:create', () => createChannel(input), '渠道已创建', setSettingsError);
          }}
          onUpdateChannel={(id, patch) => {
            void run('channel:update', () => updateChannel(id, patch), '渠道已保存', setSettingsError);
          }}
          onDeleteChannel={(id) => {
            void run(`del:${id}`, () => deleteChannel(id), '渠道已删除', setSettingsError);
          }}
          onTestChannel={(id) => {
            void run(
              `test:${id}`,
              async () => {
                const result = await testChannel(id);
                if (!result.ok) throw new MonitorAPIError(502, result.error || '渠道测试失败');
              },
              '测试消息已发送，未收到请检查渠道关键词或签名',
              setSettingsError,
            );
          }}
          onUpdateDispatch={(patch) => {
            void run('dispatch', async () => setDispatch(await updateDispatch(patch)), '投递设置已保存', setSettingsError);
          }}
          onDismissNotice={() => setNotice('')}
          probeConfig={probeConfig}
          probeDraft={probeDraft}
          probeBusy={busy === 'probe-config'}
          probeError={probeSettingsError}
          probeNotice={probeSettingsNotice}
          onProbeDraft={setProbeDraft}
          onSaveProbeConfig={(config) => {
            setProbeSettingsError('');
            setProbeSettingsNotice('');
            void run(
              'probe-config',
              async () => {
                const saved = await saveProbeConfig(config);
                setProbeConfig(saved);
                setProbeDraft(saved);
                setProbeSettingsNotice('探测配置已保存，下一轮调度生效');
              },
              undefined,
              setProbeSettingsError,
            );
          }}
        />
      )}
      </div>

      {renewRows && (
        <Modal
          title="账号续期"
          description="逐项续期登录凭证（OAuth 令牌）；积分或额度批次到期属于上游账期，续期不会延长。"
          onClose={() => {
            if (!renewBusy) setRenewRows(null);
          }}
        >
          <p className="mon-renew-lead">
            {renewRows.length === 0
              ? '当前没有 24 小时内到期的登录凭证，没有需要续期的账号。单个账号仍可在「集群容灾与 SLA 矩阵」里手动续期。'
              : `以下 ${renewRows.length} 个账号的登录凭证将在 24 小时内到期（或已过期），逐个续期：`}
          </p>
          {renewRows.length > 0 && (
            <ul className="mon-renew-list">
              {renewRows.map((row) => (
                <li key={row.providerId} className={`mon-renew-row is-${row.stage}`}>
                  <div className="mon-renew-id">
                    <strong>{row.name}</strong>
                    <span>{row.vendor || row.providerId}</span>
                  </div>
                  <div className="mon-renew-when" title={displayTime(row.expiresAt, timezone).title}>
                    {`凭证 ${relativeToNow(row.expiresAt, Date.now())}`}
                  </div>
                  <div className="mon-renew-state">
                    {row.message ?? (row.stage === 'running' ? '续期中…' : '等待')}
                  </div>
                </li>
              ))}
            </ul>
          )}
          <div className="mon-detail-actions">
            <button type="button" className="btn btn-sm" onClick={() => setRenewRows(null)} disabled={renewBusy}>
              {'关闭'}
            </button>
            {renewRows.some((row) => row.stage === 'idle') && (
              <button
                type="button"
                className="btn btn-sm primary"
                onClick={() => void runRenewPlan()}
                disabled={renewBusy}
              >
                {renewBusy ? '续期中…' : `开始续期（${renewRows.filter((row) => row.stage === 'idle').length}）`}
              </button>
            )}
          </div>
        </Modal>
      )}
    </div>
  );
}

/**
 * 采集器心跳：超过 3 分钟没成功算滞后，超过 10 分钟算停了。
 * 阈值放在前端只是为了给顶栏状态胶囊一个判断，不参与任何告警计算。
 */
const STALE_MS = 3 * 60_000;
const DOWN_MS = 10 * 60_000;

function collectorState(raw: string | undefined): 'ok' | 'stale' | 'down' {
  if (!hasTime(raw)) return 'down';
  const age = Date.now() - Date.parse(raw as string);
  if (age > DOWN_MS) return 'down';
  if (age > STALE_MS) return 'stale';
  return 'ok';
}

/**
 * 瞬时失败重试：网关重启、反代 502、连接被重置都在几百毫秒到一两秒内自愈。
 * 鉴权失败重试没有意义，直接抛出；其余按 FETCH_RETRY_DELAYS 退避重试。
 */
async function fetchWithRetry<T>(task: () => Promise<T>): Promise<T> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      return await task();
    } catch (reason) {
      if (attempt >= FETCH_RETRY_DELAYS.length) throw reason;
      if (reason instanceof MonitorAPIError && (reason.status === 401 || reason.status === 403)) throw reason;
      await new Promise((resolve) => window.setTimeout(resolve, FETCH_RETRY_DELAYS[attempt]));
    }
  }
}

/** 分页合并：轮询可能插进新行，按 id 去重才不会重复渲染。 */
function mergeByID<T extends { id: number }>(current: T[], incoming: T[]): T[] {  const merged = [...current];
  for (const item of incoming) {
    if (!merged.some((existing) => existing.id === item.id)) merged.push(item);
  }
  return merged;
}
