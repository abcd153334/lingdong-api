import { Icon } from "./Icon";
import { DEFAULT_PAGE_SIZE } from "@/lib/apiClient";

export interface PaginationProps {
  total: number;
  page: number;
  pageSize?: number;
  onPageChange: (page: number) => void;
}

/** 页码窗口（首末页 + 当前页邻域，其余以省略号折叠） */
function pageWindow(page: number, pageCount: number): (number | "…")[] {
  if (pageCount <= 7) return Array.from({ length: pageCount }, (_, i) => i + 1);
  const out: (number | "…")[] = [1];
  const start = Math.max(2, page - 1);
  const end = Math.min(pageCount - 1, page + 1);
  if (start > 2) out.push("…");
  for (let i = start; i <= end; i++) out.push(i);
  if (end < pageCount - 1) out.push("…");
  out.push(pageCount);
  return out;
}

/** 分页条（T21：共 N 条 · 每页 M 条 + 页码） */
export function Pagination({ total, page, pageSize = DEFAULT_PAGE_SIZE, onPageChange }: PaginationProps) {
  const pageCount = Math.max(1, Math.ceil(total / pageSize));
  // 越界夹取：删掉最后一页的最后一条后，page 会停在一个已经不存在的页码上。
  // 不夹取时页码窗口算出来的区间无高亮、"上一页"会把用户带到另一个无效页，
  // 看起来像分页坏了。显示层夹取，真正的状态纠正由列表页负责（它们知道何时该归 1）。
  const current = Math.min(Math.max(1, page), pageCount);
  const pages = pageWindow(current, pageCount);
  return (
    <div className="pager">
      <span>
        共 {total} 条 · 每页 {pageSize} 条
      </span>
      {pageCount > 1 ? (
        <>
          <button
            type="button"
            aria-label="上一页"
            disabled={current <= 1}
            onClick={() => onPageChange(current - 1)}
          >
            <Icon name="chevronLeft" size={14} />
          </button>
          {pages.map((p, i) =>
            p === "…" ? (
              <button key={`gap-${i}`} type="button" disabled>
                …
              </button>
            ) : (
              <button
                key={p}
                type="button"
                className={p === current ? "on" : undefined}
                onClick={() => onPageChange(p)}
              >
                {p}
              </button>
            ),
          )}
          <button
            type="button"
            aria-label="下一页"
            disabled={current >= pageCount}
            onClick={() => onPageChange(current + 1)}
          >
            <Icon name="chevronRight" size={14} />
          </button>
        </>
      ) : null}
    </div>
  );
}
