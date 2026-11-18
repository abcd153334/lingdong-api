import { Elapsed, formatDuration } from "./Elapsed";
import { EntityCell } from "./EntityCell";
import { copyText } from "@/lib/clipboard";
import { QuotaUnit } from "./MoneyText";
import { ModalityTag, StatusTag, usageStatusKey } from "./StatusTag";
import type { PortalCallDetail } from "@/types";

export interface CallSummaryBarProps {
  detail: PortalCallDetail;
}

/**
 * 详情摘要条：一行回答「成没成功、什么模型、多久、花了多少」。
 *
 * 为什么不用卡片网格：原来 13 个 `.kv` 各带底色与边框，视觉上每个字段都同等重要，
 * 而实际上"状态/模型/耗时/金额"是结论，"IP/适配器/请求 ID"是排障线索。
 * 把它们放进同一层级，等于让用户自己去分辨哪 4 个该先看。
 */
export function CallSummaryBar({ detail }: CallSummaryBarProps) {
  const d = detail;
  const elapsed = d.elapsed_ms ?? d.latency_ms ?? 0;
  return (
    <div className="cd-summary">
      <StatusTag status={usageStatusKey(d.status)} />
      <span className="model" title={d.model}>
        <EntityCell iconUrl={d.model_icon_url} name={d.model} mono />
      </span>
      <ModalityTag modality={d.modality} />
      <span className="cd-sep" aria-hidden="true" />
      <Elapsed elapsedMs={elapsed} status={d.status} anomaly={d.anomaly} />
      {/* 服务耗时与总耗时并排：只给一个总耗时解释不了"慢在哪"。
          进行中不显示 —— 那一次请求可能还没发出。 */}
      {d.status !== "pending" && (d.service_ms ?? 0) > 0 ? (
        <span className="muted small">服务 {formatDuration(d.service_ms!)}</span>
      ) : null}
      {d.first_token_ms ? (
        <span className="muted small">首字延迟 {formatDuration(d.first_token_ms)}</span>
      ) : null}
      {(d.attempts ?? 0) > 1 ? (
        <span className="muted small">尝试 {d.attempts} 次</span>
      ) : null}
      {/* 密钥名：用户有多把 KEY 时，第一眼要能确认"这笔是哪把花的"。
          它不属于排障信息，所以放摘要条。 */}
      {d.key_name ? <span className="muted small">{d.key_name}</span> : null}
      {d.charge > 0 ? (
        <QuotaUnit micro={d.charge} currency={d.currency} className="small" />
      ) : (
        <span className="muted small">未扣费</span>
      )}
      <button
        type="button"
        className="cd-id"
        title="复制记录 ID"
        onClick={() => void copyText(String(d.id))}
      >
        #{d.id}
      </button>
    </div>
  );
}

/**
 * 耗时明细（放在排障区，不占摘要条）。
 *
 * 三个数一起看才有意义：总耗时远大于服务耗时 → 慢在排队/重试；
 * 两者接近 → 慢在这次调用本身。只说"耗时 6s"解释不了任何问题。
 */
export function LatencyDetail({ detail }: { detail: PortalCallDetail }) {
  const total = detail.elapsed_ms ?? detail.latency_ms ?? 0;
  const service = detail.service_ms ?? 0;
  const gap = total - service;
  return (
    <div className="cd-kv">
      <span className="k">总耗时</span>
      <span className="v mono">{formatDuration(total)}</span>
      {service > 0 ? (
        <>
          <span className="k">服务耗时</span>
          <span className="v mono">
            {formatDuration(service)}
            {gap > 200 && !detail.task_id ? (
              <span className="muted small"> · 网关/重试 {formatDuration(gap)}</span>
            ) : null}
          </span>
        </>
      ) : null}
      {detail.first_token_ms ? (
        <>
          <span className="k">首字延迟</span>
          <span className="v mono">{formatDuration(detail.first_token_ms)}</span>
        </>
      ) : null}
      {(detail.attempts ?? 0) > 1 ? (
        <>
          <span className="k">尝试次数</span>
          <span className="v mono">{detail.attempts} 次</span>
        </>
      ) : null}
    </div>
  );
}
