// @generated-from main.tsx — 由重构脚本拆分生成，请直接维护本文件。
import React from 'react';
import { formatBytes, formatEnCompact, formatLocalISODate, formatTokenCount, normalizePromptTokenStats, pickUsageXTickIndexes } from '../lib';
import { DailyRequestPoint, StatusBucketStats } from '../types';
export function UsageLineChart({ title, points }: { title: string; points: DailyRequestPoint[] }) {
  const width = 360;
  const height = 168;
  const padLeft = 36;
  const padRight = 12;
  const padTop = 16;
  const padBottom = 28;
  const plotWidth = width - padLeft - padRight;
  const plotHeight = height - padTop - padBottom;
  const max = Math.max(1, ...points.map((p) => p.requestCount));
  const total = points.reduce((sum, point) => sum + point.requestCount, 0);
  const barGap = points.length <= 1 ? 0 : Math.min(10, plotWidth / (points.length * 4));
  const barWidth = points.length <= 0 ? 0 : Math.max(8, (plotWidth - barGap * Math.max(0, points.length - 1)) / points.length);

  return (
    <div className="usage-chart-card">
      <div className="usage-section-title">{title}</div>
      {points.length === 0 ? <div className="empty-state compact">暂无数据</div> : (
        <svg viewBox={`0 0 ${width} ${height}`} className="usage-chart-svg" role="img" aria-label={title}>
          <line x1={padLeft} y1={padTop} x2={padLeft} y2={height - padBottom} stroke="rgba(15,23,42,0.12)" strokeWidth="1" />
          <line x1={padLeft} y1={height - padBottom} x2={width - padRight} y2={height - padBottom} stroke="rgba(15,23,42,0.12)" strokeWidth="1" />
          {[0, 0.5, 1].map((ratio) => {
            const y = height - padBottom - ratio * plotHeight;
            const label = Math.round(max * ratio);
            return (
              <g key={`grid-${ratio}`}>
                <line x1={padLeft} y1={y} x2={width - padRight} y2={y} stroke="rgba(15,23,42,0.06)" strokeWidth="1" />
                <text x={padLeft - 6} y={y + 3} textAnchor="end" fontSize="9" fill="rgba(15,23,42,0.45)">{label}</text>
              </g>
            );
          })}
          {points.map((point, index) => {
            const x = padLeft + index * (barWidth + barGap);
            const barHeight = (point.requestCount / max) * plotHeight;
            const y = height - padBottom - barHeight;
            const dateLabel = (point.date || '').slice(5) || point.date;
            return (
              <g key={point.date}>
                <rect
                  x={x}
                  y={point.requestCount > 0 ? y : height - padBottom - 2}
                  width={barWidth}
                  height={point.requestCount > 0 ? Math.max(2, barHeight) : 2}
                  rx="3"
                  fill={point.requestCount > 0 ? 'var(--accent, #2563eb)' : 'rgba(15,23,42,0.12)'}
                >
                  <title>{`${point.date}: ${point.requestCount}`}</title>
                </rect>
                {point.requestCount > 0 ? (
                  <text x={x + barWidth / 2} y={y - 4} textAnchor="middle" fontSize="9" fill="rgba(15,23,42,0.7)">{point.requestCount}</text>
                ) : null}
                <text x={x + barWidth / 2} y={height - padBottom + 14} textAnchor="middle" fontSize="9" fill="rgba(15,23,42,0.5)">{dateLabel}</text>
              </g>
            );
          })}
        </svg>
      )}
      <div className="hint-line">
        {points.length
          ? `${points[0]?.date || ''} ~ ${points[points.length - 1]?.date || ''} · 合计 ${total}`
          : ''}
      </div>
    </div>
  );
}

export function UsageBarChart({ title, items, formatValue }: { title: string; items: Array<{ label: string; value: number }>; formatValue?: (value: number) => string }) {
  const max = Math.max(1, ...items.map((item) => item.value));
  return (
    <div className="usage-chart-card">
      <div className="usage-section-title">{title}</div>
      {items.length === 0 ? <div className="empty-state compact">暂无数据</div> : (
        <div className="usage-bar-list">
          {items.map((item) => (
            <div className="usage-bar-row" key={item.label}>
              <span className="usage-bar-label" title={item.label}>{item.label}</span>
              <div className="usage-bar-track"><div className="usage-bar-fill" style={{ width: `${(item.value / max) * 100}%` }} /></div>
              <span className="usage-bar-value">{formatValue ? formatValue(item.value) : item.value}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

export function UsageCacheHitRate({ title, input, cache }: { title: string; input: number; cache: number }) {
  const { totalInput, cacheHits, hitRatePct } = normalizePromptTokenStats(input, cache);
  const hitRateLabel = totalInput > 0 ? `${hitRatePct.toFixed(1)}%` : '—';

  return (
    <div className="usage-chart-card">
      <div className="usage-section-title">{title}</div>
      {totalInput === 0 ? (
        <div className="empty-state compact">暂无数据</div>
      ) : (
        <>
          <div className="usage-cache-hit-row">
            <div className="usage-stack-bar usage-cache-hit-bar" title={`cache hit rate ${hitRateLabel}`}>
              <div style={{ width: `${hitRatePct}%`, background: '#d97706' }} title={`cache ${cacheHits}`} />
            </div>
            <span className="usage-cache-hit-pct">{hitRateLabel}</span>
          </div>
          <div className="hint-line">in {formatTokenCount(totalInput)} · cache {formatTokenCount(cacheHits)}</div>
        </>
      )}
    </div>
  );
}

export function UsageStatusChart({ title, items }: { title: string; items: StatusBucketStats[] }) {
  const total = Math.max(1, items.reduce((sum, item) => sum + item.requestCount, 0));
  const colors: Record<string, string> = { '2xx': '#059669', '4xx': '#d97706', '5xx': '#dc2626', other: '#64748b' };
  return (
    <div className="usage-chart-card">
      <div className="usage-section-title">{title}</div>
      <div className="usage-stack-bar">
        {items.map((item) => (
          <div key={item.class} style={{ width: `${(item.requestCount / total) * 100}%`, background: colors[item.class] || '#64748b' }} title={`${item.class}: ${item.requestCount}`} />
        ))}
      </div>
      <div className="hint-line">{items.map((item) => `${item.class} ${item.requestCount}`).join(' · ') || '暂无数据'}</div>
    </div>
  );
}

export function Metric({ label, value, note }: { label: string; value: string; note: string }) {
  return (
    <div className="card metric">
      <div>
        <div className="metric-label">{label}</div>
        <div className="metric-value">{value}</div>
      </div>
      <div className="metric-note">{note}</div>
    </div>
  );
}

/** 0–100 的占用条，>=90% 判危险、>=75% 判警告。 */
export function UsageBar({ percent }: { percent?: number }) {
  const value = Math.max(0, Math.min(100, percent ?? 0));
  const tone = value >= 90 ? 'danger' : value >= 75 ? 'warn' : 'ok';
  return (
    <div className={`usage-bar ${tone}`} role="presentation">
      <span style={{ width: `${value}%` }} />
    </div>
  );
}

export function MachineMetric({ label, value, note, percent }: { label: string; value: string; note: string; percent?: number }) {
  return (
    <div className="card metric machine-metric">
      <div>
        <div className="metric-label">{label}</div>
        <div className="metric-value">{value}</div>
        {percent != null ? <UsageBar percent={percent} /> : null}
      </div>
      <div className="metric-note">{note}</div>
    </div>
  );
}

/** 用量统计日历：单击选单日，Shift+单击选区间，选完即回调。可选支持清除（不限日期）。 */
export function UsageRangeCalendar({ from, to, onSelect, onClear }: {
  from: string;
  to: string;
  onSelect: (from: string, to: string) => void;
  onClear?: () => void;
}) {
  const todayStr = formatLocalISODate(new Date());
  const [viewYM, setViewYM] = React.useState(() => (to || from || todayStr).slice(0, 7));
  const [year, month] = viewYM.split('-').map(Number);
  const startWeekday = (new Date(year, month - 1, 1).getDay() + 6) % 7; // 周一开头
  const daysInMonth = new Date(year, month, 0).getDate();

  const cells: (string | null)[] = [];
  for (let i = 0; i < startWeekday; i += 1) cells.push(null);
  for (let d = 1; d <= daysInMonth; d += 1) {
    cells.push(`${year}-${String(month).padStart(2, '0')}-${String(d).padStart(2, '0')}`);
  }

  function shiftMonth(delta: number) {
    const next = new Date(year, month - 1 + delta, 1);
    setViewYM(`${next.getFullYear()}-${String(next.getMonth() + 1).padStart(2, '0')}`);
  }

  function pickPreset(days: number) {
    const end = new Date();
    const start = new Date();
    start.setDate(end.getDate() - (days - 1));
    onSelect(formatLocalISODate(start), formatLocalISODate(end));
    setViewYM(formatLocalISODate(end).slice(0, 7));
  }

  function handleDayClick(day: string, event: React.MouseEvent) {
    if (event.shiftKey && from) {
      const anchor = from;
      const [a, b] = day < anchor ? [day, anchor] : [anchor, day];
      onSelect(a, b);
      return;
    }
    onSelect(day, day);
  }

  return (
    <div className="usage-calendar" role="group" aria-label="选择统计日期区间">
      <div className="usage-calendar-head">
        <button className="mini-btn" type="button" onClick={() => shiftMonth(-1)} aria-label="上个月">‹</button>
        <span className="usage-calendar-title">{year} 年 {month} 月</span>
        <button className="mini-btn" type="button" onClick={() => shiftMonth(1)} aria-label="下个月">›</button>
      </div>
      <div className="usage-calendar-grid">
        {['一', '二', '三', '四', '五', '六', '日'].map((w) => (
          <span key={w} className="usage-calendar-weekday">{w}</span>
        ))}
        {cells.map((day, index) => {
          if (!day) return <span key={`empty-${index}`} />;
          const inRange = from && to && day >= from && day <= to;
          const isEdge = day === from || day === to;
          const isFuture = day > todayStr;
          return (
            <button
              key={day}
              type="button"
              className={
                `usage-calendar-day${inRange ? ' in-range' : ''}${isEdge ? ' edge' : ''}${day === todayStr ? ' today' : ''}`
              }
              disabled={isFuture}
              onClick={(event) => handleDayClick(day, event)}
              title={day}
            >
              {Number(day.slice(8))}
            </button>
          );
        })}
      </div>
      <div className="usage-calendar-foot">
        <button className="mini-btn" type="button" onClick={() => pickPreset(1)}>今天</button>
        <button className="mini-btn" type="button" onClick={() => pickPreset(7)}>近 7 天</button>
        <button className="mini-btn" type="button" onClick={() => pickPreset(30)}>近 30 天</button>
        {onClear ? <button className="mini-btn" type="button" onClick={onClear} disabled={!from && !to}>清除</button> : null}
        <span className="usage-calendar-hint">单击选单日 · Shift+单击选区间</span>
      </div>
    </div>
  );
}

/** 每日 Token（柱 / 左轴）+ 请求次数（折线 / 右轴）组合图。 */
export function UsageMonthlyTokenBars({ title = '最近 7 天 · 每日用量', points, onPickDay }: {
  title?: string;
  points: DailyRequestPoint[];
  onPickDay?: (date: string) => void;
}) {
  const tokenValues = points.map((p) => (p.inputTokens || 0) + (p.outputTokens || 0));
  const requestValues = points.map((p) => p.requestCount || 0);
  const tokenMax = Math.max(...tokenValues, 1);
  const requestMax = Math.max(...requestValues, 1);
  // 折线坐标：每根柱子的中心点，比例坐标（0~100）
  const linePoints = points.map((p, i) => {
    const x = points.length === 1 ? 50 : ((i + 0.5) / points.length) * 100;
    const y = 100 - (requestValues[i] / requestMax) * 96 - 2;
    return `${x.toFixed(2)},${y.toFixed(2)}`;
  }).join(' ');
  // 数值标注：非 0 的每天都标
  const barX = (i: number) => (points.length === 1 ? 50 : ((i + 0.5) / points.length) * 100);
  const xTickIndexes = pickUsageXTickIndexes(points.length);
  return (
    <div className="usage-month-bars" role="img" aria-label={title}>
      <div className="usage-month-bars-head">
        <div className="usage-month-bars-title">{title}</div>
        <div className="usage-month-bars-legend">
          <span className="usage-month-legend-item"><i className="legend-bar" />Token（左轴）</span>
          <span className="usage-month-legend-item"><i className="legend-line" />请求次数（右轴）</span>
        </div>
      </div>
      {points.length === 0 ? (
        <div className="usage-month-bars-empty">暂无数据</div>
      ) : (
        <>
          <div className="usage-month-bars-plot">
            <div className="usage-month-bars-yaxis" aria-label="Token">
              <span>{formatTokenCount(tokenMax)}</span>
              <span>{formatTokenCount(Math.round(tokenMax / 2))}</span>
              <span>0</span>
            </div>
            <div className="usage-month-bars-track">
              {points.map((p, i) => {
                const value = tokenValues[i];
                const h = value > 0 ? Math.max(4, Math.round((value / tokenMax) * 100)) : 2;
                return (
                  <button
                    key={p.date}
                    type="button"
                    className={`usage-month-bar${value === 0 ? ' zero' : ''}`}
                    title={`${p.date} · ${formatTokenCount(value)} tokens · ${p.requestCount} 次请求`}
                    onClick={() => onPickDay?.(p.date)}
                  >
                    <span style={{ height: `${h}%` }} />
                  </button>
                );
              })}
              <svg
                className="usage-month-line"
                viewBox="0 0 100 100"
                preserveAspectRatio="none"
                aria-hidden="true"
              >
                <polyline
                  points={linePoints}
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="1.6"
                  strokeLinejoin="round"
                  strokeLinecap="round"
                  vectorEffect="non-scaling-stroke"
                />
              </svg>
              {/* 柱内底部 Token 标注（k / M / B），与折线上方的请求数标注错开 */}
              {points.map((p, i) => {
                if (tokenValues[i] <= 0) return null;
                return (
                  <span
                    key={`tl-${p.date}`}
                    className="usage-month-label token"
                    style={{ left: `${barX(i)}%`, bottom: '3%' }}
                  >
                    {formatEnCompact(tokenValues[i])}
                  </span>
                );
              })}
              {/* 折线请求次数标注，非 0 每天都标 */}
              {points.map((p, i) => {
                if (requestValues[i] <= 0) return null;
                const y = (requestValues[i] / requestMax) * 96 + 2;
                return (
                  <span
                    key={`rl-${p.date}`}
                    className="usage-month-label request"
                    style={{ left: `${barX(i)}%`, bottom: `${Math.min(y + 3, 95)}%` }}
                  >
                    {formatEnCompact(requestValues[i])}
                  </span>
                );
              })}
            </div>
            <div className="usage-month-bars-yaxis right" aria-label="请求次数">
              <span>{requestMax}</span>
              <span>{Math.round(requestMax / 2)}</span>
              <span>0</span>
            </div>
          </div>
          <div className="usage-month-bars-xaxis">
            {points.map((p, i) => (
              <span key={p.date} className="usage-month-bars-xtick">
                {xTickIndexes.includes(i) ? p.date.slice(5) : ''}
              </span>
            ))}
          </div>
        </>
      )}
    </div>
  );
}

/** 按日转发流量（接收/发送）双折线图。数据来自 usage_daily 汇总，与请求量同源、
 *  一天一条、永久保留。用 SVG 折线而非柱状：天数多时柱子会挤在一起互相重叠，
 *  折线在任意天数下都不会。 */
export function UsageDailyTrafficLines({ title = '按日流量', points, onPickDay }: {
  title?: string;
  points: DailyRequestPoint[];
  onPickDay?: (date: string) => void;
}) {
  const width = 960;
  const height = 260;
  const padLeft = 74;
  const padRight = 74;
  const padTop = 26;
  const padBottom = 46;
  const plotWidth = width - padLeft - padRight;
  const plotHeight = height - padTop - padBottom;

  const rxValues = points.map((p) => p.rxBytes || 0);
  const txValues = points.map((p) => p.txBytes || 0);
  const xTickIndexes = pickUsageXTickIndexes(points.length);

  // 双纵轴：接收通常比发送大一两个数量级（实测 91 MB vs 1.3 MB），共用一根轴会
  // 把发送线压在 0 刻度上、和它自己的标注值对不上。故左轴管接收、右轴管发送，
  // 各按自身量级缩放；轴刻度用同色标注，明确哪条线读哪根轴。
  const rxMax = Math.max(1, ...rxValues);
  const txMax = Math.max(1, ...txValues);

  // 单点时居中，避免除零。
  const xAt = (i: number) => (points.length <= 1 ? padLeft + plotWidth / 2 : padLeft + (i / (points.length - 1)) * plotWidth);
  const yOf = (v: number, scaleMax: number) => height - padBottom - (v / scaleMax) * plotHeight;
  const yRx = (v: number) => yOf(v, rxMax);
  const yTx = (v: number) => yOf(v, txMax);
  const polyline = (values: number[], scaleMax: number) =>
    values.map((v, i) => `${xAt(i).toFixed(1)},${yOf(v, scaleMax).toFixed(1)}`).join(' ');

  const hasTraffic = rxValues.some((v) => v > 0) || txValues.some((v) => v > 0);
  // 点太密时标注会糊成一片，只在刻度日标注；天数少时每天都标。
  const labelIndexes = points.length <= 14
    ? points.map((_, i) => i)
    : xTickIndexes;

  return (
    <div className="usage-chart-card usage-traffic-card">
      <div className="usage-month-bars-head">
        <div className="usage-month-bars-title">{title}</div>
        {/* 图例只做颜色与读轴说明。刻意不放合计数值：区间可能是几十天，
            一个区间总量对逐日曲线没有解释力，具体数值看点标注与悬浮提示。 */}
        <div className="usage-month-bars-legend">
          <span className="usage-month-legend-item"><i className="legend-line traffic-rx" />接收（左轴）</span>
          <span className="usage-month-legend-item"><i className="legend-line traffic-tx" />发送（右轴）</span>
        </div>
      </div>
      {points.length === 0 ? (
        <div className="empty-state compact">暂无数据</div>
      ) : !hasTraffic ? (
        <div className="empty-state compact">暂无流量数据（本功能上线后的请求才会计入）</div>
      ) : (
        <svg viewBox={`0 0 ${width} ${height}`} className="usage-chart-svg" role="img" aria-label={title}>
          {[0, 0.25, 0.5, 0.75, 1].map((ratio) => {
            const y = height - padBottom - ratio * plotHeight;
            return (
              <g key={`grid-${ratio}`}>
                <line className="tl-grid" x1={padLeft} y1={y} x2={width - padRight} y2={y} stroke="currentColor" strokeWidth="1" />
                {/* 左轴刻度＝接收量级，右轴刻度＝发送量级，各自用线色标注 */}
                <text className="tl-val rx" x={padLeft - 8} y={y + 3} textAnchor="end" fontSize="10" fill="currentColor">
                  {formatBytes(rxMax * ratio, 1)}
                </text>
                <text className="tl-val tx" x={width - padRight + 8} y={y + 3} textAnchor="start" fontSize="10" fill="currentColor">
                  {formatBytes(txMax * ratio, 1)}
                </text>
              </g>
            );
          })}
          {/* 坐标轴：左右各一根纵轴 + 一根横轴 */}
          <line className="tl-axis" x1={padLeft} y1={padTop} x2={padLeft} y2={height - padBottom} stroke="currentColor" strokeWidth="1" />
          <line className="tl-axis" x1={width - padRight} y1={padTop} x2={width - padRight} y2={height - padBottom} stroke="currentColor" strokeWidth="1" />
          <line className="tl-axis" x1={padLeft} y1={height - padBottom} x2={width - padRight} y2={height - padBottom} stroke="currentColor" strokeWidth="1" />
          {/* 轴标题 */}
          <text className="tl-val rx" x={padLeft - 8} y={padTop - 10} textAnchor="end" fontSize="10" fontWeight="700" fill="currentColor">接收</text>
          <text className="tl-val tx" x={width - padRight + 8} y={padTop - 10} textAnchor="start" fontSize="10" fontWeight="700" fill="currentColor">发送</text>
          <text className="tl-axis-title" x={width / 2} y={height - 6} textAnchor="middle" fontSize="10" fontWeight="700" fill="currentColor">日期</text>

          <polyline points={polyline(rxValues, rxMax)} fill="none" className="tl-line rx" stroke="currentColor" strokeWidth="2" strokeLinejoin="round" strokeLinecap="round" />
          <polyline points={polyline(txValues, txMax)} fill="none" className="tl-line tx" stroke="currentColor" strokeWidth="2" strokeLinejoin="round" strokeLinecap="round" />

          {points.map((p, i) => (
            <g key={`pt-${p.date}`}>
              {/* 透明热区：整列可点/可悬浮，不必精准命中细小的点 */}
              <rect
                x={xAt(i) - Math.max(6, plotWidth / Math.max(points.length, 1) / 2)}
                y={padTop}
                width={Math.max(12, plotWidth / Math.max(points.length, 1))}
                height={plotHeight}
                fill="transparent"
                style={{ cursor: onPickDay ? 'pointer' : 'default' }}
                onClick={() => onPickDay?.(p.date)}
              >
                <title>{`${p.date} · 接收 ${formatBytes(rxValues[i])} · 发送 ${formatBytes(txValues[i])}`}</title>
              </rect>
              {rxValues[i] > 0 ? <circle cx={xAt(i)} cy={yRx(rxValues[i])} r="3" className="tl-dot rx" fill="currentColor" /> : null}
              {txValues[i] > 0 ? <circle cx={xAt(i)} cy={yTx(txValues[i])} r="3" className="tl-dot tx" fill="currentColor" /> : null}
            </g>
          ))}

          {/* 散点数值标注。两条线各读自己的轴，位置可能仍然撞上：按屏幕 y 判断
              谁在上方，上者标上、下者标下；再钳进绘图区避免顶到轴标题。 */}
          {points.map((p, i) => {
            if (!labelIndexes.includes(i)) return null;
            const rx = rxValues[i];
            const tx = txValues[i];
            const rxScreenY = yRx(rx);
            const txScreenY = yTx(tx);
            const clamp = (y: number) => Math.min(Math.max(y, padTop + 8), height - padBottom - 3);
            const collide = Math.abs(rxScreenY - txScreenY) < 14;
            const rxAbove = !collide || rxScreenY <= txScreenY;
            return (
              <g key={`lbl-${p.date}`} pointerEvents="none">
                {rx > 0 ? (
                  <text x={xAt(i)} y={clamp(rxScreenY + (rxAbove ? -9 : 15))} textAnchor="middle" fontSize="9.5" fontWeight="700" className="tl-val rx" fill="currentColor">
                    {formatBytes(rx, 1)}
                  </text>
                ) : null}
                {tx > 0 ? (
                  <text x={xAt(i)} y={clamp(txScreenY + (rxAbove ? 15 : -9))} textAnchor="middle" fontSize="9.5" fontWeight="700" className="tl-val tx" fill="currentColor">
                    {formatBytes(tx, 1)}
                  </text>
                ) : null}
              </g>
            );
          })}

          {points.map((p, i) => (
            xTickIndexes.includes(i) ? (
              <g key={`x-${p.date}`}>
                <line className="tl-axis" x1={xAt(i)} y1={height - padBottom} x2={xAt(i)} y2={height - padBottom + 4} stroke="currentColor" strokeWidth="1" />
                <text
                  x={xAt(i)}
                  y={height - padBottom + 17}
                  textAnchor="middle"
                  fontSize="10"
                  fill="currentColor"
                  className="tl-tick"
                >
                  {p.date.slice(5)}
                </text>
              </g>
            ) : null
          ))}
        </svg>
      )}
    </div>
  );
}
