import type { ReactNode } from "react";
import { cn } from "@/lib/utils";
import { Icon, type IconName } from "./Icon";

/**
 * 统计卡状态。
 *
 * - `loading`：值显示 `—`（不是 0）。0 是一个断言「确实没有」，而加载中没有断言资格。
 * - `error`：值显示 `—` + 错误文案 + 重试按钮。以前接口挂了会显示「今日请求 0」，
 *   与「今天真的没调用」完全无法区分 —— 用户会以为系统坏了或自己没在调。
 * - `ready`：正常渲染传入的 value。
 */
export type StatCardStatus = "loading" | "error" | "ready";

export interface StatCardProps {
  icon?: IconName;
  label: string;
  value: ReactNode;
  sub?: ReactNode;
  className?: string;
  /** 缺省 `ready`：现有 30 个调用点不需要任何改动。 */
  status?: StatCardStatus;
  /** error 态的重试回调；不给则只显示文案。 */
  onRetry?: () => void;
}

/** 统计卡（对齐原型 statCard：label/等宽数字 value/sub） */
export function StatCard({
  icon,
  label,
  value,
  sub,
  className,
  status = "ready",
  onRetry,
}: StatCardProps) {
  const blank = status !== "ready";
  return (
    <div
      className={cn("stat-card", status === "error" && "stat-card--error", className)}
      role={status === "error" ? "alert" : undefined}
      aria-busy={status === "loading" ? true : undefined}
    >
      <div className="label">
        {icon ? <Icon name={icon} size={15} /> : null}
        {label}
      </div>
      <div className="value">{blank ? "—" : value}</div>
      {status === "error" ? (
        <div className="sub">
          <span>加载失败</span>
          {onRetry ? (
            <button type="button" className="btn btn-ghost btn-sm" onClick={onRetry}>
              重试
            </button>
          ) : null}
        </div>
      ) : sub ? (
        <div className="sub">{sub}</div>
      ) : null}
    </div>
  );
}
