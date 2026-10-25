import {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import type {ReactNode} from 'react';
import {NavIcon} from '../components/ui';
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
    MonitorEventKind,
    MonitorOverview,
    MonitorProbeAccount,
    MonitorProbeConfig,
    MonitorProbeRun,
    MonitorProbeRunResult,
    MonitorRule,
    MonitorSeverity,
} from './types';
import {hasTime} from './types';
import {displayTime} from './format';
import {AccountsTab} from './AccountsTab';
import type {AccountsProbeView} from './AccountsTab';
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
  { id: 'accounts', label: '账号', icon: 'input-providers' },
  { id: 'alerts', label: '告警', icon: 'quota-keeper' },
  { id: 'probes', label: '探测', icon: 'traffic-tokens' },
  { id: 'rules', label: '规则', icon: 'output-providers' },
  { id: 'settings', label: '设置', icon: 'settings' },
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

const POLL_MS = 15_000;
/**
 * 单发轻探测的后端上限是 30 秒，期间「正在探测」的按钮要一直亮着；
 * 这个节拍只比轮询密，别让它比探测本身先停。
 */
const PROBE_POLL_MS = 3_000;
/** 账号卡片点阵保留的探测发数（与 /__monitor/probes 单次返回的 50 发上限配套）。 */
const ACCOUNT_DOT_WINDOW = 10;
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
  const [eventTotal, setEventTotal] = useState(0);
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
  const [kindFilter, setKindFilter] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [settingsError, setSettingsError] = useState('');
  // 规则页的保存只写规则；成功/失败都由页面内部提示，失败更不刷新（否则响应会把编辑冲掉）。
  const [rulesNotice, setRulesNotice] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState('');
  // 抽屉里的动作要就地给反馈：账号页在 Modal 打开时，面板顶部的错误行会被抽屉挡住。
  const [drawerNote, setDrawerNote] = useState('');
  const [drawerError, setDrawerError] = useState('');
  // 顶栏副标题与「规则」页签徽标要的是全量口径：告警列表可能被状态/级别筛选过，不能拿来数。
  const [rules, setRules] = useState<MonitorRule[]>([]);
  const [lastLoadedAt, setLastLoadedAt] = useState('');

  const generation = useRef(0);
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
        fetchOverview(),
        fetchAccounts(),
        fetchAlerts({
          status: statusFilter || undefined,
          severity: severityFilter || undefined,
          range: '7d',
          pageSize: ALERT_PAGE,
        }),
        fetchEvents({
          kind: kindFilter || undefined,
          severity: severityFilter || undefined,
          range: '24h',
          pageSize: EVENT_PAGE,
        }),
        // 告警详情里的「重试推送」要选渠道，所以告警页也要拿一次渠道（脱敏视图）。
        tab === 'settings' || tab === 'alerts' ? fetchChannels() : Promise.resolve(null),
        dispatch ? Promise.resolve(null) : fetchDispatch(),
        probesPromise,
        // 规则表只给顶栏副标题和页签计数用；读失败不影响页面，等下一轮再试。
        fetchRules().catch(() => null),
      ]);
      if (mine !== generation.current) return;
      setOverview(nextOverview);
      setAccounts(nextAccounts.accounts);
      setAlerts(nextAlerts.items);
      setAlertTotal(nextAlerts.total);
      setEvents(nextEvents.items);
      setEventTotal(nextEvents.total);
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
      setError('');
    } catch (reason) {
      if (mine !== generation.current) return;
      setError(reason instanceof MonitorAPIError ? reason.message : '监控数据读取失败');
    } finally {
      if (mine === generation.current) setLoading(false);
    }
  }, [dispatch, kindFilter, probeFilter, severityFilter, statusFilter, tab]);

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

  const loadMoreEvents = () => {
    void fetchEvents({
      kind: kindFilter || undefined,
      severity: severityFilter || undefined,
      range: '24h',
      page: Math.floor(events.length / EVENT_PAGE) + 1,
      pageSize: EVENT_PAGE,
    })
      .then((page) => {
        setEvents((current) => mergeByID(current, page.items));
        setEventTotal(page.total);
      })
      .catch(() => setError('事件加载失败'));
  };

  const loadMoreAlerts = () => {
    void fetchAlerts({
      status: statusFilter || undefined,
      severity: severityFilter || undefined,
      range: '7d',
      page: Math.floor(alerts.length / ALERT_PAGE) + 1,
      pageSize: ALERT_PAGE,
    })
      .then((page) => {
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
    probes: probeStale ? { tone: 'paused', text: 'PAUSED' } : { tone: 'live', text: 'LIVE' },
    rules: ruleTotal > 0 ? { tone: 'plain', text: `${rulesEnabled} 条` } : null,
    settings: null,
  };

  return (
    <div className="monitoring-page">
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
              <span className="mon-header-sync">
                <i className="live-dot" aria-hidden="true" />
                {loading ? '同步中…' : loadedAt ? `${loadedAt} 同步` : '等待首轮同步'}
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
          <button type="button" className="mon-header-btn is-green" onClick={() => selectTab('accounts')}>
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
          alerts={alerts}
          alertTotal={alertTotal}
          events={events}
          eventTotal={eventTotal}
          kindFilter={kindFilter}
          severityFilter={severityFilter}
          timezone={timezone}
          iconOf={iconOf}
          loading={loading}
          error={error}
          onKindFilter={setKindFilter}
          onSeverityFilter={setSeverityFilter}
          onLoadMoreEvents={loadMoreEvents}
          onOpenAccount={openAccount}
          onOpenAlert={openAlert}
          onOpenTab={(next) => goTab(next, {})}
          onOpenAlerts={() => goTab('alerts', {})}
          probeRuns={probeRuns}
          onRetry={() => void loadVisible()}
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
                setDrawerNote(result.renewed ? '凭证已轮换' : '未到续期窗口，凭证保持不变');
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
        />
      )}

      {tab === 'probes' && (
        <ProbesTab
          probeAccounts={probeAccounts}
          probeRuns={probeRuns}
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
          onRefreshRuns={() => void loadVisible()}
          onClearRuns={() => {
            probeRunsClearedRef.current = true;
            setProbeRuns([]);
          }}
          onFilter={filterProbes}
          onOpenAccount={openAccount}
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

/** 分页合并：轮询可能插进新行，按 id 去重才不会重复渲染。 */
function mergeByID<T extends { id: number }>(current: T[], incoming: T[]): T[] {
  const merged = [...current];
  for (const item of incoming) {
    if (!merged.some((existing) => existing.id === item.id)) merged.push(item);
  }
  return merged;
}
