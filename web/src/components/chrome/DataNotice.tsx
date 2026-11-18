import { useMeQuery, usePortalConfigQuery } from "@/lib/api";

/**
 * DataNotice 是门户侧的「数据没加载出来」常驻提示。
 *
 * 为什么需要它：`/portal/me` 失败只会弹一条 1.2s 的 toast（queryClient 去重后消失），
 * `/portal/config` 更是 `meta.silent`（连 toast 都没有）。用户看到的是"顶栏没名字、
 * 折扣按默认算"，却不知道是网络问题还是自己没登录 —— 2026-10-29 审计 A-04。
 *
 * 只在**失败**时出现，成功后不占位；与页面共用同一 queryKey，不会多发请求。
 */
export function DataNotice() {
  const me = useMeQuery();
  const config = usePortalConfigQuery();
  const failed = me.isError || config.isError;
  if (!failed) return null;

  const which = [me.isError ? "账号信息" : "", config.isError ? "站点配置" : ""]
    .filter(Boolean)
    .join("、");

  return (
    <div className="data-notice" role="alert" data-testid="data-notice">
      <span className="data-notice__text">
        {which}加载失败，页面显示的内容可能不完整。
      </span>
      <button
        type="button"
        className="btn btn-ghost btn-sm"
        onClick={() => {
          if (me.isError) void me.refetch();
          if (config.isError) void config.refetch();
        }}
      >
        重试
      </button>
    </div>
  );
}
