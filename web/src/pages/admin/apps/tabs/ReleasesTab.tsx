import { useMemo, useState } from "react";

import { confirmDialog } from "@/components/common/ConfirmDialog";
import { EmptyState } from "@/components/common/EmptyState";
import { Pagination } from "@/components/common/Pagination";
import { Tag } from "@/components/common/StatusTag";
import { toastOk } from "@/components/common/Toaster";
import { toastApiError } from "@/lib/apiToast";
import {
  useAdminAppReleasesQuery,
  useDeleteAppReleaseMutation,
  useSetAppReleaseStatusMutation,
} from "@/lib/api";
import { DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import { formatDateTime } from "@/lib/format";
import type { AdminAppItem, AdminAppRelease } from "@/types";
import { ReleaseEditorDrawer } from "./ReleaseEditorDrawer";

/** 版本状态 → 徽标。数字含义见后端 model/app_release.go。 */
function StatusTag({ status, current }: { status: number; current: boolean }) {
  if (status === 2) {
    // 允许多个版本同时"已发布"（hotfix 回填是合法操作），但对外只下发
    // published_at 最新的那个。不标出来，运营只能靠猜 —— 猜错就会撤回正在生效的版本。
    return current ? <Tag tone="success">已发布 · 当前下发</Tag> : <Tag tone="success">已发布</Tag>;
  }
  if (status === 3) return <Tag tone="gray">已撤回</Tag>;
  return <Tag tone="warning">草稿</Tag>;
}

/**
 * 版本发布（单通道）。
 *
 * 「最近发布」按 published_at 排序，**不按版本号大小**：hotfix 回填（发 1.1.9
 * 去补 1.2.0 的漏洞）是合法操作，按 semver 取最大会让线上静默降级。
 */
export function ReleasesTab({ app }: { app: AdminAppItem }) {
  const [page, setPage] = useState(1);
  const params = useMemo(() => ({ page, page_size: DEFAULT_PAGE_SIZE }), [page]);
  const list = useAdminAppReleasesQuery(app.id, params);
  const setStatus = useSetAppReleaseStatusMutation();
  const remove = useDeleteAppReleaseMutation();
  const [editor, setEditor] = useState<{ row: AdminAppRelease | null } | null>(null);

  const items = list.data?.items ?? [];
  const total = list.data?.total ?? 0;
  const currentId = list.data?.current_release_id ?? null;

  const publish = async (r: AdminAppRelease) => {
    const ok = await confirmDialog({
      title: `发布 v${r.version}？`,
      body: r.force_update
        ? "该版本标记了「强制更新」：发布后未升级的终端将被要求先升级才能继续使用。其他已发布版本会保留记录，但对外只下发最新发布的那个。"
        : "发布后终端即可通过公开接口拉到这个版本。其他已发布版本会保留记录，但对外只下发最新发布的那个。",
      okText: "发布",
      okVariant: "primary",
    });
    if (!ok) return;
    try {
      await setStatus.mutateAsync({ id: app.id, rid: r.id, status: 2 });
      toastOk(`v${r.version} 已发布`);
    } catch (e) {
      toastApiError(e, "发布失败");
    }
  };

  const revoke = async (r: AdminAppRelease) => {
    const ok = await confirmDialog({
      title: `撤回 v${r.version}？`,
      body: "撤回后终端不再拉到这个版本；已发布的记录会保留，可重新发布。",
      okText: "撤回",
      okVariant: "danger",
    });
    if (!ok) return;
    try {
      await setStatus.mutateAsync({ id: app.id, rid: r.id, status: 3 });
      toastOk(`v${r.version} 已撤回`);
    } catch (e) {
      toastApiError(e, "撤回失败");
    }
  };

  const askDelete = async (r: AdminAppRelease) => {
    const ok = await confirmDialog({
      title: `删除草稿 v${r.version}？`,
      body: "只有从未发布过的版本可以删除。",
      okText: "删除",
      okVariant: "danger",
    });
    if (!ok) return;
    try {
      await remove.mutateAsync({ id: app.id, rid: r.id });
      if (items.length === 1 && page > 1) setPage(page - 1);
      toastOk("草稿已删除");
    } catch (e) {
      toastApiError(e, "删除失败");
    }
  };

  return (
    <div className="tab-panel">
      <div className="app-tab-head">
        <span className="small muted">
          「最近发布」按发布时间排序，不按版本号大小；已发布的版本不可删除，只能撤回。
        </span>
        <button type="button" className="btn btn-primary btn-sm" onClick={() => setEditor({ row: null })}>
          新建版本
        </button>
      </div>

      {list.isPending ? (
        <EmptyState variant="loading" title="加载中…" />
      ) : list.isError ? (
        <EmptyState variant="error" title="版本列表加载失败" desc="请稍后重试" />
      ) : items.length === 0 ? (
        <EmptyState title="还没有版本" desc="新建版本先保存为草稿，确认下载地址无误后再发布" />
      ) : (
        <div className="app-list">
          {items.map((r) => (
            <div className="app-list-row" key={r.id}>
              <div className="main">
                <div className="title mono">v{r.version}</div>
                <div className="small muted">
                  {r.published_at ? `发布于 ${formatDateTime(r.published_at)}` : "未发布"}
                  {r.force_update ? " · 强制更新" : ""}
                </div>
              </div>
              <StatusTag status={r.status} current={r.id === currentId} />
              {r.status === 2 ? (
                <button
                  type="button"
                  className="btn btn-sm btn-text"
                  disabled={setStatus.isPending}
                  onClick={() => void revoke(r)}
                >
                  撤回
                </button>
              ) : (
                <button
                  type="button"
                  className="btn btn-sm btn-text"
                  disabled={setStatus.isPending}
                  onClick={() => void publish(r)}
                >
                  {r.status === 3 ? "重新发布" : "发布"}
                </button>
              )}
              <button
                type="button"
                className="btn btn-sm btn-text"
                onClick={() => setEditor({ row: r })}
              >
                编辑
              </button>
              {r.status === 1 ? (
                <button
                  type="button"
                  className="btn btn-sm btn-ghost-danger"
                  disabled={remove.isPending}
                  onClick={() => void askDelete(r)}
                >
                  删除
                </button>
              ) : null}
            </div>
          ))}
        </div>
      )}

      {total > 0 ? (
        <Pagination total={total} page={page} pageSize={DEFAULT_PAGE_SIZE} onPageChange={setPage} />
      ) : null}

      <ReleaseEditorDrawer
        appId={app.id}
        open={editor !== null}
        row={editor?.row ?? null}
        onClose={() => setEditor(null)}
        onCreated={() => setPage(1)}
      />
    </div>
  );
}
