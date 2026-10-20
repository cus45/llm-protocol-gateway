import {useMemo, useState} from 'react';
import type {DailyRequestPoint, Provider, RequestStatsSnapshot, RTKStatsResponse} from '../types';
import {formatBytes, formatLocalISODate, formatTokenCount, normalizePromptTokenStats, providerUsageLabel} from '../lib';
import {downloadCSV} from './charts';
import './usage.css';

type Dimension = 'models' | 'providers' | 'keys' | 'protocols' | 'users';
type Metric = 'tokens' | 'requests';
type UsageRow = { id: string; name: string; requestCount: number; inputTokens: number; outputTokens: number; cacheTokens: number };
type LogTarget = { type: 'model' | 'provider' | 'apiKey'; value: string; label: string };

function csvCell(value: string | number) {
  const text = String(value);
  return `"${(/^[=+@\-\t\r]/.test(text) ? "'" : '') + text.replace(/"/g, '""')}"`;
}

function exportUsage(stats: RequestStatsSnapshot, from: string, to: string) {
  const period = stats.range;
  const lines: (string | number)[][] = [
    ['统计区间', from, to],
    ['日期', '请求数', '输入 Token（含缓存）', '输出 Token', '缓存读取 Token', '接收字节', '发送字节'],
    ...(stats.daily || []).map(d => [d.date, d.requestCount, d.inputTokens, d.outputTokens, d.cacheTokens, d.rxBytes || 0, d.txBytes || 0]),
  ];
  for (const [name, rows] of [
    ['模型', (period?.byModel || []).map(r => ({ ...r, name: r.model }))],
    ['Provider', (period?.byProvider || []).map(r => ({ ...r, name: r.providerId }))],
    ['API 密钥', (period?.byApiKey || []).map(r => ({ ...r, name: r.apiKeyName || r.apiKeyId }))],
    ['输出协议', (period?.byProtocol || []).map(r => ({ ...r, name: r.protocol }))],
    ['用户', (period?.byUser || []).map(r => ({ ...r, name: r.userName || r.userId }))],
  ] as const) {
    lines.push([], [name, '请求数', '输入 Token（含缓存）', '输出 Token', '缓存读取 Token']);
    for (const r of rows) lines.push([r.name, r.requestCount, r.inputTokens, r.outputTokens, r.cacheTokens]);
  }
  downloadCSV(`gateway-usage-${from}-to-${to}.csv`, lines.map(row => row.map(csvCell).join(',')).join('\r\n'));
}

function Stat({ label, value, note }: { label: string; value: string; note: string }) {
  return <div className="usage-stat"><span>{label}</span><strong>{value}</strong><small>{note}</small></div>;
}

function DailyTrend({ points, metric }: { points: DailyRequestPoint[]; metric: Metric }) {
  const [focused, setFocused] = useState<number | null>(null);
  const [table, setTable] = useState(false);
  const values = points.map(d => metric === 'requests' ? d.requestCount : d.inputTokens + d.outputTokens);
  const max = Math.max(2, ...values);
  const active = focused == null ? undefined : points[focused];
  const width = 900, height = 190, left = 58, right = 14, top = 16, bottom = 30;
  const plotWidth = width - left - right, plotHeight = height - top - bottom;
  const step = plotWidth / Math.max(points.length, 1);
  const valueLabel = metric === 'requests' ? '请求数' : 'Token';
  return <section className="usage-block">
    <div className="usage-block-head"><h3>每日趋势</h3><button className="mini-btn" type="button" onClick={() => setTable(!table)} aria-pressed={table}>{table ? '查看图表' : '查看数据'}</button></div>
    {points.length === 0 ? <p className="usage-empty">当前区间暂无趋势数据</p> : table ? <div className="usage-table-wrap"><table className="usage-data-table"><thead><tr><th>日期</th><th>请求数</th><th>输入</th><th>输出</th><th>缓存读取</th></tr></thead><tbody>{points.map(d => <tr key={d.date}><td>{d.date}</td><td>{d.requestCount.toLocaleString()}</td><td>{d.inputTokens.toLocaleString()}</td><td>{d.outputTokens.toLocaleString()}</td><td>{d.cacheTokens.toLocaleString()}</td></tr>)}</tbody></table></div> : <>
      <div className="usage-trend-readout" aria-live="polite">{active ? <><b>{active.date}</b><span>{values[focused!].toLocaleString()} {valueLabel}</span><span>缓存读取 {formatTokenCount(active.cacheTokens)}</span></> : <span>{valueLabel} · 悬停或使用键盘查看每日数值</span>}</div>
      <div className="usage-trend-scroll"><svg className="usage-trend-svg" viewBox={`0 0 ${width} ${height}`} role="img" aria-label={`每日${valueLabel}趋势，可切换数据表查看完整数值`}>
        {[0, 0.5, 1].map(t => <g key={t}><line x1={left} x2={width - right} y1={top + plotHeight * t} y2={top + plotHeight * t} className="usage-gridline" /><text x={left - 10} y={top + plotHeight * t + 4} textAnchor="end">{formatTokenCount(Math.round(max * (1 - t)))}</text></g>)}
        {points.map((d, i) => {
          const x = left + step * (i + 0.5), barHeight = plotHeight * values[i] / max;
          return <g key={d.date} tabIndex={0} role="button" aria-label={`${d.date}，${values[i].toLocaleString()} ${valueLabel}`} onMouseEnter={() => setFocused(i)} onMouseLeave={() => setFocused(null)} onFocus={() => setFocused(i)} onBlur={() => setFocused(null)}>
            <title>{`${d.date}：${values[i].toLocaleString()} ${valueLabel}`}</title>
            <rect x={left + step * i} y={top} width={step} height={plotHeight} fill="transparent" />
            <rect className={`usage-trend-bar${focused === i ? ' is-active' : ''}`} x={x - Math.min(20, step * 0.55) / 2} y={top + plotHeight - barHeight} width={Math.min(20, step * 0.55)} height={barHeight} rx={3} />
            {(i === 0 || i === points.length - 1 || (points.length > 4 && i % Math.ceil(points.length / 6) === 0)) && <text x={x} y={height - 7} textAnchor="middle">{d.date.slice(5)}</text>}
          </g>;
        })}
      </svg></div>
    </>}
  </section>;
}

function UsageDetails({ stats, providers, isNormalUser, metric, onNavigateLogs }: { stats: RequestStatsSnapshot | null; providers: Provider[]; isNormalUser: boolean; metric: Metric; onNavigateLogs: (target: LogTarget) => void }) {
  const [dimension, setDimension] = useState<Dimension>('models');
  const [search, setSearch] = useState('');
  const [page, setPage] = useState(0);
  const groups = useMemo(() => {
    const range = stats?.range;
    return {
      models: (range?.byModel || []).map(r => ({ ...r, id: r.model, name: r.model })),
      providers: (range?.byProvider || []).map(r => ({ ...r, id: r.providerId, name: providerUsageLabel(r.providerId, providers) })),
      keys: (range?.byApiKey || []).map(r => ({ ...r, id: r.apiKeyId, name: r.apiKeyName || r.apiKeyId || '未绑定密钥' })),
      protocols: (range?.byProtocol || []).map(r => ({ ...r, id: r.protocol, name: r.protocol })),
      users: isNormalUser ? [] : (range?.byUser || []).map(r => ({ ...r, id: r.userId, name: r.userName || r.userId || '未绑定用户' })),
    } satisfies Record<Dimension, UsageRow[]>;
  }, [stats, providers, isNormalUser]);
  const selectedDimension = isNormalUser && dimension === 'users' ? 'models' : dimension;
  const allRows = groups[selectedDimension];
  const amount = (r: UsageRow) => metric === 'requests' ? r.requestCount : r.inputTokens + r.outputTokens;
  const total = allRows.reduce((sum, r) => sum + amount(r), 0);
  const rows = [...allRows].filter(r => `${r.name} ${r.id}`.toLowerCase().includes(search.toLowerCase())).sort((a, b) => amount(b) - amount(a) || a.name.localeCompare(b.name));
  const currentPage = Math.min(page, Math.max(0, Math.ceil(rows.length / 15) - 1));
  const tabs: { id: Dimension; label: string }[] = [{ id: 'models', label: '模型' }, { id: 'providers', label: 'Provider' }, { id: 'keys', label: 'API 密钥' }, { id: 'protocols', label: '输出协议' }, ...(!isNormalUser ? [{ id: 'users' as const, label: '用户' }] : [])];
  return <section className="usage-block">
    <div className="usage-block-head"><h3>用量明细</h3><input className="usage-search" type="search" value={search} onChange={e => { setSearch(e.target.value); setPage(0); }} placeholder="搜索名称或 ID" aria-label="搜索用量明细" /></div>
    <div className="usage-detail-tabs" role="tablist" aria-label="用量分类">{tabs.map(t => <button type="button" role="tab" aria-selected={selectedDimension === t.id} className={selectedDimension === t.id ? 'active' : ''} key={t.id} onClick={() => { setDimension(t.id); setPage(0); }}>{t.label}<small>{groups[t.id].length}</small></button>)}</div>
    <div className="usage-table-wrap"><table className="usage-data-table usage-entities"><thead><tr><th>名称</th><th>请求数</th><th>输入 Token</th><th>输出 Token</th><th>缓存读取</th><th>{metric === 'requests' ? '请求' : 'Token'}占比</th><th><span className="sr-only">操作</span></th></tr></thead><tbody>
      {rows.slice(currentPage * 15, (currentPage + 1) * 15).map(r => {
        const percent = total > 0 ? amount(r) / total * 100 : 0;
        const type = selectedDimension === 'models' ? 'model' : selectedDimension === 'providers' ? 'provider' : selectedDimension === 'keys' ? 'apiKey' : null;
        return <tr key={r.id}><td><span className="usage-entity-name" title={`${r.name}\n${r.id}`}>{r.name}</span></td><td>{r.requestCount.toLocaleString()}</td><td title={r.inputTokens.toLocaleString()}>{formatTokenCount(r.inputTokens)}</td><td title={r.outputTokens.toLocaleString()}>{formatTokenCount(r.outputTokens)}</td><td title={r.cacheTokens.toLocaleString()}>{formatTokenCount(r.cacheTokens)}</td><td><div className="usage-share"><span>{percent.toFixed(1)}%</span><i><b style={{ width: `${percent}%` }} /></i></div></td><td>{type && r.id && r.id !== '_anonymous' ? <button className="mini-btn" type="button" onClick={() => onNavigateLogs({ type, value: r.id, label: r.name })}>查看日志</button> : null}</td></tr>;
      })}
      {!rows.length && <tr><td colSpan={7} className="usage-empty">{search ? '没有匹配的记录' : '当前区间暂无该分类用量'}</td></tr>}
    </tbody></table></div>
    <div className="usage-table-foot"><span>共 {rows.length} 项 · 输入包含缓存读取，缓存不重复计入总 Token</span>{rows.length > 15 && <div><button className="mini-btn" type="button" disabled={!currentPage} onClick={() => setPage(currentPage - 1)}>上一页</button><span>{currentPage + 1} / {Math.ceil(rows.length / 15)}</span><button className="mini-btn" type="button" disabled={(currentPage + 1) * 15 >= rows.length} onClick={() => setPage(currentPage + 1)}>下一页</button></div>}</div>
  </section>;
}

function CompressionUsage({ stats, days, onDaysChange, providers }: { stats: RTKStatsResponse | null; days: number; onDaysChange: (days: number) => void; providers: Provider[] }) {
  return <section className="usage-block">
    <div className="usage-block-head"><div><h3>RTK 压缩</h3><p>工具输出的文本估算，独立统计区间。</p></div><div className="usage-segments">{[{ days: 7, label: '7 天' }, { days: 30, label: '30 天' }, { days: 90, label: '90 天' }, { days: 0, label: '全部' }].map(r => <button key={r.days} type="button" aria-pressed={days === r.days} className={days === r.days ? 'active' : ''} onClick={() => onDaysChange(r.days)}>{r.label}</button>)}</div></div>
    <div className="usage-stats-strip"><Stat label="节省 Token" value={formatTokenCount(stats?.savedTokens || 0)} note={`压缩率 ${(stats?.savedPercent || 0).toFixed(1)}%`} /><Stat label="原始输出" value={formatTokenCount(stats?.originalTokens || 0)} note={`压缩后 ${formatTokenCount(stats?.compressedTokens || 0)}`} /><Stat label="压缩命令" value={(stats?.commands || 0).toLocaleString()} note={`节省 ${formatBytes(Math.max(0, (stats?.originalBytes || 0) - (stats?.compressedBytes || 0)))}`} /></div>
    {stats && !stats.available && <p className="usage-note">当前主机未检测到 RTK，已有历史记录仍会保留。</p>}
    {!stats && <p className="usage-empty">暂无压缩统计</p>}
    {!!stats?.daily.length && <details className="usage-compression-days"><summary>每日压缩明细 · {stats.daily.length} 天</summary><div className="usage-table-wrap"><table className="usage-data-table"><thead><tr><th>日期</th><th>命令</th><th>原始 Token</th><th>压缩后</th><th>节省 Token</th></tr></thead><tbody>{stats.daily.map(d => <tr key={d.date}><td>{d.date}</td><td>{d.commands.toLocaleString()}</td><td>{formatTokenCount(d.originalTokens)}</td><td>{formatTokenCount(d.compressedTokens)}</td><td>{formatTokenCount(d.savedTokens)}</td></tr>)}</tbody></table></div></details>}
    {!!stats?.byProvider?.length && <div className="usage-compression-providers">{stats.byProvider.map(p => <div key={p.providerId}><span>{providerUsageLabel(p.providerId, providers)}</span><b>{formatTokenCount(p.savedTokens)} Token</b><small>节省 {p.savedPercent.toFixed(1)}%</small></div>)}</div>}
  </section>;
}

export function UsagePage({ stats, providers, isNormalUser, from, to, loading, error, fetchedAt, onRangeChange, onRefresh, onNavigateLogs, rtkStats, rtkDays, onRTKDaysChange }: {
  stats: RequestStatsSnapshot | null; providers: Provider[]; isNormalUser: boolean; from: string; to: string; loading: boolean; error: string; fetchedAt: Date | null;
  onRangeChange: (from: string, to: string) => void; onRefresh: () => void; onNavigateLogs: (target: LogTarget) => void;
  rtkStats: RTKStatsResponse | null; rtkDays: number; onRTKDaysChange: (days: number) => void;
}) {
  const [tab, setTab] = useState<'requests' | 'compression'>('requests');
  const [metric, setMetric] = useState<Metric>('tokens');
  const range = stats?.range;
  const total = range?.total;
  const daily = stats?.daily || [];
  const requests = total?.requestCount || 0;
  const input = total?.inputTokens || 0, output = total?.outputTokens || 0;
  const cache = normalizePromptTokenStats(input, total?.cacheTokens || 0);
  const statusTotal = (stats?.status || []).reduce((n, s) => n + s.requestCount, 0);
  const successful = stats?.status?.find(s => s.class === '2xx')?.requestCount || 0;
  const measured = daily.filter(d => (d.avgLatencyMs || 0) > 0);
  const latencyCount = measured.reduce((n, d) => n + d.requestCount, 0);
  const avgLatency = latencyCount ? Math.round(measured.reduce((n, d) => n + (d.avgLatencyMs || 0) * d.requestCount, 0) / latencyCount) : null;
  const today = formatLocalISODate(new Date());
  const currentTab = isNormalUser ? 'requests' : tab;
  const matchesRange = !stats?.from || (formatLocalISODate(new Date(stats.from)) === from && formatLocalISODate(new Date(new Date(stats.to || stats.from).getTime() - 1)) === to);
  const ready = !!stats && matchesRange;
  function preset(days: number) {
    const start = new Date();
    start.setDate(start.getDate() - days + 1);
    onRangeChange(formatLocalISODate(start), today);
  }
  return <section className="section-full usage-page"><div className="card panel">
    <div className="panel-header"><div><h2 className="panel-title">用量</h2><p className="panel-desc">请求、Token 与缓存，按区间查看。</p></div><div className="usage-header-actions"><span>{loading ? '更新中…' : fetchedAt ? `更新于 ${fetchedAt.toLocaleTimeString()}` : ''}</span><button className="mini-btn" type="button" disabled={loading} onClick={onRefresh}>刷新</button>{ready && currentTab === 'requests' && <button className="mini-btn" type="button" onClick={() => exportUsage(stats!, from, to)}>导出 CSV</button>}</div></div>
    {!isNormalUser && <div className="usage-page-tabs"><button type="button" className={currentTab === 'requests' ? 'active' : ''} aria-pressed={currentTab === 'requests'} onClick={() => setTab('requests')}>请求用量</button><button type="button" className={currentTab === 'compression' ? 'active' : ''} aria-pressed={currentTab === 'compression'} onClick={() => setTab('compression')}>RTK 压缩</button></div>}
    {currentTab === 'compression' ? <CompressionUsage stats={rtkStats} days={rtkDays} onDaysChange={onRTKDaysChange} providers={providers} /> : <>
      <div className="usage-toolbar"><div className="usage-segments">{[1, 7, 30].map(days => { const start = new Date(); start.setDate(start.getDate() - days + 1); return <button key={days} type="button" className={from === formatLocalISODate(start) && to === today ? 'active' : ''} onClick={() => preset(days)}>{days === 1 ? '今天' : `${days} 天`}</button>; })}</div><div className="usage-date-inputs"><input type="date" aria-label="用量开始日期" value={from} max={to} onChange={e => { if (e.target.value) onRangeChange(e.target.value, to); }} /><span>至</span><input type="date" aria-label="用量结束日期" value={to} min={from} max={today} onChange={e => { if (e.target.value) onRangeChange(from, e.target.value); }} /></div><div className="usage-segments usage-metric-switch">{(['tokens', 'requests'] as const).map(m => <button key={m} type="button" aria-pressed={metric === m} className={metric === m ? 'active' : ''} onClick={() => setMetric(m)}>{m === 'tokens' ? 'Token' : '请求数'}</button>)}</div></div>
      {error && <p className="usage-error" role="alert">{error}</p>}
      {!ready ? <div className="usage-empty">{loading ? '正在读取所选区间…' : '所选区间暂无可用统计，请刷新重试。'}</div> : <div aria-busy={loading} className={loading ? 'usage-content is-loading' : 'usage-content'}>
        <div className="usage-stats-strip"><Stat label="总请求" value={requests.toLocaleString()} note={from === to ? from : `${from} 至 ${to}`} /><Stat label="总 Token" value={formatTokenCount(input + output)} note={`输入 ${formatTokenCount(input)} · 输出 ${formatTokenCount(output)}`} /><Stat label="缓存命中率" value={input > 0 ? `${cache.hitRatePct.toFixed(1)}%` : '—'} note={`缓存读取 ${formatTokenCount(cache.cacheHits)} Token`} /><Stat label="请求成功率" value={statusTotal > 0 ? `${(successful / statusTotal * 100).toFixed(1)}%` : '—'} note={statusTotal ? `2xx ${successful.toLocaleString()} / ${statusTotal.toLocaleString()}` : '暂无状态记录'} /></div>
        <DailyTrend points={daily} metric={metric} />
        <div className="usage-health-line"><span>平均请求耗时 <b>{avgLatency == null ? '—' : `${avgLatency.toLocaleString()} ms`}</b></span><span>接收 <b>{formatBytes(daily.reduce((n, d) => n + (d.rxBytes || 0), 0))}</b></span><span>发送 <b>{formatBytes(daily.reduce((n, d) => n + (d.txBytes || 0), 0))}</b></span>{(stats.status || []).map(s => <span key={s.class}>{s.class} <b>{s.requestCount.toLocaleString()}</b></span>)}</div>
        <UsageDetails stats={stats} providers={providers} isNormalUser={isNormalUser} metric={metric} onNavigateLogs={onNavigateLogs} />
        <p className="usage-note">统计网关记录的请求。账户订阅额度在 Provider 页面查看；日志明细受保留天数限制。</p>
      </div>}
    </>}
  </div></section>;
}
