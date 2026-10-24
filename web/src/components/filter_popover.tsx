// FilterPopoverSelect — 筛选栏里的多选下拉（弹出式）。
//
// 为什么不复用 MultiSelectFilter：那个控件的输入框宽度就是筛选栏那一格（约 130px），
// 下拉菜单又跟着输入框同宽，于是 Provider 名称一律被截成「Cur…」「Wor…」。这里把
// 菜单放进一个独立的弹出面板（宽 280px、右对齐并夹在视口内），名称能完整显示；
// 选项多时才在顶部出现搜索框，选项少时直接列出，不占额外一行。
import React from 'react';
import {createPortal} from 'react-dom';
import {BrandIcon} from './ui';
import type {MultiFilterOption} from '../types';

export type PopoverFilterOption = MultiFilterOption & {
  /** 第二行：同名项之间靠它认人（账号 / 短 ID）。 */
  hint?: string;
};

export function FilterPopoverSelect({
  label,
  options,
  selected,
  onChange,
  allLabel,
  emptyText = '无匹配项',
}: {
  label: string;
  options: PopoverFilterOption[];
  selected: string[];
  onChange: (next: string[]) => void;
  allLabel: string;
  emptyText?: string;
}) {
  const triggerRef = React.useRef<HTMLButtonElement | null>(null);
  const panelRef = React.useRef<HTMLDivElement | null>(null);
  const searchRef = React.useRef<HTMLInputElement | null>(null);
  const [open, setOpen] = React.useState(false);
  const [query, setQuery] = React.useState('');
  const [pos, setPos] = React.useState<{top: number; left: number; width: number; up: boolean} | null>(null);

  const canSearch = options.length > 8;
  const filtered = React.useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (!needle) return options;
    return options.filter((option) => option.label.toLowerCase().includes(needle) || option.id.toLowerCase().includes(needle));
  }, [options, query]);

  const display = React.useMemo(() => {
    if (selected.length === 0) return allLabel;
    const names = selected.map((id) => options.find((option) => option.id === id)?.label || id);
    return names.length === 1 ? names[0] : `${names[0]} +${names.length - 1}`;
  }, [selected, options, allLabel]);

  // 面板定位：跟随触发按钮，右对齐并夹在视口内；下方放不下就向上展开。
  const place = React.useCallback(() => {
    const trigger = triggerRef.current;
    if (!trigger) return;
    const rect = trigger.getBoundingClientRect();
    const width = Math.min(300, Math.max(240, window.innerWidth - 24));
    const estimated = Math.min(options.length, 10) * 32 + (canSearch ? 46 : 0) + 14;
    const below = window.innerHeight - rect.bottom;
    const up = below < estimated + 12 && rect.top > below;
    const left = Math.max(12, Math.min(rect.right - width, window.innerWidth - width - 12));
    setPos({top: up ? undefined! : rect.bottom + 6, left, width, up});
  }, [options.length, canSearch]);

  React.useEffect(() => {
    if (!open) return undefined;
    place();
    const onDown = (event: MouseEvent) => {
      const target = event.target as Node;
      if (triggerRef.current?.contains(target) || panelRef.current?.contains(target)) return;
      setOpen(false);
    };
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape' && !searchRef.current?.value) {
        event.stopPropagation();
        setOpen(false);
      }
    };
    const reposition = () => place();
    document.addEventListener('mousedown', onDown, true);
    document.addEventListener('keydown', onKey);
    window.addEventListener('resize', reposition);
    window.addEventListener('scroll', reposition, true);
    return () => {
      document.removeEventListener('mousedown', onDown, true);
      document.removeEventListener('keydown', onKey);
      window.removeEventListener('resize', reposition);
      window.removeEventListener('scroll', reposition, true);
    };
  }, [open, place]);

  React.useEffect(() => {
    if (!open) return;
    setQuery('');
    if (canSearch) window.setTimeout(() => searchRef.current?.focus(), 0);
  }, [open, canSearch]);

  const toggle = (id: string) => {
    onChange(selected.includes(id) ? selected.filter((item) => item !== id) : [...selected, id]);
  };

  return (
    <div className="akc-pop-filter">
      <button
        ref={triggerRef}
        type="button"
        className={`akc-pop-trigger${open ? ' open' : ''}${selected.length > 0 ? ' has-value' : ''}`}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-label={label}
        title={`${label}：${display}`}
        onClick={() => setOpen((value) => !value)}
      >
        <span className="akc-pop-trigger-text">{display}</span>
        <svg viewBox="0 0 12 12" width="12" height="12" aria-hidden="true" className="akc-pop-caret">
          <path d="M3 4.5 6 7.5 9 4.5" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      </button>
      {open && pos ? createPortal(
        <div
          ref={panelRef}
          className={`akc-pop-panel${pos.up ? ' up' : ''}`}
          role="listbox"
          aria-multiselectable="true"
          aria-label={label}
          style={{
            left: pos.left,
            width: pos.width,
            ...(pos.up
              ? {bottom: window.innerHeight - (triggerRef.current?.getBoundingClientRect().top || 0) + 6}
              : {top: pos.top}),
          }}
        >
          {canSearch ? (
            <div className="akc-pop-search">
              <input
                ref={searchRef}
                type="text"
                value={query}
                placeholder="输入关键字过滤…"
                autoComplete="one-time-code"
                data-1p-ignore
                data-lpignore="true"
                name="filterPopoverQuery"
                onChange={(event) => setQuery(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === 'Enter' && filtered.length === 1) {
                    event.preventDefault();
                    toggle(filtered[0].id);
                  }
                }}
              />
            </div>
          ) : null}
          <div className="akc-pop-list">
            {selected.length > 0 ? (
              <button type="button" className="akc-pop-option clear" onMouseDown={(event) => event.preventDefault()} onClick={() => onChange([])}>
                清除筛选（显示全部）
              </button>
            ) : null}
            {filtered.length === 0 ? (
              <div className="akc-pop-empty">{emptyText}</div>
            ) : filtered.map((option) => {
              const checked = selected.includes(option.id);
              return (
                <button
                  key={option.id}
                  type="button"
                  role="option"
                  aria-selected={checked}
                  className={`akc-pop-option${checked ? ' selected' : ''}`}
                  onMouseDown={(event) => event.preventDefault()}
                  onClick={() => toggle(option.id)}
                >
                  <span className={`akc-pop-check${checked ? ' on' : ''}`} aria-hidden="true">{checked ? '✓' : ''}</span>
                  {option.icon ? <BrandIcon icon={option.icon} name={option.label} size={15} className="filter-option-icon" /> : null}
                  <span className="akc-pop-text">
                    <span className="akc-pop-label" title={option.label}>{option.label}</span>
                    {option.hint ? <span className="akc-pop-hint">{option.hint}</span> : null}
                  </span>
                </button>
              );
            })}
          </div>
        </div>,
        document.body,
      ) : null}
    </div>
  );
}
