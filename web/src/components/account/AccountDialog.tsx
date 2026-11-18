import { useState } from "react";
import { Dialog } from "@/components/common/Dialog";
import { Icon, type IconName } from "@/components/common/Icon";
import { UserAvatar } from "@/components/common/UserAvatar";
import { AccountDrawers } from "@/components/portal/AccountDrawers";
import { AvatarPicker } from "@/components/portal/AvatarPicker";
import { DeviceList } from "@/components/portal/DeviceList";
import { MfaCard } from "@/components/portal/MfaCard";
import { ContactRow, PasswordRow } from "@/components/portal/SecurityRows";
import { useAccountSecurity } from "@/components/portal/useAccountSecurity";
import { useMeQuery } from "@/lib/api";
import { CONTACT_CHANNELS } from "@/lib/contactChannel";
import { groupDiscountText, moneyOf } from "@/lib/money";
import { cn } from "@/lib/utils";
import { useAuthStore } from "@/stores/auth";

type Section = "profile" | "security" | "devices";

const SECTIONS: { key: Section; label: string; icon: IconName }[] = [
  { key: "profile", label: "资料", icon: "user" },
  { key: "security", label: "账户安全", icon: "shield" },
  { key: "devices", label: "登录设备", icon: "monitor" },
];

export interface AccountDialogProps {
  open: boolean;
  onClose: () => void;
  /**
   * 谁在用它：管理台「账户设置」/ 门户「用户中心」。
   *
   * 只改标题 —— 分区、行组件、抽屉、`forced` 行为完全相同。两个入口各写一份页面，
   * 等于把改密/换绑/设备/两步验证各复制一遍，任何一处修文案都会漏掉另一端。
   */
  scope?: "admin" | "portal";
}

/**
 * 账户设置弹窗（管理台与门户的唯一实现：左侧分区导航 + 右侧内容）。
 *
 * 为什么必须是弹窗：入口在顶栏头像菜单里，点一下就地展开、关掉回到原页面；
 * `/admin/account` 与 `/settings` 两个路由只是它的落地页（深链 + 强制改密的跳转目标）。
 *
 * 三个硬约束：
 *  1. **改密 / 换绑抽屉必须是 `<Dialog>` 的兄弟节点**（`.dialog` 有 transform/translate，
 *     会建立 fixed 包含块，`.dl-body` 还会裁切）；
 *  2. **外壳定高**（`.acct-dialog`）+ 右栏内部滚动：`.dialog` 是自高度垂直居中盒，
 *     内容一变高就会上下各弹半格（见 components.css 里 `.acct-dialog` 的注释）；
 *  3. `forced`（临时口令未改）从服务端字段派生，不额外传参 —— 此时只渲染账户安全分区、
 *     隐藏分区导航与关闭按钮、抽屉自动打开、外壳走自然高。
 */
export function AccountDialog({ open, onClose, scope = "admin" }: AccountDialogProps) {
  const stored = useAuthStore((s) => s.user);
  const me = useMeQuery().data;

  // 临时口令态：后端只放行改密等 4 条路径，其余入口一律 403 —— 界面上就别摆死按钮。
  const forced = me?.must_change_password === true;
  const [section, setSection] = useState<Section>("profile");
  const current: Section = forced ? "security" : section;

  const s = useAccountSecurity({ me, mustChange: forced });

  const username = me?.username ?? stored?.username ?? "";
  const role = me?.role ?? stored?.role ?? 1;
  const group = me?.group ?? stored?.group ?? "default";
  // 未加载时显示「—」而不是 0：0 会被读成「没花过钱」，与实际语义相反
  const balance = me?.balance ?? stored?.balance;

  return (
    <>
      <Dialog
        open={open}
        onClose={forced ? () => {} : onClose}
        title={scope === "portal" ? "用户中心" : "账户设置"}
        size="xl"
        closeButton={!forced}
        className={cn("acct-dialog", forced && "acct-dialog--auto")}
      >
        <div className="acct">
          <aside className="acct-rail">
            <div className="uc-hero">
              {/* 头像只出现一次：这里就是换头像的入口（临时口令态换成只读渲染 —— 必 403） */}
              <div className="uc-hero__avatar">
                {forced ? (
                  <UserAvatar url={me?.avatar_url} size="lg" />
                ) : (
                  <AvatarPicker url={me?.avatar_url} size="lg" showRemove />
                )}
              </div>
              {/* 右栏两行：名称 + 角色 / 余额。竖排会让头像孤零零浮在左上，余额被挤到第 4 行 */}
              <div className="uc-hero__id">
                <div className="uc-hero__line">
                  <span className="uc-hero__name" title={username}>
                    {username}
                  </span>
                  <span className={cn("badge", role === 0 ? "badge-purple" : "badge-gray")}>
                    {role === 0 ? "管理员" : "普通用户"}
                  </span>
                </div>
                <div className="uc-hero__balance">
                  <span>余额</span>
                  <b className="mono">{moneyOf(balance)}</b>
                </div>
              </div>
            </div>
            {!forced && (
              <nav className="acct-nav" aria-label="账户设置分区">
                {SECTIONS.map((it) => (
                  <button
                    key={it.key}
                    type="button"
                    className={cn(current === it.key && "is-on")}
                    aria-current={current === it.key ? "page" : undefined}
                    onClick={() => setSection(it.key)}
                  >
                    <Icon name={it.icon} size={15} />
                    {it.label}
                  </button>
                ))}
              </nav>
            )}
          </aside>

          <div className="acct-pane">
            {current === "profile" && (
              <div className="acct-sec">
                <div className="uc-row">
                  <div className="uc-row-main">
                    <div className="rl">用户名</div>
                    <div className="rs mono">{username}</div>
                  </div>
                </div>
                <div className="uc-row">
                  <div className="uc-row-main">
                    <div className="rl">用户 ID</div>
                    <div className="rs mono">{me?.id ?? stored?.id ?? "—"}</div>
                  </div>
                </div>
                <div className="uc-row">
                  <div className="uc-row-main">
                    <div className="rl">计费分组</div>
                    {/* 只读：分组同时决定折扣与可用模型池，改动入口在「用户管理」。 */}
                    <div className="rs">
                      {group} · {groupDiscountText(me?.group_discount)}
                    </div>
                  </div>
                </div>
                <div className="uc-row">
                  <div className="uc-row-main">
                    <div className="rl">累计消耗</div>
                    <div className="rs mono">{moneyOf(me?.used)}</div>
                  </div>
                </div>
                <div className="uc-row">
                  <div className="uc-row-main">
                    <div className="rl">请求次数</div>
                    <div className="rs mono">{me?.request_count ?? "—"}</div>
                  </div>
                </div>
              </div>
            )}

            {current === "security" && (
              <div className="acct-sec">
                <PasswordRow mustChange={forced} onEdit={() => s.setPassOpen(true)} />
                {/* 临时口令态：换绑接口全部 403，绑定行与 mfa 卡都不渲染 */}
                {!forced &&
                  CONTACT_CHANNELS.map((ch) => (
                    <ContactRow
                      key={ch}
                      channel={ch}
                      email={s.email}
                      phone={s.phone}
                      available={s.availableOf(ch)}
                      onBind={() => s.setBindChannel(ch)}
                    />
                  ))}
                {!forced && <MfaCard />}
              </div>
            )}

            {current === "devices" && <DeviceList />}
          </div>
        </div>
      </Dialog>

      <AccountDrawers
        passOpen={s.passOpen}
        onClosePass={() => s.setPassOpen(false)}
        bindChannel={s.bindChannel}
        onCloseBind={() => s.setBindChannel(null)}
        email={s.email}
        phone={s.phone}
        onUnavailable={s.onUnavailable}
      />
    </>
  );
}
