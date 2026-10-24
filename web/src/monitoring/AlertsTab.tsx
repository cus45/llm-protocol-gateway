import { useState } from 'react';
import { Modal } from '../components/ui';
import type { MonitorAlert, MonitorAlertDetail, MonitorChannelView, MonitorSeverity, MonitorTimelineEntry } from './types';
import { SEVERITY_TEXT } from './types';
import { detailPairs, displayTime, relativeToNow, SEVERITY_TONE } from './format';

export interface AlertsTabProps {
  alerts: MonitorAlert[];
  total: number;
  statusFilter: string;
  severityFilter: MonitorSeverity | '';
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

const STATUS_CHIPS = [
  { id: 'firing', label: '进行中' },
  { id: 'resolved', label: '已恢复' },
  { id: '', label: '全部' },
];

const PUSH_LABELS: Record<string, string> = {
  sent: '已推送',
  failed: '推送失败',
  partial: '部分成功',
  pending: '待发送',
  skipped: '未推送',
  suppressed: '已抑制',
};

/** 时间线存在最新事件的 detail.timeline 里（spec §8.2：处置不另开事件）。 */
function timelineOf(detail: MonitorAlertDetail | null): MonitorTimelineEntry[] {
  const raw = detail?.latestEvent?.detail?.timeline;
  return Array.isArray(raw) ? (raw as MonitorTimelineEntry[]) : [];
}

export function AlertsTab(props: AlertsTabProps) {
  const {
    alerts, total, statusFilter, severityFilter, timezone, channels, detail, busy, error,
    onStatusFilter, onSeverityFilter, onOpen, onClose, onAck, onIgnore, onRetryPush, onOpenAccount, onLoadMore,
  } = props;
  const [retryChannel, setRetryChannel] = useState('');
  const now = Date.now();

  const timeline = timelineOf(detail);
  const evidence = detailPairs(detail?.latestEvent?.detail ?? detail?.alert?.detail, 10);
  const pushable = !!detail?.latestEvent && detail.latestEvent.pushStatus !== 'sent';

  return (
    <div className="card panel">
      <div className="panel-header">
        <div>
          <h2 className="panel-title">{`告警（${total}）`}</h2>
          <p className="panel-desc">{'确认/忽略只影响推送，不会停止检测；账号恢复时仍会给出恢复消息。'}</p>
        </div>
        <div className="panel-header-actions" style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
          {STATUS_CHIPS.map((chip) => (
            <button
              key={chip.id || 'all'}
              className={`status-pill-btn ${statusFilter === chip.id ? 'active' : ''}`}
              onClick={() => onStatusFilter(chip.id)}
            >
              {chip.label}
            </button>
          ))}
          {(['', 'p0', 'p1', 'p2'] as const).map((value) => (
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

      {error && <div className="metric-value err">{error}</div>}
      {alerts.length === 0 && <div className="muted-text">{'这个筛选下没有告警。'}</div>}

      {alerts.map((alert) => (
        <div className="log-row" key={alert.id}>
          <div
            className="log-row-main"
            style={{ gridTemplateColumns: '56px minmax(0,2fr) 1fr 1fr 96px 64px' }}
          >
            <span className={`badge ${SEVERITY_TONE[alert.severity]}`} title={SEVERITY_TEXT[alert.severity]}>
              {alert.severity.toUpperCase()}
            </span>
            <span className="single-line">
              <span style={{ fontWeight: 600 }}>{alert.ruleKey}</span>
              <span className="muted-text">{' · '}</span>
              <button className="status-pill-btn" onClick={() => onOpenAccount(alert.targetId)}>
                {alert.targetName || alert.targetId}
              </button>
            </span>
            <span className={alert.status === 'firing' ? 'err' : 'muted-text'}>
              {alert.status === 'firing'
                ? '进行中'
                : alert.resolutionReason === 'legacy'
                  ? '历史导入'
                  : `已恢复（${alert.resolutionReason || '—'}）`}
            </span>
            <span
              className="muted-text"
              title={`首次 ${displayTime(alert.firstSeen, timezone).text} · 最近 ${displayTime(alert.lastSeen, timezone).text}`}
            >
              {`第 ${alert.count} 次 · ${relativeToNow(alert.lastSeen, now)}`}
            </span>
            <span className="muted-text">
              {PUSH_LABELS[alert.pushStatus ?? ''] ?? '—'}
              {alert.disposition !== 'open' && (
                <span className="badge slate" style={{ marginLeft: 4 }}>
                  {alert.disposition === 'acknowledged' ? '已确认' : '已忽略'}
                </span>
              )}
            </span>
            <button className="btn btn-sm" onClick={() => onOpen(alert.id)}>{'详情'}</button>
          </div>
        </div>
      ))}

      {alerts.length < total && (
        <div style={{ paddingTop: 8 }}>
          <button className="btn btn-sm" onClick={onLoadMore}>{'加载更多'}</button>
        </div>
      )}

      {detail && (
        <Modal
          title={`${detail.alert.ruleKey} · ${detail.alert.targetName || detail.alert.targetId}`}
          description={`${SEVERITY_TEXT[detail.alert.severity]} · ${detail.alert.status === 'firing' ? '进行中' : '已恢复'}`}
          onClose={onClose}
          size="wide"
        >
          <div className="monitoring-cols">
            <div className="card">
              <div className="panel-title">{'证据'}</div>
              {evidence.length === 0 && <div className="muted-text">{'这条告警没有留下可展示的数值证据。'}</div>}
              {evidence.map(([key, value]) => (
                <div className="log-row" key={key}>
                  <span className="metric-label">{key}</span>
                  <span style={{ marginLeft: 'auto', fontWeight: 600 }}>{value}</span>
                </div>
              ))}
              <div className="metric-note" style={{ marginTop: 8 }}>
                {`首次 ${displayTime(detail.alert.firstSeen, timezone).text} · 最近 ${displayTime(detail.alert.lastSeen, timezone).text}`}
              </div>
              {detail.alert.pushError && <div className="metric-note err">{`推送报错：${detail.alert.pushError}`}</div>}
            </div>

            <div className="card">
              <div className="panel-title">{'时间线'}</div>
              <div className="metric-note">
                {`触发 → 通知（${PUSH_LABELS[detail.latestEvent?.pushStatus ?? ''] ?? '—'}）→ ${
                  detail.alert.status === 'resolved' ? '恢复' : '仍在持续'
                }`}
              </div>
              {timeline.length === 0 && <div className="muted-text" style={{ marginTop: 6 }}>{'还没有人工处置记录。'}</div>}
              {timeline.map((entry, index) => (
                <div className="log-row" key={`${entry.action}-${index}`}>
                  <span className="badge slate">
                    {entry.action === 'acknowledged' ? '已确认' : entry.action === 'ignored' ? '已忽略' : entry.action}
                  </span>
                  <span className="muted-text" style={{ marginLeft: 8 }}>
                    {`${displayTime(entry.time, timezone).text}${entry.actor ? ` · ${entry.actor}` : ''}`}
                  </span>
                </div>
              ))}
            </div>
          </div>

          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', marginTop: 12 }}>
            <button
              className="btn btn-sm"
              disabled={detail.alert.status !== 'firing' || busy === `ack:${detail.alert.id}`}
              onClick={() => onAck(detail.alert.id)}
            >
              {'确认'}
            </button>
            <button
              className="btn btn-sm"
              disabled={detail.alert.status !== 'firing' || busy === `ignore:${detail.alert.id}`}
              onClick={() => onIgnore(detail.alert.id)}
            >
              {'忽略'}
            </button>
            {detail.alert.status !== 'firing' && <span className="muted-text">{'告警已结束，无需处置。'}</span>}
            {pushable && channels.length > 0 && (
              <>
                <select
                  className="monitoring-input"
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
                  className="btn btn-sm"
                  disabled={!retryChannel || busy === `retry:${detail.latestEvent?.id}`}
                  onClick={() => detail.latestEvent && onRetryPush(detail.latestEvent.id, retryChannel)}
                >
                  {'重试推送'}
                </button>
              </>
            )}
            <button className="btn btn-sm" onClick={() => onOpenAccount(detail.alert.targetId)}>{'查看账号'}</button>
          </div>
        </Modal>
      )}
    </div>
  );
}
