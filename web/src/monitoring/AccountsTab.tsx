import { useMemo, useState } from 'react';
import { BrandIcon, Modal } from '../components/ui';
import type { MonitorAccount } from './types';
import { hasTime } from './types';
import { displayTime, quotaText, relativeToNow, SEVERITY_TONE } from './format';

export interface AccountsTabProps {
  accounts: MonitorAccount[];
  timezone: string;
  /**
   * 品牌图标键。控制台已有 `providerIcon(provider)`（它读完整 Provider 对象），
   * 监控中心只拿到精简视图，所以由外层把真实图标键传进来；不传就退回名字
   * 首字母，绝不自己造图标键（猜错的键会让 mask 图 404 成空洞）。
   */
  iconOf?: (providerId: string) => string | undefined;
  /** 当前 firing 告警数，用来标状态点与「查看告警」。 */
  alertCountOf: (providerId: string) => number;
  busy: string;
  error: string;
  onRenew: (providerId: string) => void;
  onRefreshQuota: (providerId: string) => void;
  onOpenAlerts: (providerId: string) => void;
  onOpenLogs: (providerId: string) => void;
  /** 抽屉受控：概览「详情」与深链 ?account= 都要能直接打开某个账号。 */
  selectedId: string;
  onSelect: (providerId: string) => void;
  onCloseDrawer: () => void;
}

const AUTH_LABELS: Record<string, string> = {
  api_key: 'API Key',
  claude_oauth: 'Claude 订阅',
  chatgpt_oauth: 'ChatGPT 订阅',
  cursor_oauth: 'Cursor 订阅',
  qoder_pat: 'Qoder 订阅',
  qoder_cn: 'Qoder CN 订阅',
  workbuddy: 'WorkBuddy 订阅',
  workbuddy_ai: 'WorkBuddy AI 订阅',
  zcode: 'ZCode 订阅',
  trae_cn: 'Trae CN 订阅',
  trae_global: 'Trae Global 订阅',
  opencode_zen: 'OpenCode Zen',
};

const CAPABILITY_LABELS: Array<{ key: keyof MonitorAccount['capabilities']; label: string }> = [
  { key: 'oauth', label: '凭证续期' },
  { key: 'quota', label: '额度' },
  { key: 'models', label: '模型列表' },
  { key: 'checkin', label: '每日签到' },
];

export function AccountsTab(props: AccountsTabProps) {
  const {
    accounts, timezone, iconOf, alertCountOf, busy, error,
    onRenew, onRefreshQuota, onOpenAlerts, onOpenLogs, selectedId, onSelect, onCloseDrawer,
  } = props;
  const [search, setSearch] = useState('');
  const [onlyProblems, setOnlyProblems] = useState(false);
  const now = Date.now();

  const filtered = useMemo(() => {
    const term = search.trim().toLowerCase();
    return accounts
      .filter((account) => (onlyProblems ? alertCountOf(account.providerId) > 0 || !!account.quota?.error : true))
      .filter(
        (account) =>
          !term ||
          account.name.toLowerCase().includes(term) ||
          account.providerId.toLowerCase().includes(term) ||
          account.vendor.toLowerCase().includes(term),
      )
      .sort((a, b) => a.name.localeCompare(b.name, 'zh-CN'));
  }, [accounts, search, onlyProblems, alertCountOf]);

  const detail = accounts.find((account) => account.providerId === selectedId) ?? null;

  return (
    <div className="card panel">
      <div className="panel-header">
        <div>
          <h2 className="panel-title">{`账号矩阵（${filtered.length}/${accounts.length}）`}</h2>
          <p className="panel-desc">
            {'额度取最近一次采样，超过阈值时长标为「数据过期」；没有接口的厂商显示「不适用」，不是 0。'}
          </p>
        </div>
        <div className="panel-header-actions" style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
          <input
            className="monitoring-input"
            placeholder="搜索账号 / 厂商"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
            aria-label="搜索账号"
          />
          <button
            className={`status-pill-btn ${onlyProblems ? 'active' : ''}`}
            onClick={() => setOnlyProblems((value) => !value)}
          >
            {'只看有问题'}
          </button>
        </div>
      </div>

      {error && <div className="metric-value err">{error}</div>}
      {filtered.length === 0 && <div className="muted-text">{'没有符合条件的账号。'}</div>}

      {filtered.map((account) => {
        const quota = quotaText(account.quota);
        const alerts = alertCountOf(account.providerId);
        return (
          <div className="log-row" key={account.providerId}>
            <div className="log-row-main" style={{ gridTemplateColumns: 'minmax(0,2fr) 110px 1fr 1fr 84px' }}>
              <span className="single-line" style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
                <span className={`dot ${alerts > 0 ? 'err' : 'ok'}`} aria-hidden />
                <BrandIcon icon={iconOf?.(account.providerId)} name={account.name} size={18} />
                <span className="single-line" style={{ fontWeight: 600 }}>{account.name}</span>
                {account.disabled && <span className="badge amber">{'停用'}</span>}
              </span>
              <span className="muted-text">{AUTH_LABELS[account.authType] || account.authType || 'API Key'}</span>
              <span title={`到期：${account.expiresAt ?? '无凭证或不过期'}`}>
                {hasTime(account.expiresAt) ? relativeToNow(account.expiresAt, now) : '不过期'}
              </span>
              <span className={account.quota?.error ? 'err' : ''} title={quota.note}>
                {quota.primary}
                {account.quota?.stale && <span className="badge amber" style={{ marginLeft: 6 }}>{'数据过期'}</span>}
              </span>
              <span>
                {alerts > 0 ? (
                  <span className={`badge ${SEVERITY_TONE.p1}`}>{`告警 ${alerts}`}</span>
                ) : account.healthStatus === 'unavailable' ? (
                  <span className="badge amber">{'异常'}</span>
                ) : (
                  <span className="badge green">{'正常'}</span>
                )}
              </span>
              <button className="btn btn-sm" onClick={() => onSelect(account.providerId)}>{'详情'}</button>
            </div>
          </div>
        );
      })}

      {detail && (
        <Modal
          title={detail.name}
          description={`${AUTH_LABELS[detail.authType] || detail.authType || 'API Key'} · ${detail.vendor || '未知厂商'}`}
          onClose={onCloseDrawer}
          size="wide"
        >
          <div className="monitoring-cols">
            <div className="card">
              <div className="panel-title">{'凭证'}</div>
              <div className="metric-label">{'到期时间'}</div>
              <div className="metric-value" title={detail.expiresAt ?? ''}>
                {hasTime(detail.expiresAt)
                  ? `${displayTime(detail.expiresAt, timezone).text}（${relativeToNow(detail.expiresAt, Date.now())}）`
                  : '不适用'}
              </div>
              <div className="metric-label" style={{ marginTop: 10 }}>{'最近续期'}</div>
              <div className="metric-note">
                {detail.renewal?.known
                  ? `${detail.renewal.lastOK ? '成功' : '失败'} · ${relativeToNow(detail.renewal.lastAt, Date.now())}${
                      detail.renewal.consecutive > 0 ? ` · 连续失败 ${detail.renewal.consecutive} 次` : ''
                    }`
                  : '还没有续期记录'}
              </div>
            </div>

            <div className="card">
              <div className="panel-title">{'额度 / 积分'}</div>
              <div className="metric-value">{quotaText(detail.quota).primary}</div>
              <div className="metric-note">
                {detail.quota
                  ? `${quotaText(detail.quota).note} · 采样 ${displayTime(detail.quota.observedAt, timezone).text}`
                  : '该厂商没有额度接口'}
              </div>
              {detail.quota?.error && <div className="metric-note err">{`读取失败：${detail.quota.error}`}</div>}
            </div>

            <div className="card">
              <div className="panel-title">{'健康与能力'}</div>
              <div className="metric-note">
                {`状态 ${detail.healthStatus || '未检测'} · 模型列表连续失败 ${detail.modelFailures} 次 · 活跃告警 ${alertCountOf(detail.providerId)}`}
              </div>
              <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap', marginTop: 8 }}>
                {CAPABILITY_LABELS.map((item) => (
                  <span key={item.key} className={`badge ${detail.capabilities[item.key] ? 'green' : 'slate'}`}>
                    {`${item.label}：${detail.capabilities[item.key] ? '支持' : '不适用'}`}
                  </span>
                ))}
              </div>
            </div>

            <div className="card">
              <div className="panel-title">{'操作'}</div>
              <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                <button
                  className="btn btn-sm"
                  disabled={busy === `renew:${detail.providerId}` || !detail.capabilities.oauth}
                  onClick={() => onRenew(detail.providerId)}
                  title={detail.capabilities.oauth ? '按厂商自己的窗口续期；未到窗口不会强行轮换' : '该账号没有可续期凭证'}
                >
                  {'立即续期'}
                </button>
                <button
                  className="btn btn-sm"
                  disabled={busy === `quota:${detail.providerId}` || !detail.capabilities.quota}
                  onClick={() => onRefreshQuota(detail.providerId)}
                  title={detail.capabilities.quota ? '强制读一次厂商用量接口' : '该账号没有额度接口'}
                >
                  {'刷新额度'}
                </button>
                <button className="btn btn-sm" onClick={() => onOpenAlerts(detail.providerId)}>{'查看告警'}</button>
                <button className="btn btn-sm" onClick={() => onOpenLogs(detail.providerId)}>{'查看请求日志'}</button>
              </div>
              <div className="metric-note" style={{ marginTop: 8 }}>
                {'监控中心不提供停用、隔离或改配置的按钮；处置只在请求调度里做。'}
              </div>
            </div>
          </div>
        </Modal>
      )}
    </div>
  );
}
