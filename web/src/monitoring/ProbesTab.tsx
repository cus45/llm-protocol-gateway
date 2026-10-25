import {useEffect, useRef, useState} from 'react';
import {BrandIcon, Modal, Switch} from '../components/ui';
import type {MonitorProbeAccount, MonitorProbeConfig, MonitorProbeRun, MonitorProbeRunResult} from './types';
import {ERROR_KIND_TEXT, normalizeProbeCase, probeCaseText, probeKindText, probeSkipText} from './types';
import {displayTime, displayTimeCompact, formatLatency, relativeToNow} from './format';

/** 已上线的探测种类：只有轻探测。后端对其余 kind 回 400 kind_unavailable。 */
export const PROBE_KIND_LIGHT = 'light';

/** 与 MonitoringPage 的 15 秒轮询同节奏；自动刷新关闭后记录行如实标出「数据停在哪个时刻」。 */
const AUTO_REFRESH_MS = 15_000;

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

// ── 探测记录（时间轴）────────────────────────────────────────────────────

/**
 * 一个分类胶囊。计数口径与记录行的三种结果一一对应：
 * 跳过（skipped 非空，如 quota_spent / no_model）既不算成功也不算失败。
 */
type RunCategory = 'all' | 'ok' | 'fail' | 'skip';

/** 普通字段里的空值不显示成 `undefined` / `0`；状态 0 是传输层失败，单独写 network。 */
function runField(value: string | number | undefined): string {
  if (value === undefined || value === '') return '—';
  return String(value);
}

/**
 * 错误码的核心原因。后端把 Go 的传输层错误整段存进来（`Post "https://…": context canceled`），
 * 整条 URL 塞进行内没有信息量：只取最后一个冒号之后的原因，完整原文留给悬停。
 */
function errorReason(errorCode: string): string {
  // 已知错误类别（network / auth_failed / rate_limited）本身就是结论，不是原始报文：
  // 直接映射成中文，别把 "network" 原样丢给用户。
  const known = (ERROR_KIND_TEXT as Record<string, string>)[errorCode];
  if (known) return known;
  const at = errorCode.lastIndexOf(': ');
  const tail = at >= 0 ? errorCode.slice(at + 2).trim() : errorCode;
  return tail || errorCode;
}

/** 状态码：0 是传输层失败（请求没走到上游），读成「无响应」比「0」有信息量。 */
function runStatusText(status: number): string {
  return status === 0 ? '无响应' : String(status);
}

/**
 * 一行记录等宽 payload 的原文分段，窄处按 · 逐段折行，不整块溢出。
 * 状态码在 payload 里保留原始数字（`status=0`），「无响应」的释义只给上面的指标格。
 */
function payloadSegments(row: MonitorProbeRun): string[] {
  const duration = row.durationMs > 0 ? `${row.durationMs}ms` : '—';
  return [
    `case=${runField(row.case)}`,
    `ok=${row.ok}`,
    `status=${runField(row.status)}`,
    duration,
    row.model ? `model=${row.model}` : '',
    row.errorCode ? `error=${errorReason(row.errorCode)}` : '',
    row.skipped ? `skipped=${row.skipped}` : '',
  ].filter(Boolean);
}

/** 四列指标格：值都来自 run 本身，空值写「—」，不补猜。 */
function runMetrics(row: MonitorProbeRun): Array<{ label: string; value: string; tone?: 'err' | 'muted' }> {
  const unmapped = isUnmappedKind(row.kind);
  return [
    {
      label: '状态码',
      value: unmapped ? '—' : runStatusText(row.status),
      tone: !unmapped && row.status === 0 ? 'err' : undefined,
    },
    { label: '耗时', value: row.durationMs > 0 ? formatLatency(row.durationMs) : '—' },
    { label: '模型', value: row.model || '—', tone: row.model ? undefined : 'muted' },
    {
      label: '错误码',
      value: row.errorCode ? errorReason(row.errorCode) : '—',
      tone: row.errorCode ? 'err' : 'muted',
    },
  ];
}

/** 已下线的深检（CLI）沿旧字段名取值：库里老行的 status/error_code 不是这层语义，不硬套成「无响应」。 */
function isUnmappedKind(kind: string): boolean {
  return kind !== PROBE_KIND_LIGHT && kind !== '';
}

/** kind 徽标的底色分档：轻探测沿用主色，历史深检用紫色，和事件流里按类型分色是一个意思。 */
function kindTone(kind: string): string {
  return kind === PROBE_KIND_LIGHT ? 'light' : 'legacy';
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
  const [category, setCategory] = useState<RunCategory>('all');
  const [search, setSearch] = useState('');
  const [autoRefresh, setAutoRefresh] = useState(true);
  // 上一轮记录与取到它的时刻：自动刷新关掉后，头部必须说清楚「数据停在哪」。
  const lastSeenRef = useRef('');
  // 打开页面时不一定跑过轮询：首帧就把「上次读取」标出来，不留悬空状态。
  const [fetchedAt, setFetchedAt] = useState(Date.now());

  useEffect(() => {
    const newest = probeRuns.reduce((best, row) => (row.time > best ? row.time : best), '');
    if (newest !== lastSeenRef.current) {
      lastSeenRef.current = newest;
    }
    setFetchedAt(Date.now());
  }, [probeRuns]);

  useEffect(() => {
    if (!autoRefresh || document.hidden) return;
    const timer = window.setInterval(() => onRefreshRuns(), AUTO_REFRESH_MS);
    const onVisible = () => {
      if (!document.hidden) onRefreshRuns();
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [autoRefresh, onRefreshRuns]);

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

  // 分类计数按「筛选账号之后」的记录算：胶囊上的 N 必须等于点进去看得到的行数。
  const countOf = (cat: RunCategory) => {
    if (cat === 'all') return filteredRuns.length;
    return filteredRuns.filter((row) => {
      if (cat === 'skip') return !!row.skipped;
      if (cat === 'ok') return !row.skipped && row.ok;
      return !row.skipped && !row.ok;
    }).length;
  };
  const CATEGORIES: Array<{ id: RunCategory; label: string; tone: string }> = [
    { id: 'all', label: '全部', tone: 'all' },
    { id: 'ok', label: '成功', tone: 'ok' },
    { id: 'fail', label: '失败', tone: 'err' },
    { id: 'skip', label: '跳过', tone: 'skip' },
  ];
  const keyword = search.trim().toLowerCase();
  /** 搜索同时匹配账号名与错误码（API 里的 errorCode 原样参与匹配，显示也原样）。 */
  const matchesSearch = (row: MonitorProbeRun) => {
    if (!keyword) return true;
    const name = nameOf(row.providerId).toLowerCase();
    return name.includes(keyword) || (row.errorCode ?? '').toLowerCase().includes(keyword);
  };
  const shownRuns = filteredRuns.filter((row) => {
    if (category === 'skip') return !!row.skipped;
    if (category === 'ok') return !row.skipped && row.ok;
    if (category === 'fail') return !row.skipped && !row.ok;
    return true;
  }).filter(matchesSearch);

  const newestStamp = [...probeRuns].reduce((best, row) => (row.time > best ? row.time : best), '');
  const lastRunStamp = newestStamp ? displayTime(newestStamp, timezone) : null;
  const autoRefreshHidden = !autoRefresh || document.hidden;

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

      {/* ① 控制条：轮询而非流式，所以读数是「刷新状态 + 手动刷新 + 自动刷新」，不是暂停/倍速。 */}
      <div className="mon-probe-bar">
        <div className="mon-probe-bar-main">
          <div className={`mon-probe-live ${autoRefreshHidden ? 'paused' : ''}`}>
            {autoRefreshHidden ? <span className="mon-probe-ind" aria-hidden /> : <i className="live-dot" aria-hidden />}
            <span className="mon-probe-live-text">{autoRefresh ? '轮询中' : '已暂停轮询'}</span>
            <span className="mon-probe-rate">{autoRefresh ? `${AUTO_REFRESH_MS / 1000}s/次` : '手动'}</span>
          </div>
          <span className="mon-probe-meta" title="浏览器最近一次取回探测数据的时刻">
            {`上次读取 ${displayTime(new Date(fetchedAt).toISOString(), timezone).text}`}
          </span>
          <span className="mon-probe-meta" title={lastRunStamp?.title ?? '还没有探测记录'}>
            {lastRunStamp ? `最新记录 ${lastRunStamp.text}` : '最新记录 —'}
          </span>
        </div>
        <div className="mon-probe-bar-actions">
          <Switch checked={autoRefresh} label="自动刷新" onChange={setAutoRefresh} />
          <button className="btn btn-sm" onClick={() => onRefreshRuns()}>{'手动刷新'}</button>
        </div>
      </div>

      {/* ② 分类胶囊：计数来自本轮 /__monitor/probes 的 runs（已按账号筛选）。 */}
      <div className="mon-chips" role="group" aria-label="按结果筛选">
        {CATEGORIES.map((item) => {
          const active = category === item.id;
          return (
            <button
              key={item.id}
              type="button"
              className={`mon-chip mon-chip-${item.tone}${active ? ' on' : ''}`}
              aria-pressed={active}
              onClick={() => setCategory(item.id)}
            >
              <span className="mon-chip-label">{item.label}</span>
              <span className="mon-chip-count">{countOf(item.id)}</span>
            </button>
          );
        })}
      </div>

      {/* ③ 搜索：账号名 / 错误码。 */}
      <div className="mon-probe-search">
        <input
          className="monitoring-input"
          type="search"
          placeholder="搜索账号名 / 错误码"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          aria-label="按账号名或错误码搜索探测记录"
        />
        {providerFilter && (
          <button className="btn btn-sm" onClick={() => onFilter('')}>{'查看全部账号'}</button>
        )}
      </div>

      {/* ④ 时间轴：竖线 + 按结果着色的圆点，每条下方一行等宽 payload。 */}
      <div className="mon-timeline">
        {shownRuns.length === 0 && (
          <div className="m-empty">
            {filteredRuns.length === 0
              ? providerFilter ? `${nameOf(providerFilter)} 还没有探测记录。` : '还没有探测记录，可以点「立即探测」发一发。'
              : '没有符合筛选条件的记录。'}
          </div>
        )}
        {shownRuns.map((row, index) => {
          const skipped = probeSkipText(row.skipped);
          // 跳过原因是给人看的结论：有中文释义就只显示释义，原始 key 留在 payload 的 skipped= 里。
          const skipReason = !row.skipped ? ''
            : /^已跳过（/.test(skipped) ? `本次探测跳过，原因代码 ${row.skipped}` : skipped;
          const tone = skipped ? 'skip' : row.ok ? 'ok' : 'err';
          const stamp = displayTime(row.time, timezone);
          // 徽标只说「结果」，原因永远不挤进徽标（否则看不出哪段是状态、哪段是原因）。
          const verdict = skipped ? '跳过' : row.ok ? '成功' : '失败';
          const reason = !skipped && !row.ok
            ? row.errorCode ? errorReason(row.errorCode) : row.status === 0 ? '网络不可达' : ''
            : '';
          return (
            <div className={`mon-tl-item ${tone}`} key={`${row.time}-${row.case}-${index}`}>
              <div className="mon-tl-head">
                {/* 时区在页头写着，行内只留日期时间；完整值与本地时间在悬停里。 */}
                <span className="mon-tl-time" title={stamp.title}>{displayTimeCompact(stamp.text)}</span>
                <span className={`mon-tl-verdict ${tone}`}>{verdict}</span>
                <span
                  className={`mon-tl-kind ${kindTone(row.kind)}`}
                  title={row.kind === PROBE_KIND_LIGHT ? '轻探测：最小对话，不写请求日志' : `历史记录（kind=${row.kind}）：深检已下线，只作存档`}
                >
                  {probeKindText(row.kind)}
                </span>
                <span className="mon-tl-title">{probeCaseText(row.case)}</span>
                <button
                  type="button"
                  className="mon-tl-account"
                  onClick={() => onFilter(row.providerId)}
                  title={providerFilter ? `只看 ${nameOf(row.providerId)} 的记录（已筛选）` : `只看 ${nameOf(row.providerId)} 的记录`}
                >
                  <BrandIcon icon={iconOf?.(row.providerId)} name={nameOf(row.providerId)} size={14} />
                  <span className="mon-tl-account-name">{nameOf(row.providerId)}</span>
                </button>
              </div>
              {skipped && <div className="mon-tl-skip">{skipReason}</div>}
              {/* 四列指标格：宽处一行，窄处 2×2；值整段换行，不省略。 */}
              <div className="mon-tl-metrics">
                {runMetrics(row).map((metric) => (
                  <div className="mon-tl-metric" key={metric.label}>
                    <span className="mon-tl-metric-label">{metric.label}</span>
                    <span className={`mon-tl-metric-value${metric.tone ? ` ${metric.tone}` : ''}`}>{metric.value}</span>
                  </div>
                ))}
              </div>
              {reason && <div className="mon-tl-reason" title={row.errorCode}>{reason}</div>}
              <div className="mon-tl-payload">
                <span className="mon-tl-prompt" aria-hidden>&gt;</span>
                <span className="mon-tl-segs">
                  {payloadSegments(row).map((segment) => (
                    <span
                      className={`mon-tl-seg${segment.startsWith('error=') ? ' err' : ''}`}
                      key={segment}
                      title={segment.startsWith('error=') ? row.errorCode : undefined}
                    >
                      {segment}
                    </span>
                  ))}
                </span>
              </div>
            </div>
          );
        })}
      </div>

      <div className="mon-section-title">{'账号探测现状'}</div>
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
              {/* 上次时间同样有长短两档：窄处去掉时区后缀，整句（含相对时间）不被省略号截断。 */}
              <span className="mon-pa-last" title={account.probed ? `最近一次成功执行：${lastStamp.title}` : '还没有可判定的探测'}>
                {account.probed ? (
                  <>
                    <span className="mon-pa-last-time">
                      {displayTimeCompact(lastStamp.text)}
                    </span>
                    {`（${relativeToNow(account.lastProbeAt, now)}）`}
                  </>
                ) : '—'}
              </span>
              <span className="mon-pa-daily" title={account.maxPerDay === 0 ? '当日次数不限' : '当日已探测次数 / 每日上限'}>
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

export function minutesText(value: number): string {
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
    <div className="card mon-set-card">
      <header className="mon-set-head">
        <div className="mon-set-head-text">
          <h2 className="mon-card-title">{'探测调度'}</h2>
          <p className="mon-set-desc">
            {`只对已启用、24 小时内有过真实请求的账号探测；当日次数与每日窗口都按投递时区（${timezone}）计算。`}
          </p>
        </div>
      </header>
      <div className="monitoring-cols">
        <div className="field mon-set-switch">
          <span>{'轻探测开关'}</span>
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
      <p className="mon-set-foot">
        {`当前生效：每账号每日 ${base.eventDriven.maxPerDayPerAccount === 0 ? '不限' : `${base.eventDriven.maxPerDayPerAccount} 次`} · 最小间隔 ${minutesText(base.eventDriven.minIntervalMinutes)} · 闲置阈值 ${minutesText(base.eventDriven.idleMinutes)} · 每日窗口 ${base.daily.enabled ? `${base.daily.windowStart}–${base.daily.windowEnd}` : '已关闭'}（${timezone}）`}
      </p>
      <p className="mon-set-foot">
        {base.quotaStaleMinutes > 0
          ? `额度读数超过 ${base.quotaStaleMinutes} 分钟未更新时，账号页与详情里标为「数据过期」。`
          : '额度过期判定已关闭：账号页与详情不标「数据过期」。'}
      </p>
      <div className="mon-set-form-actions">
        <button className="btn btn-sm primary" disabled={busy} onClick={onSubmit}>
          {'保存探测配置'}
        </button>
        {error && <span className="mon-set-inline err" role="alert">{error}</span>}
        {notice && !error && <span className="mon-set-inline">{notice}</span>}
      </div>
    </div>
  );
}
