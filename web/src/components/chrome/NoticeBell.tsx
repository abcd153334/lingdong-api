import { useRef } from "react";

import { Icon } from "@/components/common/Icon";
import { Popover } from "@/components/common/Popover";
import { useAnnouncementsQuery } from "@/lib/api";
import { unreadOf } from "@/lib/notice";
import { useAuthStore } from "@/stores/auth";
import { NoticePanel } from "./NoticePanel";
import { useNoticeOpen } from "./noticeOpen";

/** 未读数：由条目派生（接口不再下发 unread），未登录不请求。 */
function useUnreadCount(): number {
  const loggedIn = useAuthStore((s) => s.user !== null);
  const { data } = useAnnouncementsQuery(loggedIn);
  return unreadOf(data?.enabled ? data.items : []);
}

function bellLabel(unread: number): string {
  return unread > 0 ? `站点公告，${unread} 条未读` : "站点公告";
}

/**
 * 公告入口：**按钮 + 锚定弹层**，门户顶栏与管理台顶栏共用同一个组件。
 *
 * 为什么两端同一个：公告只有一两条时，跳页会丢掉当前上下文；弹层 + 指向铃铛的尖角
 * 既说清「这是从哪冒出来的」，也不用为管理台再维护一套整页 + 面包屑 + 已读上报。
 * 徽标、未读口径、已读语义、列表 DOM、错误/空态因此天然只有一份。
 *
 * 弹层细节（缺一项就会坏）：
 *  - `arrow`：尖角位置由定位 hook 给出，翻到上方时自动切到下沿；
 *  - `tab="trap"`：默认 Tab 语义是「关闭」，面板里按一下 Tab 会直接消失；
 *  - `focusOnOpen`：焦点留在铃铛上时 Esc 收不到、Tab 会漏到顶栏下一个控件；
 *  - `aria-expanded/controls`：读屏用户才知道按钮连着什么。
 */
export function NoticeBell() {
  const unread = useUnreadCount();
  const { open, setOpen } = useNoticeOpen();
  const btnRef = useRef<HTMLButtonElement>(null);

  return (
    <>
      <button
        ref={btnRef}
        type="button"
        className="icon-btn"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={open ? "notice-panel" : undefined}
        aria-label={bellLabel(unread)}
        title="站点公告"
        onClick={() => setOpen(!open)}
      >
        <Icon name="bell" size={18} />
        {unread > 0 ? <span className="nt-badge">{unread > 99 ? "99+" : unread}</span> : null}
      </button>
      <Popover
        open={open}
        onClose={() => setOpen(false)}
        anchorRef={btnRef}
        id="notice-panel"
        role="dialog"
        ariaLabel="站点公告"
        className="ntp"
        arrow
        tab="trap"
        focusOnOpen
        maxHeight={480}
        gap={10}
      >
        <NoticePanel onClose={() => setOpen(false)} />
      </Popover>
    </>
  );
}
