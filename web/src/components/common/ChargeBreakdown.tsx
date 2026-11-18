import { MoneyText, QuotaUnit } from "./MoneyText";
import { moneyAt } from "@/lib/money";
import {
  anomalyLabels,
  capReasonLabel,
  chargeConclusion,
  chargeRows,
  isCapped,
  sourceLabel,
  tierSourceLabel,
} from "@/lib/charge";
import type { AdminCallDetail, PortalCallDetail } from "@/types";

/**
 * 扣费明细：一句话结论 + 只含参与计算项的明细。
 *
 * 三条与"去冗余"直接相关的规则：
 *  1. 全零行不渲染（`chargeRows` 负责）—— 一张图像记录的 5 个 token 通道全是 0，
 *     平铺出来就是 5 行没有信息的 0；
 *  2. 金额只出现一次且一律带币种符号：金额是「微」，只有 `d.currency` 决定符号，
 *     不存在任何换算基准（原来的"刻度"已删除）；
 *  3. 历史记录用**行内快照币种**渲染 —— 美元时代那一笔必须显示 `$`。
 */
export function ChargeBreakdown({
  detail,
  portal,
}: {
  detail: PortalCallDetail;
  /** 门户视角：不渲染截顶档位与计费来源（那两项属于平台风控口径） */
  portal?: boolean;
}) {
  const d = detail;
  // 截顶原因与计费来源只在管理台读取：门户契约里没有这两个键。
  const admin = portal ? null : (detail as AdminCallDetail);
  const rows = chargeRows(d);
  const capped = isCapped(d);
  const anomalies = anomalyLabels(d.anomaly);
  const tier = tierSourceLabel(d.tier_source);
  const cur = d.currency;

  return (
    <div className="cd-charge">
      <div className="cd-charge-lead">{chargeConclusion(d)}</div>

      {rows.length > 0 ? (
        <div className="cd-charge-body">
          {rows.map((r) => (
            <div className="cd-charge-line" key={r.label}>
              <span className="k">{r.label}</span>
              <span className="v mono">{r.value}</span>
            </div>
          ))}
        </div>
      ) : null}

      <div className="cd-charge-rule" />

      {capped ? (
        <div className="cd-charge-line">
          <span className="k">应收（截顶前）</span>
          <span className="v mono">
            <MoneyText micro={d.charge_before_cap} currency={cur} />
          </span>
        </div>
      ) : null}
      {(d.frozen ?? 0) > 0 ? (
        <div className="cd-charge-line">
          <span className="k">冻结额</span>
          <span className="v mono">
            <MoneyText micro={d.frozen} currency={cur} />
          </span>
        </div>
      ) : null}
      <div className="cd-charge-line is-total">
        <span className="k">实收</span>
        <span className="v">
          <QuotaUnit micro={d.charge} currency={cur} />
        </span>
      </div>

      {capped ? (
        <div className="cd-warn">
          {admin
            ? `已按「${capReasonLabel(admin.cap_reason) || "上限"}」截顶：应收 ${moneyAt(
                d.charge_before_cap,
                cur,
              )} → 实收 ${moneyAt(d.charge, cur)}`
            : `已按单次上限截顶：应收 ${moneyAt(d.charge_before_cap, cur)} → 实收 ${moneyAt(
                d.charge,
                cur,
              )}`}
        </div>
      ) : null}
      {anomalies.length > 0 ? <div className="cd-warn">{anomalies.join(" · ")}</div> : null}

      {admin || tier || (d.tier_price ?? 0) > 0 ? (
        <div className="cd-muted">
          {admin ? sourceLabel(admin.usage_source) : null}
          {admin && tier ? " · " : ""}
          {tier}
          {(d.tier_price ?? 0) > 0
            ? `${admin || tier ? " · " : ""}档位单价 ${moneyAt(d.tier_price, cur)}`
            : ""}
        </div>
      ) : null}
    </div>
  );
}
