import type { MonitorProbeAccount, MonitorQuota, MonitorSeverity } from './types';
import { hasTime, quotaAbsoluteKnown, quotaRatioKnown } from './types';

/** 级别 → 现有 badge 色调（状态不能只靠颜色，文本永远同现）。 */
export const SEVERITY_TONE: Record<MonitorSeverity, string> = {
  p0: 'red',
  p1: 'amber',
  p2: 'blue',
  info: 'slate',
};

export const SEVERITY_LABEL: Record<MonitorSeverity, string> = {
  p0: 'P0',
  p1: 'P1',
  p2: 'P2',
  info: '信息',
};

/**
 * 按监控中心的时区口径显示时刻（spec §15：调度与展示同一个
 * `monitor_dispatch.timezone`，不依赖进程 TZ），悬停给浏览器本地时间。
 */
export function displayTime(raw: string | undefined, timeZone: string): { text: string; title: string } {
  if (!hasTime(raw)) return { text: '—', title: '没有可用时间' };
  const stamp = Date.parse(raw as string);
  if (Number.isNaN(stamp)) return { text: '—', title: String(raw) };
  const date = new Date(stamp);
  const options: Intl.DateTimeFormatOptions = {
    timeZone, hour12: false, month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
  };
  let zone = timeZone || 'UTC';
  let formatted: string;
  try {
    formatted = new Intl.DateTimeFormat('zh-CN', options).format(date);
  } catch {
    // 未知时区名（后端配置被手改坏）不能整页崩，退回 UTC。
    zone = 'UTC';
    formatted = new Intl.DateTimeFormat('zh-CN', { ...options, timeZone: 'UTC' }).format(date);
  }
  return { text: `${formatted} ${zone}`, title: date.toLocaleString() };
}

/** 距今多久（负值=未来）。 */
export function relativeToNow(raw: string | undefined, now: number): string {
  if (!hasTime(raw)) return '—';
  const delta = (Date.parse(raw as string) - now) / 1000;
  const abs = Math.abs(delta);
  const unit = abs >= 86400 ? `${Math.floor(abs / 86400)} 天`
    : abs >= 3600 ? `${Math.floor(abs / 3600)} 小时`
    : abs >= 60 ? `${Math.floor(abs / 60)} 分钟` : `${Math.round(abs)} 秒`;
  return delta >= 0 ? `${unit}后` : `${unit}前`;
}

export function formatRatio(ratio: number | undefined): string {
  if (ratio === undefined || ratio < 0) return '—';
  const percent = ratio * 100;
  return `${percent.toFixed(percent > 99.5 || percent < 10 ? 1 : 0)}%`;
}

/**
 * 额度文案。三种情况必须分开，混起来就是把「不知道」显示成「用完」：
 * 读数不可用 / 只有比率（采样表只存比率）/ 有绝对值。
 */
export function quotaText(quota?: MonitorQuota | null): { primary: string; note: string } {
  if (!quota) return { primary: '不适用', note: '该厂商没有额度接口' };
  if (quota.error) return { primary: '读取失败', note: `分类：${quota.error}` };
  if (!quotaRatioKnown(quota)) return { primary: '—', note: '没有可用的额度读数' };
  const ratio = formatRatio(quota.ratio);
  if (!quotaAbsoluteKnown(quota)) {
    return { primary: `剩余 ${ratio}`, note: '接口只给比例，没有绝对值' };
  }
  return { primary: `剩余 ${ratio}`, note: `${Math.round(quota.remaining)} / ${Math.round(quota.limit)}` };
}

/** 延迟：秒级保留一位小数，毫秒级不写「0.0s」这种看不出量级的形态。 */
export function formatLatency(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '—';
  if (ms < 1000) return `${Math.round(ms)}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

/** 详情里的标量证据：只渲染数字/布尔/字符串，绝不 stringify 整个对象。 */
export function detailPairs(detail: Record<string, unknown> | undefined, limit = 6): Array<[string, string]> {
  if (!detail) return [];
  const out: Array<[string, string]> = [];
  for (const [key, value] of Object.entries(detail)) {
    if (key === 'timeline') continue;
    if (typeof value === 'number') {
      out.push([key, Number.isInteger(value) ? String(value) : value.toFixed(3)]);
    } else if (typeof value === 'boolean' || typeof value === 'string') {
      const text = String(value);
      if (text) out.push([key, text]);
    } else if (Array.isArray(value)) {
      out.push([key, `${value.length} 项`]);
    }
    if (out.length >= limit) break;
  }
  return out;
}

/**
 * 账号的探测结论。probed=false 是「还没探过」，跳过（如 no_model）也不是失败，
 * 三种情况必须分开写，否则零值会被读成「探测全过」。
 *
 * consecutiveFailures 是历史 streak（成功才归零），LastStatus 是最近一发的结果：
 * 两者要分开讲，否则失败之后又跑通一次，会出现「通过 · 上游状态 401」这种自相矛盾的行。
 */
export function probeSummaryText(account: MonitorProbeAccount): string {
  // 注意：当前后端一律不把 skipped 回填到账号行——MonitorProbeOutcome 对 skipped
  // 行直接 continue 并置空（internal/store/monitoring.go:981-997），in_flight 也不落库。
  // 这里保留分支是给后端补齐之后用的；不要用它判断「为什么没探」，那种判断恒为假。
  if (!account.probed) {
    if (account.skipped === 'no_model') return '没有可用模型，未发出请求';
    if (account.skipped === 'in_flight') return '探测正在进行中';
    if (account.skipped) return `未成功执行（${account.skipped}）`;
    return '暂无数据';
  }
  const last = account.lastStatus ? ` · 最近状态 ${account.lastStatus}` : '';
  if (account.consecutiveFailures > 0) return `连续失败 ${account.consecutiveFailures} 次${last}`;
  return `通过${last}`;
}
