import { Link, useLocation } from "@tanstack/react-router";
import { portalMobileNavFor } from "@/lib/nav";
import { Icon } from "@/components/common/Icon";

export interface PortalTabBarProps {
  /** 是否为管理员（role=0）；为真时追加「管理台」项（J4） */
  isAdmin?: boolean;
}

/** 门户移动端底部 Tab Bar（≤767px 显示；触控目标 ≥44px） */
export function PortalTabBar({ isAdmin = false }: PortalTabBarProps) {
  const { pathname } = useLocation();
  return (
    <nav className="p-tabbar" aria-label="主导航">
      {portalMobileNavFor(isAdmin).map((item) => (
        <Link
          key={item.path}
          to={item.path}
          className={pathname === item.path ? "active" : undefined}
        >
          <Icon name={item.icon} size={19} />
          <span>{item.mobileLabel ?? item.title}</span>
        </Link>
      ))}
    </nav>
  );
}
