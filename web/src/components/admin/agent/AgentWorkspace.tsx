import { useEffect, useMemo, useRef, useState } from "react";
import { Icon } from "@/components/common/Icon";
import {
  useAgentDocsQuery,
  useAgentMessagesQuery,
  useAgentProposalsQuery,
  useAgentSessionsQuery,
  useAgentSettingsQuery,
  useAgentStatsQuery,
  useCreateAgentSessionMutation,
  useDeleteAgentSessionMutation,
  useRenameAgentSessionMutation,
  type AgentProposal,
} from "@/lib/api/agent";
import { AgentComposer } from "./AgentComposer";
import { AgentDocsDialog } from "./AgentDocsDialog";
import { AgentModelDialog } from "./AgentModelDialog";
import { AgentRail } from "./AgentRail";
import { AgentSettingsDialog } from "./AgentSettingsDialog";
import { AgentThread } from "./AgentThread";
import { buildTurns, hasAssistantAfter } from "./turns";
import { useAgentRun } from "./useAgentRun";
import { AgentScopeProvider } from "./scope";

type DialogName = "docs" | "model" | "settings" | null;

/**
 * AI 接入助手的完整工作台。
 *
 * 布局是一体的：左侧会话栏 + 右侧对话区，中间一条通高分割线，没有页面标题栏
 * （页面 h1 视觉隐藏，给读屏与路由用）。所有配置都收在输入框的工具栏里。
 *
 * 数据流只有一条主链：messages + 实时轮 → turns → 渲染。
 * 任何"额外状态"（比如把工具调用单独存一份）都会让顺序失真。
 */
export function AgentWorkspace({
  heading = true,
  scope,
}: {
  /**
   * 是否渲染 <h1>（默认 true）。
   *
   * 弹窗形态必须传 false：宿主页面自己有一个 h1（`.a11y-only` 的「模型服务」），
   * 两个 level-1 会让 `getByRole("heading", {level:1})` 撞 strict —— 那是 layout 门禁
   * 对每个业务页的断言，不是测试写法问题。
   */
  heading?: boolean;
  /**
   * 入口作用域：从「新建适配器」进来时传 `["adapter_spec"]`。
   * 由 apply 请求带给后端做越界校验（见 agent.EnforceScope）；这里只负责传递。
   */
  scope?: readonly string[];
} = {}) {
  const sessions = useAgentSessionsQuery();
  const settings = useAgentSettingsQuery();
  const stats = useAgentStatsQuery(7);
  const docs = useAgentDocsQuery();
  const createSession = useCreateAgentSessionMutation();
  const deleteSession = useDeleteAgentSessionMutation();
  const renameSession = useRenameAgentSessionMutation();

  const [activeId, setActiveId] = useState(0);
  const [railOpen, setRailOpen] = useState(false);
  const [dialog, setDialog] = useState<DialogName>(null);
  const [input, setInput] = useState("");
  const [quoted, setQuoted] = useState<number[]>([]);
  // 发送前的最后一条消息 id：用来判断"这一轮已经落库了"，从而收掉实时轮。
  const baseRef = useRef(-1);

  const list = sessions.data?.items ?? [];
  // 未选中时默认用最新会话：运营进来最常做的是"接着上次说"。
  const currentId = activeId > 0 ? activeId : (list[0]?.id ?? 0);
  const current = list.find((s) => s.id === currentId);

  const messages = useAgentMessagesQuery(currentId);
  const proposals = useAgentProposalsQuery(currentId);
  const run = useAgentRun(currentId, {
    onSettled: () => {
      void messages.refetch();
      void proposals.refetch();
      void sessions.refetch();
      void stats.refetch();
    },
  });

  const items = messages.data?.items ?? [];
  const lastId = items.length > 0 ? items[items.length - 1].id : 0;

  const proposalMap = useMemo(() => {
    const m = new Map<number, AgentProposal>();
    for (const p of proposals.data?.items ?? []) m.set(p.id, p);
    return m;
  }, [proposals.data]);

  // 实时轮 → 落库轮的交接在**渲染层**判定，不能只靠下面的 effect：
  // effect 要等这一帧画完才跑，而这一帧里同一轮会渲染两遍 —— 一份来自消息
  // （后端落库的 assistant），一份来自还没清掉的实时轮。错误条因此会短暂出现两条，
  // 看到的就是"同一轮报错两次"。判据与 effect 共用 hasAssistantAfter，不许两处各判一套。
  const liveTurn = useMemo(() => {
    const base = baseRef.current;
    if (!run.live || run.running || base < 0) return run.live;
    return hasAssistantAfter(items, base) ? null : run.live;
  }, [items, run.live, run.running]);

  const turns = useMemo(() => buildTurns(items, liveTurn), [items, liveTurn]);

  // 落库完成后收掉实时轮的状态（不然 `running`/`error` 还挂在 setState 里，
  // 下一次发送前的"是否在跑"判断会带上上一轮的残影）。
  //
  // 判据必须是"本轮的 assistant 消息出现了"，不能是"最后一条消息 id 变大"：
  // 后端在开流前就写了用户消息，失败那轮没有 assistant 消息 —— 按 id 判会把
  // 还在展示的实时轮（思考/工具/错误条）删掉，看起来就是"报错后对话被清空"。
  useEffect(() => {
    if (!run.live || liveTurn) return;
    baseRef.current = -1;
    run.clear();
    setQuoted([]);
  }, [liveTurn, run]);

  const available = settings.data?.available ?? false;
  // `?.item?.` 而不是 `?.item.`：接口异常时 data 可能是 {}，多一层保护不会让整页白屏。
  const enabled = settings.data?.item?.enabled ?? true;
  const disabled = !available || !enabled;
  const disabledReason = !available
    ? settings.data?.hint || "还没有可用的对话模型"
    : "接入助手已在运行参数里关闭";

  const preview = messages.isLoading ? "正在加载…" : current?.title || "新对话";

  const onSend = () => {
    const text = input.trim();
    if (!text || currentId <= 0 || disabled) return;
    baseRef.current = lastId;
    setInput("");
    void run.send({ content: text, docIds: quoted.length > 0 ? quoted : undefined });
  };

  const onCreate = () => {
    createSession.mutate(
      {},
      {
        onSuccess: (res) => {
          baseRef.current = -1;
          run.clear();
          setActiveId(res.item.id);
          setRailOpen(false);
        },
      },
    );
  };

  const onSelect = (id: number) => {
    if (id === currentId) return;
    baseRef.current = -1;
    run.clear();
    setActiveId(id);
    setQuoted([]);
  };

  const onDelete = (id: number) => {
    deleteSession.mutate(id, {
      onSuccess: () => {
        if (id === currentId) {
          baseRef.current = -1;
          run.clear();
          setActiveId(0);
        }
      },
    });
  };

  const models = settings.data?.chat_models ?? [];
  const sessionModel = current?.model_id ?? "";
  const modelLabel = sessionModel
    ? models.find((m) => m.id === sessionModel)?.display_name || sessionModel
    : "跟随默认";

  const docItems = docs.data?.items ?? [];

  return (
    <AgentScopeProvider value={scope}>
      <div className="agent-ws" data-rail={railOpen ? "open" : undefined}>
        {/* 视觉上的标题栏已经移除；h1 保留给读屏与路由（每页必须有 h1 是门禁项）。
            弹窗形态关掉它：宿主页面已有 h1，两个 level-1 会撞门禁断言。 */}
        {heading ? <h1 className="a11y-only">AI 接入助手</h1> : null}

      <AgentRail
        sessions={list}
        activeId={currentId}
        loading={sessions.isLoading}
        error={sessions.error}
        creating={createSession.isPending}
        todaySpend={settings.data?.today_spend}
        budget={stats.data?.daily_budget}
        onCreate={onCreate}
        onSelect={onSelect}
        onDelete={onDelete}
        onRename={(id, title) => renameSession.mutate({ id, title })}
        onRetry={() => void sessions.refetch()}
        onOpenSettings={() => setDialog("settings")}
        open={railOpen}
        onClose={() => setRailOpen(false)}
      />

      <div className="agent-main">
        {/* ≤899px 才有：没有它，移动端就没有入口打开会话栏（导航不能靠手势） */}
        <div className="agent-mobilebar">
          <button
            type="button"
            className="agent-iconbtn"
            aria-label="打开会话列表"
            onClick={() => setRailOpen(true)}
            data-testid="agent-rail-toggle"
          >
            <Icon name="menu" size={17} />
          </button>
          <span className="agent-mobilebar__title">{preview}</span>
        </div>

        {settings.data && (!available || !enabled) ? (
          <div className="agent-banner" data-testid="agent-banner">
            <Icon name="alert" size={14} />
            <span>{disabledReason}</span>
            <button type="button" className="agent-linkbtn" onClick={() => setDialog("settings")}>
              查看设置
            </button>
          </div>
        ) : null}

        <AgentThread
          key={currentId}
          sessionId={currentId}
          turns={turns}
          proposals={proposalMap}
          proposalsLoading={proposals.isLoading}
          running={run.running}
          messagesLoading={messages.isLoading}
          messagesError={messages.isError}
          onRetryMessages={() => void messages.refetch()}
          onRetry={(text) => {
            if (!run.running) void run.send({ content: text });
          }}
          onOpenSettings={() => setDialog("settings")}
          onAnswerAsk={(id, status, answer) => {
            run.settleAsk(id, status, answer);
            // 孤儿卡不在实时轮里，作答后要靠重取消息把它换成终态。
            // 有实时轮时不能重取：那会把正在流式的那一轮顶掉。
            if (!run.running) void messages.refetch();
          }}
          orphanAsk={run.live ? null : (messages.data?.pending_ask ?? null)}
          onPick={setInput}
          onOpenDocs={() => setDialog("docs")}
        />

        <AgentComposer
          value={input}
          onChange={setInput}
          onSend={onSend}
          onStop={run.stop}
          running={run.running}
          disabled={disabled || currentId <= 0}
          disabledReason={disabledReason}
          docs={docItems}
          quoted={quoted}
          onUnquote={(id) => setQuoted((q) => q.filter((x) => x !== id))}
          modelLabel={modelLabel}
          modelId={sessionModel || settings.data?.effective_model_id || ""}
          // 上下文占用只来自实时轮：落库消息没有 run 计量，
          // 用"消息 token 之和"冒充会显示成 3–5 倍。
          contextTokens={run.live?.contextTokens}
          contextWindow={run.live?.contextWindow}
          summarizedTurns={run.live?.summarizedTurns}
          onOpenDocs={() => setDialog("docs")}
          onOpenModel={() => currentId > 0 && setDialog("model")}
          onOpenSettings={() => setDialog("settings")}
        />

        <div className="agent-mobilebar-mask" onClick={() => setRailOpen(false)} aria-hidden="true" />
      </div>

      <AgentDocsDialog
        open={dialog === "docs"}
        onClose={() => setDialog(null)}
        quoted={quoted}
        onToggleQuote={(id) =>
          setQuoted((q) => (q.includes(id) ? q.filter((x) => x !== id) : [...q, id]))
        }
      />

      <AgentModelDialog
        open={dialog === "model"}
        onClose={() => setDialog(null)}
        sessionId={currentId}
        models={models}
        current={sessionModel}
        effective={settings.data?.effective_model_id ?? ""}
      />

      <AgentSettingsDialog
        open={dialog === "settings"}
        onClose={() => setDialog(null)}
        settings={settings.data}
        stats={stats.data}
        statsFailed={stats.isError}
      />
      </div>
    </AgentScopeProvider>
  );
}
