/**
 * 任务进度（唯一实现）。
 *
 * 列表与详情抽屉共用；终态不画进度条是刻意设计：
 * 后端进度由状态派生（`model.TaskProgress`），失败/过期/取消同样是 100，
 * 直接渲染就会出现「失败 + 满格进度条」这种自相矛盾的画面。
 */

const ENDED = new Set(["failed", "expired", "canceled"]);

export function TaskProgress({ status, progress }: { status: string; progress?: number }) {
  if (ENDED.has(status)) {
    return <span className="muted small">已结束</span>;
  }
  const raw = progress ?? (status === "succeeded" ? 100 : 0);
  const p = Math.max(0, Math.min(100, Math.round(raw)));
  return (
    <div className="share-cell">
      <div className="progress">
        <i style={{ width: `${p}%` }} />
      </div>
      <span className="num small">{p}%</span>
    </div>
  );
}
