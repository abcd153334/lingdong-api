import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

/** 空态语义（loading/error/denied 与 empty 共用骨架，差异仅在默认图标与 ARIA） */
export type EmptyVariant = "loading" | "empty" | "error" | "denied";

const DEFAULT_ICON: Record<EmptyVariant, ReactNode> = {
  loading: "…",
  empty: "◇",
  error: "⚠",
  denied: "⛔",
};

export interface EmptyStateProps {
  variant?: EmptyVariant;
  /** 图标字符（如 ◇）或节点；显式传 null 表示不渲染图标位 */
  icon?: ReactNode;
  title: string;
  desc?: ReactNode;
  /** 底部动作（通常为 Button） */
  action?: ReactNode;
  /** 自定义内边距，如 "24px" */
  pad?: string;
  className?: string;
}

/** 空态（对齐原型 emptyState；variant 仅决定默认图标与语义，不改变骨架） */
export function EmptyState({
  variant = "empty",
  icon,
  title,
  desc,
  action,
  pad,
  className,
}: EmptyStateProps) {
  const glyph = icon === undefined ? DEFAULT_ICON[variant] : icon;
  return (
    <div
      className={cn("empty", className)}
      style={pad ? { padding: pad } : undefined}
      role={variant === "error" ? "alert" : "status"}
      aria-busy={variant === "loading" || undefined}
    >
      {glyph ? <div className="icon">{glyph}</div> : null}
      <div className="title">{title}</div>
      {desc ? <div className="small">{desc}</div> : null}
      {action}
    </div>
  );
}
