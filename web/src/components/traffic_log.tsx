import type { LogEntry, Provider } from '../types';
import { ProviderLabel } from './provider_label';
import { actionLabel, isTrafficLogError, trafficLogKeyLabel, trafficLogKeyTitle, trafficLogProviderLabel } from '../lib';

/**
 * API 日志的请求表。版式照 magpie 的日志页抄：一行一条、数字等宽右对齐、
 * 耗时与速度带语义色、状态是一个点加两个字，不塞胶囊也不折第二行。
 *
 * 我们手里没有 magpie 的「费用 / 缓存写入 / 推理强度」，所以不假装：
 * 列只放网关真的记下来的东西，缺的就不占位。
 */

const NUM = 'en-US';

function fmtNum(value: number | undefined) {
  if (value == null || Number.isNaN(value)) return '—';
  return value.toLocaleString(NUM);
}

/** 耗时按量级换说法，颜色按量级分档：一眼能扫出慢请求。 */
function latencyCell(ms: number) {
  if (!ms || ms <= 0) return { text: '—', tone: '' };
  const text = ms < 1000 ? `${ms} ms` : ms < 60000 ? `≈${(ms / 1000).toFixed(1)} 秒` : `≈${Math.round(ms / 60000)} 分`;
  if (ms < 5000) return { text, tone: 'ok' };
  if (ms < 20000) return { text, tone: 'warn' };
  return { text, tone: 'slow' };
}

function speedCell(log: LogEntry) {
  if (!log.latencyMs || log.latencyMs <= 0 || !log.outputTokens) return { text: '—', tone: '' };
  const perSecond = Math.round((log.outputTokens * 1000) / log.latencyMs);
  if (perSecond <= 0) return { text: '—', tone: '' };
  // 短响应配极小耗时会算出六位数的"速度"，那列不该被它撑爆。
  const label = perSecond >= 100000 ? '99k+' : perSecond >= 10000 ? `${Math.round(perSecond / 1000)}k` : String(perSecond);
  return { text: `≥${label} token/秒`, tone: perSecond >= 30 ? 'ok' : '' };
}

function KeyGlyph() {
  return (
    <svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <circle cx="8" cy="12" r="4.2" />
      <path d="M12.2 12H21M17.5 12v3M20.8 12v2.2" />
    </svg>
  );
}

/**
 * 网关把别名映射记成一个字符串 `客户端模型 -> 实际模型`（见 model_alias.go）。
 * 挤在一列里既截断又读不动，所以照 magpie 拆成两列；没有映射时两边同值，
 * 而不是留一个永远为空的格子。
 */
function splitModel(value: string) {
  const index = value.indexOf(' -> ');
  if (index < 0) return { requested: value, effective: value };
  const requested = value.slice(0, index).trim();
  const effective = value.slice(index + 4).trim();
  return { requested: requested || effective, effective };
}

/**
 * `Claude -> OpenAI Chat` 写全要 170px，每行都放一遍既撑爆表格又是噪声：
 * 压成 C→O，完整流向留在 title 里。
 */
function compactFlow(label: string) {
  if (!label) return '—';
  const [from, to] = label.split('->').map((part) => part.trim());
  const initial = (value: string) => (value ? value[0].toUpperCase() : '?');
  return to ? `${initial(from)}→${initial(to)}` : initial(from);
}

export function TrafficLogTable({ logs, providers, onOpen }: {
  logs: LogEntry[];
  providers: Provider[];
  onOpen: (log: LogEntry) => void;
}) {
  return (
    <div className="tlog-scroll">
      <div className="tlog">
        <div className="tlog-grid tlog-head">
          <span>时间</span>
          <span>密钥</span>
          <span>请求模型</span>
          <span>实际模型</span>
          <span title="上游实际收到的推理设置">推理强度</span>
          <span>供应商</span>
          <span>协议</span>
          <span className="num">输入</span>
          <span className="num">输出</span>
          <span className="num" title="写入提示缓存的 token（已含在输入里）">缓存写入</span>
          <span className="num" title="命中缓存读取的 token（已含在输入里）">缓存读取</span>
          <span className="num">耗时</span>
          <span className="num">速度</span>
          <span>状态</span>
        </div>
        {logs.map((log) => {
          const error = isTrafficLogError(log);
          const ok = log.status >= 200 && log.status < 300;
          const latency = latencyCell(log.latencyMs);
          const speed = speedCell(log);
          const provider = providers.find((item) => item.id === log.providerId);
          const model = splitModel(log.model || '');
          return (
            <div
              className={`tlog-grid tlog-row${error ? ' error' : ''}`}
              key={`${log.id ?? log.time}-${log.model}-${log.latencyMs}`}
              onClick={() => onOpen(log)}
              title="点击查看请求链路 APM 耗时分解与完整报文"
              role="button"
              tabIndex={0}
              onKeyDown={(event) => { if (event.key === 'Enter') onOpen(log); }}
            >
              <span className="tlog-time">{new Date(log.time).toLocaleTimeString('zh-CN', { hour12: false })}</span>
              <span className="tlog-key" title={trafficLogKeyTitle(log)}>
                <KeyGlyph />
                <b>{trafficLogKeyLabel(log)}</b>
              </span>
              <span className="tlog-model" title={model.requested}>{model.requested || '—'}</span>
              <span className="tlog-model effective" title={model.effective}>{model.effective || '—'}</span>
              <span className="tlog-reasoning" title={log.reasoningEffort || '未设置推理强度'}>{log.reasoningEffort || '—'}</span>
              <span className="tlog-provider">
                {provider ? <ProviderLabel provider={provider} size={14} /> : trafficLogProviderLabel(log, providers)}
              </span>
              <span className="tlog-proto" title={`${actionLabel(log.action)} · ${log.protocolFlow || '未知流向'} · ${log.path}`}>
                {compactFlow(log.protocolFlow)}
              </span>
              <span className="num">{fmtNum(log.inputTokens)}</span>
              <span className="num">{fmtNum(log.outputTokens)}</span>
              <span className="num" title="写入提示缓存的 token（已计入输入）">{fmtNum(log.cacheWriteTokens)}</span>
              <span className="num" title="命中缓存读取的 token（已计入输入）">{fmtNum(log.cacheTokens)}</span>
              <span className={`num tlog-latency ${latency.tone}`} title={log.ttftMs != null ? `首字 ${log.ttftMs}ms` : undefined}>
                {latency.text}
              </span>
              <span className={`num tlog-speed ${speed.tone}`}>{speed.text}</span>
              <span className={`tlog-status ${ok ? 'ok' : 'bad'}`} title={`HTTP ${log.status}${log.errorDescription ? ` · ${log.errorDescription}` : ''}`}>
                <i />
                {ok ? '成功' : '失败'}
              </span>
            </div>
          );
        })}
      </div>
    </div>
  );
}
