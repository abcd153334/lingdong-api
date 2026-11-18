/**
 * 页面滚动锁（唯一实现处）。
 *
 * 锁什么：`body{overflow:hidden}` 让视口不可滚（`overflow` 从 body 传播到视口）。
 * 补什么：滚动条消失后内容区会变宽，要把这段宽度补回 `body` 的右内边距。
 *
 * ── 补偿量为什么不能按「滚动条宽度」算 ──────────────────────────────
 * 页面**不预留**滚动条位置（app.css 明确不加 `scrollbar-gutter: stable`）。
 * 所以 `body{overflow:hidden}` 让滚动条消失的那一刻，内容区真的变宽了一个滚动条
 * 宽（实测 1270 → 1280），补偿量就是这个差值。
 *
 * 不能按 `innerWidth - documentElement.clientWidth` 算：这个差值同时被「有没有
 * 滚动条」和「有没有 gutter」影响，锁定后它从 10 变 0 —— 拿它当补偿量，等于把
 * 「滚动条已经消失」读成「不用再占位」。上一版正是在全局 gutter 下多补了 10px
 * （`.a-main` 右边界 1270 → 1260），开合弹层时整页横跳一下，正是要根治的现象。
 *
 * 唯一可靠的判据是布局本身：锁前、锁后各量一次 `body` 的 border-box 宽度，差值
 * 就是「内容区实际变宽了多少」，也就是该补多少。
 *   · 页面有滚动条 → 差 = 滚动条宽 → 补；
 *   · 页面本来没有滚动条（内容不足一屏）→ 差 = 0 → 不补，也不会凭空内缩。
 * 滚动条宽度本身（Chrome 10px、Firefox thin、HiDPI 缩放下可能非整数）自动被
 * 吸收，所以这里**不允许**硬编码「滚动条宽度」。
 *
 * ── 为什么用引用计数 ────────────────────────────────────────────
 * 抽屉里再开图片预览、或代码里同时「关旧开新」时，释放顺序不一定是栈序。
 * 快照法（各自存一份「上一份内联样式」）会在关掉上层时把「底下还有一层开着」
 * 的页面解锁（背景能滚、宽度也回去）。计数法只在最后一层释放时还原，与关闭
 * 顺序无关；释放函数幂等，防止 React effect 双跑把计数减穿。
 */

let depth = 0;
/** 首次加锁前的原始内联样式，最后一次解锁时原样写回（通常是空串 → 回落样式表） */
let saved: { overflow: string; paddingRight: string } | null = null;

export type ReleaseScrollLock = () => void;

/** 亚像素容差：布局宽度是浮点数，0.5px 以内视为「没变」。 */
const EPSILON = 0.5;

/** 锁住页面滚动，返回解锁函数（幂等，重复调用只生效一次）。 */
export function lockScroll(): ReleaseScrollLock {
  if (typeof document === "undefined") return () => {};
  const body = document.body;

  if (depth === 0) {
    // 必须在改成 hidden 之前量：改完就进入「已锁定」的布局状态了。
    const before = body.getBoundingClientRect().width;
    saved = {
      overflow: body.style.overflow,
      paddingRight: body.style.paddingRight,
    };
    body.style.overflow = "hidden";
    // 同步读一次，浏览器会立刻完成布局重算，拿到的是锁定后的真实宽度。
    const delta = body.getBoundingClientRect().width - before;
    if (delta > EPSILON) {
      // 在 computed 值上累加：body 可能已有样式表或内联的 padding，直接覆盖会丢。
      const cur = parseFloat(getComputedStyle(body).paddingRight) || 0;
      body.style.paddingRight = `${cur + delta}px`;
    }
  }
  depth += 1;

  let released = false;
  return () => {
    if (released) return;
    released = true;
    depth = Math.max(0, depth - 1);
    if (depth > 0 || !saved) return;
    body.style.overflow = saved.overflow;
    body.style.paddingRight = saved.paddingRight;
    saved = null;
  };
}
