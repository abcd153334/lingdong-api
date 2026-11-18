import { describe, expect, it } from "vitest";
import type { AgentMessage } from "@/lib/api/agent";
import {
  buildTurns,
  hasAssistantAfter,
  segmentsFromParts,
  toBlocks,
  type LiveRun,
} from "@/components/admin/agent/turns";

function msg(partial: Partial<AgentMessage> & { id: number; role: AgentMessage["role"]; content: string }): AgentMessage {
  return {
    session_id: 7,
    seq: partial.id,
    parts: null,
    status: "ok",
    created_at: "2026-10-25T10:00:00Z",
    ...partial,
  };
}

describe("segmentsFromParts", () => {
  it("按下发顺序还原思考 / 叙述 / 工具 / 提案", () => {
    const segs = segmentsFromParts({
      items: [
        { k: "reason", t: "先确认端点" },
        { k: "text", t: "我查一下" },
        { k: "tool", n: "schema_get", a: '{"section":"all"}', b: '{"ok":true}', ok: true, s: 1, ms: 12 },
        { k: "proposal", id: 11 },
      ],
      proposals: [11],
    });
    expect(segs.map((s) => s.kind)).toEqual(["reason", "text", "tool", "proposal"]);
    const tool = segs[2] as Extract<(typeof segs)[number], { kind: "tool" }>;
    expect(tool.name).toBe("schema_get");
    expect(tool.ok).toBe(true);
    expect(tool.step).toBe(1);
  });

  it("items 里已有的提案不重复追加，缺的补在末尾", () => {
    const segs = segmentsFromParts({ items: [{ k: "proposal", id: 11 }], proposals: [11, 12] });
    expect(segs.filter((s) => s.kind === "proposal")).toHaveLength(2);
    expect(segs.filter((s) => s.kind === "proposal").map((s) => (s as { proposalId: number }).proposalId)).toEqual([11, 12]);
  });

  it("老消息（只有 tools[]）降级成工具段而不是空白", () => {
    const segs = segmentsFromParts({ tools: ["schema_get", "docs_read"], proposals: [3] });
    expect(segs.filter((s) => s.kind === "tool")).toHaveLength(2);
    expect(segs[segs.length - 1]).toEqual({ kind: "proposal", proposalId: 3 });
  });

  it("parts 为空时返回空数组（不抛错）", () => {
    expect(segmentsFromParts(null)).toEqual([]);
    expect(segmentsFromParts({})).toEqual([]);
  });

  it("error 段还原出错误码与可重试标记（刷新后错误条与重试按钮都靠它）", () => {
    const segs = segmentsFromParts({
      items: [
        { k: "text", t: "我读一下文档" },
        { k: "error", t: "上游超时", c: "upstream_error", r: true },
      ],
    });
    expect(segs).toHaveLength(2);
    expect(segs[1]).toEqual({
      kind: "error",
      code: "upstream_error",
      message: "上游超时",
      retryable: true,
    });
  });
});

describe("toBlocks", () => {
  it("连续工具合并成一组，文本把组切开", () => {
    const blocks = toBlocks([
      { kind: "tool", key: "a", name: "schema_get" },
      { kind: "tool", key: "b", name: "channel_list" },
      { kind: "text", text: "查完了" },
      { kind: "tool", key: "c", name: "docs_read" },
    ]);
    expect(blocks.map((b) => b.type)).toEqual(["tools", "text", "tools"]);
    const first = blocks[0] as Extract<(typeof blocks)[number], { type: "tools" }>;
    expect(first.items).toHaveLength(2);
  });

  it("error 段必须成为块（漏掉分支会让落库轮的错误条被静默丢弃）", () => {
    const blocks = toBlocks([
      { kind: "error", code: "upstream_error", message: "上游超时", retryable: true },
    ]);
    expect(blocks).toHaveLength(1);
    expect(blocks[0]).toMatchObject({
      type: "error",
      code: "upstream_error",
      message: "上游超时",
      retryable: true,
    });
  });
});

describe("hasAssistantAfter", () => {
  const items = [
    msg({ id: 1, role: "user", content: "x" }),
    msg({ id: 2, role: "user", content: "y" }),
  ];

  it("只有用户消息更大时不算收敛（失败轮的真实形态）", () => {
    expect(hasAssistantAfter(items, 1)).toBe(false);
  });

  it("出现更大的 assistant 才算出收敛", () => {
    expect(hasAssistantAfter([...items, msg({ id: 3, role: "assistant", content: "" })], 1)).toBe(true);
  });

  it("更早的 assistant 不算（不能拿历史消息冒充本轮）", () => {
    const withOld = [msg({ id: 1, role: "assistant", content: "旧答复" }), ...items];
    expect(hasAssistantAfter(withOld, 2)).toBe(false);
  });

  it("空列表 / base=0 的语义", () => {
    expect(hasAssistantAfter([], 0)).toBe(false);
    expect(hasAssistantAfter([msg({ id: 5, role: "assistant", content: "a" })], 0)).toBe(true);
  });
});

describe("buildTurns", () => {
  it("user 开新轮，紧随的 assistant 收进同一轮", () => {
    const turns = buildTurns([
      msg({ id: 1, role: "user", content: "接入可灵" }),
      msg({ id: 2, role: "assistant", content: "好的", parts: { items: [{ k: "text", t: "先看文档" }] } }),
    ]);
    expect(turns).toHaveLength(1);
    expect(turns[0].userText).toBe("接入可灵");
    expect(turns[0].userId).toBe(1);
    expect(turns[0].answer).toBe("好的");
    expect(turns[0].segments).toHaveLength(1);
  });

  it("中断的助手消息保留内容并标记 aborted", () => {
    const turns = buildTurns([
      msg({ id: 1, role: "user", content: "x" }),
      msg({ id: 2, role: "assistant", content: "说到一半", status: "aborted" }),
    ]);
    expect(turns[0].status).toBe("aborted");
    expect(turns[0].answer).toBe("说到一半");
  });

  it("孤立 assistant（没有前序 user）单独成轮，不覆盖上一轮", () => {
    const turns = buildTurns([
      msg({ id: 1, role: "user", content: "第一句" }),
      msg({ id: 2, role: "assistant", content: "第一答" }),
      msg({ id: 3, role: "assistant", content: "补一条" }),
    ]);
    expect(turns).toHaveLength(2);
    expect(turns[1].answer).toBe("补一条");
  });

  it("实时轮追加在末尾，且与落库轮结构一致", () => {
    const live: LiveRun = {
      segments: [{ kind: "tool", key: "t", name: "schema_get", ok: undefined }],
      answer: "",
      status: "running",
      userText: "再看看模型",
      startedAt: Date.now(),
      steps: 1,
      tools: 1,
      quota: 0,
      tokensIn: 0,
      tokensOut: 0,
    };
    const turns = buildTurns([msg({ id: 1, role: "user", content: "x" }), msg({ id: 2, role: "assistant", content: "y" })], live);
    expect(turns).toHaveLength(2);
    expect(turns[1].live).toBe(true);
    expect(turns[1].meta.tools).toBe(1);
    expect(turns[1].meta.steps).toBe(1);
  });
});

describe("toBlocks", () => {
  it("提案段的 preview 会原样带到块上（否则骨架渲染拿不到摘要）", () => {
    const segs = segmentsFromParts({
      items: [{ k: "proposal", id: 31 }],
      proposals: [31],
    });
    // 模拟实时轮：segments 来自 SSE，带 preview 骨架。
    segs[0] = {
      kind: "proposal",
      proposalId: 31,
      preview: { kind: "change", summary: "接入A", status: "draft", planHash: "h", diffStats: { added: 1, changed: 0, removed: 0 } },
    };
    const blocks = toBlocks(segs);
    expect(blocks[0]).toMatchObject({ type: "proposal", proposalId: 31, preview: { summary: "接入A" } });
  });
});

describe("ask 段", () => {
  it("parts.items 里的 k=ask 还原成提问段（含选项与终态）", () => {
    const segs = segmentsFromParts({
      items: [
        {
          k: "ask",
          q: "按次还是按规格？",
          w: "决定计费方式",
          o: [{ value: "per_call", label: "按次" }],
          mu: false,
          ft: true,
          st: "answered",
          ans: "按次",
          aid: 9,
        },
      ],
    });
    expect(segs[0]).toMatchObject({
      kind: "ask",
      askId: 9,
      question: "按次还是按规格？",
      why: "决定计费方式",
      multi: false,
      allowFree: true,
      status: "answered",
      answer: "按次",
    });
  });

  it("ask_user 的工具段被过滤：提问卡已经承载了它的全部信息", () => {
    const segs = segmentsFromParts({
      items: [
        { k: "tool", n: "ask_user", ok: true },
        { k: "ask", q: "问一句", o: [], aid: 3, st: "pending" },
      ],
    });
    expect(segs.filter((s) => s.kind === "tool")).toHaveLength(0);
    expect(segs).toHaveLength(1);
  });

  it("toBlocks 产出 ask 块（漏了它就等于刷新后提问消失）", () => {
    const segs = segmentsFromParts({
      items: [{ k: "ask", q: "要不要新建供应商？", o: [], aid: 5, st: "pending" }],
    });
    const blocks = toBlocks(segs);
    expect(blocks[0]).toMatchObject({ type: "ask", ask: { askId: 5, question: "要不要新建供应商？" } });
  });

  it("缺 aid 的畸形 ask 段也要能渲染（aid=0 时由 key 兜底）", () => {
    const segs = segmentsFromParts({ items: [{ k: "ask", q: "没有 id 的问题", st: "expired" }] });
    expect(segs[0]).toMatchObject({ kind: "ask", askId: 0, status: "expired" });
  });
});
