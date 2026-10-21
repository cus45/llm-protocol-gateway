import {useEffect, useMemo, useState} from 'react';
import type {Model, ModelUSDPrice, Provider} from '../types';
import {API_BASE, formatCostCoverage, providerUsageLabel} from '../lib';
import {providerAccountFace} from './provider_select';
import {Modal} from './ui';
import './usage.css';

/** 价格行的空表单初值：单价留空＝未计价，不是 0。 */
const EMPTY_RATE = '';

/** 未设价清单一次最多列几个，其余只报个数：面板不是用来翻页的。 */
const MISSING_PREVIEW = 8;

/** 四档单价的字段名：收窄成数字键，读出来就不用再判类型。 */
type RateKey = 'inputUsdPerMTok' | 'outputUsdPerMTok' | 'cacheReadUsdPerMTok' | 'cacheWriteUsdPerMTok';
type RateField = { key: RateKey; label: string; hint: string };

// 四档单价的顺序与含义要和后端 costOf 的算法一致：input 只算未命中缓存的那部分，
// 否则缓存 token 会被按全价重复计一次。
const RATE_FIELDS: RateField[] = [
  { key: 'inputUsdPerMTok', label: '输入（未缓存）', hint: '每百万 Token 的美元价。只算没有命中缓存的 prompt，缓存读取/写入另有单价。' },
  { key: 'outputUsdPerMTok', label: '输出', hint: '每百万生成 Token 的美元价。' },
  { key: 'cacheReadUsdPerMTok', label: '缓存读取', hint: '每百万缓存命中 Token 的美元价，通常是输入价的 1/10。' },
  { key: 'cacheWriteUsdPerMTok', label: '缓存写入', hint: '每百万缓存创建 Token 的美元价，一般比输入价略高。' },
];

function rateValue(price: ModelUSDPrice | null, field: RateField): string {
  if (!price) return EMPTY_RATE;
  const value = price[field.key];
  return typeof value === 'number' && value !== 0 ? String(value) : EMPTY_RATE;
}

async function readPrices(): Promise<ModelUSDPrice[]> {
  const response = await fetch(`${API_BASE}/__prices`, { credentials: 'same-origin' });
  if (!response.ok) throw new Error(`读取价格表失败：HTTP ${response.status}`);
  const data = (await response.json()) as { prices?: ModelUSDPrice[] };
  return data.prices || [];
}

/**
 * 价格表面板：网关成本账本的唯一录入入口。
 *
 * 这里的单价是运维按厂商价目表手工填的，网关不会替我们猜：猜错的价格会被悄悄加进
 * 总额，比留空（显示「未计价」）更危险。改价只影响之后的请求，历史成本已按当时的
 * 价格落库，不回算。
 */
export function PriceTablePanel({ providers, models, coverage, onClose }: {
  providers: Provider[];
  models: Model[];
  /** 区间内各 Provider 的请求数与已计价请求数，用来说「还有多少没设价」。 */
  coverage: Array<{ providerId: string; requestCount: number; pricedCount: number }>;
  onClose: () => void;
}) {
  const [prices, setPrices] = useState<ModelUSDPrice[] | null>(null);
  const [error, setError] = useState('');
  // 默认选中第一个 Provider；打开面板后下面的 effect 会把它换成「有用量但没设价」
  // 的那一个——来补价的人不该从列表第一项开始翻。
  const [providerId, setProviderId] = useState(providers[0]?.id || '');
  const [modelId, setModelId] = useState('*');
  const [rates, setRates] = useState<Record<string, string>>({});
  const [saving, setSaving] = useState(false);
  // 正在编辑的行键（provider/model）；为空表示新增。删掉一条精确行后，该模型会
  // 回落到 Provider 的 "*" 默认价，这点要在界面上说清楚。
  const [editingKey, setEditingKey] = useState('');

  const reload = async () => {
    try {
      setPrices(await readPrices());
      setError('');
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  useEffect(() => {
    void reload();
    // 打开时把编辑对象指向第一个「有用量但完全没有价格行」的 Provider，只看这一次，
    // 之后用户自己的选择不被覆盖。
    const firstUnpriced = coverage.find(item => item.requestCount > 0 && item.pricedCount === 0);
    if (firstUnpriced) setProviderId(firstUnpriced.providerId);
  }, []);

  const providerModels = useMemo(
    () => models.filter(model => model.providerId === providerId).map(model => model.id),
    [models, providerId],
  );

  const rows = prices || [];
  // 请求多的排前面：先给吃掉最多量的 Provider 设价，账本立刻就有意义。
  const missing = coverage
    .filter(item => item.requestCount > 0 && item.pricedCount === 0)
    .sort((a, b) => b.requestCount - a.requestCount);

  function loadIntoForm(price: ModelUSDPrice) {
    setProviderId(price.providerId);
    setModelId(price.modelId || '*');
    setRates(Object.fromEntries(RATE_FIELDS.map(field => [field.key, rateValue(price, field)])));
    setEditingKey(`${price.providerId}/${price.modelId}`);
  }

  function clearForm() {
    setRates({});
    setEditingKey('');
    setModelId('*');
  }

  async function save() {
    if (!providerId) {
      setError('先选择 Provider 再保存价格。');
      return;
    }
    const payload: ModelUSDPrice = {
      providerId,
      modelId: modelId.trim() || '*',
      inputUsdPerMTok: Number(rates.inputUsdPerMTok || 0),
      outputUsdPerMTok: Number(rates.outputUsdPerMTok || 0),
      cacheReadUsdPerMTok: Number(rates.cacheReadUsdPerMTok || 0),
      cacheWriteUsdPerMTok: Number(rates.cacheWriteUsdPerMTok || 0),
    };
    if (RATE_FIELDS.some(field => Number.isNaN(Number(rates[field.key] || 0)) || Number(rates[field.key] || 0) < 0)) {
      setError('单价必须是不小于 0 的数字；不设价就留空或删掉这一行。');
      return;
    }
    setSaving(true);
    setError('');
    try {
      const response = await fetch(`${API_BASE}/__prices`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (!response.ok) throw new Error(`保存失败：${(await response.text()).slice(0, 160)}`);
      await reload();
      clearForm();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  }

  async function remove(price: ModelUSDPrice) {
    setError('');
    try {
      const response = await fetch(
        `${API_BASE}/__prices/${encodeURIComponent(price.providerId)}/${encodeURIComponent(price.modelId || '*')}`,
        { method: 'DELETE', credentials: 'same-origin' },
      );
      if (!response.ok) throw new Error(`删除失败：HTTP ${response.status}`);
      await reload();
      if (editingKey === `${price.providerId}/${price.modelId}`) clearForm();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }

  return (
    <Modal title="价格表 · 成本账本" size="wide" onClose={onClose}
      description="按厂商价目表填美元/百万 Token 单价。没有价格行的 Provider 与模型一律显示「未计价」，不会算成 $0；订阅制（按积分计费）的上游建议留空。改价只影响之后的请求。">
      <div className="price-panel">
        {!!missing.length && (
          <div className="price-missing">
            <div className="price-missing-title">本区间有用量但还没设价</div>
            {missing.slice(0, MISSING_PREVIEW).map(item => (
              <div className="price-missing-row" key={item.providerId}>
                <span>{providerUsageLabel(item.providerId, providers)}</span>
                <small>{item.requestCount.toLocaleString()} 次请求未计价</small>
                <button type="button" className="mini-btn" onClick={() => { setProviderId(item.providerId); setModelId('*'); setEditingKey(''); }}>去设价</button>
              </div>
            ))}
            {missing.length > MISSING_PREVIEW ? (
              <div className="price-missing-more">另有 {missing.length - MISSING_PREVIEW} 个 Provider 在本区间也没有价格行。</div>
            ) : null}
          </div>
        )}

        <div className="price-editor">
          <div className="field">
            <label>Provider</label>
            <select value={providerId} onChange={event => { setProviderId(event.target.value); setModelId('*'); }}>
              {providers.map(provider => <option key={provider.id} value={provider.id}>{providerAccountFace(provider)}</option>)}
            </select>
          </div>
          <div className="field">
            <label>模型</label>
            <input list="price-model-options" value={modelId} placeholder="*（该 Provider 的默认价）"
              onChange={event => { setModelId(event.target.value); }} />
            <datalist id="price-model-options">
              <option value="*" />
              {providerModels.map(id => <option key={id} value={id} />)}
            </datalist>
          </div>
          {RATE_FIELDS.map(field => (
            <div className="field" key={field.key}>
              <label>{field.label} USD/MTok</label>
              <input type="number" min="0" step="0.01"
                value={rates[field.key] || EMPTY_RATE} title={field.hint}
                onChange={event => setRates(current => ({ ...current, [field.key]: event.target.value }))} />
            </div>
          ))}
          <div className="price-editor-actions">
            <button type="button" className="btn primary" disabled={saving} onClick={() => void save()}>
              {saving ? '保存中…' : editingKey ? '更新这一行' : '保存价格'}
            </button>
            {editingKey ? <button type="button" className="btn" onClick={clearForm}>取消编辑</button> : null}
          </div>
        </div>

        {error ? <div className="hint-line error">{error}</div> : null}

        <div className="price-list">
          <div className="price-row price-row-head">
            <span>Provider</span><span>模型</span><span>输入</span><span>输出</span><span>缓存读</span><span>缓存写</span><span>操作</span>
          </div>
          {!prices ? <div className="empty-state compact">正在读取价格表…</div> : !rows.length
            ? <div className="empty-state compact">还没有任何价格行，所有请求都会记为未计价。</div>
            : rows.map(price => (
              <div className="price-row" key={`${price.providerId}/${price.modelId}`}>
                <span title={price.providerId}>{providerUsageLabel(price.providerId, providers)}</span>
                <span className="font-mono">{price.modelId === '*' ? '默认（*）' : price.modelId}</span>
                {RATE_FIELDS.map(field => <span className="num" key={field.key}>{price[field.key] ? price[field.key].toFixed(2) : '—'}</span>)}
                <span className="price-row-actions">
                  <button type="button" className="mini-btn" onClick={() => loadIntoForm(price)}>编辑</button>
                  <button type="button" className="mini-btn danger" onClick={() => void remove(price)}>删除</button>
                </span>
              </div>
            ))}
        </div>

        <p className="usage-note">
          输入价只作用于未命中缓存的 prompt：缓存读取与缓存写入的 Token 已经算在输入总量里，
          再按输入价计一次就翻倍了。成本以微美元整数逐请求落库，日聚合等于请求之和。
        </p>
        {rows.length ? (
          <p className="usage-note">
            共 {rows.length} 行价格 ·{' '}
            {formatCostCoverage(
              coverage.reduce((sum, item) => sum + item.pricedCount, 0),
              coverage.reduce((sum, item) => sum + item.requestCount, 0),
            )}
          </p>
        ) : null}
      </div>
    </Modal>
  );
}
