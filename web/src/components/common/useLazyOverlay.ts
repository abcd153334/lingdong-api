import { useCallback, useState } from "react";

/**
 * 懒挂载弹层的宿主状态（**唯一实现**：账户弹窗与「新建适配器」弹窗共用）。
 *
 * 两件事都必须照做，各自都有踩坑记录：
 *  1. **懒挂载**：`Dialog` 关闭后节点仍在（要播退场动画）。常驻会给每个页面留一个
 *     `role="dialog"` 死节点 —— jsdom 用例按 role 查询会撞 strict，`.mask` 也会常驻
 *     吃掉本该落到页面的点击。
 *  2. **关闭只切 open**：不能"关闭即卸载"。弹层里的草稿、正在进行的流式请求、
 *     滚动位置都会随卸载消失，用户看到的是"我刚才写的没了"。
 *
 * 注意本 hook **不处理"宿主路由让位"**：那是 `useAccountDialog` 的职责
 * （`/admin/account` 页自己渲染一份弹窗，布局那份必须不渲染）。把两者混在一个
 * hook 里会让"页面自己就是宿主"的场景恒为 false（mounted && pathname !== hostPath）。
 */
export function useLazyOverlay() {
  const [open, setOpen] = useState(false);
  const [mounted, setMounted] = useState(false);
  // 引用必须稳定：调用方普遍把它放进 effect 依赖，每次 render 换身份会变成
  // "effect → setState → render → effect" 的空转。
  const openOverlay = useCallback(() => {
    setMounted(true);
    setOpen(true);
  }, []);
  const closeOverlay = useCallback(() => setOpen(false), []);
  return { mounted, open, openOverlay, closeOverlay };
}
