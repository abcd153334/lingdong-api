import { useEffect, useRef, type KeyboardEvent } from "react";
import { cn } from "@/lib/utils";
import { Icon, type IconName } from "./Icon";

export interface TabItem<K extends string = string> {
  key: K;
  label: string;
  /** 计数徽标：仅视觉提示，aria-hidden（不能进可访问名，否则 name:"插件" 精确匹配会失败） */
  count?: number;
  /**
   * 未保存标记（小圆点）：aria-hidden，理由同上 —— 页签的可访问名必须只有文字，
   * 否则 `getByRole("tab", { name: "站点", exact: true })` 这类断言会全部失配。
   */
  mark?: boolean;
  disabled?: boolean;
  /** 前置图标（纯装饰，aria-hidden；可访问名仍只有 label） */
  icon?: IconName;
}

export interface TabsProps<K extends string = string> {
  items: readonly TabItem<K>[];
  value: string;
  /**
   * 参数类型跟着 items 的 key 走，而不是 string。
   *
   * 用 string 时，`onChange={setTab}`（setTab 的参数是 "groups"|"guides"|"rules"）
   * 会因为「函数参数逆变」报 TS2322 —— 而这恰恰说明 Tabs 只会回调自己 items 里的 key，
   * 把类型收窄才是对的（宽松的 string 会让 `setTab("拼错的 key")` 也能编译通过）。
   */
  onChange: (key: K) => void;
  /** line=线性选项卡（页内分组）；pill=胶囊选项卡 */
  variant?: "line" | "pill";
  className?: string;
  /**
   * 生成稳定 id：`{idPrefix}-tab-{key}`。
   * 必须传**字面量**（门禁 M18 静态扫描同页重复）；不要用 useId()（会产出 `:r7:` 这类带冒号的 id）。
   */
  idPrefix?: string;
  /** tablist 的可访问名（多组页签同页时必填） */
  label?: string;
  /** 真实面板的 id；只有选中项挂 aria-controls（未选中项指向不存在的 id 是错误） */
  panelId?: string;
}

/** 分组选项卡（对齐原型 admin-config TAB / pill-tabs 两型）；纯受控，不依赖 router */
export function Tabs<K extends string = string>({
  items,
  value,
  onChange,
  variant = "line",
  className,
  idPrefix,
  label,
  panelId,
}: TabsProps<K>) {
  const btnRefs = useRef(new Map<string, HTMLButtonElement>());
  const listRef = useRef<HTMLDivElement>(null);

  const enabled = items.filter((it) => !it.disabled);

  /**
   * 选中页签必须滚入视野。
   *
   * 为什么必须有：窄屏（≤1023）与弹窗内的页签条恒 `nowrap + overflow-x:auto`，
   * 从别处切到一个在视口外的页签时，画面只换了面板内容、页签条还停着，
   * 用户会以为「点了没反应」（应用管理弹窗的插件页签就是这种场景）。
   *
   * 只横向调整 tablist 自己的 `scrollLeft`，**不用 `scrollIntoView`**：
   * 后者会把所有可滚祖先一起滚，表现为整页被顶一下。
   * 未布局（隐藏面板、jsdom）时 rect 宽为 0，直接跳过。
   */
  useEffect(() => {
    const list = listRef.current;
    const btn = btnRefs.current.get(value);
    if (!list || !btn) return;
    const box = list.getBoundingClientRect();
    const t = btn.getBoundingClientRect();
    if (!box.width || !t.width) return;
    if (t.left < box.left) list.scrollLeft -= box.left - t.left + 8;
    else if (t.right > box.right) list.scrollLeft += t.right - box.right + 8;
  }, [value, items]);

  /** 方向键循环 + Home/End；自动激活（切换即 onChange），焦点跟随 */
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!enabled.length) return;
    const cur = enabled.findIndex((it) => it.key === value);
    const last = enabled.length - 1;
    let nextIdx = -1;
    if (e.key === "ArrowRight") nextIdx = cur < 0 ? 0 : (cur + 1) % (last + 1);
    else if (e.key === "ArrowLeft") nextIdx = cur < 0 ? last : (cur - 1 + last + 1) % (last + 1);
    else if (e.key === "Home") nextIdx = 0;
    else if (e.key === "End") nextIdx = last;
    if (nextIdx < 0) return;
    e.preventDefault();
    const next = enabled[nextIdx];
    if (next.key !== value) onChange(next.key);
    btnRefs.current.get(next.key)?.focus();
  };

  return (
    <div
      ref={listRef}
      className={cn(variant === "line" ? "tabs" : "pill-tabs", className)}
      role="tablist"
      aria-label={label}
      onKeyDown={onKeyDown}
    >
      {items.map((it) => {
        const selected = it.key === value;
        return (
          <button
            key={it.key}
            ref={(el) => {
              if (el) btnRefs.current.set(it.key, el);
              else btnRefs.current.delete(it.key);
            }}
            id={idPrefix ? `${idPrefix}-tab-${it.key}` : undefined}
            type="button"
            role="tab"
            aria-selected={selected}
            /* 文案单独包一层 span：窄屏下允许 `.tab__txt{display:none}` 只留图标。
               display:none 的文字会从可访问名里消失，所以 aria-label 必须显式给 ——
               这样「图标化」不会把页签变成无名按钮。 */
            aria-label={it.label}
            aria-controls={panelId && selected ? panelId : undefined}
            disabled={it.disabled}
            tabIndex={it.disabled ? -1 : selected ? 0 : -1}
            className={cn("tab", selected && "active")}
            onClick={() => onChange(it.key)}
          >
            {it.icon ? <Icon name={it.icon} size={14} className="tab-ico" /> : null}
            <span className="tab__txt">{it.label}</span>
            {it.mark ? <span className="tab-dot" aria-hidden="true" /> : null}
            {typeof it.count === "number" ? (
              <span className="tab-count" aria-hidden="true">
                {it.count}
              </span>
            ) : null}
          </button>
        );
      })}
    </div>
  );
}
