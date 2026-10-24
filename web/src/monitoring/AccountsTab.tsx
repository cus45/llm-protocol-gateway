import { useMemo, useState } from 'react';
import { BrandIcon, Modal } from '../components/ui';
import type { MonitorAccount, MonitorProbeAccount, MonitorProbeRunResult } from './types';
import { hasTime } from './types';
import { displayTime, probeSummaryText, quotaText, relativeToNow, SEVERITY_TONE } from './format';
import { probeSkipText } from './types';
import { ProbeResultBody } from './ProbesTab';

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
  /** 抽屉内动作（续期/刷额度）的结果文案，就地显示。 */
  note: string;
  actionError: string;
  onRenew: (providerId: string) => void;
  onRefreshQuota: (providerId: string) => void;
  onOpenAlerts: (providerId: string) => void;
  onOpenLogs: (providerId: string) => void;
  /** 探测现状（来自 /__monitor/probes），用来显示末次结果与当日次数。 */
  probeOf: (providerId: string) => MonitorProbeAccount | undefined;
  /** 抽屉里的「立即探测」：结果就地显示在操作卡里，不走顶部提示条。 */
  probeBusy: string;
  probeResult: MonitorProbeRunResult | null;
  onRunProbe: (providerId: string) => void;
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
    accounts, timezone, iconOf, alertCountOf, busy, error, note, actionError,
    onRenew, onRefreshQuota, onOpenAlerts, onOpenLogs, probeOf, probeBusy, probeResult, onRunProbe,
    selectedId, onSelect, onCloseDrawer,
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
  const probe = detail ? probeOf(detail.providerId) : undefined;
  const probing = !!detail && probeBusy === `probe:${detail.providerId}`;
  // 连续失败大于零才需要值班人动手确认。
  // 这里**不能**再加 `probe.skipped` 条件：后端 MonitorProbeOutcome 对所有 skipped 行
  // 直接 continue 并置空 outcome.Skipped（internal/store/monitoring.go:981-991），
  // 这个字段在前端恒为 undefined；依赖它会让整个提示变成死代码。
  const needsProbe = !!probe && probe.probed && probe.consecutiveFailures > 0;
  // 能用的功能才出现：停用账号自动调度会跳过，手动探测也只会写一条 skipped，没有排障
  // 价值，所以直接不给入口。其余账号都给——「没配模型」由后端在按下时以 skipped 结果
  // 回给抽屉，界面如实展示那一句，不在前端猜（前端拿不到 model 字段）。
  const canProbe = !!detail && !detail.disabled;

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
            <div className="mrow mrow-account">
              <span className="m-clip" style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
                <span className={`dot ${alerts > 0 ? 'err' : 'ok'}`} aria-hidden />
                <BrandIcon icon={iconOf?.(account.providerId)} name={account.name} size={18} />
                <span className="m-clip" style={{ fontWeight: 600 }}>{account.name}</span>
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
              <div className="panel-title">{'轻探测'}</div>
              {probe ? (
                <>
                  <div className={`metric-value ${probe.probed && probe.consecutiveFailures > 0 ? 'err' : ''}`}>
                    {probeSummaryText(probe)}
                  </div>
                  <div className="metric-note">
                    {hasTime(probe.lastProbeAt)
                      ? `最近一次执行 ${displayTime(probe.lastProbeAt, timezone).text}（${relativeToNow(probe.lastProbeAt, Date.now())}）`
                      : '还没有探测记录'}
                    {probeSkipText(probe.skipped) ? ` · ${probeSkipText(probe.skipped)}` : ''}
                  </div>
                  <div className="metric-note" title="当日次数 / 每日上限，按投递时区计算">
                    {`当日次数 ${probe.maxPerDay === 0 ? `${probe.usedToday} / 不限` : `${probe.usedToday} / ${probe.maxPerDay}`} · 连续失败 ${probe.consecutiveFailures} 次`}
                  </div>
                  {needsProbe && (
                    <div className="metric-note err">{'探测未跑通，已进入 probe_failed 判定'}</div>
                  )}
                  {detail.disabled && <div className="metric-note">{'账号已停用，自动调度跳过它；手动探测也不适用。'}</div>}
                </>
              ) : (
                <div className="metric-note">{'探测现状读取中'}</div>
              )}
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
                {canProbe && (
                  <button
                    className="btn btn-sm"
                    disabled={probing}
                    onClick={() => onRunProbe(detail.providerId)}
                    title={'现在就发一发最小对话探测；同账号同时只跑一发'}
                  >
                    {probing ? '探测中…' : '立即探测'}
                  </button>
                )}
                <button className="btn btn-sm" onClick={() => onOpenAlerts(detail.providerId)}>{'查看告警'}</button>
                <button className="btn btn-sm" onClick={() => onOpenLogs(detail.providerId)}>{'查看请求日志'}</button>
              </div>
              {actionError && <div className="metric-note err" style={{ marginTop: 8 }}>{actionError}</div>}
              {note && !actionError && (
                <div className="metric-note" style={{ marginTop: 8 }}>{note}</div>
              )}
              {probeResult && (
                <div className="probe-result-inline">
                  <div className="metric-label">{'最近一次手动探测'}</div>
                  <ProbeResultBody result={probeResult} />
                </div>
              )}
              {!probeResult && probing && <div className="metric-note" style={{ marginTop: 8 }}>{'正在探测，结果会显示在这里。'}</div>}
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
