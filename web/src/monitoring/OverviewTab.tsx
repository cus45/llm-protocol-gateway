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

export function OverviewTab(props: OverviewTabProps) {
  const {
    overview, accounts, alerts, events, eventTotal, kindFilter, severityFilter, timezone, iconOf,
    loading, error, onKindFilter, onSeverityFilter, onLoadMoreEvents, onOpenAccount, onOpenAlert, onRetry,
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

      <div className="grid-4">
        <div className="metric">
          <div className="metric-label">{'可用账号'}</div>
          <div className="metric-main-row">
            <div className="metric-value">{overview ? `${usable}/${overview.accounts.total}` : '—'}</div>
            {overview && overview.accounts.subscribed > 0 && (
              <span className="badge slate">{`订阅 ${overview.accounts.subscribed}`}</span>
            )}
          </div>
          <div className="metric-note">
            {overview ? `异常 ${overview.accounts.unavailable} · 停用 ${overview.accounts.disabled}` : '等待采集'}
          </div>
        </div>
        <div className="metric">
          <div className="metric-label">{'活跃告警'}</div>
          <div className="metric-main-row">
            <div className={`metric-value ${overview && overview.firing.p0 > 0 ? 'err' : ''}`}>
              {overview ? String(overview.firing.total) : '—'}
            </div>
            {overview && (
              <span className={`badge ${overview.firing.p0 > 0 ? 'red' : 'slate'}`}>{`P0 ${overview.firing.p0}`}</span>
            )}
            {overview && <span className="badge amber">{`P1 ${overview.firing.p1}`}</span>}
          </div>
          <div className="metric-note">
            {overview
              ? `P2 提醒 ${overview.firing.p2}（不推送）· 通知渠道 ${overview.channels.enabled}/${overview.channels.total} 启用`
              : '等待采集'}
          </div>
        </div>
        <div className="metric">
          <div className="metric-label">{'24h 成功率'}</div>
          <div className="metric-value">{overview ? formatRatio(overview.requests.successRate) : '—'}</div>
          <div className="metric-note">
            {overview
              ? `${overview.requests.total.toLocaleString()} 条 · P95 首包 ${formatLatency(overview.requests.p95LatencyMs)}`
              : '等待采集'}
          </div>
        </div>
        <div className="metric">
          <div className="metric-label">{'24h 成本'}</div>
          {/* 没有价格行时绝不能显示 $0.00：那看起来像"免费模型"，实际是"没计价"。 */}
          <div className="metric-value">
            {overview && overview.requests.priced > 0 ? formatCostMicros(overview.requests.costMicros) : '未计价'}
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
        <div className="card">
          <div className="panel-title">{`账号健康（${ordered.length}）`}</div>
          {ordered.length === 0 && <div className="muted-text">{'还没有账号。接入 Provider 后这里会出现状态矩阵。'}</div>}
          {ordered.map((account) => {
            const quota = quotaText(account.quota);
            const troubled = accountScore(account, now) > 0;
            return (
              <div className="log-row" key={account.providerId}>
                <div className="mrow mrow-account-mini">
                  <span className="m-clip" style={{ display: 'grid', gridAutoFlow: 'column', gridAutoColumns: 'max-content', alignItems: 'center', gap: 8, minWidth: 0, justifyItems: 'start' }}>
                    <span className={`dot ${troubled ? 'err' : 'ok'}`} aria-hidden />
                    <BrandIcon icon={iconOf?.(account.providerId)} name={account.name} size={18} />
                    <span className="m-clip" style={{ fontWeight: 600 }} title={`${account.name}（${account.vendor || '未知厂商'}）`}>
                      {account.name}
                    </span>
                    {/* 概览这半宽卡里名称只有百来像素，「API Key」这类静态标签留给账号页；
                        停用是会改变处置决定的状态，必须留在这里。 */}
                    {account.disabled && <span className="badge amber">{'已停用'}</span>}
                  </span>
                  <span className="m-row-line">
                    <span className="muted-text" title={`凭证到期：${account.expiresAt ?? '无'}`}>
                      {hasTime(account.expiresAt) ? `续期 ${relativeToNow(account.expiresAt, now)}` : '不过期'}
                    </span>
                    {/* 额度只在宽档单独成列；半宽档与本行并排（见 .mrow-account-mini 断点） */}
                    <span className="m-clip" title={quota.note}>{quota.primary}</span>
                  </span>
                  <span style={{ display: 'flex', gap: 4, justifyContent: 'flex-end' }}>
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
                  </span>
                  <button className="btn btn-sm" onClick={() => onOpenAccount(account.providerId)}>{'详情'}</button>
                </div>
              </div>
            );
          })}
        </div>

        <div className="card">
          <div className="panel-title">{'当前告警前 5 条'}</div>
          {alerts.length === 0 && <div className="muted-text">{'没有进行中的告警。'}</div>}
          {alerts.slice(0, 5).map((alert) => (
            <div className="log-row" key={alert.id}>
              <div className="mrow mrow-alert-mini">
                <span className={`badge ${SEVERITY_TONE[alert.severity]}`} title={SEVERITY_TEXT[alert.severity]}>
                  {alert.severity.toUpperCase()}
                </span>
                <span className="m-clip">{`${alert.targetName} · ${alert.ruleKey}`}</span>
                <span className="muted-text">{`第 ${alert.count} 次 · ${relativeToNow(alert.lastSeen, now)}`}</span>
                <button className="btn btn-sm" onClick={() => onOpenAlert(alert.id)}>{'处理'}</button>
              </div>
            </div>
          ))}
        </div>
      </div>

      <div className="card">
        <div className="panel-header">
          <div className="panel-title">{`事件流（${eventTotal}）`}</div>
          <div className="panel-header-actions" style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
            {KIND_CHIPS.map((chip) => (
              <button
                key={chip.id || 'all'}
                className={`status-pill-btn ${kindFilter === chip.id ? 'active' : ''}`}
                onClick={() => onKindFilter(chip.id)}
              >
                {chip.label}
              </button>
            ))}
            {SEVERITY_CHIPS.map((value) => (
              <button
                key={value || 'any'}
                className={`status-pill-btn ${severityFilter === value ? 'active' : ''}`}
                onClick={() => onSeverityFilter(value)}
              >
                {value ? value.toUpperCase() : '任意级别'}
              </button>
            ))}
          </div>
        </div>
        {events.length === 0 && !loading && <div className="muted-text">{'这个筛选下还没有事件。'}</div>}
        {events.map((event) => {
          const pairs = detailPairs(event.detail);
          const stamp = displayTime(event.time, timezone);
          return (
            <div className="log-row" key={event.id}>
              <div className="mrow mrow-event">
                <span className="muted-text" title={stamp.title}>{stamp.text}</span>
                <span className={`badge ${SEVERITY_TONE[event.severity]}`}>{event.severity.toUpperCase()}</span>
                <span className="m-clip" title={event.targetName ? `${event.title}（${event.targetName}）` : event.title}>
                  {event.title}
                  {event.targetId ? (
                    <button className="status-pill-btn" style={{ marginLeft: 6 }} onClick={() => onOpenAccount(event.targetId as string)}>
                      {event.targetName || event.targetId}
                    </button>
                  ) : null}
                </span>
                <span
                  className="text-aux"
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
        {events.length < eventTotal && (
          <div style={{ paddingTop: 8 }}>
            <button className="btn btn-sm" onClick={onLoadMoreEvents} disabled={loading}>{'加载更多'}</button>
          </div>
        )}
      </div>
    </div>
  );
}
