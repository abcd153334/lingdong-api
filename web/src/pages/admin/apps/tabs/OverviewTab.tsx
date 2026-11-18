import { Card } from "@/components/common/Card";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { StatCard } from "@/components/common/StatCard";
import { TrendChart } from "@/components/common/Charts";
import { EmptyState } from "@/components/common/EmptyState";
import { Tag } from "@/components/common/StatusTag";
import { useAdminAppStatsQuery } from "@/lib/api";
import { formatCent, formatDateTime, formatMoney } from "@/lib/format";
import type { AdminAppItem } from "@/types";

/**
 * 概览（应用仪表盘）。
 *
 * 数据来源只有两处，且都不为本页单独造数字：
 *   - 四格汇总与基础信息全部来自**列表行**（`app`）—— 卡片显示的就是它们，
 *     所以弹窗与卡片不可能对不上；
 *   - 趋势图与"最近调用"来自 `/stats`，与「在线」页签共用同一条查询。
 *
 * 四格与基础信息不阻塞在统计上：统计慢或失败时，概览的其余部分照常可读。
 */
export function OverviewTab({
  app,
  onToggleStatus,
  onEdit,
  statusBusy,
}: {
  app: AdminAppItem;
  onToggleStatus: () => void;
  onEdit: () => void;
  statusBusy: boolean;
}) {
  const stats = useAdminAppStatsQuery(app.id);
  const trend = stats.data?.trend ?? [];
  const charge7d = trend.reduce((sum, p) => sum + p.charge, 0);

  return (
    <div className="tab-panel">
      <div className="grid-4">
        {/* "累计消费" 读 api_keys.used：它包含**在途预扣**（冻结额），
            结算后按实际金额回调，所以这个数字不是单调递增的。
            下面的"近 7 日消费合计"读 usages.charge（只算已结算）。
            两者口径不同，必须写明，否则运营会拿它们互相对账并判成"漏扣"。 */}
        <StatCard
          icon="wallet"
          label="累计消费"
          value={<span className="mono">{formatMoney(app.used)}</span>}
          sub="含在途预扣"
        />
        <StatCard
          icon="coin"
          label="已付充值"
          value={<span className="mono">{formatCent(app.paid_cent)}</span>}
          sub={`${app.paid_count} 笔`}
        />
        <StatCard
          icon="key"
          label="托管密钥"
          value={
            <span className="mono">
              {app.active_key_count}
              <span className="muted"> / {app.key_count}</span>
            </span>
          }
          sub="启用 / 全部"
        />
        <StatCard icon="user" label="用户数" value={app.user_count} sub="持有托管密钥" />
      </div>

      <Card title="近 7 日调用" sub="按业务时区分日，无调用的日期补 0">
        {stats.isPending ? (
          <EmptyState variant="loading" title="加载中…" />
        ) : stats.isError || !stats.data ? (
          <EmptyState variant="error" title="统计数据加载失败" desc="请稍后重试" />
        ) : (
          <>
            <TrendChart
              data={trend.map((p) => ({ date: p.date.slice(5), requests: p.requests }))}
              xKey="date"
              series={[{ key: "requests", name: "调用次数" }]}
              height={200}
              valueFormatter={(v) => v.toLocaleString("zh-CN")}
              empty={<EmptyState title="近 7 日没有调用" desc="接入方还没有发起过请求" />}
            />
            <div className="count-line">
              近 7 日消费合计（已结算）{" "}
              <b className="mono">{formatMoney(charge7d)}</b> · 最近调用{" "}
              {stats.data.online.last_call_at || "—"}
            </div>
          </>
        )}
      </Card>

      <div className="kv-grid">
        <div className="kv">
          <div className="small muted">应用标识</div>
          <div className="mono">{app.slug}</div>
          <div className="small muted">创建后不可改</div>
        </div>
        <div className="kv">
          <div className="small muted">接入方式</div>
          <div>{app.access_mode === "self_service" ? "自助签发" : "运营发放"}</div>
          <div className="small muted">
            {app.access_mode === "self_service" ? "用户可自行换取凭证" : "仅管理员发放接入凭证"}
          </div>
        </div>
        <div className="kv">
          <div className="small muted">后台名称</div>
          <div>{app.name}</div>
          <div className="small muted">
            {app.display_name !== app.name ? `对外展示为「${app.display_name}」` : "对外同名"}
          </div>
        </div>
        <div className="kv">
          <div className="small muted">创建时间</div>
          <div className="mono">{formatDateTime(app.created_at)}</div>
          <div className="small muted">状态 {app.status === 1 ? "启用" : "停用"}</div>
        </div>
      </div>

      <div className="alert-banner info">
        <span>
          停用只拦「新签发」，已签发的密钥仍可调用，并<b>继续产生扣费</b>。要立即断流，请在「在线」页签开启维护模式。
        </span>
      </div>

      <div className="ph-acts">
        <Tag tone={app.status === 1 ? "success" : "gray"}>
          {app.status === 1 ? "当前：启用" : "当前：停用"}
        </Tag>
        <button type="button" className="btn" onClick={onEdit}>
          编辑基本信息
        </button>
        <button
          type="button"
          className={app.status === 1 ? "btn btn-danger" : "btn"}
          disabled={statusBusy}
          onClick={async () => {
            const disable = app.status === 1;
            const ok = await confirmDialog({
              title: disable ? "停用该应用？" : "启用该应用？",
              body: disable
                ? "停用后不能再为它签发新的托管密钥；已签发的密钥仍可继续调用。"
                : "启用后用户可以重新为该应用签发托管密钥。",
              okText: disable ? "停用" : "启用",
              okVariant: disable ? "danger" : "primary",
            });
            if (ok) onToggleStatus();
          }}
        >
          {app.status === 1 ? "停用应用" : "启用应用"}
        </button>
      </div>
    </div>
  );
}
