import type { CSSProperties, ReactNode } from "react";
import {
  flexRender,
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type SortingState,
} from "@tanstack/react-table";
import { DEFAULT_PAGE_SIZE } from "@/lib/apiClient";
import { cn } from "@/lib/utils";
import { EmptyState } from "./EmptyState";
import { Pagination } from "./Pagination";

/**
 * 列元数据（全站表格的列契约，唯一出口在 `lib/table.tsx` 的 `col()`）。
 *
 * 这几项挂在列上而不是做成组件 props：它们描述的是「这一列是什么」，不是「表格怎么渲染」。
 * 页面只声明业务语义，落到 DOM 的 data-* / class 由 DataTable 一处负责。
 */
export interface TableColumnMeta {
  /** 表头文案，同时用于移动端卡片态的 `data-label` */
  label?: string;
  /** 允许换行（说明 / 备注这类长文本列）。单元格默认不换行 —— 见 components.css 的 `.tbl td` */
  wrap?: boolean;
  /** 1 = 始终可见；2 = ≤1023px 隐藏；3 = ≤1199px 隐藏（≤767px 卡片态一律恢复） */
  priority?: 1 | 2 | 3;
  /** 操作列吸右（表格比容器宽时动作仍可达） */
  pinRight?: boolean;
  align?: "left" | "right";
  /** 关键列的最小宽度（px）。表格单元格自身的 min-width 会被表格布局算法忽略，
   *  只能由块级子元素承载 —— 见 lib/table.tsx 的 col() */
  minWidth?: number;
}

export interface DataTableProps<T> {
  columns: ColumnDef<T, unknown>[];
  data: T[];
  /** 服务端分页：传 total 即启用（T21） */
  total?: number;
  page?: number;
  pageSize?: number;
  onPageChange?: (page: number) => void;
  /** 服务端排序：传 onSortChange 即启用；否则客户端排序 */
  sort?: SortingState;
  onSortChange?: (sort: SortingState) => void;
  /** 空态内容；缺省用通用空态 */
  empty?: ReactNode;
  /**
   * 加载态：**仅在 data 为空时**生效。
   * 有旧数据（placeholderData）时继续显示表格，避免每次翻页都闪成骨架。
   */
  loading?: boolean;
  /**
   * 失败态：**仅在 data 为空时**生效。
   * 没有它的话，请求失败会落进「暂无数据」，用户以为真的没有记录（2026-10-29 审计 A-04）。
   */
  error?: ReactNode;
  rowClassName?: (row: T) => string | undefined;
  /** 行点击回调（传入即渲染可点击行，用于打开明细抽屉） */
  onRowClick?: (row: T) => void;
  className?: string;
  /** 表格可访问名：一页有多张表时必须传，否则读屏只念「表格」 */
  label?: string;
}

/** 列 metadata 读取（页面手写的列可能没有 meta） */
function metaOf(column: ColumnDef<unknown, unknown>): TableColumnMeta {
  return (column.meta as TableColumnMeta | undefined) ?? {};
}

/** 表头文案：优先列 meta.label，兼容字符串 header（同时用于移动端 data-label） */
function headerLabel(column: ColumnDef<unknown, unknown>): string {
  const { label } = metaOf(column);
  if (label) return label;
  return typeof column.header === "string" ? column.header : "";
}

/** 行内交互元素：点它们不触发行点击（否则「点取消」会同时取消任务并打开详情抽屉） */
const INTERACTIVE = "button,a,input,select,textarea,label,[role='button']";

/**
 * 数据表格（TanStack Table）——全站唯一的表格渲染出口。
 *
 * 三层职责分开，别混：
 *  1. **列宽 / 对齐 / 优先级**由列的 `meta` 声明（`TableColumnMeta`），本组件只把它落成
 *     `data-p` / `.nw` / `.is-pin` / `textAlign`；
 *  2. **响应式是纯 CSS**（`data-p` 在 ≤1199 / ≤1023 隐藏，≤767 卡片态恢复）：没有 JS 断点、
 *     没有 columnVisibility 状态 —— 首帧不闪，也不会与 CSS 断点各说一套；
 *  3. **动作**一律走 `RowActions`，本组件不关心按钮长什么样。
 *
 * ≤767px 由 components.css 自动卡片化，故 td 需带 data-label（由列 meta.label 提供）。
 */
export function DataTable<T>({
  loading,
  error,
  columns,
  data,
  total,
  page = 1,
  pageSize = DEFAULT_PAGE_SIZE,
  onPageChange,
  sort,
  onSortChange,
  empty,
  rowClassName,
  onRowClick,
  className,
  label,
}: DataTableProps<T>) {
  const serverSort = onSortChange !== undefined;
  const table = useReactTable({
    data,
    columns,
    state: serverSort ? { sorting: sort ?? [] } : undefined,
    onSortingChange: serverSort
      ? (updater) => {
          const next = typeof updater === "function" ? updater(sort ?? []) : updater;
          onSortChange?.(next);
        }
      : undefined,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: serverSort ? undefined : getSortedRowModel(),
    manualSorting: serverSort,
  });

  if (!data.length) {
    // 顺序固定：失败 > 加载 > 空。失败不能说成"暂无数据"，加载不能说成"没有"。
    // 外框必须保留：否则状态切换时边框/圆角凭空出现，整块版面会跳一下。
    const state = error ?? loading ?? empty ?? <EmptyState icon="◇" title="暂无数据" />;
    return (
      <div className={className}>
        <div className="table-wrap">
          <div className="t-empty">{state}</div>
        </div>
      </div>
    );
  }

  return (
    <div className={className}>
      <div className="table-wrap">
        <table className={cn("tbl", onRowClick && "tbl--clickable")} aria-label={label}>
          <thead>
            {table.getHeaderGroups().map((hg) => (
              <tr key={hg.id}>
                {hg.headers.map((header) => {
                  const col = header.column.columnDef as ColumnDef<unknown, unknown>;
                  const m = metaOf(col);
                  const canSort = header.column.getCanSort();
                  const sorted = header.column.getIsSorted();
                  return (
                    <th
                      key={header.id}
                      scope="col"
                      // 读屏靠 aria-sort 才知道「按哪列、升还是降」，↑↓ 字符它读不出来
                      aria-sort={
                        canSort
                          ? sorted === "asc"
                            ? "ascending"
                            : sorted === "desc"
                              ? "descending"
                              : "none"
                          : undefined
                      }
                      data-p={m.priority}
                      className={cn(canSort && "cursor-pointer select-none", m.pinRight && "is-pin")}
                      style={m.align === "right" ? { textAlign: "right" } : undefined}
                      onClick={canSort ? header.column.getToggleSortingHandler() : undefined}
                    >
                      {headerLabel(col)}
                      {sorted === "asc" ? " ↑" : sorted === "desc" ? " ↓" : ""}
                    </th>
                  );
                })}
              </tr>
            ))}
          </thead>
          <tbody>
            {table.getRowModel().rows.map((row) => (
              <tr
                key={row.id}
                className={rowClassName?.(row.original)}
                onClick={
                  onRowClick
                    ? (e) => {
                        // 第一道守卫：行内交互元素不触发行点击（RowActions 内还有第二道）
                        if ((e.target as HTMLElement).closest(INTERACTIVE)) return;
                        onRowClick(row.original);
                      }
                    : undefined
                }
              >
                {row.getVisibleCells().map((cell) => {
                  const m = metaOf(cell.column.columnDef as ColumnDef<unknown, unknown>);
                  return (
                    <td
                      key={cell.id}
                      data-label={headerLabel(cell.column.columnDef as ColumnDef<unknown, unknown>)}
                      data-p={m.priority}
                      className={cn(m.wrap && "wrap", m.pinRight && "is-pin")}
                      style={m.align === "right" ? { textAlign: "right" } : undefined}
                    >
                      {flexRender(cell.column.columnDef.cell, cell.getContext())}
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {onPageChange && total !== undefined ? (
        <Pagination total={total} page={page} pageSize={pageSize} onPageChange={onPageChange} />
      ) : null}
    </div>
  );
}
