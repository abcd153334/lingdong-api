import type { AgentMessage, AgentParts, AskOption } from "@/lib/api/agent";

/**
 * 一轮对话的段。顺序就是发生顺序 —— 这是整个界面的地基。
 *
 * 重构前的问题：正文与工具是两个平铺 state，渲染时正文在前、工具在后，
 * "说一句 → 查一次 → 再说"的时间线被抹平了。
 */
export type Segment =
  | { kind: "reason"; text: string }
  | { kind: "text"; text: string }
  | {
      kind: "tool";
      key: string;
      name: string;
      args?: string;
      brief?: string;
      ok?: boolean;
      ms?: number;
      step?: number;
    }
  | {
      kind: "proposal";
      proposalId: number;
      /** 流式事件里带来的骨架：卡片不必等提案列表刷新就能渲染 */
      preview?: {
        kind: string;
        summary: string;
        status: string;
        planHash: string;
        diffStats?: { added: number; changed: number; removed: number };
      };
    }
  /** 失败原因。它是错误的**唯一**形态：实时轮（SSE error）与落库轮（parts.items）
   *  产出同一种段，因此刷新前后渲染一致，重试按钮也不会因刷新消失。 */
  | { kind: "error"; code: string; message: string; retryable: boolean }
  /**
   * 向运营提问。status 是终态：只有 pending 可作答，其余只读留痕。
   * 它与 error 段同级同管：实时轮（SSE ask）与落库轮（parts.items k=ask）同构。
   */
  | {
      kind: "ask";
      askId: number;
      question: string;
      why?: string;
      options: AskOption[];
      multi: boolean;
      allowFree: boolean;
      status: string;
      answer?: string;
      /** 剩余秒数（仅实时轮有；落库轮的 ask 已经结算，倒计时无意义） */
      expiresIn?: number;
    }
  | { kind: "elided"; count: number };

/**
 * ask_user 工具本身不单独出卡：它的内容全在 ask 段里。
 *
 * 不过滤的话，界面上会出现"工具卡：向运营提问（已返回）"紧跟"提问卡"两张，
 * 而前者什么信息都没有 —— 这正是重构前"工具卡堆在正文末尾"的同一种冗余。
 */
export const ASK_TOOL = "ask_user";

export type RunStatus = "running" | "ok" | "aborted" | "failed";

export interface TurnMeta {
  /** 本轮实际使用的对话模型（来自 start 事件） */
  model?: string;
  steps?: number;
  tools?: number;
  quota?: number;
  tokensIn?: number;
  tokensOut?: number;
  /** 本轮首步的上下文占用（token）；只在实时轮有（落库轮不存 run 计量） */
  contextTokens?: number;
  /** 本轮生效窗口；与 contextTokens 一起显示"用了多少 / 有多少" */
  contextWindow?: number;
  /** 0 未压缩 / 1 摘要旧轮 / 2 并入会话摘要 */
  compactLevel?: number;
  summarizedTurns?: number;
}

export interface Turn {
  key: string;
  /** 用户消息 id（提案按它归属到轮次） */
  userId?: number;
  userText: string;
  userAt?: string;
  segments: Segment[];
  answer: string;
  status: RunStatus;
  meta: TurnMeta;
  /** 实时轮（还没落库）：时间信息来自本地时钟 */
  live: boolean;
  startedAt?: number;
  endedAt?: number;
}

/** 实时一轮的状态（useAgentRun 产出，turns 只消费）。 */
export interface LiveRun {
  model?: string;
  segments: Segment[];
  answer: string;
  status: RunStatus;
  userText: string;
  startedAt: number;
  endedAt?: number;
  steps: number;
  tools: number;
  quota: number;
  tokensIn: number;
  tokensOut: number;
  /** 首步 prompt_tokens：真实上下文占用（不是跨步累加值） */
  contextTokens?: number;
  /** 本轮生效窗口 */
  contextWindow?: number;
  /** 压缩级别（0/1/2） */
  compactLevel?: number;
  /** 被降级成摘要的轮数 */
  summarizedTurns?: number;
}

/**
 * 持久化的 parts → 段。
 *
 * 两种格式都要认：
 *  - 新格式 `items`：有序段，含思考/叙述/工具/提案；
 *  - 旧格式只有 `tools[]` + `proposals[]`：降级成"一轮调了这些工具"，
 *    过程顺序不可知，但不能因此不显示（老会话打开就是空白更糟）。
 */
export function segmentsFromParts(parts?: AgentParts | null): Segment[] {
  if (!parts) return [];
  const out: Segment[] = [];
  const seenProposal = new Set<number>();
  if (Array.isArray(parts.items)) {
    parts.items.forEach((it, i) => {
      switch (it.k) {
        case "reason":
          if (it.t) out.push({ kind: "reason", text: it.t });
          break;
        case "text":
          if (it.t) out.push({ kind: "text", text: it.t });
          break;
        case "tool":
          if (it.n === ASK_TOOL) break;
          out.push({
            kind: "tool",
            key: `t${i}`,
            name: it.n ?? "",
            args: it.a,
            brief: it.b,
            ok: it.ok,
            ms: it.ms,
            step: it.s,
          });
          break;
        case "proposal":
          if (typeof it.id === "number") {
            seenProposal.add(it.id);
            out.push({ kind: "proposal", proposalId: it.id });
          }
          break;
        case "elided":
          out.push({ kind: "elided", count: it.s ?? 0 });
          break;
        case "ask":
          // 提问也走有序段：它出现在"第几步"，决定了后续内容的因果。
          if (it.q || typeof it.aid === "number") {
            out.push({
              kind: "ask",
              askId: it.aid ?? 0,
              question: it.q ?? "",
              why: it.w,
              options: it.o ?? [],
              multi: it.mu === true,
              allowFree: it.ft !== false,
              status: it.st ?? "pending",
              answer: it.ans,
            });
          }
          break;
        case "error":
          // 失败原因也走有序段：它可能出现在任意位置（例如第 3 步之后上游挂了），
          // 单独存成消息字段就会丢掉"在哪一步失败的"。
          if (it.t || it.c) {
            out.push({
              kind: "error",
              code: it.c ?? "",
              message: it.t ?? "",
              retryable: it.r === true,
            });
          }
          break;
        default:
          break;
      }
    });
  }
  // 老格式：只有工具名，没有参数与结果。给一个"完成"的灰色状态，不假装成功。
  // 放在提案兜底之前：先"调了什么"，再"产出什么"，与人读记录的顺序一致。
  if (!Array.isArray(parts.items) && (parts.tools?.length ?? 0) > 0) {
    parts.tools!.forEach((name, i) => {
      out.push({ kind: "tool", key: `legacy-${i}`, name, ms: undefined });
    });
  }
  // 提案兜底：items 里漏了（老数据 / 只写了 proposals 数组）也要能看到卡。
  for (const pid of parts.proposals ?? []) {
    if (!seenProposal.has(pid)) out.push({ kind: "proposal", proposalId: pid });
  }
  return out;
}

/**
 * 收尾时把仍是 pending 的提问段结算成终态。
 *
 * 为什么必须做：run 结束后那张卡已经没人等了（后端在超时/断线时会把行置 expanded）。
 * 不结算的话它会一直显示"剩余 0s"且按钮可点，点下去得到 410 ——
 * 一个永远点不动又永远在的按钮比没有按钮更糟。
 */
export function settlePendingAsks(segments: Segment[], status: string): Segment[] {
  let dirty = false;
  const out = segments.map((s) => {
    if (s.kind === "ask" && s.status === "pending") {
      dirty = true;
      return { ...s, status };
    }
    return s;
  });
  return dirty ? out : segments;
}

/** 段 → 渲染块：连续的工具合并成一组（视觉上一张卡，不是十张）。 */
export type Block =
  | { type: "reason"; key: string; text: string }
  | { type: "text"; key: string; text: string }
  | { type: "tools"; key: string; items: Extract<Segment, { kind: "tool" }>[] }
  | {
      type: "proposal";
      key: string;
      proposalId: number;
      preview?: Extract<Segment, { kind: "proposal" }>["preview"];
    }
  | { type: "error"; key: string; code: string; message: string; retryable: boolean }
  | { type: "ask"; key: string; ask: Extract<Segment, { kind: "ask" }> }
  | { type: "elided"; key: string; count: number };

export function toBlocks(segments: Segment[]): Block[] {
  const blocks: Block[] = [];
  let buf: Extract<Segment, { kind: "tool" }>[] = [];
  const flush = () => {
    if (buf.length === 0) return;
    blocks.push({ type: "tools", key: `g-${buf[0].key}`, items: buf });
    buf = [];
  };
  segments.forEach((s, i) => {
    if (s.kind === "tool") {
      buf.push(s);
      return;
    }
    flush();
    switch (s.kind) {
      case "reason":
        blocks.push({ type: "reason", key: `r${i}`, text: s.text });
        break;
      case "text":
        blocks.push({ type: "text", key: `x${i}`, text: s.text });
        break;
      case "proposal":
        blocks.push({
          type: "proposal",
          key: `p${s.proposalId}`,
          proposalId: s.proposalId,
          preview: s.preview,
        });
        break;
      case "elided":
        blocks.push({ type: "elided", key: `e${i}`, count: s.count });
        break;
      case "ask":
        blocks.push({ type: "ask", key: `ak${s.askId || i}`, ask: s });
        break;
      case "error":
        // 漏掉这个分支 = 落库轮的错误条被 default 静默吞掉，刷新后"错误消失"。
        blocks.push({
          type: "error",
          key: `er${i}`,
          code: s.code,
          message: s.message,
          retryable: s.retryable,
        });
        break;
    }
  });
  flush();
  return blocks;
}

function metaOf(segments: Segment[], base?: TurnMeta): TurnMeta {
  const tools = segments.filter((s) => s.kind === "tool").length;
  let steps = 0;
  for (const s of segments) {
    if (s.kind === "tool" && typeof s.step === "number") steps = Math.max(steps, s.step);
  }
  return { ...base, tools, steps: steps || undefined };
}

/**
 * 本轮的 assistant 消息是否已经落库。
 *
 * 判据必须落在 **assistant** 上，不能只看"最后一条消息 id 变大"：
 * 后端在开流之前就写了用户消息，失败时那一轮没有 assistant 消息 ——
 * 若按 id 判，界面会把还在展示的实时轮（含思考/工具/错误）当成"已落库"删掉。
 */
export function hasAssistantAfter(items: AgentMessage[], baseId: number): boolean {
  return items.some((m) => m.role === "assistant" && m.id > baseId);
}

/**
 * 消息 + 实时轮 → 轮次列表。
 *
 * 归属规则：`user` 开新轮，紧随其后的 `assistant` 收进同一轮；
 * 提案按 `message_id`（后端写的是**用户消息 id**）挂到对应轮。
 */
export function buildTurns(messages: AgentMessage[], live?: LiveRun | null): Turn[] {
  const turns: Turn[] = [];
  for (const m of messages) {
    if (m.role === "user") {
      turns.push({
        key: `u${m.id}`,
        userId: m.id,
        userText: m.content,
        userAt: m.created_at,
        segments: [],
        answer: "",
        status: "ok",
        meta: {},
        live: false,
      });
      continue;
    }
    if (m.role !== "assistant") continue;
    let cur = turns[turns.length - 1];
    // 前一轮已有答复 = 这是一条孤立/续写的助手消息，另开一轮而不是覆盖
    // （覆盖会静默丢掉上一条答复，而它可能是唯一有结论的那条）。
    if (!cur || cur.answer || cur.segments.length > 0) {
      cur = {
        key: `a${m.id}`,
        userText: "",
        segments: [],
        answer: "",
        status: "ok",
        meta: {},
        live: false,
      };
      turns.push(cur);
    }
    // 落库轮不可能是"还在等回答"：run 结束才会落库。
    // 库里仍是 pending 的 ask 段说明那一轮异常结束（行已被后端置 expired），
    // 这里统一按 expired 呈现，避免出现一张点不动的可答卡片。
    cur.segments = settlePendingAsks(segmentsFromParts(m.parts), "expired");
    cur.answer = m.content;
    cur.status = m.status === "aborted" ? "aborted" : m.status === "failed" ? "failed" : "ok";
    cur.meta = metaOf(cur.segments, {
      tokensIn: m.tokens_in || undefined,
      tokensOut: m.tokens_out || undefined,
    });
  }
  if (live && (live.userText || live.segments.length > 0 || live.answer)) {
    // 后端在开流前就写了用户消息；若失败那轮没有 assistant 消息，界面会同时有
    // "落库的用户气泡"与"实时轮的用户气泡" —— 同一句话出现两遍。
    // 末尾这种"只有用户消息、没有答复"的轮，就是实时轮本身，合并掉。
    const tail = turns[turns.length - 1];
    if (
      tail &&
      !tail.live &&
      tail.userText === live.userText &&
      tail.segments.length === 0 &&
      !tail.answer
    ) {
      turns.pop();
    }
    turns.push({
      key: "live",
      userText: live.userText,
      segments: live.segments,
      answer: live.answer,
      status: live.status,
      live: true,
      startedAt: live.startedAt,
      endedAt: live.endedAt,
      meta: {
        model: live.model || undefined,
        steps: live.steps || undefined,
        tools: live.tools || undefined,
        quota: live.quota || undefined,
        tokensIn: live.tokensIn || undefined,
        tokensOut: live.tokensOut || undefined,
      },
    });
  }
  return turns;
}
