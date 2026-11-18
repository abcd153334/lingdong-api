import { describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import type { AgentEvent } from "@/lib/api/agent";

/**
 * 一轮编排的状态机：事件 → 有序段。
 *
 * 这里 mock 掉 SSE 客户端，只验证"事件怎么变成界面状态"这条映射 ——
 * 网络层有它自己的实现与用例。
 */
const h = vi.hoisted(() => ({
  events: [] as unknown[],
  /** 让单次 send 直接抛错（模拟 409 / 网络失败） */
  throwErr: null as null | { code: number; message: string },
}));

vi.mock("@/lib/api/agent", () => ({
  sendAgentMessage: async (
    _id: number,
    opts: { onEvent: (ev: unknown) => void },
  ) => {
    for (const ev of h.events) opts.onEvent(ev);
    if (h.throwErr) {
      const { ApiError } = await import("@/lib/apiClient");
      throw new ApiError(h.throwErr.code, h.throwErr.message);
    }
  },
}));

const { useAgentRun } = await import("@/components/admin/agent/useAgentRun");

function setEvents(events: AgentEvent[]) {
  h.events = events;
  h.throwErr = null;
}

async function run() {
  const { result } = renderHook(() => useAgentRun(7));
  await act(async () => {
    await result.current.send({ content: "看看端点" });
  });
  return result;
}

describe("useAgentRun", () => {
  it("思考与正文分流，且保持发生顺序", async () => {
    setEvents([
      { type: "start", run_id: 1, message_id: 2 },
      { type: "step", step: 1 },
      { type: "reasoning", text: "先想一下", step: 1 },
      { type: "delta", text: "我查一下。", step: 1 },
      { type: "tool_call", call_id: "c1", name: "schema_get", args_brief: "{}", step: 1 },
      { type: "tool_result", call_id: "c1", name: "schema_get", ok: true, brief: "{}", ms: 10, step: 1 },
      { type: "delta", text: "支持 chat。", step: 2 },
      {
        type: "proposal", proposal_id: 11, kind: "change", summary: "s", status: "draft",
        diff_stats: { added: 1, changed: 0, removed: 0 }, plan_hash: "abc123",
      },
      {
        type: "usage", tokens_in: 10, tokens_out: 5, quota_est: 7, steps: 2,
        // 上下文计量：落进实时轮后才能显示"用了多少 / 有多少"
        context_tokens: 42000, context_window: 2000000, compact_level: 1, summarized_turns: 3,
      },
      { type: "done", message_id: 2, partial: false },
    ]);
    const result = await run();
    const live = result.current.live!;
    expect(live.segments.map((s) => s.kind)).toEqual(["reason", "text", "tool", "text", "proposal"]);
    expect(live.status).toBe("ok");
    expect(result.current.running).toBe(false);
    expect(live.quota).toBe(7);
    expect(live.steps).toBe(2);
    // 上下文占用必须是"首步真实占用"，而不是 tokensIn 那种跨步累加值：
    // 两者一旦混用，界面会显示成 3–5 倍，"上下文满了"就无法归因。
    expect(live.contextTokens).toBe(42000);
    expect(live.contextWindow).toBe(2000000);
    expect(live.summarizedTurns).toBe(3);
  });

  it("工具结果按 call_id 配对（同名并发调用不串台）", async () => {
    setEvents([
      { type: "tool_call", call_id: "a", name: "schema_get", args_brief: "{}" },
      { type: "tool_call", call_id: "b", name: "schema_get", args_brief: "{}" },
      { type: "tool_result", call_id: "b", name: "schema_get", ok: false, brief: '{"ok":false,"error":"参数错"}' },
      { type: "tool_result", call_id: "a", name: "schema_get", ok: true, brief: "{}" },
      { type: "done", message_id: 2, partial: false },
    ]);
    const result = await run();
    const tools = result.current.live!.segments.filter((s) => s.kind === "tool") as {
      key: string;
      ok?: boolean;
    }[];
    expect(tools.find((t) => t.key === "a")?.ok).toBe(true);
    expect(tools.find((t) => t.key === "b")?.ok).toBe(false);
  });

  it("error 事件不清空已生成内容", async () => {
    setEvents([
      { type: "delta", text: "正在核对价目…" },
      { type: "error", code: "agent_budget_exceeded", message: "今日额度预算已用尽", retryable: false },
    ]);
    const result = await run();
    expect(result.current.phase).toBe("error");
    expect(result.current.error?.message).toContain("预算已用尽");
    // 已生成的内容必须留着（不能因为报错清屏）；错误本身也是一段：
    // 实时轮与落库轮（parts.items 的 k=error）渲染同构，否则刷新后错误条会消失。
    const segs = result.current.live!.segments;
    expect(segs.map((s) => s.kind)).toEqual(["text", "error"]);
  });

  it("流没有 done 就结束（代理掐断）时按未完成收尾，不会永远转圈", async () => {
    setEvents([{ type: "delta", text: "说到一半" }]);
    const result = await run();
    expect(result.current.running).toBe(false);
    expect(result.current.live!.status).toBe("aborted");
  });

  it("409 显示为「正在处理上一条消息」，而不是泛化的网络错误", async () => {
    h.events = [];
    h.throwErr = { code: 409, message: "该会话正在处理上一条消息" };
    const result = await run();
    expect(result.current.error?.code).toBe("agent_busy");
    expect(result.current.error?.message).toContain("正在处理上一条消息");
  });
});

describe("start 事件与提案骨架", () => {
  it("start 下发本轮实际模型，且能在实时轮里显示", async () => {
    setEvents([
      { type: "start", run_id: 1, message_id: 2, model: "relay-chat" },
      { type: "delta", text: "好的", step: 1 },
      { type: "done", message_id: 2 },
    ]);
    const result = await run();
    // 会话级模型回退到全站默认时，界面只能靠这一条看出"这轮用的是哪个模型"。
    expect(result.current.live?.model).toBe("relay-chat");
  });

  it("proposal 事件带摘要时立刻能渲染骨架（不必等提案列表刷新）", async () => {
    setEvents([
      { type: "start", run_id: 1, message_id: 2, model: "relay-chat" },
      {
        type: "proposal", proposal_id: 21, kind: "change", summary: "接入中转A",
        status: "draft", diff_stats: { added: 2, changed: 0, removed: 0 }, plan_hash: "h21",
      },
      { type: "done", message_id: 2 },
    ]);
    const result = await run();
    const seg = result.current.live?.segments.find((s) => s.kind === "proposal");
    expect(seg).toBeTruthy();
    expect(seg).toMatchObject({
      proposalId: 21,
      preview: { summary: "接入中转A", planHash: "h21", diffStats: { added: 2, changed: 0, removed: 0 } },
    });
  });

  it("usage/done 缺字段（omitempty）时按零值兜底，不产生 NaN", async () => {
    setEvents([
      { type: "start", run_id: 1, message_id: 2 },
      { type: "usage", steps: 1 },
      { type: "done" },
    ]);
    const result = await run();
    expect(result.current.live?.tokensOut ?? 0).toBe(0);
    expect(result.current.phase).toBe("done");
  });
});
