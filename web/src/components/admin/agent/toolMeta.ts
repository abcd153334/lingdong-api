import type { IconName } from "@/components/common/Icon";

/** 工具调用的展示元数据（名字 → 人话）。 */
export interface ToolMeta {
  label: string;
  icon: IconName;
  /** 语义分组：读现状 / 探测外网 / 生成提案（决定卡片左侧色条的色调） */
  tone: "read" | "probe" | "write";
}

/**
 * 28 个工具的中文名（与后端 ToolSpecs 一一对应）。
 *
 * 为什么不让后端下发中文名：这是**展示**问题，不是协议问题。后端一旦开始
 * 下发文案，同一句话就会有两个真相源（前端改词要发版本，后端改词要发后端），
 * 而且这些名字与 toolMeta 里的参数/结果摘要必须成套修改。
 *
 * 未登记的工具一律走 `toolMetaOf` 的兜底（显示原始名）：后端加工具时，
 * 前端只会"名字不好看"，不会白屏或漏渲染。
 */
const META: Record<string, ToolMeta> = {
  schema_get: { label: "读取平台字段手册", icon: "book", tone: "read" },
  spec_support_check: { label: "判断接入方式", icon: "shield", tone: "read" },
  vendor_list: { label: "查供应商分组", icon: "rows", tone: "read" },
  adapter_list: { label: "查可用适配器", icon: "grid", tone: "read" },
  channel_list: { label: "查渠道", icon: "rows", tone: "read" },
  channel_get: { label: "读渠道详情", icon: "eye", tone: "read" },
  model_list: { label: "查模型", icon: "rows", tone: "read" },
  model_get: { label: "读模型配置", icon: "eye", tone: "read" },
  spec_list: { label: "查声明式规格", icon: "rows", tone: "read" },
  spec_get: { label: "读规格定义", icon: "eye", tone: "read" },
  pricing_basis: { label: "读额度刻度", icon: "coin", tone: "read" },
  docs_read: { label: "读素材文档", icon: "doc", tone: "read" },
  skill_read: { label: "读接入知识包", icon: "book", tone: "read" },
  doc_tree: { label: "查文档目录", icon: "rows", tone: "read" },
  doc_node_get: { label: "读文档节点", icon: "eye", tone: "read" },
  doc_node_create: { label: "建文档节点", icon: "edit", tone: "write" },
  doc_node_update: { label: "改文档节点", icon: "edit", tone: "write" },
  doc_endpoint_get: { label: "读接口文档", icon: "eye", tone: "read" },
  doc_endpoint_create: { label: "建接口文档", icon: "edit", tone: "write" },
  doc_endpoint_update: { label: "改接口文档", icon: "edit", tone: "write" },
  doc_fields_write: { label: "写字段表", icon: "edit", tone: "write" },
  doc_check: { label: "文档体检", icon: "check", tone: "read" },
  doc_seed_diff: { label: "核对出厂文档", icon: "rows", tone: "read" },
  validate_draft: { label: "预演草稿", icon: "check", tone: "read" },
  // ask_user 的卡片由 AgentAskCard 渲染（它的工具卡被 turns 过滤掉）；
  // 这里仍要登记：工具计数与"本轮调了什么"的列表都读这张表。
  ask_user: { label: "向你提问", icon: "help", tone: "read" },
  dry_run_draft: { label: "连通性探测", icon: "zap", tone: "probe" },
  propose_change: { label: "生成变更提案", icon: "edit", tone: "write" },
  submit_unsupported: { label: "标记需写代码适配", icon: "wrench", tone: "write" },
};

export function toolMetaOf(name: string): ToolMeta {
  return META[name] ?? { label: name || "调用工具", icon: "wrench", tone: "read" };
}

const SECTION_LABEL: Record<string, string> = {
  all: "全部字段",
  endpoints: "端点",
  capabilities: "能力",
  spec_prices: "计价格",
  labels: "中文名",
  limits: "限额",
  adapters: "适配器",
  plan: "提案字段",
  spec_dsl: "规格 DSL",
};

const ENTITY_LABEL: Record<string, string> = {
  channel: "渠道",
  model: "模型",
  spec: "规格",
};

const PROBE_LABEL: Record<string, string> = {
  none: "只校验地址",
  models: "拉模型列表",
  chat_min: "最小对话",
};

/** 安全解析（截断过的 JSON 会解析失败，返回 null 而不是抛错）。 */
function parse(raw?: string): Record<string, unknown> | null {
  if (!raw) return null;
  try {
    const v = JSON.parse(raw) as unknown;
    return v && typeof v === "object" ? (v as Record<string, unknown>) : null;
  } catch {
    return null;
  }
}

function str(v: unknown): string {
  return typeof v === "string" ? v : typeof v === "number" ? String(v) : "";
}

/**
 * 参数摘要：只说运营看得懂的那一项。
 *
 * 直接把 args_brief 摊开（`{"section":"endpoints"}`）在聊天流里毫无意义；
 * 每条只挑一个"最能说明它在干嘛"的字段。
 */
export function describeToolArgs(name: string, raw?: string): string {
  const a = parse(raw);
  if (!a) return "";
  switch (name) {
    case "schema_get":
      return SECTION_LABEL[str(a.section)] ?? "全部字段";
    case "channel_get":
      return `渠道 #${str(a.id)}`;
    case "model_get":
      return `模型 ${str(a.id)}`;
    case "spec_get":
      return `规格 ${str(a.slug)}`;
    case "docs_read": {
      const off = typeof a.offset === "number" ? a.offset : 0;
      return off > 0 ? `文档 #${str(a.doc_id)} · 第 ${off} 段起` : `文档 #${str(a.doc_id)}`;
    }
    case "dry_run_draft": {
      const probe = PROBE_LABEL[str(a.probe)] ?? "";
      const adapter = str(a.adapter_id) || "候选渠道";
      return probe ? `${adapter} · ${probe}` : adapter;
    }
    case "validate_draft":
      return `${ENTITY_LABEL[str(a.entity)] ?? "草稿"}草稿`;
    case "propose_change": {
      const ops = Array.isArray(a.operations) ? a.operations.length : 0;
      return ops > 0 ? `${ops} 项操作` : "";
    }
    case "submit_unsupported":
      return str(a.vendor);
    case "skill_read":
      return str(a.id);
    case "doc_node_get":
    case "doc_node_create":
    case "doc_node_update":
      return str(a.key);
    case "doc_endpoint_get":
    case "doc_endpoint_create":
    case "doc_endpoint_update":
      return `${str(a.node)}/${str(a.key)}`;
    case "doc_fields_write": {
      const n = Array.isArray(a.fields) ? a.fields.length : 0;
      return `${str(a.node)}/${str(a.key)} · ${n} 个字段`;
    }
    case "doc_check":
      return str(a.node) ? `${str(a.node)}${a.key ? "/" + str(a.key) : ""}` : "全站";
    case "doc_tree":
      return str(a.section) || str(a.query);
    case "channel_list":
    case "model_list":
    case "vendor_list":
      return str(a.query);
    default:
      return "";
  }
}

export interface ToolResultSummary {
  text: string;
  /** ok=成功 / bad=失败 / plain=中性（列表条数这类既不好也不坏） */
  tone: "ok" | "bad" | "plain";
}

/** 结果摘要：条数、提案号、错误原因 —— 运营真正想知道的三类信息。 */
export function describeToolResult(name: string, raw?: string, ok?: boolean): ToolResultSummary {
  const r = parse(raw);
  if (!r) {
    return { text: ok === false ? "失败" : "", tone: ok === false ? "bad" : "plain" };
  }
  if (r.ok === false) {
    const msg = str(r.error) || str(r.message) || "调用失败";
    return { text: msg, tone: "bad" };
  }
  if (name === "propose_change") {
    const pid = r.proposal_id ?? r.id;
    return pid ? { text: `提案 #${str(pid)}`, tone: "ok" } : { text: "已生成", tone: "ok" };
  }
  if (name === "dry_run_draft") {
    return { text: "探测通过", tone: "ok" };
  }
  if (name === "validate_draft") {
    // 预演会带 warnings（非阻断提醒）：恒说"校验通过"会让运营忽略它们，
    // 而那些提醒正是"能存但调用会失败"的预兆。
    const n = Array.isArray(r.warnings) ? r.warnings.length : 0;
    // tone 只有 ok/plain/bad 三档：提醒不是错误，用 plain 而不是硬造一个"警告色"
    // （新增 tone 要同步样式与看板语义，为一个计数不值得）。
    return { text: n > 0 ? `校验通过 · ${n} 条提醒` : "校验通过", tone: n > 0 ? "plain" : "ok" };
  }
  if (name === "doc_check") {
    // 体检的结论就是那一串 blocking：0 才是"能发"，否则要点名还剩几条。
    const n = typeof r.blocking === "number" ? r.blocking : 0;
    return n > 0
      ? { text: `${n} 项会阻止发布`, tone: "bad" }
      : { text: "体检通过（发布由人工完成）", tone: "ok" };
  }
  if (name === "doc_tree") {
    const nodes = typeof r.totals === "object" && r.totals ? (r.totals as Record<string, unknown>) : null;
    const n = nodes && typeof nodes.nodes === "number" ? nodes.nodes : 0;
    return { text: `${n} 个节点`, tone: "plain" };
  }
  if (name.startsWith("doc_")) {
    // 写明"草稿"：AI 写文档不等于对外可见，发布是人做的事。
    if (name.endsWith("_get") || name === "doc_seed_diff") {
      return { text: "已读取", tone: "plain" };
    }
    const n = typeof r.blocking === "number" ? r.blocking : 0;
    return n > 0
      ? { text: `已存为草稿 · ${n} 项待修`, tone: "plain" }
      : { text: "已存为草稿", tone: "ok" };
  }
  const items = r.items;
  if (Array.isArray(items)) {
    return { text: `${items.length} 条`, tone: "plain" };
  }
  if (typeof r.total === "number") {
    return { text: `${r.total} 条`, tone: "plain" };
  }
  if (r.truncated === true) {
    return { text: "结果较长，已截断", tone: "plain" };
  }
  return { text: ok === false ? "失败" : "", tone: ok === false ? "bad" : "plain" };
}
