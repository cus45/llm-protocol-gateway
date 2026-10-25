import { useEffect, useState } from 'react';
import { Switch } from '../components/ui';
import type {
  MonitorChannelInput,
  MonitorChannelView,
  MonitorDispatchConfig,
  MonitorProbeConfig,
  MonitorSeverity,
} from './types';
import { SEVERITY_TEXT } from './types';
import { ProbeSettingsPanel } from './ProbesTab';

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

const CHANNEL_TYPES: Array<{ value: string; label: string }> = [
  { value: 'feishu', label: '飞书' },
  { value: 'dingtalk', label: '钉钉' },
  { value: 'wecom', label: '企业微信' },
  { value: 'telegram', label: 'Telegram' },
];

const SEVERITIES: MonitorSeverity[] = ['p0', 'p1', 'p2'];

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

export function SettingsTab(props: SettingsTabProps) {
  const {
    channels, dispatch, busy, error, notice,
    onCreateChannel, onUpdateChannel, onDeleteChannel, onTestChannel, onUpdateDispatch, onDismissNotice,
    probeConfig, probeDraft, probeBusy, probeError, probeNotice, onProbeDraft, onSaveProbeConfig,
  } = props;
  const [draft, setDraft] = useState<ChannelDraft>(emptyDraft());
  const [form, setForm] = useState<MonitorDispatchConfig | null>(dispatch);

  useEffect(() => setForm(dispatch), [dispatch]);

  const isTelegram = draft.type === 'telegram';
  const editing = !!draft.id;

  const submitChannel = () => {
    const payload: MonitorChannelInput = {
      type: draft.type,
      name: draft.name.trim() || CHANNEL_TYPES.find((item) => item.value === draft.type)?.label || draft.type,
      minSeverity: draft.minSeverity,
      enabled: draft.enabled,
      webhook: draft.webhook.trim(),
      secret: draft.secret.trim(),
      botToken: draft.botToken.trim(),
      chatId: draft.chatId.trim(),
    };
    if (editing) onUpdateChannel(draft.id, payload);
    else onCreateChannel(payload);
  };

  const credentialReady = (channel: MonitorChannelView) => !!(channel.webhookSet || channel.secretSet || channel.tokenSet);
  const enabledChannels = channels.filter((channel) => channel.enabled).length;

  return (
    <div className="mon-settings">
      <section className="card mon-set-card">
        <header className="mon-set-head">
          <div className="mon-set-head-text">
            <h2 className="mon-card-title">{`通知渠道（${channels.length}）`}</h2>
            <p className="mon-set-desc">
              {'webhook、签名密钥与 bot token 只写不读：留空表示保留服务端已有值。告警按级别投递到各渠道的最低级别以上。'}
            </p>
          </div>
          <div className="mon-set-chips">
            <span className="mon-set-chip">
              <span className={`mon-acc-state-dot ${enabledChannels > 0 ? 'ok' : 'off'}`} aria-hidden />
              {`${enabledChannels} 个启用`}
            </span>
            <span className="mon-set-chip">{`${channels.length - enabledChannels} 个停用`}</span>
          </div>
        </header>

        {error && <div className="mon-acc-error" role="alert">{error}</div>}
        {notice && (
          <div className="mon-set-notice">
            <span className="mon-set-notice-label">{'最近操作'}</span>
            <span className="mon-set-notice-text">{notice}</span>
            <button type="button" className="btn btn-sm mon-set-notice-btn" onClick={onDismissNotice}>{'收起'}</button>
          </div>
        )}

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
                <div className="mon-set-row-main">
                  <div className="mon-set-row-head">
                    <span className="mon-set-row-name">{channel.name}</span>
                    <span className="mon-acc-tag">{CHANNEL_TYPES.find((item) => item.value === channel.type)?.label ?? channel.type}</span>
                    <span className={`mon-acc-state ${channel.minSeverity === 'p0' ? 'bad' : channel.minSeverity === 'p1' ? 'warn' : ''}`}>
                      {`≥ ${channel.minSeverity.toUpperCase()}`}
                    </span>
                    <span className={`mon-acc-state ${channel.enabled ? 'ok' : 'off'}`}>
                      <span className="mon-acc-state-dot" aria-hidden />
                      {channel.enabled ? '启用' : '停用'}
                    </span>
                  </div>
                  <div className="mon-set-row-meta">
                    <span className={credentialReady(channel) ? 'mon-set-row-ok' : 'mon-set-row-warn'}>
                      {credentialReady(channel) ? '凭据已配置' : '未填凭据，推送会失败'}
                    </span>
                    {channel.chatId && <span>{`chat ${channel.chatId}`}</span>}
                  </div>
                </div>
                <div className="mon-set-row-actions">
                  <button type="button" className="btn btn-sm" onClick={() => setDraft(draftFromChannel(channel))}>{'编辑'}</button>
                  <button type="button" className="btn btn-sm" disabled={busy === `test:${channel.id}`} onClick={() => onTestChannel(channel.id)}>{'测试'}</button>
                  <button type="button" className="btn btn-sm" disabled={busy === `del:${channel.id}`} onClick={() => onDeleteChannel(channel.id)}>{'删除'}</button>
                </div>
              </div>
            ))}
          </div>
        )}
      </section>

      <section className="card mon-set-card">
        <header className="mon-set-head">
          <div className="mon-set-head-text">
            <h2 className="mon-card-title">{editing ? '编辑渠道' : '新增渠道'}</h2>
            <p className="mon-set-desc">
              {editing
                ? `正在编辑「${draft.name || '未命名渠道'}」；凭据留空即保留原值。`
                : '填好名称与凭据即可创建；类型创建后不可更改。'}
            </p>
          </div>
        </header>
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
          {editing && <button type="button" className="btn btn-sm" onClick={() => setDraft(emptyDraft())}>{'取消编辑'}</button>}
        </div>
      </section>

      {form && (
        <section className="card mon-set-card">
          <header className="mon-set-head">
            <div className="mon-set-head-text">
              <h2 className="mon-card-title">{'投递与静默'}</h2>
              <p className="mon-set-desc">
                {'聚合间隔决定同类告警多久合并成一条摘要；静默时段只顺延 P1，P0 永远即时送达。'}
              </p>
            </div>
          </header>
          <div className="monitoring-cols">
            <label className="field">
              <span>{'聚合间隔（分钟）'}</span>
              <input
                type="number"
                min={5}
                max={120}
                value={form.digestMinutes}
                onChange={(event) => setForm({ ...form, digestMinutes: Number(event.target.value) })}
              />
            </label>
            <label className="field">
              <span>{'时区'}</span>
              <input value={form.timezone} placeholder="Asia/Shanghai" onChange={(event) => setForm({ ...form, timezone: event.target.value })} />
            </label>
            <label className="field">
              <span>{'静默开始（HH:MM）'}</span>
              <input value={form.quietStart} onChange={(event) => setForm({ ...form, quietStart: event.target.value })} />
            </label>
            <label className="field">
              <span>{'静默结束（HH:MM）'}</span>
              <input value={form.quietEnd} onChange={(event) => setForm({ ...form, quietEnd: event.target.value })} />
            </label>
            <label className="field">
              <span>{'控制台地址（消息里的深链前缀）'}</span>
              <input value={form.baseUrl} placeholder="https://…" onChange={(event) => setForm({ ...form, baseUrl: event.target.value })} />
            </label>
            <div className="field mon-set-switch">
              <span>{'静默与恢复'}</span>
              <div className="mon-set-switches">
                <Switch checked={form.quietEnabled} label="启用静默时段" onChange={(value) => setForm({ ...form, quietEnabled: value })} />
                <Switch checked={form.recoveryNotify} label="发送恢复通知" onChange={(value) => setForm({ ...form, recoveryNotify: value })} />
              </div>
            </div>
          </div>
          <p className="mon-set-foot">
            {'静默时段只顺延 P1，P0 永远即时；当前逐条即时发送，聚合摘要随后续阶段上线。'}
          </p>
          <div className="mon-set-form-actions">
            <button type="button" className="btn btn-sm primary" disabled={busy === 'dispatch'} onClick={() => onUpdateDispatch(form)}>
              {'保存投递设置'}
            </button>
          </div>
        </section>
      )}

      <ProbeSettingsPanel
        config={probeConfig}
        timezone={form?.timezone || dispatch?.timezone || 'Asia/Shanghai'}
        draft={probeDraft}
        onChange={onProbeDraft}
        onSubmit={() => probeDraft && onSaveProbeConfig(probeDraft)}
        busy={probeBusy}
        error={probeError}
        notice={probeNotice}
      />
    </div>
  );
}
