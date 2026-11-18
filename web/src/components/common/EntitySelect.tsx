import { useId, useRef, useState } from "react";

import { EntityCell } from "./EntityCell";
import { EntityIcon } from "./EntityIcon";
import { Icon } from "./Icon";
import { Popover } from "./Popover";
import { cn } from "@/lib/utils";

export interface EntityOption {
  /** 提交值（模型 id / 渠道 id） */
  value: string;
  /** 展示名 */
  label: string;
  /** 图标地址（空 = 首字占位） */
  iconUrl?: string;
  /** 品牌色（首字占位底色） */
  color?: string;
  /** 右侧补充说明（如厂商名、模态） */
  hint?: string;
}

export interface EntitySelectProps {
  id?: string;
  value: string;
  options: EntityOption[];
  onChange: (value: string) => void;
  /** 无选中值时的占位文案 */
  placeholder?: string;
  /**
   * 「全部 / 不限」项的文案：提供后列表顶部多一项 `value=""`，选中它等于清空。
   *
   * 筛选用得上（与 `Select` 的 `allLabel` 同一契约）。刻意**不接受**调用方
   * 自己往 `options` 里塞一个空值项：那一项没有图标，`EntityCell` 会给它渲染
   * 一个「全」字方块，而它根本不是一个实体。
   */
  allLabel?: string;
  /**
   * 面板（listbox）的可访问名。**默认取 placeholder**，但那是"请选择"，
   * 读屏念出来不知道在选什么 —— 有可见 label 时请一并传（与 `<label for>` 一致）。
   */
  ariaLabel?: string;
  disabled?: boolean;
  className?: string;
  /** 无选项时的提示（默认「暂无可选项」） */
  emptyText?: string;
}

/**
 * 实体下拉（带图标）：全项目**唯一**的「从一组实体里选一个」控件。
 *
 * 为什么不用原生 `<select>`：`<option>` 里放不了图片。模型/渠道这类选择
 * 恰恰是"靠图认"的（名字常常长到被截断），而原生下拉只能给一串字符。
 * 也不自己实现 popup 定位：那是 `Popover` 的事（portal + 翻转 + 关闭语义）。
 *
 * 键盘模型用 **select-only combobox**（WAI-ARIA 1.2）：
 * 焦点**始终留在触发器上**，用 `aria-activedescendant` 指向高亮项 ——
 * 这样 portal 出去的面板不需要接管焦点，也就不会和外层弹窗的焦点环打架。
 *
 * 无障碍契约（缺一条都会让读屏用户用不了）：
 *   - 触发器 `role="combobox"` + `aria-expanded` + `aria-controls`；
 *   - 面板 `role="listbox"`，选项 `role="option"` + `aria-selected`；
 *   - 展开时 `aria-activedescendant` 指向"当前高亮项"的 id（不是焦点项）。
 */
export function EntitySelect({
  id,
  value,
  options,
  onChange,
  placeholder = "请选择",
  allLabel,
  ariaLabel,
  disabled = false,
  className,
  emptyText = "暂无可选项",
}: EntitySelectProps) {
  const [open, setOpen] = useState(false);
  /** 高亮项下标（-1 = 还没开始用键盘） */
  const [active, setActive] = useState(-1);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const uid = useId();
  const listId = `${uid}-list`;
  const optId = (i: number): string => `${uid}-opt-${i}`;

  // 下拉项 = （可选的「全部」行）+ 真实实体行。`plain` 行没有图标，
  // 选择器对它只显示文字（见 allLabel 的注释）。
  type Row = EntityOption & { plain?: boolean };
  const rows: Row[] = allLabel ? [{ value: "", label: allLabel, plain: true }, ...options] : options;

  const selected = rows.find((o) => o.value === value) ?? null;
  const selectedIdx = rows.findIndex((o) => o.value === value);

  const openPanel = (): void => {
    if (disabled) return;
    setActive(selectedIdx >= 0 ? selectedIdx : 0);
    setOpen(true);
  };

  const close = (): void => setOpen(false);

  const commit = (i: number): void => {
    const opt = rows[i];
    if (!opt) return;
    // 选同一个值也关面板：用户的意图是"选它"，重复点不该没有反馈。
    onChange(opt.value);
    setOpen(false);
    triggerRef.current?.focus();
  };

  /** 移动高亮项并把面板滚到位（长列表里方向键要能"跟着走"）。 */
  const move = (next: number): void => {
    if (rows.length === 0) return;
    const i = (next + rows.length) % rows.length;
    setActive(i);
    listRef.current
      ?.querySelector<HTMLElement>(`[data-idx="${i}"]`)
      ?.scrollIntoView({ block: "nearest" });
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLButtonElement>): void => {
    if (disabled) return;
    if (!open) {
      if (e.key === "Enter" || e.key === " " || e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        openPanel();
      }
      return;
    }
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        move(active + 1);
        break;
      case "ArrowUp":
        e.preventDefault();
        move(active - 1);
        break;
      case "Home":
        e.preventDefault();
        move(0);
        break;
      case "End":
        e.preventDefault();
        move(rows.length - 1);
        break;
      case "Enter":
      case " ":
        e.preventDefault();
        commit(active);
        break;
      case "Escape":
        // 必须吃掉：外层 Dialog 在 document 上监听 Esc，不拦会连带关掉整个弹窗。
        e.preventDefault();
        e.stopPropagation();
        setOpen(false);
        break;
      case "Tab":
        // 焦点离开 = 放弃选择（不提交）。stopPropagation 的理由同上：
        // 外层弹窗的焦点环会把 portal 里的节点当"外面"，从而抢焦点。
        e.stopPropagation();
        setOpen(false);
        break;
      default:
        break;
    }
  };

  return (
    <>
      <button
        id={id}
        ref={triggerRef}
        type="button"
        role="combobox"
        className={cn("esel", open && "is-open", className)}
        aria-expanded={open}
        aria-controls={open ? listId : undefined}
        aria-haspopup="listbox"
        // 触发器自己也要名字：只有 `aria-activedescendant` + 面板名的话，
        // 读屏念的是"自动选择"，听不出这是在选模型还是选渠道。
        // 给了 ariaLabel 就用它覆盖内容名（与 `<label for>` 同义）。
        aria-label={ariaLabel}
        aria-activedescendant={open && active >= 0 ? optId(active) : undefined}
        disabled={disabled}
        onClick={() => (open ? close() : openPanel())}
        onKeyDown={onKeyDown}
      >
        {selected ? (
          selected.plain ? (
            <span className="esel-plain">{selected.label}</span>
          ) : (
            <EntityCell iconUrl={selected.iconUrl} name={selected.label} color={selected.color} />
          )
        ) : (
          <span className="esel-empty">{placeholder}</span>
        )}
        <Icon name="chevronDown" size={14} className={cn("esel-caret", open && "is-open")} />
      </button>

      <Popover
        open={open}
        onClose={close}
        anchorRef={triggerRef}
        id={listId}
        role="listbox"
        ariaLabel={ariaLabel ?? placeholder}
        className="esel-pop"
        matchWidth
        maxHeight={320}
        prefer="below"
        gap={4}
      >
        <div className="esel-list" ref={listRef}>
          {rows.length === 0 ? (
            <div className="esel-none">{emptyText}</div>
          ) : (
            rows.map((o, i) => (
              <div
                key={o.value}
                id={optId(i)}
                data-idx={i}
                role="option"
                aria-selected={o.value === value}
                className={cn("esel-opt", o.value === value && "is-on", i === active && "is-active")}
                // 高亮跟随鼠标：键盘高亮与鼠标悬停同时存在时，用户会以为"选中的是悬停那个"
                onPointerMove={() => setActive(i)}
                onClick={() => commit(i)}
              >
                {o.plain ? null : (
                  <EntityIcon url={o.iconUrl} name={o.label} color={o.color} size="xs" />
                )}
                <span className="esel-opt-t">{o.label}</span>
                {o.hint ? <span className="esel-opt-h">{o.hint}</span> : null}
                {o.value === value ? <Icon name="check" size={13} className="esel-check" /> : null}
              </div>
            ))
          )}
        </div>
      </Popover>
    </>
  );
}
