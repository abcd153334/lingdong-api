import { useState } from "react";
import { Icon } from "@/components/common/Icon";
import { confirmDialog } from "@/components/common/ConfirmDialog";
import { ApiError } from "@/lib/apiClient";
import type { AgentProposal, DiffStats } from "@/lib/api/agent";
import {
  useAgentProposalQuery,
  useApplyProposalMutation,
  useRejectProposalMutation,
  useRollbackProposalMutation,
} from "@/lib/api/agent";
import { cn } from "@/lib/utils";
import { useAgentScope } from "./scope";

/** 提案状态 → 中文（状态机文案唯一来源，别在页面里另写一套）。 */
const STATUS_LABEL: Record<string, string> = {
  draft: "待确认",
  applying: "应用中",
  applied: "已生效",
  rejected: "已忽略",
  failed: "应用失败",
  stale: "需要重新生成",
};

const STATUS_TONE: Record<string, string> = {
  draft: "draft",
  applying: "live",
  applied: "ok",
  rejected: "muted",
  failed: "bad",
  stale: "bad",
};

/** 探测未通过的原因 → 中文（判定在后端 ProbeTarget.reason，这里只翻译）。 */
const PROBE_REASON: Record<string, string> = {
  missing: "未探测",
  expired: "凭据已过期",
  failed: "上次探测未通过",
  key_mismatch: "密钥已更换",
};

const OP_LABEL: Record<string, string> = {
  create: "新建",
  update: "修改",
  disable: "停用",
  replace: "替换",
};

const ENTITY_LABEL: Record<string, string> = {
  vendor: "供应商分组",
  channel: "渠道",
  model: "模型",
  model_bindings: "模型绑定",
  adapter_spec: "声明式规格",
};

interface DiffItem {
  op?: string;
  entity?: string;
  key?: string;
  id?: number;
  after?: unknown;
}

function readStats(p: AgentProposal): DiffStats {
  const raw = p.diff as { stats?: DiffStats } | null | undefined;
  return raw?.stats ?? { added: 0, changed: 0, removed: 0 };
}

function readItems(p: AgentProposal): DiffItem[] {
  const raw = p.diff as { items?: DiffItem[] } | null | undefined;
  return Array.isArray(raw?.items) ? raw.items : [];
}

function summaryOf(p: AgentProposal): string {
  const plan = p.plan as { summary?: string } | undefined;
  return plan?.summary ?? "";
}

/**
 * 提案卡：聊天流里唯一的"交付物"。
 *
 * 三个动作语义不同，不能合并：
 *  - 应用：写生产表（要求 plan_hash + confirm，防篡改防误触）；
 *  - 回滚：生成一条**反向提案**（不是撤销，回滚本身也要确认）；
 *  - 忽略：只把这条标成 rejected，不动任何数据。
 *
 * 为什么应用要多一层二次确认：这是聊天界面，按钮会随着内容滚动从光标下方经过，
 * 误点的代价是直接改线上配置。`plan_hash` 防的是"显示的是 A、提交的是 B"，
 * 防不了手滑。
 */
export function AgentProposalCard({
  proposal,
  defaultOpen = false,
  preview = false,
}: {
  proposal: AgentProposal;
  /** 刚生成的提案默认展开改动明细；历史提案保持收起。 */
  defaultOpen?: boolean;
  /** 骨架模式：摘要已到、明细还没到，动作按钮先禁用。 */
  preview?: boolean;
}) {
  const apply = useApplyProposalMutation();
  // 必须在组件顶层取：hook 不能在事件回调里调用（React 会抛 Invalid hook call，
  // 而 tsc 完全看不出来）。
  const scope = useAgentScope();
  const rollback = useRollbackProposalMutation();
  const reject = useRejectProposalMutation();
  const [err, setErr] = useState("");
  const [notice, setNotice] = useState("");
  // 与 notice 分开：它是"成功了但有副作用没做完"，用成功色会让运营以为文档也更新了。
  const [warn, setWarn] = useState("");
  const [openDiff, setOpenDiff] = useState(defaultOpen);
  const detail = useAgentProposalQuery(proposal.id, openDiff);

  const st = readStats(proposal);
  // 探测未满足时禁用"确认应用"：点了也只会在后端吃 409（同一句话），
  // 不如在按钮旁直接说清缺哪个地址、下一步做什么。
  const probe = proposal.probe;
  const pendingProbe = !!probe?.required && !probe.satisfied;
  const canApply = !preview && proposal.kind === "change" && proposal.status === "draft" && !pendingProbe;
  // 忽略提案不受探测状态影响：运营可能就是不想接了。
  const canReject = !preview && proposal.status === "draft";
  const canRollback = !preview && proposal.status === "applied";
  const items = readItems(detail.data?.item ?? proposal);
  // L3 结论（kind=unsupported）的正文：没有操作清单，只有原因与所需能力。
  // 不渲染它，运营看到的就只是"需写代码适配"四个字，既无法排期也无法复核。
  const unsup = (proposal.plan as {
    unsupported?: { vendor?: string; reasons?: string[]; required_features?: string[] };
  })?.unsupported;
  const ops = items.length || (proposal.plan as { operations?: unknown[] })?.operations?.length || 0;

  const failText = (e: unknown) => (e instanceof ApiError ? e.message : "操作失败，请重试");

  const onApply = async () => {
    setErr("");
    setNotice("");
    setWarn("");
    const ok = await confirmDialog({
      title: `确认写入这 ${ops} 项变更？`,
      body: (
        <>
          会立即修改线上的渠道、模型或规格配置，并要求与页面显示的内容完全一致
          （内容被改过会被拒绝）。写入后可以生成反向提案回滚。
          改动：{st.added} 新建 / {st.changed} 修改 / {st.removed} 停用。
        </>
      ),
      okText: "确认写入",
    });
    if (!ok) return;
    apply.mutate(
      { id: proposal.id, planHash: proposal.plan_hash, scope },
      {
        onSuccess: (res) => {
          setNotice(`已生效，共执行 ${res.ops} 个操作`);
          // warnings 与 docs_error 合并成一条提醒区：两者都是"已落库但还要人看一眼"，
          // 分成两个位置会让其中一条永远没人看（尤其 docs_error 只影响文档）。
          const warns = [...(res.warnings ?? [])];
          if (res.docs_error) {
            warns.push("文档站重算失败（下次变更或重启会自动补）");
          }
          setWarn(warns.length ? `已生效，但有 ${warns.length} 条提醒：${warns.join("；")}` : "");
        },
        onError: (e) => setErr(failText(e)),
      },
    );
  };

  const onRollback = async () => {
    setErr("");
    setNotice("");
    const ok = await confirmDialog({
      title: "生成回滚提案？",
      body: "回滚不是撤销：它先生成一条反向提案，你确认之后才会把配置改回去。",
      okText: "生成回滚提案",
    });
    if (!ok) return;
    rollback.mutate(proposal.id, {
      onSuccess: () => setNotice("已生成反向提案，确认后才会改回去"),
      onError: (e) => setErr(failText(e)),
    });
  };

  const onReject = () => {
    setErr("");
    setNotice("");
    reject.mutate(proposal.id, {
      onSuccess: () => setNotice("已忽略这条提案"),
      onError: (e) => setErr(failText(e)),
    });
  };

  return (
    <div
      className="agent-prop"
      id={`proposal-${proposal.id}`}
      data-testid={`agent-proposal-${proposal.id}`}
      data-status={proposal.status}
    >
      <div className="agent-prop__head">
        <span className={cn("agent-tag", `agent-tag--${STATUS_TONE[proposal.status] ?? "muted"}`)}>
          {STATUS_LABEL[proposal.status] ?? proposal.status}
        </span>
        <span className="agent-prop__title">{summaryOf(proposal) || `提案 #${proposal.id}`}</span>
        <span className="agent-prop__id">#{proposal.id}</span>
        {proposal.kind === "unsupported" ? <span className="agent-tag">需写代码适配</span> : null}
      </div>

      {proposal.kind === "unsupported" ? (
        <div className="agent-prop__unsup" data-testid={`agent-unsupported-${proposal.id}`}>
          {unsup?.vendor ? (
            <div className="agent-prop__muted">厂商：{unsup.vendor}</div>
          ) : null}
          {unsup?.reasons?.length ? (
            <ul className="agent-unsup__list">
              {unsup.reasons.map((r, i) => (
                <li key={i}>{r}</li>
              ))}
            </ul>
          ) : null}
          {unsup?.required_features?.length ? (
            <div className="agent-prop__muted">需要新增的能力：{unsup.required_features.join("、")}</div>
          ) : null}
        </div>
      ) : null}

      {proposal.kind === "change" ? (
        <div className="agent-prop__stats">
          <span>{ops} 项操作</span>
          <span className="agent-dot">新建 {st.added}</span>
          <span className="agent-dot">修改 {st.changed}</span>
          <span className="agent-dot">停用 {st.removed}</span>
          <button
            type="button"
            className="agent-linkbtn"
            aria-expanded={openDiff}
            onClick={() => setOpenDiff((v) => !v)}
          >
            {openDiff ? "收起改动" : "查看改动"}
            <Icon name="chevronDown" size={12} className={cn("agent-fold", openDiff && "is-open")} />
          </button>
        </div>
      ) : null}

      {probe?.required ? (
        probe.satisfied ? (
          <div className="agent-prop__stats" data-testid="agent-probe-ok">
            <span className="agent-tag agent-tag--ok">探测已通过</span>
            <span className="agent-prop__muted">
              {(probe.targets ?? []).map((t) => t.base_url).join("、")}
              {probe.targets?.[0]?.expires_at
                ? `（凭据有效至 ${probe.targets[0].expires_at.replace("T", " ").slice(0, 16)}）`
                : ""}
            </span>
          </div>
        ) : (
          <>
            <div className="agent-alert agent-alert--warn" data-testid="agent-probe-required">
              <Icon name="alert" size={14} />
              <span className="agent-alert__text">
                需先探测 {probe.targets.filter((t) => !t.satisfied).length} 个目标：
                {probe.targets
                  .filter((t) => !t.satisfied)
                  .map((t) => `${t.base_url}（${PROBE_REASON[t.reason] ?? t.reason}）`)
                  .join("；")}
              </span>
            </div>
            <p className="agent-prop__hint">
              在本会话里让它做一次连通性探测（例如说「做一下连通性探测」）即可，凭据 24 小时内有效，
              不需要重新生成提案。
            </p>
          </>
        )
      ) : null}

      {openDiff ? (
        <div className="agent-prop__diff" data-testid={`agent-diff-${proposal.id}`}>
          {detail.isLoading ? (
            <div className="agent-prop__muted">正在读取改动明细…</div>
          ) : items.length === 0 ? (
            <div className="agent-prop__muted">这条提案没有字段明细（历史数据或结论型提案）</div>
          ) : (
            <ul>
              {items.map((it, i) => (
                <li key={i} className="agent-diffrow">
                  <span className="agent-diffrow__op" data-op={it.op}>
                    {OP_LABEL[it.op ?? ""] ?? it.op ?? "操作"}
                  </span>
                  <span className="agent-diffrow__entity">
                    {ENTITY_LABEL[it.entity ?? ""] ?? it.entity ?? "对象"}
                  </span>
                  {it.key ? <span className="agent-diffrow__key">{it.key}</span> : null}
                  {it.id ? <span className="agent-diffrow__key">#{it.id}</span> : null}
                  {it.after ? (
                    <details className="agent-diffrow__more">
                      <summary>字段</summary>
                      <pre>{JSON.stringify(it.after, null, 2)}</pre>
                    </details>
                  ) : null}
                </li>
              ))}
            </ul>
          )}
        </div>
      ) : null}

      {proposal.error ? <div className="agent-alert agent-alert--bad">{proposal.error}</div> : null}
      {err ? <div className="agent-alert agent-alert--bad">{err}</div> : null}
      {notice ? <div className="agent-alert agent-alert--ok">{notice}</div> : null}
      {warn ? <div className="agent-alert agent-alert--warn">{warn}</div> : null}

      {canApply || canReject || canRollback ? (
        <div className="agent-prop__acts">
          {canApply ? (
            <button
              type="button"
              className="btn btn-primary btn-sm"
              onClick={() => void onApply()}
              disabled={apply.isPending}
              data-testid={`agent-apply-${proposal.id}`}
            >
              {apply.isPending ? "应用中…" : "确认应用"}
            </button>
          ) : null}
          {canRollback ? (
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => void onRollback()}
              disabled={rollback.isPending}
              data-testid={`agent-rollback-${proposal.id}`}
            >
              {rollback.isPending ? "生成中…" : "回滚"}
            </button>
          ) : null}
          {canReject ? (
            <button
              type="button"
              className="btn btn-sm"
              onClick={onReject}
              disabled={reject.isPending}
              data-testid={`agent-reject-${proposal.id}`}
            >
              忽略
            </button>
          ) : null}
        </div>
      ) : null}

      {/* stale 的说明必须在动作块之外：stale 时三个动作全都不可用，
          写在块里就永远不会显示，运营只会看到一张"没有按钮也没有原因"的卡。 */}
      {proposal.status === "stale" ? (
        <div className="agent-prop__hint" data-testid={`agent-stale-${proposal.id}`}>
          数据已变化，请让助手重新生成提案
        </div>
      ) : null}
    </div>
  );
}
