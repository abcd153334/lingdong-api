import { useMemo, useState } from "react";

import { confirmDialog } from "@/components/common/ConfirmDialog";
import { EmptyState } from "@/components/common/EmptyState";
import { Pagination } from "@/components/common/Pagination";
import { SwitchRow } from "@/components/common/SwitchRow";
import { Tag } from "@/components/common/StatusTag";
import { toastOk } from "@/components/common/Toaster";
import { toastApiError } from "@/lib/apiToast";
import {
  useAdminAppNoticesQuery,
  useDeleteAppNoticeMutation,
  useSetAppNoticeStatusMutation,
} from "@/lib/api";
import { DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import { formatDateTime } from "@/lib/format";
import type { AdminAppItem, AdminAppNotice } from "@/types";
import { NoticeEditorDrawer } from "./NoticeEditorDrawer";

const LEVEL_TEXT: Record<AdminAppNotice["level"], string> = {
  info: "普通",
  warn: "提醒",
  critical: "重要",
};
const LEVEL_TONE: Record<AdminAppNotice["level"], "info" | "warning" | "danger"> = {
  info: "info",
  warn: "warning",
  critical: "danger",
};

/**
 * 生效区间的可读描述。
 *
 * "未生效"只由后端下发的 `active` 决定，不再用浏览器时钟去猜"未开始/已过期"：
 * 浏览器时间与服务器不一致时（客户端时钟慢/快几分钟）两个分支都不命中，
 * 结果是一行公告既不显示原因也不显示"未生效"，运营只能猜。
 */
function windowText(n: AdminAppNotice): string {
  const parts: string[] = [];
  if (!n.enabled) parts.push("已停用");
  else if (!n.active) parts.push("未在生效时间内");
  if (n.starts_at && n.ends_at) {
    parts.push(`${formatDateTime(n.starts_at)} → ${formatDateTime(n.ends_at)}`);
  } else if (n.starts_at) {
    parts.push(`${formatDateTime(n.starts_at)} 起长期`);
  } else if (n.ends_at) {
    parts.push(`至 ${formatDateTime(n.ends_at)}`);
  } else {
    parts.push("长期生效");
  }
  return parts.join(" · ");
}

/**
 * 应用公告。
 *
 * 与站点公告（系统配置 → 通用里的「公告」）是两套数据：那一套给门户/管理台的
 * 登录用户看，有已读状态；这一套按应用隔离，只对该应用的终端下发。
 */
export function NoticesTab({ app }: { app: AdminAppItem }) {
  const [page, setPage] = useState(1);
  const params = useMemo(() => ({ page, page_size: DEFAULT_PAGE_SIZE }), [page]);
  const list = useAdminAppNoticesQuery(app.id, params);
  const setStatus = useSetAppNoticeStatusMutation();
  const remove = useDeleteAppNoticeMutation();
  const [editor, setEditor] = useState<{ row: AdminAppNotice | null } | null>(null);

  const items = list.data?.items ?? [];
  const total = list.data?.total ?? 0;

  const toggle = async (n: AdminAppNotice, enabled: boolean) => {
    try {
      await setStatus.mutateAsync({ id: app.id, nid: n.id, enabled });
      toastOk(enabled ? "公告已启用" : "公告已停用");
    } catch (e) {
      toastApiError(e, "操作失败");
    }
  };

  const askDelete = async (n: AdminAppNotice) => {
    const ok = await confirmDialog({
      title: "删除该公告？",
      body: `「${n.title}」将被删除，终端将立即不再收到它。`,
      okText: "删除",
      okVariant: "danger",
    });
    if (!ok) return;
    try {
      await remove.mutateAsync({ id: app.id, nid: n.id });
      // 删掉的是本页最后一条时，页码会停在一个已经不存在的第 N 页：
      // 列表变空、分页条随之隐藏，用户既看不到数据也没有回到上一页的入口。
      if (items.length === 1 && page > 1) setPage(page - 1);
      toastOk("公告已删除");
    } catch (e) {
      toastApiError(e, "删除失败");
    }
  };

  return (
    <div className="tab-panel">
      <div className="app-tab-head">
        <span className="small muted">仅对使用本应用的终端用户可见；停用或过期后不再下发。</span>
        <button type="button" className="btn btn-primary btn-sm" onClick={() => setEditor({ row: null })}>
          新建公告
        </button>
      </div>

      {list.isPending ? (
        <EmptyState variant="loading" title="加载中…" />
      ) : list.isError ? (
        <EmptyState variant="error" title="公告加载失败" desc="请稍后重试" />
      ) : items.length === 0 ? (
        <EmptyState title="还没有公告" desc="新建公告后，接入方的终端即可通过公开接口拉到" />
      ) : (
        <div className="app-list">
          {items.map((n) => (
            <div className="app-list-row" key={n.id}>
              <div className="main">
                <div className="title">{n.title}</div>
                <div className="small muted">{windowText(n)}</div>
              </div>
              <Tag tone={LEVEL_TONE[n.level]}>{LEVEL_TEXT[n.level]}</Tag>
              <SwitchRow
                label=""
                ariaLabel={`启用公告：${n.title}`}
                checked={n.enabled}
                disabled={setStatus.isPending}
                onChange={(v) => void toggle(n, v)}
              />
              <button
                type="button"
                className="btn btn-sm btn-text"
                onClick={() => setEditor({ row: n })}
              >
                编辑
              </button>
              <button
                type="button"
                className="btn btn-sm btn-ghost-danger"
                disabled={remove.isPending}
                onClick={() => void askDelete(n)}
              >
                删除
              </button>
            </div>
          ))}
        </div>
      )}

      {/* 分页条按 total 渲染：按 items 渲染时，删到本页为空会连"回上一页"的入口一起消失。 */}
      {total > 0 ? (
        <Pagination total={total} page={page} pageSize={DEFAULT_PAGE_SIZE} onPageChange={setPage} />
      ) : null}

      <NoticeEditorDrawer
        appId={app.id}
        open={editor !== null}
        row={editor?.row ?? null}
        onClose={() => setEditor(null)}
        onCreated={() => setPage(1)}
      />
    </div>
  );
}
