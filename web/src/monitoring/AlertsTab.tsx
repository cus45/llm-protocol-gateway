import { useState } from 'react';
import { Modal } from '../components/ui';
import { copyToClipboard } from '../lib';
import type { MonitorAlert, MonitorAlertDetail, MonitorChannelView, MonitorSeverity, MonitorTimelineEntry } from './types';
import { ERROR_KIND_TEXT, SEVERITY_TEXT, hasTime } from './types';
import { detailPairs, displayTime, displayTimeCompact, relativeToNow } from './format';

export interface AlertsTabProps {
  alerts: MonitorAlert[];
  total: number;
  statusFilter: string;
  severityFilter: MonitorSeverity | '';
  /** 服务端合计口径：进行中 / 已恢复（不受当前筛选影响），给筛选条括号里的数字用。 */
  firingTotal: number;
  resolvedTotal: number;
  timezone: string;
  /** 重试推送要选渠道，所以告警页也要拿到渠道列表（只有 id/name/type 脱敏视图）。 */
  channels: MonitorChannelView[];
  detail: MonitorAlertDetail | null;
  busy: string;
  error: string;
  onStatusFilter: (status: string) => void;
  onSeverityFilter: (severity: MonitorSeverity | '') => void;
  onOpen: (id: number) => void;
  onClose: () => void;
  onAck: (id: number) => void;
  onIgnore: (id: number) => void;
  onRetryPush: (eventId: number, channelId: string) => void;
  onOpenAccount: (providerId: string) => void;
  onLoadMore: () => void;
}

/** 状态胶囊。空串 = 不筛状态（全部），与 MonitoringPage 的 status 参数对齐。 */
const STATUS_CHIPS = [
  { id: 'firing', label: '进行中', title: '条件仍成立或已超过冷却仍未恢复的告警' },
  { id: 'resolved', label: '已恢复', title: '条件已恢复、或读数不可用而收起的告警' },
  { id: '', label: '全部', title: '不按状态筛选' },
];

const SEVERITY_CHIPS: Array<MonitorSeverity | ''> = ['', 'p0', 'p1', 'p2'];

const PUSH_LABELS: Record<string, string> = {
  sent: '已推送',
  failed: '推送失败',
  partial: '部分成功',
  pending: '待发送',
  skipped: '未推送',
  suppressed: '已抑制',
};

/** 恢复原因：后端只给 recovered / data_unavailable / legacy 三种，原样输出英文不读得懂。 */
const RESOLUTION_TEXT: Record<string, string> = {
  recovered: '条件已恢复',
  data_unavailable: '读数不可用，先行收起',
  legacy: '历史导入',
};

/**
 * 规则 key → 中文名。告警只带 ruleKey（旧版用点号 quota.auth_failed，新版用下划线），
 * 界面上出现裸 key 等于把排障细节丢给值班同学。映射不到时整句给「未知规则」，
 * 原 key 只留在悬停里——绝不把它当标题输出。与后端 `/__monitor/rules` 的 name 保持一致。
 */
const RULE_LABELS: Record<string, string> = {
  auth_invalid: '凭证失效',
  checkin_failed: '签到连续失败',
  collector_stale: '监控采集器停滞',
  concurrent_ip: 'API Key 并发 IP',
  credit_expiring: '积分即将到期',
  empty_response: '空响应',
  key_budget_exhausted: '密钥额度耗尽',
  key_budget_warn: '密钥额度预警',
  model_unavailable: '模型列表拉取失败',
  multi_ip: 'API Key 多 IP 使用',
  oauth_expiring: '凭证即将过期',
  oauth_refresh_failed: '凭证连续续期失败',
  probe_failed: '轻探测失败',
  provider_down: 'Provider 成功率过低',
  quota_low: '额度不足',
  rate_limit_spike: '上游限流激增',
  server_error_spike: '上游 5xx 激增',
  ttft_regression: '首包延迟回退',
  // 以下三条是历史告警留下的 key（规则表里已不存在，库里还留着旧指纹），按语义给中文名。
  quota_auth_failed: '额度鉴权失败',
  quota_stale: '额度读数滞后',
  renewal_failed: '凭证续期失败',
};

const UNKNOWN_RULE = '未知规则';

/** 两种写法都认；都认不出时给「未知规则」，不回落成 key 本身。 */
export function ruleLabel(key: string | undefined): string {
  if (!key) return UNKNOWN_RULE;
  return RULE_LABELS[key] || RULE_LABELS[key.replace(/\./g, '_')] || UNKNOWN_RULE;
}

function ruleKnown(key: string | undefined): boolean {
  return !!key && !!(RULE_LABELS[key] || RULE_LABELS[key.replace(/\./g, '_')]);
}

/** 证据里的键名同理：数值证据给中文标签，认不出的键照常显示（它是排错用的原始字段）。 */
const EVIDENCE_LABELS: Record<string, string> = {
  consecutiveFailures: '连续失败（次）',
  consecutiveDays: '连续天数',
  lastProbeAt: '最近探测',
  lastStatus: '最近状态码',
  threshold: '触发阈值',
  costLimitMicros: '年度成本上限（微元）',
  costUsedMicros: '已用成本（微元）',
  quotaWindow: '额度窗口',
  ratio: '读数占比',
  successRatio: '成功率',
  tokenLimit: 'Token 上限',
  tokenUsed: '已用 Token',
};

const DISPOSITION_TEXT: Record<string, string> = {
  open: '待处置',
  acknowledged: '已确认',
  ignored: '已忽略',
};

/** 目标类型的称呼：账号以外的目标不该叫「账号」。 */
const TARGET_TEXT: Record<string, string> = {
  provider: '账号',
  api_key: '密钥',
  gateway: '网关',
};

/** 时间线存在最新事件的 detail.timeline 里（spec §8.2：处置不另开事件）。 */
function timelineOf(detail: MonitorAlertDetail | null): MonitorTimelineEntry[] {
  const raw = detail?.latestEvent?.detail?.timeline;
  return Array.isArray(raw) ? (raw as MonitorTimelineEntry[]) : [];
}

function targetText(alert: MonitorAlert): string {
  return alert.targetName || alert.targetId || '—';
}

/** 计数按千分位：第 1509 次的告警原文读起来像 hb 数。 */
function countText(value: number): string {
  return value.toLocaleString('zh-CN');
}

/** detail 里的数字：后端 JSON 解码后是 float64，也可能是字符串，统一收成 number 或 undefined。 */
function num(detail: Record<string, unknown> | undefined, key: string): number | undefined {
  const raw = detail?.[key];
  if (typeof raw === 'number' && Number.isFinite(raw)) return raw;
  if (typeof raw === 'string' && raw.trim() !== '' && Number.isFinite(Number(raw))) return Number(raw);
  return undefined;
}

function str(detail: Record<string, unknown> | undefined, key: string): string {
  const raw = detail?.[key];
  return typeof raw === 'string' ? raw : '';
}

/** 比率 → 百分比文案。0.0004 这种不能显示成「0.0%」，那会丢掉「还差多少」的信息。 */
function percent(ratio: number, digits = 1): string {
  const pct = ratio * 100;
  if (pct > 0 && pct < 0.1) return '<0.1%';
  return `${pct.toFixed(digits)}%`;
}

/** 时长：分钟级给「N 分钟」，小时级给「N 小时」；不足一分钟按「不到 1 分钟」。 */
function durationText(minutes: number): string {
  if (!Number.isFinite(minutes) || minutes < 0) return '—';
  if (minutes < 1) return '不到 1 分钟';
  if (minutes < 60) return `${Math.round(minutes)} 分钟`;
  const hours = minutes / 60;
  return hours < 48 ? `${hours.toFixed(hours < 10 ? 1 : 0)} 小时` : `${(hours / 24).toFixed(1)} 天`;
}

/** 距离某时刻还有多久（用于「N 小时后失效」）。 */
function untilText(raw: string, now: number): string {
  if (!hasTime(raw)) return '';
  const delta = (Date.parse(raw) - now) / 1000;
  if (Number.isNaN(delta)) return '';
  if (delta <= 0) return '已经到期';
  const hours = delta / 3600;
  if (hours < 1) return `${Math.round(delta / 60)} 分钟后`;
  if (hours < 48) return `${Math.round(hours)} 小时后`;
  return `${(hours / 24).toFixed(1)} 天后`;
}

/**
 * 告警描述：一句能直接读懂的现状句，由「规则 + 目标 + 当前读数 + 处置后果」拼成。
 *
 * 数据只取 detail 里真实存在的字段；缺数据时退回规则名 + 目标 + 状态，绝不编数值。
 * 输出里不出现原始 key、英文枚举或空话（「请及时处理」这类），每一段都要带信息。
 */
export function describeAlert(alert: MonitorAlert, now: number): string {
  const rule = alert.ruleKey.replace(/\./g, '_');
  const target = targetText(alert);
  const kind = TARGET_TEXT[alert.targetType] ?? '目标';
  const detail = alert.detail;

  switch (rule) {
    case 'quota_low': {
      const ratio = num(detail, 'ratio');
      if (ratio === undefined || ratio < 0) {
        return `${kind} ${target} 的额度读数不可用，无法判断剩余配额，建议登录上游核对额度状态。`;
      }
      const how = ratio <= 0 ? '已降至 0%' : `仅剩 ${percent(ratio)}`;
      return `${kind} ${target} 的可用额度${how}，低于告警阈值；余额见底的请求会直接失败，请充值或调整调度权重。`;
    }
    case 'credit_expiring': {
      const expiresAt = str(detail, 'expiresAt');
      const left = untilText(expiresAt, now);
      const remain = num(detail, 'remaining');
      const amount = remain !== undefined && remain > 0 ? `剩余 ${Math.round(remain)} 积分/额度` : '剩余额度';
      if (!left) {
        return `${kind} ${target} 的额度即将到期，${amount}届时将失效，请核对到期时间并尽快使用或续费。`;
      }
      return `${kind} ${target} ${amount}将在 ${left}失效，届时未用完的部分不再可用，请尽快使用或充值。`;
    }
    case 'oauth_expiring': {
      const expiresAt = str(detail, 'expiresAt');
      const left = untilText(expiresAt, now);
      const renewalOk = detail?.renewalOk === true;
      if (!left) {
        return `${kind} ${target} 的登录凭证即将过期，且最近一次自动续期没有成功，请手动重新授权。`;
      }
      return renewalOk
        ? `${kind} ${target} 的登录凭证将在 ${left}过期，最近一次自动续期成功，若到时未刷新需手动重新授权。`
        : `${kind} ${target} 的登录凭证将在 ${left}过期，自动续期未成功，过期后该账号的请求会全部失败。`;
    }
    case 'oauth_refresh_failed': {
      const fails = num(detail, 'consecutiveFailures');
      const threshold = num(detail, 'threshold');
      const times = fails !== undefined ? `已连续 ${fails} 次` : '近期多次';
      const bar = threshold !== undefined && fails !== undefined && fails >= threshold ? `，达到阈值 ${threshold} 次` : '';
      return `${kind} ${target} 的凭证自动续期${times}失败${bar}，凭证到期后将无法自动恢复，请重新授权或检查上游登录状态。`;
    }
    case 'probe_failed': {
      const fails = num(detail, 'consecutiveFailures');
      const threshold = num(detail, 'threshold');
      const status = num(detail, 'lastStatus');
      const skipped = str(detail, 'skipped');
      if (skipped) {
        return `${kind} ${target} 的最近一次轻探测被跳过（原因：${skipped}），上游是否可用还没有结论。`;
      }
      const chain = fails !== undefined ? `连续 ${fails} 次轻探测失败` : '轻探测持续失败';
      const bar = threshold !== undefined ? `（阈值 ${threshold} 次）` : '';
      const code = status ? `，最近一次返回状态码 ${status}` : '';
      const escalated = detail?.escalated === true ? '；返回 401/403，凭证可能已失效，已提升告警级别' : '';
      return `${kind} ${target} ${chain}${bar}${code}${escalated}，请求可能已经被调度绕开。`;
    }
    case 'model_unavailable': {
      const fails = num(detail, 'consecutiveFailures');
      const threshold = num(detail, 'threshold');
      const chain = fails !== undefined ? `已连续 ${fails} 次` : '近期多次';
      const bar = threshold !== undefined ? `（阈值 ${threshold} 次）` : '';
      return `${kind} ${target} 拉取模型列表${chain}失败${bar}，该账号无法同步上游模型，新建请求会缺少可用模型。`;
    }
    case 'auth_invalid': {
      const auth = num(detail, 'authCount');
      const total = num(detail, 'total');
      const ratio = num(detail, 'authRatio');
      const window = num(detail, 'windowMinutes');
      const parts: string[] = [];
      if (auth !== undefined && total !== undefined) parts.push(`最近 ${window !== undefined ? durationText(window) : '一段时间'}内 ${countText(auth)} / ${countText(total)} 次请求被上游拒绝`);
      else parts.push('近期多次请求被上游以 401/403 拒绝');
      const pct = ratio !== undefined ? `（${percent(ratio)}）` : '';
      return `${kind} ${target} ${parts[0]}${pct}，凭证可能已失效，请重新授权后再启用。`;
    }
    case 'provider_down': {
      const success = num(detail, 'successRatio');
      const total = num(detail, 'total');
      const window = num(detail, 'windowMinutes');
      const ratio = success !== undefined ? `成功率 ${percent(success)}` : '成功率过低';
      const sample = total !== undefined ? `（窗口 ${window !== undefined ? durationText(window) : '—'} 内 ${countText(total)} 次请求）` : '';
      return `${kind} ${target} 最近 ${ratio}${sample}，低于告警线，请求正在失败或被调度绕开。`;
    }
    case 'server_error_spike': {
      const server = num(detail, 'serverError');
      const total = num(detail, 'total');
      const ratio = num(detail, 'errorRatio');
      const window = num(detail, 'windowMinutes');
      const hit = server !== undefined && total !== undefined ? `${countText(server)} / ${countText(total)} 次请求返回 5xx` : '上游 5xx 明显增多';
      const pct = ratio !== undefined ? `（${percent(ratio)}）` : '';
      const win = window !== undefined ? `，统计窗口 ${durationText(window)}` : '';
      return `${kind} ${target} ${hit}${pct}${win}，上游服务端正在报错，建议降低该账号权重或切换主备。`;
    }
    case 'rate_limit_spike': {
      const limited = num(detail, 'rateLimited');
      const total = num(detail, 'total');
      const ratio = num(detail, 'rateLimitRatio');
      const window = num(detail, 'windowMinutes');
      const hit = limited !== undefined ? `${countText(limited)} 次请求被上游限流${total !== undefined ? `（共 ${countText(total)} 次）` : ''}` : '上游限流明显增多';
      const pct = ratio !== undefined ? `，占比 ${percent(ratio)}` : '';
      const win = window !== undefined ? `，统计窗口 ${durationText(window)}` : '';
      return `${kind} ${target} ${hit}${pct}${win}，上游配额或并发受限，请求会被降速或失败。`;
    }
    case 'empty_response': {
      const empty = num(detail, 'empty');
      const total = num(detail, 'total');
      const ratio = num(detail, 'emptyRatio');
      const window = num(detail, 'windowMinutes');
      const hit = empty !== undefined ? `${countText(empty)} 次空响应${total !== undefined ? `（共 ${countText(total)} 次请求）` : ''}` : '空响应明显增多';
      const pct = ratio !== undefined ? `，占比 ${percent(ratio)}` : '';
      const win = window !== undefined ? `，统计窗口 ${durationText(window)}` : '';
      return `${kind} ${target} ${hit}${pct}${win}，上游返回了没有内容的回复，客户端会收到空消息。`;
    }
    case 'ttft_regression': {
      const p95 = num(detail, 'ttftP95');
      const baseline = num(detail, 'baselineP95');
      const factor = num(detail, 'factor');
      const window = num(detail, 'windowMinutes');
      if (p95 !== undefined && baseline !== undefined && baseline > 0) {
        const times = factor !== undefined ? `，为基线的 ${(p95 / baseline).toFixed(1)} 倍（阈值 ${factor} 倍）` : '';
        return `${kind} ${target} 首包延迟 P95 升至 ${Math.round(p95)}ms，基线 ${Math.round(baseline)}ms${times}${window !== undefined ? `，统计窗口 ${durationText(window)}` : ''}，上游明显变慢。`;
      }
      return `${kind} ${target} 的首包延迟明显高于基线，上游响应变慢。`;
    }
    case 'checkin_failed': {
      const days = num(detail, 'consecutiveDays');
      const threshold = num(detail, 'threshold');
      const chain = days !== undefined ? `已连续 ${days} 天` : '近期多次';
      const bar = threshold !== undefined ? `（阈值 ${threshold} 天）` : '';
      return `${kind} ${target} 自动签到${chain}失败${bar}，签到奖励正在漏领，请检查上游登录态。`;
    }
    case 'key_budget_exhausted':
    case 'key_budget_warn': {
      const ratio = num(detail, 'ratio');
      const used = num(detail, 'tokenUsed');
      const limit = num(detail, 'tokenLimit');
      const window = str(detail, 'quotaWindow');
      const usedText = used !== undefined && limit !== undefined && limit > 0
        ? `，已用 ${countText(used)} / ${countText(limit)} Token`
        : '';
      // 有绝对值就以绝对值为准：ratio 是触发那一刻的读数，之后用量还会涨，两者对不上时
      // 「已用 3121%」这种话比没有更糟。
      const pct = usedText && used !== undefined && limit !== undefined && limit > 0
        ? `已用 ${percent(used / limit)}`
        : ratio !== undefined
          ? `已用 ${percent(ratio)}`
          : '额度接近上限';
      const win = window ? `（额度窗口 ${window}）` : '';
      const consequence = rule === 'key_budget_exhausted' ? '额度耗尽后该密钥的请求会被直接拒绝' : '继续使用将很快耗尽额度';
      return `${kind} ${target} ${pct}${usedText}${win}，${consequence}。`;
    }
    case 'multi_ip':
    case 'concurrent_ip': {
      const ips = Array.isArray(detail?.ips) ? (detail?.ips as unknown[]).length : undefined;
      const count = num(detail, 'ipCount') ?? ips;
      const window = num(detail, 'windowMinutes');
      const hit = count !== undefined ? `出现 ${count} 个不同 IP` : '出现多个 IP';
      const win = window !== undefined ? `（统计窗口 ${durationText(window)}）` : '';
      const consequence = rule === 'multi_ip' ? '，密钥可能已泄露或被多人共用' : '，疑似同一时刻被多处并发使用';
      return `${kind} ${target} ${hit}${win}${consequence}，建议核对来源或轮换密钥。`;
    }
    case 'collector_stale': {
      const stale = num(detail, 'staleMinutes');
      const maxAge = num(detail, 'maxAgeMinutes');
      const since = stale !== undefined ? `已 ${durationText(stale)}没有成功采集` : '长时间没有成功采集';
      const bar = maxAge !== undefined ? `（阈值 ${durationText(maxAge)}）` : '';
      return `${kind} ${target} 的监控采集器${since}${bar}，账号与信号数据正在变旧，告警可能滞后。`;
    }
    default: {
      // 未知 / 历史 key：只给「规则名 + 目标 + 当前状态」，不猜数值。
      const state = alert.status === 'firing' ? '仍在持续' : '已结束';
      return `${kind} ${target} 触发了${ruleKnown(alert.ruleKey) ? ruleLabel(alert.ruleKey) : '一条规则表里没有的检测规则'}，当前${state}。`;
    }
  }
}

/** 推送状态一句话：参考里是「已向企业群组派发通知」，这里按真实推送结果分色。 */
function pushSentence(alert: MonitorAlert): { text: string; tone: string } {
  switch (alert.pushStatus) {
    case 'sent':
      return { text: '已向通知渠道派发', tone: 'ok' };
    case 'partial':
      return { text: '部分渠道推送成功', tone: 'warn' };
    case 'pending':
      return { text: '推送排队中', tone: 'warn' };
    case 'failed':
      return { text: alert.pushError ? `推送失败：${alert.pushError}` : '推送失败', tone: 'err' };
    case 'suppressed':
      return { text: '静默时段内已抑制', tone: 'off' };
    case 'skipped':
      return { text: '未推送（无匹配渠道）', tone: 'off' };
    default:
      return { text: '尚未推送', tone: 'off' };
  }
}

/**
 * 诊断包：这条告警的完整 JSON，字段名与后端 MonitorAlert 一致，直接可以贴进 issue。
 *
 * 时间统一给 ISO 原值（带 UTC），浏览器时区换算交给读者；本地化后的「08-01 12:00」
 * 贴进排障群反而对不上日志。detail 原样带上（不含 token：后端只回分类与数值）。
 */
export function diagnosticPackage(alert: MonitorAlert): string {
  return JSON.stringify(
    {
      fingerprint: alert.fingerprint,
      ruleKey: alert.ruleKey,
      ruleName: ruleLabel(alert.ruleKey),
      severity: alert.severity,
      status: alert.status,
      disposition: alert.disposition,
      count: alert.count,
      target: { type: alert.targetType, id: alert.targetId, name: alert.targetName },
      firstSeen: alert.firstSeen,
      lastSeen: alert.lastSeen,
      lastNotifiedAt: hasTime(alert.lastNotifiedAt) ? alert.lastNotifiedAt : undefined,
      acknowledgedAt: alert.disposition === 'acknowledged' && hasTime(alert.acknowledgedAt) ? alert.acknowledgedAt : undefined,
      resolutionReason: alert.resolutionReason || undefined,
      pushStatus: alert.pushStatus || undefined,
      pushError: alert.pushError || undefined,
      latestEventId: alert.latestEventId,
      detail: alert.detail ?? {},
    },
    null,
    2,
  );
}

/** 全局 toast（与 CopyButton 同一个 #toast 元素），3 秒后收起。 */
function showToast(text: string) {
  const toast = document.getElementById('toast');
  if (!toast) return;
  toast.textContent = text;
  toast.classList.add('show');
  window.setTimeout(() => toast.classList.remove('show'), 3000);
}

/** 处置状态徽标：待处置用中性灰，已确认绿、已忽略灰；颜色之外永远有文字。 */
function dispositionTone(disposition: string): string {
  return disposition === 'acknowledged' ? 'ok' : 'off';
}

/** 筛选条括号里的合计：只信服务端合计，为 0 时退回本页计数（服务端没给时才可能为 0）。 */
function countInParens(value: number, loaded: number): string {
  return countText(value > 0 ? value : loaded);
}

export function AlertsTab(props: AlertsTabProps) {
  const {
    alerts, total, statusFilter, severityFilter, firingTotal, resolvedTotal, timezone, channels, detail, busy, error,
    onStatusFilter, onSeverityFilter, onOpen, onClose, onAck, onIgnore, onRetryPush, onOpenAccount, onLoadMore,
  } = props;
  const [retryChannel, setRetryChannel] = useState('');
  const now = Date.now();

  const timeline = timelineOf(detail);
  const evidence = detailPairs(detail?.latestEvent?.detail ?? detail?.alert?.detail, 10);
  const pushable = !!detail?.latestEvent && detail.latestEvent.pushStatus !== 'sent';

  // 进行中 / 已恢复 的合计：服务端给的全量口径（不受状态筛选影响），没给时退回本页计数。
  const loadedFiring = alerts.filter((alert) => alert.status === 'firing').length;
  const firingCount = countInParens(firingTotal, loadedFiring);
  const resolvedCount = countInParens(resolvedTotal, alerts.length - loadedFiring);
  const allCount = countText(firingTotal + resolvedTotal);
  const statusCounts: Record<string, string> = { firing: firingCount, resolved: resolvedCount, '': allCount };

  const filtered = statusFilter !== '' || severityFilter !== '';

  return (
    <div className="mon-alerts">
      {/* 一行紧凑筛选：状态分段（带合计）→ 级别分段 →「全部标记恢复」。 */}
      <div className="mon-alerts-bar">
        <div className="mon-seg" role="group" aria-label="按状态筛选">
          {STATUS_CHIPS.map((chip) => (
            <button
              key={chip.id || 'all'}
              type="button"
              className={`mon-seg-btn${statusFilter === chip.id ? ' on' : ''}`}
              aria-pressed={statusFilter === chip.id}
              title={chip.title}
              onClick={() => onStatusFilter(chip.id)}
            >
              {`${chip.label} (${statusCounts[chip.id] ?? '0'})`}
            </button>
          ))}
        </div>
        <div className="mon-seg mon-seg-level" role="group" aria-label="按级别筛选">
          {SEVERITY_CHIPS.map((value) => (
            <button
              key={value || 'any'}
              type="button"
              className={`mon-seg-btn${severityFilter === value ? ' on' : ''}`}
              aria-pressed={severityFilter === value}
              title={value ? `只看 ${SEVERITY_TEXT[value]}` : '不按级别筛选'}
              onClick={() => onSeverityFilter(value)}
            >
              {value ? value.toUpperCase() : '全部'}
            </button>
          ))}
        </div>
        {/*
          参考里的「全部标记恢复」是参考自己 mock 出来的批量动作，后端没有对应接口；
          前端不该伪造状态（条件仍成立会立刻重新触发），所以右侧如实给出处置入口说明，
          逐条的「确认 / 忽略」在详情弹窗里。
        */}
        <span
          className="mon-alerts-bulk"
          title="批量标记恢复没有后端接口；条件仍成立的告警即使标记恢复也会重新触发，所以请逐条在详情里确认"
        >
          {'逐条处置'}
        </span>
      </div>

      {error && <div className="mon-alerts-error" role="alert">{error}</div>}

      {alerts.length === 0 && !error && (
        <div className="mon-alerts-empty">
          <span className="mon-alerts-empty-icon" aria-hidden>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.7} strokeLinecap="round" strokeLinejoin="round" width={22} height={22}>
              <path d="M12 3.5 19 6v5.5c0 4.3-2.9 7.6-7 9-4.1-1.4-7-4.7-7-9V6l7-2.5ZM8.8 12l2.2 2.2 4.2-4.4" />
            </svg>
          </span>
          <span className="mon-alerts-empty-title">{filtered ? '这个筛选下没有告警' : '暂无告警'}</span>
          <span className="mon-alerts-empty-note">
            {filtered ? '换个状态或级别看看；「全部」与「全部级别」会列出最近 7 天的每一条。' : '最近 7 天没有命中任何检测规则。'}
          </span>
        </div>
      )}

      {alerts.length > 0 && (
        <div className="mon-alerts-list">
          {alerts.map((alert) => {
            const stamp = displayTime(alert.lastSeen, timezone);
            const known = ruleKnown(alert.ruleKey);
            const firing = alert.status === 'firing';
            const push = pushSentence(alert);
            const ackBusy = busy === `ack:${alert.id}`;
            return (
              <article className={`mon-alert-row tone-${alert.severity}${firing ? '' : ' is-resolved'}`} key={alert.id}>
                <div className="mon-alert-main">
                  {/* 第一行：级别胶囊 + 规则中文名（原始 key 括号标注）+ 目标账号徽标 */}
                  <div className="mon-alert-head">
                    <span
                      className={`mon-alert-sev tone-${alert.severity}`}
                      title={SEVERITY_TEXT[alert.severity]}
                    >
                      {alert.severity === 'info' ? '信息' : alert.severity.toUpperCase()}
                    </span>
                    <h3 className="mon-alert-title">
                      <span>{ruleLabel(alert.ruleKey)}</span>
                      <span className="mon-alert-rulekey" title={known ? '规则 key' : '规则表里没有这一条'}>
                        {`(${alert.ruleKey || '未记录规则 key'})`}
                      </span>
                    </h3>
                    <button
                      type="button"
                      className="mon-alert-target"
                      onClick={() => onOpenAccount(alert.targetId)}
                      title={`${TARGET_TEXT[alert.targetType] ?? '目标'}：${targetText(alert)} · 点击查看详情`}
                    >
                      <span className="mon-alert-target-kind">{TARGET_TEXT[alert.targetType] ?? '目标'}</span>
                      <span className="mon-alert-target-name">{targetText(alert)}</span>
                    </button>
                  </div>

                  {/* 第二行：一句告警描述（规则 + 目标 + 关键数值 + 后果） */}
                  <p className="mon-alert-desc">{describeAlert(alert, now)}</p>

                  {/* 第三行：触发次数 · 更新时间 · 推送状态 ·（已处置时）处置状态 ·（已恢复时）恢复原因 */}
                  <div className="mon-alert-meta">
                    {!firing && (
                      <span className="mon-alert-state resolved">
                        {'已恢复'}
                        <span className="mon-alert-meta-sub">
                          {` · ${RESOLUTION_TEXT[alert.resolutionReason ?? ''] ?? '原因未记录'}`}
                        </span>
                      </span>
                    )}
                    <span className="mon-alert-meta-item">
                      <span className="mon-alert-meta-key">{'触发'}</span>
                      <span className="mon-alert-meta-num">{`${countText(alert.count)} 次`}</span>
                    </span>
                    <span className="mon-alert-meta-item" title={stamp.title}>
                      <span className="mon-alert-meta-key">{'更新'}</span>
                      <span className="mon-alert-meta-num">
                        <span className="m-stamp-full">{relativeToNow(alert.lastSeen, now)}</span>
                        <span className="m-stamp-short">{displayTimeCompact(stamp.text)}</span>
                      </span>
                    </span>
                    <span className={`mon-alert-push push-${alert.pushStatus ?? 'none'} mon-alert-sentence tone-${push.tone}`}>
                      {push.text}
                    </span>
                    {alert.disposition !== 'open' && (
                      <>
                        <span className="mon-alert-meta-dot" aria-hidden>{'·'}</span>
                        <span className={`mon-acc-state ${dispositionTone(alert.disposition)}`} title="确认/忽略只影响推送，不会停止检测">
                          <span className="mon-acc-state-dot" aria-hidden />
                          {DISPOSITION_TEXT[alert.disposition] ?? '已处置'}
                        </span>
                      </>
                    )}
                  </div>
                </div>

                {/* 右侧按钮组：处理并标记恢复（= ack，只在进行中可用）+ 复制诊断包 */}
                <div className="mon-alert-actions">
                  <button
                    type="button"
                    className="btn btn-sm mon-alert-ack"
                    disabled={!firing || ackBusy}
                    title={
                      alert.disposition === 'acknowledged'
                        ? '已确认（只影响推送，不停止检测）'
                        : firing
                          ? '标记为已确认（只影响推送，不停止检测）'
                          : '告警已结束，无需处置'
                    }
                    onClick={() => onAck(alert.id)}
                  >
                    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2.4} strokeLinecap="round" strokeLinejoin="round" width={13} height={13} aria-hidden>
                      <path d="m5 12.5 4.5 4.5L19 7" />
                    </svg>
                    {ackBusy ? '处理中…' : alert.disposition === 'acknowledged' && firing ? '已确认' : '处理并标记恢复'}
                  </button>
                  <button
                    type="button"
                    className="btn btn-sm"
                    title="复制该告警的完整 JSON（指纹、规则、级别、次数、时间、detail）"
                    onClick={() => {
                      void copyToClipboard(diagnosticPackage(alert)).then((ok) => {
                        showToast(ok ? `已复制「${ruleLabel(alert.ruleKey)}」的诊断包` : '复制失败，浏览器拒绝了剪贴板权限');
                      });
                    }}
                  >
                    {'复制诊断包'}
                  </button>
                  <button type="button" className="btn btn-sm" onClick={() => onOpen(alert.id)}>
                    {'详情'}
                  </button>
                </div>
              </article>
            );
          })}
        </div>
      )}

      {alerts.length < total && (
        <div className="mon-alerts-more">
          <button type="button" className="btn btn-sm" onClick={onLoadMore}>
            {`加载更多（已显示 ${countText(alerts.length)} / ${countText(total)}）`}
          </button>
        </div>
      )}

      {detail && (
        <Modal
          title={`${ruleLabel(detail.alert.ruleKey)} · ${targetText(detail.alert)}`}
          description={`${SEVERITY_TEXT[detail.alert.severity]} · ${
            detail.alert.status === 'firing'
              ? '进行中'
              : `已恢复（${RESOLUTION_TEXT[detail.alert.resolutionReason ?? ''] ?? '原因未记录'}）`
          }`}
          onClose={onClose}
          size="wide"
        >
          <p className="mon-alert-desc mon-detail-desc">{describeAlert(detail.alert, now)}</p>
          <div className="monitoring-cols">
            <div className="card mon-detail-card">
              <div className="mon-card-title">{'证据'}</div>
              {!ruleKnown(detail.alert.ruleKey) && (
                <div className="mon-detail-note">{`规则 key：${detail.alert.ruleKey}（规则表里没有这一条，可能是历史版本留下的告警）`}</div>
              )}
              {evidence.length === 0 && <div className="mon-detail-note">{'这条告警没有留下可展示的数值证据。'}</div>}
              {evidence.map(([key, value]) => (
                <div className="mon-detail-kv" key={key}>
                  <span className="mon-detail-k" title={key}>{EVIDENCE_LABELS[key] ?? key}</span>
                  <span className="mon-detail-v" title={value}>{value}</span>
                </div>
              ))}
              <div className="mon-detail-note">
                {`首次 ${displayTime(detail.alert.firstSeen, timezone).text} · 最近 ${displayTime(detail.alert.lastSeen, timezone).text}`}
              </div>
              {detail.alert.pushError && <div className="mon-detail-note err">{`推送报错：${detail.alert.pushError}`}</div>}
            </div>

            <div className="card mon-detail-card">
              <div className="mon-card-title">{'时间线'}</div>
              <div className="mon-detail-note">
                {`触发 → 通知（${PUSH_LABELS[detail.latestEvent?.pushStatus ?? ''] ?? '未通知'}）→ ${
                  detail.alert.status === 'resolved' ? '恢复' : '仍在持续'
                }`}
              </div>
              {timeline.length === 0 && <div className="mon-detail-note">{'还没有人工处置记录。'}</div>}
              {timeline.map((entry, index) => (
                <div className="mon-detail-tl" key={`${entry.action}-${index}`}>
                  <span className="badge slate">
                    {entry.action === 'acknowledged' ? '已确认' : entry.action === 'ignored' ? '已忽略' : '已处置'}
                  </span>
                  <span className="mon-detail-k">
                    {`${displayTime(entry.time, timezone).text}${entry.actor ? ` · ${entry.actor}` : ''}`}
                  </span>
                </div>
              ))}
              {detail.latestEvent?.detail?.errorKind !== undefined && (
                <div className="mon-detail-note">
                  {ERROR_KIND_TEXT[String(detail.latestEvent.detail.errorKind) as keyof typeof ERROR_KIND_TEXT] ?? '未知错误'}
                </div>
              )}            </div>
          </div>

          <div className="mon-detail-actions">
            <button
              type="button"
              className="btn btn-sm"
              disabled={detail.alert.status !== 'firing' || busy === `ack:${detail.alert.id}`}
              onClick={() => onAck(detail.alert.id)}
            >
              {'确认'}
            </button>
            <button
              type="button"
              className="btn btn-sm"
              disabled={detail.alert.status !== 'firing' || busy === `ignore:${detail.alert.id}`}
              onClick={() => onIgnore(detail.alert.id)}
            >
              {'忽略'}
            </button>
            {detail.alert.status !== 'firing' && <span className="mon-detail-note">{'告警已结束，无需处置。'}</span>}
            {pushable && channels.length > 0 && (
              <>
                <select
                  className="monitoring-input mon-detail-select"
                  value={retryChannel}
                  onChange={(event) => setRetryChannel(event.target.value)}
                  aria-label="选择重试渠道"
                >
                  <option value="">{'选择渠道'}</option>
                  {channels.map((channel) => (
                    <option key={channel.id} value={channel.id}>{`${channel.name}（${channel.type}）`}</option>
                  ))}
                </select>
                <button
                  type="button"
                  className="btn btn-sm"
                  disabled={!retryChannel || busy === `retry:${detail.latestEvent?.id}`}
                  onClick={() => detail.latestEvent && onRetryPush(detail.latestEvent.id, retryChannel)}
                >
                  {'重试推送'}
                </button>
              </>
            )}
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => {
                void copyToClipboard(diagnosticPackage(detail.alert)).then((ok) => {
                  showToast(ok ? `已复制「${ruleLabel(detail.alert.ruleKey)}」的诊断包` : '复制失败，浏览器拒绝了剪贴板权限');
                });
              }}
            >
              {'复制诊断包'}
            </button>
            <button type="button" className="btn btn-sm" onClick={() => onOpenAccount(detail.alert.targetId)}>
              {detail.alert.targetType === 'provider' ? '查看账号' : `查看${TARGET_TEXT[detail.alert.targetType] ?? '目标'}`}
            </button>
          </div>
        </Modal>
      )}
    </div>
  );
}
