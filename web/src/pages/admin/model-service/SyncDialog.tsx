import { useState } from "react";
import { toast } from "sonner";
import type { ColumnDef } from "@tanstack/react-table";

import { DataTable } from "@/components/common/DataTable";
import { Dialog } from "@/components/common/Dialog";
import { EmptyState } from "@/components/common/EmptyState";
import { ModalityTag, StatusTag, Tag } from "@/components/common/StatusTag";
import { useUpstreamPreviewMutation, useUpstreamSourcesQuery } from "@/lib/api";
import { ApiError } from "@/lib/apiClient";
import { toastApiError } from "@/lib/apiToast";
import { formatNum } from "@/lib/format";
import type { UpstreamPreviewItem } from "@/types";

/**
 * 上游模型同步（预览 → 手动建）。
 *
 * 从旧模型页搬过来时只改了一处：打开/关闭由父级控制（`open`/`onClose`），
 * 预览结果每次打开都重置 —— 拿着上一次的差异表对着另一个来源看，是这类工具最典型的误操作。
 */
export function SyncDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [syncSource, setSyncSource] = useState("");
  const sourcesQuery = useUpstreamSourcesQuery(open);
  const previewMut = useUpstreamPreviewMutation();
  const sources = sourcesQuery.data?.sources ?? [];
  const preview = previewMut.data?.items;



/**
 * 上游预览里的价格单位是**上游自己的美元 / 1M tokens**，与本平台结算币种无关。
 *
 * 不能套 `moneyOf`：那会把"上游的 $3"按当前币种符号渲染成"¥3"，
 * 运营据此判断差价时方向就错了。
 */
function usdOf(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return "—";
  return `$${formatNum(v, 4)}`;
}

/** 上游同步预览列 */
const PREVIEW_COLUMNS: ColumnDef<UpstreamPreviewItem, unknown>[] = [
  {
    id: "name",
    accessorKey: "name",
    meta: { label: "名称", minWidth: 180 },
    cell: ({ row }) => <span className="mono">{row.original.name}</span>,
  },
  { id: "vendor", accessorKey: "vendor", meta: { label: "供应商", priority: 3 } },
  {
    id: "modality",
    accessorKey: "modality",
    meta: { label: "模态" },
    cell: ({ row }) => <ModalityTag modality={row.original.modality} />,
  },
  {
    id: "input_price",
    accessorKey: "input_price",
    meta: { label: "输入价", align: "right" },
    cell: ({ row }) => <span className="mono small">{usdOf(row.original.input_price)}</span>,
  },
  {
    id: "output_price",
    accessorKey: "output_price",
    meta: { label: "输出价", align: "right" },
    cell: ({ row }) => <span className="mono small">{usdOf(row.original.output_price)}</span>,
  },
  {
    id: "cache_price",
    accessorKey: "cache_price",
    meta: { label: "缓存价", priority: 2 },
    cell: ({ row }) => <span className="mono small">{usdOf(row.original.cache_price)}</span>,
  },
  {
    id: "status",
    accessorKey: "status",
    meta: { label: "状态" },
    cell: ({ row }) => <StatusTag status={row.original.status} />,
  },
];

  const pick = (id: string) => {
    setSyncSource(id);
    // 换来源必须清掉上一个来源的预览：两份不同的差异表长得一模一样。
    previewMut.reset();
  };

  const doPreview = () => {
    if (!syncSource) {
      toast.error("请选择上游来源");
      return;
    }
    const src = sources.find((s) => s.id === syncSource);
    if (src && !src.ok) {
      toast.error("该来源当前不可用");
      return;
    }
    previewMut.mutate(syncSource, { onError: (e) => toastApiError(e, "预览失败") });
  };

  return (
      <Dialog
        open={open}
        onClose={onClose}
        title="上游模型同步"
        size="lg"
        footer={
          <button type="button" className="btn" onClick={onClose}>
            关闭
          </button>
        }
      >
        {sourcesQuery.isError ? (
          <EmptyState
            icon="!"
            title="来源加载失败"
            desc={sourcesQuery.error instanceof ApiError ? sourcesQuery.error.message : "请稍后重试"}
          />
        ) : sourcesQuery.isPending ? (
          <EmptyState title="加载中…" />
        ) : (
          <div>
            {sources.map((s) => (
              <button
                key={s.id}
                type="button"
                disabled={!s.ok}
                onClick={() => pick(s.id)}
                className="bind-item"
              >
                <span>{s.name}</span>
                {!s.ok ? (
                  <span className="right small muted">{s.err || "来源不可用"}</span>
                ) : syncSource === s.id ? (
                  <Tag tone="primary" className="right">
                    已选择
                  </Tag>
                ) : (
                  <span className="right small muted">可用</span>
                )}
              </button>
            ))}
          </div>
        )}

        <button
          type="button"
          className="btn btn-primary"
          disabled={sourcesQuery.isPending || !syncSource || previewMut.isPending}
          onClick={doPreview}
        >
          获取预览
        </button>

        {previewMut.isPending ? (
          <EmptyState title="获取预览中…" />
        ) : preview ? (
          <div className="mt16">
            <DataTable
              columns={PREVIEW_COLUMNS}
              data={preview}
              empty={<EmptyState icon="◇" title="暂无差异数据" desc="该来源没有可同步的模型" />}
            />
          </div>
        ) : previewMut.isError ? (
          <EmptyState
            icon="!"
            title="预览失败"
            desc={previewMut.error instanceof ApiError ? previewMut.error.message : "请稍后重试"}
          />
        ) : null}
      </Dialog>
  );
}
