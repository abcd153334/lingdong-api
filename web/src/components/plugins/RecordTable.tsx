import { useMemo } from "react";

import { DataTable } from "@/components/common/DataTable";
import { actionsCol, renderCol } from "@/lib/table";
import { cellText, fieldOf, isLinkColumn } from "@/lib/pluginTypes";
import type { AdminAppPluginRecord, PluginEntity } from "@/types";

/**
 * 插件记录列表。
 *
 * 列从 `entity.list.columns` 来（后端编译好的 {key,label,type}）：
 * 前端不再查字段表、不再判断"这列是什么类型"，只按 type 选渲染方式。
 * 列定义必须走 `lib/table.tsx` 的 `col()` —— 手写 ColumnDef 会丢掉移动端的
 * `data-label`，窄屏卡片化时会变成没有标签的裸值。
 */
export function RecordTable({
  entity,
  items,
  total,
  page,
  pageSize,
  loading,
  error,
  onPageChange,
  onEdit,
  onDelete,
  canWrite,
}: {
  entity: PluginEntity;
  items: AdminAppPluginRecord[];
  total: number;
  page: number;
  pageSize: number;
  loading?: boolean;
  error?: React.ReactNode;
  onPageChange: (page: number) => void;
  onEdit: (row: AdminAppPluginRecord) => void;
  onDelete: (row: AdminAppPluginRecord) => void;
  canWrite: boolean;
}) {
  const columns = useMemo(() => {
    const cols = entity.list.columns.map((c) => {
      const f = fieldOf(entity, c.key);
      if (c.type === "link") {
        return renderCol<AdminAppPluginRecord>(
          c.key,
          c.label,
          (r) => r.data[c.key],
          (r) => {
            const href = r.data[c.key];
            if (typeof href !== "string" || href === "") return <span className="muted">—</span>;
            return (
              <a href={href} target="_blank" rel="noopener noreferrer">
                {href}
              </a>
            );
          },
          { wrap: true },
        );
      }
      return renderCol<AdminAppPluginRecord>(
        c.key,
        c.label,
        (r) => r.data[c.key],
        (r) => {
          const text = cellText(c.type, r.data[c.key]);
          if (text === "—") return <span className="muted">—</span>;
          // text 列允许换行：备注/详情挤成一行等于逼人去点开看。
          return f?.type === "text" ? <span className="pre-wrap">{text}</span> : text;
        },
        { wrap: f?.type === "text", minWidth: isLinkColumn(c.type) ? 180 : undefined },
      );
    });
    // 编号列放最后且允许在窄屏隐藏：它是排障用的，不是日常阅读的一列。
    cols.push(
      renderCol<AdminAppPluginRecord>(
        "record_key",
        "编号",
        (r) => r.record_key,
        (r) => <span className="mono">{r.record_key}</span>,
        { priority: 3 },
      ),
    );
    if (canWrite) {
      cols.push(
        actionsCol<AdminAppPluginRecord>((row) => [
          { icon: "edit", label: "编辑", onClick: () => onEdit(row) },
          { icon: "trash", label: "删除", tone: "danger", onClick: () => onDelete(row) },
        ]),
      );
    }
    return cols;
  }, [entity, canWrite, onEdit, onDelete]);

  return (
    <DataTable
      columns={columns}
      data={items}
      total={total}
      page={page}
      pageSize={pageSize}
      onPageChange={onPageChange}
      loading={loading}
      error={error}
      label={`${entity.name}列表`}
    />
  );
}
