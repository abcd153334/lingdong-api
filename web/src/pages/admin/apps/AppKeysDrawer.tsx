import { useEffect, useMemo, useState } from "react";

import { confirmDialog } from "@/components/common/ConfirmDialog";
import { Drawer } from "@/components/common/Drawer";
import { EmptyState } from "@/components/common/EmptyState";
import { Pagination } from "@/components/common/Pagination";
import { Tag } from "@/components/common/StatusTag";
import { toastOk } from "@/components/common/Toaster";
import { toastApiError } from "@/lib/apiToast";
import { useAdminAppKeysQuery, useAdminKeyStatusMutation } from "@/lib/api";
import { DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import { formatDateTime, formatMoney } from "@/lib/format";
import type { AdminAppItem, AdminAppKeyItem } from "@/types";

/**
 * 应用下的托管密钥（后台只读）。
 *
 * 不展示明文、也不提供"导出/查看明文"：管理员要帮接入方恢复凭证时，正确动作
 * 是让接入方自己轮换，而不是由后台导出别人的应用凭证。
 *
 * `portal` 必须开：这个抽屉既可能挂在页面上，也可能从管理弹窗里打开 ——
 * 而 `.dl-body` 是 `overflow:auto` 且弹窗有 transform，不 portal 会被裁切。
 *
 * 必须分页：一个应用可能有上百把密钥，只列前 20 把而底部写"共 N 把"，
 * 等于让第 21 把以后的密钥"看得见、管不到"（它们仍然能正常调用）。
 */
export function AppKeysDrawer({ app, onClose }: { app: AdminAppItem | null; onClose: () => void }) {
  const [page, setPage] = useState(1);
  // 抽屉常驻挂载：换应用时必须回到第 1 页，否则会请求新应用的第 3 页。
  useEffect(() => {
    setPage(1);
  }, [app?.id]);

  const params = useMemo(() => ({ page, page_size: DEFAULT_PAGE_SIZE }), [page]);
  const keys = useAdminAppKeysQuery(app?.id ?? 0, params, app !== null);
  const items = keys.data?.items ?? [];
  const total = keys.data?.total ?? 0;
  const toggleStatus = useAdminKeyStatusMutation();

  const askToggle = async (k: AdminAppKeyItem) => {
    const enable = k.status !== 1;
    const ok = await confirmDialog({
      title: enable ? "启用该密钥？" : "禁用该密钥？",
      body: enable ? "启用后接入方立即可继续调用" : "禁用后接入方调用将立即失败",
      okText: enable ? "启用" : "禁用",
      okVariant: enable ? "primary" : "danger",
    });
    if (!ok) return;
    try {
      await toggleStatus.mutateAsync({ id: k.id, status: enable ? 1 : 2, appId: app?.id });
      toastOk(enable ? "密钥已启用" : "密钥已禁用");
    } catch (e) {
      toastApiError(e, "操作失败，请稍后再试");
    }
  };

  return (
    <Drawer open={app !== null} onClose={onClose} title={app ? `${app.name} · 托管密钥` : ""} portal>
      {keys.isError ? (
        // 错误不能渲染成空态：那会让管理员得出"这个应用没人用"的结论，
        // 于是去排查接入方，而真正坏的是接口。
        <EmptyState
          variant="error"
          title="密钥列表加载失败"
          desc="这不代表该应用没有密钥，请重试"
          action={
            <button type="button" className="btn" onClick={() => void keys.refetch()}>
              重试
            </button>
          }
        />
      ) : keys.isPending ? (
        <EmptyState variant="loading" title="加载中…" />
      ) : items.length ? (
        <>
          <div className="table-wrap">
            {items.map((k) => (
              <div className="au-row" key={k.id}>
                <span className="mono">{k.username || `#${k.user_id}`}</span>
                <span className="mono">{k.key_prefix}…</span>
                <Tag tone={k.status === 1 ? "success" : "gray"}>{k.status === 1 ? "启用" : "禁用"}</Tag>
                <span className="mono">{formatMoney(k.used)}</span>
                <span className="muted">
                  {k.rotated_at ? `轮换于 ${formatDateTime(k.rotated_at)}` : "未轮换"}
                </span>
                <button
                  type="button"
                  className={k.status === 1 ? "btn btn-sm btn-ghost-danger" : "btn btn-sm btn-text"}
                  disabled={toggleStatus.isPending}
                  onClick={() => void askToggle(k)}
                >
                  {k.status === 1 ? "禁用" : "启用"}
                </button>
              </div>
            ))}
          </div>
          <Pagination total={total} page={page} pageSize={DEFAULT_PAGE_SIZE} onPageChange={setPage} />
        </>
      ) : (
        <EmptyState title="还没有用户签发密钥" desc="接入方需要在用户登录后调用签发接口" />
      )}
    </Drawer>
  );
}
