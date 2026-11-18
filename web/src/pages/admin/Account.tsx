import { useNavigate } from "@tanstack/react-router";
import { AccountDialog } from "@/components/account/AccountDialog";

/**
 * `/admin/account`：账户设置弹窗的**落地页**（深链 + 强制改密的跳转目标）。
 *
 * 入口在顶栏头像菜单里（不占侧栏），弹窗本体由 `AccountDialog` 提供。
 * 这一页必须存在：`beforeLoad` 在 `must_change_password` 时只能 `redirect` 到一个路由；
 * 关闭弹窗后落到仪表盘（`replace` 避免 Back 又打开弹窗）。
 */
export function AdminAccountPage() {
  const navigate = useNavigate();
  return (
    <AccountDialog
      open
      onClose={() => void navigate({ to: "/admin/dashboard", replace: true })}
    />
  );
}
