import { createContext, useContext } from "react";

/**
 * 当前最近的浮层（Dialog / Drawer）是否**处于打开态**。
 *
 * 为什么需要它：Dialog 关闭时**不卸载子树**，只是把 `.mask` 换成 Tailwind 的
 * `hidden`（display:none），退场交给 CSS（见 useOverlayBehavior 的注释）。
 * 而弹层（图标库 / 实体下拉）是 portal 到 `document.body` 的 —— 它不在
 * `.dialog` 的 DOM 链上，父弹窗 display:none 管不到它。没有这个 context，
 * 点「取消」关掉编辑弹窗后，图标面板会继续浮在页面上。
 *
 * 默认 true：脱离任何弹窗使用（例如页面级搜索建议）时不应该被误卸载。
 */
export const OverlayOpenContext = createContext(true);

/** 读取最近的浮层打开态（无 Provider 时为 true）。 */
export function useOverlayOpen(): boolean {
  return useContext(OverlayOpenContext);
}
