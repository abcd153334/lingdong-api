import { useCallback, useSyncExternalStore } from "react";

/**
 * 移动端断点：**必须与 `styles/components.css` 里 `@media (max-width: 767px)`
 * 的取值逐字一致**。
 *
 * 为什么需要 JS 侧的一份：断点不只是「长什么样」，还决定「是不是弹层」——
 * 侧栏在 ≤767px 是覆盖页面的抽屉（要锁滚动、圈焦点、Esc 关闭），在 ≥768px 是
 * 常驻导航（这些行为一条都不能有）。只靠 CSS 表达不出来。
 *
 * 踩过的坑：原先遮罩写的是 Tailwind 的 `lg:hidden`（min-width:1024px），而侧栏的
 * 抽屉化在 max-width:767px —— 768~1023 这段里侧栏是常驻的，遮罩却被渲染出来盖住
 * 整页。两套断点混用必然错开，所以这里只留一个常量、CSS 侧靠注释强制对齐。
 */
export const MOBILE_QUERY = "(max-width: 767px)";

/**
 * 订阅一条媒体查询。
 *
 * 用 `useSyncExternalStore` 而不是 `useState + useEffect`：后者首帧恒为初始值
 * （通常是 false），移动端首次渲染会先按桌面渲染一帧再切回来——侧栏会闪一下
 * 常驻态。`getServerSnapshot` 返回 false（本项目无 SSR，仅为签名完整）。
 */
export function useMediaQuery(query: string): boolean {
  const subscribe = useCallback(
    (onChange: () => void) => {
      const mql = window.matchMedia(query);
      mql.addEventListener("change", onChange);
      return () => mql.removeEventListener("change", onChange);
    },
    [query],
  );
  const getSnapshot = useCallback(() => window.matchMedia(query).matches, [query]);
  return useSyncExternalStore(subscribe, getSnapshot, () => false);
}

/** 是否处于移动端断点（≤767px）。 */
export function useIsMobile(): boolean {
  return useMediaQuery(MOBILE_QUERY);
}
/**
 * 页内两栏的降级断点：**不是**移动端断点，而是"300px 的导航 + 模型表已经放不下"。
 *
 * 与 `MOBILE_QUERY` 的分工必须写清，否则下一个人会以为 767 就是全部：
 *   - `MOBILE_QUERY`(767) 决定**全局 chrome**（侧栏是抽屉还是常驻）；
 *   - `RAIL_DRAWER_QUERY`(899) 只决定**模型服务页的左栏**（两栏 → 抽屉）。
 * 两者在 768–899 之间同时生效：全局侧栏是常驻的，页内左栏却已经是抽屉。
 *
 * 必须与 `styles/model-service.css` 的 `@media (max-width: 899px)` 逐字一致 ——
 * JS 侧靠它决定 inert / role / 焦点陷阱，取值错开就是"抽屉打开了但键盘进不去"。
 */
export const RAIL_DRAWER_QUERY = "(max-width: 899px)";

/** 模型服务左栏是否处于抽屉形态（≤899px）。 */
export function useIsRailDrawer(): boolean {
  return useMediaQuery(RAIL_DRAWER_QUERY);
}

/**
 * 文档站的抽屉断点：**必须与 `web/src/docs/docs.css` 里
 * `@media (max-width: 1100px)` 逐字一致**。
 *
 * 1100 是为了「280 侧栏 + 820 正文 + 392 右栏」这三栏能同时成立；
 * 比它窄时侧栏变成覆盖式抽屉、右栏落到正文下方（不是收成更窄的一栏 ——
 * 抠到 200px 的侧栏里，接口路径会被截成两行残句）
 *
 * 与 `MOBILE_QUERY`(767) 的分工：文档站是独立子应用，不共用全站 chrome，
 * 767 以下它仍然是「抽屉 + 单栏」，没有第二套形态。
 */
export const DOCS_DRAWER_QUERY = "(max-width: 1100px)";

/**
 * 文档站右栏变窄的断点（≤1280px）：392 → 344。
 *
 * 只是视觉收窄，不改变结构，因此不提供 hook —— 用到它做行为判断的场景不存在。
 * 与 `docs.css` 的同名 @media 逐字一致。
 */
export const DOCS_RAIL_NARROW_QUERY = "(max-width: 1280px)";

/** 文档站侧栏是否处于抽屉形态（≤1100px）。 */
export function useIsDocsDrawer(): boolean {
  return useMediaQuery(DOCS_DRAWER_QUERY);
}

/**
 * 文档编辑页（/admin/docs）的抽屉断点：**必须与 `styles/admin-docs.css` 里
 * `@media (max-width: 1023px)` 逐字一致**。
 *
 * 为什么不复用 899（模型服务）：那一页右栏是表格与短表单，600px 还够；
 * 这一页右栏是密集表单 + 字段卡 + 代码块，899 时正文只剩约 600px，
 * 路径与示例会被压成竖排。1023 是「左栏已经放不下」的临界点。
 */
export const DOCS_ADMIN_DRAWER_QUERY = "(max-width: 1023px)";

/** 文档编辑页左栏是否处于抽屉形态（≤1023px）。 */
export function useIsDocsAdminDrawer(): boolean {
  return useMediaQuery(DOCS_ADMIN_DRAWER_QUERY);
}
