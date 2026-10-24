import { useEffect, useState } from 'react';
import { Switch } from '../components/ui';
import type { MonitorChannelInput, MonitorChannelView, MonitorDispatchConfig, MonitorSeverity } from './types';
import { SEVERITY_TEXT } from './types';

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

  return (
    <div className="card panel">
      <div className="panel-header">
        <div>
          <h2 className="panel-title">{'通知渠道与投递'}</h2>
          <p className="panel-desc">
            {'webhook、签名密钥与 bot token 只写不读：留空表示保留服务端已有值。规则阈值编辑属阶段 4，这里不提供。'}
          </p>
        </div>
      </div>

      {error && <div className="metric-value err">{error}</div>}
      {notice && (
        <div className="metric">
          <div className="metric-label">{'最近操作'}</div>
          <div className="metric-note">{notice}</div>
          <button className="btn btn-sm" onClick={onDismissNotice}>{'收起'}</button>
        </div>
      )}

      <div className="card">
        <div className="panel-title">{`渠道（${channels.length}）`}</div>
        {channels.length === 0 && <div className="muted-text">{'还没有渠道，告警只记录不推送。'}</div>}
        {channels.map((channel) => (
          <div className="log-row" key={channel.id}>
            <div className="mrow mrow-channel">
              <span className="m-clip" style={{ fontWeight: 600 }}>{channel.name}</span>
              <span className="badge slate">{CHANNEL_TYPES.find((item) => item.value === channel.type)?.label ?? channel.type}</span>
              <span className={`badge ${channel.minSeverity === 'p0' ? 'red' : channel.minSeverity === 'p1' ? 'amber' : 'blue'}`}>
                {`≥ ${channel.minSeverity.toUpperCase()}`}
              </span>
              <span className="muted-text">
                {channel.webhookSet || channel.secretSet || channel.tokenSet ? '凭据已配置' : '未填凭据'}
                {channel.chatId ? ` · chat ${channel.chatId}` : ''}
              </span>
              <button className="btn btn-sm" onClick={() => setDraft(draftFromChannel(channel))}>{'编辑'}</button>
              <button className="btn btn-sm" disabled={busy === `test:${channel.id}`} onClick={() => onTestChannel(channel.id)}>{'测试'}</button>
              <button className="btn btn-sm" disabled={busy === `del:${channel.id}`} onClick={() => onDeleteChannel(channel.id)}>{'删除'}</button>
            </div>
          </div>
        ))}
      </div>

      <div className="card">
        <div className="panel-title">{editing ? `编辑渠道：${draft.name}` : '新增渠道'}</div>
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
          <div className="field">
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

        <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
          <button className="btn btn-sm primary" disabled={busy.startsWith('channel')} onClick={submitChannel}>
            {editing ? '保存修改' : '创建渠道'}
          </button>
          {editing && <button className="btn btn-sm" onClick={() => setDraft(emptyDraft())}>{'取消编辑'}</button>}
        </div>
      </div>

      {form && (
        <div className="card">
          <div className="panel-title">{'投递与静默'}</div>
          <div className="monitoring-cols">
            <label className="field">
              <span>{'聚合间隔（分钟）'}</span>
              <input
                type="number"
                min={1}
                max={1440}
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
            <div className="field" style={{ display: 'flex', gap: 16, alignItems: 'center' }}>
              <Switch checked={form.quietEnabled} label="启用静默时段" onChange={(value) => setForm({ ...form, quietEnabled: value })} />
              <Switch checked={form.recoveryNotify} label="发送恢复通知" onChange={(value) => setForm({ ...form, recoveryNotify: value })} />
            </div>
          </div>
          <div className="metric-note">
            {'静默时段只顺延 P1，P0 永远即时；阶段 1–3 逐条即时发送，聚合摘要随阶段 4 上线。'}
          </div>
          <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
            <button className="btn btn-sm primary" disabled={busy === 'dispatch'} onClick={() => onUpdateDispatch(form)}>
              {'保存投递设置'}
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
