import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { DataTable } from "@/components/common/DataTable";
import { FilterBar, type FilterField } from "@/components/common/FilterBar";
import { PageHead } from "@/components/common/PageHead";
import { Tag } from "@/components/common/StatusTag";
import { toastErr, toastOk } from "@/components/common/Toaster";
import {
  exportAdminAuthEvents,
  useAdminAuthEventMetaQuery,
  useAdminAuthEventsQuery,
  type AdminAuthEventsParams,
} from "@/lib/api";
import { ApiError, DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import { formatDateTime } from "@/lib/format";
import type { AuthEventItem } from "@/types";

/**
 * 认证审计（/admin/auth-events）。
 *
 * 与「审计日志」(`/admin/audits`) 刻意分成两页：那边记的是**管理员做了什么**，
 * 这边记的是**账号层面发生了什么**（登录失败、备份码被用、2FA 被关）。
 * 合成一页的话，一次撞库就能把「昨天谁改了配置」埋进几百条登录失败里。
 *
 * 事件的英文 key 与中文名都来自后端（`authevent` 白名单）：前端再维护一份映射，
 * 迟早会出现「下拉里有、库里没有」的幽灵选项，而它不报错。
 */
export function AdminAuthEventsPage() {
  const meta = useAdminAuthEventMetaQuery();
  const [page, setPage] = useState(1);
  const [values, setValues] = useState<Record<string, string>>({ range: "24h" });

  const params: AdminAuthEventsParams = useMemo(
    () => ({
      page,
      page_size: DEFAULT_PAGE_SIZE,
      event: values.event || undefined,
      result: values.result || undefined,
      // uid 必须是数字：后端按 uint64 解析，传 "abc" 直接 400（筛选器不会静默失效）。
      uid: values.uid ? Number(values.uid) : undefined,
      ip: values.ip || undefined,
      account: values.account || undefined,
      range: values.range || undefined,
    }),
    [page, values],
  );

  const events = useAdminAuthEventsQuery(params);

  const fields: FilterField[] = useMemo(
    () => [
      { id: "account", t: "search", label: "账号", placeholder: "账号前缀" },
      { id: "uid", t: "search", label: "用户", placeholder: "用户 ID" },
      {
        id: "event",
        t: "select",
        label: "事件",
        allLabel: "全部事件",
        items: (meta.data?.events ?? []).map(
          (e) => [e.event, e.label] as [string, string],
        ),
      },
      {
        id: "result",
        t: "select",
        label: "结果",
        allLabel: "全部结果",
        items: [
          ["success", "成功"],
          ["fail", "失败"],
        ],
      },
      { id: "ip", t: "search", label: "IP", placeholder: "精确 IP" },
      {
        id: "range",
        t: "select",
        label: "时间",
        allLabel: "不限",
        def: "24h",
        items: [
          ["1h", "近 1 小时"],
          ["24h", "近 24 小时"],
          ["7d", "近 7 天"],
          ["30d", "近 30 天"],
          ["all", "全部时间"],
        ],
      },
      { id: "export", t: "btn", label: "导出 CSV" },
    ],
    [meta.data?.events],
  );

  const onExport = async () => {
    try {
      await exportAdminAuthEvents(params);
      toastOk("已开始下载 CSV");
    } catch (err) {
      toastErr(err instanceof ApiError ? err.message : "导出失败，请稍后重试");
    }
  };

  const columns: ColumnDef<AuthEventItem, unknown>[] = useMemo(
    () => [
      {
        id: "created_at",
        header: "时间",
        meta: { label: "时间" },
        cell: ({ row }) => <span className="mono">{formatDateTime(row.original.created_at)}</span>,
      },
      {
        id: "account",
        header: "账号",
        meta: { label: "账号" },
        cell: ({ row }) =>
          row.original.uid ? (
            <span>
              <span className="mono">#{row.original.uid}</span> {row.original.account || "—"}
            </span>
          ) : (
            // uid=0 = 账号不存在（撞库最典型的形态），原文照显便于归并攻击源。
            <span>{row.original.account || "—"}</span>
          ),
      },
      {
        id: "event",
        header: "事件",
        meta: { label: "事件" },
        cell: ({ row }) => (
          <Tag tone="purple" title={row.original.event}>
            {row.original.label || row.original.event}
          </Tag>
        ),
      },
      {
        id: "result",
        header: "结果",
        meta: { label: "结果" },
        cell: ({ row }) => (
          <Tag tone={row.original.result === "success" ? "success" : "danger"}>
            {row.original.result === "success" ? "成功" : "失败"}
          </Tag>
        ),
      },
      {
        id: "reason",
        header: "原因",
        meta: { label: "原因" },
        cell: ({ row }) => <span className="mono">{row.original.reason || "—"}</span>,
      },
      {
        id: "ip",
        header: "IP",
        meta: { label: "IP" },
        cell: ({ row }) => <span className="mono">{row.original.ip || "—"}</span>,
      },
      {
        id: "ua",
        header: "User-Agent",
        meta: { label: "User-Agent" },
        cell: ({ row }) => (
          <span className="ellipsis" title={row.original.ua || undefined}>
            {row.original.ua || "—"}
          </span>
        ),
      },
      {
        id: "session_id",
        header: "会话",
        meta: { label: "会话" },
        cell: ({ row }) => (
          <span className="mono">{row.original.session_id ? `#${row.original.session_id}` : "—"}</span>
        ),
      },
    ],
    [],
  );

  const total = events.data?.total ?? 0;
  const items = events.data?.items ?? [];

  return (
    <>
      <PageHead title="认证审计" desc="登录 / 注册 / 改密 / 二次验证的完整流水" />

      <FilterBar
        fields={fields}
        value={values}
        onChange={(id, value) => {
          setPage(1);
          setValues((prev) => ({ ...prev, [id]: value }));
        }}
        onReset={() => {
          setValues({ range: "24h" });
          setPage(1);
        }}
        onAction={(id) => {
          if (id === "export") void onExport();
        }}
      />

      <div className="count-line">
        共 <b className="mono">{total}</b> 条认证事件（保留 {meta.data?.retention_days ?? 90} 天）
      </div>

      <DataTable
        columns={columns}
        data={items}
        total={total}
        page={page}
        pageSize={DEFAULT_PAGE_SIZE}
        onPageChange={setPage}
        empty={<span>无匹配记录</span>}
      />
    </>
  );
}
