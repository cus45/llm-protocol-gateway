import {useEffect, useMemo, useRef, useState} from 'react';
import {API_BASE, creditsExpiryHint, resetHint, subscriptionOAuthPrefix} from '../lib';
import type {ProviderConnectKind, SubscriptionCheckin, SubscriptionUsage} from '../types';
import {CheckinRow, TimeLine, UsageBucketRow, UsagePanelShell, useAccountUsageReport} from './panels';
import {CopyButton} from './ui';

/**
 * 需要浏览器登录的订阅（ZCode、Trae）共用的连接面板。
 *
 * WorkBuddy 的那套流程散在 main.tsx 的五个 state 里，是历史原因；新加的订阅
 * 不再复制它。这里一个组件管完：发起、拿链接、轮询状态、暂停与恢复，状态只在
 * 组件内部，父组件只需要在登录完成后刷新一次 state。
 */

type ConnectState = {
  busy: boolean;
  polling: boolean;
  error: string;
  authUrl: string;
  flowId: string;
};

const idle: ConnectState = { busy: false, polling: false, error: '', authUrl: '', flowId: '' };

export function SubscriptionConnect({ providerId, kind, connected, accountLabel, plan, expiresAt, hint, onConnected, onPaused }: {
  providerId: string;
  kind: ProviderConnectKind;
  connected?: boolean;
  accountLabel?: string;
  plan?: string;
  expiresAt?: string;
  /** 该连接方式独有的注意事项，例如 Trae 只接受回环地址的回调。 */
  hint?: string;
  onConnected: () => void;
  onPaused?: () => void;
}) {
  const [state, setState] = useState<ConnectState>(idle);
  // 轮询挂在 ref 上：组件在等待期间可能被父组件重绘甚至换 providerId 复用，
  // 一个过期的轮询不能把新流程的状态覆盖掉。
  const activeFlow = useRef('');

  useEffect(() => () => { activeFlow.current = ''; }, [providerId]);

  const prefix = subscriptionOAuthPrefix(kind);
  const endpoint = (path: string) => `${API_BASE}/__providers/${encodeURIComponent(providerId)}/${prefix}${path}`;

  async function poll(flowId: string) {
    activeFlow.current = flowId;
    setState((current) => ({ ...current, polling: true }));
    const deadline = Date.now() + 10 * 60 * 1000;
    while (activeFlow.current === flowId && Date.now() < deadline) {
      await new Promise((resolve) => setTimeout(resolve, 2000));
      if (activeFlow.current !== flowId) return;
      let payload: { status?: string; message?: string };
      try {
        const response = await fetch(endpoint(`/status?flowId=${encodeURIComponent(flowId)}`));
        if (!response.ok) continue;
        payload = await response.json() as typeof payload;
      } catch {
        continue;
      }
      if (payload.status === 'done') {
        activeFlow.current = '';
        setState({ ...idle });
        onConnected();
        return;
      }
      if (payload.status === 'failed') {
        activeFlow.current = '';
        setState({ ...idle, error: payload.message || '授权失败' });
        return;
      }
    }
    if (activeFlow.current === flowId) {
      activeFlow.current = '';
      setState({ ...idle, error: '登录超时，请重新发起' });
    }
  }

  async function start() {
    setState({ ...idle, busy: true });
    try {
      const response = await fetch(endpoint('/start'), { method: 'POST' });
      const data = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error((data as { error?: { message?: string } }).error?.message || `发起登录失败（HTTP ${response.status}）`);
      const authUrl = (data as { authUrl?: string }).authUrl || '';
      const flowId = (data as { flowId?: string }).flowId || '';
      if (!authUrl) throw new Error('上游没有返回登录页面');
      setState({ busy: false, polling: true, error: '', authUrl, flowId });
      window.open(authUrl, '_blank', 'noopener,noreferrer');
      void poll(flowId);
    } catch (error) {
      setState({ ...idle, error: error instanceof Error ? error.message : String(error) });
    }
  }

  async function setDisconnected(disconnected: boolean) {
    setState({ ...idle, busy: true });
    try {
      const response = await fetch(endpoint(disconnected ? '/pause' : '/resume'), { method: 'POST' });
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      if (onPaused) onPaused();
      setState(idle);
    } catch (error) {
      setState({ ...idle, error: error instanceof Error ? error.message : String(error) });
    }
  }

  const waiting = state.busy || state.polling;

  if (connected && !state.authUrl) {
    return (
      <>
        {/* 「已登录…」与两个按钮同一行：原来按钮单独占一行，账号信息与按钮之间
            隔着面板 12px 行距 + 按钮本身一行高度，看起来就是一段莫名的空白。 */}
        <div className="sub-account-row">
          <div className="sub-account-info">
            <span>{`已登录${accountLabel ? ` · ${accountLabel}` : ''}${plan ? ` · ${plan}` : ''}`}</span>
            {expiresAt ? <span className="sub-account-expiry">{`令牌到期：${new Date(expiresAt).toLocaleString()}`}</span> : null}
            {hint ? <span className="sub-account-hint">{hint}</span> : null}
          </div>
          <div className="sub-account-actions">
            <button className="btn btn-sm" disabled={waiting} onClick={() => void start()}>{waiting ? '等待授权…' : '换账号登录'}</button>
            <button className="btn btn-sm danger" disabled={state.busy} onClick={() => void setDisconnected(true)}>{state.busy ? '处理中…' : '暂停使用'}</button>
          </div>
        </div>
        {state.error ? <div className="hint-line error">{state.error}</div> : null}
      </>
    );
  }

  if (connected) {
    // 已登录，但这次「换账号」正等着新授权：显示链接与等待状态。
    return (
      <>
        <div className="hint-text">{state.polling ? '已打开登录页面，等待完成授权…' : '正在发起登录…'}</div>
        <SubscriptionAuthLink url={state.authUrl} onReopen={() => window.open(state.authUrl, '_blank', 'noopener,noreferrer')} />
        {state.error ? <div className="hint-line error">{state.error}</div> : null}
      </>
    );
  }

  return (
    <>
      <button className="btn primary" disabled={waiting} onClick={() => void start()}>
        {waiting ? '等待授权…' : '登录'}
      </button>
      {state.authUrl ? <SubscriptionAuthLink url={state.authUrl} onReopen={() => window.open(state.authUrl, '_blank', 'noopener,noreferrer')} /> : null}
      {hint ? <div className="hint-text">{hint}</div> : null}
      {state.error ? <div className="hint-line error">{state.error}</div> : null}
    </>
  );
}

function SubscriptionAuthLink({ url, onReopen }: { url: string; onReopen: () => void }) {
  return (
    <div className="field-inline" style={{ marginTop: 8 }}>
      <input type="text" readOnly value={url} onFocus={(event) => event.target.select()} />
      <button className="mini-btn" onClick={onReopen}>重新打开</button>
      <CopyButton value={url} label="复制链接" toastContent="登录链接已复制" className="mini-btn" />
    </div>
  );
}

/**
 * 订阅额度面板：ZCode 的套餐窗口与 Trae 的 credits / 美元窗口形状相同，
 * 所以共用一个；两者都由后端归一成 {plan, windows[]}。
 */
export function SubscriptionUsagePanel({ providerId, accountKey, kind, connected, compact, subtitle, title }: {
  accountKey?: string;
  providerId: string;
  kind: ProviderConnectKind;
  connected?: boolean;
  compact?: boolean;
  subtitle?: string;
  title: string;
}) {
  const prefix = subscriptionOAuthPrefix(kind);
  const request = useMemo(() => ({
    path: `/__providers/${encodeURIComponent(providerId)}/${prefix}/usage`,
  }), [providerId, prefix]);
  const { report, loading, refresh } = useAccountUsageReport<SubscriptionUsage>(
    Boolean(connected),
    `subscription:${accountKey ?? providerId}:${kind}`,
    request,
  );
  // 签到开关与「立即签到」的回答由后端直接给出，先本地显示，等下一次自然刷新
  // 再和额度一起回来——为此重读一次上游额度不值得。
  const [checkinOverride, setCheckinOverride] = useState<SubscriptionCheckin | null>(null);
  useEffect(() => { setCheckinOverride(null); }, [providerId]);
  const checkin = checkinOverride || report?.checkin;
  // 账号级的那一行时间：Qoder 报套餐有效期，WorkBuddy 那类按批发积分的报哪批先到期。
  const expiry = resetHint(report?.until, new Date(), '套餐有效期至') || creditsExpiryHint(report?.windows);

  // 紧凑卡片本来只在该账号报出额度时才出现；签到是厂商账号接口上的另一件事，
  // 额度读不出来时它也得留在原地，否则开关连同状态一起消失。
  const hasWindows = Boolean(report) && !report?.error && (report?.windows.length ?? 0) > 0;
  const showCheckin = Boolean(report?.checkin?.supported);
  if (compact && !hasWindows && !showCheckin) return null;

  return (
    <UsagePanelShell
      panelClass="subscription-usage-panel"
      compact={compact}
      subtitle={subtitle}
      title={title}
      planName={report?.plan}
      stateMessage={report && !report.error && report.windows.length > 0 ? '可用' : undefined}
      loading={loading}
      fetchedAt={report?.fetchedAt}
      onRefresh={refresh}
    >
      {!report ? (
        <div className="claude-usage-empty">{loading ? '正在拉取额度…' : '暂无额度数据'}</div>
      ) : report.error ? (
        <div className="claude-usage-empty">{report.error}</div>
      ) : report.windows.length === 0 ? (
        <div className="claude-usage-empty">该账号没有返回额度窗口（未订阅或接口未开放）</div>
      ) : (
        <div className="claude-usage-grid">
          {report.windows.map((window, index) => (
            <UsageBucketRow
              // WorkBuddy 会返回同名（或都没有名字）的几批额度，只用 name 做 key 时
              // React 会把它们当成同一行：列表重排时内容会串行，控制台还会报重复 key。
              key={`${window.name || 'window'}-${index}`}
              label={window.name}
              percent={Math.min(100, Math.max(0, window.used))}
              note={window.limit != null && window.limit > 0
                ? `${(window.amount ?? 0).toFixed(0)} / ${window.limit.toFixed(0)}${window.unit ? ` ${window.unit}` : ''}`
                : undefined}
              reset={resetHint(window.resetsAt)}
              compact={compact}
            />
          ))}
        </div>
      )}
      {/* 厂商另给的账号级时间：Qoder 报套餐有效期，WorkBuddy 报哪一批积分先过期。
          没有就不写这一行。 */}
      {expiry ? <TimeLine hint={expiry} className="subscription-expiry" /> : null}
      {checkin?.supported ? (
        <CheckinRow providerId={providerId} view={checkin} onChange={setCheckinOverride} />
      ) : null}
    </UsagePanelShell>
  );
}
