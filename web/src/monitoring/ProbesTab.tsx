import {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import {BrandIcon, Modal, Switch} from '../components/ui';
import {API_BASE} from '../lib';
import type {
    MonitorEvent,
    MonitorProbeAccount,
    MonitorProbeConfig,
    MonitorProbeRunResult,
    MonitorProbeStreamMode,
} from './types';
import {ERROR_KIND_TEXT, normalizeProbeCase, probeCaseText, probeSkipText} from './types';
import {displayTime, displayTimeCompact, formatLatency} from './format';
import type {MonitorStreamEventName} from './api';
import {fetchEvents, MONITOR_STREAM_EVENTS, MONITOR_STREAM_PATH, parseStreamEventsFrame,} from './api';

/** 已上线的探测种类：只有轻探测。后端对其余 kind 回 400 kind_unavailable。 */
export const PROBE_KIND_LIGHT = 'light';

/**
 * SSE 不可用时的轮询节奏。也用于「超过这么久没收到任何帧就判流断了」：
 * 后端每 15 秒发一次 `:heartbeat` 注释行，45 秒静默只能是连接假死。
 */
const POLL_FALLBACK_MS = 15_000;
const STREAM_DEAD_MS = 45_000;

/** 新条目入场高亮的时长，与 styles.css 里 .mon-ev-card.fresh 的动画一致。 */
const FRESH_MS = 1600;

/** 一次读多少条事件；「加载更多」按页追加。 */
const EVENT_PAGE_SIZE = 100;
/** 本地列表上限：流是长期连接，不封顶的话几个小时后 DOM 会拖垮页面。 */
const EVENT_CAP = 500;

/** 速率窗口：最近 5 秒收到的事件条数 ÷ 5。 */
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

// ── 事件分类 ───────────────────────────────────────────────────────────

type EventCategory = 'all' | 'probe' | 'alert' | 'renewal' | 'checkin' | 'system';

/** 分类顺序与参考的事件流页签一致；quota 尚无生产者，归档到「系统内核」。 */
const EVENT_CATEGORIES: Array<{ id: EventCategory; label: string }> = [
  { id: 'all', label: '全部' },
  { id: 'renewal', label: '账号续期' },
  { id: 'probe', label: '轻探测' },
  { id: 'alert', label: '告警' },
  { id: 'checkin', label: '自动签到' },
  { id: 'system', label: '系统内核' },
];

const KIND_LABEL: Record<string, string> = {
  probe: '轻探测',
  alert: '告警',
  renewal: '账号续期',
  checkin: '自动签到',
  quota: '系统内核',
  system: '系统内核',
  test: '测试',
};

function categoryOf(kind: string): Exclude<EventCategory, 'all'> {
  if (kind === 'probe' || kind === 'alert' || kind === 'renewal' || kind === 'checkin') return kind;
  return 'system';
}

/** 级别胶囊：P0/P1/P2 沿用告警语义，其余一律 INFO（探测、续期成功、系统采集都是信息）。 */
function severityOf(ev: MonitorEvent): { label: string; tone: 'p0' | 'p1' | 'p2' | 'info' } {
  if (ev.severity === 'p0') return { label: 'P0', tone: 'p0' };
  if (ev.severity === 'p1') return { label: 'P1', tone: 'p1' };
  if (ev.severity === 'p2') return { label: 'P2', tone: 'p2' };
  return { label: 'INFO', tone: 'info' };
}

function asText(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function asNumber(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0;
}

function statusCodeText(status: number | undefined): string {
  if (status === undefined || status === 0) return '—';
  return String(status);
}

/**
 * 错误码的核心原因。后端把 Go 的传输层错误整段存进来（`Post "https://…": context canceled`），
 * 整条 URL 塞进行内没有信息量：只取最后一个冒号之后的原因，完整原文留给悬停。
 */
function errorReason(errorCode: string): string {
  const known = (ERROR_KIND_TEXT as Record<string, string>)[errorCode];
  if (known) return known;
  const at = errorCode.lastIndexOf(': ');
  const tail = at >= 0 ? errorCode.slice(at + 2).trim() : errorCode;
  return tail || errorCode;
}

function eventKey(ev: MonitorEvent): string {
  return String(ev.id);
}

/** 合并事件：按 id 去重（后到的覆盖先到的），时间新→旧，保留最近 EVENT_CAP 条。 */
function mergeEvents(base: MonitorEvent[], incoming: MonitorEvent[]): MonitorEvent[] {
  const byId = new Map<number, MonitorEvent>();
  for (const ev of base) byId.set(ev.id, ev);
  for (const ev of incoming) byId.set(ev.id, ev);
  const rows = [...byId.values()].sort((a, b) => Date.parse(b.time) - Date.parse(a.time));
  return rows.slice(0, EVENT_CAP);
}

interface EventMetric {
  label: string;
  value: string;
  tone?: 'err' | 'muted';
}

/**
 * 卡片里的指标格：探测与续期有结构化的 detail，能换算成四列读数；
 * 其余类型只留一行 payload，不为了凑格数去猜字段。
 */
function eventMetrics(ev: MonitorEvent, timezone: string): EventMetric[] {
  const detail = ev.detail || {};
  if (ev.kind === 'probe') {
    const cases = Array.isArray(detail.cases) ? detail.cases : [];
    const first = cases.length > 0 ? (cases[0] as Record<string, unknown>) : null;
    const skipped = asText(detail.skipped);
    const status = asNumber(first?.status);
    const duration = asNumber(first?.durationMs);
    const model = asText(first?.model);
    const error = asText(first?.error);
    const passed = first?.ok === true || detail.ok === true;
    return [
      {
        label: '状态码',
        value: skipped ? '跳过' : status === 0 ? '无响应' : String(status),
        tone: !skipped && status === 0 ? 'err' : undefined,
      },
      { label: '耗时', value: duration > 0 ? formatLatency(duration) : '—', tone: duration > 0 ? undefined : 'muted' },
      { label: '模型', value: model || '—', tone: model ? undefined : 'muted' },
      {
        label: '结果',
        value: error ? errorReason(error) : skipped ? probeSkipText(skipped) : passed ? '通过' : '失败',
        tone: error || (!passed && !skipped) ? 'err' : undefined,
      },
    ];
  }
  if (ev.kind === 'renewal') {
    const duration = asNumber(detail.durationMs);
    const expires = asText(detail.expiresAt);
    const errorKind = asText(detail.errorKind);
    return [
      { label: '耗时', value: duration > 0 ? formatLatency(duration) : '—', tone: duration > 0 ? undefined : 'muted' },
      { label: '凭证商', value: asText(detail.vendor) || '—', tone: asText(detail.vendor) ? undefined : 'muted' },
      {
        label: '新到期',
        value: expires ? displayTimeCompact(displayTime(expires, timezone).text) : '—',
        tone: expires ? undefined : 'muted',
      },
      {
        label: '结果',
        value: errorKind ? errorReason(errorKind) : '成功',
        tone: errorKind ? 'err' : undefined,
      },
    ];
  }
  return [];
}

/** 一行等宽 payload：探测按 case 展开，其余把 detail 拍平成 key=value。 */
function payloadSegments(ev: MonitorEvent): string[] {
  const detail = ev.detail || {};
  if (ev.kind === 'probe') {
    const cases = Array.isArray(detail.cases) ? detail.cases : [];
    const first = cases.length > 0 ? (cases[0] as Record<string, unknown>) : null;
    const status = asNumber(first?.status);
    const duration = asNumber(first?.durationMs);
    const model = asText(first?.model);
    const error = asText(first?.error);
    const skipped = asText(detail.skipped);
    return [
      `case=${asText(first?.case) || asText(detail.case) || 'chat'}`,
      `ok=${first?.ok === true || detail.ok === true}`,
      status > 0 ? `status=${status}` : '',
      duration > 0 ? `${duration}ms` : '',
      model ? `model=${model}` : '',
      error ? `error=${errorReason(error)}` : '',
      skipped ? `skipped=${skipped}` : '',
    ].filter(Boolean);
  }
  const segments: string[] = [];
  if (ev.ruleKey) segments.push(`rule=${ev.ruleKey}`);
  for (const [key, value] of Object.entries(detail)) {
    if (key === 'timeline' || key === 'cases' || value === null || value === undefined || value === '') continue;
    const text = typeof value === 'object' ? JSON.stringify(value) : String(value);
    segments.push(`${key}=${text}`);
  }
  return segments.slice(0, 10);
}

export interface ProbesTabProps {
  probeAccounts: MonitorProbeAccount[];
  /** 事件按账号筛选（深链 ?account= 与卡片上的账号按钮都走这里）；空 = 全部。 */
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
  onFilter: (providerId: string) => void;
  /** 流状态上报给外层：页签上的 LIVE / PAUSED 徽标与页面里的状态保持一致。 */
  onModeChange?: (mode: MonitorProbeStreamMode) => void;
}

export function ProbesTab(props: ProbesTabProps) {
  const {
    probeAccounts, providerFilter, timezone, busy, error, iconOf,
    probeResult, onDismissResult, onRunProbe, onFilter, onModeChange,
  } = props;

  // ── 实时流 ────────────────────────────────────────────────────────────
  /*
   * 数据分两层：REST 读回的 24h 底稿 + SSE 逐条增量（含轮询降级的合并结果）。
   * 渲染一律用合并后的 views：流断了退回底稿；流活着时新事件立即出现。
   */
  const [baseEvents, setBaseEvents] = useState<MonitorEvent[]>([]);
  const [streamEvents, setStreamEvents] = useState<MonitorEvent[]>([]);
  const [totalCount, setTotalCount] = useState(0);
  const [pageShown, setPageShown] = useState(1);
  const [mode, setMode] = useState<MonitorProbeStreamMode>('live');
  // 只有 onopen / 收到第一帧才算真的连上；建连期间不点亮绿色 LIVE。
  const [connected, setConnected] = useState(false);
  const [streamNote, setStreamNote] = useState('');
  // 自增即重建 EventSource（轮询降级后点「重试实时流」）。
  const [attempt, setAttempt] = useState(0);
  // 速率窗口里的到达时刻：只用来每秒算一次速率，不参与渲染数据。
  const [rateCount, setRateCount] = useState(0);
  const [paused, setPaused] = useState(false);
  const [backlogCount, setBacklogCount] = useState(0);
  const [freshKeys, setFreshKeys] = useState<ReadonlySet<string>>(() => new Set());
  // 置顶锁定：新条目从顶部插入并滚到顶；取消后可以自由往下翻。
  const [pinned, setPinned] = useState(true);
  // 与参考一致的两种视图：脉冲流卡片 / 极速终端。
  const [view, setView] = useState<'pipeline' | 'terminal'>('pipeline');
  const [category, setCategory] = useState<EventCategory>('all');
  const [search, setSearch] = useState('');
  // 浏览器最近一次取到数据的时刻：流停住时头部必须说清楚「数据停在哪」。
  const [fetchedAt, setFetchedAt] = useState(() => Date.now());
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [loadError, setLoadError] = useState('');
  const [reloadSeq, setReloadSeq] = useState(0);
  // 本地清空的分界：早于这一刻的事件不再上屏（服务端记录不受影响）。
  const [clearedAt, setClearedAt] = useState(0);
  const [pickerOpen, setPickerOpen] = useState(false);
  const [pickerSearch, setPickerSearch] = useState('');
  const [runError, setRunError] = useState('');

  // 暂停时新到的事件先攒着（连接不断），恢复时一次补上。
  const pausedRef = useRef(false);
  const backlogRef = useRef<MonitorEvent[]>([]);
  // 已经上过屏的事件 id：SSE 与 REST 重复送达时只算一次。
  const seenRef = useRef<Set<number>>(new Set());
  const highlightRef = useRef<Set<string>>(new Set());
  const freshTimersRef = useRef(new Map<string, number>());
  const gotDataRef = useRef(false);
  const rateRef = useRef<number[]>([]);
  const streamEventsRef = useRef<MonitorEvent[]>([]);
  const degradedRef = useRef(false);
  // 本次连接是否真的 open 过（EventSource 连上 text/event-stream 才会触发 onopen）。
  const openedRef = useRef(false);
  // 降级到轮询时要主动关掉这条连接：否则浏览器会按自己的节奏无限重连 404/5xx。
  const sourceRef = useRef<EventSource | null>(null);
  const viewportRef = useRef<HTMLDivElement | null>(null);
  const providerRef = useRef(providerFilter);
  const clearedAtRef = useRef(0);
  const pageShownRef = useRef(1);
  const pinnedRef = useRef(true);
  const pausedStateRef = useRef(false);

  useEffect(() => { onModeChange?.(mode); }, [mode, onModeChange]);
  // 结果一到就收起选择器：两层 Modal 叠在一起时点哪里都不对。
  useEffect(() => { if (probeResult) setPickerOpen(false); }, [probeResult]);
  useEffect(() => { providerRef.current = providerFilter; }, [providerFilter]);
  useEffect(() => { clearedAtRef.current = clearedAt; }, [clearedAt]);
  useEffect(() => { pageShownRef.current = pageShown; }, [pageShown]);
  useEffect(() => { pinnedRef.current = pinned; }, [pinned]);
  useEffect(() => { pausedStateRef.current = paused; }, [paused]);

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

  /** 新到的事件：按 id 去重、记速率；暂停时入 backlog，否则立即上屏并高亮。 */
  const ingest = useCallback((incoming: MonitorEvent[]) => {
    const scoped = providerRef.current
      ? incoming.filter((ev) => ev.targetId === providerRef.current)
      : incoming;
    const cut = clearedAtRef.current;
    const added: MonitorEvent[] = [];
    for (const ev of scoped) {
      if (seenRef.current.has(ev.id)) continue;
      if (cut > 0 && Date.parse(ev.time) <= cut) continue;
      seenRef.current.add(ev.id);
      added.push(ev);
    }
    if (added.length === 0) return;
    const stamp = Date.now();
    rateRef.current.push(...added.map(() => stamp));
    setRateCount(rateRef.current.length);
    if (pausedRef.current) {
      backlogRef.current = mergeEvents(backlogRef.current, added);
      setBacklogCount(backlogRef.current.length);
      return;
    }
    streamEventsRef.current = mergeEvents(streamEventsRef.current, added);
    setStreamEvents(streamEventsRef.current);
    setFetchedAt(stamp);
    highlight(added.map(eventKey));
  }, [highlight]);

  /** 读一页事件。replace 用于首次 / 换账号，merge 用于轮询、补缺口与「加载更多」。 */
  const fetchPage = useCallback(async (page: number, how: 'replace' | 'merge') => {
    const data = await fetchEvents({
      range: '24h',
      page,
      pageSize: EVENT_PAGE_SIZE,
      targetId: providerFilter || undefined,
    });
    const items = data.items;
    for (const ev of items) seenRef.current.add(ev.id);
    setTotalCount(data.total);
    setBaseEvents((prev) => (how === 'replace' ? items : mergeEvents(prev, items)));
    setPageShown((prev) => (how === 'replace' ? page : Math.max(prev, page)));
    setFetchedAt(Date.now());
  }, [providerFilter]);

  // 首屏 / 换账号：整页换数据，清掉旧账号的增量与清空分界。
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setLoadError('');
    setBaseEvents([]);
    setStreamEvents([]);
    streamEventsRef.current = [];
    setTotalCount(0);
    setPageShown(1);
    seenRef.current = new Set();
    backlogRef.current = [];
    setBacklogCount(0);
    setClearedAt(0);
    void fetchEvents({
      range: '24h',
      page: 1,
      pageSize: EVENT_PAGE_SIZE,
      targetId: providerFilter || undefined,
    })
      .then((data) => {
        if (cancelled) return;
        for (const ev of data.items) seenRef.current.add(ev.id);
        // 首屏也按时间倒序与本地上限走一遍，和后面增量合并的口径一致。
        setBaseEvents(mergeEvents(data.items, []));
        setTotalCount(data.total);
        setFetchedAt(Date.now());
      })
      .catch((reason: unknown) => {
        if (cancelled) return;
        setLoadError(reason instanceof Error ? reason.message : '事件读取失败');
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => { cancelled = true; };
  }, [providerFilter, reloadSeq]);

  /** 降级到轮询：清掉流专属状态，数据回到底稿 + 定时重读。只生效一次。 */
  const degrade = useCallback((note: string) => {
    if (degradedRef.current) return;
    degradedRef.current = true;
    sourceRef.current?.close();
    sourceRef.current = null;
    pausedRef.current = false;
    backlogRef.current = [];
    setBacklogCount(0);
    setPaused(false);
    setMode('poll');
    setConnected(false);
    setStreamNote(note);
    rateRef.current = [];
    setRateCount(0);
  }, []);

  /** 每帧到达都调：清掉降级原因；轮询态收到帧说明流又活了（浏览器重连成功）。 */
  const enterStream = useCallback(() => {
    if (degradedRef.current) return;
    setStreamNote('');
    setMode((current) => (current === 'poll' ? 'live' : current));
  }, []);

  // SSE 订阅。卸载（切页签 / 离开监控中心）即 close()，不留连接。
  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.EventSource !== 'function') {
      degrade('当前浏览器不支持 EventSource');
      return;
    }
    openedRef.current = false;
    setConnected(false);
    const source = new EventSource(streamUrl(), { withCredentials: true });
    sourceRef.current = source;
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
      setConnected(true);
      enterStream();
    };

    source.onopen = () => {
      // 只有 onopen 才点亮 LIVE：正在建连、被 404/HTML 挡住时 onopen 不会来，徽标保持灰。
      openedRef.current = true;
      lastFrame = Date.now();
      setConnected(true);
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
      // 快照不带事件列表（首屏已经走了 REST），这里只确认链路可用。
      payload(event);
    };

    const onEvents = (event: MessageEvent<string>) => {
      touch();
      const rows = parseStreamEventsFrame(payload(event));
      if (rows.length > 0) ingest(rows);
    };

    // 告警 / 探测状态帧不直接携带事件行；probe 落库会另发 events 帧，这里只续命。
    const onAlert = () => touch();
    const onProbe = () => touch();

    // 后端丢了一段（慢消费者 / 快照失败）：resync 之后不会再来一次快照，
    // 主动把已加载的页重读一遍，缺口才补得回来。
    const onResync = () => {
      touch();
      const pages = pageShownRef.current;
      for (let page = 1; page <= pages; page += 1) {
        void fetchPage(page, 'merge').catch(() => undefined);
      }
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
      if (sourceRef.current === source) sourceRef.current = null;
      gotDataRef.current = false;
      pausedRef.current = false;
      backlogRef.current = [];
    };
  }, [attempt, degrade, enterStream, fetchPage, ingest]);

  // 降级后的轮询：15 秒重读第一页（合并进底稿），页面隐藏时跳过。
  useEffect(() => {
    if (mode !== 'poll' || document.hidden) return;
    const refresh = () => {
      void fetchPage(1, 'merge').catch(() => undefined);
    };
    const timer = window.setInterval(refresh, POLL_FALLBACK_MS);
    const onVisible = () => {
      if (!document.hidden) refresh();
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [mode, fetchPage]);

  // 每秒：滑出速率窗口之外的到达时刻，并驱动「x 秒前」这类相对时间文本。
  const [tickNow, setTickNow] = useState(() => Date.now());
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
    streamEventsRef.current = mergeEvents(streamEventsRef.current, backlog);
    setStreamEvents(streamEventsRef.current);
    setFetchedAt(Date.now());
    highlight(backlog.map(eventKey));
  };

  const clearStream = () => {
    const cut = Date.now();
    clearedAtRef.current = cut;
    setClearedAt(cut);
    backlogRef.current = [];
    setBacklogCount(0);
    streamEventsRef.current = [];
    setStreamEvents([]);
    setBaseEvents([]);
    setTotalCount(0);
    setPageShown(1);
    rateRef.current = [];
    setRateCount(0);
    for (const timer of freshTimersRef.current.values()) window.clearTimeout(timer);
    freshTimersRef.current.clear();
    highlightRef.current.clear();
    setFreshKeys(new Set());
  };

  const loadMore = () => {
    setLoadingMore(true);
    void fetchPage(pageShown + 1, 'merge')
      .catch((reason: unknown) => setLoadError(reason instanceof Error ? reason.message : '事件加载失败'))
      .finally(() => setLoadingMore(false));
  };

  // 探测期间选择器保持打开（行内显示「探测中…」），成功后再收起、由外层弹结果；
  // 请求本身失败就留在列表里给一句原因，不把人甩回事件流。
  const run = (providerId: string) => {
    setRunError('');
    void onRunProbe(providerId).then((data) => {
      if (data) setPickerOpen(false);
      else setRunError(`${nameOf(providerId)}：探测请求失败，请稍后重试`);
    });
  };

  // 合并后的事件：底稿 ∪ 增量，再按本地清空分界过滤。
  const views = useMemo(() => {
    const rows = mergeEvents(baseEvents, streamEvents);
    return clearedAt > 0 ? rows.filter((ev) => Date.parse(ev.time) > clearedAt) : rows;
  }, [baseEvents, streamEvents, clearedAt]);

  const counts = useMemo(() => {
    const map: Record<EventCategory, number> = { all: views.length, probe: 0, alert: 0, renewal: 0, checkin: 0, system: 0 };
    for (const ev of views) map[categoryOf(ev.kind)] += 1;
    return map;
  }, [views]);

  const keyword = search.trim().toLowerCase();
  const filteredEvents = useMemo(() => {
    return views.filter((ev) => {
      if (category !== 'all' && categoryOf(ev.kind) !== category) return false;
      if (!keyword) return true;
      const haystack = `${ev.title}\n${ev.targetName ?? ''}\n${ev.ruleKey ?? ''}\n${payloadSegments(ev).join(' ')}`.toLowerCase();
      return haystack.includes(keyword);
    });
  }, [views, category, keyword]);

  // 置顶锁定：新的顶栏条目出现时滚回顶部；取消勾选后完全不碰滚动。
  const lastTopIdRef = useRef(0);
  useEffect(() => {
    const top = filteredEvents[0];
    if (!top) return;
    if (top.id === lastTopIdRef.current) return;
    lastTopIdRef.current = top.id;
    if (!pinnedRef.current || pausedStateRef.current) return;
    const el = viewportRef.current;
    if (el) el.scrollTo({ top: 0, behavior: 'auto' });
  }, [filteredEvents]);

  const newestStamp = views[0] ? displayTime(views[0].time, timezone) : null;
  const hasMore = pageShown * EVENT_PAGE_SIZE < totalCount;
  const pickerKeyword = pickerSearch.trim().toLowerCase();
  const pickerAccounts = pickerKeyword
    ? probeAccounts.filter((account) => account.name.toLowerCase().includes(pickerKeyword))
    : probeAccounts;

  const emptyTitle = views.length === 0
    ? providerFilter ? `${nameOf(providerFilter)} 24h 内没有事件` : '24h 内还没有事件'
    : '这个筛选条件下没有事件';
  const emptyNote = views.length === 0
    ? '轻探测、告警、续期和系统采集写入后会实时出现在这里。'
    : '换个分类或清空搜索词再试。';

  return (
    <div className="mon-ev">
      {(error || loadError) && (
        <div className="mon-overview-error mon-ev-error" role="alert">
          <span>{loadError || error}</span>
          {loadError && (
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => setReloadSeq((value) => value + 1)}
            >
              {'重试'}
            </button>
          )}
        </div>
      )}

      {/* ① 控制条：LIVE 状态 + 暂停 + 两种视图 + 置顶 + 手动探测 + 清空（对齐参考的 Living Stream Pipeline）。 */}
      <div className="mon-ev-bar mon-probe-bar">
        <div className="mon-probe-bar-main" role="group" aria-label="事件实时流状态">
          <div
            className={`mon-probe-live${mode === 'live' && connected ? ' on' : ' off'}`}
            role="status"
            aria-live="polite"
            title={mode === 'poll'
              ? `实时流不可用${streamNote ? `（${streamNote}）` : ''}，已按 ${POLL_FALLBACK_MS / 1000} 秒轮询取数`
              : mode === 'paused'
                ? '连接保持中，新事件已暂存，点「继续采集」一次补上'
                : connected ? 'SSE 实时流已连上' : '正在建立 SSE 连接，还没收到数据'}
          >
            {mode === 'live' && connected
              ? <span className="mon-probe-pulse" aria-hidden />
              : <span className="mon-probe-ind" aria-hidden />}
            <span className="mon-probe-live-text">{
              mode === 'live'
                ? connected ? 'LIVE TAIL 流式采集' : '正在连接实时流'
                : mode === 'paused' ? 'STREAM PAUSED'
                : '轮询模式'
            }</span>
            {mode !== 'live' || connected
              ? <span className="mon-probe-rate" title="最近 5 秒收到的事件速率">{`${rateText(rateCount)} 条/秒`}</span>
              : null}
            {mode === 'paused' && backlogCount > 0 && (
              <span className="mon-probe-pending" title="恢复后一次补上">{`+${backlogCount} 待上屏`}</span>
            )}
            {mode === 'poll' && streamNote && <span className="mon-probe-note">{streamNote}</span>}
          </div>
          <button
            type="button"
            className={`btn btn-sm mon-probe-pause${mode === 'paused' ? ' on' : ''}`}
            aria-pressed={mode === 'paused'}
            onClick={togglePaused}
            title={mode === 'poll' ? '重新尝试连接实时流' : paused ? '把暂停期间暂存的事件一次补上' : '停止上屏新事件，连接保持'}
          >
            {mode === 'poll' ? '重试实时流' : mode === 'paused' ? '继续采集' : '暂停采集'}
          </button>
          <div className="mon-ev-seg" role="group" aria-label="事件视图">
            <button
              type="button"
              className={view === 'pipeline' ? 'on' : ''}
              aria-pressed={view === 'pipeline'}
              onClick={() => setView('pipeline')}
            >
              {'脉冲流图'}
            </button>
            <button
              type="button"
              className={view === 'terminal' ? 'on' : ''}
              aria-pressed={view === 'terminal'}
              onClick={() => setView('terminal')}
            >
              {'极速终端'}
            </button>
          </div>
          <Switch checked={pinned} label="置顶锁定最新" onChange={setPinned} />
          <span className="mon-probe-meta" title={newestStamp?.title ?? '还没有事件'}>
            {newestStamp ? `最新事件 ${newestStamp.text}` : '最新事件 —'}
          </span>
        </div>
        <div className="mon-probe-bar-actions" role="group" aria-label="事件流操作">
          <button
            type="button"
            className="btn btn-sm primary"
            onClick={() => setPickerOpen(true)}
            title="选一个账号立即发一发最小对话探测"
          >
            {'手动触发探测'}
          </button>
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

      {/* ② 分类页签（带计数）+ 搜索：与参考的 Category Filter Tabs 同构。 */}
      <div className="mon-ev-filters">
        <div
          className="mon-ev-cats"
          role="group"
          aria-label="按事件类型筛选"
          title="计数只统计已载入的 24h 事件（不含尚未加载的历史页）"
        >
          {EVENT_CATEGORIES.map((item) => {
            const active = category === item.id;
            return (
              <button
                key={item.id}
                type="button"
                className={`mon-ev-cat${item.id === 'renewal' ? ' is-renewal' : ''}${active ? ' on' : ''}`}
                aria-pressed={active}
                onClick={() => setCategory(item.id)}
              >
                <span>{item.label}</span>
                <span className="mon-ev-cat-count">{counts[item.id]}</span>
              </button>
            );
          })}
        </div>
        <label className="mon-ev-search">
          <svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true">
            <circle cx="11" cy="11" r="7" />
            <path d="m20 20-3.5-3.5" />
          </svg>
          <input
            type="search"
            placeholder="搜索账号 / 标题 / payload 参数"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
            aria-label="搜索事件"
          />
        </label>
      </div>

      {/* ③ 事件流视窗：脉冲流图（左侧光轨 + 卡片）或极速终端。框内滚动，页面不被撑长。 */}
      <div className="mon-ev-viewport" ref={viewportRef}>
        {loading && <div className="mon-ev-empty"><span className="mon-ev-empty-title">{'正在读取 24h 事件…'}</span></div>}
        {!loading && filteredEvents.length === 0 && (
          <div className="mon-ev-empty">
            <span className="mon-ev-empty-title">{emptyTitle}</span>
            <span>{emptyNote}</span>
          </div>
        )}
        {!loading && filteredEvents.length > 0 && view === 'pipeline' && (
          <div className="mon-ev-stream">
            <span className="mon-ev-rail" aria-hidden />
            <div className="mon-ev-list">
              {filteredEvents.map((ev) => {
                const cat = categoryOf(ev.kind);
                const sev = severityOf(ev);
                const stamp = displayTime(ev.time, timezone);
                const metrics = eventMetrics(ev, timezone);
                const segments = payloadSegments(ev);
                const fresh = freshKeys.has(eventKey(ev));
                return (
                  <article className={`mon-ev-card k-${cat} s-${sev.tone}${fresh ? ' fresh' : ''}`} key={ev.id}>
                    <span className="mon-ev-bead" aria-hidden />
                    <div className="mon-ev-card-head">
                      <span className="mon-ev-time" title={stamp.title}>{displayTimeCompact(stamp.text)}</span>
                      <span className={`mon-ev-sev ${sev.tone}`}>{sev.label}</span>
                      <span className={`mon-ev-kind k-${cat}`}>{KIND_LABEL[ev.kind] ?? ev.kind}</span>
                      <span className="mon-ev-title">{ev.title}</span>
                      {ev.targetName && (
                        <button
                          type="button"
                          className="mon-ev-target"
                          onClick={() => onFilter(ev.targetId ?? '')}
                          title={`只看 ${ev.targetName} 的事件`}
                        >
                          <BrandIcon icon={ev.targetId ? iconOf?.(ev.targetId) : undefined} name={ev.targetName} size={14} />
                          <span>{ev.targetName}</span>
                        </button>
                      )}
                    </div>
                    {metrics.length > 0 && (
                      <div className="mon-ev-metrics">
                        {metrics.map((metric) => (
                          <div className="mon-ev-metric" key={metric.label}>
                            <span className="mon-ev-metric-label">{metric.label}</span>
                            <span className={`mon-ev-metric-value${metric.tone ? ` ${metric.tone}` : ''}`}>{metric.value}</span>
                          </div>
                        ))}
                      </div>
                    )}
                    {segments.length > 0 && (
                      <div className="mon-ev-payload">
                        <span className="mon-ev-payload-prompt" aria-hidden>&gt;</span>
                        <span className="mon-ev-segs">
                          {segments.map((segment, index) => (
                            <span
                              className={`mon-ev-seg${segment.startsWith('error=') ? ' err' : ''}`}
                              key={`${segment}-${index}`}
                            >
                              {segment}
                            </span>
                          ))}
                        </span>
                      </div>
                    )}
                  </article>
                );
              })}
            </div>
          </div>
        )}
        {!loading && filteredEvents.length > 0 && view === 'terminal' && (
          <div className="mon-ev-terminal">
            <div className="mon-ev-term-head">
              <span>{`# NEXUS_GATEWAY_TELEMETRY_TAIL_SOCKET // ${
                mode === 'live' ? (connected ? 'CONNECTED' : 'CONNECTING') : mode === 'paused' ? 'PAUSED' : 'POLLING'
              }`}</span>
              <span>{`RATE: ${rateText(rateCount)} ops/sec`}</span>
            </div>
            {filteredEvents.map((ev) => {
              const sev = severityOf(ev);
              const cat = categoryOf(ev.kind);
              return (
                <div className="mon-ev-term-row" key={ev.id}>
                  <span className="mon-ev-term-time">{`[${displayTimeCompact(displayTime(ev.time, timezone).text)}]`}</span>
                  <span className={`mon-ev-term-sev ${sev.tone}`}>{`[${sev.label}]`}</span>
                  <span className={`mon-ev-term-kind k-${cat}`}>{`[${KIND_LABEL[ev.kind] ?? ev.kind}]`}</span>
                  <span className="mon-ev-term-title">{ev.title}</span>
                  <span className="mon-ev-term-detail">{payloadSegments(ev).join(' ')}</span>
                </div>
              );
            })}
          </div>
        )}
      </div>

      {/* ④ 脚注：窗口口径、已载入/总数、按账号筛选与加载更多。 */}
      <div className="mon-ev-foot">
        <span title="REST 读 24h 最近一页，SSE 增量按 id 去重合并；清空只影响本页">
          {`24h 窗口 · 已载入 ${views.length} 条${totalCount > views.length ? ` / 共 ${totalCount} 条` : ''}${clearedAt > 0 ? ' · 已本地清空旧条目' : ''} · 取数于 ${displayTime(new Date(fetchedAt).toISOString(), timezone).text}`}
        </span>
        <div className="mon-ev-foot-actions">
          {providerFilter && (
            <button type="button" className="btn btn-sm" onClick={() => onFilter('')}>
              {'查看全部账号'}
            </button>
          )}
          {hasMore && (
            <button type="button" className="btn btn-sm" disabled={loadingMore} onClick={loadMore}>
              {loadingMore ? '加载中…' : '加载更多'}
            </button>
          )}
        </div>
      </div>

      {pickerOpen && (
        <Modal
          title="手动触发轻探测"
          description="最小对话（max_tokens=16），不写请求日志，不进账号成功率与成本；失败只进入 probe_failed 规则。"
          onClose={() => setPickerOpen(false)}
        >
          <div className="mon-ev-picker">
            <input
              className="monitoring-input"
              type="search"
              placeholder="搜索账号"
              value={pickerSearch}
              onChange={(event) => setPickerSearch(event.target.value)}
              aria-label="搜索账号"
            />
            <div className="mon-ev-picker-list">
              {pickerAccounts.length === 0 && <div className="muted-text">{'没有可探测的账号。'}</div>}
              {pickerAccounts.map((account) => {
                const probing = busy === `probe:${account.providerId}`;
                const last = account.probed ? displayTime(account.lastProbeAt, timezone) : null;
                return (
                  <div className="mon-ev-picker-row" key={account.providerId}>
                    <BrandIcon icon={iconOf?.(account.providerId)} name={account.name} size={18} />
                    <span className="mon-ev-picker-name" title={account.name}>{account.name}</span>
                    <span className="mon-ev-picker-meta" title={last?.title ?? '还没有可判定的探测'}>
                      {account.probed ? `上次 ${last ? displayTimeCompact(last.text) : '—'}` : '未探测'}
                    </span>
                    <span className="mon-ev-picker-meta">
                      {account.maxPerDay === 0 ? `今日 ${account.usedToday} / 不限` : `今日 ${account.usedToday} / ${account.maxPerDay}`}
                    </span>
                    <button
                      type="button"
                      className="btn btn-sm"
                      disabled={busy !== ''}
                      onClick={() => run(account.providerId)}
                      title={account.disabled ? '账号已停用；探测会记为「账号已停用，未探测」' : '现在就发一发最小对话探测'}
                    >
                      {probing ? '探测中…' : '立即探测'}
                    </button>
                  </div>
                );
              })}
            </div>
            {runError && <div className="metric-note err" role="alert">{runError}</div>}
            <p className="mon-ev-picker-note" title="探测调度参数在「设置与调度」里调整">
              {'探测调度参数（每日上限 / 最小间隔 / 窗口）在「设置与调度」里调整。'}
            </p>
          </div>
        </Modal>
      )}

      {probeResult && (
        <Modal
          title={`探测结果：${nameOf(probeResult.providerId)}`}
          description="手动探测只读取结果，不会改动账号配置或调度状态"
          onClose={onDismissResult}
        >
          <ProbeResultBody result={probeResult.data} />
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
