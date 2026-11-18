import { useEffect } from "react";
import { useLocation } from "@tanstack/react-router";

import { useLazyOverlay } from "@/components/common/useLazyOverlay";

/**
 * 账户弹窗的宿主状态（管理台 / 门户布局共用的唯一实现）。
 *
 * 在 `useLazyOverlay`（懒挂载 + 关闭只切 open）之上只加一件这里独有的事：
 * **宿主路由让位** —— `/admin/account` 与 `/settings` 自己就渲染这个弹窗，
 * 布局这份再渲染一次就会出现两个 `#pw-old`（`Drawer` 关闭态也渲染字段）。
 *
 * 另外两件事跟着 `useLazyOverlay` 一起具备了：懒挂载、关闭只切 open。
 * **站内跳转收弹窗**仍在这里：门户布局是 `/mfa/setup` 的父级，不收的话
 * 「去开启两步验证」会被自己的弹窗盖住。
 *
 * 也正因"宿主路由让位"这一条，本 hook 不能被"页面自己开弹窗"的场景复用：
 * 那里的 pathname 恒等于 hostPath，`mounted` 会是 false、弹窗永远不渲染 ——
 * 那种场景直接用 `useLazyOverlay`。
 */
export function useAccountDialog(hostPath: string) {
  const { pathname } = useLocation();
  const { mounted, open, openOverlay, closeOverlay } = useLazyOverlay();

  // 首次也会跑一次：状态未变时 React 直接 bail out，不产生额外渲染
  useEffect(() => {
    closeOverlay();
  }, [pathname, closeOverlay]);

  return {
    /** 是否渲染弹窗节点：进过至少一次 + 不在宿主路由上 */
    mounted: mounted && pathname !== hostPath,
    open,
    openDialog: openOverlay,
    closeDialog: closeOverlay,
  };
}
