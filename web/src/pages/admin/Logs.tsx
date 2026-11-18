import { useMemo, useState } from "react";
import { CallDetailDrawer } from "@/components/common/CallDetailDrawer";
import { Card } from "@/components/common/Card";
import { RankBars } from "@/components/common/Charts";
import { ErrorState } from "@/components/common/QueryStates";
import { DataTable } from "@/components/common/DataTable";
import { EmptyState } from "@/components/common/EmptyState";
import { FilterBar, type FilterField } from "@/components/common/FilterBar";
import { PageHead } from "@/components/common/PageHead";
import { Tabs } from "@/components/common/Tabs";
import { toastErr, toastOk } from "@/components/common/Toaster";
import { usePageTabs, type PageTab } from "@/lib/pageTabs";
import {
  exportAdminLogs,
  useAdminLogAnalyticsQuery,
  useAdminLogDetailQuery,
  useAdminLogsQuery,
  type AdminLogsParams,
} from "@/lib/api";
import { ApiError, DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import { logsColumns } from "@/lib/table";
import type { AdminLogItem } from "@/types";

type LogScope = "hot" | "archive";

const SCOPE_TABS: readonly PageTab<LogScope>[] = [
  { key: "hot", label: "明细" },
  { key: "archive", label: "归档" },
];

/** 事件类型筛选项（含预检拒绝的四种细分：责任方与「打出去失败」完全不同）。 */
const KIND_ITEMS: [string, string][] = [
  ["consume", "消耗"],
  ["client_abort", "客户端断开"],
  ["error", "失败"],
  ["upstream_error", "上游错误"],
  ["pending", "进行中"],
  ["rate_limited", "预检限流"],
  ["auth_error", "预检鉴权失败"],
  ["insufficient_balance", "余额不足"],
  ["invalid_request", "预检参数错误"],
];

const CALL_KIND_ITEMS: [string, string][] = [
  ["sync", "同步调用"],
  ["async", "异步任务"],
  ["precheck", "预检拒绝"],
];

const FILTER_FIELDS: FilterField[] = [
  { id: "user_id", t: "search", label: "用户ID", placeholder: "用户 ID" },
  { id: "model", t: "search", label: "模型", placeholder: "模型名称" },
  // 排障时手上最先有的就是这两个 id：放在筛选条上比"翻页找"快一个数量级。
  { id: "request_id", t: "search", label: "请求ID", placeholder: "request_id" },
  { id: "upstream_request_id", t: "search", label: "上游单号", placeholder: "上游 request id" },
  // 密钥池面板给出的是指纹，这里要的是 keyID：多 key 渠道出 401 时，
  // 「是不是就这一把在挂」只能靠它筛出来。
  { id: "channel_key_id", t: "search", label: "密钥ID", placeholder: "密钥池面板可复制" },
  { id: "kind", t: "select", label: "事件类型", allLabel: "全部类型", items: KIND_ITEMS },
  { id: "call_kind", t: "select", label: "调用来源", allLabel: "全部来源", items: CALL_KIND_ITEMS },
  {
    id: "status",
    t: "select",
    label: "状态",
    allLabel: "全部状态",
    items: [
      ["ok", "成功"],
      ["429", "限流 429"],
      ["err", "客户端错误 4xx"],
      ["5xx", "上游错误 5xx"],
    ],
  },
  {
    id: "range",
    t: "select",
    label: "时间",
    allLabel: "自定义/不限",
    def: "24h",
    items: [
      ["1h", "近 1 小时"],
      ["3h", "近 3 小时"],
      ["24h", "近 24 小时"],
      ["7d", "近 7 天"],
      ["30d", "近 30 天"],
      ["all", "全部时间"],
    ],
  },
  { id: "export", t: "btn", label: "导出 CSV" },
];

/**
 * 调用日志（S11 §4.9.2 / C1）：明细/归档 + 多条件筛选 + 导出 CSV + 类型分布/错误排行 + 详情抽屉。
 *
 * 详情走独立接口（/admin/logs/:id）而不是复用列表行：列表是裁剪投影，
 * 而详情需要原始报文（≤256KiB）与上游错误原文 —— 把它塞进列表会让每页多传几百 KB。
 */
export function AdminLogsPage() {
  const { tab: scope, setTab: setScope } = usePageTabs(SCOPE_TABS, "hot");
  const [page, setPage] = useState(1);
  const [values, setValues] = useState<Record<string, string>>({ range: "24h" });
  const [detailID, setDetailID] = useState<number | null>(null);

  const params: AdminLogsParams = useMemo(
    () => ({
      scope,
      page,
      page_size: DEFAULT_PAGE_SIZE,
      user_id: values.user_id ? Number(values.user_id) : undefined,
      model: values.model || undefined,
      kind: values.kind || undefined,
      status: values.status || undefined,
      call_kind: values.call_kind || undefined,
      request_id: values.request_id?.trim() || undefined,
      upstream_request_id: values.upstream_request_id?.trim() || undefined,
      channel_key_id: values.channel_key_id?.trim() || undefined,
      range: values.range || undefined,
    }),
    [scope, page, values],
  );

  const logs = useAdminLogsQuery(params);
  const analytics = useAdminLogAnalyticsQuery(params);
  const detail = useAdminLogDetailQuery(detailID, scope);

  const onFilter = (id: string, value: string) => {
    setPage(1);
    setValues((prev) => ({ ...prev, [id]: value }));
  };

  const onExport = async () => {
    try {
      await exportAdminLogs(params);
      toastOk("已开始下载 CSV");
    } catch (err) {
      toastErr(err instanceof ApiError ? err.message : "导出失败，请稍后重试");
    }
  };

  const columns = useMemo(() => logsColumns<AdminLogItem>({ onDetail: (r) => setDetailID(r.id) }), []);

  return (
    <>
      <PageHead title="调用日志" desc="全部调用记录查询与分析" />

      <Tabs
        items={SCOPE_TABS}
        value={scope}
        onChange={(k) => {
          setScope(k as LogScope);
          setPage(1); // 切范围必须回第一页，否则会请求越界页码拿到空列表
        }}
        idPrefix="admin-logs"
        label="日志范围"
      />

      <FilterBar
        fields={FILTER_FIELDS}
        value={values}
        onChange={onFilter}
        onReset={() => {
          setValues({ range: "24h" });
          setPage(1);
        }}
        onAction={(id) => {
          if (id === "export") void onExport();
        }}
      />

      <div className="count-line">
        共 <b className="mono">{logs.data?.total ?? 0}</b> 条
      </div>

      <DataTable
        columns={columns}
        data={logs.data?.items ?? []}
        loading={logs.isPending}
        error={logs.isError ? <ErrorState error={logs.error} onRetry={() => void logs.refetch()} /> : undefined}
        total={logs.data?.total ?? 0}
        page={page}
        pageSize={DEFAULT_PAGE_SIZE}
        onPageChange={setPage}
        empty={
          logs.isError ? (
            <EmptyState
              icon="⚠"
              title="调用日志加载失败"
              desc={logs.error instanceof ApiError ? logs.error.message : "请稍后重试"}
              action={
                <button type="button" className="btn btn-primary" onClick={() => void logs.refetch()}>
                  重试
                </button>
              }
            />
          ) : logs.isLoading ? (
            <EmptyState title="加载中…" />
          ) : (
            <EmptyState title="当前条件下暂无调用记录" />
          )
        }
      />

      <div className="grid grid-2">
        <Card title="调用类型分布" >
          {analytics.data?.kinds?.length ? (
            <RankBars data={analytics.data.kinds} unit="次" height={200} />
          ) : (
            <EmptyState title="暂无数据" />
          )}
        </Card>
        <Card title="错误排行" >
          {analytics.data?.errors?.length ? (
            <div className="kv-grid">
              {analytics.data.errors.map((e) => (
                <div className="kv" key={e.label}>
                  <span className="k mono" title={e.label}>
                    {e.label}
                  </span>
                  <span className="v mono">
                    {e.value.toLocaleString("en-US")}
                    <span className="muted"> {(e.share * 100).toFixed(1)}%</span>
                  </span>
                </div>
              ))}
            </div>
          ) : (
            <EmptyState title="暂无失败记录" />
          )}
        </Card>
      </div>

      <CallDetailDrawer
        open={detailID !== null}
        onClose={() => setDetailID(null)}
        detail={detail.data ?? null}
        loading={detail.isFetching}
        error={
          detail.isError
            ? detail.error instanceof ApiError
              ? detail.error.message
              : "详情加载失败"
            : undefined
        }
        onRetry={() => void detail.refetch()}
      />
    </>
  );
}
