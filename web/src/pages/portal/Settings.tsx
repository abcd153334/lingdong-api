import { useNavigate } from "@tanstack/react-router";
import { AccountDialog } from "@/components/account/AccountDialog";
import { PageHead } from "@/components/common/PageHead";

/**
 * 用户中心（S2）。
 *
 * 这里**不再是一整页内容**：资料、账户安全（改密 / 换绑 / 两步验证）、登录设备都在
 * `AccountDialog` 里，与管理台「账户设置」是同一个组件（`scope` 只改标题）。本页只剩两件事：
 *  1. 提供 h1「用户中心」—— 深链、移动端 TabBar「我的」、以及 responsive/layout/portal-chrome
 *     三处 e2e 的 h1 契约都指向它；
 *  2. 作为 `must_change_password` 的落地 URL（路由守卫只能 redirect 到一个页面，不能 redirect 到弹窗）。
 *
 * 关闭弹窗后去首页：这一页没有"原页面"可回，留在原地就是一个只有标题的空壳。
 * 也**不摆「充值 / 兑换码」按钮**：它们永远在遮罩下面，是点不到的死按钮；钱包入口在顶栏余额胶囊与 `/topup`。
 */
export function SettingsPage() {
  const navigate = useNavigate();
  return (
    <>
      <PageHead title="用户中心" desc="资料、账户安全与登录设备" />
      <AccountDialog
        open
        scope="portal"
        onClose={() => void navigate({ to: "/home", replace: true })}
      />
    </>
  );
}
