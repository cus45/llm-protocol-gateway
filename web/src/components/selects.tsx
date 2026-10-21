// @generated-from main.tsx — 由重构脚本拆分生成，请直接维护本文件。
import React from 'react';
import {createPortal} from 'react-dom';
import {modelSelectOptionLabel, reportIfLooksLikeAutofill} from '../lib';
import {Model, MultiFilterOption, SearchableModelOption} from '../types';
import {BrandIcon, InfoTip} from './ui';

export function filterModelOptions(models: Model[], queryRaw: string): SearchableModelOption[] {
  const needle = queryRaw.trim().toLowerCase();
  const options = models.map((model) => ({ id: model.id, label: modelSelectOptionLabel(model) }));
  if (!needle) return options;
  return options.filter((option) => option.id.toLowerCase().includes(needle) || option.label.toLowerCase().includes(needle));
}

// MultiSelectFilter：下拉多选 + 文本快捷过滤合一的筛选框（复用
// searchable-select 样式，参考 SearchableModelSelect 的交互）。收起时输入框
// 展示已选摘要，展开时变成关键字过滤；点击选项切换勾选且菜单保持展开。
/**
 * 下拉菜单 Portal：菜单渲染到 document.body 并按输入框位置 fixed 定位，
 * 跟随页面/弹窗滚动实时更新；空间不足时自动向上翻。
 * 解决菜单被 overflow:auto/hidden 的祖先（.modal-card、.api-keys-table-wrap
 * 等）裁剪、以及在弹窗内 z-index 不够被遮挡的问题。
 */
export function SelectMenuPortal({ anchorRef, open, menuRef, children, id, multiselect }: {
  anchorRef: React.RefObject<HTMLDivElement | null>;
  open: boolean;
  menuRef: React.RefObject<HTMLDivElement | null>;
  children: React.ReactNode;
  id?: string;
  multiselect?: boolean;
}) {
  const [style, setStyle] = React.useState<React.CSSProperties>({});

  React.useLayoutEffect(() => {
    if (!open) return undefined;
    const update = () => {
      const anchor = anchorRef.current;
      if (!anchor) return;
      const rect = anchor.getBoundingClientRect();
      const menuHeight = menuRef.current?.offsetHeight ?? 280;
      const spaceBelow = window.innerHeight - rect.bottom;
      const openUp = spaceBelow < Math.min(menuHeight, 220) && rect.top > spaceBelow;
      setStyle({
        position: 'fixed',
        left: rect.left,
        width: rect.width,
        top: openUp ? undefined : rect.bottom + 6,
        bottom: openUp ? window.innerHeight - rect.top + 6 : undefined,
        zIndex: 3200,
      });
    };
    update();
    window.addEventListener('resize', update);
    window.addEventListener('scroll', update, true);
    return () => {
      window.removeEventListener('resize', update);
      window.removeEventListener('scroll', update, true);
    };
  }, [anchorRef, menuRef, open, children]);

  if (!open) return null;
  return createPortal(
    <div
      className="searchable-select-menu portal"
      style={style}
      role="listbox"
      aria-multiselectable={multiselect || undefined}
      id={id}
      ref={menuRef}
    >
      {children}
    </div>,
    document.body,
  );
}

export function MultiSelectFilter({ label, options, selected, onChange, allLabel, fieldClassName }: {
  label?: string;
  options: MultiFilterOption[];
  selected: string[];
  onChange: (next: string[]) => void;
  allLabel?: string;
  // 外层 field 样式：筛选栏默认窄宽度，弹窗内可传 '' 占满整行。
  fieldClassName?: string;
}) {
  const rootRef = React.useRef<HTMLDivElement | null>(null);
  const inputRef = React.useRef<HTMLInputElement | null>(null);
  const listRef = React.useRef<HTMLDivElement | null>(null);
  const [open, setOpen] = React.useState(false);
  const [query, setQuery] = React.useState('');
  const [highlight, setHighlight] = React.useState(0);

  const filtered = React.useMemo(() => {
    const keyword = query.trim().toLowerCase();
    if (!keyword) return options;
    return options.filter((option) => option.label.toLowerCase().includes(keyword) || option.id.toLowerCase().includes(keyword));
  }, [options, query]);

  const displayLabel = React.useMemo(() => {
    if (selected.length === 0) return allLabel || '全部';
    const labels = selected.map((id) => options.find((option) => option.id === id)?.label || id);
    const joined = labels.join('、');
    return joined.length > 24 ? `已选 ${selected.length} 项` : joined;
  }, [selected, options, allLabel]);

  const close = React.useCallback(() => {
    setOpen(false);
    setQuery('');
    setHighlight(0);
  }, []);

  const toggleOption = React.useCallback((id: string) => {
    onChange(selected.includes(id) ? selected.filter((item) => item !== id) : [...selected, id]);
  }, [onChange, selected]);

  React.useEffect(() => {
    if (!open) return;
    // 用捕获阶段监听：Modal 的 modal-card 会在冒泡阶段对 mousedown 调用
    // stopPropagation（防止点击弹窗内部误触发遮罩层的关闭逻辑），这会导致
    // 挂在 document 冒泡阶段的“点击外部关闭”监听永远收不到事件——弹窗内的
    // 下拉框因此选完选项后无法通过点击其他地方收起。捕获阶段先于冒泡阶段
    // 触发，不受该 stopPropagation 影响。
    const onPointerDown = (event: MouseEvent) => {
      const target = event.target as Node;
      // 菜单已 Portal 到 body，不在 rootRef 内，必须额外判断，否则点选项会触发关闭。
      if (rootRef.current?.contains(target) || listRef.current?.contains(target)) return;
      close();
    };
    document.addEventListener('mousedown', onPointerDown, true);
    return () => document.removeEventListener('mousedown', onPointerDown, true);
  }, [close, open]);

  React.useEffect(() => {
    setHighlight(0);
  }, [query, open]);

  React.useEffect(() => {
    if (!open || !listRef.current) return;
    const active = listRef.current.querySelector<HTMLElement>('[data-active="true"]');
    active?.scrollIntoView({ block: 'nearest' });
  }, [highlight, open, filtered]);

  const onKeyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'ArrowDown') {
      event.preventDefault();
      if (!open) { setOpen(true); return; }
      setHighlight((prev) => (filtered.length === 0 ? 0 : (prev + 1) % filtered.length));
      return;
    }
    if (event.key === 'ArrowUp') {
      event.preventDefault();
      if (!open) { setOpen(true); return; }
      setHighlight((prev) => (filtered.length === 0 ? 0 : (prev - 1 + filtered.length) % filtered.length));
      return;
    }
    if (event.key === 'Enter') {
      event.preventDefault();
      if (!open) { setOpen(true); return; }
      const option = filtered[highlight];
      if (option) toggleOption(option.id);
      return;
    }
    if (event.key === 'Escape') {
      // 下拉框展开时：ESC 只收起下拉框，并阻止事件冒泡到 Modal 的全局 ESC 监听
      // （否则整个弹窗会被一起关掉）。未展开时不拦截，让 ESC 正常关闭弹窗。
      if (open) {
        event.preventDefault();
        event.stopPropagation();
        close();
      }
      return;
    }
  };

  return (
    <div className={`field ${fieldClassName ?? 'api-keys-filter-field'}`.trim()}>
      {label ? <label>{label}</label> : null}
      <div className={`searchable-select${open ? ' open' : ''}`} ref={rootRef}>
        <input
          ref={inputRef}
          className="searchable-select-input"
          type="text"
          role="combobox"
          aria-expanded={open}
          aria-autocomplete="list"
          // 该输入框会出现在「用户名 + 密码」正下方（用户管理弹窗），整体形状
          // 与登录表单一致，Chrome 会把保存的账号（实测 "admin"）灌进来且无视
          // autoComplete="off"。one-time-code 是 Chrome 真正尊重的非登录语义；
          // data-1p/lpignore 关掉 1Password / LastPass 注入。
          autoComplete="one-time-code"
          data-1p-ignore
          data-lpignore="true"
          name="multiSelectFilterQuery"
          value={open ? query : displayLabel}
          placeholder={open ? '输入关键字过滤…' : (allLabel || '全部')}
          readOnly={!open}
          onFocus={(event) => {
            event.target.removeAttribute('readonly');
            setOpen(true);
            setQuery('');
          }}
          onClick={() => { setOpen(true); }}
          onChange={(event) => {
            reportIfLooksLikeAutofill('multiselect-filter-filled', query, event.target.value, event);
            setOpen(true);
            setQuery(event.target.value);
          }}
          onKeyDown={onKeyDown}
        />
        {open ? (
          <SelectMenuPortal anchorRef={rootRef} open={open} menuRef={listRef} multiselect>
            {selected.length > 0 ? (
              <button
                type="button"
                className="searchable-select-option multi-clear"
                onMouseDown={(event) => { event.preventDefault(); onChange([]); }}
              >
                清除筛选（显示全部）
              </button>
            ) : null}
            {filtered.length === 0 ? (
              <div className="searchable-select-empty">无匹配项</div>
            ) : filtered.map((option, index) => {
              const checked = selected.includes(option.id);
              return (
                <button
                  key={option.id}
                  type="button"
                  role="option"
                  aria-selected={checked}
                  data-active={index === highlight ? 'true' : 'false'}
                  className={`searchable-select-option${checked ? ' selected' : ''}${index === highlight ? ' active' : ''}`}
                  onMouseEnter={() => setHighlight(index)}
                  onMouseDown={(event) => {
                    event.preventDefault();
                    toggleOption(option.id);
                  }}
                >
                  <span className={`multi-check${checked ? ' on' : ''}`}>{checked ? '✓' : ''}</span>
                  {option.icon ? <BrandIcon icon={option.icon} name={option.label} size={15} className="filter-option-icon" /> : null}
                  <span className="searchable-select-option-label">{option.label}</span>
                </button>
              );
            })}
          </SelectMenuPortal>
        ) : null}
      </div>
    </div>
  );
}

export function SearchableModelSelect({
  value,
  models,
  disabled,
  emptyLabel,
  onChange,
}: {
  value: string;
  models: Model[];
  disabled?: boolean;
  emptyLabel: string;
  onChange: (value: string) => void;
}) {
  const rootRef = React.useRef<HTMLDivElement | null>(null);
  const inputRef = React.useRef<HTMLInputElement | null>(null);
  const listRef = React.useRef<HTMLDivElement | null>(null);
  const [open, setOpen] = React.useState(false);
  const [query, setQuery] = React.useState('');
  const [highlight, setHighlight] = React.useState(0);

  const filtered = React.useMemo(() => filterModelOptions(models, query), [models, query]);
  const options = React.useMemo<SearchableModelOption[]>(() => {
    const empty: SearchableModelOption = { id: '', label: emptyLabel };
    if (!query.trim()) return [empty, ...filtered];
    const matchedEmpty = emptyLabel.toLowerCase().includes(query.trim().toLowerCase());
    return matchedEmpty ? [empty, ...filtered] : filtered;
  }, [emptyLabel, filtered, query]);

  const selectedModel = models.find((model) => model.id === value);
  const displayLabel = value
    ? (selectedModel ? modelSelectOptionLabel(selectedModel) : value)
    : emptyLabel;

  const close = React.useCallback(() => {
    setOpen(false);
    setQuery('');
    setHighlight(0);
  }, []);

  const selectOption = React.useCallback((next: string) => {
    onChange(next);
    close();
  }, [close, onChange]);

  React.useEffect(() => {
    if (!open) return;
    // 用捕获阶段监听：Modal 的 modal-card 会在冒泡阶段对 mousedown 调用
    // stopPropagation（防止点击弹窗内部误触发遮罩层的关闭逻辑），这会导致
    // 挂在 document 冒泡阶段的“点击外部关闭”监听永远收不到事件——弹窗内的
    // 下拉框因此选完选项后无法通过点击其他地方收起。捕获阶段先于冒泡阶段
    // 触发，不受该 stopPropagation 影响。
    const onPointerDown = (event: MouseEvent) => {
      const target = event.target as Node;
      if (rootRef.current?.contains(target) || listRef.current?.contains(target)) return;
      close();
    };
    document.addEventListener('mousedown', onPointerDown, true);
    return () => document.removeEventListener('mousedown', onPointerDown, true);
  }, [close, open]);

  React.useEffect(() => {
    if (!open) return;
    inputRef.current?.focus();
    inputRef.current?.select();
  }, [open]);

  React.useEffect(() => {
    setHighlight(0);
  }, [query, open]);

  React.useEffect(() => {
    if (!open || !listRef.current) return;
    const active = listRef.current.querySelector<HTMLElement>('[data-active="true"]');
    active?.scrollIntoView({ block: 'nearest' });
  }, [highlight, open, options]);

  const onKeyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'ArrowDown') {
      event.preventDefault();
      if (!open) {
        setOpen(true);
        return;
      }
      setHighlight((prev) => (options.length === 0 ? 0 : (prev + 1) % options.length));
      return;
    }
    if (event.key === 'ArrowUp') {
      event.preventDefault();
      if (!open) {
        setOpen(true);
        return;
      }
      setHighlight((prev) => (options.length === 0 ? 0 : (prev - 1 + options.length) % options.length));
      return;
    }
    if (event.key === 'Enter') {
      event.preventDefault();
      if (!open) {
        setOpen(true);
        return;
      }
      const option = options[highlight];
      if (option) selectOption(option.id);
      return;
    }
    if (event.key === 'Escape') {
      // 下拉框展开时：ESC 只收起下拉框，并阻止事件冒泡到 Modal 的全局 ESC 监听
      // （否则整个弹窗会被一起关掉）。未展开时不拦截，让 ESC 正常关闭弹窗。
      if (open) {
        event.preventDefault();
        event.stopPropagation();
        close();
      }
      return;
    }
  };

  return (
    <div className={`searchable-select${open ? ' open' : ''}`} ref={rootRef}>
      <input
        ref={inputRef}
        className="searchable-select-input"
        type="text"
        role="combobox"
        aria-expanded={open}
        aria-autocomplete="list"
        aria-controls="searchable-model-list"
        disabled={disabled}
        value={open ? query : displayLabel}
        placeholder={open ? '输入关键字筛选模型…' : emptyLabel}
        onFocus={() => {
          if (disabled) return;
          setOpen(true);
          setQuery('');
        }}
        onClick={() => {
          if (disabled) return;
          setOpen(true);
          setQuery('');
        }}
        onChange={(event) => {
          setOpen(true);
          setQuery(event.target.value);
        }}
        onKeyDown={onKeyDown}
      />
      {open ? (
        <SelectMenuPortal anchorRef={rootRef} open={open} menuRef={listRef} id="searchable-model-list">
          {options.length === 0 ? (
            <div className="searchable-select-empty">无匹配模型</div>
          ) : options.map((option, index) => (
            <button
              key={option.id || '__empty__'}
              type="button"
              role="option"
              aria-selected={option.id === value}
              data-active={index === highlight ? 'true' : 'false'}
              className={`searchable-select-option${option.id === value ? ' selected' : ''}${index === highlight ? ' active' : ''}`}
              onMouseEnter={() => setHighlight(index)}
              onMouseDown={(event) => {
                event.preventDefault();
                selectOption(option.id);
              }}
            >
              {option.label}
            </button>
          ))}
        </SelectMenuPortal>
      ) : null}
    </div>
  );
}

export type IconSelectOption = {
  value: string;
  label: string;
  // 品牌图标名（与 provider preset 的 icon 同一套文件）；空 = 没有厂商可指。
  icon?: string;
  // 第二行：同名项之间真正能认人的那部分（账号 / 邮箱 / 编号）。
  description?: string;
  // 选项右侧的处理（协议名、价格档位这类分类标签）。
  meta?: string;
  // 搜索用的额外关键字（id、账号全称），展示时不用。
  keywords?: string;
};

/**
 * IconSelect：能带品牌图标的单选框。原生 <select> 的 <option> 放不进图标，所以
 * 触发器和菜单都自己画；外观直接复用 .searchable-select（高度、边框、聚焦环、
 * 下拉开合），和筛选栏里的下拉是同一套观感。
 *
 * 同一个服务商多登几个账号就会出现一串同名条目，光靠 label 分不出是谁——
 * 所以每项还能带一行 description（账号 / 邮箱 / 编号）和一枚 meta（协议）。
 * 选项一多就打开搜索，按 label、description、keywords 一起过滤。
 */
export function IconSelect({
  label,
  value,
  options,
  onChange,
  disabled,
  fullWidth,
  hint,
  placeholder = '请选择',
  searchable,
  renderPortal = true,
}: {
  label?: string;
  value: string;
  options: IconSelectOption[];
  onChange: (value: string) => void;
  disabled?: boolean;
  fullWidth?: boolean;
  // 字段说明：收进标签旁的 InfoTip，不占一行纵向空间。
  hint?: string;
  placeholder?: string;
  // 缺省时按选项数量决定；页面筛选栏这类常驻控件可以显式关掉。
  searchable?: boolean;
  // 弹窗里的下拉必须挂到 body，否则被 modal-card 的 overflow 裁掉；
  // 页面内联的筛选框直接就地展开，省掉跟随滚动重定位的那套。
  renderPortal?: boolean;
}) {
  const rootRef = React.useRef<HTMLDivElement | null>(null);
  const listRef = React.useRef<HTMLDivElement | null>(null);
  const inputRef = React.useRef<HTMLInputElement | null>(null);
  const [open, setOpen] = React.useState(false);
  const [query, setQuery] = React.useState('');
  const [highlight, setHighlight] = React.useState(0);

  // 筛选栏的选项本来就一屏放得下，默认不弹搜索框；长列表才需要。
  const canSearch = searchable ?? options.length > 12;
  const selected = options.find((option) => option.value === value);
  // 带说明的选项（同名 Provider 靠短 ID 区分）：菜单里分两行更好读，但收起后的
  // 触发器只有一行高 —— 说明就是认人的那个值（账号），不能藏在「名称 · 描述」
  // 拼接出来的省略号里：名称让位，账号始终完整显示。
  const selectedInline = selected ? [selected.label, selected.description].filter(Boolean).join(' · ') : '';

  const filtered = React.useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (!needle) return options;
    return options.filter((option) =>
      option.label.toLowerCase().includes(needle) ||
      (option.description || '').toLowerCase().includes(needle) ||
      (option.meta || '').toLowerCase().includes(needle) ||
      (option.keywords || '').toLowerCase().includes(needle));
  }, [options, query]);

  const close = React.useCallback(() => {
    setOpen(false);
    setQuery('');
  }, []);

  const pick = React.useCallback((option: IconSelectOption | undefined) => {
    if (!option) return;
    onChange(option.value);
    close();
  }, [close, onChange]);

  React.useEffect(() => {
    if (disabled) setOpen(false);
  }, [disabled]);

  React.useEffect(() => {
    if (!open) return;
    setQuery('');
    const index = options.findIndex((option) => option.value === value);
    setHighlight(index >= 0 ? index : 0);
    if (canSearch) inputRef.current?.focus();
  }, [open]);

  React.useEffect(() => {
    if (!open) return;
    // 捕获阶段监听，理由同 MultiSelectFilter：Modal 的 modal-card 在冒泡阶段
    // stopPropagation，挂在冒泡阶段的「点击外部关闭」收不到弹窗内的点击。
    const onPointerDown = (event: MouseEvent) => {
      const target = event.target as Node;
      if (rootRef.current?.contains(target) || listRef.current?.contains(target)) return;
      close();
    };
    document.addEventListener('mousedown', onPointerDown, true);
    return () => document.removeEventListener('mousedown', onPointerDown, true);
  }, [close, open]);

  React.useEffect(() => {
    if (!open || !listRef.current) return;
    const active = listRef.current.querySelector<HTMLElement>('[data-active="true"]');
    active?.scrollIntoView({ block: 'nearest' });
  }, [highlight, open, filtered]);

  const move = (delta: number) => {
    setHighlight((prev) => (filtered.length === 0 ? 0 : (prev + delta + filtered.length) % filtered.length));
  };

  const onKeyDown = (event: React.KeyboardEvent<HTMLElement>) => {
    if (disabled) return;
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      if (!open) { setOpen(true); return; }
      move(event.key === 'ArrowDown' ? 1 : -1);
      return;
    }
    if (event.key === 'Enter') {
      event.preventDefault();
      if (!open) { setOpen(true); return; }
      pick(filtered[highlight]);
      return;
    }
    if (event.key === 'Escape') {
      // 下拉展开时 ESC 只收起下拉框，不能冒泡到 Modal 的全局 ESC 把弹窗关掉。
      if (open) {
        event.preventDefault();
        event.stopPropagation();
        close();
      }
    }
  };

  const searchField = open && canSearch ? (
    <div className="icon-select-search">
      <input
        ref={inputRef}
        type="text"
        value={query}
        placeholder="搜索名称、账号或 ID…"
        autoComplete="one-time-code"
        data-1p-ignore
        data-lpignore="true"
        onChange={(event) => { setQuery(event.target.value); setHighlight(0); }}
        onKeyDown={onKeyDown}
      />
    </div>
  ) : null;

  const rows = filtered.length === 0 ? (
    <div className="searchable-select-empty">无匹配项</div>
  ) : filtered.map((option, index) => {
    const isSelected = option.value === value;
    return (
      // 行用 div 而不是 button：某些浏览器（实测本机的 in-app 引擎）对 flex 容器里的
      // <button> 会退回 inline-block，图标和文字各自成块、行高忽大忽小。按钮语义
      // 由 role="option" + 键盘处理保留，点击仍走 onMouseDown。
      <div
        key={option.value || '__empty__'}
        role="option"
        tabIndex={-1}
        aria-selected={isSelected}
        data-active={index === highlight ? 'true' : 'false'}
        className={`icon-select-option${isSelected ? ' selected' : ''}${index === highlight ? ' active' : ''}`}
        onMouseEnter={() => setHighlight(index)}
        onMouseDown={(event) => {
          event.preventDefault();
          pick(option);
        }}
      >
        <BrandIcon icon={option.icon} name={option.label} size={16} className="filter-option-icon" />
        <span className="icon-select-text">
          <span className="icon-select-line">
            <span className="icon-select-name">{option.label}</span>
            {option.meta ? <span className="icon-select-meta">{option.meta}</span> : null}
          </span>
          {option.description ? <span className="icon-select-sub">{option.description}</span> : null}
        </span>
      </div>
    );
  });

  const menu = (
    <>
      {searchField}
      {rows}
    </>
  );

  return (
    <div className={`field${fullWidth ? ' field-full' : ''}`}>
      {label ? (
        <div className="field-label-row">
          <label>{label}</label>
          {hint ? <InfoTip text={hint} /> : null}
        </div>
      ) : null}
      <div className={`searchable-select icon-select${open ? ' open' : ''}`} ref={rootRef}>
        <button
          type="button"
          className="icon-select-trigger"
          role="combobox"
          aria-expanded={open}
          aria-haspopup="listbox"
          disabled={disabled}
          onClick={() => setOpen((current) => !current)}
          onKeyDown={onKeyDown}
        >
          {selected ? (
            <>
              <BrandIcon icon={selected.icon} name={selected.label} size={16} />
              <span className="icon-select-line">
                <span className="icon-select-name" title={selectedInline}>
                  {selected.description && selected.description !== selected.label ? (
                    <>
                      <span className="icon-select-name-text">{selected.label}</span>
                      <span className="icon-select-trigger-account">{selected.description}</span>
                    </>
                  ) : selected.label}
                </span>
                {selected.meta ? <span className="icon-select-meta">{selected.meta}</span> : null}
              </span>
            </>
          ) : (
            <span className="icon-select-name muted" title={placeholder}>{placeholder}</span>
          )}
        </button>
        {renderPortal ? (
          <SelectMenuPortal anchorRef={rootRef} open={open} menuRef={listRef}>{menu}</SelectMenuPortal>
        ) : open ? (
          <div className="searchable-select-menu inline" role="listbox" ref={listRef}>{menu}</div>
        ) : null}
      </div>
    </div>
  );
}
