import {useState} from 'react';
import {BrandIcon, Modal, Switch} from '../components/ui';
import type {MonitorProbeAccount, MonitorProbeConfig, MonitorProbeRun, MonitorProbeRunResult} from './types';
import {normalizeProbeCase, probeCaseText, probeKindText, probeSkipText} from './types';
import {displayTime, formatLatency, relativeToNow} from './format';

/** 已上线的探测种类：只有轻探测。后端对其余 kind 回 400 kind_unavailable。 */
export const PROBE_KIND_LIGHT = 'light';

export interface ProbesTabProps {
  probeAccounts: MonitorProbeAccount[];
  probeRuns: MonitorProbeRun[];
  /** 探测记录按账号筛选（深链 ?account= 与记录里的账号按钮都走这里）；空 = 全部。 */
  providerFilter: string;
  timezone: string;
  busy: string;
  error: string;
  /** 品牌图标键由外层提供（见 AccountsTab 同名说明）。 */
  iconOf?: (providerId: string) => string | undefined;
  /** 结果由外层按 providerId 存，轮询换新数据时弹层不会被刷掉。 */
  probeResult: { providerId: string; data: MonitorProbeRunResult } | null;
  onDismissResult: () => void;
  onRunProbe: (providerId: string) => Promise<MonitorProbeRunResult | null>;
  /** 探测结束后再取一次记录列表（探测行可能比第一次刷新晚落库）。 */
  onRefreshRuns: () => void;
  onFilter: (providerId: string) => void;
  onOpenAccount: (providerId: string) => void;
}

type ProbeTone = 'ok' | 'err' | 'skip' | 'none';

/** 行状态只判「跑成什么」：跳过与从未探测都不是失败。 */
function outcomeOf(account: MonitorProbeAccount): { tone: ProbeTone; status: string; skip: string } {
  if (!account.probed) {
    const skip = probeSkipText(account.skipped);
    return skip ? { tone: 'skip', status: '未成功执行', skip } : { tone: 'none', status: '暂无数据', skip: '' };
  }
  if (account.consecutiveFailures > 0) {
    const auth = account.lastStatus === 401 || account.lastStatus === 403;
    return { tone: 'err', status: `连续失败 ${account.consecutiveFailures} 次`, skip: auth ? '最近一次认证失败' : '' };
  }
  return { tone: 'ok', status: '通过', skip: '' };
}

function statusCodeText(status: number | undefined): string {
  if (status === undefined || status === 0) return '—';
  return String(status);
}

/** 状态码文案：「最近状态」是服务端记录里最新那一发的状态，和连续失败次数不是一回事。 */
function lastStatusText(status: number | undefined): string {
  if (status === undefined || status === 0) return '最近状态 —';
  return `最近状态 ${status}`;
}

/** 当日次数：maxPerDay=0 是「不限」，不能显示成「0 次」。 */
function dailyText(account: MonitorProbeAccount): string {
  return account.maxPerDay === 0 ? `${account.usedToday} / 不限` : `${account.usedToday} / ${account.maxPerDay}`;
}

export function ProbesTab(props: ProbesTabProps) {
  const {
    probeAccounts, probeRuns, providerFilter, timezone, busy, error, iconOf,
    probeResult, onDismissResult, onRunProbe, onFilter, onOpenAccount, onRefreshRuns,
  } = props;
  const now = Date.now();
  const nameOf = (providerId: string) =>
    probeAccounts.find((item) => item.providerId === providerId)?.name || providerId;

  const [runError, setRunError] = useState('');

  const run = (providerId: string) => {
    setRunError('');
    void onRunProbe(providerId).then((data) => {
      // onRunProbe 内部已经刷新过，但刷新请求可能在探测行落库之前返回；这里再把已落库的记录补进来，
      // 否则弹层打开期间「最近探测记录」会少掉刚跑的两行。
      if (data) void onRefreshRuns();
      else setRunError(`${nameOf(providerId)}：探测请求失败，请稍后重试`);
    });
  };

  const filteredRuns = providerFilter ? probeRuns.filter((row) => row.providerId === providerFilter) : probeRuns;
  // 账号排序：失败（次数多的在前）→ 有跳过说明 → 未探过 → 通过；同档按名字，刷新不跳位。
  const rank = (item: MonitorProbeAccount) =>
    item.probed && item.consecutiveFailures > 0 ? 0 : item.skipped ? 1 : !item.probed ? 2 : 3;
  const ordered = [...probeAccounts].sort((a, b) => {
    const delta = rank(a) - rank(b);
    if (delta !== 0) return delta;
    if (rank(a) === 0) return b.consecutiveFailures - a.consecutiveFailures;
    return a.name.localeCompare(b.name, 'zh-CN');
  });
  // 调用方（MonitoringPage）已过滤掉「账号已不在账号列表里」的结果，这里不再二次判断（审查 O4）。
  const shownResult = probeResult;

  return (
    <div className="card panel">
      <div className="panel-header">
        <div>
          <h2 className="panel-title">{`探测（${probeAccounts.length} 个账号）`}</h2>
          <p className="panel-desc">
            {'轻探测是最小对话（max_tokens=16），不写请求日志，不进账号成功率与成本。探测失败只进入 probe_failed 规则，不会自动处置账号。'}
          </p>
        </div>
      </div>

      {error && <div className="metric-value err">{error}</div>}
      {runError && <div className="metric-value err">{runError}</div>}

      <div className="panel-title">{'账号'}</div>
      {ordered.length === 0 && <div className="muted-text">{'还没有账号。接入 Provider 后这里会出现探测矩阵。'}</div>}
      {ordered.map((account) => {
        const outcome = outcomeOf(account);
        const probing = busy === `probe:${account.providerId}`;
        const lastStamp = displayTime(account.lastProbeAt, timezone);
        return (
          <div className="log-row" key={account.providerId}>
            <div className="mrow mrow-probe">
              <span className="m-clip" style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                <span className={`dot ${account.probed ? (outcome.tone === 'err' ? 'fail' : 'ok') : 'idle'}`} aria-hidden />
                <BrandIcon icon={iconOf?.(account.providerId)} name={account.name} size={18} />
                <span className="m-clip" style={{ fontWeight: 600 }} title={account.name}>
                  {account.name}
                </span>
              </span>
              <span className="m-clip" title={outcome.skip ? `${outcome.status}：${outcome.skip}` : outcome.status}>
                <span className={outcome.tone === 'err' ? 'err' : outcome.tone === 'ok' ? '' : 'muted-text'}>
                  {outcome.status}
                </span>
                {outcome.skip && <span className="muted-text">{` · ${outcome.skip}`}</span>}
              </span>
              <span className="m-clip" title={account.probed ? `最近一次成功执行：${lastStamp.title}` : '还没有可判定的探测'}>
                {account.probed ? `${lastStamp.text}（${relativeToNow(account.lastProbeAt, now)}）` : '—'}
              </span>
              <span className="muted-text" title={account.maxPerDay === 0 ? '当日次数不限' : '当日已探测次数 / 每日上限'}>
                {`今日 ${dailyText(account)}`}
              </span>
              <span className="m-row-num" title="最近一发探测的上游 HTTP 状态；0 表示传输层失败">
                {lastStatusText(account.lastStatus)}
              </span>
              <span className="probe-actions">
                <button
                  className="btn btn-sm"
                  disabled={probing}
                  onClick={() => run(account.providerId)}
                  title={account.disabled ? '账号已停用；探测会记为「账号已停用，未探测」' : '现在就发一发最小对话探测'}
                >
                  {probing ? '探测中…' : '立即探测'}
                </button>
                <button className="btn btn-sm" onClick={() => onOpenAccount(account.providerId)}>{'详情'}</button>
              </span>
            </div>
          </div>
        );
      })}

      <div className="card" style={{ marginTop: 12 }}>
        <div className="panel-header">
          <div className="panel-title">{`最近探测记录（${filteredRuns.length}）`}</div>
          {providerFilter && (
            <div className="panel-header-actions">
              <button className="btn btn-sm" onClick={() => onFilter('')}>{`查看全部账号（当前：${nameOf(providerFilter)}）`}</button>
            </div>
          )}
        </div>
        {filteredRuns.length === 0 && (
          <div className="muted-text">
            {providerFilter ? `${nameOf(providerFilter)} 还没有探测记录。` : '还没有探测记录，可以点「立即探测」发一发。'}
          </div>
        )}
        {filteredRuns.map((row, index) => {
          const skipped = probeSkipText(row.skipped);
          const stamp = displayTime(row.time, timezone);
          return (
            <div className="log-row" key={`${row.time}-${row.case}-${index}`}>
              <div className="mrow mrow-probe-run">
                <span className="muted-text m-clip" title={stamp.title}>{stamp.text}</span>
                <span>
                  {!providerFilter && (
                    <button
                      className="status-pill-btn m-row-link"
                      onClick={() => onFilter(row.providerId)}
                      title={row.providerId}
                    >
                      {nameOf(row.providerId)}
                    </button>
                  )}
                </span>
                <span className="muted-text">{probeKindText(row.kind)}</span>
                <span className="m-clip">{probeCaseText(row.case)}</span>
                <span className="m-row-num">{statusCodeText(row.status)}</span>
                <span className={`m-clip ${skipped ? 'muted-text' : row.ok ? '' : 'err'}`}>
                  {skipped || (row.ok ? '通过' : row.status === 0 && row.errorCode === 'network' ? '失败 · 网络不可达' : '失败')}
                </span>
                <span className="muted-text">{formatLatency(row.durationMs)}</span>
                <span className="muted-text m-clip" title={row.model || ''}>{row.model || '—'}</span>
                <span className="muted-text m-clip" title={row.errorCode || ''}>{row.errorCode || '—'}</span>
              </div>
            </div>
          );
        })}
      </div>

      {shownResult && (
        <Modal
          title={`探测结果：${nameOf(shownResult.providerId)}`}
          description="手动探测只读取结果，不会改动账号配置或调度状态"
          onClose={onDismissResult}
        >
          <ProbeResultBody result={shownResult.data} />
        </Modal>
      )}
    </div>
  );
}

export interface ProbeResultBodyProps {
  result: MonitorProbeRunResult;
}

/**
 * 一次手动探测的结果。抽屉与探测页共用：结果永远就地显示在触发它的地方，
 * 不靠顶部提示条（抽屉打开时顶部会被 Modal 挡住）。
 */
export function ProbeResultBody({ result }: ProbeResultBodyProps) {
  if (result.skipped === 'in_flight') {
    return <div className="metric-note">{'已有探测在进行中：同一账号同时只跑一发，等它结束后再试。'}</div>;
  }
  if (result.errorKind) {
    return <div className="metric-note err">{`探测未能执行：${result.errorKind}`}</div>;
  }
  const skipped = probeSkipText(result.skipped);
  if (skipped) return <div className="metric-note">{skipped}</div>;
  const cases = result.cases ?? [];
  if (cases.length === 0) return <div className="metric-note">{'探测已提交，但没有返回用例结果。'}</div>;
  const headline = result.ok
    ? '轻探测通过'
    : '轻探测失败：只进入 probe_failed 规则，不会自动处置账号。';
  return (
    <div>
      <div className="metric-note">{headline}</div>
      {cases.map(normalizeProbeCase).map((item, index) => (
        <div className="log-row" key={`${item.key}-${index}`}>
          <div className="mrow mrow-probe-case">
            <span className={`badge ${item.ok ? 'green' : 'red'}`}>{item.ok ? '通过' : '失败'}</span>
            <span className="m-clip">{probeCaseText(item.key)}</span>
            <span className="m-row-num">{`状态 ${statusCodeText(item.status)}`}</span>
            <span className="muted-text">{formatLatency(item.durationMs)}</span>
            <span className="muted-text m-clip" title={item.error}>{item.model || item.error || '—'}</span>
          </div>
          {item.error && !item.model && <div className="metric-note err m-clip">{item.error}</div>}
        </div>
      ))}
    </div>
  );
}

function minutesText(value: number): string {
  if (value <= 0) return '不限制';
  if (value % 1440 === 0) return `${value / 1440} 天`;
  if (value % 60 === 0) return `${value / 60} 小时`;
  return `${value} 分钟`;
}

export interface ProbeSettingsPanelProps {
  config: MonitorProbeConfig | null;
  timezone: string;
  draft: MonitorProbeConfig | null;
  onChange: (next: MonitorProbeConfig) => void;
  onSubmit: () => void;
  busy: boolean;
  error: string;
  notice: string;
}

/**
 * 探测配置的唯一入口（设置页）。只暴露决定「会花多少额度」的参数，外加额度读数
 * 过期判定（账号页的「数据过期」标签读的就是它）。表单没覆盖的字段一律原样带回，
 * 避免保存时被零值覆盖。
 */
export function ProbeSettingsPanel(props: ProbeSettingsPanelProps) {
  const { config, timezone, draft, onChange, onSubmit, busy, error, notice } = props;
  // 配置先到、或表单草稿先到都会渲染：后端配置只用来显示「当前生效」，
  // 表单值以草稿为准；草稿还没到时就先显示后端值，不让整张卡片闪空。
  const base = draft ?? config;
  if (!base) return null;
  const set = (patch: Partial<MonitorProbeConfig>) => onChange({ ...base, ...patch });
  const setEvent = (patch: Partial<MonitorProbeConfig['eventDriven']>) =>
    set({ eventDriven: { ...base.eventDriven, ...patch } });
  const setDaily = (patch: Partial<MonitorProbeConfig['daily']>) => set({ daily: { ...base.daily, ...patch } });
  // type=number 在输入非法时 value 是 ''，直接 Number('') 会变成 0（悄悄改成「不限」）。
  const numberValue = (raw: string, fallback: number) => {
    if (raw.trim() === '') return fallback;
    const value = Number(raw);
    return Number.isFinite(value) ? Math.trunc(value) : fallback;
  };

  return (
    <div className="card">
      <div className="panel-title">{'探测调度'}</div>
      <p className="panel-desc">
        {`只对已启用、24 小时内有过真实请求的账号探测；当日次数与每日窗口都按投递时区（${timezone}）计算。`}
      </p>
      <div className="monitoring-cols">
        <div className="field">
          <Switch
            checked={base.eventDriven.enabled}
            label="事件驱动轻探测"
            onChange={(value) => setEvent({ enabled: value })}
          />
        </div>
        <label className="field">
          <span>{'每账号每日上限（0 = 不限）'}</span>
          <input
            type="number"
            min={0}
            max={24}
            value={base.eventDriven.maxPerDayPerAccount}
            onChange={(event) => setEvent({ maxPerDayPerAccount: numberValue(event.target.value, base.eventDriven.maxPerDayPerAccount) })}
          />
        </label>
        <label className="field">
          <span>{'两次探测最小间隔（分钟）'}</span>
          <input
            type="number"
            min={0}
            max={1440}
            value={base.eventDriven.minIntervalMinutes}
            onChange={(event) => setEvent({ minIntervalMinutes: numberValue(event.target.value, base.eventDriven.minIntervalMinutes) })}
          />
        </label>
        <label className="field">
          <span>{'账号闲置多久后才补探（分钟）'}</span>
          <input
            type="number"
            min={0}
            max={1440}
            value={base.eventDriven.idleMinutes}
            onChange={(event) => setEvent({ idleMinutes: numberValue(event.target.value, base.eventDriven.idleMinutes) })}
          />
        </label>
        <label className="field">
          <span>{'每日窗口开始（HH:MM）'}</span>
          <input value={base.daily.windowStart} placeholder="04:00" onChange={(event) => setDaily({ windowStart: event.target.value })} />
        </label>
        <label className="field">
          <span>{'每日窗口结束（HH:MM）'}</span>
          <input value={base.daily.windowEnd} placeholder="06:00" onChange={(event) => setDaily({ windowEnd: event.target.value })} />
        </label>
        <label className="field">
          <span>{'额度读数过期判定（分钟，0 = 不标记）'}</span>
          <input
            type="number"
            min={0}
            value={base.quotaStaleMinutes}
            onChange={(event) => set({ quotaStaleMinutes: Math.max(0, numberValue(event.target.value, base.quotaStaleMinutes)) })}
          />
        </label>
      </div>
      <div className="metric-note">
        {`当前生效：每账号每日 ${base.eventDriven.maxPerDayPerAccount === 0 ? '不限' : `${base.eventDriven.maxPerDayPerAccount} 次`} · 最小间隔 ${minutesText(base.eventDriven.minIntervalMinutes)} · 闲置阈值 ${minutesText(base.eventDriven.idleMinutes)} · 每日窗口 ${base.daily.enabled ? `${base.daily.windowStart}–${base.daily.windowEnd}` : '已关闭'}（${timezone}）`}
      </div>
      <div className="metric-note">
        {base.quotaStaleMinutes > 0
          ? `额度读数超过 ${base.quotaStaleMinutes} 分钟未更新时，账号页与详情里标为「数据过期」。`
          : '额度过期判定已关闭：账号页与详情不标「数据过期」。'}
      </div>
      <div className="panel-actions">
        <button className="btn btn-sm primary" disabled={busy} onClick={onSubmit}>
          {'保存探测配置'}
        </button>
        {error && <span className="metric-note err">{error}</span>}
        {notice && !error && <span className="metric-note">{notice}</span>}
      </div>
    </div>
  );
}
