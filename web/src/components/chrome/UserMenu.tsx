import { useEffect, useRef, useState, type ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import { Icon } from "@/components/common/Icon";
import { UserAvatar } from "@/components/common/UserAvatar";
import { moneyOf } from "@/lib/money";
import { cn } from "@/lib/utils";
import type { UserRole } from "@/types";

export interface UserMenuProps {
  username?: string;
  email?: string;
  /** 头像 data URL；空 = 渲染打包进产物的默认头像 */
  avatarUrl?: string;
  /** 余额（微）。未加载时传 undefined → 显示 — */
  balance?: number;
  role?: UserRole;
  /** 是否为管理台（菜单项差异：管理台不展示门户菜单） */
  admin?: boolean;
  /** 自定义菜单项插槽（管理台「返回门户」、门户「切到管理台」等） */
  extraItems?: ReactNode;
  /** 打开账户弹窗（管理台「账户设置」/门户「用户中心」就地展开，不跳页） */
  onOpenAccount?: () => void;
  /** 该菜单项文案：管理台「账户设置」/门户「用户中心」（两端的叫法是既有的，不由 admin 派生） */
  accountLabel?: string;
  onLogout?: () => void;
}

/**
 * 头像下拉菜单（对齐原型 .p-user.open .menu）。
 *
 * 面板靠 CSS 的 `display:none` 隐藏而**不卸载**：`chrome.test.tsx` 在菜单未展开时
 * 就用 `getByRole("menuitem")` 找项（jsdom 不加载样式表），改成条件渲染会直接红。
 */
export function UserMenu({
  username = "未登录",
  email,
  avatarUrl,
  balance,
  role,
  admin,
  extraItems,
  onOpenAccount,
  accountLabel = "账户设置",
  onLogout,
}: UserMenuProps) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const btnRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (!open) return;
    const onDocClick = (e: MouseEvent) => {
      if (!rootRef.current?.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      setOpen(false);
      // 关闭后焦点回到触发按钮：否则键盘用户会掉回文档开头。
      btnRef.current?.focus();
    };
    document.addEventListener("click", onDocClick);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("click", onDocClick);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  return (
    <div className={cn("p-user", open && "open")} ref={rootRef}>
      <button
        ref={btnRef}
        type="button"
        className="p-user-btn"
        aria-label="用户菜单"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <UserAvatar url={avatarUrl} size="md" />
      </button>
      <div className="menu" role="menu">
        <div className="p-user-head">
          <div className="p-user-head__row">
            <UserAvatar url={avatarUrl} size="lg" />
            <div className="p-user-head__id">
              <div className="p-user-head__line">
                <span className="p-user-head__name">{username}</span>
                {role === undefined ? null : (
                  <span className={cn("badge", role === 0 ? "badge-purple" : "badge-gray")}>
                    {role === 0 ? "管理员" : "普通用户"}
                  </span>
                )}
              </div>
              {email ? <div className="p-user-head__meta">{email}</div> : null}
            </div>
          </div>
          <div className="p-user-head__balance">
            <span>余额</span>
            <b className="mono">{balance === undefined ? "—" : moneyOf(balance)}</b>
          </div>
        </div>
        <div className="sep" />
        {onOpenAccount ? (
          <button
            type="button"
            role="menuitem"
            onClick={() => {
              setOpen(false);
              onOpenAccount();
            }}
          >
            <Icon name="user" size={15} />
            {accountLabel}
          </button>
        ) : null}
        {extraItems}
        {!admin ? (
          /* 账户入口只有上面那一项（两端都传 onOpenAccount，就地开弹窗）。
             这里**不再**放一条跳 /settings 的「用户中心」：同名两项会让人以为是两个功能。 */
          <Link to="/keys" search={{ create: 0 }} role="menuitem" onClick={() => setOpen(false)}>
            <Icon name="key" size={15} />
            API 密钥
          </Link>
        ) : null}
        <div className="sep" />
        <button
          type="button"
          role="menuitem"
          onClick={() => {
            setOpen(false);
            onLogout?.();
          }}
        >
          <Icon name="logout" size={15} />
          退出登录
        </button>
      </div>
    </div>
  );
}
