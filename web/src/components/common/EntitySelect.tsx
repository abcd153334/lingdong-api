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
  /**
   * 可搜索（默认 false，既有调用点行为零变化）。
   *
   * 打开后触发器变成**可编辑输入框**（`input[role=combobox]`），按 label / hint
   * 做客户端过滤。为什么焦点必须留在输入框上、而不是面板里的搜索框：
   * 面板经 `Popover` portal 到 body，落在 `Dialog` 子树**之外** ——
   * 弹窗的焦点环与移动端 Sheet 都会把焦点从面板里抢回去（`Popover` 的注释
   * 记的就是这个坑）。输入框在页面内，Tab/焦点环一切照旧。
   *
   * 选项量大时调用方才需要它（应用筛选）；模型/渠道这类"靠图认"的选择
   * 用默认的 select-only 形态更好 —— 键盘直接翻，不用先输入。
   */
  searchable?: boolean;
}

/**
 * 实体下拉（带图标）：全项目**唯一**的「从一组实体里选一个」控件。
 *
 * 为什么不用原生 `<select>`：`<option>` 里放不了图片。模型/渠道这类选择
 * 恰恰是"靠图认"的（名字常常长到被截断），而原生下拉只能给一串字符。
 * 也不自己实现 popup 定位：那是 `Popover` 的事（portal + 翻转 + 关闭语义）。
 *
 * 两种键盘模型，互不混用：
 *   - 默认（select-only combobox）：焦点留在触发器上，用 `aria-activedescendant`
 *     指向高亮项。portal 出去的面板因此不需要接管焦点，不会和外层弹窗的焦点环打架。
 *   - `searchable`：触发器本身是输入框（editable combobox），焦点天然在页面内，
 *     输入即过滤，↑↓ 移动、Enter 提交、Esc 关闭。
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
  searchable = false,
}: EntitySelectProps) {
  const [open, setOpen] = useState(false);
  /** 高亮项下标（-1 = 还没开始用键盘） */
  const [active, setActive] = useState(-1);
  /** 搜索词（仅 searchable 使用；关闭时清空，输入框改显示已选项的 label） */
  const [q, setQ] = useState("");
  const triggerRef = useRef<HTMLButtonElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const uid = useId();
  const listId = `${uid}-list`;
  const optId = (i: number): string => `${uid}-opt-${i}`;

  // 下拉项 = （可选的「全部」行）+ 真实实体行。`plain` 行没有图标，
  // 选择器对它只显示文字（见 allLabel 的注释）。
  type Row = EntityOption & { plain?: boolean };
  const rows: Row[] = allLabel ? [{ value: "", label: allLabel, plain: true }, ...options] : options;

  // 可搜索时的可见行：按 label + hint 过滤（hint 常放 slug/厂商名，用户会按它搜）。
  // 「全部」项也参与过滤，避免"输入了字就清不掉筛选"。
  const kw = q.trim().toLowerCase();
  const visible: Row[] = searchable && kw !== ""
    ? rows.filter((o) => `${o.label} ${o.hint ?? ""}`.toLowerCase().includes(kw))
    : rows;

  const selected = rows.find((o) => o.value === value) ?? null;
  const selectedIdx = rows.findIndex((o) => o.value === value);
  const selectedText = selected?.label ?? "";

  const openPanel = (): void => {
    if (disabled) return;
    setActive(selectedIdx >= 0 ? selectedIdx : 0);
    setOpen(true);
  };

  const close = (): void => setOpen(false);

  const commit = (i: number): void => {
    const opt = visible[i];
    if (!opt) return;
    // 选同一个值也关面板：用户的意图是"选它"，重复点不该没有反馈。
    onChange(opt.value);
    setOpen(false);
    setQ("");
    (searchable ? inputRef.current : triggerRef.current)?.focus();
  };

  /** 移动高亮项并把面板滚到位（长列表里方向键要能"跟着走"）。 */
  const move = (next: number): void => {
    if (visible.length === 0) return;
    const i = (next + visible.length) % visible.length;
    setActive(i);
    listRef.current
      ?.querySelector<HTMLElement>(`[data-idx="${i}"]`)
      ?.scrollIntoView({ block: "nearest" });
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLElement>): void => {
    if (disabled) return;
    if (!open) {
      // 可搜索模式下这几个键是**输入字符**，不能当快捷键吃掉
      // （Esc 关掉面板后焦点还在输入框里，此时按空格就是把空格打进搜索词）。
      if (searchable) {
        if (e.key === "ArrowDown") {
          e.preventDefault();
          openPanel();
        }
        return;
      }
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
        move(visible.length - 1);
        break;
      case "Enter":
      case " ":
        // 空格在可搜索模式下是搜索字符，不能当提交键。
        if (searchable && e.key === " ") return;
        e.preventDefault();
        commit(active);
        break;
      case "Escape":
        // 必须吃掉：外层 Dialog 在 document 上监听 Esc，不拦会连带关掉整个弹窗。
        e.preventDefault();
        e.stopPropagation();
        setOpen(false);
        setQ("");
        break;
      case "Tab":
        // 焦点离开 = 放弃选择（不提交）。stopPropagation 的理由同上：
        // 外层弹窗的焦点环会把 portal 里的节点当"外面"，从而抢焦点。
        e.stopPropagation();
        setOpen(false);
        setQ("");
        break;
      default:
        break;
    }
  };

  /** 选项行（两个分支共用同一份 DOM 契约，避免改一处漏一处）。 */
  const optionRow = (o: Row, i: number) => (
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
      {o.plain ? null : <EntityIcon url={o.iconUrl} name={o.label} color={o.color} size="xs" />}
      <span className="esel-opt-t">{o.label}</span>
      {o.hint ? <span className="esel-opt-h">{o.hint}</span> : null}
      {o.value === value ? <Icon name="check" size={13} className="esel-check" /> : null}
    </div>
  );

  const panel = (
    <Popover
      open={open}
      onClose={close}
      anchorRef={searchable ? inputRef : triggerRef}
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
        {visible.length === 0 ? (
          <div className="esel-none">{searchable && kw !== "" ? "没有匹配的项" : emptyText}</div>
        ) : (
          visible.map((o, i) => optionRow(o, i))
        )}
      </div>
    </Popover>
  );

  if (searchable) {
    return (
      <>
        <div className={cn("esel esel--search", open && "is-open", className)}>
          <input
            id={id}
            ref={inputRef}
            type="text"
            role="combobox"
            className="esel-input"
            // 打开时显示搜索词（空），关闭时显示已选项的 label —— 这样"当前筛的是谁"
            // 一眼可见，而一旦开始输入就让位给搜索词。
            value={open ? q : selectedText}
            placeholder={selectedText || placeholder}
            aria-expanded={open}
            aria-controls={open ? listId : undefined}
            aria-haspopup="listbox"
            aria-autocomplete="list"
            aria-label={ariaLabel}
            aria-activedescendant={open && active >= 0 && visible[active] ? optId(active) : undefined}
            disabled={disabled}
            onFocus={openPanel}
            onChange={(e) => {
              setQ(e.target.value);
              // 高亮回到第一项：仍停在下标 5 会让人以为"没过滤"。
              setActive(0);
              if (!open) setOpen(true);
            }}
            onKeyDown={onKeyDown}
          />
          <Icon name="chevronDown" size={14} className={cn("esel-caret", open && "is-open")} />
        </div>
        {panel}
      </>
    );
  }

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
      {panel}
    </>
  );
}
