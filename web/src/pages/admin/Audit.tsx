import { useMemo, useState } from "react";
import { EmptyState } from "@/components/common/EmptyState";
import { FilterBar, type FilterField } from "@/components/common/FilterBar";
import { PageHead } from "@/components/common/PageHead";
import { Pagination } from "@/components/common/Pagination";
import { Tag } from "@/components/common/StatusTag";
import { toastErr, toastOk } from "@/components/common/Toaster";
import { exportAdminAudits, useAdminAuditMetaQuery, useAdminAuditsQuery, type AdminAuditsParams } from "@/lib/api";
import { ApiError, DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import { formatDateTime } from "@/lib/format";

/** 今日零点（原型的「今日」为日历日口径，走后端 from 日期边界而非近 24 小时）。 */
function todayDate(): string {
  const d = new Date();
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

/** 审计日志（S11 §4.9.3 / G18）：动作由库内实际集合数据驱动，避免枚举漂移；详情按原型手风琴展开。 */
export function AdminAuditPage() {
  const meta = useAdminAuditMetaQuery();
  const [page, setPage] = useState(1);
  const [values, setValues] = useState<Record<string, string>>({ range: "24h" });

  const params: AdminAuditsParams = useMemo(
    () => ({
      page,
      page_size: DEFAULT_PAGE_SIZE,
      action: values.action || undefined,
      admin_id: values.admin_id ? Number(values.admin_id) : undefined,
      keyword: values.keyword || undefined,
      range: values.range === "today" ? undefined : values.range || undefined,
      from: values.range === "today" ? todayDate() : undefined,
    }),
    [page, values],
  );

  const audits = useAdminAuditsQuery(params);

  const fields: FilterField[] = useMemo(
    () => [
      { id: "admin_id", t: "search", label: "管理员", placeholder: "管理员 ID" },
      {
        id: "action",
        t: "select",
        label: "动作",
        allLabel: "全部动作",
        items: (meta.data?.actions ?? []).map(
          (a) => [a.action, `${a.action}（${a.value}）`] as [string, string],
        ),
      },
      { id: "keyword", t: "search", label: "关键词", placeholder: "搜索对象 / 详情关键词" },
      {
        id: "range",
        t: "select",
        label: "时间",
        allLabel: "不限",
        def: "24h",
        items: [
          ["today", "今日"],
          ["24h", "近 24 小时"],
          ["7d", "近 7 天"],
          ["30d", "近 30 天"],
          ["all", "全部时间"],
        ],
      },
      { id: "export", t: "btn", label: "导出 CSV" },
    ],
    [meta.data?.actions],
  );

  const onExport = async () => {
    try {
      await exportAdminAudits(params);
      toastOk("已开始下载 CSV");
    } catch (err) {
      toastErr(err instanceof ApiError ? err.message : "导出失败，请稍后重试");
    }
  };

  const total = audits.data?.total ?? 0;
  const items = audits.data?.items ?? [];

  return (
    <>
      <PageHead title="审计日志" desc="管理员操作留痕" />

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
        共 <b className="mono">{total}</b> 条审计记录（保留 {meta.data?.retention_days ?? 180} 天）
      </div>

      {items.length ? (
        <div className="table-wrap">
          {items.map((a) => (
            <div className="au-row" key={a.id}>
              <span className="t">{formatDateTime(a.created_at)}</span>
              <Tag tone="info">{a.actor}</Tag>
              <Tag tone="purple" className="act">
                {a.action}
              </Tag>
              <span className="tgt">
                {[a.target_type, a.target_id].filter(Boolean).join(" #") || "—"}
              </span>
              <details>
                <summary>详情数据</summary>
                <div className="json-view">
                  {a.detail ? JSON.stringify(a.detail, null, 2) : "（无变更内容）"}
                </div>
              </details>
            </div>
          ))}
        </div>
      ) : (
        <EmptyState title="无匹配记录" />
      )}

      {items.length ? (
        <Pagination total={total} page={page} pageSize={DEFAULT_PAGE_SIZE} onPageChange={setPage} />
      ) : null}
    </>
  );
}
