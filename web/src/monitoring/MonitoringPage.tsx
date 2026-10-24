import { useCallback, useEffect, useRef, useState } from 'react';
import {
  MonitorAPIError,
  acknowledgeAlert,
  createChannel,
  deleteChannel,
  fetchAccounts,
  fetchAlert,
  fetchAlerts,
  fetchChannels,
  fetchDispatch,
  fetchEvents,
  fetchOverview,
  ignoreAlert,
  renewAccount,
  refreshQuota,
  retryPush,
  testChannel,
  updateChannel,
  updateDispatch,
} from './api';
import type {
  MonitorAccount,
  MonitorAlert,
  MonitorAlertDetail,
  MonitorChannelInput,
  MonitorChannelView,
  MonitorDispatchConfig,
  MonitorEvent,
  MonitorOverview,
  MonitorSeverity,
} from './types';
import { AccountsTab } from './AccountsTab';
import { AlertsTab } from './AlertsTab';
import { OverviewTab } from './OverviewTab';
import { SettingsTab } from './SettingsTab';

type TabID = 'overview' | 'accounts' | 'alerts' | 'settings';

const TABS: Array<{ id: TabID; label: string }> = [
  { id: 'overview', label: '概览' },
  { id: 'accounts', label: '账号' },
  { id: 'alerts', label: '告警' },
  { id: 'settings', label: '设置' },
];

const POLL_MS = 15_000;
const EVENT_PAGE = 30;
const ALERT_PAGE = 50;

function tabFromLocation(): TabID {
  const value = new URLSearchParams(window.location.search).get('tab');
  const found = TABS.find((item) => item.id === value);
  return found ? found.id : 'overview';
}

function paramFromLocation(key: string): string {
  return new URLSearchParams(window.location.search).get(key) || '';
}

export interface MonitoringPageProps {
  /** 只用于品牌图标：外层把控制台已有的 providerIcon 结果传进来。 */
  iconOf?: (providerId: string) => string | undefined;
  /** 「查看请求日志」：外层把 API 日志页按该 provider 过滤并切过去。 */
  onOpenAccountLogs?: (providerId: string) => void;
}

export function MonitoringPage({ iconOf, onOpenAccountLogs }: MonitoringPageProps) {
  const [tab, setTab] = useState<TabID>(() => tabFromLocation());
  const [overview, setOverview] = useState<MonitorOverview | null>(null);
  const [accounts, setAccounts] = useState<MonitorAccount[]>([]);
  const [alerts, setAlerts] = useState<MonitorAlert[]>([]);
  const [alertTotal, setAlertTotal] = useState(0);
  const [events, setEvents] = useState<MonitorEvent[]>([]);
  const [eventTotal, setEventTotal] = useState(0);
  const [channels, setChannels] = useState<MonitorChannelView[]>([]);
  const [dispatch, setDispatch] = useState<MonitorDispatchConfig | null>(null);
  const [alertDetail, setAlertDetail] = useState<MonitorAlertDetail | null>(null);
  const [selectedAccount, setSelectedAccount] = useState<string>(() => paramFromLocation('account'));
  const [statusFilter, setStatusFilter] = useState('firing');
  const [severityFilter, setSeverityFilter] = useState<MonitorSeverity | ''>('');
  const [kindFilter, setKindFilter] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [settingsError, setSettingsError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState('');

  const generation = useRef(0);
  const timezone = dispatch?.timezone || 'Asia/Shanghai';

  const goTab = useCallback((next: TabID, params: Record<string, string> = {}) => {
    setTab(next);
    const search = new URLSearchParams();
    search.set('tab', next);
    for (const [key, value] of Object.entries(params)) {
      if (value) search.set(key, value);
    }
    window.history.pushState({}, '', `${window.location.pathname}?${search.toString()}`);
  }, []);

  // 深链：首屏与浏览器前进/后退都还原到同一 tab 与对象。
  useEffect(() => {
    const restore = () => {
      setTab(tabFromLocation());
      setSelectedAccount(paramFromLocation('account'));
    };
    window.addEventListener('popstate', restore);
    return () => window.removeEventListener('popstate', restore);
  }, []);

  const loadVisible = useCallback(async () => {
    if (document.hidden) return;
    const mine = ++generation.current;
    setLoading(true);
    try {
      const [nextOverview, nextAccounts, nextAlerts, nextEvents, nextChannels, nextDispatch] = await Promise.all([
        fetchOverview(),
        fetchAccounts(),
        fetchAlerts({
          status: statusFilter || undefined,
          severity: severityFilter || undefined,
          range: '7d',
          pageSize: ALERT_PAGE,
        }),
        fetchEvents({
          kind: kindFilter || undefined,
          severity: severityFilter || undefined,
          range: '24h',
          pageSize: EVENT_PAGE,
        }),
        // 告警详情里的「重试推送」要选渠道，所以告警页也要拿一次渠道（脱敏视图）。
        tab === 'settings' || tab === 'alerts' ? fetchChannels() : Promise.resolve(null),
        dispatch ? Promise.resolve(null) : fetchDispatch(),
      ]);
      if (mine !== generation.current) return;
      setOverview(nextOverview);
      setAccounts(nextAccounts.accounts);
      setAlerts(nextAlerts.items);
      setAlertTotal(nextAlerts.total);
      setEvents(nextEvents.items);
      setEventTotal(nextEvents.total);
      if (nextChannels) setChannels(nextChannels);
      if (nextDispatch) setDispatch(nextDispatch);
      setError('');
    } catch (reason) {
      if (mine !== generation.current) return;
      setError(reason instanceof MonitorAPIError ? reason.message : '监控数据读取失败');
    } finally {
      if (mine === generation.current) setLoading(false);
    }
  }, [dispatch, kindFilter, severityFilter, statusFilter, tab]);

  useEffect(() => {
    void loadVisible();
  }, [loadVisible]);

  // 15 秒轮询；页面隐藏时不发请求，切回前台立即补一次。
  useEffect(() => {
    const timer = window.setInterval(() => void loadVisible(), POLL_MS);
    const onVisible = () => {
      if (!document.hidden) void loadVisible();
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [loadVisible]);

  /** 统一动作包装：busy 标记、错误文案、成功后刷新（保留旧数据不闪）。 */
  const run = useCallback(
    async (
      key: string,
      action: () => Promise<unknown>,
      successText?: string,
      routeError?: (message: string) => void,
    ) => {
      setBusy(key);
      try {
        await action();
        if (successText) setNotice(successText);
        await loadVisible();
      } catch (reason) {
        const message = reason instanceof MonitorAPIError ? reason.message : '操作失败，请稍后重试';
        if (routeError) routeError(message);
        else setError(message);
      } finally {
        setBusy('');
      }
    },
    [loadVisible],
  );

  const alertCountOf = useCallback(
    (providerId: string) => alerts.filter((alert) => alert.targetId === providerId && alert.status === 'firing').length,
    [alerts],
  );

  const openAccount = useCallback(
    (providerId: string) => {
      setSelectedAccount(providerId);
      goTab('accounts', { account: providerId });
    },
    [goTab],
  );

  const openAlert = useCallback(
    (id: number) => {
      goTab('alerts', { alert: String(id) });
      void fetchAlert(id).then(setAlertDetail).catch(() => setError('告警详情读取失败'));
    },
    [goTab],
  );

  // 深链 ?alert= 直接打开详情（从飞书/钉钉卡片的链接进来就是这个形态）。
  useEffect(() => {
    const raw = paramFromLocation('alert');
    if (!raw || tab !== 'alerts') return;
    const id = Number(raw);
    if (!Number.isFinite(id) || id <= 0) return;
    void fetchAlert(id).then(setAlertDetail).catch(() => setError('告警详情读取失败'));
  }, [tab]);

  const loadMoreEvents = () => {
    void fetchEvents({
      kind: kindFilter || undefined,
      severity: severityFilter || undefined,
      range: '24h',
      page: Math.floor(events.length / EVENT_PAGE) + 1,
      pageSize: EVENT_PAGE,
    })
      .then((page) => {
        setEvents((current) => mergeByID(current, page.items));
        setEventTotal(page.total);
      })
      .catch(() => setError('事件加载失败'));
  };

  const loadMoreAlerts = () => {
    void fetchAlerts({
      status: statusFilter || undefined,
      severity: severityFilter || undefined,
      range: '7d',
      page: Math.floor(alerts.length / ALERT_PAGE) + 1,
      pageSize: ALERT_PAGE,
    })
      .then((page) => {
        setAlerts((current) => mergeByID(current, page.items));
        setAlertTotal(page.total);
      })
      .catch(() => setError('告警加载失败'));
  };

  return (
    <div>
      <div style={{ display: 'flex', gap: 8, alignItems: 'center', marginBottom: 12, flexWrap: 'wrap' }}>
        {TABS.map((item) => (
          <button
            key={item.id}
            className={`status-pill-btn ${tab === item.id ? 'active' : ''}`}
            aria-current={tab === item.id ? 'page' : undefined}
            onClick={() => goTab(item.id, item.id === 'accounts' && selectedAccount ? { account: selectedAccount } : {})}
          >
            {item.label}
          </button>
        ))}
        {loading && <span className="muted-text" style={{ marginLeft: 'auto' }}>{'刷新中…'}</span>}
      </div>

      {tab === 'overview' && (
        <OverviewTab
          overview={overview}
          accounts={accounts}
          alerts={alerts}
          events={events}
          eventTotal={eventTotal}
          kindFilter={kindFilter}
          severityFilter={severityFilter}
          timezone={timezone}
          iconOf={iconOf}
          loading={loading}
          error={error}
          onKindFilter={setKindFilter}
          onSeverityFilter={setSeverityFilter}
          onLoadMoreEvents={loadMoreEvents}
          onOpenAccount={openAccount}
          onOpenAlert={openAlert}
          onRetry={() => void loadVisible()}
        />
      )}

      {tab === 'accounts' && (
        <AccountsTab
          accounts={accounts}
          timezone={timezone}
          iconOf={iconOf}
          alertCountOf={alertCountOf}
          busy={busy}
          error={error}
          selectedId={selectedAccount}
          onSelect={openAccount}
          onCloseDrawer={() => {
            setSelectedAccount('');
            goTab('accounts', {});
          }}
          onRenew={(providerId) => {
            void run(`renew:${providerId}`, () => renewAccount(providerId), undefined, setSettingsError);
          }}
          onRefreshQuota={(providerId) => {
            void run(
              `quota:${providerId}`,
              async () => {
                const result = await refreshQuota(providerId);
                if (!result.ok) throw new MonitorAPIError(502, `额度读取失败：${result.errorKind ?? 'unknown'}`);
              },
              undefined,
              setSettingsError,
            );
          }}
          onOpenAlerts={(providerId) => {
            goTab('alerts', { target: providerId });
            setSelectedAccount('');
            void fetchAlerts({ targetId: providerId, status: statusFilter || undefined, range: '7d' })
              .then((page) => {
                setAlerts(page.items);
                setAlertTotal(page.total);
              })
              .catch(() => setError('告警读取失败'));
          }}
          onOpenLogs={onOpenAccountLogs ?? (() => undefined)}
        />
      )}

      {tab === 'alerts' && (
        <AlertsTab
          alerts={alerts}
          total={alertTotal}
          statusFilter={statusFilter}
          severityFilter={severityFilter}
          timezone={timezone}
          channels={channels}
          detail={alertDetail}
          busy={busy}
          error={error}
          onStatusFilter={(value) => {
            setStatusFilter(value);
            setAlertDetail(null);
          }}
          onSeverityFilter={setSeverityFilter}
          onOpen={openAlert}
          onClose={() => {
            setAlertDetail(null);
            goTab('alerts', {});
          }}
          onAck={(id) => {
            void run(`ack:${id}`, async () => setAlertDetail(await acknowledgeAlert(id)));
          }}
          onIgnore={(id) => {
            void run(`ignore:${id}`, async () => setAlertDetail(await ignoreAlert(id)));
          }}
          onRetryPush={(eventId, channelId) => {
            void run(`retry:${eventId}`, () => retryPush(eventId, channelId), '重试推送已提交');
          }}
          onOpenAccount={openAccount}
          onLoadMore={loadMoreAlerts}
        />
      )}

      {tab === 'settings' && (
        <SettingsTab
          channels={channels}
          dispatch={dispatch}
          busy={busy}
          error={settingsError}
          notice={notice}
          onCreateChannel={(input: MonitorChannelInput) => {
            void run('channel:create', () => createChannel(input), '渠道已创建', setSettingsError);
          }}
          onUpdateChannel={(id, patch) => {
            void run('channel:update', () => updateChannel(id, patch), '渠道已保存', setSettingsError);
          }}
          onDeleteChannel={(id) => {
            void run(`del:${id}`, () => deleteChannel(id), '渠道已删除', setSettingsError);
          }}
          onTestChannel={(id) => {
            void run(
              `test:${id}`,
              async () => {
                const result = await testChannel(id);
                if (!result.ok) throw new MonitorAPIError(502, result.error || '渠道测试失败');
              },
              undefined,
              setSettingsError,
            );
          }}
          onUpdateDispatch={(patch) => {
            void run('dispatch', async () => setDispatch(await updateDispatch(patch)), '投递设置已保存', setSettingsError);
          }}
          onDismissNotice={() => setNotice('')}
        />
      )}
    </div>
  );
}

/** 分页合并：轮询可能插进新行，按 id 去重才不会重复渲染。 */
function mergeByID<T extends { id: number }>(current: T[], incoming: T[]): T[] {
  const merged = [...current];
  for (const item of incoming) {
    if (!merged.some((existing) => existing.id === item.id)) merged.push(item);
  }
  return merged;
}
