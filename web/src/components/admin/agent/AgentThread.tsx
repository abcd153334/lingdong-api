import dayjs from "dayjs";
import { Icon } from "@/components/common/Icon";
import type { AgentProposal, AskView } from "@/lib/api/agent";
import { AgentAskCard } from "./AgentAskCard";
import { AgentEmpty } from "./AgentEmpty";
import { AgentTurn } from "./AgentTurn";
import type { Turn } from "./turns";
import { useStickToBottom } from "./useStickToBottom";

export interface AgentThreadProps {
  /** 作答要带会话 id（mutate 成功后按会话失效消息缓存） */
  sessionId: number;
  turns: Turn[];
  proposals: Map<number, AgentProposal>;
  proposalsLoading?: boolean;
  running: boolean;
  messagesLoading: boolean;
  /** 历史消息拉取失败：必须显式呈现，不能退化成一个"新会话"的空态 */
  messagesError?: boolean;
  onRetryMessages?: () => void;
  onRetry?: (text: string) => void;
  onOpenSettings?: () => void;
  /** 提问作答成功后把本地段切成终态（不重取消息，见 useAgentRun.settleAsk） */
  onAnswerAsk: (askId: number, status: string, answer?: string) => void;
  /**
   * "库里有一个待答问题，但这条连接上没有实时轮"的情况。
   *
   * 典型场景：A 标签页在等回答，运营切到 B 标签页。B 没有 SSE 连接，
   * 若不显示这张卡，它既看不到问题也无法作答，而 A 那边正在倒数 60 秒。
   */
  orphanAsk?: { id: number; ask: AskView } | null;
  onPick: (text: string) => void;
  onOpenDocs: () => void;
}

/**
 * 对话滚动画布。
 *
 * 自动跟随的判据是"用户本来就贴着底部"，不是"有新内容就滚"：
 * 流式输出时每一帧都滚到底，等于让人无法回看上一段（他正想看的正是那段）。
 * 跟随由 useStickToBottom 负责（ResizeObserver：内容高度一变就重新贴底，
 * Markdown/代码块/提案卡/思考块展开都不会漏）。
 */
export function AgentThread({
  sessionId,
  turns,
  proposals,
  proposalsLoading,
  running,
  messagesLoading,
  messagesError,
  onRetryMessages,
  onRetry,
  onOpenSettings,
  onAnswerAsk,
  orphanAsk,
  onPick,
  onOpenDocs,
}: AgentThreadProps) {
  // 会话切换由外层 key={currentId} 重挂载处理，这里不需要再传会话标识。
  const { ref, stick, hasNew, scrollToBottom, onScroll } = useStickToBottom(0);

  const last = turns[turns.length - 1];
  const hasTurns = turns.length > 0;

  return (
    <div className="agent-thread-wrap">
      <div className="agent-thread" ref={ref} onScroll={onScroll} data-testid="agent-stream">
        <div className="agent-thread__inner">
          {messagesLoading ? (
            <div className="agent-thread__loading">正在加载对话…</div>
          ) : messagesError && !hasTurns ? (
            <div className="agent-alert agent-alert--bad" data-testid="agent-messages-error">
              <Icon name="alert" size={14} />
              <span className="agent-alert__text">历史消息加载失败</span>
              <span className="agent-alert__acts">
                <button type="button" className="agent-linkbtn" onClick={onRetryMessages}>
                  重试
                </button>
              </span>
            </div>
          ) : !hasTurns ? (
            <AgentEmpty onPick={onPick} onOpenDocs={onOpenDocs} />
          ) : (
            turns.map((t, i) => (
              <AgentTurn
                key={t.key}
                turn={t}
                index={i}
                proposals={proposals}
                proposalsLoading={proposalsLoading}
                running={running && t.key === last?.key}
                timeLabel={t.userAt ? dayjs(t.userAt).format("HH:mm") : undefined}
                sessionId={sessionId}
                onRetry={onRetry}
                onOpenSettings={onOpenSettings}
                onAnswerAsk={onAnswerAsk}
              />
            ))
          )}

          {/* 孤儿卡只在这个标签页没有实时轮时出现：有实时轮时它的 ask 段已经在
              turns 里，再渲染一张就会重复。 */}
          {orphanAsk && !running ? (
            <AgentAskCard
              ask={{
                kind: "ask",
                askId: orphanAsk.id,
                question: orphanAsk.ask.question,
                why: orphanAsk.ask.why,
                options: orphanAsk.ask.options,
                multi: orphanAsk.ask.multi,
                allowFree: orphanAsk.ask.allow_free_text,
                status: "pending",
                expiresIn: orphanAsk.ask.expires_in,
              }}
              sessionId={sessionId}
              onSettled={(status, answer) => onAnswerAsk(orphanAsk.id, status, answer)}
            />
          ) : null}
        </div>
      </div>

      {hasNew && !stick ? (
        <button type="button" className="agent-jump" onClick={() => scrollToBottom(true)}>
          <Icon name="chevronDown" size={14} />
          有新内容
        </button>
      ) : null}
    </div>
  );
}
