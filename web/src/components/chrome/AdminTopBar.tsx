import { Link, useLocation } from "@tanstack/react-router";
import { adminTitleOf } from "@/lib/nav";
import { Icon } from "@/components/common/Icon";
import { usePortalConfigQuery } from "@/lib/api";
import { useAuthStore } from "@/stores/auth";
import type { UserRole } from "@/types";
import { NoticeBell } from "./NoticeBell";
import { ThemeToggle } from "./ThemeToggle";
import { UserMenu } from "./UserMenu";

export interface AdminTopBarProps {
  /** 汉堡按钮（≤767px 展开侧栏） */
  onToggleSidebar: () => void;
  username?: string;
  email?: string;
  /** 头像 data URL；空 = 默认头像 */
  avatarUrl?: string;
  /** 余额（微），显示在用户菜单面板头 */
  balance?: number;
  role?: UserRole;
  /** 打开「账户设置」弹窗（入口已从侧栏移到头像菜单） */
  onOpenAccount?: () => void;
  onLogout?: () => void;
}

/** 管理台顶栏（面包屑 + 全局动作） */
export function AdminTopBar({
  onToggleSidebar,
  username,
  email,
  avatarUrl,
  balance,
  role,
  onOpenAccount,
  onLogout,
}: AdminTopBarProps) {
  const { pathname } = useLocation();
  const title = adminTitleOf(pathname);
  // 公告只有一个数据源（/api/portal/announcements）。临时口令态下后端只放行 4 条路径，
  // 挂上去就是每渲染一次 403 一次，所以直接不渲染。
  const mustChange = useAuthStore((s) => s.user?.must_change_password === true);
  // 关闭门户后「返回门户」必被守卫弹回登录页：入口留着只会让人以为坏了。
  const portalOpen = usePortalConfigQuery().data?.portal_enabled !== false;
  return (
    <div className="a-top">
      <button
        type="button"
        className="icon-btn a-burger"
        aria-label="展开导航"
        onClick={onToggleSidebar}
      >
        <Icon name="menu" size={18} />
      </button>
      <span className="crumb">
        {/* 「管理台」前缀在 ≤767px 隐藏，把宽度让给页面标题（否则小屏只剩「管理台 / 仪…」） */}
        <span className="a-crumb-root">管理台</span>
        {title ? (
          <>
            <span className="sep">/</span>
            <b>{title}</b>
          </>
        ) : null}
      </span>
      {/* 动作组独立成块：靠 .a-acts 的 margin-left:auto 恒贴右，不随面包屑宽度漂移 */}
      <div className="a-acts">
        {/* 与门户同一组件（按钮 + 弹层）：公告没有「管理台专用视图」，整页已删除 */}
        {mustChange ? null : <NoticeBell />}
        <ThemeToggle />
        <UserMenu
          username={username}
          email={email}
          avatarUrl={avatarUrl}
          balance={balance}
          role={role}
          admin
          onOpenAccount={onOpenAccount}
          onLogout={onLogout}
          extraItems={
            portalOpen ? (
              <Link to="/home" role="menuitem">
                <Icon name="external" size={15} />
                返回门户
              </Link>
            ) : null
          }
        />
      </div>
    </div>
  );
}
