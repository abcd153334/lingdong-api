import { useState } from "react";
import { useNavigate } from "@tanstack/react-router";
import { toast } from "sonner";
import { Card } from "@/components/common/Card";
import { Drawer } from "@/components/common/Drawer";
import { timeAgo, uaLabel } from "@/lib/device";
import { formatDateTime } from "@/lib/format";
import {
  useRevokeAllSessionsMutation,
  useRevokeSessionMutation,
  useSessionsQuery,
} from "@/lib/api";

/**
 * 登录设备列表（会话管理）。
 *
 * 三件事必须成立，任何一条破了都是安全缺陷：
 *  1. 只列自己的会话 —— 作用域由服务端从会话里取 uid，前端不传用户 id；
 *  2. 当前设备不给「退出」按钮 —— 点它等于把自己踢掉却仍停在页面上，
 *     走右上角登出走正常的本地清理流程；
 *  3. 「退出全部设备」必须二次确认 —— 它包含当前设备，误点即全端掉线。
 */
export function DeviceList() {
  const navigate = useNavigate();
  const sessionsQuery = useSessionsQuery();
  const revoke = useRevokeSessionMutation();
  const revokeAll = useRevokeAllSessionsMutation();
  const [confirmAll, setConfirmAll] = useState(false);

  // 兜底 `?? []`：后端契约是 []，但桩件/降级响应可能没有该字段，
  // undefined 上取 length 会整页白屏（HTTP 200，日志干净）。
  const rows = sessionsQuery.data?.sessions ?? [];

  const doRevoke = (id: number) => {
    revoke.mutate(id, {
      onSuccess: () => toast.success("该设备已退出登录"),
    });
  };

  const doRevokeAll = () => {
    revokeAll.mutate(undefined, {
      onSuccess: () => {
        setConfirmAll(false);
        toast.success("已退出全部设备，请重新登录");
        // 守卫只在**导航时**才探测会话：此刻本地态已清空，但不导航的话
        // 页面会停在一个已经无权限的界面上继续渲染旧数据。
        void navigate({ to: "/login" });
      },
    });
  };

  return (
    <>
      <Card
        title="登录设备"
        sub={rows.length > 0 ? `共 ${rows.length} 台` : undefined}
        testId="device-list"
        actions={
          rows.length > 1 ? (
            <button
              type="button"
              className="btn btn-sm"
              data-testid="revoke-all-sessions"
              onClick={() => setConfirmAll(true)}
            >
              退出全部设备
            </button>
          ) : undefined
        }
      >
        {sessionsQuery.isPending ? (
          <div className="uc-row">
            <div className="uc-row-main">
              <div className="rs">加载中…</div>
            </div>
          </div>
        ) : rows.length === 0 ? (
          <div className="uc-row">
            <div className="uc-row-main">
              <div className="rl">暂无其它登录设备</div>
              <div className="rs">在其它浏览器或设备登录后会出现在这里</div>
            </div>
          </div>
        ) : (
          rows.map((s) => (
            <div className="uc-row" key={s.id} data-testid={`device-${s.id}`}>
              <div className="uc-row-main">
                <div className="rl">
                  {uaLabel(s.ua)}
                  {s.current ? (
                    <span className="badge badge-primary" style={{ marginLeft: 8 }}>
                      本机
                    </span>
                  ) : null}
                </div>
                <div className="rs">
                  IP <span className="mono">{s.ip || "—"}</span> · 最近活跃 {timeAgo(s.last_seen_at)} ·
                  登录于 {formatDateTime(s.created_at)}
                </div>
              </div>
              <div className="act">
                {s.current ? null : (
                  <button
                    type="button"
                    className="btn btn-sm"
                    data-testid={`revoke-${s.id}`}
                    disabled={revoke.isPending}
                    onClick={() => doRevoke(s.id)}
                  >
                    退出
                  </button>
                )}
              </div>
            </div>
          ))
        )}
      </Card>

      <Drawer
        open={confirmAll}
        onClose={() => setConfirmAll(false)}
        title="退出全部设备"
        // 这张确认抽屉会被放进管理台「账户设置」弹窗（`.dialog` 有 transform/translate，
        // 会给 fixed 后代建立包含块），因此固定挂到 body。
        portal
        actions={[
          { label: "取消", onClick: () => setConfirmAll(false) },
          {
            label: "全部退出",
            variant: "primary",
            disabled: revokeAll.isPending,
            onClick: doRevokeAll,
          },
        ]}
      >
        <div className="alert-banner warn">
          将退出包括当前设备在内的全部登录状态，需要重新输入密码登录。
        </div>
      </Drawer>
    </>
  );
}
