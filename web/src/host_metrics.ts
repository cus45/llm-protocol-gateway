// 机器状态的实时推送与历史。
//
// 数据只来自服务端的 /__host-metrics/stream，前端不做任何推算：同一份快照既驱动
// 顶部指标卡，也喂给趋势图，所以卡片上的数字与图的最后一个点永远一致。
//
// 连接建起来时服务端先补一整窗历史（网关自己一直在采样、也落库），之后每出一个
// 采样推一帧，所以曲线是连续的，不依赖这个浏览器有没有开着页面。EventSource 自带
// 断线重连与 Last-Event-ID 续传；实在建不起流（反代缓冲、企业代理）才退回轮询。
// localStorage 只作离线兜底：流还没连上时先画出上次看到的形状。
import {API_BASE, fetchWithTimeout, hostMetricsPollMs} from './lib';
import type {HostMetrics} from './types';

export const HOST_WINDOW_MINUTES = 60;
const STORAGE_KEY = 'gateway:host-history';
const MAX_GAP_FACTOR = 4;

export type HostPoint = {
  t: number; // 采样时间（毫秒）
  cpu: number;
  load: number;
  memUsed: number;
  memTotal: number;
  memPercent: number;
  processCpu: number;
  diskRead: number;
  diskWrite: number;
  netRx: number;
  netTx: number;
  goroutines: number;
  rss: number;
  fds: number;
};

export type HostHistory = {
  points: HostPoint[];
  last: HostMetrics | null;
  lastAt: number;
};

export function pointFromMetrics(m: HostMetrics, now: number): HostPoint {
  const at = m.collectedAt ? Date.parse(m.collectedAt) : now;
  return {
    t: Number.isFinite(at) ? at : now,
    cpu: m.cpuPercent ?? 0,
    load: m.load1 ?? 0,
    memUsed: m.memUsed ?? 0,
    memTotal: m.memTotal ?? 0,
    memPercent: m.memPercent ?? 0,
    processCpu: m.processCpuPercent ?? 0,
    diskRead: m.diskIO?.readRate ?? m.diskReadRate ?? 0,
    diskWrite: m.diskIO?.writeRate ?? m.diskWriteRate ?? 0,
    netRx: m.netRxRate ?? 0,
    netTx: m.netTxRate ?? 0,
    goroutines: m.goroutines ?? 0,
    rss: m.processRssMiB ?? 0,
    fds: m.processFds ?? 0,
  };
}

/** 按时间窗截断；同一时间戳重复到达（轮询重叠）只保留最新一份。 */
export function appendPoint(points: HostPoint[], next: HostPoint, now: number): HostPoint[] {
  const cutoff = now - (HOST_WINDOW_MINUTES + 5) * 60_000;
  const kept = points.filter((point) => point.t >= cutoff && point.t !== next.t);
  if (kept.length > 0 && kept[kept.length - 1].t > next.t) {
    // 服务端时钟回拨：重排而不是把旧点当最新点接在结尾。
    kept.push(next);
    kept.sort((a, b) => a.t - b.t);
    return kept;
  }
  kept.push(next);
  return kept;
}

/**
 * 相邻两点间隔超过容差即视为断档——图上断开，而不是连一条直线。
 *
 * 容差不能只按页面的轮询间隔算：网关在没人看的时候是降频采样（15 秒一档），
 * 那段时间在图上本来就是稀疏的正常数据。真正值得断开的只有「网关自己停了」——
 * 重启、进程卡住、机器挂起——所以底线放到 45 秒。
 */
export function isGap(prev: HostPoint | undefined, next: HostPoint, pollMs: number): boolean {
  if (!prev) return false;
  return next.t - prev.t > Math.max(pollMs * MAX_GAP_FACTOR, 45_000);
}

export function loadHistory(): HostHistory {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return {points: [], last: null, lastAt: 0};
    const parsed = JSON.parse(raw) as Partial<HostHistory>;
    const points = Array.isArray(parsed.points) ? parsed.points.filter(isPoint) : [];
    const last = (parsed.last ?? null) as HostMetrics | null;
    const lastAt = typeof parsed.lastAt === 'number' ? parsed.lastAt : 0;
    return {points, last, lastAt};
  } catch {
    return {points: [], last: null, lastAt: 0};
  }
}

function isPoint(value: unknown): value is HostPoint {
  if (!value || typeof value !== 'object') return false;
  const point = value as HostPoint;
  return typeof point.t === 'number' && Number.isFinite(point.t) && typeof point.cpu === 'number';
}

export function saveHistory(history: HostHistory): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(history));
  } catch {
    // 隐私模式或配额满：这一次刷新不再恢复历史即可，不影响实时采样。
  }
}

export type HostSampleResult =
  | {status: 'ok'; metrics: HostMetrics}
  | {status: 'unauthorized'}
  | {status: 'error'};

export async function fetchHostMetrics(signal?: AbortSignal): Promise<HostSampleResult> {
  try {
    const response = await fetchWithTimeout(`${API_BASE}/__host-metrics`, {credentials: 'include', signal});
    if (response.status === 401 || response.status === 403) return {status: 'unauthorized'};
    if (!response.ok) return {status: 'error'};
    return {status: 'ok', metrics: (await response.json()) as HostMetrics};
  } catch {
    return {status: 'error'};
  }
}

export type HostDiagnosis = {
  verdict: string;
  severity: 'ok' | 'info' | 'warn' | 'critical';
  windowMs: number;
  findings: Array<{id: string; severity: string; title: string; detail: string; action?: string}>;
  gateway: {
    version?: string;
    goroutines: number;
    inFlight: number;
    requestRate: number;
    requestsDelta: number;
  };
  evaluatedAt: string;
};

export type HostSnapshotResponse = {
  metrics: HostMetrics;
  previous: HostMetrics;
  intervalMs: number;
  diagnosis?: HostDiagnosis;
};

/** 一次性 A/B 采样：报告用，不写历史，也不打扰后台采样器的节奏。 */
export async function fetchHostSnapshot(withDiagnosis: boolean): Promise<HostSnapshotResponse> {
  const response = await fetchWithTimeout(
    `${API_BASE}/__host-metrics/snapshot${withDiagnosis ? '?diagnose=1' : ''}`,
    {credentials: 'include'},
    15_000,
  );
  if (!response.ok) {
    throw new Error(response.status === 403 ? '没有查看机器状态的权限' : `采集失败（HTTP ${response.status}）`);
  }
  return (await response.json()) as HostSnapshotResponse;
}

export function hostDiagnosticsURL(): string {
  return `${API_BASE}/__host-diagnostics`;
}

export const HOST_STREAM_PATH = '/__host-metrics/stream';

/** 服务端下发的采样行（与 store.HostMetricSample 同名同义，单位见后端注释）。 */
export type HostSamplePoint = {
  at: number;
  cpu: number;
  load: number;
  memUsed: number;
  memTotal: number;
  memPercent: number;
  processCpu: number;
  diskRead: number;
  diskWrite: number;
  netRx: number;
  netTx: number;
  goroutines: number;
  rss: number;
  fds: number;
};

export function pointFromSample(sample: HostSamplePoint): HostPoint {
  return {
    t: sample.at,
    cpu: sample.cpu ?? 0,
    load: sample.load ?? 0,
    memUsed: sample.memUsed ?? 0,
    memTotal: sample.memTotal ?? 0,
    memPercent: sample.memPercent ?? 0,
    processCpu: sample.processCpu ?? 0,
    diskRead: sample.diskRead ?? 0,
    diskWrite: sample.diskWrite ?? 0,
    netRx: sample.netRx ?? 0,
    netTx: sample.netTx ?? 0,
    goroutines: sample.goroutines ?? 0,
    rss: sample.rss ?? 0,
    fds: sample.fds ?? 0,
  };
}

export type HostStreamStatus = 'connecting' | 'live' | 'reconnecting' | 'polling' | 'forbidden';

export type HostStreamHandlers = {
  onSnapshot: (stream: {points: HostPoint[]; latest: HostMetrics | null; intervalMs: number}) => void;
  onSample: (point: HostPoint, latest: HostMetrics | null) => void;
  onStatus: (status: HostStreamStatus) => void;
};

/**
 * 订阅机器状态流。
 *
 * 返回的 stop 会关闭连接并停掉一切定时器。EventSource 出错时浏览器自己重连
 * （带 Last-Event-ID），但有两种情况它永远好不了：403（这个角色没有
 * machine.view）和反代把 text/event-stream 缓冲成一次性响应。前者直接停，
 * 后者在连续失败后降级成轮询，保证页面至少还在动。
 */
export function openHostStream(handlers: HostStreamHandlers): () => void {
  let source: EventSource | null = null;
  let pollTimer = 0;
  let retryTimer = 0;
  let failures = 0;
  let stopped = false;
  let latest: HostMetrics | null = null;
  let lastPointAt = 0;

  const stopPolling = () => {
    if (pollTimer) window.clearInterval(pollTimer);
    pollTimer = 0;
  };

  const startPolling = () => {
    if (stopped || pollTimer) return;
    handlers.onStatus('polling');
    const tick = async () => {
      if (stopped) return;
      const result = await fetchHostMetrics();
      if (result.status === 'unauthorized') {
        handlers.onStatus('forbidden');
        cleanup();
        return;
      }
      if (result.status !== 'ok') return;
      latest = result.metrics;
      const sample = pointFromMetrics(result.metrics, Date.now());
      // 轮询拿到的是一次实时快照；只要比已有数据新就当一个采样点用。
      if (sample.t > lastPointAt) {
        lastPointAt = sample.t;
        handlers.onSample(sample, latest);
      }
    };
    void tick();
    pollTimer = window.setInterval(() => void tick(), hostMetricsPollMs());
  };

  const connect = () => {
    if (stopped) return;
    handlers.onStatus(source ? 'reconnecting' : 'connecting');
    const url = `${API_BASE}${HOST_STREAM_PATH}`;
    source = new EventSource(url, {withCredentials: true});

    source.addEventListener('snapshot', (event) => {
      failures = 0;
      const payload = JSON.parse((event as MessageEvent<string>).data) as {
        points?: HostSamplePoint[];
        latest?: HostMetrics;
        intervalMs?: number;
      };
      stopPolling();
      latest = payload.latest ?? null;
      const points = (payload.points ?? []).map(pointFromSample);
      if (points.length > 0) lastPointAt = points[points.length - 1].t;
      handlers.onStatus('live');
      handlers.onSnapshot({points, latest, intervalMs: payload.intervalMs ?? 0});
    });

    source.addEventListener('sample', (event) => {
      failures = 0;
      const payload = JSON.parse((event as MessageEvent<string>).data) as {
        point?: HostSamplePoint;
        latest?: HostMetrics;
      };
      if (!payload.point) return;
      if (payload.latest) latest = payload.latest;
      const point = pointFromSample(payload.point);
      if (point.t <= lastPointAt) return;
      lastPointAt = point.t;
      handlers.onStatus('live');
      handlers.onSample(point, payload.latest ?? null);
    });

    source.addEventListener('resync', () => {
      // 队列被挤掉了一段：关掉重连，snapshot 会把整窗补回来。
      source?.close();
      source = null;
      retryTimer = window.setTimeout(connect, 800);
    });

    source.onerror = () => {
      if (stopped) return;
      failures += 1;
      // EventSource 的 readyState=CLOSED 表示它放弃了（4xx / 代理返回非流）。
      const dead = source?.readyState === EventSource.CLOSED;
      source?.close();
      source = null;
      if (dead) {
        if (failures >= 2) {
          // 连续失败：先降级成轮询把页面养活，再定期试一次流。20 秒足够短，
          // 代理抖动恢复后不用干等；成本也只是一个被拒的请求。
          startPolling();
          retryTimer = window.setTimeout(() => {
            stopPolling();
            connect();
          }, 20_000);
          return;
        }
        handlers.onStatus('reconnecting');
        retryTimer = window.setTimeout(connect, 3000);
        return;
      }
      handlers.onStatus('reconnecting');
    };
  };

  const cleanup = () => {
    stopped = true;
    if (retryTimer) window.clearTimeout(retryTimer);
    stopPolling();
    source?.close();
    source = null;
  };

  // 页面在后台时浏览器会把连接挂起，切回来时如果已经断了立刻重连一次。
  const onVisible = () => {
    if (stopped) return;
    if (document.visibilityState === 'visible' && (!source || source.readyState === EventSource.CLOSED)) {
      if (retryTimer) window.clearTimeout(retryTimer);
      connect();
    }
  };
  document.addEventListener('visibilitychange', onVisible);

  connect();
  return () => {
    document.removeEventListener('visibilitychange', onVisible);
    cleanup();
  };
}

