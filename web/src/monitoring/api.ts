import { API_BASE, fetchWithTimeout } from '../lib';
import type {
  MonitorAccount,
  MonitorAlert,
  MonitorAlertDetail,
  MonitorChannelInput,
  MonitorChannelTestResult,
  MonitorChannelView,
  MonitorDispatchConfig,
  MonitorEvent,
  MonitorOverview,
  MonitorPage,
  MonitorQuotaResult,
  MonitorRenewalResult,
  MonitorSeverity,
} from './types';

/**
 * 监控中心接口封装（阶段 1 已上线的 `/__monitor/*` 子集）。
 *
 * 两条来自现有控制台教训的约定：
 *
 * 1. 只按 content-type 分流，不单看状态码。会话过期或反代超时时响应体是 HTML，
 *    无条件 `.json()` 会抛 `Unexpected token '<'`，把真实原因盖住。
 * 2. 失败也要读 body。后端把上游额度失败映射成 502 且 body 里带 `{ok:false,errorKind}`，
 *    那是有信息的响应，不能一概当「网络不通」让用户白重试。
 */

export class MonitorAPIError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'MonitorAPIError';
    this.status = status;
  }
}

type JSONBody = Record<string, unknown>;

function messageOf(status: number, body: JSONBody | null): string {
  const nested = (body?.error as JSONBody | undefined)?.message;
  if (typeof body?.message === 'string' && body.message) return body.message;
  if (typeof nested === 'string' && nested) return nested;
  return `请求失败（HTTP ${status}）`;
}

async function parse(response: Response): Promise<JSONBody | null> {
  const type = response.headers.get('content-type') ?? '';
  if (!type.includes('json')) return null;
  return (await response.json()) as JSONBody;
}

function guard(status: number, body: JSONBody | null): string {
  if (status === 401 || status === 403) return '需要管理员权限，请重新登录控制台';
  return messageOf(status, body);
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const response = await fetchWithTimeout(`${API_BASE}/__monitor${path}`, {
    credentials: 'same-origin',
    ...init,
    headers: { 'Content-Type': 'application/json', ...(init.headers ?? {}) },
  });
  const body = await parse(response);
  if (!response.ok) throw new MonitorAPIError(response.status, guard(response.status, body));
  if (body === null) {
    if (response.status === 204) return undefined as T;
    throw new MonitorAPIError(response.status, `监控服务返回了非 JSON 响应（HTTP ${response.status}）`);
  }
  return body as unknown as T;
}

function query(params: Record<string, string | number | boolean | undefined | null>): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '') continue;
    search.set(key, String(value));
  }
  const raw = search.toString();
  return raw ? `?${raw}` : '';
}

export interface MonitorEventFilters {
  kind?: string;
  severity?: MonitorSeverity;
  targetId?: string;
  range?: '24h' | '7d' | '30d';
  page?: number;
  pageSize?: number;
}

export interface MonitorAlertFilters {
  status?: string;
  disposition?: string;
  severity?: MonitorSeverity;
  rule?: string;
  targetId?: string;
  range?: '24h' | '7d' | '30d';
  page?: number;
  pageSize?: number;
}

export function fetchOverview(): Promise<MonitorOverview> {
  return request<MonitorOverview>('/overview');
}

export function fetchAccounts(): Promise<{ accounts: MonitorAccount[]; total: number }> {
  return request<{ accounts: MonitorAccount[]; total: number }>('/accounts');
}

export function fetchAccount(providerId: string): Promise<MonitorAccount> {
  return request<MonitorAccount>(`/accounts/${encodeURIComponent(providerId)}`);
}

/**
 * 强制续期。`ok:true` 且 `renewed:false` 表示未到续期窗口——不是失败：
 * 后端不会为了「立即」去轮换一枚还能用的 refresh token（并发轮换会让后到的
 * 那枚被判无效，严重的会作废整条令牌链）。
 */
export function renewAccount(providerId: string): Promise<MonitorRenewalResult> {
  return request<MonitorRenewalResult>(`/accounts/${encodeURIComponent(providerId)}/refresh`, { method: 'POST' });
}

/** 额度刷新：上游拒绝时后端回 502 + `{ok:false,errorKind}`，这个分类要留给界面显示。 */
export async function refreshQuota(providerId: string): Promise<MonitorQuotaResult> {
  const response = await fetchWithTimeout(`${API_BASE}/__monitor/accounts/${encodeURIComponent(providerId)}/quota/refresh`, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
  });
  const body = (await parse(response)) as MonitorQuotaResult | null;
  if (response.status === 401 || response.status === 403) {
    throw new MonitorAPIError(response.status, guard(response.status, body as unknown as JSONBody));
  }
  return { ok: response.ok && body?.ok !== false, quota: body?.quota, errorKind: body?.errorKind };
}

export function fetchEvents(filters: MonitorEventFilters = {}): Promise<MonitorPage<MonitorEvent>> {
  return request<MonitorPage<MonitorEvent>>(`/events${query({ ...filters })}`);
}

export function fetchAlerts(filters: MonitorAlertFilters = {}): Promise<MonitorPage<MonitorAlert>> {
  return request<MonitorPage<MonitorAlert>>(`/alerts${query({ ...filters })}`);
}

export function fetchAlert(id: number): Promise<MonitorAlertDetail> {
  return request<MonitorAlertDetail>(`/alerts/${id}`);
}

export function acknowledgeAlert(id: number): Promise<MonitorAlertDetail> {
  return request<MonitorAlertDetail>(`/alerts/${id}/ack`, { method: 'POST' });
}

export function ignoreAlert(id: number): Promise<MonitorAlertDetail> {
  return request<MonitorAlertDetail>(`/alerts/${id}/ignore`, { method: 'POST' });
}

/** 按渠道重试某条事件的推送；后端会把结果折回事件的聚合状态。 */
export function retryPush(eventId: number, channelId: string): Promise<MonitorChannelTestResult> {
  return request<MonitorChannelTestResult>(`/events/${eventId}/retry-push`, {
    method: 'POST',
    body: JSON.stringify({ channelId }),
  });
}

export function fetchChannels(): Promise<MonitorChannelView[]> {
  return request<MonitorChannelView[]>('/channels');
}

export function createChannel(input: MonitorChannelInput): Promise<MonitorChannelView> {
  return request<MonitorChannelView>('/channels', { method: 'POST', body: JSON.stringify(input) });
}

/** 编辑：id 与 type 不可改；secret 类字段留空 = 保留服务端原值。 */
export function updateChannel(id: string, input: Partial<MonitorChannelInput>): Promise<MonitorChannelView> {
  return request<MonitorChannelView>(`/channels/${encodeURIComponent(id)}`, { method: 'PUT', body: JSON.stringify(input) });
}

export function deleteChannel(id: string): Promise<MonitorChannelView[]> {
  return request<MonitorChannelView[]>(`/channels/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

export function testChannel(id: string): Promise<MonitorChannelTestResult> {
  return request<MonitorChannelTestResult>(`/channels/${encodeURIComponent(id)}/test`, { method: 'POST' });
}

export function fetchDispatch(): Promise<MonitorDispatchConfig> {
  return request<MonitorDispatchConfig>('/dispatch');
}

/** 局部 PATCH：没提交的字段后端保留原值，不会抹回默认。 */
export function updateDispatch(patch: Partial<MonitorDispatchConfig>): Promise<MonitorDispatchConfig> {
  return request<MonitorDispatchConfig>('/dispatch', { method: 'PUT', body: JSON.stringify(patch) });
}
