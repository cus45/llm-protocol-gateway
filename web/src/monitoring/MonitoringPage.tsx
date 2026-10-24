import {useCallback, useEffect, useRef, useState} from 'react';
import {
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
    fetchProbeConfig,
    fetchProbes,
    ignoreAlert,
    MonitorAPIError,
    refreshQuota,
    renewAccount,
    retryPush,
    runProbe,
    saveProbeConfig,
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
    MonitorProbeAccount,
    MonitorProbeConfig,
    MonitorProbeRun,
    MonitorProbeRunResult,
    MonitorSeverity,
} from './types';
import {AccountsTab} from './AccountsTab';
import {AlertsTab} from './AlertsTab';
import {OverviewTab} from './OverviewTab';
import {PROBE_KIND_LIGHT, ProbesTab} from './ProbesTab';
import {SettingsTab} from './SettingsTab';

type TabID = 'overview' | 'accounts' | 'alerts' | 'probes' | 'settings';

const TABS: Array<{ id: TabID; label: string }> = [
  { id: 'overview', label: '概览' },
  { id: 'accounts', label: '账号' },
  { id: 'alerts', label: '告警' },
  { id: 'probes', label: '探测' },
  { id: 'settings', label: '设置' },
];

const POLL_MS = 15_000;
/**
 * 单发轻探测的后端上限是 30 秒，期间「正在探测」的按钮要一直亮着；
 * 这个节拍只比轮询密，别让它比探测本身先停。
 */
const PROBE_POLL_MS = 3_000;
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
  const [probeAccounts, setProbeAccounts] = useState<MonitorProbeAccount[]>([]);
  const [probeRuns, setProbeRuns] = useState<MonitorProbeRun[]>([]);
  const [probeConfig, setProbeConfig] = useState<MonitorProbeConfig | null>(null);
  const [probeRunBusy, setProbeRunBusy] = useState('');
  /*
   * 正在跑的探测用 ref 记：连续点两下时，第一发的 finally 不能提前清掉第二发的 busy，
   * 否则按钮在探测途中就恢复成可点，让人以为没生效再点一次。
   */
  const probeRunSeq = useRef(0);
  // 弹层里的探测结果按 providerId 存：轮询换新数据时结果不跟着丢，新的探测覆盖旧的。
  const [probeResult, setProbeResult] = useState<{ providerId: string; data: MonitorProbeRunResult } | null>(null);
  /*
   * 配置表单的编辑草稿。只在「首次拿到后端配置」与「保存成功」时重置，
   * 之后的 15 秒轮询不覆盖未保存的编辑；null = 跟随后端值。
   */
  const [probeDraft, setProbeDraft] = useState<MonitorProbeConfig | null>(null);
  // 配置表单只在「后端配置变了（首次加载/保存成功）」时重置，定时轮询不覆盖未保存的编辑。
  const [probeSettingsError, setProbeSettingsError] = useState('');
  const [probeSettingsNotice, setProbeSettingsNotice] = useState('');
  const [alertDetail, setAlertDetail] = useState<MonitorAlertDetail | null>(null);
  const [selectedAccount, setSelectedAccount] = useState<string>(() => paramFromLocation('account'));
  // 探测页的记录筛选直接取自深链 ?account=，与 popstate 还原同一个来源，不另存一份状态。
  const probeFilter = tab === 'probes' ? selectedAccount : '';
  const [statusFilter, setStatusFilter] = useState('firing');
  const [severityFilter, setSeverityFilter] = useState<MonitorSeverity | ''>('');
  const [kindFilter, setKindFilter] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [settingsError, setSettingsError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState('');
  // 抽屉里的动作要就地给反馈：账号页在 Modal 打开时，面板顶部的错误行会被抽屉挡住。
  const [drawerNote, setDrawerNote] = useState('');
  const [drawerError, setDrawerError] = useState('');

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
    const mine = ++generation.current;
    setLoading(true);
    try {
      // 探测接口失败不能拖垮整页刷新（探测页照常显示账号与告警，只是缺记录），所以单独兜底。
      const probesPromise = fetchProbes(tab === 'probes' ? probeFilter : undefined).catch((reason: unknown) => {
        if (reason instanceof MonitorAPIError && (reason.status === 401 || reason.status === 403)) throw reason;
        return null;
      });
      const [nextOverview, nextAccounts, nextAlerts, nextEvents, nextChannels, nextDispatch, nextProbes] = await Promise.all([
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
        probesPromise,
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
      if (nextProbes) {
        setProbeAccounts(nextProbes.accounts);
        setProbeRuns(nextProbes.runs);
        setProbeConfig(nextProbes.config);
      }
      setError('');
    } catch (reason) {
      if (mine !== generation.current) return;
      setError(reason instanceof MonitorAPIError ? reason.message : '监控数据读取失败');
    } finally {
      if (mine === generation.current) setLoading(false);
    }
  }, [dispatch, kindFilter, probeFilter, severityFilter, statusFilter, tab]);

  useEffect(() => {
    void loadVisible();
  }, [loadVisible]);

  // 探测配置：只在设置页读一次 `/probe-config`（写入要走 PUT）。失败不拖垮整页：
  // 设置页的其它卡片照常可用，探测配置卡片不出现，等下一轮再试。
  useEffect(() => {
    if (tab !== 'settings' || probeConfig) return;
    let cancelled = false;
    void fetchProbeConfig()
      .then((config) => {
        if (!cancelled) setProbeConfig(config);
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, [tab, probeConfig]);

  // 15 秒轮询；只有定时任务在页面隐藏时跳过，手动/操作后的刷新必须真的取数。
  useEffect(() => {
    const timer = window.setInterval(() => {
      if (!document.hidden) void loadVisible();
    }, POLL_MS);
    const onVisible = () => {
      if (!document.hidden) void loadVisible();
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [loadVisible]);

  /** 探测中每 3 秒刷新一次现状（账号行显示「探测中…」时结果来得比 15 秒轮询快）。 */
  // 探测不走 run()，busy 里永远不会出现 probe: 前缀，所以只看 probeRunBusy（审查 O1）。
  const probeBusyActive = !!probeRunBusy;
  useEffect(() => {
    if (!probeBusyActive) return;
    const timer = window.setInterval(() => {
      if (tab === 'probes' || tab === 'accounts') void loadVisible();
    }, PROBE_POLL_MS);
    return () => window.clearInterval(timer);
  }, [probeBusyActive, loadVisible, tab]);

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

  /**
   * 手动探测：两个入口（探测页行内、账号抽屉）共用。结果返回给调用处就地显示，
   * 这里只管 busy 与刷新；返回 null 表示请求本身失败（网络/权限），由调用处给文案。
   */
  const runProbeFor = useCallback(
    async (providerId: string): Promise<MonitorProbeRunResult | null> => {
      const seq = ++probeRunSeq.current;
      setProbeRunBusy(`probe:${providerId}`);
      // 上一次的结果先收起来：新一轮探测还在跑时，抽屉里不该还挂着上一轮「通过」。
      setProbeResult((current) => (current?.providerId === providerId ? null : current));
      try {
        const result = await runProbe(providerId, PROBE_KIND_LIGHT);
        setProbeResult({ providerId, data: result });
        await loadVisible();
        return result;
      } catch (reason) {
        const message = reason instanceof MonitorAPIError ? reason.message : '探测请求失败，请稍后重试';
        setError(message);
        return null;
      } finally {
        if (seq === probeRunSeq.current) setProbeRunBusy('');
      }
    },
    [loadVisible],
  );

  /** 探测记录按账号筛选：同一个 providerId 同时决定深链 ?account=，刷新能还原。 */
  const filterProbes = useCallback(
    (providerId: string) => {
      setSelectedAccount(providerId);
      goTab('probes', { account: providerId });
    },
    [goTab],
  );

  const openAccount = useCallback(
    (providerId: string) => {
      setSelectedAccount(providerId);
      setDrawerNote('');
      setDrawerError('');
      // 抽屉里的结果是「刚刚在抽屉里点出来的」，不能把探测页或别的账号的结果带进来。
      setProbeResult(null);
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
            onClick={() =>
              goTab(item.id, (item.id === 'accounts' || item.id === 'probes') && selectedAccount ? { account: selectedAccount } : {})
            }
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
          note={drawerNote}
          actionError={drawerError}
          probeOf={(providerId) => probeAccounts.find((item) => item.providerId === providerId)}
          probeBusy={probeRunBusy}
          probeResult={probeResult && probeResult.providerId === selectedAccount ? probeResult.data : null}
          onRunProbe={(providerId) => {
            setDrawerError('');
            // 真实原因已由 runProbeFor 写到面板顶部错误行，这里只补一句「在哪没成」。
            void runProbeFor(providerId).then((result) => {
              if (!result) setDrawerError('探测没有发出：账号或上游已不可用，原因见页面顶部提示');
            });
          }}
          onCloseDrawer={() => {
            setSelectedAccount('');
            setDrawerNote('');
            setDrawerError('');
            // 关掉就作废旧结果：重开同一账号时不该「复活」上一次的探测结论。
            setProbeResult(null);
            goTab('accounts', {});
          }}
          onRenew={(providerId) => {
            void run(
              `renew:${providerId}`,
              async () => {
                const result = await renewAccount(providerId);
                if (!result.ok) throw new MonitorAPIError(502, `续期失败：${result.errorKind ?? 'unknown'}`);
                setDrawerNote(result.renewed ? '凭证已轮换' : '未到续期窗口，凭证保持不变');
              },
              undefined,
              setDrawerError,
            );
          }}
          onRefreshQuota={(providerId) => {
            void run(
              `quota:${providerId}`,
              async () => {
                const result = await refreshQuota(providerId);
                if (!result.ok) throw new MonitorAPIError(502, `额度读取失败：${result.errorKind ?? 'unknown'}`);
                setDrawerNote('额度已刷新');
              },
              undefined,
              setDrawerError,
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

      {tab === 'probes' && (
        <ProbesTab
          probeAccounts={probeAccounts}
          probeRuns={probeRuns}
          providerFilter={probeFilter}
          timezone={timezone}
          busy={probeRunBusy}
          error={error}
          iconOf={iconOf}
          probeResult={
            probeResult && probeAccounts.some((item) => item.providerId === probeResult.providerId) ? probeResult : null
          }
          onDismissResult={() => setProbeResult(null)}
          onRunProbe={runProbeFor}
          onRefreshRuns={() => void loadVisible()}
          onFilter={filterProbes}
          onOpenAccount={openAccount}
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
              '测试消息已发送，未收到请检查渠道关键词或签名',
              setSettingsError,
            );
          }}
          onUpdateDispatch={(patch) => {
            void run('dispatch', async () => setDispatch(await updateDispatch(patch)), '投递设置已保存', setSettingsError);
          }}
          onDismissNotice={() => setNotice('')}
          probeConfig={probeConfig}
          probeDraft={probeDraft}
          probeBusy={busy === 'probe-config'}
          probeError={probeSettingsError}
          probeNotice={probeSettingsNotice}
          onProbeDraft={setProbeDraft}
          onSaveProbeConfig={(config) => {
            setProbeSettingsError('');
            setProbeSettingsNotice('');
            void run(
              'probe-config',
              async () => {
                const saved = await saveProbeConfig(config);
                setProbeConfig(saved);
                setProbeDraft(saved);
                setProbeSettingsNotice('探测配置已保存，下一轮调度生效');
              },
              undefined,
              setProbeSettingsError,
            );
          }}
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
