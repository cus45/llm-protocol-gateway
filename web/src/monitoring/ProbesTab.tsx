import {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import {BrandIcon, Modal, Switch} from '../components/ui';
import {API_BASE} from '../lib';
import type {
  MonitorProbeAccount, MonitorProbeConfig, MonitorProbeRun, MonitorProbeRunResult, MonitorProbeStreamMode,
} from './types';
import {ERROR_KIND_TEXT, normalizeProbeCase, probeCaseText, probeKindText, probeSkipText} from './types';
import {displayTime, displayTimeCompact, formatLatency, relativeToNow} from './format';
import {
  MONITOR_STREAM_EVENTS, MONITOR_STREAM_PATH, fetchProbes, parseProbeAccount, parseProbePage, parseProbeRun,
  parseStreamProbeFrame,
} from './api';
import type {MonitorStreamEventName} from './api';

/** 已上线的探测种类：只有轻探测。后端对其余 kind 回 400 kind_unavailable。 */
export const PROBE_KIND_LIGHT = 'light';

/**
 * SSE 不可用时的轮询节奏。也用于「超过这么久没收到任何帧就判流断了」：
 * 后端每 15 秒发一次 `:heartbeat` 注释行，45 秒静默只能是连接假死。
 */
const POLL_FALLBACK_MS = 15_000;
const STREAM_DEAD_MS = 45_000;

/** 新条目入场高亮的时长，与 styles.css 里 .mon-tl-item.fresh 的动画一致。 */
const FRESH_MS = 1600;

/** 本地列表上限：流是长期连接，不封顶的话几个小时后 DOM 会拖垮页面。 */
const RUN_CAP = 200;

/** 速率窗口：最近 5 秒收到的记录条数 ÷ 5。 */
const RATE_WINDOW_MS = 5_000;

/** 未收到 snapshot 的宽限时长：超时说明对面不是 SSE（老后端 404、反代吐 HTML），直接降级。 */
const SNAPSHOT_GRACE_MS = 8_000;

/**
 * 测完 SSE 要把它变回「老后端」时设成非 0 的路径（浏览器控制台执行一次即可），测完设回 0。
 * 0 = 正常连接 /__monitor/stream。
 */
const STREAM_PATH_OVERRIDE_KEY = '__monitorStreamPathOverride';

function streamUrl(): string {
  const override = readStreamOverride();
  return `${API_BASE}${override || MONITOR_STREAM_PATH}`;
}

function readStreamOverride(): string {
  try {
    const raw = (globalThis as Record<string, unknown>)[STREAM_PATH_OVERRIDE_KEY];
    return typeof raw === 'string' ? raw : '';
  } catch {
    return '';
  }
}

/** 速率读数：整数保留一位小数（14.3 条/秒），零显示 0。 */
function rateText(count: number): string {
  const rate = count / (RATE_WINDOW_MS / 1000);
  const rounded = Math.round(rate * 10) / 10;
  return Number.isInteger(rounded) ? String(rounded) : rounded.toFixed(1);
}

/** 记录行的稳定键。同一轮探测里 case 唯一，时间可能同毫秒，所以用「时间去毫秒 + 账号 + case」。 */
function runKey(row: MonitorProbeRun): string {
  const stamp = Date.parse(row.time);
  const label = Number.isNaN(stamp) ? row.time : String(Math.floor(stamp / 1000));
  return `${row.providerId}|${row.case}|${label}`;
}

/** 合并记录：按 runKey 去重、时间新→旧；同一发探测的两次快照以后到的为准。 */
function mergeRuns(base: MonitorProbeRun[], incoming: MonitorProbeRun[]): { rows: MonitorProbeRun[]; added: string[] } {
  const byKey = new Map<string, MonitorProbeRun>();
  for (const row of base) byKey.set(runKey(row), row);
  const added: string[] = [];
  for (const row of incoming) {
    const key = runKey(row);
    if (!byKey.has(key)) added.push(key);
    byKey.set(key, row);
  }
  const rows = [...byKey.values()].sort((a, b) => Date.parse(b.time) - Date.parse(a.time));
  return { rows: rows.slice(0, RUN_CAP), added };
}

/** 「列表会不会变」：同一批键、同一顺序就算没变，流里重复帧不引起重渲染。 */
function sameRuns(a: MonitorProbeRun[], b: MonitorProbeRun[]): boolean {
  if (a.length !== b.length) return false;
  for (let index = 0; index < a.length; index += 1) {
    if (runKey(a[index]) !== runKey(b[index])) return false;
  }
  return true;
}

/** 账号现状合并：流里的快照可能不带某个账号，缺的保持原值（不把已有的行抹掉）。 */
function mergeAccounts(base: MonitorProbeAccount[], incoming: MonitorProbeAccount[]): MonitorProbeAccount[] {
  if (incoming.length === 0) return base;
  const byId = new Map(incoming.map((item) => [item.providerId, item]));
  const merged = base.map((item) => byId.get(item.providerId) ?? item);
  const known = new Set(base.map((item) => item.providerId));
  for (const item of incoming) if (!known.has(item.providerId)) merged.push(item);
  return merged;
}

export interface ProbesTabProps {
  probeAccounts: MonitorProbeAccount[];
  probeRuns: MonitorProbeRun[];
  /** 探测记录按账号筛选（深链 ?account= 与记录里的账号按钮都走这里）；空 = 全部。 */
  providerFilter: string;
  timezone: string;
  busy: string;
  error: string;
  /** 品牌图标键由外层提供（见 AccountsTab 同名说明）。 */
  iconOf?: (providerId: string) => string | undefined;
  /** 结果由外层按 providerId 存，轮询换新数据时弹层不会被刷掉。 */
  probeResult: { providerId: string; data: MonitorProbeRunResult } | null;
  onDismissResult: () => void;
  onRunProbe: (providerId: string) => Promise<MonitorProbeRunResult | null>;
  /** 探测结束后再取一次记录列表（探测行可能比第一次刷新晚落库）。 */
  onRefreshRuns: () => void;
  /** 清空本地记录时同步清掉外层底稿（否则下一轮轮询会把旧记录带回来）。 */
  onClearRuns: () => void;
  onFilter: (providerId: string) => void;
  onOpenAccount: (providerId: string) => void;
}

type ProbeTone = 'ok' | 'err' | 'skip' | 'none';

/** 行状态只判「跑成什么」：跳过与从未探测都不是失败。 */
function outcomeOf(account: MonitorProbeAccount): { tone: ProbeTone; status: string; skip: string } {
  if (!account.probed) {
    const skip = probeSkipText(account.skipped);
    return skip ? { tone: 'skip', status: '未成功执行', skip } : { tone: 'none', status: '暂无数据', skip: '' };
  }
  if (account.consecutiveFailures > 0) {
    const auth = account.lastStatus === 401 || account.lastStatus === 403;
    return { tone: 'err', status: `连续失败 ${account.consecutiveFailures} 次`, skip: auth ? '最近一次认证失败' : '' };
  }
  return { tone: 'ok', status: '通过', skip: '' };
}

function statusCodeText(status: number | undefined): string {
  if (status === undefined || status === 0) return '—';
  return String(status);
}

/** 状态码文案：「最近状态」是服务端记录里最新那一发的状态，和连续失败次数不是一回事。 */
function lastStatusText(status: number | undefined): string {
  if (status === undefined || status === 0) return '最近状态 —';
  return `最近状态 ${status}`;
}

/** 当日次数：maxPerDay=0 是「不限」，不能显示成「0 次」。 */
function dailyText(account: MonitorProbeAccount): string {
  return account.maxPerDay === 0 ? `${account.usedToday} / 不限` : `${account.usedToday} / ${account.maxPerDay}`;
}

// ── 探测记录（时间轴）────────────────────────────────────────────────────

/**
 * 一个分类胶囊。计数口径与记录行的三种结果一一对应：
 * 跳过（skipped 非空，如 quota_spent / no_model）既不算成功也不算失败。
 */
type RunCategory = 'all' | 'ok' | 'fail' | 'skip';

/** 普通字段里的空值不显示成 `undefined` / `0`；状态 0 是传输层失败，单独写 network。 */
function runField(value: string | number | undefined): string {
  if (value === undefined || value === '') return '—';
  return String(value);
}

/**
 * 错误码的核心原因。后端把 Go 的传输层错误整段存进来（`Post "https://…": context canceled`），
 * 整条 URL 塞进行内没有信息量：只取最后一个冒号之后的原因，完整原文留给悬停。
 */
function errorReason(errorCode: string): string {
  // 已知错误类别（network / auth_failed / rate_limited）本身就是结论，不是原始报文：
  // 直接映射成中文，别把 "network" 原样丢给用户。
  const known = (ERROR_KIND_TEXT as Record<string, string>)[errorCode];
  if (known) return known;
  const at = errorCode.lastIndexOf(': ');
  const tail = at >= 0 ? errorCode.slice(at + 2).trim() : errorCode;
  return tail || errorCode;
}

/** 状态码：0 是传输层失败（请求没走到上游），读成「无响应」比「0」有信息量。 */
function runStatusText(status: number): string {
  return status === 0 ? '无响应' : String(status);
}

/**
 * 一行记录等宽 payload 的原文分段，窄处按 · 逐段折行，不整块溢出。
 * 状态码在 payload 里保留原始数字（`status=0`），「无响应」的释义只给上面的指标格。
 */
function payloadSegments(row: MonitorProbeRun): string[] {
  const duration = row.durationMs > 0 ? `${row.durationMs}ms` : '—';
  return [
    `case=${runField(row.case)}`,
    `ok=${row.ok}`,
    `status=${runField(row.status)}`,
    duration,
    row.model ? `model=${row.model}` : '',
    row.errorCode ? `error=${errorReason(row.errorCode)}` : '',
    row.skipped ? `skipped=${row.skipped}` : '',
  ].filter(Boolean);
}

/** 四列指标格：值都来自 run 本身，空值写「—」，不补猜。 */
function runMetrics(row: MonitorProbeRun): Array<{ label: string; value: string; tone?: 'err' | 'muted' }> {
  const unmapped = isUnmappedKind(row.kind);
  return [
    {
      label: '状态码',
      value: unmapped ? '—' : runStatusText(row.status),
      tone: !unmapped && row.status === 0 ? 'err' : undefined,
    },
    { label: '耗时', value: row.durationMs > 0 ? formatLatency(row.durationMs) : '—' },
    { label: '模型', value: row.model || '—', tone: row.model ? undefined : 'muted' },
    {
      label: '错误码',
      value: row.errorCode ? errorReason(row.errorCode) : '—',
      tone: row.errorCode ? 'err' : 'muted',
    },
  ];
}

/** 已下线的深检（CLI）沿旧字段名取值：库里老行的 status/error_code 不是这层语义，不硬套成「无响应」。 */
function isUnmappedKind(kind: string): boolean {
  return kind !== PROBE_KIND_LIGHT && kind !== '';
}

/** kind 徽标的底色分档：轻探测沿用主色，历史深检用紫色，和事件流里按类型分色是一个意思。 */
function kindTone(kind: string): string {
  return kind === PROBE_KIND_LIGHT ? 'light' : 'legacy';
}

export function ProbesTab(props: ProbesTabProps) {
  const {
    probeAccounts: outerAccounts, probeRuns: outerRuns, providerFilter, timezone, busy, error, iconOf,
    probeResult, onDismissResult, onRunProbe, onFilter, onOpenAccount, onRefreshRuns, onClearRuns,
  } = props;

  // ── 实时流 ────────────────────────────────────────────────────────────
  /*
   * 数据分三层：外层（MonitoringPage）轮询来的底稿、流的快照、流里逐发追加的记录。
   * 渲染一律用合并后的 views：流断了退回底稿；流活着时新记录立即出现。
   */
  const [streamRuns, setStreamRuns] = useState<MonitorProbeRun[]>([]);
  const [streamAccounts, setStreamAccounts] = useState<MonitorProbeAccount[]>([]);
  const [mode, setMode] = useState<MonitorProbeStreamMode>('live');
  const [streamNote, setStreamNote] = useState('');
  // 自增即重建 EventSource（轮询降级后点「重试实时流」）。
  const [attempt, setAttempt] = useState(0);
  // 速率窗口里的到达时刻：只用来每秒算一次速率，不参与渲染数据。
  const [rateCount, setRateCount] = useState(0);
  const [paused, setPaused] = useState(false);
  const [backlogCount, setBacklogCount] = useState(0);
  const [tickNow, setTickNow] = useState(() => Date.now());
  const [freshKeys, setFreshKeys] = useState<ReadonlySet<string>>(() => new Set());
  // 置顶锁定：新条目从顶部插入并滚到顶；取消后可以自由往下翻。
  const [pinned, setPinned] = useState(true);
  const [runError, setRunError] = useState('');
  const [category, setCategory] = useState<RunCategory>('all');
  // 记录流一屏显示多少条；「加载更多」每次再加这么多。
  const [runLimit, setRunLimit] = useState(20);
  const [search, setSearch] = useState('');
  // 浏览器最近一次取到数据的时刻：流停住时头部必须说清楚「数据停在哪」。
  const [fetchedAt, setFetchedAt] = useState(() => Date.now());

  // 暂停时新到的记录先攒着（连接不断），恢复时一次补上。
  const pausedRef = useRef(false);
  const backlogRef = useRef<MonitorProbeRun[]>([]);
  // 已经上过屏的键：同一发探测被「探测帧 + 后续快照」重复送达时只算一次。
  const seenRef = useRef<Set<string>>(new Set());
  const highlightRef = useRef<Set<string>>(new Set());
  const freshTimersRef = useRef(new Map<string, number>());
  const gotDataRef = useRef(false);
  const rateRef = useRef<number[]>([]);
  const streamRunsRef = useRef<MonitorProbeRun[]>([]);
  const streamAccountsRef = useRef<MonitorProbeAccount[]>([]);
  // 账号现状刷新在飞行中：探测成串到达时只发一次 /__monitor/probes。
  const accountFetchRef = useRef(false);
  const accountAgainRef = useRef(false);
  const degradedRef = useRef(false);
  // 本次连接是否真的 open 过（EventSource 连上 text/event-stream 才会触发 onopen）。
  const openedRef = useRef(false);
  const timelineRef = useRef<HTMLDivElement | null>(null);

  // 合并：外层底稿 ∪ 流快照 ∪ 流增量。按 runKey 去重后时间新→旧。
  const views = useMemo(() => mergeRuns(outerRuns, streamRuns).rows, [outerRuns, streamRuns]);
  const probeAccounts = streamAccounts.length > 0 ? mergeAccounts(outerAccounts, streamAccounts) : outerAccounts;
  const now = tickNow;
  const nameOf = (providerId: string) =>
    probeAccounts.find((item) => item.providerId === providerId)?.name || providerId;

  /** 给新键打高亮：到期由定时器摘下，组件卸载时统一清掉。同一键重复打只延长一次。 */
  const highlight = useCallback((keys: string[]) => {
    if (keys.length === 0) return;
    for (const key of keys) {
      highlightRef.current.add(key);
      const old = freshTimersRef.current.get(key);
      if (old !== undefined) window.clearTimeout(old);
      freshTimersRef.current.set(key, window.setTimeout(() => {
        highlightRef.current.delete(key);
        freshTimersRef.current.delete(key);
        setFreshKeys(new Set(highlightRef.current));
      }, FRESH_MS));
    }
    setFreshKeys(new Set(highlightRef.current));
  }, []);

  /** 新到的记录：去重、记速率；暂停时入 backlog，否则立即上屏并高亮。 */
  const ingest = useCallback((incoming: MonitorProbeRun[]) => {
    const added: MonitorProbeRun[] = [];
    for (const row of incoming) {
      const key = runKey(row);
      if (seenRef.current.has(key)) continue;
      seenRef.current.add(key);
      added.push(row);
    }
    if (added.length === 0) return;
    const stamp = Date.now();
    rateRef.current.push(...added.map(() => stamp));
    setRateCount(rateRef.current.length);
    if (pausedRef.current) {
      backlogRef.current = mergeRuns(backlogRef.current, added).rows;
      setBacklogCount(backlogRef.current.length);
      return;
    }
    streamRunsRef.current = mergeRuns(streamRunsRef.current, added).rows;
    setStreamRuns(streamRunsRef.current);
    setFetchedAt(stamp);
    highlight(added.map(runKey));
  }, [highlight]);

  /** 整体换成一批记录（快照 / 重读）：高亮由探测帧负责，快照批量到达不逐条闪。 */
  const replaceRuns = useCallback((rows: MonitorProbeRun[]) => {
    seenRef.current = new Set(rows.map(runKey));
    // 快照是「当前全量」：暂停期间攒的增量已全部包含在其中，清空 backlog。
    backlogRef.current = [];
    setBacklogCount(0);
    streamRunsRef.current = mergeRuns(rows, []).rows;
    setStreamRuns(streamRunsRef.current);
    setFetchedAt(Date.now());
  }, []);

  /** 降级到轮询：清掉流专属状态，数据回到外层轮询。只生效一次。 */
  const degrade = useCallback((note: string) => {
    if (degradedRef.current) return;
    degradedRef.current = true;
    pausedRef.current = false;
    backlogRef.current = [];
    setBacklogCount(0);
    setPaused(false);
    setMode('poll');
    setStreamNote(note);
    setStreamRuns([]);
    streamRunsRef.current = [];
    setStreamAccounts([]);
    streamAccountsRef.current = [];
    rateRef.current = [];
    setRateCount(0);
  }, []);

  /** 每帧到达都调：清掉降级原因；轮询态收到帧说明流又活了（浏览器重连成功）。 */
  const enterStream = useCallback(() => {
    if (degradedRef.current) return;
    setStreamNote('');
    setMode((current) => (current === 'poll' ? 'live' : current));
  }, []);

  /** 探测之后账号现状可能变了：重读一次 /__monitor/probes（在飞的请求合并成下一轮）。 */
  const refreshAccountViews = useCallback(() => {
    if (accountFetchRef.current) {
      accountAgainRef.current = true;
      return;
    }
    accountFetchRef.current = true;
    void fetchProbes()
      .then((page) => {
        const accounts = page.accounts
          .map(parseProbeAccount)
          .filter((item): item is MonitorProbeAccount => item !== null);
        streamAccountsRef.current = accounts;
        setStreamAccounts(accounts);
        setFetchedAt(Date.now());
      })
      .catch(() => undefined)
      .finally(() => {
        accountFetchRef.current = false;
        if (accountAgainRef.current) {
          accountAgainRef.current = false;
          refreshAccountViews();
        }
      });
  }, []);

  // SSE 订阅。卸载（切页签 / 离开监控中心）即 close()，不留连接。
  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.EventSource !== 'function') {
      degrade('当前浏览器不支持 EventSource');
      return;
    }
    openedRef.current = false;
    const source = new EventSource(streamUrl(), { withCredentials: true });
    let closed = false;
    let lastFrame = Date.now();
    /* 浏览器对 EventSource 断线重连的处理（实测）：
       - 404 / 5xx：不会触发 error 回调，只打印一条控制台报错并自动重连（≈3s 一次）；
       - 200 但 Content-Type 不对：同样不派发 error（CONNECTING 状态），等头部；
       - 只有正常 text/event-stream 才触发 onopen。
       所以「连上但拿不到数据」只能自己判：收到 error，或者宽限期内既没 open 也没数据，都降级轮询。 */
    const guard = window.setTimeout(() => {
      if (!openedRef.current && !gotDataRef.current) degrade('实时流无响应，已回退轮询');
    }, SNAPSHOT_GRACE_MS);

    const payload = (event: MessageEvent<string>): unknown => {
      try {
        return JSON.parse(event.data) as unknown;
      } catch {
        return null;
      }
    };
    const touch = () => {
      lastFrame = Date.now();
      gotDataRef.current = true;
      enterStream();
    };

    source.onopen = () => {
      // 只有 onopen 才点亮 LIVE：正在建连、被 404/HTML 挡住时 onopen 不会来，徽标保持灰。
      openedRef.current = true;
      lastFrame = Date.now();
      enterStream();
    };
    source.onerror = () => {
      if (closed) return;
      // 拿到过数据后连接出错 → 直接回轮询，不让徽标在灰/绿之间来回跳。
      if (gotDataRef.current) degrade('实时流已断开，已回退轮询');
    };

    const onSnapshot = (event: MessageEvent<string>) => {
      touch();
      window.clearTimeout(guard);
      const raw = payload(event) as Record<string, unknown> | null;
      const probePage = raw ? parseProbePage(raw.probes) : null;
      if (!probePage) return;
      streamAccountsRef.current = probePage.accounts;
      setStreamAccounts(probePage.accounts);
      replaceRuns(probePage.runs);
    };

    const onProbe = (event: MessageEvent<string>) => {
      touch();
      const frame = parseStreamProbeFrame(payload(event));
      if (!frame) return;
      const rows = frame.runs
        .map(parseProbeRun)
        .filter((row): row is MonitorProbeRun => row !== null);
      ingest(rows);
      // 连续失败次数、最近状态、今日次数随探测一起变：重读账号现状，不重拉整页。
      refreshAccountViews();
    };

    // 事件（告警 / 续期 / 额度……）与告警状态变化都可能带出新的探测结论：现状重读一次。
    const onEvents = (event: MessageEvent<string>) => {
      touch();
      const rows = payload(event);
      if (Array.isArray(rows) && rows.length > 0) refreshAccountViews();
    };
    const onAlert = () => {
      touch();
      refreshAccountViews();
    };
    // 后端丢了一段（慢消费者 / 快照失败）：补齐后再重读现状。记录本身由下一次快照收敛。
    const onResync = () => {
      touch();
      refreshAccountViews();
    };

    const handlers: Record<MonitorStreamEventName, (event: MessageEvent<string>) => void> = {
      snapshot: onSnapshot,
      events: onEvents,
      alert: onAlert,
      probe: onProbe,
      resync: onResync,
    };
    for (const name of MONITOR_STREAM_EVENTS) {
      source.addEventListener(name, handlers[name] as EventListener);
    }

    // 假死检测：后端 15 秒一次心跳，45 秒无帧就重建连接
    //（浏览器不会替我们判「连上了但不再来数据」）。
    const watchdog = window.setInterval(() => {
      if (closed || Date.now() - lastFrame <= STREAM_DEAD_MS) return;
      if (!gotDataRef.current) {
        degrade('实时流无响应，已回退轮询');
        return;
      }
      lastFrame = Date.now();
      setAttempt((value) => value + 1);
    }, 5_000);

    return () => {
      closed = true;
      window.clearTimeout(guard);
      window.clearInterval(watchdog);
      source.onopen = null;
      source.onerror = null;
      source.close();
      gotDataRef.current = false;
      pausedRef.current = false;
      backlogRef.current = [];
    };
  }, [attempt, degrade, enterStream, ingest, refreshAccountViews, replaceRuns]);

  // 降级后的轮询：与原「自动刷新」同节奏，页面隐藏时跳过。流活着时这条路径完全不走。
  useEffect(() => {
    if (mode !== 'poll' || document.hidden) return;
    const timer = window.setInterval(() => onRefreshRuns(), POLL_FALLBACK_MS);
    const onVisible = () => {
      if (!document.hidden) onRefreshRuns();
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [mode, onRefreshRuns]);

  // 每秒：滑出速率窗口之外的到达时刻，并驱动「x 秒前」这类相对时间文本。
  useEffect(() => {
    const timer = window.setInterval(() => {
      const cut = Date.now() - RATE_WINDOW_MS;
      rateRef.current = rateRef.current.filter((at) => at > cut);
      setRateCount(rateRef.current.length);
      setTickNow(Date.now());
    }, 1_000);
    return () => window.clearInterval(timer);
  }, []);

  useEffect(() => {
    const timers = freshTimersRef.current;
    return () => {
      for (const timer of timers.values()) window.clearTimeout(timer);
      timers.clear();
    };
  }, []);

  const togglePaused = () => {
    if (mode === 'poll') {
      // 轮询模式下的「重试」：重建连接，仍不可用会在宽限期后再回退。
      setMode('live');
      setAttempt((value) => value + 1);
      return;
    }
    if (!paused) {
      pausedRef.current = true;
      setPaused(true);
      setMode('paused');
      return;
    }
    pausedRef.current = false;
    setPaused(false);
    setMode('live');
    const backlog = backlogRef.current;
    backlogRef.current = [];
    setBacklogCount(0);
    if (backlog.length === 0) return;
    // 恢复：暂停期间攒下的条目一次补上，并同样走高亮。
    streamRunsRef.current = mergeRuns(streamRunsRef.current, backlog).rows;
    setStreamRuns(streamRunsRef.current);
    setFetchedAt(Date.now());
    highlight(backlog.map(runKey));
  };

  const clearStream = () => {
    backlogRef.current = [];
    setBacklogCount(0);
    streamRunsRef.current = [];
    setStreamRuns([]);
    rateRef.current = [];
    setRateCount(0);
    for (const timer of freshTimersRef.current.values()) window.clearTimeout(timer);
    freshTimersRef.current.clear();
    highlightRef.current.clear();
    setFreshKeys(new Set());
    // 已上屏集合留空：同一轮探测之后若再推一遍（后端重放），仍会被当成新条目补上。
    // 外层底稿也一起清：轮询/快照虽会重新带回服务端记录，但「清空」这一刻必须立即生效。
    onClearRuns();
  };

  // 置顶锁定：新的顶栏条目出现时滚回顶部；取消勾选后完全不碰滚动。
  const lastTopKeyRef = useRef('');
  useEffect(() => {
    const top = views[0];
    const topKey = top ? runKey(top) : '';
    if (topKey === lastTopKeyRef.current) return;
    lastTopKeyRef.current = topKey;
    if (!pinned || paused) return;
    // 锁的只是「流动框」：用户翻到下面看账号现状时，新条目不该把整页拽回顶部。
    const el = timelineRef.current?.closest<HTMLElement>('.mon-probe-viewport');
    if (el) el.scrollTo({ top: 0, behavior: 'auto' });
  }, [views, pinned, paused]);

  const run = (providerId: string) => {
    setRunError('');
    void onRunProbe(providerId).then((data) => {
      // onRunProbe 内部已经刷新过，但刷新请求可能在探测行落库之前返回；这里再把已落库的记录补进来，
      // 否则弹层打开期间「最近探测记录」会少掉刚跑的两行。
      if (data) void onRefreshRuns();
      else setRunError(`${nameOf(providerId)}：探测请求失败，请稍后重试`);
      // 实时流下探测行不经过外层轮询：跑完直接重读账号现状，结果不必等下一帧。
      if (mode !== 'poll') refreshAccountViews();
    });
  };

  const filteredRuns = providerFilter ? views.filter((row) => row.providerId === providerFilter) : views;
  // 账号排序：失败（次数多的在前）→ 有跳过说明 → 未探过 → 通过；同档按名字，刷新不跳位。
  const rank = (item: MonitorProbeAccount) =>
    item.probed && item.consecutiveFailures > 0 ? 0 : item.skipped ? 1 : !item.probed ? 2 : 3;
  const ordered = [...probeAccounts].sort((a, b) => {
    const delta = rank(a) - rank(b);
    if (delta !== 0) return delta;
    if (rank(a) === 0) return b.consecutiveFailures - a.consecutiveFailures;
    return a.name.localeCompare(b.name, 'zh-CN');
  });
  // 调用方（MonitoringPage）已过滤掉「账号已不在账号列表里」的结果，这里不再二次判断（审查 O4）。
  const shownResult = probeResult;

  // 分类计数按「筛选账号之后」的记录算：胶囊上的 N 必须等于点进去看得到的行数。
  const countOf = (cat: RunCategory) => {
    if (cat === 'all') return filteredRuns.length;
    return filteredRuns.filter((row) => {
      if (cat === 'skip') return !!row.skipped;
      if (cat === 'ok') return !row.skipped && row.ok;
      return !row.skipped && !row.ok;
    }).length;
  };
  const CATEGORIES: Array<{ id: RunCategory; label: string; tone: string }> = [
    { id: 'all', label: '全部', tone: 'all' },
    { id: 'ok', label: '成功', tone: 'ok' },
    { id: 'fail', label: '失败', tone: 'err' },
    { id: 'skip', label: '跳过', tone: 'skip' },
  ];
  const keyword = search.trim().toLowerCase();
  /** 搜索同时匹配账号名与错误码（API 里的 errorCode 原样参与匹配，显示也原样）。 */
  const matchesSearch = (row: MonitorProbeRun) => {
    if (!keyword) return true;
    const name = nameOf(row.providerId).toLowerCase();
    return name.includes(keyword) || (row.errorCode ?? '').toLowerCase().includes(keyword);
  };
  const matchedRuns = filteredRuns.filter((row) => {
    if (category === 'skip') return !!row.skipped;
    if (category === 'ok') return !row.skipped && row.ok;
    if (category === 'fail') return !row.skipped && !row.ok;
    return true;
  }).filter(matchesSearch);
  // 一次只渲染一屏多一点的记录：58 条全展开会把页面拉到 8000px（1440 实测），
  // 而且每条都有四列指标格 + payload，滚一趟要很久。参考同样只显示有限条数。
  const shownRuns = matchedRuns.slice(0, runLimit);
  const hasMoreRuns = matchedRuns.length > shownRuns.length;

  const newestStamp = views.reduce((best, row) => (row.time > best ? row.time : best), '');
  const lastRunStamp = newestStamp ? displayTime(newestStamp, timezone) : null;

  return (
    <div className="card panel">
      <div className="panel-header">
        <div>
          <h2 className="panel-title">{`探测（${probeAccounts.length} 个账号）`}</h2>
          <p className="panel-desc">
            {'轻探测是最小对话（max_tokens=16），不写请求日志，不进账号成功率与成本。探测失败只进入 probe_failed 规则，不会自动处置账号。'}
          </p>
        </div>
      </div>

      {error && <div className="metric-value err">{error}</div>}
      {runError && <div className="metric-value err">{runError}</div>}

      {/* ① 控制条：实时流的读数（LIVE/PAUSED、速率、暂停、置顶、清空）。降级时同一位置显示「轮询模式」。 */}
      <div className="mon-probe-bar">
        <div className="mon-probe-bar-main" role="group" aria-label="探测实时流状态">
          <div
            className={`mon-probe-live${mode === 'live' ? ' on' : ' off'}`}
            role="status"
            aria-live="polite"
            title={mode === 'poll'
              ? `实时流不可用${streamNote ? `（${streamNote}）` : ''}，已按 ${POLL_FALLBACK_MS / 1000} 秒轮询取数`
              : mode === 'paused' ? '连接保持中，新条目已暂存，点「继续采集」一次补上' : 'SSE 实时流已连上'}
          >
            {mode === 'live'
              ? <span className="mon-probe-pulse" aria-hidden />
              : <span className="mon-probe-ind" aria-hidden />}
            <span className="mon-probe-live-text">{
              mode === 'live' ? 'LIVE TAIL 流式采集'
                : mode === 'paused' ? '已暂停采集'
                : '轮询模式'
            }</span>
            <span className="mon-probe-rate" title="最近 5 秒收到的探测记录速率">{`${rateText(rateCount)} 条/秒`}</span>
            {mode === 'paused' && backlogCount > 0 && (
              <span className="mon-probe-pending" title="恢复后一次补上">{`+${backlogCount} 待上屏`}</span>
            )}
            {mode === 'poll' && streamNote && <span className="mon-probe-note">{streamNote}</span>}
          </div>
          <span className="mon-probe-meta" title="浏览器最近一次拿到探测数据（流的帧或轮询结果）的时刻">
            {`上次更新 ${displayTime(new Date(fetchedAt).toISOString(), timezone).text}`}
          </span>
          <span className="mon-probe-meta" title={lastRunStamp?.title ?? '还没有探测记录'}>
            {lastRunStamp ? `最新记录 ${lastRunStamp.text}` : '最新记录 —'}
          </span>
        </div>
        <div className="mon-probe-bar-actions" role="group" aria-label="实时流操作">
          <button
            type="button"
            className={`btn btn-sm mon-probe-pause${mode === 'paused' ? ' on' : ''}`}
            aria-pressed={mode === 'paused'}
            onClick={togglePaused}
            title={mode === 'poll' ? '重新尝试连接实时流' : paused ? '把暂停期间暂存的条目一次补上' : '停止上屏新条目，连接保持'}
          >
            {mode === 'poll' ? '重试实时流' : mode === 'paused' ? '继续采集' : '暂停采集'}
          </button>
          <Switch
            checked={pinned}
            label="置顶锁定最新"
            disabled={mode === 'poll'}
            onChange={setPinned}
          />
          <button
            type="button"
            className="btn btn-sm mon-probe-clear"
            onClick={clearStream}
            disabled={views.length === 0 && backlogCount === 0}
            title="只清空本页列表，不影响服务端记录"
          >
            {'清空'}
          </button>
        </div>
      </div>

      {/* ② 分类胶囊：计数来自本轮 /__monitor/probes 的 runs（已按账号筛选）。 */}
      <div className="mon-chips" role="group" aria-label="按结果筛选">
        {CATEGORIES.map((item) => {
          const active = category === item.id;
          return (
            <button
              key={item.id}
              type="button"
              className={`mon-chip mon-chip-${item.tone}${active ? ' on' : ''}`}
              aria-pressed={active}
              onClick={() => setCategory(item.id)}
            >
              <span className="mon-chip-label">{item.label}</span>
              <span className="mon-chip-count">{countOf(item.id)}</span>
            </button>
          );
        })}
      </div>

      {/* ③ 搜索：账号名 / 错误码。 */}
      <div className="mon-probe-search">
        <input
          className="monitoring-input"
          type="search"
          placeholder="搜索账号名 / 错误码"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          aria-label="按账号名或错误码搜索探测记录"
        />
        {providerFilter && (
          <button className="btn btn-sm" onClick={() => onFilter('')}>{'查看全部账号'}</button>
        )}
      </div>

      {/* ④ 时间轴：竖线 + 按结果着色的圆点，每条下方一行等宽 payload。*/}
      {/* 流动框：实时流下条目会一直长，框内滚动；框高与「账号探测现状」平分剩余视口，两块都留在首屏。 */}
      <div className="mon-probe-live-grid">
        <div className="mon-probe-viewport">
          <div className="mon-timeline" ref={timelineRef}>
            {shownRuns.length === 0 && (
              <div className="m-empty">
                {filteredRuns.length === 0
                  ? providerFilter ? `${nameOf(providerFilter)} 还没有探测记录。` : '还没有探测记录，可以点「立即探测」发一发。'
                  : '没有符合筛选条件的记录。'}
              </div>
            )}
            {shownRuns.map((row) => {
              const skipped = probeSkipText(row.skipped);
              // 跳过原因是给人看的结论：有中文释义就只显示释义，原始 key 留在 payload 的 skipped= 里。
              const skipReason = !row.skipped ? ''
                : /^已跳过（/.test(skipped) ? `本次探测跳过，原因代码 ${row.skipped}` : skipped;
              const tone = skipped ? 'skip' : row.ok ? 'ok' : 'err';
              const stamp = displayTime(row.time, timezone);
              // 徽标只说「结果」，原因永远不挤进徽标（否则看不出哪段是状态、哪段是原因）。
              const verdict = skipped ? '跳过' : row.ok ? '成功' : '失败';
              const reason = !skipped && !row.ok
                ? row.errorCode ? errorReason(row.errorCode) : row.status === 0 ? '网络不可达' : ''
                : '';
              return (
                <div
                  className={`mon-tl-item ${tone}${freshKeys.has(runKey(row)) ? ' fresh' : ''}`}
                  key={runKey(row)}
                >
                  <div className="mon-tl-head">
                    {/* 时区在页头写着，行内只留日期时间；完整值与本地时间在悬停里。 */}
                    <span className="mon-tl-time" title={stamp.title}>{displayTimeCompact(stamp.text)}</span>
                    <span className={`mon-tl-verdict ${tone}`}>{verdict}</span>
                    <span
                      className={`mon-tl-kind ${kindTone(row.kind)}`}
                      title={row.kind === PROBE_KIND_LIGHT ? '轻探测：最小对话，不写请求日志' : `历史记录（kind=${row.kind}）：深检已下线，只作存档`}
                    >
                      {probeKindText(row.kind)}
                    </span>
                    <span className="mon-tl-title">{probeCaseText(row.case)}</span>
                    <button
                      type="button"
                      className="mon-tl-account"
                      onClick={() => onFilter(row.providerId)}
                      title={providerFilter ? `只看 ${nameOf(row.providerId)} 的记录（已筛选）` : `只看 ${nameOf(row.providerId)} 的记录`}
                    >
                      <BrandIcon icon={iconOf?.(row.providerId)} name={nameOf(row.providerId)} size={14} />
                      <span className="mon-tl-account-name">{nameOf(row.providerId)}</span>
                    </button>
                  </div>
                  {skipped && <div className="mon-tl-skip">{skipReason}</div>}
                  {/* 四列指标格：宽处一行，窄处 2×2；值整段换行，不省略。 */}
                  <div className="mon-tl-metrics">
                    {runMetrics(row).map((metric) => (
                      <div className="mon-tl-metric" key={metric.label}>
                        <span className="mon-tl-metric-label">{metric.label}</span>
                        <span className={`mon-tl-metric-value${metric.tone ? ` ${metric.tone}` : ''}`}>{metric.value}</span>
                      </div>
                    ))}
                  </div>
                  {reason && <div className="mon-tl-reason" title={row.errorCode}>{reason}</div>}
                  <div className="mon-tl-payload">
                    <span className="mon-tl-prompt" aria-hidden>&gt;</span>
                    <span className="mon-tl-segs">
                      {payloadSegments(row).map((segment) => (
                        <span
                          className={`mon-tl-seg${segment.startsWith('error=') ? ' err' : ''}`}
                          key={segment}
                          title={segment.startsWith('error=') ? row.errorCode : undefined}
                        >
                          {segment}
                        </span>
                      ))}
                    </span>
                  </div>
                </div>
              );
            })}
            {hasMoreRuns && (
              <div className="mon-tl-more">
                <button type="button" className="btn btn-sm" onClick={() => setRunLimit((n) => n + 20)}>
                  {`加载更多（还有 ${matchedRuns.length - shownRuns.length} 条）`}
                </button>
              </div>
            )}
          </div>
        </div>

      <div className="mon-section-title">{'账号探测现状'}</div>
      <div className="mon-pa-list">
        {ordered.length === 0 && <div className="muted-text">{'还没有账号。接入 Provider 后这里会出现探测矩阵。'}</div>}
        {ordered.map((account) => {
          const outcome = outcomeOf(account);
          const probing = busy === `probe:${account.providerId}`;
          const lastStamp = displayTime(account.lastProbeAt, timezone);
          return (
            <div className="log-row" key={account.providerId}>
              <div className="mrow mrow-probe">
                <span className="m-clip" style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <span className={`dot ${account.probed ? (outcome.tone === 'err' ? 'fail' : 'ok') : 'idle'}`} aria-hidden />
                  <BrandIcon icon={iconOf?.(account.providerId)} name={account.name} size={18} />
                  <span className="m-clip" style={{ fontWeight: 600 }} title={account.name}>
                    {account.name}
                  </span>
                </span>
                <span className="m-clip" title={outcome.skip ? `${outcome.status}：${outcome.skip}` : outcome.status}>
                  <span className={outcome.tone === 'err' ? 'err' : outcome.tone === 'ok' ? '' : 'muted-text'}>
                    {outcome.status}
                  </span>
                  {outcome.skip && <span className="muted-text">{` · ${outcome.skip}`}</span>}
                </span>
                {/* 上次时间同样有长短两档：窄处去掉时区后缀，整句（含相对时间）不被省略号截断。 */}
                <span className="mon-pa-last" title={account.probed ? `最近一次成功执行：${lastStamp.title}` : '还没有可判定的探测'}>
                  {account.probed ? (
                    <>
                      <span className="mon-pa-last-time">
                        {displayTimeCompact(lastStamp.text)}
                      </span>
                      {`（${relativeToNow(account.lastProbeAt, now)}）`}
                    </>
                  ) : '—'}
                </span>
                <span className="mon-pa-daily" title={account.maxPerDay === 0 ? '当日次数不限' : '当日已探测次数 / 每日上限'}>
                  {`今日 ${dailyText(account)}`}
                </span>
                <span className="m-row-num" title="最近一发探测的上游 HTTP 状态；0 表示传输层失败">
                  {lastStatusText(account.lastStatus)}
                </span>
                <span className="probe-actions">
                  <button
                    className="btn btn-sm"
                    disabled={probing}
                    onClick={() => run(account.providerId)}
                    title={account.disabled ? '账号已停用；探测会记为「账号已停用，未探测」' : '现在就发一发最小对话探测'}
                  >
                    {probing ? '探测中…' : '立即探测'}
                  </button>
                  <button className="btn btn-sm" onClick={() => onOpenAccount(account.providerId)}>{'详情'}</button>
                </span>
              </div>
            </div>
          );
        })}
        </div>
      </div>

      {shownResult && (
        <Modal
          title={`探测结果：${nameOf(shownResult.providerId)}`}
          description="手动探测只读取结果，不会改动账号配置或调度状态"
          onClose={onDismissResult}
        >
          <ProbeResultBody result={shownResult.data} />
        </Modal>
      )}
    </div>
  );
}

export interface ProbeResultBodyProps {
  result: MonitorProbeRunResult;
}

/**
 * 一次手动探测的结果。抽屉与探测页共用：结果永远就地显示在触发它的地方，
 * 不靠顶部提示条（抽屉打开时顶部会被 Modal 挡住）。
 */
export function ProbeResultBody({ result }: ProbeResultBodyProps) {
  if (result.skipped === 'in_flight') {
    return <div className="metric-note">{'已有探测在进行中：同一账号同时只跑一发，等它结束后再试。'}</div>;
  }
  if (result.errorKind) {
    return <div className="metric-note err">{`探测未能执行：${result.errorKind}`}</div>;
  }
  const skipped = probeSkipText(result.skipped);
  if (skipped) return <div className="metric-note">{skipped}</div>;
  const cases = result.cases ?? [];
  if (cases.length === 0) return <div className="metric-note">{'探测已提交，但没有返回用例结果。'}</div>;
  const headline = result.ok
    ? '轻探测通过'
    : '轻探测失败：只进入 probe_failed 规则，不会自动处置账号。';
  return (
    <div>
      <div className="metric-note">{headline}</div>
      {cases.map(normalizeProbeCase).map((item, index) => (
        <div className="log-row" key={`${item.key}-${index}`}>
          <div className="mrow mrow-probe-case">
            <span className={`badge ${item.ok ? 'green' : 'red'}`}>{item.ok ? '通过' : '失败'}</span>
            <span className="m-clip">{probeCaseText(item.key)}</span>
            <span className="m-row-num">{`状态 ${statusCodeText(item.status)}`}</span>
            <span className="muted-text">{formatLatency(item.durationMs)}</span>
            <span className="muted-text m-clip" title={item.error}>{item.model || item.error || '—'}</span>
          </div>
          {item.error && !item.model && <div className="metric-note err m-clip">{item.error}</div>}
        </div>
      ))}
    </div>
  );
}

export function minutesText(value: number): string {
  if (value <= 0) return '不限制';
  if (value % 1440 === 0) return `${value / 1440} 天`;
  if (value % 60 === 0) return `${value / 60} 小时`;
  return `${value} 分钟`;
}

export interface ProbeSettingsPanelProps {
  config: MonitorProbeConfig | null;
  timezone: string;
  draft: MonitorProbeConfig | null;
  onChange: (next: MonitorProbeConfig) => void;
  onSubmit: () => void;
  busy: boolean;
  error: string;
  notice: string;
}

/**
 * 探测配置的唯一入口（设置页）。只暴露决定「会花多少额度」的参数，外加额度读数
 * 过期判定（账号页的「数据过期」标签读的就是它）。表单没覆盖的字段一律原样带回，
 * 避免保存时被零值覆盖。
 */
export function ProbeSettingsPanel(props: ProbeSettingsPanelProps) {
  const { config, timezone, draft, onChange, onSubmit, busy, error, notice } = props;
  // 配置先到、或表单草稿先到都会渲染：后端配置只用来显示「当前生效」，
  // 表单值以草稿为准；草稿还没到时就先显示后端值，不让整张卡片闪空。
  const base = draft ?? config;
  if (!base) return null;
  const set = (patch: Partial<MonitorProbeConfig>) => onChange({ ...base, ...patch });
  const setEvent = (patch: Partial<MonitorProbeConfig['eventDriven']>) =>
    set({ eventDriven: { ...base.eventDriven, ...patch } });
  const setDaily = (patch: Partial<MonitorProbeConfig['daily']>) => set({ daily: { ...base.daily, ...patch } });
  // type=number 在输入非法时 value 是 ''，直接 Number('') 会变成 0（悄悄改成「不限」）。
  const numberValue = (raw: string, fallback: number) => {
    if (raw.trim() === '') return fallback;
    const value = Number(raw);
    return Number.isFinite(value) ? Math.trunc(value) : fallback;
  };

  return (
    <div className="card mon-set-card">
      <header className="mon-set-head">
        <div className="mon-set-head-text">
          <h2 className="mon-card-title">{'探测调度'}</h2>
          <p className="mon-set-desc">
            {`只对已启用、24 小时内有过真实请求的账号探测；当日次数与每日窗口都按投递时区（${timezone}）计算。`}
          </p>
        </div>
      </header>
      <div className="monitoring-cols">
        <div className="field mon-set-switch">
          <span>{'轻探测开关'}</span>
          <Switch
            checked={base.eventDriven.enabled}
            label="事件驱动轻探测"
            onChange={(value) => setEvent({ enabled: value })}
          />
        </div>
        <label className="field">
          <span>{'每账号每日上限（0 = 不限）'}</span>
          <input
            type="number"
            min={0}
            max={24}
            value={base.eventDriven.maxPerDayPerAccount}
            onChange={(event) => setEvent({ maxPerDayPerAccount: numberValue(event.target.value, base.eventDriven.maxPerDayPerAccount) })}
          />
        </label>
        <label className="field">
          <span>{'两次探测最小间隔（分钟）'}</span>
          <input
            type="number"
            min={0}
            max={1440}
            value={base.eventDriven.minIntervalMinutes}
            onChange={(event) => setEvent({ minIntervalMinutes: numberValue(event.target.value, base.eventDriven.minIntervalMinutes) })}
          />
        </label>
        <label className="field">
          <span>{'账号闲置多久后才补探（分钟）'}</span>
          <input
            type="number"
            min={0}
            max={1440}
            value={base.eventDriven.idleMinutes}
            onChange={(event) => setEvent({ idleMinutes: numberValue(event.target.value, base.eventDriven.idleMinutes) })}
          />
        </label>
        <label className="field">
          <span>{'每日窗口开始（HH:MM）'}</span>
          <input value={base.daily.windowStart} placeholder="04:00" onChange={(event) => setDaily({ windowStart: event.target.value })} />
        </label>
        <label className="field">
          <span>{'每日窗口结束（HH:MM）'}</span>
          <input value={base.daily.windowEnd} placeholder="06:00" onChange={(event) => setDaily({ windowEnd: event.target.value })} />
        </label>
        <label className="field">
          <span>{'额度读数过期判定（分钟，0 = 不标记）'}</span>
          <input
            type="number"
            min={0}
            value={base.quotaStaleMinutes}
            onChange={(event) => set({ quotaStaleMinutes: Math.max(0, numberValue(event.target.value, base.quotaStaleMinutes)) })}
          />
        </label>
      </div>
      <p className="mon-set-foot">
        {`当前生效：每账号每日 ${base.eventDriven.maxPerDayPerAccount === 0 ? '不限' : `${base.eventDriven.maxPerDayPerAccount} 次`} · 最小间隔 ${minutesText(base.eventDriven.minIntervalMinutes)} · 闲置阈值 ${minutesText(base.eventDriven.idleMinutes)} · 每日窗口 ${base.daily.enabled ? `${base.daily.windowStart}–${base.daily.windowEnd}` : '已关闭'}（${timezone}）`}
      </p>
      <p className="mon-set-foot">
        {base.quotaStaleMinutes > 0
          ? `额度读数超过 ${base.quotaStaleMinutes} 分钟未更新时，账号页与详情里标为「数据过期」。`
          : '额度过期判定已关闭：账号页与详情不标「数据过期」。'}
      </p>
      <div className="mon-set-form-actions">
        <button className="btn btn-sm primary" disabled={busy} onClick={onSubmit}>
          {'保存探测配置'}
        </button>
        {error && <span className="mon-set-inline err" role="alert">{error}</span>}
        {notice && !error && <span className="mon-set-inline">{notice}</span>}
      </div>
    </div>
  );
}
