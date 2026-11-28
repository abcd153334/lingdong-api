import { useCallback, useLayoutEffect, useState, type CSSProperties, type RefObject } from "react";

/**
 * 锚定浮层的定位计算（Tip 气泡、下拉弹层、公告弹层共用）。
 *
 * 为什么必须 portal + fixed：`.dialog` 带 `transform: translate(-50%,-50%) scale(.95)`，
 * 而 CSS Transforms 规定 transform 祖先会成为 `position:fixed` 后代的**包含块** ——
 * 原地的 fixed 会以弹窗 padding box 为原点，还会被打开动画的 scale(.95) 二次缩放。
 * 放进 `.dl-body` 走 absolute 同样不行：那里是 `overflow-y:auto`，会被裁剪。
 *
 * 四处「不写就会错」的细节（都是实测过的）：
 *
 *  1. **两趟测量**：`maxHeight` 依赖翻转判定，翻转判定依赖高度，一趟算不完。
 *     先以 `visibility:hidden` 挂载量 `offsetWidth/offsetHeight`，再落坐标。
 *  2. **scroll 必须 capture**：弹窗打开时 body 的滚动被锁，真正的滚动发生在
 *     `.dl-body` 内部，不 capture 收不到事件，浮层会掉队。但**面板自己内部**的
 *     滚动必须排除，否则列表每滚一帧就重定位一次。
 *  3. **监听 visualViewport**：移动端软键盘弹出只改变 visualViewport 的高度，
 *     `resize` 不一定触发；不跟的话弹层底部会被键盘盖住。
 *  4. **尺寸变化要重算**：面板翻到锚点上方时 `top` 是由高度反推的
 *     （`r.top - gap - h`）。内容变高（异步数据到达、手风琴展开）却不重算，
 *     面板就会向下长、盖住锚点。Tip 内容固定所以旧实现没暴露。
 */
export interface AnchoredLayerOpts {
  open: boolean;
  anchorRef: RefObject<HTMLElement | null>;
  popRef: RefObject<HTMLElement | null>;
  /**
   * 优先出现在锚点哪一侧（默认 below）。
   *
   * `right` 是给"二级导航"用的：面板左缘贴住锚点右缘、纵向对齐 `alignRef`。
   * 它**不参与上下翻转**（见 place 里的注释）——把贴边的面板翻到左边会直接压住
   * 导航栏本体，比溢出更糟。
   */
  prefer?: "above" | "below" | "right";
  /**
   * `prefer="right"` 时的纵向基准：缺省用 `anchorRef`。
   *
   * 为什么需要两个 ref：水平锚必须是**整栏**（卡片右缘比栏右缘内缩 10px，
   * 拿卡片当水平锚会让面板压进栏里），而纵向必须对齐**被点的那张卡**。
   */
  alignRef?: RefObject<HTMLElement | null>;
  /**
   * 重算触发器：值变了就重新定位，即使 `open` 没变。
   *
   * 没有它，「从 A 卡切到 B 卡」时 `open` 恒为 true，effect 不会重跑，
   * 面板会停在 A 的坐标上 —— 内容已经换成 B，位置还是 A，是必现的错位。
   */
  repositionKey?: unknown;
  /** 与锚点的间距（Tip 用 8，下拉用 6） */
  gap?: number;
  /** 与视口边缘的最小间距 */
  edge?: number;
  /** 水平额外偏移（Tip 的 -6 就是它） */
  offsetX?: number;
  /** 给定时按可用空间压缩高度；不给则只平移（气泡的行为） */
  maxHeight?: number;
  /** 与锚点等宽（实体下拉用） */
  matchWidth?: boolean;
}

/** 定位结果：样式 + 箭头需要的两项几何事实。 */
export interface AnchoredLayer {
  style: CSSProperties;
  /** 面板最终落在锚点的哪一侧（决定尖角画在哪条边） */
  placement: "above" | "below" | "right";
  /** 尖角中心相对面板左缘的偏移（已夹取，单位 px） */
  arrowX: number;
  /** 尖角中心相对面板上缘的偏移（`right` 用；其余情况为 0） */
  arrowY: number;
}

const HIDDEN: CSSProperties = { left: 0, top: 0, visibility: "hidden" };
const HIDDEN_BOX: AnchoredLayer = { style: HIDDEN, placement: "below", arrowX: 24, arrowY: 0 };

/** 压缩后的最小可用高度：再小就没有可读内容了，不如让它溢出。 */
const MIN_HEIGHT = 96;

/** 尖角中心距面板左右缘的最小距离：必须大于面板圆角，否则尖角会落进圆角里被吃掉。 */
const ARROW_INSET = 20;

/** 定位值全等时返回原对象，斩断 ResizeObserver → setState → 重渲染 的往返。 */
function sameBox(a: AnchoredLayer, b: AnchoredLayer): boolean {
  return (
    a.style.left === b.style.left &&
    a.style.top === b.style.top &&
    a.style.width === b.style.width &&
    a.style.maxHeight === b.style.maxHeight &&
    a.style.visibility === b.style.visibility &&
    a.placement === b.placement &&
    a.arrowX === b.arrowX &&
    a.arrowY === b.arrowY
  );
}

export function useAnchoredLayerBox(opts: AnchoredLayerOpts): AnchoredLayer {
  const {
    open,
    anchorRef,
    popRef,
    prefer = "below",
    alignRef,
    repositionKey,
    gap = 6,
    edge = 8,
    offsetX = 0,
    maxHeight,
    matchWidth = false,
  } = opts;
  const [box, setBox] = useState<AnchoredLayer>(HIDDEN_BOX);

  const place = useCallback(() => {
    const anchor = anchorRef.current;
    const pop = popRef.current;
    if (!anchor || !pop) return;
    const r = anchor.getBoundingClientRect();
    /**
     * 宽度用 **html 的布局宽度**：它是 `position:fixed` 包含块的同源量（实测锁定
     * 前后与 fixed 元素逐像素一致）。`documentElement.clientWidth` 的语义会随
     * 「有没有滚动条 / 有没有 scrollbar-gutter」变化 —— 一旦有人给 html 加回
     * `scrollbar-gutter: stable`，锁定滚动后它就会比布局多报一个滚动条宽，浮层
     * 按它右对齐会直接顶出内容区。用 rect 不依赖这条语义，也不随滚动条策略漂移。
     *
     * 高度**不能**同样换成 rect：`html` 的 rect.height 是内容总高（一屏以上时
     * 是几千像素），不是视口高。`clientHeight` 不受锁定影响，保持原样。
     */
    const vw = document.documentElement.getBoundingClientRect().width || window.innerWidth;
    const vh = document.documentElement.clientHeight || window.innerHeight;
    const w = matchWidth ? r.width : pop.offsetWidth;
    const h = pop.offsetHeight;

    /**
     * `right`：贴在锚点右缘（gap=0 时严丝合缝）、纵向对齐 `alignRef` 的顶边。
     *
     * 不做左右翻转：这一支服务的是"导航栏 → 二级面板"，翻到左边会盖住导航本身。
     * 右溢时只夹进视口（面板变窄），宁可遮一点内容，也不能跑到栏里。
     */
    if (prefer === "right") {
      const ar = (alignRef?.current ?? anchor).getBoundingClientRect();
      const left = Math.min(r.right + gap, Math.max(edge, vw - edge - w));
      // 顶边对齐被点的那张卡；放不下时整体上推（不低于 edge）
      const top = Math.min(Math.max(ar.top, edge), Math.max(edge, vh - edge - h));
      const wantY = ar.top + ar.height / 2 - top;
      const arrowY = Math.min(Math.max(wantY, ARROW_INSET), Math.max(ARROW_INSET, h - ARROW_INSET));
      const next: CSSProperties = { left, top, visibility: "visible" };
      if (matchWidth) next.width = w;
      if (maxHeight !== undefined) {
        next.maxHeight = Math.max(MIN_HEIGHT, Math.min(maxHeight, vh - edge * 2));
      }
      const box: AnchoredLayer = { style: next, placement: "right", arrowX: 0, arrowY };
      setBox((prev) => (sameBox(prev, box) ? prev : box));
      return;
    }

    // 水平：先对齐锚点左缘，右溢改成右对齐，最后夹进视口
    let left = r.left + offsetX;
    if (left + w > vw - edge) left = vw - edge - w;
    if (left < edge) left = edge;

    // 垂直：优先侧放得下就用；否则换另一侧；两侧都放不下就取较大一侧并压缩
    const spaceAbove = r.top - gap - edge;
    const spaceBelow = vh - edge - (r.bottom + gap);
    const fitsAbove = spaceAbove >= h;
    const fitsBelow = spaceBelow >= h;

    let top: number;
    let limit: number;
    if (prefer === "below") {
      if (fitsBelow || spaceBelow >= spaceAbove) {
        top = r.bottom + gap;
        limit = spaceBelow;
      } else {
        top = r.top - gap - Math.min(h, spaceAbove);
        limit = spaceAbove;
      }
    } else if (fitsAbove || spaceAbove >= spaceBelow) {
      top = r.top - gap - Math.min(h, spaceAbove);
      limit = spaceAbove;
    } else {
      top = r.bottom + gap;
      limit = spaceBelow;
    }

    // 尖角必须跟着**夹取之后**的 left 算，否则面板右移时尖角会留在旧坐标上。
    const want = r.left + r.width / 2 - left;
    const arrowX = Math.min(Math.max(want, ARROW_INSET), Math.max(ARROW_INSET, w - ARROW_INSET));

    const next: CSSProperties = { left, top, visibility: "visible" };
    if (matchWidth) next.width = w;
    // 只有调用方显式要求压缩时才写 max-height：
    // 气泡（Tip）靠"整体上移"避免溢出，给它加 max-height 会把文案截断。
    if (maxHeight !== undefined) {
      next.maxHeight = Math.max(MIN_HEIGHT, Math.min(maxHeight, limit));
    }
    const result: AnchoredLayer = {
      style: next,
      // 判据只看最终落点：压缩到上方时 top 必然小于锚点底边
      placement: top >= r.bottom ? "below" : "above",
      arrowX,
      arrowY: 0,
    };
    setBox((prev) => (sameBox(prev, result) ? prev : result));
  }, [anchorRef, popRef, prefer, alignRef, gap, edge, offsetX, maxHeight, matchWidth]);

  useLayoutEffect(() => {
    if (!open) {
      setBox(HIDDEN_BOX);
      return;
    }
    place();
    // 弹窗打开动画 0.25s 内锚点 rect 在变（scale .95→1），一帧后再校正一次。
    const raf = window.requestAnimationFrame(place);
    const onMove = (e?: Event): void => {
      // 面板内部滚动不该触发重定位：它是 fixed 的，自己滚动不影响自身坐标。
      if (e?.target instanceof Node && popRef.current?.contains(e.target)) return;
      place();
    };
    window.addEventListener("scroll", onMove, true);
    window.addEventListener("resize", onMove);
    const vv = window.visualViewport;
    vv?.addEventListener("resize", onMove);
    vv?.addEventListener("scroll", onMove);
    const pop = popRef.current;
    const ro =
      pop && typeof ResizeObserver !== "undefined" ? new ResizeObserver(() => place()) : null;
    if (ro && pop) ro.observe(pop);
    return () => {
      window.cancelAnimationFrame(raf);
      window.removeEventListener("scroll", onMove, true);
      window.removeEventListener("resize", onMove);
      vv?.removeEventListener("resize", onMove);
      vv?.removeEventListener("scroll", onMove);
      ro?.disconnect();
    };
  }, [open, place, popRef, repositionKey]);

  return box;
}

/**
 * 兼容出口：只要 styles 的调用方（Tip）用它，行为与改造前一致。
 * 新代码需要尖角或翻转结论时用 `useAnchoredLayerBox`。
 */
export function useAnchoredLayer(opts: AnchoredLayerOpts): CSSProperties {
  return useAnchoredLayerBox(opts).style;
}
