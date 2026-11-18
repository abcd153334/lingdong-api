import { useCallback, useEffect, useRef, type PointerEvent as ReactPointerEvent } from "react";
import { MOBILE_QUERY, useMediaQuery } from "@/lib/responsive";

/**
 * 页面级滚动条（overlay，自绘）。
 *
 * ── 为什么必须自绘 ────────────────────────────────────────────
 * 只要页面滚动条占布局，**任何内容都画不到它下面那 10px** —— `position:fixed`
 * 也一样，它的包含块同样被扣掉滚动条宽。顶栏是横向铺满的 sticky 条，于是右侧
 * 永远缺一块，看起来像"给滚动条预留的留白"。负外边距硬补只会把内容顶出视口，
 * 换来一根横向滚动条（实测 scrollWidth 1280 > clientWidth 1270）。
 * 所以 app.css 把 `html::-webkit-scrollbar` 宽度设为 0（滚动、键盘、锚点、
 * `window.scrollY` 全不变），视觉由这个固定层的条接管：
 *   · 轨道 10px、拇指 6px、颜色取 `--sb-thumb`，与内部容器的原生条同源；
 *   · 拇指浮在内容之上（含顶栏），页面左侧内容不再被挤窄。
 *
 * ── 为什么用 ref 直接改样式 ──────────────────────────────────
 * 滚动是每帧一次的高频事件，走 setState 会让整棵树参与 reconcile。
 * 这里只在 `scroll` 里读三个布局值、写两个 style，rAF 合并成每帧最多一次。
 * `hidden` 也直接写 DOM：它同时受 CSS `[hidden]` 控制，不需要 React 参与。
 *
 * ── 为什么不监听 router ──────────────────────────────────────
 * 路由切换会整块替换 `#root` 子树，`MutationObserver(childList)` 必然触发；
 * 再叠加 `resize` 与资源 `load`（图片撑高）即可覆盖高度变化的全部来源。
 * 订阅 router 反而要把组件塞进 RouterProvider 内部，得不偿失。
 */

/** 拇指最小高度：再小就点不准，也会在超长页面上变成一根头发丝。 */
const MIN_THUMB = 28;

export function PageScrollbar() {
  const mobile = useMediaQuery(MOBILE_QUERY);
  const trackRef = useRef<HTMLDivElement>(null);
  const thumbRef = useRef<HTMLDivElement>(null);

  const sync = useCallback(() => {
    const track = trackRef.current;
    const thumb = thumbRef.current;
    if (!track || !thumb) return;

    const de = document.documentElement;
    // 轨道是 `fixed inset-y-0`，高度等于视口高；用 clientHeight 而不是 innerHeight：
    // 后者含（已被我们隐藏的）滚动条区域，桌面浏览器缩放下会差 1px。
    const view = de.clientHeight;
    const total = de.scrollHeight;
    const range = total - view;

    if (range <= 1) {
      track.hidden = true;
      return;
    }
    track.hidden = false;

    const raw = Math.round((view * view) / total);
    const height = Math.min(Math.max(raw, MIN_THUMB), Math.max(view - 8, 8));
    const maxTop = Math.max(view - height, 0);
    const top = maxTop === 0 ? 0 : Math.round((de.scrollTop / range) * maxTop);

    thumb.style.height = `${height}px`;
    thumb.style.transform = `translateY(${top}px)`;
  }, []);

  useEffect(() => {
    if (mobile) return;
    let raf = 0;
    const schedule = () => {
      if (raf) return;
      raf = requestAnimationFrame(() => {
        raf = 0;
        sync();
      });
    };

    window.addEventListener("scroll", schedule, { passive: true });
    window.addEventListener("resize", schedule);
    // 图片/字体加载完成后内容会二次撑高，这类变化不改 DOM 结构，只能靠 load 捕获。
    window.addEventListener("load", schedule, true);

    const root = document.getElementById("root") ?? document.body;
    const mo = new MutationObserver(schedule);
    mo.observe(root, { childList: true, subtree: true });

    sync();
    return () => {
      if (raf) cancelAnimationFrame(raf);
      window.removeEventListener("scroll", schedule);
      window.removeEventListener("resize", schedule);
      window.removeEventListener("load", schedule, true);
      mo.disconnect();
    };
  }, [mobile, sync]);

  /** 轨道/拇指的指针交互：拖拇指跟手，点轨道翻到该处（与原生一致）。 */
  const onPointerDown = useCallback(
    (e: ReactPointerEvent<HTMLDivElement>) => {
      const track = trackRef.current;
      const thumb = thumbRef.current;
      if (!track || !thumb) return;

      const de = document.documentElement;
      const view = de.clientHeight;
      const range = de.scrollHeight - view;
      if (range <= 1) return;

      const height = thumb.getBoundingClientRect().height;
      const maxTop = Math.max(view - height, 1);

      if (e.target !== thumb) {
        // 点轨道：把拇指中心挪到指针处，再反算滚动位置。
        const y = Math.min(Math.max(e.clientY - height / 2, 0), maxTop);
        de.scrollTop = (y / maxTop) * range;
        sync();
        return;
      }

      e.preventDefault();
      const startY = e.clientY;
      const startScroll = de.scrollTop;
      const ratio = range / maxTop;
      // 指针捕获挂在轨道上：拖到视口外也能继续跟手，且不依赖 window 级监听。
      track.setPointerCapture(e.pointerId);

      const move = (ev: PointerEvent) => {
        de.scrollTop = startScroll + (ev.clientY - startY) * ratio;
      };
      const end = () => {
        track.removeEventListener("pointermove", move);
        track.removeEventListener("pointerup", end);
        track.removeEventListener("pointercancel", end);
        if (track.hasPointerCapture(e.pointerId)) track.releasePointerCapture(e.pointerId);
      };
      track.addEventListener("pointermove", move);
      track.addEventListener("pointerup", end);
      track.addEventListener("pointercancel", end);
    },
    [sync],
  );

  if (mobile) return null;

  return (
    <div
      ref={trackRef}
      className="pg-sb"
      hidden
      aria-hidden="true"
      onPointerDown={onPointerDown}
    >
      <div ref={thumbRef} className="pg-sb-thumb" />
    </div>
  );
}
