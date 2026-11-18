import { useMemo } from "react";
import { Icon } from "@/components/common/Icon";
import type { AgentProposal } from "@/lib/api/agent";
import { AgentAskCard } from "./AgentAskCard";
import { AgentProposalCard } from "./AgentProposalCard";
import { AgentThinking, AgentToolGroup } from "./AgentProcess";
import { Markdown } from "./Markdown";
import { formatMoney } from "@/lib/format";
import { toBlocks, type Turn } from "./turns";

export interface AgentTurnProps {
  /** 提问作答要带会话 id */
  sessionId: number;
  turn: Turn;
  index: number;
  proposals: Map<number, AgentProposal>;
  /** 提案列表还在加载：此时"读不到"只是还没到，不能说成"可能已删除" */
  proposalsLoading?: boolean;
  /** 这一轮正在跑（只有实时轮可能为 true） */
  running: boolean;
  timeLabel?: string;
  /** 重试：用同一段用户原话再发一轮（失败轮专用） */
  onRetry?: (text: string) => void;
  /** 去设置：预算/无模型/总闸关闭这类"改配置才能解决"的错误 */
  onOpenSettings?: () => void;
  /** 提问作答成功后把本地段切成终态 */
  onAnswerAsk: (askId: number, status: string, answer?: string) => void;
}

/** 这些错误靠"再试一次"解决不了，要给的是设置入口。 */
const SETTINGS_CODES = new Set([
  "agent_budget_exceeded",
  "agent_model_unavailable",
  "agent_disabled",
  // 这两个的错误文案本身就是"请去运行参数里调大 / 换窗口更大的模型"，
  // 少了按钮就只剩一句让用户自己找入口的话。
  "agent_budget_too_small",
  "context_overflow",
]);

/** token 计数的人类可读形式（上下文占用动辄几十万，原始数字没法扫读）。 */
function fmtTokens(n: number): string {
  return n >= 1000 ? `${(n / 1000).toFixed(1)}k` : String(n);
}

/**
 * 一轮对话：用户气泡 → 过程（思考/工具/叙述/提案）→ 答复 → 元信息。
 *
 * 顺序就是真相：所有段都来自同一个有序数组（见 turns.ts），
 * 不再出现"正文在上、工具卡在下"的错位。
 */
export function AgentTurn({
  sessionId,
  turn,
  index,
  proposals,
  proposalsLoading,
  running,
  timeLabel,
  onRetry,
  onOpenSettings,
  onAnswerAsk,
}: AgentTurnProps) {
  const blocks = useMemo(() => toBlocks(turn.segments), [turn.segments]);
  // 实时轮的所有文本都在段里（含最后一段答复）；落库轮的答复单独存在 answer。
  const answer = turn.live ? "" : turn.answer;
  const showThinking = running && (turn.status === "running");
  const hasErrorBlock = blocks.some((b) => b.type === "error");

  const meta: string[] = [];
  // 本轮实际用的模型排第一：会话级模型回退到全站默认时，这是唯一能看出来的地方。
  if (turn.meta.model) meta.push(turn.meta.model);
  if (turn.meta.steps) meta.push(`${turn.meta.steps} 步`);
  if (turn.meta.tools) meta.push(`${turn.meta.tools} 个操作`);
  // 金额是**微**：必须走 formatMoney，否则会显示成 100 万倍的"额度"数字。
  if (turn.meta.quota) meta.push(`约 ${formatMoney(turn.meta.quota)}`);
  if (turn.meta.tokensIn || turn.meta.tokensOut) {
    meta.push(`${(turn.meta.tokensIn ?? 0) + (turn.meta.tokensOut ?? 0)} tokens`);
  }
  // 上下文占用只在实时轮有（落库轮不存 run 计量）：显示"用了多少 / 有多少"，
  // 并说明被降级了多少轮 —— 否则运营会以为模型"忘事"。
  if (turn.meta.contextTokens && turn.meta.contextWindow) {
    const sum = turn.meta.summarizedTurns ? ` · 摘要 ${turn.meta.summarizedTurns} 轮` : "";
    meta.push(`上下文 ${fmtTokens(turn.meta.contextTokens)}/${fmtTokens(turn.meta.contextWindow)}${sum}`);
  }
  let elapsed = "";
  if (turn.live && turn.startedAt) {
    const end = turn.endedAt ?? Date.now();
    elapsed = `${((end - turn.startedAt) / 1000).toFixed(1)}s`;
  }

  const copy = () => {
    const text = turn.live
      ? turn.segments
          .filter((s) => s.kind === "text")
          .map((s) => (s as { text: string }).text)
          .join("\n\n")
      : turn.answer;
    if (text) void navigator.clipboard?.writeText(text);
  };

  return (
    <section className="agent-turn" data-testid={`agent-turn-${index}`}>
      {turn.userText ? (
        <div className="agent-turn__user">
          <div className="agent-bubble">{turn.userText}</div>
          {timeLabel ? <span className="agent-turn__time">{timeLabel}</span> : null}
        </div>
      ) : null}

      <div className="agent-turn__body">
        {blocks.map((b, i) => {
          const isLast = i === blocks.length - 1;
          switch (b.type) {
            case "reason":
              return (
                <AgentThinking
                  key={b.key}
                  text={b.text}
                  running={showThinking && isLast}
                  index={i}
                />
              );
            case "tools":
              return (
                <AgentToolGroup
                  key={b.key}
                  items={b.items}
                  running={showThinking && isLast}
                  index={i}
                />
              );
            case "text":
              return (
                <div key={b.key} className="agent-say">
                  <Markdown text={b.text} />
                </div>
              );
            case "proposal": {
              const p = proposals.get(b.proposalId);
              if (p) return <AgentProposalCard key={b.key} proposal={p} />;
              // 流里已经带了摘要/统计：先用骨架把卡片画出来，别让运营盯着"读取中"。
              if (b.preview) {
                const skeleton: AgentProposal = {
                  id: b.proposalId,
                  session_id: 0,
                  message_id: 0,
                  kind: b.preview.kind as AgentProposal["kind"],
                  status: b.preview.status as AgentProposal["status"],
                  plan: { summary: b.preview.summary },
                  plan_hash: b.preview.planHash,
                  diff: { stats: b.preview.diffStats ?? { added: 0, changed: 0, removed: 0 }, items: [] },
                  created_at: "",
                };
                return <AgentProposalCard key={b.key} proposal={skeleton} preview />;
              }
              return (
                <div key={b.key} className="agent-say agent-say--muted">
                  {proposalsLoading
                    ? `正在读取提案 #${b.proposalId}…`
                    : `提案 #${b.proposalId} 读不到（可能已被删除）`}
                </div>
              );
            }
            case "ask":
              return (
                <AgentAskCard
                  key={b.key}
                  ask={b.ask}
                  sessionId={sessionId}
                  onSettled={(status, answer) => onAnswerAsk(b.ask.askId, status, answer)}
                />
              );
            case "error":
              return (
                <div
                  key={b.key}
                  className="agent-alert agent-alert--bad"
                  data-testid="agent-run-error"
                >
                  <Icon name="alert" size={14} />
                  <span className="agent-alert__text">{b.message || "这一轮没有完成"}</span>
                  <span className="agent-alert__acts">
                    {b.retryable && turn.userText && !running ? (
                      <button
                        type="button"
                        className="agent-linkbtn"
                        onClick={() => onRetry?.(turn.userText)}
                      >
                        重试
                      </button>
                    ) : null}
                    {SETTINGS_CODES.has(b.code) ? (
                      <button type="button" className="agent-linkbtn" onClick={onOpenSettings}>
                        去设置
                      </button>
                    ) : null}
                  </span>
                </div>
              );
            case "elided":
              return (
                <div key={b.key} className="agent-say agent-say--muted">
                  过程较长，中间省略了 {b.count} 段
                </div>
              );
            default:
              return null;
          }
        })}

        {answer ? (
          <div className="agent-answer">
            <Markdown text={answer} />
            {running ? <span className="agent-caret" aria-hidden="true" /> : null}
          </div>
        ) : null}

        {!answer && running && blocks.length === 0 ? (
          <div className="agent-say agent-say--muted">正在准备…</div>
        ) : null}

        {turn.status === "aborted" && !hasErrorBlock ? (
          <div className="agent-alert agent-alert--warn">
            已停止，已生成的内容已保留，可以接着说
          </div>
        ) : null}
        {turn.status === "failed" && !hasErrorBlock ? (
          <div className="agent-alert agent-alert--bad">这一轮没有完成</div>
        ) : null}

        {meta.length > 0 || elapsed ? (
          <div className="agent-turn__meta">
            <span>{[...meta, elapsed].filter(Boolean).join(" · ")}</span>
            {(answer || turn.segments.length > 0) && !running ? (
              <button type="button" className="agent-linkbtn" onClick={copy}>
                <Icon name="copy" size={12} />
                复制
              </button>
            ) : null}
          </div>
        ) : null}
      </div>
    </section>
  );
}
