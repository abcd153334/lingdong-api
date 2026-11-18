import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { toast } from "sonner";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { DataTable } from "@/components/common/DataTable";
import { CallDetailDrawer } from "@/components/common/CallDetailDrawer";
import { EmptyState } from "@/components/common/EmptyState";
import { EntityCell } from "@/components/common/EntityCell";
import { FilterBar, type FilterField } from "@/components/common/FilterBar";
import { QuotaUnit } from "@/components/common/MoneyText";
import { PageHead } from "@/components/common/PageHead";
import { PageTabs } from "@/components/common/PageTabs";
import { StatCard, type StatCardStatus } from "@/components/common/StatCard";
import { ModalityTag, StatusTag, usageStatusKey } from "@/components/common/StatusTag";
import { TaskProgress } from "@/components/common/TaskProgress";
import { usePageTabs, type PageTab } from "@/lib/pageTabs";
import {
  exportPortalUsage,
  useCancelPortalTaskMutation,
  useKeysQuery,
  usePortalOverviewQuery,
  usePortalTaskQuery,
  usePortalTasksQuery,
  usePortalUsageDetailQuery,
  usePortalUsageQuery,
  type PortalTasksParams,
  type PortalUsageParams,
} from "@/lib/api";
import { ApiError, DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import { MODALITY_LABELS, formatDateTime, formatNum } from "@/lib/format";
import { chargeSub, monthSub, todaySub } from "@/lib/homeStats";
import { moneyOf } from "@/lib/money";
import { isActiveTask } from "@/lib/task";
import { actionsCol, usageColumns } from "@/lib/table";
import type { TaskItem, UsageItem } from "@/types";

/* ----------------------------- 调用记录（S5） ----------------------------- */

const EMPTY_FILTERS: Record<string, string> = {
  fKey: "",
  fModality: "",
  fStatus: "",
  fTime: "",
  fModel: "",
  fStart: "",
  fEnd: "",
};

const MODALITY_ITEMS: [string, string][] = Object.entries(MODALITY_LABELS);

/** 时间预设（原型 usage.html:43）：选中后回填开始/结束日期 */
const TIME_ITEMS: [string, string][] = [
  ["today", "今日"],
  ["7d", "近 7 天"],
  ["30d", "近 30 天"],
];

/** 状态筛选取值对齐后端（settled/pending/released） */
const STATUS_ITEMS: [string, string][] = [
  ["settled", "已成功"],
  ["pending", "进行中"],
  ["released", "失败"],
];

/** 日期控件 → RFC3339 本地日界（开始=当日 00:00:00，结束=当日 23:59:59.999） */
function dayStart(d: string): string | undefined {
  return d ? new Date(`${d}T00:00:00`).toISOString() : undefined;
}
function dayEnd(d: string): string | undefined {
  return d ? new Date(`${d}T23:59:59.999`).toISOString() : undefined;
}
function localDate(d: Date): string {
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${d.getFullYear()}-${m}-${day}`;
}


/** 列定义（密钥名列由 keyName 解析） */
function buildColumns(keyName: (id: number) => string): ColumnDef<UsageItem, unknown>[] {
  return usageColumns<UsageItem>({
    keyName,
    withLatency: true,
    statusMap: usageStatusKey,
  });
}

/**
 * 调用记录视图（S5 / S17）
 * 服务端分页 + 筛选（密钥 / 类型 / 状态 / 时间 / 关键词 / 日期区间）；费用只读换算双币展示。
 * S17 补齐：行点击明细抽屉、按筛选导出 CSV（限本人）。
 * 类型分布卡不在这里 —— 它是概览信息，唯一位置是首页（/home 的 modality-dist 卡），
 * 数据仍来自同一个 `/portal/usage` 响应，本页只消费 items/total。
 */
function UsageRecordsView() {
  const [page, setPage] = useState(1);
  const [filters, setFilters] = useState<Record<string, string>>(EMPTY_FILTERS);
  // 详情走独立接口：列表行是裁剪投影，报文原文只在管理台可见，门户拿分类文案。
  const [detailID, setDetailID] = useState<number | null>(null);

  const keysQuery = useKeysQuery();
  const overviewQuery = usePortalOverviewQuery(7);
  const todayStat = overviewQuery.data?.today;
  const monthStat = overviewQuery.data?.month;
  // 与首页同一套判定：error 优先于 loading（有 placeholderData 时两者可能同时为真）。
  const overviewStatus: StatCardStatus = overviewQuery.isError
    ? "error"
    : overviewQuery.isPending
      ? "loading"
      : "ready";
  /** 区间颠倒时不提交结束日期，改为在下方显式提示（避免静默丢弃条件） */
  const rangeReversed = Boolean(
    filters.fStart && filters.fEnd && filters.fStart > filters.fEnd,
  );
  const params: PortalUsageParams = {
    page,
    page_size: DEFAULT_PAGE_SIZE,
    model: filters.fModel.trim() || undefined,
    modality: filters.fModality || undefined,
    status: filters.fStatus || undefined,
    key_id: filters.fKey ? Number(filters.fKey) : undefined,
    start: dayStart(filters.fStart),
    end: rangeReversed ? undefined : dayEnd(filters.fEnd),
  };
  const query = usePortalUsageQuery(params);
  const detailQuery = usePortalUsageDetailQuery(detailID);
  const items = query.data?.items ?? [];
  const total = query.data?.total ?? 0;

  const nameOf = (id: number) => keysQuery.data?.items.find((k) => k.id === id)?.name ?? `#${id}`;

  const keyItems: [string, string][] = (keysQuery.data?.items ?? []).map((k) => [
    String(k.id),
    `${k.name}（${k.key_prefix}）`,
  ]);
  const fields: FilterField[] = [
    { t: "select", id: "fKey", label: "密钥", allLabel: "全部密钥", items: keyItems },
    { t: "select", id: "fModality", label: "类型", allLabel: "全部类型", items: MODALITY_ITEMS },
    { t: "select", id: "fStatus", label: "状态", allLabel: "全部状态", items: STATUS_ITEMS },
    { t: "select", id: "fTime", label: "时间", allLabel: "全部时间", items: TIME_ITEMS },
    // 起止日期合并为单一「起 – 止」控件（range:true 由 FilterBar 归并），避免各占一行
    { t: "date", id: "fStart", label: "开始日期", range: true },
    { t: "date", id: "fEnd", label: "结束日期", range: true },
    { t: "search", id: "fModel", label: "关键词", placeholder: "模型名 / 任务 ID" },
    { t: "btn", id: "export", label: "导出 CSV" },
    { t: "btn", id: "reset", label: "重置筛选" },
  ];

  const columns = useMemo(() => {
    return buildColumns(nameOf);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [keysQuery.data]);

  const onFilterChange = (id: string, value: string) => {
    setFilters((f) => {
      if (id === "fTime") {
        const next: Record<string, string> = { ...f, fTime: value };
        if (!value) return next;
        const span = value === "today" ? 0 : value === "7d" ? 6 : 29;
        const end = new Date();
        const start = new Date(end);
        start.setDate(end.getDate() - span);
        next.fStart = localDate(start);
        next.fEnd = localDate(end);
        return next;
      }
      if (id === "fStart" || id === "fEnd") return { ...f, [id]: value, fTime: "" };
      return { ...f, [id]: value };
    });
    setPage(1);
  };
  const onFilterReset = () => {
    setFilters(EMPTY_FILTERS);
    setPage(1);
  };

  const onExport = async () => {
    try {
      await exportPortalUsage(params);
      toast.success("已开始下载 CSV");
    } catch (error) {
      toast.error(error instanceof ApiError ? error.message : "导出失败，请稍后重试");
    }
  };

  return (
    <>
      <FilterBar
        fields={fields}
        value={filters}
        onChange={onFilterChange}
        onReset={onFilterReset}
        onAction={(id) => {
          if (id === "export") void onExport();
          if (id === "reset") onFilterReset();
        }}
      />
      {rangeReversed ? (
        <div className="hint warn">开始日期不得晚于结束日期，本次仅按开始日期筛选。</div>
      ) : null}

      <div className="grid grid-3 stat-strip">
        <StatCard
          icon="rows"
          label="今日请求"
          status={overviewStatus}
          onRetry={() => void overviewQuery.refetch()}
          value={formatNum(todayStat?.requests ?? 0)}
          sub={todaySub(todayStat)}
        />
        <StatCard
          icon="wallet"
          label="今日费用"
          status={overviewStatus}
          onRetry={() => void overviewQuery.refetch()}
          value={moneyOf(todayStat?.charge ?? 0)}
          sub={chargeSub(todayStat, overviewQuery.data?.generated_at)}
        />
        <StatCard
          icon="doc"
          label="本月费用"
          status={overviewStatus}
          onRetry={() => void overviewQuery.refetch()}
          value={moneyOf(monthStat?.charge ?? 0)}
          sub={monthSub(monthStat, overviewQuery.data?.month_recharge)}
        />
      </div>

      <div className="card">
        <DataTable
          columns={columns}
          data={items}
          total={total}
          page={page}
          pageSize={DEFAULT_PAGE_SIZE}
          onPageChange={setPage}
          onRowClick={(row) => setDetailID(row.id)}
          empty={
            query.isError ? (
              <EmptyState
                icon="⚠"
                title="调用记录加载失败"
                desc={query.error instanceof ApiError ? query.error.message : "请稍后重试"}
                action={
                  <button type="button" className="btn btn-primary" onClick={() => void query.refetch()}>
                    重试
                  </button>
                }
              />
            ) : (
              <EmptyState icon="rows" title="暂无调用记录" desc="发起模型调用后，明细会实时出现在这里。" />
            )
          }
        />
      </div>

      <CallDetailDrawer
        open={detailID !== null}
        onClose={() => setDetailID(null)}
        detail={detailQuery.data ?? null}
        loading={detailQuery.isFetching}
        error={
          detailQuery.isError
            ? detailQuery.error instanceof ApiError
              ? detailQuery.error.message
              : "详情加载失败"
            : undefined
        }
        onRetry={() => void detailQuery.refetch()}
        portal
      />
    </>
  );
}

/* ----------------------------- 异步任务（S7） ----------------------------- */

const TASKS_EMPTY_FILTERS: Record<string, string> = { fStatus: "", fModel: "" };

/** 状态筛选取值对齐后端冻结枚举 */
const TASK_STATUS_ITEMS: [string, string][] = [
  ["queued", "排队中"],
  ["running", "运行中"],
  ["succeeded", "已成功"],
  ["failed", "失败"],
  ["expired", "已过期"],
  ["canceled", "已取消"],
];

const TASK_FILTER_FIELDS: FilterField[] = [
  { t: "select", id: "fStatus", label: "状态", allLabel: "全部状态", items: TASK_STATUS_ITEMS },
  { t: "search", id: "fModel", label: "模型", placeholder: "模型名关键词" },
];

/** 费用：进行中显示冻结金额，终态显示实际结算金额（失败/超时/取消为 0 = 已全额退回） */
function costCell(t: TaskItem) {
  const active = isActiveTask(t.status);
  return (
    <div>
      <QuotaUnit micro={active ? t.frozen : t.settled} />
      <div className="muted small">{active ? "冻结中" : "已结算"}</div>
    </div>
  );
}

/** 异步任务视图（S7）：筛选 + 列表 + 详情 / 取消 */
function PortalTasksView() {
  const [page, setPage] = useState(1);
  const [filters, setFilters] = useState<Record<string, string>>(TASKS_EMPTY_FILTERS);
  // 列表不下发产物清单，点开时才按任务详情取；selected 保证详情未到时先有摘要。
  const [selected, setSelected] = useState<TaskItem | null>(null);
  // 产物抽屉：列表不下发产物清单，点开时才按任务详情取（音乐一次出 2 首，必须逐条列出）
  const [detailId, setDetailId] = useState<string | null>(null);
  const detailQ = usePortalTaskQuery(detailId);

  const params: PortalTasksParams = {
    page,
    page_size: DEFAULT_PAGE_SIZE,
    status: filters.fStatus || undefined,
    model: filters.fModel.trim() || undefined,
  };
  const query = usePortalTasksQuery(params);
  const items = query.data?.items ?? [];
  const total = query.data?.total ?? 0;

  const cancelMut = useCancelPortalTaskMutation();

  const cancelTask = async (t: TaskItem) => {
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
        closeDetail();
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

  /**
   * 打开详情：列表不下发产物清单，点开时按任务详情取。
   *
   * 以前只有「查看结果」（仅 succeeded），失败/进行中的任务在门户没有任何入口看原因；
   * 统一抽屉后所有状态都能打开 —— 进度、失败分类文案、产物都在同一个面板里。
   */
  const openDetail = (t: TaskItem) => {
    setDetailId(t.id);
    setSelected(t);
  };

  const closeDetail = () => {
    setDetailId(null);
    setSelected(null);
  };

  const columns: ColumnDef<TaskItem, unknown>[] = [
    {
      id: "created_at",
      accessorKey: "created_at",
      enableSorting: false,
      meta: { label: "时间" },
      cell: ({ row }) => <span className="mono small">{formatDateTime(row.original.created_at)}</span>,
    },
    {
      id: "model",
      accessorKey: "model",
      enableSorting: false,
      meta: { label: "模型" },
      cell: ({ row }) => <EntityCell iconUrl={row.original.model_icon_url} name={row.original.model} mono />,
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
    actionsCol<TaskItem>(
      (t) => [
        { icon: "eye", label: "详情", onClick: () => openDetail(t) },
        {
          icon: "close",
          label: "取消",
          tone: "danger",
          hidden: t.status !== "queued",
          busy: cancelMut.isPending,
          onClick: () => void cancelTask(t),
        },
      ],
      "操作",
      "actions",
    ),
  ];

  return (
    <>
      <FilterBar
        fields={TASK_FILTER_FIELDS}
        value={filters}
        onChange={(id, value) => {
          setFilters((f) => ({ ...f, [id]: value }));
          setPage(1);
        }}
        onReset={() => {
          setFilters(TASKS_EMPTY_FILTERS);
          setPage(1);
        }}
      />

      <div className="hint">失败、超时或已取消的任务会自动全额退回预扣额度。</div>

      <div className="card">
        <DataTable
          columns={columns}
          data={items}
          total={total}
          page={page}
          pageSize={DEFAULT_PAGE_SIZE}
          onPageChange={setPage}
          empty={
            query.isError ? (
              <EmptyState
                icon="⚠"
                title="异步任务加载失败"
                desc={query.error instanceof ApiError ? query.error.message : "请稍后重试"}
                action={
                  <button type="button" className="btn btn-primary" onClick={() => void query.refetch()}>
                    重试
                  </button>
                }
              />
            ) : (
              <EmptyState
                icon="rows"
                title="暂无异步任务"
                desc="提交视频等长任务后，可在此跟踪进度、取消或查看产物。"
              />
            )
          }
        />
      </div>

      {/* 与「调用记录」同一个详情抽屉：任务信息、产物预览、扣费与空态都由它统一给出。 */}
      <CallDetailDrawer
        open={detailId !== null}
        onClose={closeDetail}
        task={detailQ.data ?? selected}
        loading={detailQ.isPending}
        error={
          detailQ.isError
            ? detailQ.error instanceof ApiError
              ? detailQ.error.message
              : "任务详情加载失败"
            : undefined
        }
        onRetry={() => void detailQ.refetch()}
        portal
        actions={
          (detailQ.data ?? selected)?.status === "queued" ? (
            <button
              type="button"
              className="btn btn-danger"
              disabled={cancelMut.isPending}
              onClick={() => {
                const t = detailQ.data ?? selected;
                if (t) void cancelTask(t);
              }}
            >
              取消任务
            </button>
          ) : undefined
        }
      />
    </>
  );
}

/**
 * 调用记录（S5）与异步任务（S7）合并页
 * 「异步任务」为 S7 新增视图；顶部 Tabs 切换，默认展示调用记录（保持既有行为）。
 */
type UsageTab = "records" | "tasks";

const USAGE_TABS: readonly PageTab<UsageTab>[] = [
  { key: "records", label: "调用记录" },
  { key: "tasks", label: "异步任务" },
];

export function UsagePage() {
  // ?tab=records|tasks：两个并列视图进 URL（可深链/可刷新保持）。
  // 注意 .stat-strip / .pbar-fields 必须留在各自 View 内，提到页签外会让「异步任务」也显示调用统计。
  const { tab, setTab } = usePageTabs(USAGE_TABS, "records");
  return (
    <>
      <PageHead title="调用记录" desc="统计范围为你全部 API KEY" />

      <PageTabs
        tabs={USAGE_TABS}
        value={tab}
        onChange={setTab}
        idPrefix="portal-usage"
        label="调用视图"
      >
        {(t) => (t === "records" ? <UsageRecordsView /> : <PortalTasksView />)}
      </PageTabs>
    </>
  );
}

