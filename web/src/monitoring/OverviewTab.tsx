import { useMemo } from 'react';
import { BrandIcon } from '../components/ui';
import { formatCostMicros } from '../lib';
import type { MonitorAccount, MonitorAlert, MonitorEvent, MonitorOverview, MonitorSeverity } from './types';
import { hasTime, SEVERITY_TEXT } from './types';
import { detailPairs, displayTime, formatLatency, formatRatio, quotaText, relativeToNow, SEVERITY_TONE } from './format';

export interface OverviewTabProps {
  overview: MonitorOverview | null;
  accounts: MonitorAccount[];
  alerts: MonitorAlert[];
  /** 当前筛选下告警总条数（alerts 只是第一页，右卡标题要报全量）。 */
  alertTotal: number;
  events: MonitorEvent[];
  eventTotal: number;
  kindFilter: string;
  severityFilter: MonitorSeverity | '';
  timezone: string;
  /** 品牌图标键由外层提供（见 AccountsTab 同名说明）。 */
  iconOf?: (providerId: string) => string | undefined;
  loading: boolean;
  error: string;
  onKindFilter: (kind: string) => void;
  onSeverityFilter: (severity: MonitorSeverity | '') => void;
  onLoadMoreEvents: () => void;
  onOpenAccount: (providerId: string) => void;
  onOpenAlert: (alertId: number) => void;
  /** 告警卡「全部」：切到告警页看完整列表（概览右卡只给最近的几条）。 */
  onOpenAlerts: () => void;
  onRetry: () => void;
}

const KIND_CHIPS: Array<{ id: string; label: string }> = [
  { id: '', label: '全部' },
  { id: 'alert', label: '告警' },
  { id: 'renewal', label: '续期' },
  { id: 'quota', label: '额度' },
  { id: 'checkin', label: '签到' },
  { id: 'system', label: '系统' },
];

const SEVERITY_CHIPS: Array<MonitorSeverity | ''> = ['', 'p0', 'p1', 'p2'];

/**
 * 账号行的优先排序：问题账号永远在前。判据依次是活跃告警、额度不可用/极低、
 * 凭证已过期、模型列表连续失败、停用。同档保持名字序，避免每次刷新跳位。
 */
function accountScore(account: MonitorAccount, now: number): number {
  if (account.disabled) return 0;
  let score = 0;
  if (account.activeAlerts > 0) score += 100 + account.activeAlerts;
  if (account.healthStatus === 'unavailable') score += 60;
  if (account.quota?.error || (account.quota && account.quota.ratio < 0)) score += 30;
  else if (account.quota && account.quota.ratio >= 0 && account.quota.ratio < 0.1) score += 25;
  if (hasTime(account.expiresAt) && Date.parse(account.expiresAt as string) < now) score += 40;
  if (account.modelFailures > 0) score += 10;
  return score;
}

/** 告警与事件里只带 ruleKey（如 key_budget_warn）；这里按规则表的中文名直出，未知 key 原样显示。
    固定成前端常量是因为概览页不额外拉规则表；与后端 `/__monitor/rules` 的 name 保持一致。 */
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
};

/** 后端旧版 key 用点号（quota.auth_failed），规则表用下划线，两种都认。 */
function ruleLabel(key: string): string {
  return RULE_LABELS[key] || RULE_LABELS[key.replace(/\./g, '_')] || key;
}

const EVENT_SHOWN = 20;

export function OverviewTab(props: OverviewTabProps) {
  const {
    overview, accounts, alerts, alertTotal, events, eventTotal, kindFilter, severityFilter, timezone, iconOf,
    loading, error, onKindFilter, onSeverityFilter, onLoadMoreEvents, onOpenAccount, onOpenAlert, onOpenAlerts, onRetry,
  } = props;
  const now = Date.now();

  const ordered = useMemo(
    () =>
      [...accounts].sort((a, b) => {
        const delta = accountScore(b, now) - accountScore(a, now);
        if (delta !== 0) return delta;
        return a.name.localeCompare(b.name, 'zh-CN');
      }),
    // now 只用于「是否已过期」判断，刷新节奏由轮询驱动，不必进依赖。
    [accounts],
  );

  const usable = overview ? overview.accounts.total - overview.accounts.unavailable - overview.accounts.disabled : 0;

  return (
    <div className="card panel">
      <div className="panel-header">
        <div>
          <h2 className="panel-title">{'监控中心概览'}</h2>
          <p className="panel-desc">
            {`窗口 ${overview ? overview.windowHours : 24} 小时 · 采集器心跳 ${
              overview ? displayTime(overview.collectorLastSuccess, timezone).text : '等待采集'
            }`}
          </p>
        </div>
        <div className="panel-header-actions">
          <button className="btn btn-sm" onClick={onRetry} disabled={loading}>
            {loading ? '刷新中…' : '立即刷新'}
          </button>
        </div>
      </div>

      {error && (
        <div className="metric">
          <div className="metric-label">{'读取失败'}</div>
          <div className="metric-value err">{error}</div>
        </div>
      )}

      {/* 量卡：与「用量统计」指标卡同一套语言（标签 + 大数字 + 副注），四卡一行。 */}
      <div className="grid-4 monitor-metric-row">
        <div className="card metric modern-metric-card">
          <div className="metric-header-row">
            <span className="metric-label">{'账号'}</span>
            {overview && overview.accounts.subscribed > 0 && (
              <span className="metric-badge badge-blue">{`订阅 ${overview.accounts.subscribed}`}</span>
            )}
          </div>
          <div className="metric-main-row">
            <div className="metric-value">{overview ? `${usable}/${overview.accounts.total}` : '—'}</div>
          </div>
          <div className="metric-note">
            {overview
              ? `可用 ${usable} · 异常 ${overview.accounts.unavailable} · 停用 ${overview.accounts.disabled}`
              : '等待采集'}
          </div>
        </div>
        <div className="card metric modern-metric-card">
          <div className="metric-header-row">
            <span className="metric-label">{'活跃告警'}</span>
            {overview && overview.firing.p0 > 0 && <span className="metric-badge badge-red">{'需要处理'}</span>}
          </div>
          <div className="metric-main-row">
            <div className={`metric-value ${overview && overview.firing.p0 > 0 ? 'err' : ''}`}>
              {overview ? String(overview.firing.total) : '—'}
            </div>
          </div>
          <div className="metric-note">
            {overview
              ? `P0 ${overview.firing.p0} · P1 ${overview.firing.p1} · P2 提醒 ${overview.firing.p2}（不推送）`
              : '等待采集'}
          </div>
        </div>
        <div className="card metric modern-metric-card">
          <div className="metric-header-row">
            <span className="metric-label">{'24h 成功率'}</span>
          </div>
          <div className="metric-main-row">
            <div className="metric-value">{overview ? formatRatio(overview.requests.successRate) : '—'}</div>
          </div>
          <div className="metric-note">
            {overview
              ? `${overview.requests.total.toLocaleString()} 条 · P95 首包 ${formatLatency(overview.requests.p95LatencyMs)}`
              : '等待采集'}
          </div>
        </div>
        <div className="card metric modern-metric-card">
          <div className="metric-header-row">
            <span className="metric-label">{'24h 成本'}</span>
            {overview && overview.requests.priced === 0 && <span className="metric-badge badge-amber">{'未计价'}</span>}
          </div>
          <div className="metric-main-row">
            {/* 没有价格行时绝不能显示 $0.00：那看起来像"免费模型"，实际是"没计价"。 */}
            <div className="metric-value">
              {overview && overview.requests.priced > 0 ? formatCostMicros(overview.requests.costMicros) : '—'}
            </div>
          </div>
          <div className="metric-note">
            {overview
              ? overview.requests.priced > 0
                ? `计价覆盖 ${overview.requests.priced}/${overview.requests.total}`
                : '这些模型还没有价格行'
              : '等待采集'}
          </div>
        </div>
      </div>

      <div className="monitoring-cols">
        <div className="card panel m-col-card">
          <div className="m-panel-head">
            <div className="panel-title">{`账号健康（${ordered.length}）`}</div>
            <span className="m-panel-note">{'问题账号排在前面'}</span>
          </div>
          {ordered.length === 0 && <div className="m-empty">{'还没有账号。接入 Provider 后这里会出现状态矩阵。'}</div>}
          {/* 账号多时列表内部滚动：右卡同高，整页不被 26 行拉长。 */}
          <div className="m-scroll-list">
            {ordered.map((account) => {
              const quota = quotaText(account.quota);
              const quotaTone = account.quota?.error
                ? 'has-err'
                : account.quota && account.quota.ratio >= 0 && account.quota.ratio < 0.1
                  ? 'has-warn'
                  : '';
              const troubled = accountScore(account, now) > 0;
              const renewal = hasTime(account.expiresAt) ? `续期 ${relativeToNow(account.expiresAt, now)}` : '不过期';
              return (
                <div className="log-row" key={account.providerId}>
                  <div className="mrow mrow-account-mini">
                    <span className="m-acc-name">
                      <span className={`dot ${troubled ? 'err' : 'ok'}`} aria-hidden />
                      <BrandIcon icon={iconOf?.(account.providerId)} name={account.name} size={18} />
                      <span className="m-acc-label" title={`${account.name}（${account.vendor || '未知厂商'}）`}>
                        {/* 空白后的零宽断点：行宽不够时在词与词之间折行，而不是把整句挤成省略号。 */}
                        {account.name.split(/(\s+)/).map((part, index) =>
                          /^\s+$/.test(part) ? (
                            <span key={index}>
                              {part}
                              <wbr />
                            </span>
                          ) : (
                            part
                          ),
                        )}
                      </span>
                    </span>
                    {/* 续期与额度各占一行：半宽卡里横排会把「续期 24 天后」折成「剩余…」，读不出来。 */}
                    <span className="m-acc-facts">
                      <span className="m-acc-fact" title={`凭证到期：${account.expiresAt ?? '无'}`}>
                        <b>{'凭证'}</b>
                        <span>{renewal}</span>
                      </span>
                      <span className="m-acc-fact quota" title={quota.note}>
                        <b>{'额度'}</b>
                        <span className={quotaTone}>{quota.primary}</span>
                      </span>
                    </span>
                    <span className="m-acc-badges">
                      {/* 概览这半宽卡里放不下厂商列；模型列表连续失败才是这里要看的信号。 */}
                      {account.modelFailures > 0 && <span className="badge slate">{`列表 ${account.modelFailures}×`}</span>}
                      {account.activeAlerts > 0 ? (
                        <span className="badge red">{`告警 ${account.activeAlerts}`}</span>
                      ) : account.healthStatus === 'unavailable' ? (
                        // 请求判定为异常但还没有成级告警（例如刚过冷却/未达持续窗口）：
                        // 不能显示「正常」，否则概览说异常、列表说正常，自相矛盾。
                        <span className="badge amber">{'异常'}</span>
                      ) : (
                        <span className="badge green">{'正常'}</span>
                      )}
                      {account.disabled && <span className="badge amber">{'已停用'}</span>}
                    </span>
                    <button className="btn btn-sm m-acc-detail" onClick={() => onOpenAccount(account.providerId)}>{'详情'}</button>
                  </div>
                </div>
              );
            })}
          </div>
        </div>

        <div className="card panel m-col-card">
          <div className="m-panel-head">
            <div className="panel-title">{`当前告警（${alerts.length}）`}</div>
            <div className="m-panel-note" style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              {alertTotal > alerts.length && <span>{`共 ${alertTotal} 条`}</span>}
              {alerts.length > 6 && <button className="mini-btn" onClick={onOpenAlerts}>{'全部'}</button>}
            </div>
          </div>
          {alerts.length === 0 && <div className="m-empty">{'没有进行中的告警。'}</div>}
          {/* 与左卡同高、内部滚动：告警少时不留死白，多时也不把整页拉长。 */}
          <div className="m-scroll-list">
            {alerts.slice(0, 20).map((alert) => (
              <div className="log-row" key={alert.id}>
                <div className="mrow mrow-alert-mini">
                  <span className={`badge ${SEVERITY_TONE[alert.severity]}`} title={SEVERITY_TEXT[alert.severity]}>
                    {alert.severity.toUpperCase()}
                  </span>
                  <span className="m-alert-main">
                    <span className="m-alert-title">{alert.targetName || ruleLabel(alert.ruleKey)}</span>
                    <span className="m-alert-meta" title={alert.ruleKey}>{`${ruleLabel(alert.ruleKey)} · 第 ${alert.count} 次 · ${relativeToNow(alert.lastSeen, now)}`}</span>
                  </span>
                  <button className="btn btn-sm" onClick={() => onOpenAlert(alert.id)}>{'处理'}</button>
                </div>
              </div>
            ))}
          </div>
        </div>
      </div>

      <div className="card panel">
        <div className="m-panel-head has-filters">
          <div className="panel-title">{`事件流（${eventTotal}）`}</div>
          {/* 概览只拉最近一段事件，标题写实际条数，不然「事件流（12）」和列表对不上。 */}
          {eventTotal > events.length && <span className="m-panel-note">{`显示最近 ${events.length} 条`}</span>}
          {/* 不带 flexWrap：隐藏时为空字符串的「刷新中…」，原样式里的 wrap 会留下一个空行撑高。 */}
          <div className="panel-header-actions" style={{ display: 'flex', gap: 6 }}>
            <div className="usage-segments">
              {KIND_CHIPS.map((chip) => (
                <button key={chip.id || 'all'} className={kindFilter === chip.id ? 'active' : ''} onClick={() => onKindFilter(chip.id)}>
                  {chip.label}
                </button>
              ))}
            </div>
            <div className="usage-segments">
              {SEVERITY_CHIPS.map((value) => (
                <button key={value || 'any'} className={severityFilter === value ? 'active' : ''} onClick={() => onSeverityFilter(value)}>
                  {value ? value.toUpperCase() : '任意级别'}
                </button>
              ))}
            </div>
          </div>
        </div>
        {events.length === 0 && !loading && <div className="m-empty">{'这个筛选下还没有事件。'}</div>}
        {events.slice(0, EVENT_SHOWN).map((event) => {
          const pairs = detailPairs(event.detail);
          const stamp = displayTime(event.time, timezone);
          return (
            <div className="log-row" key={event.id}>
              <div className="mrow mrow-event">
                <span className="m-alert-meta" title={stamp.title}>{stamp.text}</span>
                <span className={`badge ${SEVERITY_TONE[event.severity]}`}>{event.severity.toUpperCase()}</span>
                <span className="m-event-title" title={event.title}>
                  {event.title}
                  {event.targetId ? (
                    <button className="m-event-link" onClick={() => onOpenAccount(event.targetId as string)}>
                      {event.targetName || event.targetId}
                    </button>
                  ) : null}
                </span>
                <span
                  className="m-event-detail"
                  title={pairs.map(([key, value]) => `${key}=${value}`).join(' · ')}
                >
                  {pairs.length > 0
                    ? pairs.map(([key, value]) => `${key}=${value}`).join(' · ')
                    : event.status === 'resolved' && event.resolutionReason
                      ? `已恢复（${event.resolutionReason}）`
                      : '—'}
                </span>
              </div>
            </div>
          );
        })}
        {/* 拉到 20 条后先收住：概览是摘要，继续往后翻是告警页/日志页的事。 */}
        {events.length > EVENT_SHOWN && (
          <div style={{ paddingTop: 8 }}>
            <button className="btn btn-sm" onClick={onLoadMoreEvents} disabled={loading}>{'再看更多'}</button>
          </div>
        )}
      </div>
    </div>
  );
}
