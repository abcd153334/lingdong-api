import { useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { toast } from "sonner";
import { CallDetailDrawer } from "@/components/common/CallDetailDrawer";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { DataTable } from "@/components/common/DataTable";
import { EmptyState } from "@/components/common/EmptyState";
import { EntityCell } from "@/components/common/EntityCell";
import { FilterBar, type FilterField } from "@/components/common/FilterBar";
import { MoneyText } from "@/components/common/MoneyText";
import { PageHead } from "@/components/common/PageHead";
import { ModalityTag, StatusTag } from "@/components/common/StatusTag";
import { actionsCol } from "@/lib/table";
import { TaskProgress } from "@/components/common/TaskProgress";
import {
  useAdminTaskQuery,
  useAdminTasksQuery,
  useCancelAdminTaskMutation,
  useRetryAdminTaskMutation,
  type AdminTasksParams,
} from "@/lib/api";
import { ApiError, DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import { formatDateTime } from "@/lib/format";
import { isActiveTask } from "@/lib/task";
import type { AdminTaskItem } from "@/types";

const EMPTY_FILTERS: Record<string, string> = { fStatus: "" };

/** 状态筛选取值对齐后端冻结枚举 */
const STATUS_ITEMS: [string, string][] = [
  ["queued", "排队中"],
  ["running", "运行中"],
  ["succeeded", "已成功"],
  ["failed", "失败"],
  ["canceled", "已取消"],
  ["expired", "已过期"],
];

const FILTER_FIELDS: FilterField[] = [
  { t: "select", id: "fStatus", label: "状态", allLabel: "全部状态", items: STATUS_ITEMS },
];

/** 费用：进行中显示冻结金额，终态显示实际结算金额 */
function costCell(t: AdminTaskItem) {
  const active = isActiveTask(t.status);
  return (
    <div>
      <MoneyText micro={active ? t.frozen : t.settled} />
      <br />
      <span className="muted small">{active ? "冻结中" : "已结算"}</span>
    </div>
  );
}

/**
 * 异步任务（S7）
 * 长任务六态（排队/运行/成功/失败/超时/取消）、进度、预扣与结算额度、产物与到期清理；
 * 失败/超时/取消自动全额退款。仅 queued 可由人工取消，其余后端拒绝（task_not_cancelable）。
 */
export function AdminTasksPage() {
  const [page, setPage] = useState(1);
  const [filters, setFilters] = useState<Record<string, string>>(EMPTY_FILTERS);
  const [detail, setDetail] = useState<AdminTaskItem | null>(null);
  // 产物清单只在详情接口下发（列表补会造成 N+1），抽屉打开时按需取。
  const detailQ = useAdminTaskQuery(detail?.id ?? null);
  // 详情未到时先用列表行渲染摘要与任务信息，避免打开瞬间的空窗。
  const current = detailQ.data ?? detail;

  const params: AdminTasksParams = {
    page,
    page_size: DEFAULT_PAGE_SIZE,
    status: filters.fStatus || undefined,
  };
  const query = useAdminTasksQuery(params);
  const items = query.data?.items ?? [];
  const total = query.data?.total ?? 0;

  const cancelMut = useCancelAdminTaskMutation();
  const retryMut = useRetryAdminTaskMutation();

  const cancelTask = async (t: AdminTaskItem) => {
    const ok = await confirmDialog({
      title: "取消异步任务",
      body: (
        <>
          确定取消任务 <b>#{t.id}</b>？取消后预扣额度将全额退回。
        </>
      ),
      okText: "确认取消",
      okVariant: "danger",
    });
    if (!ok) return;
    cancelMut.mutate(t.id, {
      onSuccess: () => {
        toast.success("任务已取消");
        setDetail(null);
      },
      onError: (error) => {
        if (error instanceof ApiError && error.errorCode === "task_not_cancelable") {
          toast.error(error.message || "该任务当前不可取消");
          return;
        }
        toast.error(error instanceof ApiError ? error.message : "取消失败，请稍后重试");
      },
    });
  };

  // 重试：后端仅接受 status=failed（轮询超限以 kind=timeout + status=failed 呈现），
  // expired 不在可重试范围（task.Retry 仅 CAS failed），故不渲染重试入口。
  const retryable = (t: AdminTaskItem) => t.status === "failed";

  const retryTask = async (t: AdminTaskItem) => {
    const ok = await confirmDialog({
      title: "重试异步任务",
      body: (
        <>
          确定重试任务 <b>#{t.id}</b>？将重新预扣额度并重新提交上游。
          <div className="small muted mt16">
            再生成一律按 <b>2K</b> 档提交与计费（出站档与计价档必须同一个），
            与原任务当时选的档位无关；预扣额随之上浮时请确认用户余额充足。
          </div>
        </>
      ),
      okText: "确认重试",
    });
    if (!ok) return;
    retryMut.mutate(t.id, {
      onSuccess: () => {
        toast.success("任务已重新提交");
        setDetail(null);
      },
      onError: (error) => {
        if (error instanceof ApiError && error.errorCode === "task_not_retryable") {
          toast.error(error.message || "该任务当前不可重试");
          return;
        }
        toast.error(error instanceof ApiError ? error.message : "重试失败，请稍后重试");
      },
    });
  };

  const columns: ColumnDef<AdminTaskItem, unknown>[] = [
    {
      id: "id",
      accessorKey: "id",
      enableSorting: false,
      meta: { label: "ID" },
      cell: ({ row }) => <span className="mono">{row.original.id}</span>,
    },
    {
      id: "created_at",
      accessorKey: "created_at",
      enableSorting: false,
      meta: { label: "时间" },
      cell: ({ row }) => <span className="mono small">{formatDateTime(row.original.created_at)}</span>,
    },
    {
      id: "user_id",
      accessorKey: "user_id",
      enableSorting: false,
      meta: { label: "用户" },
      cell: ({ row }) => <span className="mono small">#{row.original.user_id}</span>,
    },
    {
      id: "model",
      accessorKey: "model",
      enableSorting: false,
      meta: { label: "模型" },
      cell: ({ row }) => (
        <EntityCell iconUrl={row.original.model_icon_url} name={row.original.model} mono />
      ),
    },
    {
      id: "modality",
      accessorKey: "modality",
      enableSorting: false,
      meta: { label: "类型" },
      cell: ({ row }) => <ModalityTag modality={row.original.modality} />,
    },
    {
      id: "status",
      accessorKey: "status",
      enableSorting: false,
      meta: { label: "状态" },
      cell: ({ row }) => <StatusTag status={row.original.status} />,
    },
    {
      id: "progress",
      accessorKey: "progress",
      enableSorting: false,
      meta: { label: "进度" },
      cell: ({ row }) => (
        <TaskProgress status={row.original.status} progress={row.original.progress} />
      ),
    },
    {
      id: "cost",
      enableSorting: false,
      meta: { label: "费用" },
      cell: ({ row }) => costCell(row.original),
    },
    {
      id: "upstream_task_id",
      accessorKey: "upstream_task_id",
      enableSorting: false,
      meta: { label: "上游 ID" },
      cell: ({ row }) =>
        row.original.upstream_task_id ? (
          <span className="mono small">{row.original.upstream_task_id}</span>
        ) : (
          <span className="muted">—</span>
        ),
    },
    actionsCol<AdminTaskItem>(
      (t) => [
        { icon: "eye", label: "详情", onClick: () => setDetail(t) },
        {
          icon: "close",
          label: "取消",
          tone: "danger",
          hidden: t.status !== "queued",
          busy: cancelMut.isPending,
          onClick: () => void cancelTask(t),
        },
        {
          icon: "refresh",
          label: "重试",
          hidden: !retryable(t),
          busy: retryMut.isPending,
          onClick: () => void retryTask(t),
        },
      ],
      "操作",
      "actions",
    ),
  ];

  return (
    <>
      <PageHead title="异步任务" desc="任务进度查看与额度结算" />

      <FilterBar
        fields={FILTER_FIELDS}
        value={filters}
        onChange={(id, value) => {
          setFilters((f) => ({ ...f, [id]: value }));
          setPage(1);
        }}
        onReset={() => {
          setFilters(EMPTY_FILTERS);
          setPage(1);
        }}
      />

      {query.isLoading ? (
        <EmptyState icon="◇" title="正在加载任务…" />
      ) : query.isError ? (
        <EmptyState
          icon="⚠"
          title="任务加载失败"
          desc={query.error instanceof ApiError ? query.error.message : "请稍后重试"}
          action={
            <button type="button" className="btn btn-primary" onClick={() => void query.refetch()}>
              重试
            </button>
          }
        />
      ) : (
        <DataTable
          columns={columns}
          data={items}
          total={total}
          page={page}
          pageSize={DEFAULT_PAGE_SIZE}
          onPageChange={setPage}
          empty={
            <EmptyState
              icon="rows"
              title="暂无异步任务"
              desc="上游长任务（如视频生成）提交后，会在此跟踪状态与产物。"
            />
          }
        />
      )}

      {/* 与调用记录共用同一个详情抽屉：任务信息、产物预览与扣费口径全站一致。 */}
      <CallDetailDrawer
        open={detail !== null}
        onClose={() => setDetail(null)}
        task={current}
        loading={detailQ.isPending}
        error={
          detailQ.isError
            ? detailQ.error instanceof ApiError
              ? detailQ.error.message
              : "任务详情加载失败"
            : undefined
        }
        onRetry={() => void detailQ.refetch()}
        actions={
          current?.status === "queued" ? (
            <button
              type="button"
              className="btn btn-danger"
              disabled={cancelMut.isPending}
              onClick={() => void cancelTask(current)}
            >
              取消任务
            </button>
          ) : current?.status === "failed" ? (
            <button
              type="button"
              className="btn"
              disabled={retryMut.isPending}
              onClick={() => void retryTask(current)}
            >
              重试
            </button>
          ) : undefined
        }
      />
    </>
  );
}
