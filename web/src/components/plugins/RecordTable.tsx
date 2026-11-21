import { useMemo, type ReactNode } from "react";
import type { ColumnDef, SortingState } from "@tanstack/react-table";

import { DataTable } from "@/components/common/DataTable";
import { actionsCol, renderCol } from "@/lib/table";
import { cellText, fieldOf, isLinkColumn } from "@/lib/pluginTypes";
import type { AdminAppPluginRecord, PluginEntity } from "@/types";

/** 选择列的 id（字段名不能以 `_` 开头，所以不会与数据列冲突）。 */
const SEL_COL = "__sel";

export interface RecordSelection {
  selected: ReadonlySet<string>;
  toggle: (recordKey: string) => void;
  toggleAll: () => void;
  allSelected: boolean;
  someSelected: boolean;
}

/**
 * 插件记录列表。
 *
 * 列从 `entity.list.columns` 来（后端编译好的 {key,label,type}）：
 * 前端不再查字段表、不再判断"这列是什么类型"，只按 type 选渲染方式。
 * 列定义必须走 `lib/table.tsx` 的 `col()/renderCol()` —— 手写 ColumnDef 会丢掉
 * 移动端的 `data-label`，窄屏卡片化时会变成没有标签的裸值。
 *
 * 排序白名单来自 `entity.list.sort`：只有声明过的列才给表头加箭头，
 * 否则"能点但服务端 400"会让排序看起来是坏的。
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
  onView,
  canWrite,
  sort,
  onSortChange,
  selection,
}: {
  entity: PluginEntity;
  items: AdminAppPluginRecord[];
  total: number;
  page: number;
  pageSize: number;
  loading?: boolean;
  error?: ReactNode;
  onPageChange: (page: number) => void;
  onEdit: (row: AdminAppPluginRecord) => void;
  onDelete: (row: AdminAppPluginRecord) => void;
  /** 只读详情（行点击与「查看」共用） */
  onView?: (row: AdminAppPluginRecord) => void;
  canWrite: boolean;
  sort?: SortingState;
  onSortChange?: (sort: SortingState) => void;
  /** 传了才渲染选择列（只有声明了 batch_delete 才需要） */
  selection?: RecordSelection;
}) {
  const columns = useMemo(() => {
    const sortable = new Set(entity.list.sort.map((s) => s.key));
    const cols: ColumnDef<AdminAppPluginRecord, unknown>[] = [];

    if (selection) {
      // 手写 ColumnDef：表头必须是 checkbox，`col()` 只支持字符串表头。
      // meta.label 仍要给 —— 卡片态（≤767）的 td 靠它当标签。
      cols.push({
        id: SEL_COL,
        header: () => (
          // 卡片态表头隐藏（.tbl thead{display:none}），手机上只能逐条勾选。
          <input
            type="checkbox"
            aria-label="全选本页"
            checked={selection.allSelected}
            ref={(el) => {
              if (el) el.indeterminate = selection.someSelected && !selection.allSelected;
            }}
            onChange={selection.toggleAll}
          />
        ),
        meta: { label: "选择" },
        enableSorting: false,
        cell: ({ row }) => (
          <input
            type="checkbox"
            aria-label={`选择 ${row.original.record_key}`}
            checked={selection.selected.has(row.original.record_key)}
            onChange={() => selection.toggle(row.original.record_key)}
          />
        ),
      });
    }

    for (const c of entity.list.columns) {
      const f = fieldOf(entity, c.key);
      // renderCol 把 enableSorting 写死为 false（全站默认不可排）；这里按白名单覆盖。
      const base =
        c.type === "link"
          ? renderCol<AdminAppPluginRecord>(
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
            )
          : renderCol<AdminAppPluginRecord>(
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
      cols.push({ ...base, enableSorting: sortable.has(c.key) });
    }

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
    if (onView || canWrite) {
      cols.push(
        actionsCol<AdminAppPluginRecord>((row) => [
          // 「查看」给键盘与读屏用：行点击只有鼠标可达。
          ...(onView ? [{ icon: "eye" as const, label: "查看", onClick: () => onView(row) }] : []),
          ...(canWrite
            ? [
                { icon: "edit" as const, label: "编辑", onClick: () => onEdit(row) },
                {
                  icon: "trash" as const,
                  label: "删除",
                  tone: "danger" as const,
                  onClick: () => onDelete(row),
                },
              ]
            : []),
        ]),
      );
    }
    return cols;
  }, [entity, canWrite, onEdit, onDelete, onView, selection]);

  return (
    <DataTable
      columns={columns}
      data={items}
      total={total}
      page={page}
      pageSize={pageSize}
      onPageChange={onPageChange}
      sort={sort}
      onSortChange={onSortChange}
      onRowClick={onView}
      loading={loading}
      error={error}
      label={`${entity.name}列表`}
    />
  );
}
