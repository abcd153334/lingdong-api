import type { ErrorComponentProps } from "@tanstack/react-router";
import { EmptyState } from "./EmptyState";
import { ApiError } from "@/lib/apiClient";

/**
 * RouteError 是**路由级**错误兜底。
 *
 * 之前整仓没有 ErrorBoundary，任何渲染期异常都会落到 TanStack 默认页
 * （英文 "Something went wrong!"，无重试、无回首页），中文用户只能刷新碰运气。
 *
 * 这里刻意不吞掉错误信息：`message` 直接展示，配合 `data-testid` 让 e2e 能断言
 * 「本该正常渲染的页面没有落进错误页」。
 */
export function RouteError({ error, reset }: ErrorComponentProps) {
  const message =
    error instanceof ApiError || (error instanceof Error && error.message)
      ? (error as Error).message
      : "页面渲染时发生未知错误";

  return (
    <div data-testid="route-error" className="page">
      <EmptyState
        variant="error"
        title="页面加载失败"
        desc={message}
        action={
          <div className="ph-acts">
            <button type="button" className="btn btn-primary btn-sm" onClick={reset}>
              重试
            </button>
            {/* 用原生 a 而不是 router Link：错误页出现时路由状态可能已经坏了，
                整页跳转是最可靠的出口（代价只是丢一次 SPA 导航）。 */}
            <a href="/home" className="btn btn-ghost btn-sm">
              返回首页
            </a>
          </div>
        }
      />
      <p className="muted small" style={{ marginTop: 12 }}>
        如果反复出现，请把这条提示与操作步骤反馈给管理员。
      </p>
    </div>
  );
}

/** RoutePending 是 beforeLoad（会话探测）期间的占位，避免白屏等待。 */
export function RoutePending() {
  return (
    <div className="page">
      <EmptyState variant="loading" title="正在加载…" />
    </div>
  );
}
