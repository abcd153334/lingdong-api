import { EntityIcon } from "@/components/common/EntityIcon";
import { Tag } from "@/components/common/StatusTag";
import { formatMoney } from "@/lib/format";
import type { AdminAppItem } from "@/types";

/**
 * 应用卡片。
 *
 * 刻意**不在卡片上放启停**：停用会拒绝一切新接入，属于低频高风险动作，
 * 放在卡片上容易被当成"展开/收起"误点。启停在弹窗「概览」页签里，
 * 带明确文案与二次确认。
 *
 * 指标只放三格（密钥 / 用户 / 近 7 日调用）：六七个数字堆在一张卡上，
 * 运营一眼抓不到重点；金额放底栏，与版本号同一行。
 */
export function AppCard({
  app,
  onManage,
  onKeys,
}: {
  app: AdminAppItem;
  onManage: () => void;
  onKeys: () => void;
}) {
  const title = app.display_name || app.name;
  const desc = app.tagline || app.description;

  return (
    <div className="app-card" data-testid="app-card">
      <div className="app-card-head">
        <EntityIcon url={app.logo_url} name={title} size="md" />
        <div className="app-card-title">
          <div className="name">{title}</div>
          <div className="mono slug">{app.slug}</div>
        </div>
      </div>

      <div className="ph-acts">
        {app.status === 1 ? <Tag tone="success">启用</Tag> : <Tag tone="gray">停用</Tag>}
        {app.maintenance_mode ? <Tag tone="warning">维护中</Tag> : null}
        {app.allow_self_issue ? null : <Tag tone="warning">关闭自助签发</Tag>}
        {app.notice_count > 0 ? <Tag tone="info">{app.notice_count} 条公告</Tag> : null}
      </div>

      {/* 固定两行高度：卡片等高靠它，否则描述长短会让同一行的卡片参差 */}
      <div className="app-card-desc">{desc || "暂无描述"}</div>

      <div className="app-card-metrics">
        <div>
          <b className="mono">
            {app.active_key_count}/{app.key_count}
          </b>
          <span>托管密钥</span>
        </div>
        <div>
          <b>{app.user_count}</b>
          <span>用户数</span>
        </div>
        <div>
          {/* `?? 0`：字段缺失（老缓存/后端裁剪）时 toLocaleString 会抛错，
              整张卡片连带整页都渲染不出来 —— 一个统计字段不值得这个代价。 */}
          <b className="mono">{(app.calls_7d ?? 0).toLocaleString("zh-CN")}</b>
          <span>近 7 日调用</span>
        </div>
      </div>

      <div className="app-card-foot">
        <span className="meta" title={app.latest_release?.published_at ?? ""}>
          {app.latest_release ? `v${app.latest_release.version}` : "未发布版本"}
          {" · "}
          <span className="mono">{formatMoney(app.used)}</span>
        </span>
        <span className="acts">
          <button type="button" className="btn btn-sm btn-text" onClick={onKeys}>
            密钥
          </button>
          <button type="button" className="btn btn-sm" onClick={onManage}>
            管理
          </button>
        </span>
      </div>
    </div>
  );
}
