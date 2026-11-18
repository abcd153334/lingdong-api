import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

export interface CardProps {
  /** 卡片标题（渲染为 `<h3>`） */
  title?: ReactNode;
  /** 标题右侧说明（`<span className="sub">`） */
  sub?: ReactNode;
  /** 标题行右侧动作区（`.right` 包裹） */
  actions?: ReactNode;
  /** 透传 `data-testid`（e2e 契约锚点） */
  testId?: string;
  className?: string;
  children: ReactNode;
}

/**
 * 卡片共享组件（对齐原型 `card > h3 + span.sub` 骨架）
 *
 * 替代页面层 20+ 处手写卡片标题骨架；无 h3 的裸 `.card` 仍直接用语义类。
 *
 * 没有 `flush` 开关：`.card` 已不携带外边距，块间距由页面列（`.a-content` /
 * `.p-main` 的 flex gap）统一给出 —— 「栅格里的卡要不要去边距」这个问题不再存在。
 */
export function Card({ title, sub, actions, testId, className, children }: CardProps) {
  return (
    <div className={cn("card", className)} data-testid={testId}>
      {title ? (
        <h3>
          {title}
          {sub ? <span className="sub">{sub}</span> : null}
          {actions ? <span className="right">{actions}</span> : null}
        </h3>
      ) : null}
      {children}
    </div>
  );
}
