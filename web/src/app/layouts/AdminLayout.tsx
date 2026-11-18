import { useState } from "react";
import { Outlet, useNavigate } from "@tanstack/react-router";
import { AccountDialog } from "@/components/account/AccountDialog";
import { useAccountDialog } from "@/components/account/useAccountDialog";
import { AdminSidebar } from "@/components/chrome/AdminSidebar";
import { AdminTopBar } from "@/components/chrome/AdminTopBar";
import { NoticeOpenProvider } from "@/components/chrome/noticeOpen";
import { useLogoutMutation } from "@/lib/api";
import { useAuthStore } from "@/stores/auth";

/** 管理台布局（侧栏 + 顶栏 + 内容；≤767px 侧栏为汉堡抽屉） */
export function AdminLayout() {
  const [navOpen, setNavOpen] = useState(false);
  // 弹窗宿主状态（懒挂载 / 宿主路由让位 / 站内跳转收窗）与门户同一实现
  const acct = useAccountDialog("/admin/account");
  const navigate = useNavigate();
  const user = useAuthStore((s) => s.user);
  const logout = useLogoutMutation();

  const handleLogout = () => {
    logout.mutate(undefined, { onSettled: () => void navigate({ to: "/login" }) });
  };

  return (
    /* 公告弹层的开合状态挂布局层：切页不重置，与门户同构（同一组件、同一 Provider） */
    <NoticeOpenProvider>
      <div className="a-layout">
        <AdminSidebar open={navOpen} onClose={() => setNavOpen(false)} />
        <div className="a-main">
          <AdminTopBar
            onToggleSidebar={() => setNavOpen((v) => !v)}
            username={user?.username}
            email={user?.email}
            avatarUrl={user?.avatar_url}
            balance={user?.balance}
            role={user?.role}
            onOpenAccount={acct.openDialog}
            onLogout={handleLogout}
          />
          <div className="a-content">
            <Outlet />
          </div>
        </div>
        {/* 账户弹窗挂在 .a-layout 层：.a-top 是 sticky + z-index（层叠上下文），
            挂进顶栏会让它的遮罩被困在 z=30 里，侧栏(z=40)反而盖住遮罩。 */}
        {acct.mounted ? (
          <AccountDialog open={acct.open} onClose={acct.closeDialog} />
        ) : null}
      </div>
    </NoticeOpenProvider>
  );
}
