import { useEffect, useRef, type RefObject } from "react";
import { lockScroll } from "@/lib/scrollLock";

const FOCUSABLE =
  'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';

/**
 * 当前打开的弹层栈（后开的在栈顶）。
 *
 * 为什么需要它：`keydown` 挂在 document 上，每个打开的弹层各挂一份。两层同时打开时
 * （账户弹窗里再开「修改密码」抽屉）一次 Esc 会**同时关掉两层**，两个 focus trap 还会
 * 把焦点互相拽回去。只有栈顶能处理 Esc/Tab，与视觉层级保持一致。
 */
const overlayStack: symbol[] = [];

/**
 * 弹层通用行为（`Dialog` / `Drawer` 共用，方案 §5.1）：
 * Esc 关闭、Tab 焦点环闭合（focus trap）、打开时初始聚焦、关闭后焦点归还、
 * 页面滚动锁（`lib/scrollLock.ts` 是唯一实现：计数锁 + 滚动条宽度补偿）。
 *
 * 注意：关闭态不移除节点——`.dialog/.drawer` 的关闭动画依赖节点保留（`visibility` 承担移出无障碍树）。
 */
export function useOverlayBehavior(
  open: boolean,
  onClose: () => void,
  ref: RefObject<HTMLElement | null>,
): void {
  const closeRef = useRef(onClose);
  closeRef.current = onClose;

  useEffect(() => {
    if (!open) return;
    const el = ref.current;
    const restore = document.activeElement as HTMLElement | null;
    const releaseScroll = lockScroll();
    const id = Symbol("overlay");
    overlayStack.push(id);

    const focusables = (): HTMLElement[] =>
      el
        ? Array.from(el.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
            // 折叠的菜单分区仍在 DOM 里但 display:none —— 不滤掉就会把隐藏节点当成「末项」，
            // 回绕条件 active === last 永不成立，Tab 从最后一个可见项直接逃出弹层。
            //
            // 用标准的 checkVisibility()，不用 getClientRects()/offsetParent：
            // 后两者依赖布局引擎，而 jsdom 不做布局（rect 恒空、offsetParent 恒 null），
            // 会把**所有**元素判成隐藏 ⇒ 弹层单测连初始聚焦都做不到。
            // jsdom 也没实现 checkVisibility，此时按可见处理（它同样不加载样式表，本来就判断不了）。
            (n) => typeof n.checkVisibility !== "function" || n.checkVisibility(),
          )
        : [];

    // 初始聚焦（M14）：**优先输入类控件**，其次首个可聚焦元素（无则保持当前焦点）。
    // 为什么不直接取 `focusables()[0]`：表单弹窗的第一个可聚焦元素往往是 label 里的
    // 问号按钮（`Field.tip` 渲染成 button），于是一打开弹窗就把提示气泡弹在第一个
    // 字段上、把值挡个正着 —— 用户只会当成"版式坏了"（编辑应用信息弹窗就是这么被报的）。
    // 跳过 readonly/disabled：只读回显（如应用标识）聚焦上去既不能改、还会顶掉真输入框。
    //
    // 另外两个时序坑会让这次 focus() 落空：
    //   ① 打开瞬间节点仍在 visibility 过渡的起点（hidden），浏览器忽略 focus()；
    //   ② 鼠标打开时，浏览器把焦点交给触发按钮的默认动作晚于 React effect。
    // 故先用 rAF 轮询补偿（≤6 帧，落焦即停），键盘打开首帧即成功、不会重复触发。
    const list = focusables();
    const initial =
      list.find((n) => {
        if (!/^(INPUT|TEXTAREA|SELECT)$/.test(n.tagName)) return false;
        const c = n as HTMLInputElement;
        return !c.readOnly && !c.disabled;
      }) ?? list[0];
    let rafId = 0;
    let tries = 0;
    const settleFocus = (): void => {
      if (!initial?.isConnected) return;
      if (el?.contains(document.activeElement)) return;
      initial.focus();
      if (el?.contains(document.activeElement)) return;
      if (tries++ < 6) rafId = requestAnimationFrame(settleFocus);
    };
    initial?.focus();
    rafId = requestAnimationFrame(settleFocus);

    const onKey = (e: KeyboardEvent): void => {
      // 只有最上层弹层响应：否则 Esc 一次关两层、Tab 焦点环互抢。
      if (overlayStack[overlayStack.length - 1] !== id) return;
      if (e.key === "Escape") {
        closeRef.current();
        return;
      }
      if (e.key !== "Tab") return;
      const list = focusables();
      if (!list.length) return;
      const first = list[0];
      const last = list[list.length - 1];
      const active = document.activeElement as HTMLElement | null;
      if (e.shiftKey && (active === first || !el?.contains(active))) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && active === last) {
        e.preventDefault();
        first.focus();
      }
    };

    document.addEventListener("keydown", onKey);
    return () => {
      // 先出栈再解绑：两个弹层同时关闭时，清理顺序不保证，用 lastIndexOf 兜住。
      const idx = overlayStack.lastIndexOf(id);
      if (idx >= 0) overlayStack.splice(idx, 1);
      cancelAnimationFrame(rafId);
      document.removeEventListener("keydown", onKey);
      releaseScroll();
      if (restore?.isConnected) restore.focus();
    };
  }, [open, ref]);
}
