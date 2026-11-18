import { useId, useState, type ReactNode } from "react";
import { cn } from "@/lib/utils";

export interface CollapseSectionProps {
  /** 分区标题（同时是按钮文案） */
  title: string;
  /** 标题右侧的摘要值（如「2 个 · 1.2 MB」）；空则不留白 */
  summary?: ReactNode;
  /** 默认展开（只有「生成结果」用 true） */
  defaultOpen?: boolean;
  children: ReactNode;
}

/**
 * 调用详情里的折叠分区。
 *
 * 为什么不用原生 `<details>`：
 *  1. 需要在标题右侧挂摘要值（`<summary>` 里塞 flex 布局在各浏览器表现不一致）；
 *  2. 需要 `aria-expanded` 受控，折叠状态要能被测试与读屏软件读到；
 *  3. 需要在 `@media print` 时强制展开（打印出来的记录必须完整）。
 *
 * 为什么折叠时**条件渲染**而不是 `hidden`：
 * `hidden` 的元素仍在 DOM 里，`toContainText` 之类的断言会命中"用户看不到的文本"，
 * 于是测试全绿但界面是空的 —— 这是最难查的一类假绿。
 */
export function CollapseSection({ title, summary, defaultOpen = false, children }: CollapseSectionProps) {
  const [open, setOpen] = useState(defaultOpen);
  const bodyId = useId();
  return (
    <section className="cd-sec">
      <button
        type="button"
        className="cd-sec-head"
        aria-expanded={open}
        aria-controls={bodyId}
        onClick={() => setOpen((v) => !v)}
      >
        <span className={cn("cd-caret", open && "open")} aria-hidden="true">
          ▸
        </span>
        <span>{title}</span>
        {summary ? <span className="sum">{summary}</span> : null}
      </button>
      {open ? (
        <div className="cd-sec-body" id={bodyId}>
          {children}
        </div>
      ) : null}
    </section>
  );
}
