import { Link, Outlet, useNavigate } from "@tanstack/react-router";
import { AccountDialog } from "@/components/account/AccountDialog";
import { useAccountDialog } from "@/components/account/useAccountDialog";
import { DataNotice } from "@/components/chrome/DataNotice";
import { NoticeOpenProvider } from "@/components/chrome/noticeOpen";
import { PortalTabBar } from "@/components/chrome/PortalTabBar";
import { PortalTopBar } from "@/components/chrome/PortalTopBar";
import { useLogoutMutation } from "@/lib/api";
import { portalFooterNav } from "@/lib/nav";
import { useSite } from "@/lib/site";
import { useAuthStore } from "@/stores/auth";

/** 门户布局（顶部导航 + 内容 + 页脚 + 移动端 Tab Bar） */
export function PortalLayout() {
  const navigate = useNavigate();
  const user = useAuthStore((s) => s.user);
  const site = useSite();
  const logout = useLogoutMutation();
  // 与管理台同一份宿主状态机：`/settings` 自己渲染弹窗，这里只在别的页面就地开
  const acct = useAccountDialog("/settings");

  const handleLogout = () => {
    logout.mutate(undefined, { onSettled: () => void navigate({ to: "/login" }) });
  };

  return (
    <NoticeOpenProvider>
      <PortalTopBar
        username={user?.username}
        email={user?.email}
        avatarUrl={user?.avatar_url}
        balance={user?.balance}
        isAdmin={user?.role === 0}
        onOpenAccount={acct.openDialog}
        onLogout={handleLogout}
      />
      <main className="p-main">
        <DataNotice />
        <Outlet />
      </main>
      <footer className="p-foot">
        <div>{site.footerText || `© 2026 ${site.name} · 模型中枢`}</div>
        {/* 页脚次级链接：低频但常驻的入口（文档）与站点信息类页面同一行 */}
        <div className="p-foot-links">
          {portalFooterNav.map((item) => (
            <Link key={item.path} to={item.path}>
              {item.title}
            </Link>
          ))}
          <Link to="/about">关于我们</Link>
          <Link to="/agreement">用户协议</Link>
        </div>
        {site.icpBeian ? <div>{site.icpBeian}</div> : null}
      </footer>
      <PortalTabBar isAdmin={user?.role === 0} />
      {/* 弹窗挂在布局根层、顶栏之外：`.p-top` 有 backdrop-filter（会给 fixed 后代建包含块），
          挂进顶栏会被困在顶栏盒子里 */}
      {acct.mounted ? (
        <AccountDialog open={acct.open} scope="portal" onClose={acct.closeDialog} />
      ) : null}
    </NoticeOpenProvider>
  );
}
