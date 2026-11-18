import { EmptyState } from "./EmptyState";
import { ApiError } from "@/lib/apiClient";

/**
 * ErrorState 是「请求失败」的统一展示。
 *
 * 为什么单独抽出来：2026-10-29 审计发现列表页失败时会渲染成「暂无数据」——
 * 用户以为真的没有记录，于是去别处找。失败必须有**自己的**文案与动作。
 */
export function ErrorState({
  error,
  onRetry,
  title = "加载失败",
  testId,
}: {
  error?: unknown;
  onRetry?: () => void;
  title?: string;
  testId?: string;
}) {
  const desc =
    error instanceof ApiError || (error instanceof Error && error.message)
      ? (error as Error).message
      : "请稍后重试";
  return (
    <div data-testid={testId}>
      <EmptyState
        variant="error"
        icon="⚠"
        title={title}
        desc={desc}
        action={
          onRetry ? (
            <button type="button" className="btn btn-ghost btn-sm" onClick={onRetry}>
              重试
            </button>
          ) : undefined
        }
      />
    </div>
  );
}

/** LoadingState 是列表首屏的骨架位，避免空白闪一下。 */
export function LoadingState({ title = "正在加载…" }: { title?: string }) {
  return <EmptyState icon="◇" title={title} />;
}
