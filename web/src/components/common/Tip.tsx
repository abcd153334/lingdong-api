import { useEffect, useId, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { cn } from "@/lib/utils";
import { Icon } from "./Icon";
import { useAnchoredLayer } from "./useAnchoredLayer";

export interface TipProps {
  /** 气泡文案（多行自动换行） */
  text: string;
  className?: string;
  /** 由 Field 注入：供控件 aria-describedby 指向的唯一 id */
  id?: string;
}

/** 气泡与锚点、气泡与视口边缘的最小间距 */
const GAP = 8;
const EDGE = 8;

/**
 * 规则解释问号提示（对齐原型 .tip）。
 *
 * 气泡走 **portal + fixed 定位 + 视口夹取**，不用 CSS 伪元素。原因：
 * 伪元素 `absolute` 定位在 `.dl-body`（`overflow-y:auto`）这类滚动容器里会被裁剪，
 * 且滚动条一出现就横向溢出（260px 气泡放在两列栅格的右列必然越界）。
 * 气泡往左还是往右，取决于锚点离容器右缘还有多少像素 —— CSS 读不到这个距离，
 * 只有量出 `getBoundingClientRect()` 才能算。伪元素还受祖先 `overflow:hidden` 裁剪，
 * `fixed` 不受（除非祖先有 transform，那时它反而是正确参照）。
 *
 * 四个刻意为之的细节（都是修过的缺陷，改动前请先看清楚）：
 *
 *  1. `aria-label` 是**固定的「说明」**，不是 `text`。
 *     用 `text` 会让整段规则成为这个 span 的可访问名，进而污染外层
 *     `<label htmlFor>` 的可访问名 —— 读屏会把「按钮」读成一长句业务规则，
 *     `getByLabel` 也可能因此多命中。真正的规则文本放进被 aria-describedby
 *     引用的隐藏节点里，聚焦问号时仍能听到；气泡本身 `aria-hidden`，否则读两遍。
 *
 *  2. `onMouseDown`/`onClick` 都要 `preventDefault()`。
 *     `Tip` 渲染在 `<label htmlFor>` 内部，浏览器会把 label 内任意点击转发给
 *     关联控件 —— 只 `stopPropagation()` 挡不住这个默认行为，表现为「点问号
 *     顺带把开关切了」。
 *
 *  3. 开合必须区分**指针类型**与**打开来源**。
 *     触屏上 `pointerenter`（pointerType=touch）紧跟 `click`，两者都 toggle
 *     就是「点一下开、同一次点击又关掉」。鼠标悬停打开后若被 click 关掉，
 *     鼠标还停在问号上却看不到气泡，也是错的。所以：悬停/聚焦属于「瞬时」，
 *     移出或失焦即关；点击/键盘属于「钉住」，只有再次点击、Esc 或点别处才关。
 *
 *  4. 键盘 Enter/Space 必须能开合，Esc 要能关（否则键盘用户打开了收不起来）。
 */
export function Tip({ text, className, id }: TipProps) {
  const [open, setOpen] = useState(false);
  const anchorRef = useRef<HTMLSpanElement>(null);
  const popRef = useRef<HTMLSpanElement>(null);
  /** true = 瞬时（hover/focus，移出即关）；false = 钉住（click/键盘，需再点或 Esc） */
  const transient = useRef(true);
  const autoId = useId();
  // 传了 id 就直接用（Field 的 aria-describedby 指向它），不再加后缀：
  // 两边各拼一个后缀时必然有一处对不上，表现为「关联了但描述节点找不到」。
  const descId = id ?? `${autoId}-desc`;

  // 定位交给共用的锚定浮层 hook（与下拉弹层同一份实现）：优先上方、不压缩高度。
  const popStyle = useAnchoredLayer({
    open,
    anchorRef,
    popRef,
    prefer: "above",
    gap: GAP,
    edge: EDGE,
    offsetX: -6,
  });

  useEffect(() => {
    if (!open) return;
    const onDocClick = () => setOpen(false);
    document.addEventListener("click", onDocClick);
    return () => document.removeEventListener("click", onDocClick);
  }, [open]);

  const pin = (): void => {
    transient.current = false;
    setOpen(true);
  };

  return (
    <>
      <span
        ref={anchorRef}
        className={cn("tip", open && "open", className)}
        tabIndex={0}
        role="button"
        aria-label="说明"
        aria-describedby={descId}
        aria-expanded={open}
        data-tip={text}
        onMouseDown={(e) => e.preventDefault()}
        onPointerEnter={(e) => {
          // 触屏的 enter 紧跟 click；这里只处理鼠标，触屏交给 click
          if (e.pointerType !== "mouse" || open) return;
          transient.current = true;
          setOpen(true);
        }}
        onPointerLeave={(e) => {
          if (e.pointerType !== "mouse" || !transient.current) return;
          setOpen(false);
        }}
        onFocus={() => {
          if (open) return;
          transient.current = true;
          setOpen(true);
        }}
        onBlur={() => {
          if (!transient.current) return;
          setOpen(false);
        }}
        onClick={(e) => {
          // 阻止 <label> 的「点击转发给关联控件」默认行为（否则开关会被误切换）。
          e.preventDefault();
          e.stopPropagation();
          if (open && !transient.current) {
            setOpen(false);
            return;
          }
          pin();
        }}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            e.stopPropagation();
            if (open && !transient.current) setOpen(false);
            else pin();
            return;
          }
          if (e.key === "Escape") setOpen(false);
        }}
      >
        <Icon name="help" size={12} />
        <span id={descId} hidden>
          {text}
        </span>
      </span>
      {open
        ? createPortal(
            <span ref={popRef} className="tip-pop" aria-hidden="true" style={popStyle}>
              {text}
            </span>,
            document.body,
          )
        : null}
    </>
  );
}
