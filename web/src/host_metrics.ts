// 机器状态的实时采样与历史。
//
// 数据只来自服务端的 /__host-metrics，前端不做任何推算：同一份快照既驱动顶部
// 指标卡，也喂给趋势图，所以卡片上的数字与图的最后一个点永远一致。
//
// 历史保存在内存里并落 localStorage：刷新页面、切换页签回来都能接上前 60 分钟，
// 不必等图重新画满。断线（页面隐藏、网络中断、服务端采样暂停）超过 gapFactor 倍
// 采样间隔的点之间会留出缺口，不连成一条假直线。
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

/** 相邻两点间隔超过容差即视为断档——图上断开，而不是连一条直线。 */
export function isGap(prev: HostPoint | undefined, next: HostPoint, pollMs: number): boolean {
  if (!prev) return false;
  return next.t - prev.t > pollMs * MAX_GAP_FACTOR;
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

/**
 * 切到别的页签就停采样，切回来立即补一帧。
 * 判定用 hasFocus() 而不是 visibilityState：只要页面所在窗口在前台就继续采，
 * 即使浏览器把页面标成 hidden（内嵌浏览器、最小化后恢复、部分桌面壳都会这样），
 * 否则界面会一直停在「读取中」。真正切走了，hasFocus() 会立刻变 false。
 */
export function shouldSample(): boolean {
  if (typeof document === 'undefined') return true;
  return document.visibilityState !== 'hidden' || document.hasFocus();
}

export function hostPollMs(): number {
  return hostMetricsPollMs();
}
