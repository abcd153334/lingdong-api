import { createContext, useContext, useMemo, useState, type ReactNode } from "react";

/**
 * 公告弹层的开合状态。
 *
 * 为什么需要它：铃铛在 `PortalTopBar`，首页横幅在 `Home`，两者分处两棵子树，
 * 但必须打开**同一个**弹层。状态的生命周期就是门户布局（切页不重置），
 * 所以挂 Context 而不是引入全局 store。
 */
export interface NoticeOpenValue {
  open: boolean;
  setOpen: (next: boolean) => void;
}

const NoticeOpenContext = createContext<NoticeOpenValue | null>(null);

export function NoticeOpenProvider({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState(false);
  const value = useMemo(() => ({ open, setOpen }), [open]);
  return <NoticeOpenContext.Provider value={value}>{children}</NoticeOpenContext.Provider>;
}

/**
 * 读取开合状态；没有 Provider 时回落到组件内 state。
 *
 * 回落不是为了「双实现」：运行时门户布局一定提供 Provider（唯一 owner）；
 * 它只是让单测/展示台单独渲染铃铛时不至于点了没反应。
 */
export function useNoticeOpen(): NoticeOpenValue {
  const ctx = useContext(NoticeOpenContext);
  const [localOpen, setLocalOpen] = useState(false);
  return ctx ?? { open: localOpen, setOpen: setLocalOpen };
}
