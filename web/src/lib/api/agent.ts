import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ModelTag } from "@/types";
import { apiClient } from "@/lib/apiClient";
import { streamSSE } from "@/lib/sse";

/**
 * AI 编排（/admin/agent）的接口层。
 *
 * 一条铁律：**这里没有任何"直接改渠道/模型"的方法**。AI 只能产出提案，
 * 生效的唯一入口是 `applyProposal`，它要求回传 plan_hash 与 confirm。
 * 前端少一个写方法，就少一条绕过确认的路径。
 */

export type ProposalStatus =
  | "draft"
  | "applying"
  | "applied"
  | "rejected"
  | "failed"
  | "stale";

export interface AgentSession {
  id: number;
  title: string;
  model_id: string;
  status: number;
  last_seq: number;
  created_at: string;
  updated_at: string;
}

export interface AgentMessage {
  id: number;
  session_id: number;
  seq: number;
  role: "user" | "assistant" | "tool";
  content: string;
  parts?: AgentParts | null;
  status: "ok" | "aborted" | "failed";
  tokens_in?: number;
  tokens_out?: number;
  created_at: string;
}

/** 一段执行过程（后端 `parts.items`，见 service/agent/trace.go）。 */
export interface AgentTraceItem {
  /**
   * reason 思考 / text 中间叙述 / tool 工具 / proposal 提案 / elided 被折叠的段 /
   * error 失败原因 / ask 向运营提问
   */
  k: "reason" | "text" | "tool" | "proposal" | "elided" | "error" | "ask";
  /** 文本内容 */
  t?: string;
  /** 工具名 */
  n?: string;
  /** 工具参数摘要（截断过的 JSON 串） */
  a?: string;
  /** 工具结果摘要（截断过的 JSON 串） */
  b?: string;
  ok?: boolean;
  /** 第几步 */
  s?: number;
  /** 耗时（毫秒） */
  ms?: number;
  /** 提案号（k=proposal 时） */
  id?: number;
  /** 错误码（k=error 时） */
  c?: string;
  /** 是否可重试（k=error 时） */
  r?: boolean;
  /** 问题（k=ask 时） */
  q?: string;
  /** 为什么问（k=ask 时） */
  w?: string;
  /** 选项（k=ask 时） */
  o?: AskOption[];
  /** 是否多选（k=ask 时） */
  mu?: boolean;
  /** 是否允许补充文字（k=ask 时） */
  ft?: boolean;
  /**
   * 终态：pending / answered / timeout / expired / denied / no_time / aborted / unavailable。
   * 只有 pending 可作答；其余全部是只读留痕。
   */
  st?: string;
  /** 答了什么（结算后回写） */
  ans?: string;
  /** agent_asks 行号（作答接口用） */
  aid?: number;
}

/** 提问的一个选项：value 回传、label 显示、detail 说明代价。 */
export interface AskOption {
  value: string;
  label: string;
  detail?: string;
}

/** 一张待答卡片（SSE `ask` 事件与 `pending_ask` 同构）。 */
export interface AskView {
  question: string;
  why?: string;
  options: AskOption[];
  multi: boolean;
  allow_free_text: boolean;
  /** 剩余秒数（不是时间戳：客户端时钟不可信） */
  expires_in: number;
}

/**
 * 消息的结构化轨迹。
 *
 * `items` 是有序段（新格式）；`tools` / `proposals` 是平铺数组（老格式，也仍在写，
 * 供事后检索）。前端必须两种都认：库里既有重构前的老消息，也有新消息。
 */
export interface AgentParts {
  items?: AgentTraceItem[];
  tools?: string[];
  proposals?: number[];
}

/** 消息列表响应：`pending_ask` 是"正在等回答"的那张卡片（没在等时为 null）。 */
export interface AgentMessagesResp {
  items: AgentMessage[];
  pending_ask?: { id: number; ask: AskView } | null;
}

export interface DiffStats {
  added: number;
  changed: number;
  removed: number;
}

/**
 * 探测目标现状。
 *
 * reason 是"为什么没通过"的唯一判据（ok/missing/expired/failed/key_mismatch）：
 * 只说"需要探测"会让运营把"换了密钥"当成"忘了探测"，反复重试同一件事。
 */
export interface ProbeTarget {
  adapter_id: string;
  base_url: string;
  source: string;
  satisfied: boolean;
  reason: "ok" | "missing" | "expired" | "failed" | "key_mismatch";
  status?: number;
  message?: string;
  checked_at?: string;
  expires_at?: string;
}

export interface ProbeStatus {
  required: boolean;
  satisfied: boolean;
  targets: ProbeTarget[];
}

export interface AgentProposal {
  id: number;
  session_id: number;
  message_id: number;
  kind: "change" | "unsupported";
  status: ProposalStatus;
  plan: Record<string, unknown>;
  plan_hash: string;
  diff?: Record<string, unknown> | null;
  error?: string;
  applied_at?: string | null;
  created_at: string;
  /** 探测状态：后端算好下发（判定需要渠道现状与密钥指纹，前端两样都没有）。 */
  probe?: ProbeStatus;
}

export interface AgentSettings {
  item: {
    id: number;
    enabled: boolean;
    model_id: string;
    max_steps: number;
    /** 整轮 token 成本闸（0 = 不限） */
    max_tokens_per_run: number;
    /** 输入上下文预算（0 = 用内置默认 2,000,000） */
    context_window: number;
    /** 单次输出上限（0 = 用内置默认 8,192） */
    max_output_tokens: number;
    /** 每日成本上限（**微**，0 = 不限） */
    daily_amount_budget: number;
    dry_run_required: boolean;
  };
  available: boolean;
  reason?: string;
  hint?: string;
  /** 这一轮实际会用的模型（设置为空时是系统默认） */
  effective_model_id: string;
  /** 今日已花（**微**），展示前必须走 formatMoney */
  today_spend: number;
  /** 各字段合法区间：min/max 的唯一来源，前端不得再写一份常量 */
  limits: {
    max_steps: [number, number];
    max_tokens_per_run: [number, number];
    context_window: [number, number];
    max_output_tokens: [number, number];
  };
  /** 输入框「模型」菜单的候选（只有对话模型） */
  chat_models?: AgentChatModel[];
}

export interface AgentChatModel {
  id: string;
  display_name: string;
  vendor_name: string;
  /** 图标地址（模型自己的，缺失时后端已回落到供应商的）；空串 = 首字 */
  icon_url?: string;
  /** 供应商品牌色（首字占位底色） */
  vendor_color?: string;
  is_default: boolean;
  /** 营销标签（与模型广场同一份快照，已过滤到期项） */
  tags?: ModelTag[];
}

export interface AgentStats {
  days: number;
  runs: number;
  failed: number;
  quota_est: number;
  tokens_in: number;
  tokens_out: number;
  proposals: number;
  applied: number;
  avg_latency_ms: number;
  daily_budget: number;
  today_spend: number;
}

export interface AgentDocItem {
  id: number;
  title: string;
  source_type: string;
  source_url?: string;
  size_bytes: number;
  created_at: string;
}

export interface AgentDocLimits {
  /** 单份文档正文上限（粘贴） */
  max_bytes: number;
  /** 按链接抓取的上限 */
  max_fetch_bytes: number;
  /** 预览返回的字节数 */
  preview_bytes: number;
}

export interface AgentDocList {
  items: AgentDocItem[];
  limits?: AgentDocLimits;
}

export interface AgentDocSection {
  index: number;
  title: string;
  level: number;
  start: number;
  end: number;
}

export interface AgentDocPreview {
  id: number;
  title: string;
  size_bytes: number;
  text: string;
  sections: AgentDocSection[];
  truncated: boolean;
}

/**
 * 一轮编排里前端要处理的 11 种事件（type=ping 是 SSE 注释帧，不经这里）。
 *
 * 除 type 外的字段几乎都带 `omitempty`：后端在值为零时会**整个键都不下发**。
 * 所以这里一律声明为可选 —— 把 `partial`/`diff_stats` 声明成必填只会让代码
 * 看起来安全，实际拿到的是 undefined（强转 `as AgentEvent` 不会替你检查）。
 */
export type AgentEvent =
  | { type: "start"; run_id?: number; message_id?: number; model?: string }
  | { type: "step"; step: number }
  | { type: "reasoning"; text: string; step?: number }
  | { type: "delta"; text: string; step?: number }
  | { type: "tool_call"; call_id?: string; name: string; args_brief: string; step?: number }
  | {
      type: "tool_result";
      call_id?: string;
      name: string;
      ok: boolean;
      brief: string;
      truncated?: boolean;
      ms?: number;
      step?: number;
    }
  | {
      type: "proposal";
      proposal_id: number;
      kind: string;
      summary: string;
      status: ProposalStatus;
      diff_stats?: DiffStats;
      /** 防篡改凭据：卡片在明细到达前也要能带着它渲染 */
      plan_hash?: string;
    }
  | {
      type: "usage";
      tokens_in?: number;
      tokens_out?: number;
      /** 估算成本（微） */
      quota_est?: number;
      steps?: number;
      /** 首步 prompt_tokens：本轮真实上下文占用（不是累加值） */
      context_tokens?: number;
      /** 本轮生效窗口：与 context_tokens 一起显示"用了多少 / 有多少" */
      context_window?: number;
      /** 0 未压缩 / 1 摘要了旧轮 / 2 已并入会话摘要 */
      compact_level?: number;
      summarized_turns?: number;
      /** 上游没给 usage，用量是估算值 */
      estimated?: boolean;
    }
  | { type: "error"; code: string; message: string; retryable?: boolean }
  /** 提问：run 仍然活着并在等服务端唤醒，答完同一轮继续 */
  | { type: "ask"; ask_id: number; step?: number; ask: AskView }
  | { type: "done"; message_id?: number; partial?: boolean };

export const agentKeys = {
  sessions: ["admin", "agent", "sessions"] as const,
  messages: (id: number) => ["admin", "agent", "messages", id] as const,
  proposals: (sessionId: number) => ["admin", "agent", "proposals", sessionId] as const,
  settings: ["admin", "agent", "settings"] as const,
  stats: (days: number) => ["admin", "agent", "stats", days] as const,
  docs: ["admin", "agent", "docs"] as const,
};

/* ------------------------------ 查询 ------------------------------ */

export function useAgentSessionsQuery() {
  return useQuery({
    queryKey: agentKeys.sessions,
    queryFn: ({ signal }) =>
      apiClient.get<{ items: AgentSession[]; total: number }>("/admin/agent/sessions", {
        signal,
      }),
  });
}

export function useAgentMessagesQuery(sessionId: number) {
  return useQuery({
    queryKey: agentKeys.messages(sessionId),
    enabled: sessionId > 0,
    queryFn: ({ signal }) =>
      apiClient.get<AgentMessagesResp>(
        // limit 显式传：后端首屏取**最新** N 条（不传取最新 100，语义一致，
        // 但写死参数避免以后默认值变化时界面与模型上下文各看一段）。
        `/admin/agent/sessions/${sessionId}/messages?limit=100`,
        { signal },
      ),
  });
}

export function useAgentProposalsQuery(sessionId: number) {
  return useQuery({
    queryKey: agentKeys.proposals(sessionId),
    enabled: sessionId > 0,
    queryFn: ({ signal }) =>
      apiClient.get<{ items: AgentProposal[] }>("/admin/agent/proposals", {
        signal,
        query: { session_id: sessionId },
      }),
  });
}

export function useAgentSettingsQuery() {
  return useQuery({
    queryKey: agentKeys.settings,
    queryFn: ({ signal }) =>
      apiClient.get<AgentSettings>("/admin/agent/settings", { signal }),
  });
}

export function useAgentStatsQuery(days = 7) {
  return useQuery({
    queryKey: agentKeys.stats(days),
    queryFn: ({ signal }) =>
      apiClient.get<AgentStats>("/admin/agent/stats", { signal, query: { days } }),
  });
}

export function useAgentDocsQuery() {
  return useQuery({
    queryKey: agentKeys.docs,
    queryFn: ({ signal }) =>
      apiClient.get<AgentDocList>("/admin/agent/docs", { signal }),
  });
}

/**
 * 单份文档预览（前 8KB + 目录）。
 *
 * 懒加载：素材弹窗可能存着十几份文档，进弹窗就全量取正文等于把对话框拖慢十倍。
 * 只有展开某一份时才请求。
 */
export function useAgentDocPreviewQuery(id: number, enabled: boolean) {
  return useQuery({
    queryKey: [...agentKeys.docs, "preview", id],
    enabled: enabled && id > 0,
    queryFn: ({ signal }) =>
      apiClient.get<AgentDocPreview>(`/admin/agent/docs/${id}/preview`, { signal }),
  });
}

/**
 * 单条提案的完整内容（含 diff 明细）。
 *
 * 列表接口为了轻量不下发大对象；卡片展开"改了什么"时才取详情。
 */
export function useAgentProposalQuery(id: number, enabled: boolean) {
  return useQuery({
    queryKey: [...agentKeys.proposals(id), "detail"],
    enabled: enabled && id > 0,
    queryFn: ({ signal }) =>
      apiClient.get<{ item: AgentProposal; diff_stats: DiffStats }>(
        `/admin/agent/proposals/${id}`,
        { signal },
      ),
  });
}

/* ------------------------------ 变更 ------------------------------ */

export function useCreateAgentSessionMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: { title?: string; model_id?: string }) =>
      apiClient.post<{ item: AgentSession }>("/admin/agent/sessions", body),
    onSuccess: () => qc.invalidateQueries({ queryKey: agentKeys.sessions }),
  });
}

/**
 * 改会话属性：标题 / 归档 / 本会话使用的模型。
 *
 * model_id 是**会话级**的"这次对话用哪个模型"：传空串表示回到全站默认。
 * 与设置里的 model_id（全站默认）是两件事，别合并。
 */
export function useRenameAgentSessionMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      id,
      title,
      status,
      model_id,
    }: {
      id: number;
      title?: string;
      status?: number;
      model_id?: string;
    }) => apiClient.patch<null>(`/admin/agent/sessions/${id}`, { title, status, model_id }),
    onSuccess: () => qc.invalidateQueries({ queryKey: agentKeys.sessions }),
  });
}

export function useDeleteAgentSessionMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => apiClient.delete<null>(`/admin/agent/sessions/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: agentKeys.sessions }),
  });
}

export function useSaveAgentSettingsMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: Partial<AgentSettings["item"]>) =>
      apiClient.put<{ item: AgentSettings["item"] }>("/admin/agent/settings", body),
    onSuccess: () => qc.invalidateQueries({ queryKey: agentKeys.settings }),
  });
}

/**
 * 应用一条提案。
 *
 * `confirm` 与 `plan_hash` 都是必填：前者防误触，后者防"页面上是 A、提交的是 B"。
 * 前端把 hash 原样带回，任何一处改了提案内容都会在服务端被拒（409）。
 */
export function useApplyProposalMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      id,
      planHash,
      scope,
    }: {
      id: number;
      planHash: string;
      /** 入口作用域（新建适配器 = ["adapter_spec"]）；不传 = 不限制。 */
      scope?: readonly string[];
    }) =>
      apiClient.post<{
        proposal_id: number;
        ops: number;
        added: number;
        /** 配置已落库但文档站重算失败时的原因（不能让运营以为文档也更新了） */
        docs_error?: string;
        /**
         * 非阻断提醒（例如：新模型在当前渠道上没有可用的异步端点）。
         *
         * 后端一直在返回，前端此前**一个字段都没接** —— 运营看到"已生效"就以为万事俱备，
         * 而实际上那条绑定在第一次调用时才会失败。
         */
        warnings?: string[];
        touched_specs?: string[];
      }>(
        `/admin/agent/proposals/${id}/apply`,
        // scope 只在非空时出现：助手页的请求体与改造前逐字一致，
        // 少一个字段就少一次"这个字段是干嘛的"的疑问。
        { plan_hash: planHash, confirm: true, ...(scope?.length ? { scope } : {}) },
      ),
    onSuccess: () => {
      // 应用会改渠道/模型/规格：相关列表全部失效，否则运营看到的是旧数据。
      qc.invalidateQueries({ queryKey: ["admin"] });
    },
  });
}

export function useRollbackProposalMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) =>
      apiClient.post<{ item: AgentProposal; plan_hash: string }>(
        `/admin/agent/proposals/${id}/rollback`,
        { confirm: true },
      ),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin", "agent"] }),
  });
}

/**
 * 回答助手提出的问题。
 *
 * **不 invalidate 消息列表**：作答的后续内容仍从原来那条 SSE 连接流进来，
 * 一刷新就会把"正在流式的那一轮"顶掉。列表在 done 之后才重取（见 useAgentRun）。
 */
export function useAnswerAgentAskMutation(sessionId: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      askId,
      selected,
      freeText,
      skipped,
    }: {
      askId: number;
      selected?: string[];
      freeText?: string;
      skipped?: boolean;
    }) =>
      apiClient.post<{ id: number; status: string }>(
        `/admin/agent/asks/${askId}/answer`,
        { selected: selected ?? [], free_text: freeText ?? "", skipped: skipped ?? false },
      ),
    onSuccess: () => qc.invalidateQueries({ queryKey: agentKeys.messages(sessionId) }),
  });
}

export function useRejectProposalMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) =>
      apiClient.post<null>(`/admin/agent/proposals/${id}/reject`, {}),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["admin", "agent"] }),
  });
}

export function useCreateAgentDocMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: {
      title?: string;
      source_type: "paste" | "url";
      content?: string;
      url?: string;
    }) =>
      apiClient.post<{ item: AgentDocItem; duplicate: boolean }>("/admin/agent/docs", body),
    onSuccess: () => qc.invalidateQueries({ queryKey: agentKeys.docs }),
  });
}

export function useDeleteAgentDocMutation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => apiClient.delete<null>(`/admin/agent/docs/${id}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: agentKeys.docs }),
  });
}

/* ------------------------------ 流式一轮 ------------------------------ */

export interface SendMessageOptions {
  content: string;
  modelId?: string;
  docIds?: number[];
  signal?: AbortSignal;
  onEvent: (ev: AgentEvent) => void;
}

/**
 * 发一条消息并消费一轮编排（SSE）。
 *
 * 不用 useMutation：流不是一个"请求-响应"，
 * 它要在整个过程中持续回调；而 onEvent 抛错即中断（组件卸载/用户点停止）。
 */
export async function sendAgentMessage(
  sessionId: number,
  { content, modelId, docIds, signal, onEvent }: SendMessageOptions,
): Promise<void> {
  await streamSSE(
    `/admin/agent/sessions/${sessionId}/messages`,
    { content, model_id: modelId, doc_ids: docIds },
    {
      signal,
      onEvent: ({ type, payload }) => {
        onEvent({ type, ...payload } as AgentEvent);
      },
    },
  );
}
