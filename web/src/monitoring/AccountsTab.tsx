import {useMemo, useState} from 'react';
import {BrandIcon, Modal} from '../components/ui';
import type {MonitorAccount} from './types';
import {hasTime} from './types';
import {displayTime, formatLatency, quotaText, relativeToNow} from './format';

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
  /** 卡片内动作（续期/刷额度）的结果文案，直接显示在动作行下面（抽屉打开时同样可见）。 */
  note: string;
  actionError: string;
  onRenew: (providerId: string) => void;
  onRefreshQuota: (providerId: string) => void;
  onOpenAlerts: (providerId: string) => void;
  onOpenLogs: (providerId: string) => void;
  /** 抽屉里的「立即探测」busy 键（`probe:<providerId>`）。 */
  probeBusy: string;
  /** 「立即探测」的入口（卡片与抽屉共用），返回 null 表示请求本身没发出去。 */
  onRunProbe: (providerId: string) => Promise<unknown> | void;
  /** `/__monitor/probes` 归一后的每账号点阵与指标；不传时点阵退回「无记录」。 */
  probeView?: AccountsProbeView;
  /** 抽屉受控：概览「详情」与深链 ?account= 都要能直接打开某个账号。 */
  selectedId: string;
  onSelect: (providerId: string) => void;
  onCloseDrawer: () => void;
}

/**
 * 卡片视图的两个输入：`/__monitor/accounts` 的快照与 `/__monitor/probes` 的每账号探测现状，
 * 由 MonitoringPage 把接口原样传进来（页面已按 providerId 归一）。账号页自己不取数，
 * 免得与页面轮询各读一次、两边时点不一致。
 */
export interface AccountsProbeFacts {
  probed: boolean;
  consecutiveFailures: number;
  lastProbeAt?: string;
  /** 最近一发的状态码。 */
  lastStatus?: number;
  /** 点阵：最近 N 发（新→旧），每一发按后端 MonitorProbeRun.ok 落绿/红。 */
  recent: boolean[];
  /** 当日次数 / 每日上限；maxPerDay=0 是「不限」。 */
  usedToday: number;
  maxPerDay: number;
  /** 探测耗时：最近一发未被跳过的探测（与最近一发同为 ok/失败）的 durationMs；没有有效记录时 undefined。 */
  rttMs?: number;
}

export interface AccountsProbeView {
  /** 卡片点阵保留的探测发数。 */
  windowSize: number;
  factsOf: (providerId: string) => AccountsProbeFacts;
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

type AccountState = 'ok' | 'warn' | 'bad' | 'off';

const STATE_TEXT: Record<AccountState, string> = {
  ok: '正常',
  warn: '告警',
  bad: '异常',
  off: '停用',
};

type StateFilter = 'all' | 'problem' | AccountState;

/**
 * 卡片与筛选共用的账号状态。判定顺序必须与概览页一致（概览说异常、这里说正常就是自相矛盾）：
 * 停用 → 有 firing 告警 → 请求判定异常 → 否则正常。
 */
function stateOf(account: MonitorAccount, alerts: number): AccountState {
  if (account.disabled) return 'off';
  if (alerts > 0) return 'warn';
  if (account.healthStatus === 'unavailable') return 'bad';
  return 'ok';
}

const STATE_RANK: Record<AccountState, number> = { bad: 0, warn: 1, off: 2, ok: 3 };

/**
 * 账号名按空白插零宽断点：长名在词与词之间折行，而不是把整段挤成省略号。
 * 中文名没有空格，靠 CSS 的 overflow-wrap: anywhere 在任意字间断。
 */
function NameText({ name }: { name: string }) {
  return (
    <>
      {name.split(/(\s+)/).map((part, index) =>
        /^\s+$/.test(part) ? (
          <span key={index}>
            {part}
            <wbr />
          </span>
        ) : (
          part
        ),
      )}
    </>
  );
}

/** 续期文案：没过期时间就写「不过期」，不把 Go 零值时间当成公元 1 年。 */
function expiryText(account: MonitorAccount, now: number): string {
  if (!hasTime(account.expiresAt)) return '不过期';
  return `续期 ${relativeToNow(account.expiresAt, now)}`;
}

/** 当天已用次数；上限为 0 时后端意为「不限」，此时不能写成「x / 0」。 */
function usedTodayText(facts: AccountsProbeFacts): string {
  return facts.maxPerDay === 0 ? `${facts.usedToday}（不限）` : `${facts.usedToday} / ${facts.maxPerDay}`;
}

/** 最近一发的结论。没有状态码就是网络层失败，0 与 200 不是一回事，要分开写。 */
function lastProbeText(facts: AccountsProbeFacts): string {
  if (!facts.probed) return '未探测';
  if (facts.lastStatus) return `HTTP ${facts.lastStatus}`;
  return '网络失败';
}

function ProbeStrip({ facts, windowSize }: { facts: AccountsProbeFacts; windowSize: number }) {
  // 新→旧排；右侧补灰色占位到 windowSize，点数恒定，卡与卡之间的点阵长度一致可比。
  const cells: Array<{ tone: 'ok' | 'err' | 'none'; title: string }> = facts.recent
    .slice(0, windowSize)
    .map((ok, index) => ({
      tone: ok ? 'ok' : 'err',
      title: ok ? `第 ${index + 1} 近发：通过` : `第 ${index + 1} 近发：失败`,
    }));
  while (cells.length < windowSize) {
    cells.push({ tone: 'none', title: '没有更多探测记录' });
  }
  const measured = facts.recent.length;
  const okCount = facts.recent.filter(Boolean).length;
  return (
    <>
      <div className="mon-acc-probe-head">
        <span className="mon-acc-probe-label">{`最近 ${windowSize} 次轻探测`}</span>
        <span className="mon-acc-probe-note">
          {measured > 0 ? `${Math.round((okCount / measured) * 100)}% 成功（${measured} 发）` : '暂无记录'}
        </span>
      </div>
      <div className="mon-acc-probe-dots" title={measured > 0 ? `最近 ${measured} 发探测，右为最近` : '还没有探测记录'}>
        {cells.map((cell, index) => (
          <span key={index} className={`mon-acc-dot ${cell.tone}`} title={cell.title} />
        ))}
      </div>
    </>
  );
}

export function AccountsTab(props: AccountsTabProps) {
  const {
    accounts, timezone, iconOf, alertCountOf, busy, error, note, actionError,
    onRenew, onRefreshQuota, onOpenAlerts, onOpenLogs, probeBusy, onRunProbe, probeView,
    selectedId, onSelect, onCloseDrawer,
  } = props;
  const [search, setSearch] = useState('');
  const [vendor, setVendor] = useState('all');
  const [stateFilter, setStateFilter] = useState<StateFilter>('all');
  const [authFilter, setAuthFilter] = useState('all');
  const now = Date.now();

  const vendors = useMemo(() => {
    const seen = new Map<string, number>();
    for (const account of accounts) {
      const label = account.vendor || '未知厂商';
      seen.set(label, (seen.get(label) ?? 0) + 1);
    }
    return [...seen.entries()].sort((a, b) => a[0].localeCompare(b[0], 'zh-CN'));
  }, [accounts]);

  const authTypes = useMemo(() => {
    const seen = new Set<string>();
    for (const account of accounts) seen.add(AUTH_LABELS[account.authType] || account.authType || 'API Key');
    return [...seen].sort((a, b) => a.localeCompare(b, 'zh-CN'));
  }, [accounts]);

  const filtered = useMemo(() => {
    const term = search.trim().toLowerCase();
    return accounts
      .filter((account) => (vendor === 'all' ? true : (account.vendor || '未知厂商') === vendor))
      .filter((account) => {
        const state = stateOf(account, alertCountOf(account.providerId));
        if (stateFilter === 'problem') return state === 'bad' || state === 'warn';
        if (stateFilter !== 'all') return state === stateFilter;
        return true;
      })
      .filter((account) =>
        authFilter === 'all' ? true : (AUTH_LABELS[account.authType] || account.authType || 'API Key') === authFilter,
      )
      .filter(
        (account) =>
          !term ||
          account.name.toLowerCase().includes(term) ||
          account.providerId.toLowerCase().includes(term) ||
          account.vendor.toLowerCase().includes(term),
      )
      .sort((a, b) => {
        const delta = STATE_RANK[stateOf(a, alertCountOf(a.providerId))] - STATE_RANK[stateOf(b, alertCountOf(b.providerId))];
        if (delta !== 0) return delta;
        return a.name.localeCompare(b.name, 'zh-CN');
      });
  }, [accounts, search, vendor, stateFilter, authFilter, alertCountOf]);

  const detail = accounts.find((account) => account.providerId === selectedId) ?? null;
  const detailFacts = detail && probeView ? probeView.factsOf(detail.providerId) : undefined;
  const probing = !!detail && probeBusy === `probe:${detail.providerId}`;
  const windowSize = probeView?.windowSize ?? 10;
  // 能用的功能才出现：停用账号自动调度会跳过，手动探测也只会写一条 skipped，没有排障
  // 价值，所以直接不给入口。其余账号都给——「没配模型」由后端在按下时以 skipped 结果
  // 回给抽屉，界面如实展示那一句，不在前端猜（前端拿不到 model 字段）。
  const canProbe = !!detail && !detail.disabled;
  const detailProbing = detail ? probeBusy === `probe:${detail.providerId}` : false;

  const empty = accounts.length === 0;

  return (
    <div className="mon-accounts">
      <div className="card panel mon-acc-panel">
        <div className="panel-header mon-acc-head">
          <div>
            <h2 className="panel-title">{'账号矩阵'}</h2>
            <p className="panel-desc">
              {'点阵与连续失败取自轻探测记录；额度取最近一次采样，超过阈值标为「数据过期」；没有接口的厂商显示「不适用」，不是 0。'}
            </p>
          </div>
        </div>

        {error && <div className="mon-acc-error">{error}</div>}

        <div className="mon-acc-toolbar" role="group" aria-label="账号筛选">
          <label className="mon-acc-search">
            <SearchIcon />
            <input
              className="monitoring-input"
              placeholder="按账号名、Provider ID、厂商搜索"
              value={search}
              onChange={(event) => setSearch(event.target.value)}
              aria-label="搜索账号"
            />
          </label>

          <div className="mon-acc-vendors" role="group" aria-label="厂商">
            <button
              type="button"
              className={`mon-acc-vendor${vendor === 'all' ? ' on' : ''}`}
              aria-pressed={vendor === 'all'}
              onClick={() => setVendor('all')}
            >
              {'全部'}
              <span className="mon-acc-vendor-count">{accounts.length}</span>
            </button>
            {vendors.map(([label, count]) => (
              <button
                key={label}
                type="button"
                className={`mon-acc-vendor${vendor === label ? ' on' : ''}`}
                aria-pressed={vendor === label}
                onClick={() => setVendor(label)}
              >
                {label}
                <span className="mon-acc-vendor-count">{count}</span>
              </button>
            ))}
          </div>

          <div className="mon-acc-selects">
            <label className="mon-acc-select">
              <span className="mon-acc-select-label">{'状态'}</span>
              <select
                className="monitoring-input mon-acc-select-input"
                value={stateFilter}
                onChange={(event) => setStateFilter(event.target.value as StateFilter)}
              >
                <option value="all">{'全部状态'}</option>
                <option value="problem">{'只看有问题'}</option>
                <option value="ok">{'正常'}</option>
                <option value="warn">{'告警'}</option>
                <option value="bad">{'异常'}</option>
                <option value="off">{'停用'}</option>
              </select>
            </label>
            <label className="mon-acc-select">
              <span className="mon-acc-select-label">{'凭证'}</span>
              <select
                className="monitoring-input mon-acc-select-input"
                value={authFilter}
                onChange={(event) => setAuthFilter(event.target.value)}
              >
                <option value="all">{'全部凭证'}</option>
                {authTypes.map((label) => (
                  <option key={label} value={label}>
                    {label}
                  </option>
                ))}
              </select>
            </label>
          </div>

          <span className="mon-acc-count">{`${filtered.length} / ${accounts.length} 个账号`}</span>
        </div>

        {empty && <div className="m-empty">{'还没有账号。接入 Provider 后这里会出现账号矩阵。'}</div>}
        {!empty && filtered.length === 0 && <div className="m-empty">{'没有符合当前筛选的账号。'}</div>}

        <div className="mon-acc-grid">
          {filtered.map((account) => {
            const alerts = alertCountOf(account.providerId);
            const state = stateOf(account, alerts);
            const facts = probeView?.factsOf(account.providerId);
            const quota = quotaText(account.quota);
            const quotaTone = account.quota?.error
              ? 'err'
              : account.quota && account.quota.ratio >= 0 && account.quota.ratio < 0.1
                ? 'warn'
                : '';
            const cardProbing = probeBusy === `probe:${account.providerId}`;
            const renewing = busy === `renew:${account.providerId}`;
            const fresh = selectedId === account.providerId;
            return (
              <article
                key={account.providerId}
                className={`mon-acc-card state-${state}${fresh ? ' is-open' : ''}`}
                aria-label={`账号 ${account.name}`}
              >
                <div className="mon-acc-card-top">
                  <span className="mon-acc-tag" title={`厂商：${account.vendor || '未知'} · 凭证：${AUTH_LABELS[account.authType] || account.authType || 'API Key'}`}>
                    {account.vendor || AUTH_LABELS[account.authType] || 'API Key'}
                  </span>
                  <span className={`mon-acc-state ${state}`}>
                    <span className="mon-acc-state-dot" aria-hidden />
                    {alerts > 0 && state === 'warn' ? `告警 ${alerts}` : STATE_TEXT[state]}
                  </span>
                </div>

                <h3 className="mon-acc-name">
                  <BrandIcon icon={iconOf?.(account.providerId)} name={account.name} size={18} />
                  <span className="mon-acc-name-text" title={account.name}>
                    <NameText name={account.name} />
                  </span>
                </h3>
                <div className="mon-acc-sub" title={account.providerId}>
                  {`${AUTH_LABELS[account.authType] || account.authType || 'API Key'} · ${account.providerId}`}
                </div>

                <div className="mon-acc-metrics">
                  <div className="mon-acc-metric">
                    <span className="mon-acc-metric-label">{'最近一发'}</span>
                    <span className="mon-acc-metric-value">{facts ? lastProbeText(facts) : '—'}</span>
                    <span className="mon-acc-metric-note">{facts?.probed ? '轻探测结论' : '未探测'}</span>
                  </div>
                  <div className="mon-acc-metric">
                    <span className="mon-acc-metric-label">{'探测 RTT'}</span>
                    <span className="mon-acc-metric-value">
                      {facts?.rttMs === undefined ? '—' : formatLatency(facts.rttMs)}
                    </span>
                    <span className="mon-acc-metric-note">
                      {facts?.rttMs === undefined ? '无有效耗时' : '端到端往返'}
                    </span>
                  </div>
                  <div className="mon-acc-metric">
                    <span className="mon-acc-metric-label">{'连续失败'}</span>
                    <span
                      className={`mon-acc-metric-value${
                        facts && facts.probed && facts.consecutiveFailures > 0 ? ' bad' : ''
                      }`}
                      title={facts && facts.probed ? undefined : '还没有可用的探测记录'}
                    >
                      {facts && facts.probed ? `${facts.consecutiveFailures} 次` : '—'}
                    </span>
                    <span className="mon-acc-metric-note">
                      {facts && facts.probed
                        ? facts.consecutiveFailures > 0
                          ? '已连续失败'
                          : '当前无连败'
                        : '无记录'}
                    </span>
                  </div>
                </div>

                {facts ? (
                  <div className="mon-acc-probe">
                    <ProbeStrip facts={facts} windowSize={windowSize} />
                    <div className="mon-acc-probe-meta" title="按投递时区计的当日探测次数 / 每日上限；上限 0 表示不限">
                      {`今日探测 ${usedTodayText(facts)} · ${
                        hasTime(facts.lastProbeAt) ? `最近 ${relativeToNow(facts.lastProbeAt, now)}` : '还没有记录'
                      }`}
                    </div>
                  </div>
                ) : (
                  <div className="mon-acc-probe">
                    <div className="mon-acc-probe-head">
                      <span className="mon-acc-probe-label">{`最近 ${windowSize} 次轻探测`}</span>
                      <span className="mon-acc-probe-note">{'读取中'}</span>
                    </div>
                  </div>
                )}

                <div className="mon-acc-facts">
                  <div className="mon-acc-fact" title={`凭证到期：${account.expiresAt ?? '无凭证或不过期'}`}>
                    <span className="mon-acc-fact-key">
                      <ClockIcon />
                      {'续期'}
                    </span>
                    <span className="mon-acc-fact-value">{expiryText(account, now)}</span>
                  </div>
                  <div className="mon-acc-fact right" title={quota.note}>
                    <span className="mon-acc-fact-key">{'额度'}</span>
                    <span className={`mon-acc-fact-value ${quotaTone}`}>{quota.primary}</span>
                    {account.quota?.stale && <span className="badge amber">{'数据过期'}</span>}
                  </div>
                </div>

                <div className="mon-acc-actions">
                  <button type="button" className="btn btn-sm mon-acc-btn" onClick={() => onSelect(account.providerId)}>
                    {'详情'}
                  </button>
                  <button
                    type="button"
                    className="btn btn-sm mon-acc-btn"
                    disabled={renewing || !account.capabilities.oauth}
                    onClick={() => onRenew(account.providerId)}
                    title={
                      account.capabilities.oauth
                        ? '按厂商自己的窗口续期；未到窗口不会强行轮换'
                        : '该账号没有可续期凭证'
                    }
                  >
                    {renewing ? '续期中…' : '续期'}
                  </button>
                  <button
                    type="button"
                    className="btn btn-sm mon-acc-btn"
                    disabled={cardProbing || account.disabled}
                    onClick={() => void onRunProbe(account.providerId)}
                    title={account.disabled ? '账号已停用，探测跳过' : '现在就发一发最小对话探测'}
                  >
                    {cardProbing ? '探测中…' : '轻探测'}
                  </button>
                </div>
                {selectedId === account.providerId && actionError && (
                  <div className="mon-acc-inline err">{actionError}</div>
                )}
                {selectedId === account.providerId && !actionError && note && <div className="mon-acc-inline">{note}</div>}
                {alerts > 0 && (
                  <button type="button" className="mon-acc-alert-link" onClick={() => onOpenAlerts(account.providerId)}>
                    {`${alerts} 条未恢复告警，查看 →`}
                  </button>
                )}
              </article>
            );
          })}
        </div>
      </div>

      {detail && (
        <Modal
          title={detail.name}
          description={`${AUTH_LABELS[detail.authType] || detail.authType || 'API Key'} · ${detail.vendor || '未知厂商'} · ${detail.providerId}`}
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
              {detailFacts ? (
                <>
                  <div className={`metric-value ${detailFacts.probed && detailFacts.consecutiveFailures > 0 ? 'err' : ''}`}>
                    {detailFacts.probed
                      ? detailFacts.consecutiveFailures > 0
                        ? `连续失败 ${detailFacts.consecutiveFailures} 次`
                        : '通过'
                      : '暂无数据'}
                  </div>
                  <div className="metric-note">
                    {hasTime(detailFacts.lastProbeAt)
                      ? `最近一次执行 ${displayTime(detailFacts.lastProbeAt, timezone).text}（${relativeToNow(detailFacts.lastProbeAt, Date.now())}）`
                      : '还没有探测记录'}
                  </div>
                  <div className="metric-note" title="当日次数 / 每日上限，按投递时区计算">
                    {`当日次数 ${detailFacts.maxPerDay === 0 ? `${detailFacts.usedToday} / 不限` : `${detailFacts.usedToday} / ${detailFacts.maxPerDay}`} · 连续失败 ${detailFacts.consecutiveFailures} 次`}
                  </div>
                  <div className="mon-acc-drawer-dots">
                    <ProbeStrip facts={detailFacts} windowSize={windowSize} />
                  </div>
                  {detailFacts.probed && detailFacts.consecutiveFailures > 0 && (
                    <div className="metric-note err">{'探测未跑通，已进入 probe_failed 判定'}</div>
                  )}
                  {detail.disabled && <div className="metric-note">{'账号已停用，自动调度跳过它，也不会发探测。'}</div>}
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
                    disabled={detailProbing}
                    onClick={() => void onRunProbe(detail.providerId)}
                    title={'现在就发一发最小对话探测；同账号同时只跑一发'}
                  >
                    {detailProbing ? '探测中…' : '立即探测'}
                  </button>
                )}
                <button className="btn btn-sm" onClick={() => onOpenAlerts(detail.providerId)}>{'查看告警'}</button>
                <button className="btn btn-sm" onClick={() => onOpenLogs(detail.providerId)}>{'查看请求日志'}</button>
              </div>
              {actionError && <div className="metric-note err" style={{ marginTop: 8 }}>{actionError}</div>}
              {note && !actionError && <div className="metric-note" style={{ marginTop: 8 }}>{note}</div>}
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

function SearchIcon() {
  return (
    <svg className="mon-acc-search-icon" viewBox="0 0 24 24" width="14" height="14" aria-hidden="true">
      <circle cx="10.5" cy="10.5" r="6.2" fill="none" stroke="currentColor" strokeWidth="1.9" />
      <path d="M15.2 15.2 20 20" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" />
    </svg>
  );
}

function ClockIcon() {
  return (
    <svg className="mon-acc-fact-icon" viewBox="0 0 24 24" width="12" height="12" aria-hidden="true">
      <circle cx="12" cy="12" r="8.2" fill="none" stroke="currentColor" strokeWidth="1.9" />
      <path d="M12 7.6V12l3 1.9" fill="none" stroke="currentColor" strokeWidth="1.9" strokeLinecap="round" />
    </svg>
  );
}
