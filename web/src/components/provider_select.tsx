import {protocolLabel, providerAccountLabel, providerOptionLabel, shortenAccountLabel} from '../lib';
import {providerIcon} from './provider_label';
import type {IconSelectOption} from './selects';
import type {Provider} from '../types';

/**
 * 每个 Provider 在下拉里给人看的名字：品牌名 + 脱敏账号
 * （WorkBuddy 订阅 · 1815****4947）。同一个服务商每多登一个账号就多一条同名
 * 记录，只读名字分不出是哪条；账号必须在这里，展开选的时候能分清，选完之后
 * 触发器上也得能分清，两处用同一块文字。
 */
export function providerAccountFace(provider: Provider): string {
  const account = providerAccountLabel(provider);
  const name = provider.name.trim();
  if (!account || account === name) return provider.name;
  return `${name} · ${maskAccountLabel(account)}`;
}

/**
 * 筛选项 / 弹窗里的「输入 Provider」选项：品牌图标 + 带账号的名字，右侧标协议。
 * 没有账号可认的（API Key 类）第二行退回短 ID，同名项之间靠它区分。
 *
 * 账号已经并在 label 里了，菜单不再重复挂第二行——4 个 WorkBuddy 账号挤在一屏
 * 里，每项两行账号只会把列表撑长、把能区分的那个值说两遍。
 */
export function providerSelectOptions(providers: Provider[], options: { withProtocol?: boolean } = {}): IconSelectOption[] {
  const withProtocol = options.withProtocol ?? true;
  return providers.map((provider) => {
    const account = providerAccountLabel(provider);
    return {
      value: provider.id,
      label: providerAccountFace(provider),
      icon: providerIcon(provider),
      description: account ? undefined : provider.id,
      meta: withProtocol ? protocolLabel(provider.protocol) : undefined,
      keywords: `${providerOptionLabel(provider)} ${account} ${provider.id}`,
    };
  });
}

/** 手机号只留前 4 后 4（1815****4947）；邮箱砍掉域名；其余原样。 */
function maskAccountLabel(label: string): string {
  const phone = label.replace(/[\s-]/g, '');
  if (/^\+?\d{7,15}$/.test(phone)) {
    return phone.length <= 8 ? phone : `${phone.slice(0, 4)}****${phone.slice(-4)}`;
  }
  return shortenAccountLabel(label);
}
