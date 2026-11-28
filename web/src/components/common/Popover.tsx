import { useEffect, useRef, type CSSProperties, type ReactNode, type RefObject } from "react";
import { createPortal } from "react-dom";
import { cn } from "@/lib/utils";
import { useOverlayOpen } from "./overlayContext";
import { useAnchoredLayerBox } from "./useAnchoredLayer";

/** 允许下发自定义属性（尖角位置走 `--pop-arrow-x`，由 CSS 消费）。 */
type PopStyle = CSSProperties & Record<`--${string}`, string | number>;

/** 面板内可聚焦元素：`tabIndex=-1` 的容器本身不算。 */
const FOCUSABLE =
  'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';

export interface PopoverProps {
  open: boolean;
  onClose: () => void;
  /** 触发器节点：定位基准 + 关闭后的焦点落点 */
  anchorRef: RefObject<HTMLElement | null>;
  /**
   * `prefer="right"` 时的纵向基准，以及关闭后的焦点落点（缺省回 `anchorRef`）。
   *
   * 为什么要把"关掉后焦点回哪"单独给一个 prop：二级导航的水平锚是整栏 `<aside>`，
   * 它不可聚焦，回焦到它等于把焦点丢到 body —— 键盘用户按 Esc 之后就找不到原位置了。
   */
  alignRef?: RefObject<HTMLElement | null>;
  returnFocusRef?: RefObject<HTMLElement | null>;
  /** 值变化即重新定位（锚点换了但 `open` 没变时用，例如从 A 卡切到 B 卡） */
  repositionKey?: unknown;
  children: ReactNode;
  /** 附加类名（面板形态由组件自己给，例如 ipop / epop / ntp） */
  className?: string;
  id?: string;
  role?: string;
  ariaLabel?: string;
  /** 与触发器等宽（实体下拉用） */
  matchWidth?: boolean;
  /** 可用空间不足时压缩到的上限（px） */
  maxHeight?: number;
  prefer?: "above" | "below" | "right";
  gap?: number;
  /** 画指向锚点的尖角（需要调用方保证 `.pop--arrow` 没有被裁切的子元素） */
  arrow?: boolean;
  /**
   * Tab 语义：
   * - `close`（默认）：Tab 即关闭并回焦触发器 —— 下拉/图标库的心智「选完就走」
   * - `trap`：Tab 在面板内循环 —— 面板里有多条可聚焦内容时（如公告列表），
   *   默认语义会让键盘用户按一下 Tab 面板就消失
   */
  tab?: "close" | "trap";
  /** 打开后把焦点交给面板自身（Esc 才收得到；焦点留在触发器上时 Esc/Tab 都会漏出去） */
  focusOnOpen?: boolean;
}

/**
 * 锚定弹层的**唯一**实现：portal + 定位 + 关闭语义。
 *
 * 刻意只管这几件事，业务内容由调用方给 —— 图标库与实体下拉需要完全不同的
 * 内部结构（网格 vs listbox），但「怎么浮起来、怎么关掉」必须只有一份。
 *
 * 三个关闭语义点，每一个都对应一类修过的缺陷：
 *
 *  1. **Esc 要 stopPropagation**：`useOverlayBehavior` 在 document 上冒泡监听
 *     keydown 且不判 target（`:48-52`），不拦的话「关图标库」会顺带把整张
 *     编辑弹窗关掉，用户填了一半的模型直接丢。
 *  2. **Tab / Shift+Tab 也要 stopPropagation**：`useOverlayBehavior` 的焦点环
 *     只在 `.dialog` 子树里算 focusables，portal 出去的节点永远 `!contains`，
 *     Shift+Tab 时它会把焦点抢回弹窗最后一个元素 —— 弹层变成键盘出不去。
 *  3. **外部点击用 pointerdown、且要排除触发器**：用 click 会与 `.mask` 的
 *     click 打架；不排除触发器则「点触发器关不掉」（down 关掉、click 又打开）。
 */
export function Popover(props: PopoverProps) {
  const {
    open,
    onClose,
    anchorRef,
    alignRef,
    returnFocusRef,
    repositionKey,
    children,
    className,
    id,
    role,
    ariaLabel,
    matchWidth,
    maxHeight,
    prefer,
    gap,
    arrow = false,
    tab = "close",
    focusOnOpen = false,
  } = props;
  const popRef = useRef<HTMLDivElement>(null);
  const closeRef = useRef(onClose);
  closeRef.current = onClose;
  // 外层弹窗关掉时必须自行卸载：portal 出去的内容不在 .dialog 的 DOM 链上，
  // 父弹窗的 display:none 管不到它（见 overlayContext 的注释）。
  const overlayOpen = useOverlayOpen();
  const visible = open && overlayOpen;
  const box = useAnchoredLayerBox({
    open: visible,
    anchorRef,
    alignRef,
    repositionKey,
    popRef,
    prefer,
    gap,
    maxHeight,
    matchWidth,
  });
  const style = (
    arrow
      ? {
          ...box.style,
          "--pop-arrow-x": `${box.arrowX}px`,
          "--pop-arrow-y": `${box.arrowY}px`,
        }
      : box.style
  ) as PopStyle;

  useEffect(() => {
    if (!visible) return;
    const onDown = (e: PointerEvent): void => {
      const target = e.target as Node | null;
      if (!target) return;
      if (popRef.current?.contains(target)) return;
      // 触发器自己负责 toggle（它的 onClick），这里必须放行，
      // 否则「按下就关」+「点击又开」= 永远关不掉。
      if (anchorRef.current?.contains(target)) return;
      closeRef.current();
    };
    document.addEventListener("pointerdown", onDown, true);
    return () => document.removeEventListener("pointerdown", onDown, true);
  }, [visible, anchorRef]);

  // 打开即聚焦面板本身。两个时序坑：hidden→visible 那一帧 focus 会落空，
  // 鼠标打开时浏览器的默认聚焦又晚于 effect —— 用 rAF 轮询补偿（≤6 帧）。
  useEffect(() => {
    if (!visible || !focusOnOpen) return;
    const el = popRef.current;
    if (!el) return;
    let raf = 0;
    let tries = 0;
    const settle = (): void => {
      if (!el.isConnected) return;
      if (el.contains(document.activeElement)) return;
      el.focus();
      if (el.contains(document.activeElement)) return;
      if (tries++ < 6) raf = window.requestAnimationFrame(settle);
    };
    settle();
    return () => window.cancelAnimationFrame(raf);
  }, [visible, focusOnOpen]);

  if (!visible) return null;

  /** 关掉并把焦点交回触发器（Esc / Tab 都要落在这里，否则焦点掉到 body）。 */
  const closeAndRestoreFocus = (): void => {
    (returnFocusRef?.current ?? anchorRef.current)?.focus();
    closeRef.current();
  };

  return createPortal(
    <div
      ref={popRef}
      id={id}
      role={role}
      aria-label={ariaLabel}
      tabIndex={focusOnOpen ? -1 : undefined}
      className={cn(
        "pop",
        arrow && "pop--arrow",
        arrow && box.placement === "above" && "pop-above",
        arrow && box.placement === "right" && "pop--arrow-left",
        className,
      )}
      style={style}
      onKeyDown={(e) => {
        if (e.key === "Escape") {
          e.stopPropagation();
          e.preventDefault();
          closeAndRestoreFocus();
          return;
        }
        if (e.key !== "Tab") return;
        e.stopPropagation();
        if (tab === "trap") {
          const list = Array.from(
            popRef.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? [],
          ).filter((el) => el.offsetParent !== null || el === document.activeElement);
          if (!list.length) {
            e.preventDefault();
            return;
          }
          const first = list[0];
          const last = list[list.length - 1];
          const active = document.activeElement as HTMLElement | null;
          const inside = !!active && !!popRef.current?.contains(active);
          if (e.shiftKey) {
            if (!inside || active === first) {
              e.preventDefault();
              last.focus();
            }
          } else if (!inside || active === last) {
            e.preventDefault();
            first.focus();
          }
          return;
        }
        // 先聚焦触发器再放行默认 Tab：浏览器按「当前 activeElement」算下一站，
        // 于是 Tab=往后一个字段、Shift+Tab=往前一个字段，符合「下拉」的心智。
        closeAndRestoreFocus();
      }}
    >
      {children}
    </div>,
    document.body,
  );
}
