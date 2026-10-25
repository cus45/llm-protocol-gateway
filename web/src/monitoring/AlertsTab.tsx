import { useState } from 'react';
import { Modal } from '../components/ui';
import type { MonitorAlert, MonitorAlertDetail, MonitorChannelView, MonitorSeverity, MonitorTimelineEntry } from './types';
import { SEVERITY_TEXT } from './types';
import { detailPairs, displayTime, displayTimeCompact, relativeToNow } from './format';

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
  lastProbeAt: '最近探测',
  lastStatus: '最近状态码',
  threshold: '触发阈值',
  costLimitMicros: '年度成本上限（微元）',
  costUsedMicros: '已用成本（微元）',
  quotaWindow: '额度窗口',
  ratio: '读数占比',
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

/** 处置状态徽标：待处置用中性灰，已确认绿、已忽略灰；颜色之外永远有文字。 */
function dispositionTone(disposition: string): string {
  return disposition === 'acknowledged' ? 'ok' : 'off';
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

  /*
   * 统计条的口径：当前这一页里按级别/状态各多少。列表是分页的（后端按合计报 total），
   * 这里不按比例外推，题注写明「本页 N / 共 M」；多少只数已经取回的行。
   */
  const severityCount = { p0: 0, p1: 0, p2: 0, info: 0 } as Record<MonitorSeverity, number>;
  let firing = 0;
  let acknowledged = 0;
  let ignored = 0;
  for (const alert of alerts) {
    severityCount[alert.severity] += 1;
    if (alert.status === 'firing') firing += 1;
    if (alert.disposition === 'acknowledged') acknowledged += 1;
    if (alert.disposition === 'ignored') ignored += 1;
  }
  const resolvedShown = alerts.length - firing;

  const filtered = statusFilter !== '' || severityFilter !== '';

  return (
    <div className="mon-alerts">
      <div className="mon-alerts-bar">
        <div className="mon-alerts-stats" role="group" aria-label="本页告警统计">
          <div className="mon-alerts-stat sev-p0">
            <span className="mon-alerts-stat-label">{'P0 严重'}</span>
            <span className="mon-alerts-stat-value">{severityCount.p0.toLocaleString('zh-CN')}</span>
          </div>
          <div className="mon-alerts-stat sev-p1">
            <span className="mon-alerts-stat-label">{'P1 重要'}</span>
            <span className="mon-alerts-stat-value">{severityCount.p1.toLocaleString('zh-CN')}</span>
          </div>
          <div className="mon-alerts-stat sev-p2">
            <span className="mon-alerts-stat-label">{'P2 提醒'}</span>
            <span className="mon-alerts-stat-value">{severityCount.p2.toLocaleString('zh-CN')}</span>
          </div>
          <div className="mon-alerts-stat">
            <span className="mon-alerts-stat-label">{'进行中'}</span>
            <span className="mon-alerts-stat-value">{firing.toLocaleString('zh-CN')}</span>
          </div>
          <div className="mon-alerts-stat">
            <span className="mon-alerts-stat-label">{'已确认'}</span>
            <span className="mon-alerts-stat-value">{acknowledged.toLocaleString('zh-CN')}</span>
          </div>
          <div className="mon-alerts-stat">
            <span className="mon-alerts-stat-label">{'已忽略'}</span>
            <span className="mon-alerts-stat-value">{ignored.toLocaleString('zh-CN')}</span>
          </div>
        </div>
        <p className="mon-alerts-note">
          {`本页 ${alerts.length.toLocaleString('zh-CN')} 条 · 合计 ${total.toLocaleString('zh-CN')} 条${
            resolvedShown > 0 ? ` · 其中已恢复 ${resolvedShown.toLocaleString('zh-CN')} 条` : ''
          }${filtered ? ' · 当前有筛选' : ''}`}
        </p>
      </div>

      <div className="mon-alerts-toolbar">
        <div className="mon-alerts-filter">
          <span className="mon-alerts-filter-label">{'状态'}</span>
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
                {chip.label}
              </button>
            ))}
          </div>
        </div>
        <div className="mon-alerts-filter">
          <span className="mon-alerts-filter-label">{'级别'}</span>
          <div className="mon-seg" role="group" aria-label="按级别筛选">
            {SEVERITY_CHIPS.map((value) => (
              <button
                key={value || 'any'}
                type="button"
                className={`mon-seg-btn${severityFilter === value ? ' on' : ''}`}
                aria-pressed={severityFilter === value}
                title={value ? SEVERITY_TEXT[value] : '不按级别筛选'}
                onClick={() => onSeverityFilter(value)}
              >
                {value ? value.toUpperCase() : '全部'}
              </button>
            ))}
          </div>
        </div>
        {/* 时间范围由外层固定为最近 7 天（与告警查询的 range 参数同口径），这里如实写明，不给假下拉。 */}
        <div className="mon-alerts-filter">
          <span className="mon-alerts-filter-label">{'时间范围'}</span>
          <span className="mon-alerts-fixed" title="告警查询固定取最近 7 天的事件，与接口 range 参数一致">{'最近 7 天'}</span>
        </div>
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
            const disposition = alert.disposition !== 'open' || alert.status !== 'firing';
            const known = ruleKnown(alert.ruleKey);
            return (
              <article className={`mon-alert-row tone-${alert.severity}${alert.status === 'firing' ? '' : ' is-resolved'}`} key={alert.id}>
                <div className="mon-alert-main">
                  <div className="mon-alert-head">
                    <span
                      className={`mon-alert-sev tone-${alert.severity}`}
                      title={SEVERITY_TEXT[alert.severity]}
                    >
                      {alert.severity === 'info' ? '信息' : alert.severity.toUpperCase()}
                    </span>
                    <h3 className="mon-alert-title" title={known ? undefined : `规则 key：${alert.ruleKey}（规则表里没有这一条）`}>
                      {ruleLabel(alert.ruleKey)}
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
                  <div className="mon-alert-meta">
                    <span className="mon-alert-meta-item">
                      {alert.status === 'firing' ? (
                        <span className="mon-alert-state firing">
                          <span className="mon-alert-state-dot" aria-hidden />
                          {'进行中'}
                        </span>
                      ) : (
                        <span className="mon-alert-state resolved">
                          {'已恢复'}
                          <span className="mon-alert-meta-sub">
                            {` · ${RESOLUTION_TEXT[alert.resolutionReason ?? ''] ?? '原因未记录'}`}
                          </span>
                        </span>
                      )}
                    </span>
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
                    <span className="mon-alert-meta-item">
                      <span className={`mon-alert-push push-${alert.pushStatus ?? 'none'}`}>
                        {PUSH_LABELS[alert.pushStatus ?? ''] ?? '未通知'}
                      </span>
                    </span>
                    {disposition && (
                      <span className={`mon-acc-state ${dispositionTone(alert.disposition)}`} title="确认/忽略只影响推送，不会停止检测">
                        <span className="mon-acc-state-dot" aria-hidden />
                        {DISPOSITION_TEXT[alert.disposition] ?? '已处置'}
                      </span>
                    )}
                  </div>
                </div>
                <div className="mon-alert-actions">
                  <button type="button" className="btn btn-sm" onClick={() => onOpen(alert.id)}>
                    {'处理'}
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
            </div>
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
            <button type="button" className="btn btn-sm" onClick={() => onOpenAccount(detail.alert.targetId)}>
              {detail.alert.targetType === 'provider' ? '查看账号' : `查看${TARGET_TEXT[detail.alert.targetType] ?? '目标'}`}
            </button>
          </div>
        </Modal>
      )}
    </div>
  );
}
