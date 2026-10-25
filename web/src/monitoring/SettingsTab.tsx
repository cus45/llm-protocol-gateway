import { useEffect, useId, useState } from 'react';
import { Switch } from '../components/ui';
import type {
  MonitorChannelInput,
  MonitorChannelView,
  MonitorDispatchConfig,
  MonitorProbeConfig,
  MonitorSeverity,
} from './types';
import { SEVERITY_TEXT } from './types';
import { minutesText } from './ProbesTab';

export interface SettingsTabProps {
  channels: MonitorChannelView[];
  dispatch: MonitorDispatchConfig | null;
  busy: string;
  error: string;
  notice: string;
  onCreateChannel: (input: MonitorChannelInput) => void;
  onUpdateChannel: (id: string, patch: Partial<MonitorChannelInput>) => void;
  onDeleteChannel: (id: string) => void;
  onTestChannel: (id: string) => void;
  onUpdateDispatch: (patch: Partial<MonitorDispatchConfig>) => void;
  onDismissNotice: () => void;
  /** 探测配置的唯一入口放在这里：它与「投递与静默」同属调度参数，探测页只负责看结果。 */
  probeConfig: MonitorProbeConfig | null;
  probeDraft: MonitorProbeConfig | null;
  probeBusy: boolean;
  probeError: string;
  probeNotice: string;
  onProbeDraft: (config: MonitorProbeConfig) => void;
  onSaveProbeConfig: (config: MonitorProbeConfig) => void;
}

const CHANNEL_TYPES: Array<{ value: string; label: string; mark: string }> = [
  { value: 'feishu', label: '飞书', mark: '飞书' },
  { value: 'dingtalk', label: '钉钉', mark: '钉钉' },
  { value: 'wecom', label: '企业微信', mark: '企微' },
  { value: 'telegram', label: 'Telegram', mark: 'TG' },
];

const SEVERITIES: MonitorSeverity[] = ['p0', 'p1', 'p2'];

/**
 * 渠道行的 webhook 展示。凭据只写不读，服务端不会回传原文，所以这里不编造主机名：
 * 填过就按官方 webhook 的形态脱敏（主机 + /…/…），没填就如实写「未配置」。
 * Telegram 走 bot token + chat id，没有 webhook，单列 chat。
 */
const WEBHOOK_HOSTS: Record<string, string> = {
  feishu: 'https://open.feishu.cn/open-apis/bot/v2/hook/',
  dingtalk: 'https://oapi.dingtalk.com/robot/send?access_token=',
  wecom: 'https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=',
};

function webhookText(channel: MonitorChannelView): string {
  if (channel.type === 'telegram') return `bot ${channel.webhookSet || channel.tokenSet ? '••••••••' : '未配置'}${channel.chatId ? ` · chat ${channel.chatId}` : ''}`;
  const host = WEBHOOK_HOSTS[channel.type];
  if (!host) return channel.webhookSet ? '••••••••••••••••' : '未配置 webhook';
  return channel.webhookSet ? `${host}••••••••` : `${host}（未配置）`;
}

interface ChannelDraft {
  id: string;
  type: string;
  name: string;
  minSeverity: MonitorSeverity;
  enabled: boolean;
  webhook: string;
  secret: string;
  botToken: string;
  chatId: string;
}

const emptyDraft = (): ChannelDraft => ({
  id: '', type: 'feishu', name: '', minSeverity: 'p1', enabled: true,
  webhook: '', secret: '', botToken: '', chatId: '',
});

const draftFromChannel = (channel: MonitorChannelView): ChannelDraft => ({
  id: channel.id,
  type: channel.type,
  name: channel.name,
  minSeverity: channel.minSeverity,
  enabled: channel.enabled,
  webhook: '',
  secret: '',
  botToken: '',
  chatId: channel.chatId ?? '',
});

const typeLabel = (type: string) => CHANNEL_TYPES.find((item) => item.value === type)?.label ?? type;
const typeMark = (type: string) => CHANNEL_TYPES.find((item) => item.value === type)?.mark ?? '?';

interface ParamProps {
  label: string;
  hint?: string;
  children: (id: string) => React.ReactNode;
}

function Param({ label, hint, children }: ParamProps) {
  const id = useId();
  return (
    <div className="mon-set-param">
      <label className="mon-set-param-label" htmlFor={id}>{label}</label>
      <div className="mon-set-param-control">{children(id)}</div>
      {hint && <p className="mon-set-param-hint">{hint}</p>}
    </div>
  );
}

export function SettingsTab(props: SettingsTabProps) {
  const {
    channels, dispatch, busy, error, notice,
    onCreateChannel, onUpdateChannel, onDeleteChannel, onTestChannel, onUpdateDispatch, onDismissNotice,
    probeConfig, probeDraft, probeBusy, probeError, probeNotice, onProbeDraft, onSaveProbeConfig,
  } = props;
  const [draft, setDraft] = useState<ChannelDraft>(emptyDraft());
  const [form, setForm] = useState<MonitorDispatchConfig | null>(dispatch);
  /** 新增表单默认收起；点「新增渠道」展开，「取消」收回。编辑时固定展开。 */
  const [createOpen, setCreateOpen] = useState(false);

  useEffect(() => setForm(dispatch), [dispatch]);

  const isTelegram = draft.type === 'telegram';
  const editing = !!draft.id;
  const formOpen = editing || createOpen;

  const submitChannel = () => {
    const payload: MonitorChannelInput = {
      type: draft.type,
      name: draft.name.trim() || typeLabel(draft.type),
      minSeverity: draft.minSeverity,
      enabled: draft.enabled,
      webhook: draft.webhook.trim(),
      secret: draft.secret.trim(),
      botToken: draft.botToken.trim(),
      chatId: draft.chatId.trim(),
    };
    if (editing) onUpdateChannel(draft.id, payload);
    else onCreateChannel(payload);
    setDraft(emptyDraft());
    setCreateOpen(false);
  };

  const credentialReady = (channel: MonitorChannelView) => !!(channel.webhookSet || channel.secretSet || channel.tokenSet);
  const enabledChannels = channels.filter((channel) => channel.enabled).length;

  // 探测草稿：配置先到、或草稿先到都渲染；表单值以草稿为准。
  const probeBase = probeDraft ?? probeConfig;
  const setProbe = (patch: Partial<MonitorProbeConfig>) => probeBase && onProbeDraft({ ...probeBase, ...patch });
  const setEvent = (patch: Partial<MonitorProbeConfig['eventDriven']>) =>
    probeBase && onProbeDraft({ ...probeBase, eventDriven: { ...probeBase.eventDriven, ...patch } });
  const setDaily = (patch: Partial<MonitorProbeConfig['daily']>) =>
    probeBase && onProbeDraft({ ...probeBase, daily: { ...probeBase.daily, ...patch } });
  // type=number 在输入非法时 value 是 ''，直接 Number('') 会变成 0（悄悄改成「不限」）。
  const numberValue = (raw: string, fallback: number) => {
    if (raw.trim() === '') return fallback;
    const value = Number(raw);
    return Number.isFinite(value) ? Math.trunc(value) : fallback;
  };
  const timezone = form?.timezone || dispatch?.timezone || 'Asia/Shanghai';
  const dispatchDirty = !!form && !!dispatch && JSON.stringify(form) !== JSON.stringify(dispatch);
  const probeDirty = !!probeBase && !!probeDraft && JSON.stringify(probeBase) !== JSON.stringify(probeConfig);

  return (
    <div className="mon-settings">
      {/* ── 巡检与轻探测引擎调度 ─────────────────────────────────────── */}
      <section className="mon-set-card">
        <header className="mon-set-head">
          <div className="mon-set-head-text">
            <h2 className="mon-card-title">
              <svg className="mon-set-head-icon" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round">
                <path d="M4 7h9M17 7h3M4 17h3M11 17h9" /><circle cx="15" cy="7" r="2" /><circle cx="9" cy="17" r="2" />
              </svg>
              {'巡检与轻探测引擎调度'}
            </h2>
            <p className="mon-set-desc">
              {`只对已启用、24 小时内有过真实请求的账号探测；当日次数与每日窗口都按投递时区（${timezone}）计算。静默时段只顺延 P1，P0 永远即时送达。`}
            </p>
          </div>
        </header>

        {probeError && <div className="mon-acc-error" role="alert">{probeError}</div>}
        {error && <div className="mon-acc-error" role="alert">{error}</div>}

        {/* 参数组按「一对一组」排：左列探测、右列投递，同一行是同类语义（开关 / 周期 / 静默时段 / 时区）。 */}
        <div className="mon-set-grid">
          {probeBase && (
            <Param label="轻探测开关" hint="关闭后只在探测页手动触发，不随请求自动补探。">
              {(id) => (
                <span id={id} className="mon-set-param-switch">
                  <Switch
                    checked={probeBase.eventDriven.enabled}
                    label={probeBase.eventDriven.enabled ? '事件驱动轻探测已开启' : '事件驱动轻探测已关闭'}
                    onChange={(value) => setEvent({ enabled: value })}
                  />
                </span>
              )}
            </Param>
          )}
          {form && (
            <Param label="静默与恢复" hint="静默开启时只顺延 P1，P0 仍即时送达。">
              {(id) => (
                <div id={id} className="mon-set-switches">
                  <Switch checked={form.quietEnabled} label="启用静默时段" onChange={(value) => setForm({ ...form, quietEnabled: value })} />
                  <Switch checked={form.recoveryNotify} label="发送恢复通知" onChange={(value) => setForm({ ...form, recoveryNotify: value })} />
                </div>
              )}
            </Param>
          )}
          {probeBase && (
            <Param label="每账号每日上限" hint="0 = 不限。超过上限的账号当天不再补探。">
              {(id) => (
                <div className="mon-set-input-pair">
                  <input
                    id={id}
                    type="number"
                    min={0}
                    max={24}
                    value={probeBase.eventDriven.maxPerDayPerAccount}
                    onChange={(event) => setEvent({ maxPerDayPerAccount: numberValue(event.target.value, probeBase.eventDriven.maxPerDayPerAccount) })}
                  />
                  <span className="mon-set-unit">次 / 天</span>
                </div>
              )}
            </Param>
          )}
          {form && (
            <Param label="聚合间隔" hint="同类告警多久合并成一条摘要；当前逐条即时发送，聚合摘要随后续阶段上线。">
              {(id) => (
                <div className="mon-set-input-pair">
                  <input
                    id={id}
                    type="number"
                    min={5}
                    max={120}
                    value={form.digestMinutes}
                    onChange={(event) => setForm({ ...form, digestMinutes: Number(event.target.value) })}
                  />
                  <span className="mon-set-unit">分钟 / 次</span>
                </div>
              )}
            </Param>
          )}
          {probeBase && (
            <Param label="两次探测最小间隔" hint="同一账号两次探测之间至少隔这么久，防止连续请求把探测打满。">
              {(id) => (
                <div className="mon-set-input-pair">
                  <input
                    id={id}
                    type="number"
                    min={0}
                    max={1440}
                    value={probeBase.eventDriven.minIntervalMinutes}
                    onChange={(event) => setEvent({ minIntervalMinutes: numberValue(event.target.value, probeBase.eventDriven.minIntervalMinutes) })}
                  />
                  <span className="mon-set-unit">分钟 / 次</span>
                </div>
              )}
            </Param>
          )}
          {form && (
            <Param label="静默时段" hint="HH:MM–HH:MM，可跨零点（如 22:00–08:00）。">
              {(id) => (
                <div id={id} className="mon-set-range">
                  <input
                    aria-label="静默开始"
                    value={form.quietStart}
                    placeholder="23:00"
                    onChange={(event) => setForm({ ...form, quietStart: event.target.value })}
                  />
                  <span className="mon-set-range-sep" aria-hidden="true">{'–'}</span>
                  <input
                    aria-label="静默结束"
                    value={form.quietEnd}
                    placeholder="07:00"
                    onChange={(event) => setForm({ ...form, quietEnd: event.target.value })}
                  />
                </div>
              )}
            </Param>
          )}
          {probeBase && (
            <Param label="账号闲置多久后补探" hint="闲置超过该时长的账号才会被轻探测挑中。">
              {(id) => (
                <div className="mon-set-input-pair">
                  <input
                    id={id}
                    type="number"
                    min={0}
                    max={1440}
                    value={probeBase.eventDriven.idleMinutes}
                    onChange={(event) => setEvent({ idleMinutes: numberValue(event.target.value, probeBase.eventDriven.idleMinutes) })}
                  />
                  <span className="mon-set-unit">分钟</span>
                </div>
              )}
            </Param>
          )}
          {form && (
            <Param label="时区" hint="静默时段、每日窗口与当日次数都按这个时区计算。">
              {(id) => (
                <input id={id} value={form.timezone} placeholder="Asia/Shanghai" onChange={(event) => setForm({ ...form, timezone: event.target.value })} />
              )}
            </Param>
          )}
          {probeBase && (
            <Param label="每日窗口" hint={`当前生效：${probeBase.daily.enabled ? `${probeBase.daily.windowStart}–${probeBase.daily.windowEnd}` : '已关闭'}（${timezone}）`}>
              {(id) => (
                <div id={id} className="mon-set-window">
                  <Switch checked={probeBase.daily.enabled} label="启用每日窗口" onChange={(value) => setDaily({ enabled: value })} />
                  <div className="mon-set-range">
                    <input
                      aria-label="每日窗口开始"
                      value={probeBase.daily.windowStart}
                      placeholder="04:00"
                      onChange={(event) => setDaily({ windowStart: event.target.value })}
                    />
                    <span className="mon-set-range-sep" aria-hidden="true">{'–'}</span>
                    <input
                      aria-label="每日窗口结束"
                      value={probeBase.daily.windowEnd}
                      placeholder="06:00"
                      onChange={(event) => setDaily({ windowEnd: event.target.value })}
                    />
                  </div>
                </div>
              )}
            </Param>
          )}
          {form && (
            <Param label="控制台地址" hint="消息里「查看详情」深链的前缀。">
              {(id) => <input id={id} value={form.baseUrl} placeholder="https://…" onChange={(event) => setForm({ ...form, baseUrl: event.target.value })} />}
            </Param>
          )}
          {probeBase && (
            <Param
              label="额度读数过期判定"
              hint={probeBase.quotaStaleMinutes > 0
                ? `超过 ${probeBase.quotaStaleMinutes} 分钟未更新时，账号页标「数据过期」。`
                : '已关闭：账号页与详情不标「数据过期」。'}
            >
              {(id) => (
                <div className="mon-set-input-pair">
                  <input
                    id={id}
                    type="number"
                    min={0}
                    value={probeBase.quotaStaleMinutes}
                    onChange={(event) => setProbe({ quotaStaleMinutes: Math.max(0, numberValue(event.target.value, probeBase.quotaStaleMinutes)) })}
                  />
                  <span className="mon-set-unit">分钟</span>
                </div>
              )}
            </Param>
          )}
        </div>
        {probeBase && (
          <p className="mon-set-foot">
            {`当前生效：每账号每日 ${probeBase.eventDriven.maxPerDayPerAccount === 0 ? '不限' : `${probeBase.eventDriven.maxPerDayPerAccount} 次`} · 最小间隔 ${minutesText(probeBase.eventDriven.minIntervalMinutes)} · 闲置阈值 ${minutesText(probeBase.eventDriven.idleMinutes)} · 每日窗口 ${probeBase.daily.enabled ? `${probeBase.daily.windowStart}–${probeBase.daily.windowEnd}` : '已关闭'}（${timezone}）`}
          </p>
        )}
        {(probeDirty || dispatchDirty) && (
          <p className="mon-set-inline mon-set-dirty">
            {[probeDirty ? '探测配置' : '', dispatchDirty ? '投递设置' : ''].filter(Boolean).join('与') + '有未保存的修改。'}
          </p>
        )}

        <div className="mon-set-actions">
          <span className="mon-set-inline" role="status">{probeNotice || notice}</span>
          <button
            type="button"
            className="btn btn-sm primary"
            disabled={probeBusy || !probeDraft}
            onClick={() => probeDraft && onSaveProbeConfig(probeDraft)}
          >
            {'保存探测配置'}
          </button>
        </div>
      </section>

      {/* ── 通知渠道与 Webhook 机器人 ─────────────────────────────────── */}
      <section className="mon-set-card">
        <header className="mon-set-head">
          <div className="mon-set-head-text">
            <h2 className="mon-card-title">
              <svg className="mon-set-head-icon warn" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
                <path d="M18 8a6 6 0 1 0-12 0c0 6-2.5 7-2.5 7h17S18 14 18 8Z" /><path d="M10.3 19.5a2 2 0 0 0 3.4 0" />
              </svg>
              {'通知渠道与 Webhook 机器人'}
            </h2>
            <p className="mon-set-desc">
              {'webhook、签名密钥与 bot token 只写不读：留空表示保留服务端已有值。告警按级别投递到各渠道的最低级别以上。'}
            </p>
          </div>
          <div className="mon-set-chips">
            <span className="mon-set-chip">
              <span className={`mon-acc-state-dot ${enabledChannels > 0 ? 'ok' : 'off'}`} aria-hidden />
              {enabledChannels > 0 ? `${enabledChannels} 个启用` : '通道未就绪'}
            </span>
            <span className="mon-set-chip">{`${channels.length - enabledChannels} 个停用`}</span>
          </div>
        </header>

        {error && <div className="mon-acc-error" role="alert">{error}</div>}

        {channels.length === 0 && (
          <div className="mon-set-empty">
            <span className="mon-set-empty-title">{'还没有通知渠道'}</span>
            <span className="mon-set-empty-note">{'告警仍然会记录在本地，只是不会推送到任何群。下面新建一个渠道即可开始推送。'}</span>
          </div>
        )}
        {channels.length > 0 && (
          <div className="mon-set-list">
            {channels.map((channel) => (
              <div className={`mon-set-row${channel.enabled ? '' : ' is-off'}`} key={channel.id}>
                <span className={`mon-set-icon type-${channel.type}`} aria-hidden="true">{typeMark(channel.type)}</span>
                <div className="mon-set-row-main">
                  <div className="mon-set-row-head">
                    <span className="mon-set-row-name">{channel.name}</span>
                    <span className="mon-acc-tag">{typeLabel(channel.type)}</span>
                    <span className={`mon-acc-state ${channel.minSeverity === 'p0' ? 'bad' : channel.minSeverity === 'p1' ? 'warn' : ''}`}>
                      {`≥ ${channel.minSeverity.toUpperCase()}`}
                    </span>
                    <span className={`mon-acc-state ${channel.enabled ? 'ok' : 'off'}`}>
                      <span className="mon-acc-state-dot" aria-hidden />
                      {channel.enabled ? '启用' : '停用'}
                    </span>
                  </div>
                  <div className="mon-set-row-meta">
                    <code className="mon-set-row-hook" title={credentialReady(channel) ? '凭据已配置，原值不回传' : '尚未填写凭据'}>
                      {webhookText(channel)}
                    </code>
                    <span className={credentialReady(channel) ? 'mon-set-row-ok' : 'mon-set-row-warn'}>
                      {credentialReady(channel) ? '凭据已配置' : '未填凭据，推送会失败'}
                    </span>
                  </div>
                </div>
                <div className="mon-set-row-actions">
                  <Switch
                    checked={channel.enabled}
                    disabled={busy === `channel:update`}
                    onChange={(value) => onUpdateChannel(channel.id, { enabled: value })}
                    label={channel.enabled ? '停用渠道' : '启用渠道'}
                  />
                  <button type="button" className="btn btn-sm" onClick={() => setDraft(draftFromChannel(channel))}>{'编辑'}</button>
                  <button type="button" className="btn btn-sm" disabled={busy === `test:${channel.id}`} onClick={() => onTestChannel(channel.id)}>{'测试'}</button>
                  <button type="button" className="btn btn-sm" disabled={busy === `del:${channel.id}`} onClick={() => onDeleteChannel(channel.id)}>{'删除'}</button>
                </div>
              </div>
            ))}
          </div>
        )}

        {formOpen && (
          <div className="mon-set-form">
            <div className="mon-set-group-head">
              <span className="mon-set-group-title">{editing ? '编辑渠道' : '新增渠道'}</span>
              <span className="mon-set-group-note">
                {editing
                  ? `正在编辑「${draft.name || '未命名渠道'}」；凭据留空即保留原值。`
                  : '填好名称与凭据即可创建；类型创建后不可更改。'}
              </span>
            </div>
            <div className="monitoring-cols">
              <label className="field">
                <span>{'类型'}</span>
                <select
                  value={draft.type}
                  disabled={editing}
                  onChange={(event) => setDraft({ ...draft, type: event.target.value })}
                >
                  {CHANNEL_TYPES.map((item) => (
                    <option key={item.value} value={item.value}>{item.label}</option>
                  ))}
                </select>
              </label>
              <label className="field">
                <span>{'名称'}</span>
                <input value={draft.name} placeholder="值班群" onChange={(event) => setDraft({ ...draft, name: event.target.value })} />
              </label>
              <label className="field">
                <span>{'最低级别'}</span>
                <select
                  value={draft.minSeverity}
                  onChange={(event) => setDraft({ ...draft, minSeverity: event.target.value as MonitorSeverity })}
                >
                  {SEVERITIES.map((value) => (
                    <option key={value} value={value}>{SEVERITY_TEXT[value]}</option>
                  ))}
                </select>
              </label>
              <div className="field mon-set-switch">
                <span>{'状态'}</span>
                <Switch checked={draft.enabled} label="启用" onChange={(value) => setDraft({ ...draft, enabled: value })} />
              </div>
            </div>

            {isTelegram ? (
              <div className="monitoring-cols">
                <label className="field">
                  <span>{'Bot Token'}</span>
                  <input
                    type="password"
                    value={draft.botToken}
                    placeholder={editing ? '留空保持不变' : ''}
                    onChange={(event) => setDraft({ ...draft, botToken: event.target.value })}
                  />
                </label>
                <label className="field">
                  <span>{'Chat ID'}</span>
                  <input value={draft.chatId} onChange={(event) => setDraft({ ...draft, chatId: event.target.value })} />
                </label>
              </div>
            ) : (
              <div className="monitoring-cols">
                <label className="field">
                  <span>{'Webhook'}</span>
                  <input
                    type="password"
                    value={draft.webhook}
                    placeholder={editing ? '留空保持不变' : 'https://…'}
                    onChange={(event) => setDraft({ ...draft, webhook: event.target.value })}
                  />
                </label>
                <label className="field">
                  <span>{'签名密钥（可选）'}</span>
                  <input
                    type="password"
                    value={draft.secret}
                    placeholder={editing ? '留空保持不变' : ''}
                    onChange={(event) => setDraft({ ...draft, secret: event.target.value })}
                  />
                </label>
              </div>
            )}

            <div className="mon-set-form-actions">
              <button type="button" className="btn btn-sm primary" disabled={(busy ?? '').startsWith('channel')} onClick={submitChannel}>
                {editing ? '保存修改' : '创建渠道'}
              </button>
              <button
                type="button"
                className="btn btn-sm"
                onClick={() => { setDraft(emptyDraft()); setCreateOpen(false); }}
              >
                {editing ? '取消编辑' : '取消'}
              </button>
            </div>
          </div>
        )}

        <div className="mon-set-actions">
          <span className="mon-set-inline" role="status">{notice}</span>
          <button type="button" className="btn btn-sm" onClick={() => { setDraft(emptyDraft()); setCreateOpen(true); }}>
            {'新增渠道'}
          </button>
          <button
            type="button"
            className="btn btn-sm primary"
            disabled={busy === 'dispatch' || !form}
            onClick={() => form && onUpdateDispatch(form)}
          >
            {'保存调度配置'}
          </button>
        </div>
      </section>
    </div>
  );
}
