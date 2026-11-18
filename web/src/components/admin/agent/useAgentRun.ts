import { useCallback, useEffect, useReducer, useRef } from "react";
import { ApiError } from "@/lib/apiClient";
import { sendAgentMessage, type AgentEvent } from "@/lib/api/agent";
import type { AskView } from "@/lib/api/agent";
import { ASK_TOOL, settlePendingAsks, type LiveRun, type RunStatus, type Segment } from "./turns";

/**
 * 一轮编排的前端状态机：SSE 事件 → 有序段。
 *
 * 三条不能丢的性质：
 *  1. **顺序**：思考 / 叙述 / 工具都进同一个 `segments` 数组，谁先发生谁在前；
 *     拆成两个 state 就再也还原不出"说一句 → 查一次 → 再说"。
 *  2. **停止保留内容**：abort 只是不再接收事件，已生成的段必须留在界面上
 *     （后端的 aborted 消息同样保留正文）。
 *  3. **首尾一致**：实时轮与落库轮用同一套 Segment 结构，刷新前后长得一样。
 */

export type RunPhase = "idle" | "running" | "done" | "stopped" | "error";

interface State {
  phase: RunPhase;
  /** 本轮实际使用的模型（后端 start 事件下发） */
  model: string;
  userText: string;
  segments: Segment[];
  answer: string;
  status: RunStatus;
  startedAt: number;
  endedAt?: number;
  step: number;
  tools: number;
  quota: number;
  tokensIn: number;
  tokensOut: number;
  /** 首步 prompt_tokens：真实上下文占用（跨步累加值会放大 3–5 倍） */
  contextTokens: number;
  contextWindow: number;
  compactLevel: number;
  summarizedTurns: number;
  error: { code: string; message: string; retryable: boolean } | null;
}

const EMPTY: State = {
  phase: "idle",
  model: "",
  userText: "",
  segments: [],
  answer: "",
  status: "running",
  startedAt: 0,
  step: 0,
  tools: 0,
  quota: 0,
  tokensIn: 0,
  tokensOut: 0,
  contextTokens: 0,
  contextWindow: 0,
  compactLevel: 0,
  summarizedTurns: 0,
  error: null,
};

type Action =
  | { type: "begin"; userText: string; at: number }
  | { type: "start"; model: string }
  | { type: "step"; step: number }
  | { type: "reason"; text: string }
  | { type: "delta"; text: string }
  | { type: "tool_call"; key: string; name: string; args?: string; step?: number }
  | { type: "tool_result"; key: string; name: string; ok: boolean; brief?: string; ms?: number }
  | {
      type: "proposal";
      id: number;
      kind?: string;
      summary?: string;
      status?: string;
      planHash?: string;
      diffStats?: { added: number; changed: number; removed: number };
    }
  | {
      type: "usage";
      steps: number;
      quota: number;
      tokensIn: number;
      tokensOut: number;
      contextTokens: number;
      contextWindow: number;
      compactLevel: number;
      summarizedTurns: number;
    }
  | { type: "fail"; code: string; message: string; retryable: boolean }
  | { type: "ask"; askId: number; ask: AskView }
  | { type: "ask_settle"; askId: number; status: string; answer?: string }
  | { type: "finish"; partial: boolean }
  | { type: "stop" }
  | { type: "clear" };

/** 末尾同类型的文本段合并（否则每帧一个 <p>）。 */
function pushText(segments: Segment[], kind: "reason" | "text", text: string): Segment[] {
  if (!text) return segments;
  const last = segments[segments.length - 1];
  if (last && last.kind === kind) {
    const merged: Segment = { kind, text: last.text + text };
    return [...segments.slice(0, -1), merged];
  }
  return [...segments, { kind, text }];
}

function reduce(state: State, action: Action): State {
  switch (action.type) {
    case "begin":
      return {
        ...EMPTY,
        phase: "running",
        status: "running",
        userText: action.userText,
        startedAt: action.at,
      };
    case "start":
      return action.model ? { ...state, model: action.model } : state;
    case "step":
      return { ...state, step: Math.max(state.step, action.step) };
    case "reason":
      return { ...state, segments: pushText(state.segments, "reason", action.text) };
    case "delta":
      return { ...state, segments: pushText(state.segments, "text", action.text), answer: state.answer + action.text };
    case "tool_call":
      // ask_user 不单独出工具卡：提问卡已经承载了它的全部内容（问题/选项/答复）。
      // 仍然计入 tools 计数，否则"调了几次工具"会少一个。
      if (action.name === ASK_TOOL) return { ...state, tools: state.tools + 1 };
      return {
        ...state,
        tools: state.tools + 1,
        segments: [
          ...state.segments,
          { kind: "tool", key: action.key, name: action.name, args: action.args, step: action.step },
        ],
      };
    case "tool_result": {
      if (action.name === ASK_TOOL) return state;
      // 先用 call_id 精确配对；老上游不给 id 时退回"同名且未完成"的最后一个。
      let idx = -1;
      for (let i = state.segments.length - 1; i >= 0; i--) {
        const s = state.segments[i];
        if (s.kind !== "tool" || s.ok !== undefined) continue;
        if (action.key && s.key === action.key) {
          idx = i;
          break;
        }
        if (!action.key && s.name === action.name) {
          idx = i;
          break;
        }
      }
      if (idx < 0) return state;
      const next = [...state.segments];
      const s = next[idx] as Extract<Segment, { kind: "tool" }>;
      next[idx] = { ...s, ok: action.ok, brief: action.brief, ms: action.ms };
      return { ...state, segments: next };
    }
    case "proposal": {
      const preview =
        action.kind || action.summary
          ? {
              kind: action.kind ?? "change",
              summary: action.summary ?? "",
              status: action.status ?? "draft",
              planHash: action.planHash ?? "",
              diffStats: action.diffStats,
            }
          : undefined;
      const hit = state.segments.findIndex(
        (s) => s.kind === "proposal" && s.proposalId === action.id,
      );
      if (hit >= 0) {
        // 同一提案可能先来骨架、再来一次带完整信息的（resume）：以最新事件为准。
        const next = [...state.segments];
        next[hit] = { kind: "proposal", proposalId: action.id, preview };
        return { ...state, segments: next };
      }
      return {
        ...state,
        segments: [...state.segments, { kind: "proposal", proposalId: action.id, preview }],
      };
    }
    case "ask": {
      // 同一 askId 重复到达（重连补发）只保留一张卡，并且以最新状态为准。
      const hit = state.segments.findIndex((s) => s.kind === "ask" && s.askId === action.askId);
      const seg: Segment = {
        kind: "ask",
        askId: action.askId,
        question: action.ask.question,
        why: action.ask.why,
        options: action.ask.options,
        multi: action.ask.multi,
        allowFree: action.ask.allow_free_text,
        status: "pending",
        expiresIn: action.ask.expires_in,
      };
      if (hit >= 0) {
        const next = [...state.segments];
        next[hit] = seg;
        return { ...state, segments: next };
      }
      return { ...state, segments: [...state.segments, seg] };
    }
    case "ask_settle": {
      const hit = state.segments.findIndex((s) => s.kind === "ask" && s.askId === action.askId);
      if (hit < 0) return state;
      const next = [...state.segments];
      const cur = next[hit] as Extract<Segment, { kind: "ask" }>;
      next[hit] = { ...cur, status: action.status, answer: action.answer ?? cur.answer };
      return { ...state, segments: next };
    }
    case "usage":
      return {
        ...state,
        step: Math.max(state.step, action.steps),
        quota: action.quota,
        tokensIn: action.tokensIn,
        tokensOut: action.tokensOut,
        contextTokens: action.contextTokens,
        contextWindow: action.contextWindow,
        compactLevel: action.compactLevel,
        summarizedTurns: action.summarizedTurns,
      };
    case "fail": {
      // 错误也是一种段：实时轮与落库轮（parts.items 的 k=error）渲染同构，
      // 否则刷新后错误条消失、重试按钮也不见了。
      // 幂等：同一错误重复到达（流内 error + 兜底）只保留一条。
      const seg: Segment = {
        kind: "error",
        code: action.code,
        message: action.message,
        retryable: action.retryable,
      };
      const last = state.segments[state.segments.length - 1];
      const segments =
        last && last.kind === "error" && last.code === action.code
          ? [...state.segments.slice(0, -1), seg]
          : [...state.segments, seg];
      return {
        ...state,
        phase: "error",
        status: "failed",
        // 这一轮已经结束：还挂着的提问没人会接了，一律结算掉。
        segments: settlePendingAsks(segments, "expired"),
        error: { code: action.code, message: action.message, retryable: action.retryable },
        endedAt: state.endedAt ?? Date.now(),
      };
    }
    case "finish":
      // 已经因 error 收尾的，不要把 phase 改回 done（否则错误框消失、界面像成功了）。
      if (state.phase === "error" || state.phase === "stopped") {
        return { ...state, endedAt: state.endedAt ?? Date.now() };
      }
      return {
        ...state,
        phase: "done",
        status: action.partial ? "aborted" : "ok",
        // 收尾即结算：这一轮结束后还挂着的提问（超时/断线）没人会接。
        segments: settlePendingAsks(state.segments, "expired"),
        endedAt: Date.now(),
      };
    case "stop":
      return {
        ...state,
        phase: "stopped",
        status: "aborted",
        segments: settlePendingAsks(state.segments, "aborted"),
        endedAt: Date.now(),
      };
    case "clear":
      return EMPTY;
    default:
      return state;
  }
}

export interface UseAgentRunResult {
  live: LiveRun | null;
  phase: RunPhase;
  error: State["error"];
  running: boolean;
  send: (args: { content: string; docIds?: number[]; modelId?: string }) => Promise<void>;
  stop: () => void;
  clear: () => void;
  /** 把提问卡切成已答态（POST /asks/:id/answer 成功后调用）。 */
  settleAsk: (askId: number, status: string, answer?: string) => void;
}

export function useAgentRun(
  sessionId: number,
  opts: { onSettled?: () => void } = {},
): UseAgentRunResult {
  const [state, dispatch] = useReducer(reduce, EMPTY);
  const abortRef = useRef<AbortController | null>(null);
  // 是否已经收过尾（done / error / stop）。流被代理提前掐断时不会有 done，
  // 不兜底就会永远停在"进行中"——运营看着转圈，后端其实早就结束了。
  const settledRef = useRef(true);
  // 文本增量按帧合并：长回答逐字 setState 会让整棵树每字重渲染一次。
  const bufRef = useRef<{ kind: "reason" | "text"; text: string } | null>(null);
  const frameRef = useRef<number>(0);
  const onSettled = opts.onSettled;

  const flush = useCallback(() => {
    if (frameRef.current) {
      cancelAnimationFrame(frameRef.current);
      frameRef.current = 0;
    }
    const buf = bufRef.current;
    bufRef.current = null;
    if (!buf || !buf.text) return;
    dispatch({ type: buf.kind === "reason" ? "reason" : "delta", text: buf.text });
  }, []);

  const buffer = useCallback(
    (kind: "reason" | "text", text: string) => {
      const cur = bufRef.current;
      if (cur && cur.kind === kind) {
        cur.text += text;
      } else {
        flush();
        bufRef.current = { kind, text };
      }
      if (!frameRef.current) {
        frameRef.current = requestAnimationFrame(() => {
          frameRef.current = 0;
          const buf = bufRef.current;
          bufRef.current = null;
          if (buf && buf.text) {
            dispatch({ type: buf.kind === "reason" ? "reason" : "delta", text: buf.text });
          }
        });
      }
    },
    [flush],
  );

  // 切会话必须中止上一轮：否则上一轮的增量会写进新会话的界面。
  useEffect(() => {
    return () => {
      abortRef.current?.abort();
      abortRef.current = null;
    };
  }, [sessionId]);

  const send = useCallback(
    async ({ content, docIds, modelId }: { content: string; docIds?: number[]; modelId?: string }) => {
      const text = content.trim();
      if (!text || sessionId <= 0) return;
      const controller = new AbortController();
      abortRef.current = controller;
      bufRef.current = null;
      settledRef.current = false;
      dispatch({ type: "begin", userText: text, at: Date.now() });

      const handle = (ev: AgentEvent) => {
        switch (ev.type) {
          case "start":
            // 本轮实际使用的模型：会话级模型回退时，这是唯一能看出来的地方。
            dispatch({ type: "start", model: ev.model ?? "" });
            break;
          case "step":
            dispatch({ type: "step", step: ev.step });
            break;
          case "reasoning":
            buffer("reason", ev.text);
            break;
          case "delta":
            buffer("text", ev.text);
            break;
          case "tool_call":
            // 顺序敏感：先把缓冲的文本落地，工具卡才能排在这段话之后。
            flush();
            dispatch({
              type: "tool_call",
              key: ev.call_id || `${ev.name}-${ev.step ?? 0}-${Date.now()}`,
              name: ev.name,
              args: ev.args_brief,
              step: ev.step,
            });
            break;
          case "tool_result":
            flush();
            dispatch({
              type: "tool_result",
              key: ev.call_id || "",
              name: ev.name,
              ok: ev.ok,
              brief: ev.brief,
              ms: ev.ms,
            });
            break;
          case "proposal":
            flush();
            dispatch({
              type: "proposal",
              id: ev.proposal_id,
              kind: ev.kind,
              summary: ev.summary,
              status: ev.status,
              planHash: ev.plan_hash,
              diffStats: ev.diff_stats,
            });
            break;
          case "ask":
            // 提问要排在"刚说完的话"之后：先 flush，卡片才落在正确位置。
            flush();
            dispatch({ type: "ask", askId: ev.ask_id, ask: ev.ask });
            break;
          case "usage":
            flush();
            dispatch({
              type: "usage",
              // 后端对这些字段带 omitempty：零值时键直接不下发，必须显式兜底。
              steps: ev.steps ?? 0,
              quota: ev.quota_est ?? 0,
              tokensIn: ev.tokens_in ?? 0,
              tokensOut: ev.tokens_out ?? 0,
              contextTokens: ev.context_tokens ?? 0,
              contextWindow: ev.context_window ?? 0,
              compactLevel: ev.compact_level ?? 0,
              summarizedTurns: ev.summarized_turns ?? 0,
            });
            break;
          case "error":
            flush();
            settledRef.current = true;
            dispatch({
              type: "fail",
              code: ev.code,
              message: ev.message,
              retryable: ev.retryable ?? false,
            });
            break;
          case "done":
            flush();
            settledRef.current = true;
            dispatch({ type: "finish", partial: ev.partial ?? false });
            break;
          default:
            break;
        }
      };

      try {
        await sendAgentMessage(sessionId, { content: text, docIds, modelId, signal: controller.signal, onEvent: handle });
      } catch (err) {
        flush();
        settledRef.current = true;
        if (err instanceof ApiError) {
          // 409 有两种语义，用户动作完全不同：一种是"等一等"，
          // 一种是"先去回答上面那个问题"。合成一句"正在处理，请稍候"会让人干等。
          const awaiting = err.errorCode === "agent_awaiting_answer";
          dispatch({
            type: "fail",
            code: awaiting ? "agent_awaiting_answer" : err.code === 409 ? "agent_busy" : "request_failed",
            message: awaiting
              ? "助手还在等你回答上一个问题，先回答它或等它超时"
              : err.code === 409
                ? "这个会话正在处理上一条消息，请稍候"
                : err.message,
            retryable: err.code >= 500,
          });
        } else {
          // 主动中止不算错误（用户点了停止 / 切了会话）。
          dispatch({ type: "stop" });
        }
      } finally {
        flush();
        // 流结束了却没有 done（代理超时/上游半途断开）：按"本轮未完成"收尾，
        // 已生成的内容保留，人可以接着问。
        if (!settledRef.current) dispatch({ type: "finish", partial: true });
        settledRef.current = true;
        abortRef.current = null;
        onSettled?.();
      }
    },
    [buffer, flush, onSettled, sessionId],
  );

  const stop = useCallback(() => {
    abortRef.current?.abort();
    abortRef.current = null;
    settledRef.current = true;
    flush();
    dispatch({ type: "stop" });
  }, [flush]);

  /**
   * 把提问卡切成已答态（POST 成功后调用）。
   *
   * 只改本地段，不重取消息：作答之后的正文还在同一条 SSE 上往回流，
   * 一重取就会把"正在流式的那一轮"顶掉，界面闪一下再重来。
   */
  const settleAsk = useCallback((askId: number, status: string, answer?: string) => {
    dispatch({ type: "ask_settle", askId, status, answer });
  }, []);

  const clear = useCallback(() => {
    abortRef.current?.abort();
    abortRef.current = null;
    bufRef.current = null;
    dispatch({ type: "clear" });
  }, []);

  useEffect(() => {
    return () => {
      if (frameRef.current) cancelAnimationFrame(frameRef.current);
    };
  }, []);

  const live: LiveRun | null =
    state.phase === "idle"
      ? null
      : {
          model: state.model || undefined,
          segments: state.segments,
          answer: state.answer,
          status: state.status,
          userText: state.userText,
          startedAt: state.startedAt,
          endedAt: state.endedAt,
          steps: state.step,
          tools: state.tools,
          quota: state.quota,
          tokensIn: state.tokensIn,
          tokensOut: state.tokensOut,
          contextTokens: state.contextTokens || undefined,
          contextWindow: state.contextWindow || undefined,
          compactLevel: state.compactLevel || undefined,
          summarizedTurns: state.summarizedTurns || undefined,
        };

  return {
    live,
    phase: state.phase,
    error: state.error,
    running: state.phase === "running",
    send,
    stop,
    clear,
    settleAsk,
  };
}
