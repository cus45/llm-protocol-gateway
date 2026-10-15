import React, { useEffect, useMemo, useRef, useState } from 'react';
import type { Provider, ProviderPreset } from '../types';
import { presetMatchesProvider } from '../lib';
import { groupProviderPresets } from '../provider_presets';
import { Modal } from './ui';

/**
 * 添加 Provider 的目录弹窗：检索 + 分区磁贴，点一个即预填连接方式、协议与接入
 * 地址。走的是设计系统既有的 Modal（Portal 到 body、z-index 与 ESC 由 modalLayers
 * 统一管理），不自己造遮罩与层级。
 */
export function ProviderCatalogDialog({
  open,
  providers,
  hasProviders,
  onClose,
  onPick,
}: {
  open: boolean;
  providers: Provider[];
  hasProviders: boolean;
  onClose: () => void;
  onPick: (preset: ProviderPreset) => void;
}) {
  const [query, setQuery] = useState('');
  const searchRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    if (!open) return;
    // 每次打开都从完整目录开始，避免上次的检索词把这次"框里没东西"地卡住。
    setQuery('');
    const timer = window.setTimeout(() => searchRef.current?.focus(), 60);
    return () => window.clearTimeout(timer);
  }, [open]);

  const groups = useMemo(() => groupProviderPresets(query), [query]);
  const addedCount = (preset: ProviderPreset) => providers.filter((provider) => presetMatchesProvider(preset, provider)).length;

  if (!open) return null;

  return (
    <Modal
      title={hasProviders ? '添加 Provider' : '添加你的第一个 Provider'}
      description="选一个接入方式，地址与协议自动填好；订阅账号登录一次即可，无需粘贴密钥。"
      size="wide"
      onClose={onClose}
    >
      <div className="provider-catalog-search">
        <input
          ref={searchRef}
          type="text"
          className="models-search-input"
          value={query}
          placeholder="检索厂商、中转站或地址…"
          aria-label="检索 Provider 目录"
          onChange={(event) => setQuery(event.target.value)}
          onKeyDown={(event) => {
            // 有检索词时 ESC 先清词，再按一次才关弹窗，避免误触丢掉输入条件。
            if (event.key === 'Escape' && query) {
              event.preventDefault();
              event.stopPropagation();
              setQuery('');
            }
          }}
        />
      </div>
      {groups.length === 0 ? (
        <div className="provider-catalog-empty">
          目录里没有「{query.trim()}」。
          <button className="btn-text" type="button" onClick={() => onPick(customEndpointPreset())}>按自定义端点添加</button>
        </div>
      ) : (
        groups.map((group) => (
          <div className="provider-catalog-section" key={group.id}>
            <div className="provider-catalog-section-head">
              <span className="provider-catalog-section-title">{group.title}</span>
              <span className="provider-catalog-section-desc">{group.desc}</span>
            </div>
            <div className="provider-tiles">
              {group.presets.map((preset) => {
                const added = addedCount(preset);
                return (
                  <button
                    key={preset.id}
                    type="button"
                    className={`provider-tile ${added > 0 ? 'have' : ''}`}
                    title={[preset.baseUrl, preset.note, preset.keysUrl ? `密钥：${preset.keysUrl}` : ''].filter(Boolean).join('\n')}
                    onClick={() => onPick(preset)}
                  >
                    <PresetMark preset={preset} />
                    <span className="provider-tile-name">{preset.name}</span>
                    {added > 0 ? <span className="provider-tile-count">{added}</span> : null}
                  </button>
                );
              })}
            </div>
          </div>
        ))
      )}
    </Modal>
  );
}

// PresetMark：品牌图标。-color 是彩色版，按 <img> 原样显示；其余是 currentColor
// 单色版，只能走 CSS mask 才会跟着主题着色——直接当图片渲染会在深色下变成一团黑。
// 没有图标的（自定义端点、自助注册）退回名称首字。
function PresetMark({ preset }: { preset: ProviderPreset }) {
  const icon = preset.icon;
  if (!icon) {
    return <span className="provider-tile-mark" aria-hidden="true">{presetMonogram(preset)}</span>;
  }
  const source = `/icons/${icon}.svg`;
  if (icon.endsWith('-color')) {
    return <img className="provider-tile-icon" src={source} alt="" width={18} height={18} />;
  }
  return (
    <span
      className="provider-tile-icon mask"
      aria-hidden="true"
      style={{ WebkitMaskImage: `url(${source})`, maskImage: `url(${source})` }}
    />
  );
}

function customEndpointPreset(): ProviderPreset {
  return {
    id: 'custom',
    name: '自定义端点',
    section: 'local',
    authType: 'api_key',
    protocol: 'openai_chat',
    baseUrl: 'https://example.com/v1/chat/completions',
  };
}

// presetMonogram：磁贴左侧的名称首字。中文名只取一个字——两个汉字放不进 18px
// 的方块，实测会把"阶跃"挤成两行。
function presetMonogram(preset: ProviderPreset): string {
  const source = preset.name.replace(/[（(].*$/, '').trim();
  const letters = source.match(/[A-Za-z]/g);
  if (letters && letters.length >= 2) return letters.slice(0, 2).join('').toUpperCase();
  return source.slice(0, 1);
}
