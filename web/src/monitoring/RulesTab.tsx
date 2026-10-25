import {useEffect, useRef, useState} from 'react';
import {Switch} from '../components/ui';
import {fetchRules, MonitorAPIError, saveRules} from './api';
import type {MonitorRule, MonitorRuleSeverity} from './types';
import {SEVERITY_SHORT} from './types';

/**
 * 规则页。保存是整表替换（PUT 全部规则），所以草稿必须是整张表：
 * 只改一条也要把其余条目原样带回——缺项会被后端当成「恢复默认」。
 *
 * 轮询不覆盖未保存的编辑：草稿只在本组件第一次拿到后端值、以及保存成功
 * （用响应覆盖）时重置。失败时草稿原样保留，不闪回。
 */

export interface RulesTabProps {
  onNotice: (text: string) => void;
  /** 页面顶部错误行（读取失败一类）；行内保存错误由本组件就地显示。 */
  error: string;
  /** 保存成功的提示（由外层持有，切走再回来仍在）。 */
  notice: string;
  timezone: string;
}

/** 只有三档级别可配；`info` 是事件用的，不能设成规则级别。 */
const RULE_SEVERITIES: MonitorRuleSeverity[] = ['p0', 'p1', 'p2'];
const SEVERITY_LABEL: Record<MonitorRuleSeverity, string> = { p0: 'P0', p1: 'P1', p2: 'P2' };
/** 窗口后端只接受 5/10/15，前端给三档选择器而不是自由输入。 */
const WINDOW_CHOICES = [5, 10, 15];
const COOLDOWN_MIN = 5;
const COOLDOWN_MAX = 1440;

/**
 * 参数名文案与量纲。带 Ratio 的占比是指 0–1；minSamples / minCount / maxCount / minIPs
 * 是正整数计数；ttft_regression.factor 是倍数。未列出的 key（probe_failed.authSeverity
 * 是级别码，不是阈值）不渲染成输入框。
 */
interface ParamSpec {
  label: string;
  /** 行尾「恢复默认」左侧的简短说明；空 = 不显示。 */
  short: string;
  unit: string;
  kind: 'ratio' | 'int' | 'number';
}

const PARAM_SPECS: Record<string, ParamSpec> = {
  leadMinutes: { label: '提前提醒', short: '提前', unit: '分钟', kind: 'int' },
  leadHours: { label: '提前提醒', short: '提前', unit: '小时', kind: 'int' },
  consecutiveFailures: { label: '连续失败', short: '连败≥', unit: '次', kind: 'int' },
  consecutiveDays: { label: '连续失败', short: '连败≥', unit: '天', kind: 'int' },
  windowMinutes: { label: '统计窗口', short: '窗口', unit: '', kind: 'int' },
  minCount: { label: '最少次数', short: '次数≥', unit: '次', kind: 'int' },
  minSamples: { label: '最少样本', short: '样本≥', unit: '次', kind: 'int' },
  maxCount: { label: '次数上限', short: '次数≤', unit: '次', kind: 'int' },
  minIPs: { label: 'IP 数', short: 'IP≥', unit: '个', kind: 'int' },
  maxAgeMinutes: { label: '最大滞后', short: '滞后≤', unit: '分钟', kind: 'int' },
  minRatio: { label: '最低占比', short: '占比≥', unit: '', kind: 'ratio' },
  maxRatio: { label: '占比上限', short: '占比≤', unit: '', kind: 'ratio' },
  maxSuccessRatio: { label: '成功率下限', short: '成功率≥', unit: '', kind: 'ratio' },
  maxErrorRatio: { label: '错误率上限', short: '错误率≤', unit: '', kind: 'ratio' },
  warnRatio: { label: '预警占比', short: '预警≤', unit: '', kind: 'ratio' },
  criticalRatio: { label: '耗尽占比', short: '耗尽≥', unit: '', kind: 'ratio' },
  floorMs: { label: '延迟地板', short: '地板', unit: 'ms', kind: 'int' },
  factor: { label: '回退倍数', short: '倍数≥', unit: '倍', kind: 'number' },
};

/** targetType 徽标文案。 */
const TARGET_TEXT: Record<string, string> = {
  provider: '账号',
  api_key: '密钥',
  gateway: '网关',
};

/** 草稿里参数一律按字符串存：type=number 清空时是 ''，直接转数字会变成 0。 */
interface RuleDraft {
  key: string;
  enabled: boolean;
  severity: MonitorRuleSeverity;
  cooldownMinutes: string;
  params: Record<string, string>;
}

/** updateDraft 的 patch：只允许整字段，params 必须走 paramPatch（避免整块覆盖成 {}）。 */
type RulePatch = Partial<Omit<RuleDraft, 'key' | 'params'>>;

/**
 * 「改没改」与「恢复默认」共用的基线：首次 GET（或保存成功后）那一刻的后端值。
 * 参数默认值取 GET 的 defaultParams（参数块的键集合也来自它）；开关/级别/冷却直接取
 * 当时的 GET 值——不在前端另存一份默认常量，免得和 config.go 漂移。
 */
interface RuleBaseline {
  enabled: boolean;
  severity: MonitorRuleSeverity;
  cooldownMinutes: string;
  params: Record<string, string>;
}

/** 后端 PUT 的条目：params 只带填了值的项（空 = 这一条按定义的默认参数落库）。 */
function toRequest(draft: RuleDraft): MonitorRule {
  const params: Record<string, number> = {};
  for (const [key, raw] of Object.entries(draft.params)) {
    if (raw.trim() === '') continue;
    params[key] = Number(raw);
  }
  return {
    key: draft.key,
    enabled: draft.enabled,
    severity: draft.severity,
    cooldownMinutes: Number(draft.cooldownMinutes),
    params,
  } as MonitorRule;
}

/** 一条规则 → 草稿。rule 缺失（键对不上）返回 null，调用方跳过，不抛。 */
function toDraft(rule: MonitorRule | undefined): RuleDraft | null {
  if (!rule) return null;
  const params: Record<string, string> = {};
  // 参数块的键集合取 defaultParams 与当前 params 的并集：老存储里可能缺某个键（那时 GET 的 params
  // 没有它，显式给出 defaultParams 的键越多越安全），两边都列出来才不会漏掉可编辑的参数。
  const keys = new Set([...Object.keys(rule.defaultParams ?? {}), ...Object.keys(rule.params ?? {})]);
  for (const key of [...keys].sort()) {
    const value = rule.params?.[key];
    // 未配置/不适用留空，渲染成「—」；写 0 会被后端当成缺项而静默补默认值。
    params[key] = typeof value === 'number' && value > 0 ? String(value) : '';
  }
  return {
    key: rule.key,
    enabled: rule.enabled,
    severity: rule.severity === 'p0' || rule.severity === 'p2' ? rule.severity : 'p1',
    cooldownMinutes: String(rule.cooldownMinutes),
    params,
  };
}

/** 一条草稿相对 GET 原值是否改过。GET 原值经 toDraft→toRequest 归一化，两边同口径。 */
function dirtyOf(draft: RuleDraft, rules: MonitorRule[]): boolean {
  const rule = rules.find((item) => item.key === draft.key);
  const base = toDraft(rule);
  if (!base) return false;
  return !sameRule(toRequest(draft), toRequest(base));
}

function toDrafts(rules: MonitorRule[]): RuleDraft[] {
  return rules.map((rule) => toDraft(rule)).filter((draft): draft is RuleDraft => draft !== null);
}

function baselineOf(rules: MonitorRule[]): Map<string, RuleBaseline> {
  const out = new Map<string, RuleBaseline>();
  for (const rule of rules) {
    const params: Record<string, string> = {};
    for (const key of Object.keys(rule.defaultParams ?? {})) {
      params[key] = rule.defaultParams[key] > 0 ? String(rule.defaultParams[key]) : '';
    }
    out.set(rule.key, {
      enabled: rule.enabled,
      severity: rule.severity === 'p0' || rule.severity === 'p2' ? rule.severity : 'p1',
      cooldownMinutes: String(rule.cooldownMinutes),
      params,
    });
  }
  return out;
}

/** 两条规则配置是否相同。不用 JSON.stringify 比：参数对象的键顺序不受控。 */
function sameRule(a: MonitorRule | undefined, b: MonitorRule | undefined): boolean {
  if (!a || !b) return a === b;
  if (a.enabled !== b.enabled || a.severity !== b.severity || a.cooldownMinutes !== b.cooldownMinutes) return false;
  const keys = new Set([...Object.keys(a.params), ...Object.keys(b.params)]);
  for (const key of keys) {
    if (a.params[key] !== b.params[key]) return false;
  }
  return true;
}

/** 前端预检：只是体验，唯一的防线仍是服务端 ValidateRules。 */
function checkDraft(draft: RuleDraft): string {
  const cooldown = Number(draft.cooldownMinutes);
  if (draft.cooldownMinutes.trim() === '' || !Number.isInteger(cooldown) || cooldown < COOLDOWN_MIN || cooldown > COOLDOWN_MAX) {
    return '冷却需为 5–1440 的整数分钟';
  }
  for (const [key, raw] of Object.entries(draft.params)) {
    if (raw.trim() === '') continue;
    const spec = PARAM_SPECS[key];
    const value = Number(raw);
    if (!Number.isFinite(value)) return `${spec?.label ?? key}：需要数字`;
    if (value <= 0) return `${spec?.label ?? key}：需大于 0`;
    if (spec?.kind === 'int' && !Number.isInteger(value)) return `${spec?.label ?? key}：需要整数`;
    if (key === 'windowMinutes' && !WINDOW_CHOICES.includes(value)) return '统计窗口只能是 5/10/15 分钟';
    if (key === 'minSamples' && (value < 1 || value > 10000)) return '最少样本需为 1–10000';
    if (spec?.kind === 'ratio' && value > 1) return `${spec.label}：取值 0–1`;
  }
  return '';
}

/** 比率后面跟的百分比：0.05 → 5%，让 0–1 的取值一眼可见。 */
function ratioText(value: string): string {
  const number = Number(value);
  if (value.trim() === '' || !Number.isFinite(number)) return value;
  return `${(number * 100).toFixed((number * 100) % 1 === 0 ? 0 : 1)}%`;
}

function paramHint(key: string, spec: ParamSpec | undefined): string {
  if (key === 'windowMinutes') return '5/10/15';
  if (key === 'minSamples') return '1–10000';
  if (spec?.kind === 'ratio') return '0–1';
  return '';
}

export function RulesTab({ onNotice, error, notice, timezone }: RulesTabProps) {
  const [rules, setRules] = useState<MonitorRule[]>([]);
  /*
   * 草稿放 ref：同一拍里连续两次改动，state 里的旧值会被后一次覆盖（读不到前一次）。
   * 每次写 ref 后再 bump 一个版本号触发重渲染。渲染里读的 drafts 永远是最新写入。
   */
  const draftsRef = useRef<RuleDraft[]>([]);
  const [, setDraftVersion] = useState(0);
  const drafts = draftsRef.current;
  const [baseline, setBaseline] = useState<Map<string, RuleBaseline>>(() => new Map());
  const [loadError, setLoadError] = useState('');
  const [saveError, setSaveError] = useState('');
  const [saving, setSaving] = useState(false);
  const [loading, setLoading] = useState(false);

  // 首次进入规则页读一次。之后只有保存成功才用响应覆盖——页面轮询不覆盖草稿。
  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    void fetchRules()
      .then((next) => {
        if (cancelled) return;
        setRules(next);
        writeDrafts(toDrafts(next));
        setBaseline(baselineOf(next));
        setLoadError('');
      })
      .catch((reason: unknown) => {
        if (cancelled) return;
        setLoadError(reason instanceof MonitorAPIError ? reason.message : '规则读取失败');
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const ruleOf = new Map(rules.map((rule) => [rule.key, rule]));
  // 「改没改」：GET 原样值过 toDraft → toRequest 归一化后与草稿逐字段比（同源同口径，参数键顺序无关）。
  // 「改没改」只看 GET 原值：每次渲染现算（规则 18 条，开销可忽略；缓存键漏字段会让行永远「未保存」）。
  const dirtyKeys = new Set(
    drafts.filter((draft) => dirtyOf(draft, rules)).map((draft) => draft.key),
  );

  const writeDrafts = (next: RuleDraft[]) => {
    draftsRef.current = next;
    setDraftVersion((version) => version + 1);
  };
  const updateDraft = (key: string, patch: RulePatch, paramPatch?: Record<string, string>) => {
    setSaveError('');
    const current = draftsRef.current;
    const next = current.map((draft) =>
      draft.key === key ? { ...draft, ...patch, params: paramPatch ? { ...draft.params, ...paramPatch } : draft.params } : draft,
    );
    writeDrafts(next);
  };

  /**
   * 恢复默认：只重置这一条。参数用 GET 的 defaultParams，开关/级别/冷却用首次 GET 记下的
   * 基线值——前端不另存一份默认常量，免得和 config.go 漂移。
   */
  const restoreOne = (key: string) => {
    const base = baseline.get(key);
    if (!base) return;
    // 基线里的参数是 GET 回来的**当前** params（不是 defaultParams）：恢复默认要回的是
    // 「打开页面时服务端的样子」，其中可能含界面不渲染的键（probe_failed.authSeverity）。
    // 用按键合并而不是整块替换，任何未在界面上暴露的参数都不会被这一下抹掉。
    updateDraft(key, { enabled: base.enabled, severity: base.severity, cooldownMinutes: base.cooldownMinutes }, base.params);
  };

  const save = async () => {
    setSaveError('');
    for (const draft of drafts) {
      const rule = ruleOf.get(draft.key);
      const problem = rule ? checkDraft(draft) : '规则定义缺失';
      if (problem) {
        setSaveError(`${rule?.name ?? draft.key}：${problem}`);
        return;
      }
    }
    setSaving(true);
    try {
      const saved = await saveRules(drafts.map(toRequest));
      setRules(saved);
      writeDrafts(toDrafts(saved));
      setBaseline(baselineOf(saved));
      setSaveError('');
      onNotice(`规则已保存（${saved.length} 条），下一拍调度生效`);
    } catch (reason) {
      // 保留编辑：只显示错误，不把草稿换回后端值。
      setSaveError(reason instanceof MonitorAPIError ? reason.message : '规则保存失败，请稍后重试');
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="mon-rules">
      <div className="mon-rules-bar">
        <div className="mon-rules-stats">
          <span className="mon-rules-stat">
            <b className="mon-rules-stat-value">{drafts.filter((draft) => draft.enabled).length}</b>
            <span className="mon-rules-stat-key">{'条已启用'}</span>
          </span>
          <span className="mon-rules-stat-sep" aria-hidden>{'/'}</span>
          <span className="mon-rules-stat">
            <b className="mon-rules-stat-value">{drafts.length}</b>
            <span className="mon-rules-stat-key">{'条总数'}</span>
          </span>
          {dirtyKeys.size > 0 && (
            <span className="mon-rules-dirty" title="改动只在点「保存规则」后生效；保存是整表提交">
              <span className="mon-rules-dirty-dot" aria-hidden />
              {`${dirtyKeys.size} 条未保存`}
            </span>
          )}
        </div>
        <div className="mon-rules-actions">
          <button className="btn btn-sm primary" disabled={saving || drafts.length === 0} onClick={() => void save()}>
            {saving ? '保存中…' : '保存规则'}
          </button>
        </div>
      </div>
      <p className="mon-rules-note">
        {'每条规则只在命中时通知，不会自动处置账号或密钥。保存时整表提交，参数在服务端校验，非法值不会落库。'}
      </p>

      {error && <div className="rules-inline-error" role="alert"><span className="rules-err">{error}</span></div>}
      {loadError && <div className="rules-inline-error" role="alert"><span className="rules-err">{loadError}</span></div>}
      {notice && !saveError && (
        <div className="rules-inline-notice">
          <span className="muted-text">{notice}</span>
        </div>
      )}
      {saveError && (
        <div className="rules-inline-error" role="alert">
          <span className="rules-err">{`未保存：${saveError}`}</span>
        </div>
      )}
      {loading && drafts.length === 0 && <div className="m-empty">{'读取中…'}</div>}

      {drafts.map((draft) => {
        const rule = ruleOf.get(draft.key);
        if (!rule) return null;
        const paramKeys = Object.keys(baseline.get(draft.key)?.params ?? rule.defaultParams ?? {}).sort();
        const dirty = dirtyKeys.has(draft.key);
        return (
          <div
            className={`mon-rules-item${dirty ? ' is-dirty' : ''}${draft.enabled ? '' : ' is-off'}`}
            key={draft.key}
          >
            <div className="mrow mrow-rule">
              <span className="rules-switch">
                <Switch
                  checked={draft.enabled}
                  label={draft.enabled ? '启用' : '停用'}
                  onChange={(value) => updateDraft(draft.key, { enabled: value })}
                />
              </span>
              <span className="rules-name">
                <span className="mon-rules-name" title={`${rule.name}（${rule.key}）`}>
                  {rule.name}
                  {dirty && <span className="mon-rules-dirty-dot" aria-hidden />}
                </span>
                <span className={`mon-acc-tag rules-tag-${rule.targetType}`}>
                  {TARGET_TEXT[rule.targetType] ?? '目标'}
                </span>
                <span className="rules-interval" title="引擎检查这条规则的间隔">
                  {`每 ${rule.intervalMinutes} 分钟检查`}
                </span>
              </span>
              <div className="rules-fields">
                <label className="rules-cell">
                  <span className="rules-cell-label">{'级别'}</span>
                  <select
                    className="rules-control"
                    aria-label={`${rule.name} 级别`}
                    value={draft.severity}
                    onChange={(event) => updateDraft(draft.key, { severity: event.target.value as MonitorRuleSeverity })}
                  >
                    {RULE_SEVERITIES.map((value) => (
                      <option key={value} value={value}>
                        {`${SEVERITY_LABEL[value]} · ${SEVERITY_SHORT[value]}`}
                      </option>
                    ))}
                  </select>
                </label>
                <label className="rules-cell">
                  <span className="rules-cell-label" title="同一条告警在这段时间内不重复通知；5–1440 分钟">
                    {'冷却'}
                  </span>
                  <span className="rules-cooldown">
                    <input
                      className="rules-control rules-num"
                      type="number"
                      min={COOLDOWN_MIN}
                      max={COOLDOWN_MAX}
                      step={1}
                      aria-label={`${rule.name} 冷却分钟`}
                      value={draft.cooldownMinutes}
                      onChange={(event) => updateDraft(draft.key, { cooldownMinutes: event.target.value })}
                    />
                    <span className="muted-text">{'分钟'}</span>
                  </span>
                </label>
              </div>
              <span className="rules-params">
                {paramKeys.length === 0 && <span className="mon-rules-param-empty">{'这条规则没有可调参数'}</span>}
                {paramKeys.map((key) => {
                  const spec = PARAM_SPECS[key];
                  const raw = draft.params[key] ?? '';
                  if (!spec) {
                    // 界面不认识的参数（probe_failed.authSeverity 是「认证失败时按该级别通知」）：
                    // 按级别文字显示，既不出现裸数字，也不变成 0。
                    return (
                      <span className="rules-param" key={key} title={`${key} · 认证失败时按该级别通知`}>
                        <span className="rules-cell-label">{'认证失败级别'}</span>
                        <span className="mon-rules-readonly">{raw === '' ? '—' : SEVERITY_LABEL[raw as MonitorRuleSeverity] ?? raw}</span>
                      </span>
                    );
                  }
                  const hint = paramHint(key, spec);
                  const ratio = spec.kind === 'ratio' && raw !== '' && raw !== '0';
                  return (
                    <label
                      className={`rules-param${key === 'windowMinutes' ? ' rules-param-wide' : ''}`}
                      key={key}
                      title={`${key}${hint ? ` · ${hint}` : ''}`}
                    >
                      <span className="rules-cell-label">
                        {spec.label}
                        {spec.unit ? `（${spec.unit}）` : ''}
                        {ratio && <span className="rules-ratio-note">{` ${ratioText(raw)}`}</span>}
                      </span>
                      {key === 'windowMinutes' ? (
                        <select
                          className="rules-control"
                          aria-label={`${rule.name} ${key}`}
                          value={raw}
                          onChange={(event) => updateDraft(draft.key, {}, { [key]: event.target.value })}
                        >
                          {WINDOW_CHOICES.map((value) => (
                            <option key={value} value={String(value)}>{`${value} 分钟`}</option>
                          ))}
                        </select>
                      ) : (
                        <input
                          className={`rules-control ${spec.kind === 'int' ? 'rules-num' : ''}`}
                          type="number"
                          step={spec.kind === 'int' ? 1 : 'any'}
                          min={0}
                          aria-label={`${rule.name} ${key}`}
                          placeholder="—"
                          value={raw}
                          onChange={(event) => updateDraft(draft.key, {}, { [key]: event.target.value })}
                        />
                      )}
                    </label>
                  );
                })}
              </span>
              <span className="rules-actions">
                {/* 这里曾有一个「参数简称摘要」（次数≥ · 占比≥ …）。参数改成两段式后
                    每个参数都有自己的块，摘要是重复信息；且它和「恢复默认」挤在同一格，
                    宽度不够时会被截成「次数≥ 占比≥ 样本≥ 窗」这种残缺文字。删掉。 */}
                <button
                  className="btn btn-sm"
                  disabled={!dirtyKeys.has(draft.key)}
                  title="只把这一条恢复成默认值；保存后才生效"
                  onClick={() => restoreOne(draft.key)}
                >
                  {'恢复默认'}
                </button>
              </span>
            </div>
          </div>
        );
      })}

      <p className="mon-rules-foot">
        {`恢复默认只作用于单条；保存是整表提交，同一时刻的其它改动会一起写入。时间按投递时区 ${timezone} 计算。`}
      </p>
    </div>
  );
}
