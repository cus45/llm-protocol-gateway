import { providerOptionLabel } from '../lib';
import { resolveProviderIcon } from '../provider_presets';
import type { Provider } from '../types';
import { BrandIcon } from './ui';

/**
 * Provider 的统一署名：品牌图标 + 名称。
 *
 * 「输入 Provider」卡片早就带图标了，但模型列表的筛选项、密钥的路由与方案、
 * API 日志的 Provider 列都还是光秃秃一行字——同一个服务商在七个地方长得都不
 * 一样，认厂商只能靠读字。这里收一个组件，各处都用它。
 */
export function ProviderLabel({ provider, size = 16, protocol, className = '' }: {
  provider: Provider;
  size?: number;
  /** 需要时把协议跟在名字后面，省得每处自己拼 Badge。 */
  protocol?: string;
  className?: string;
}) {
  return (
    <span className={`provider-label${className ? ` ${className}` : ''}`} title={providerOptionLabel(provider)}>
      <BrandIcon icon={resolveProviderIcon(provider)} name={provider.name} size={size} />
      <span className="provider-label-name">{provider.name}</span>
      {protocol ? <span className="provider-label-protocol">{protocol}</span> : null}
    </span>
  );
}

/**
 * 只有 providerId 的场合（日志、用量这类按 id 聚合的行）：找不到对应 Provider
 * 时退回显示原始 id，图标位用首字占位，不留空洞也不假装知道是谁。
 */
export function ProviderRefLabel({ providerId, providers, size = 16 }: {
  providerId?: string;
  providers: Provider[];
  size?: number;
}) {
  const provider = providers.find((item) => item.id === providerId);
  if (!provider) {
    return <span className="provider-label missing" title={providerId || '未知 Provider'}>{providerId || '未知'}</span>;
  }
  return <ProviderLabel provider={provider} size={size} />;
}

/** 给筛选下拉之类的纯数据选项用的图标字段：与 ProviderLabel 同一张图标表。 */
export function providerIcon(provider: Provider): string {
  return resolveProviderIcon(provider);
}
