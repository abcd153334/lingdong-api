import { Link, useLocation } from "@tanstack/react-router";
import { portalDesktopNav } from "@/lib/nav";
import { useSite } from "@/lib/site";
import { moneyOf } from "@/lib/money";
import { Icon } from "@/components/common/Icon";
import { SiteLogo } from "@/components/common/SiteLogo";
import { NoticeBell } from "./NoticeBell";
import { ThemeToggle } from "./ThemeToggle";
import { UserMenu } from "./UserMenu";

export interface PortalTopBarProps {
  /** 余额（微，带符号渲染）；S3 接数据前为 undefined（显示 —） */
  balance?: number | null;
  username?: string;
  email?: string;
  /** 头像 data URL；空 = 默认头像 */
  avatarUrl?: string;
  /** 是否为管理员（role=0）；为真时在用户菜单提供「切到管理台」入口 */
  isAdmin?: boolean;
  /** 头像菜单就地打开「用户中心」弹窗（与管理台 onOpenAccount 同构） */
  onOpenAccount?: () => void;
  onLogout?: () => void;
}

/** 门户顶栏（玻璃 sticky；≤767px 隐藏桌面导航与余额标签，主题入口保留） */
export function PortalTopBar({
  balance,
  username,
  email,
  avatarUrl,
  isAdmin,
  onOpenAccount,
  onLogout,
}: PortalTopBarProps) {
  const { pathname } = useLocation();
  const site = useSite();

  return (
    <header className="p-top">
      <Link to="/home" className="p-logo" aria-label={site.name}>
        <SiteLogo />
        <span>{site.name}</span>
      </Link>
      <nav className="p-nav" aria-label="门户导航">
        {portalDesktopNav.map((item) => (
          <Link
            key={item.path}
            to={item.path}
            className={pathname === item.path ? "active" : undefined}
          >
            {/* 图标随文字走色（lucide 用 currentColor）：hover/激活无需额外规则 */}
            <Icon name={item.icon} size={15} />
            <span>{item.title}</span>
          </Link>
        ))}
      </nav>
      <div className="p-right">
        <Link to="/topup" className="balance-pill" aria-label="钱包余额">
          <Icon name="wallet" size={15} />
          <span className="lbl">余额</span>
          <span className="mono">
            {balance === undefined || balance === null ? "—" : `${moneyOf(balance)}`}
          </span>
        </Link>
        <NoticeBell />
        <ThemeToggle />
        <UserMenu
          username={username}
          email={email}
          avatarUrl={avatarUrl}
          balance={balance ?? undefined}
          role={isAdmin ? 0 : 1}
          admin={false}
          accountLabel="用户中心"
          onOpenAccount={onOpenAccount}
          onLogout={onLogout}
          extraItems={
            isAdmin ? (
              <Link to="/admin/dashboard" role="menuitem">
                <Icon name="shield" size={15} />
                切到管理台
              </Link>
            ) : null
          }
        />
      </div>
    </header>
  );
}
