/**
 * 管理台 · API 文档编辑页的类型（与后端 `docdocs/v7_admin.go` / `v7_sections.go` 一一对应）。
 *
 * 与 `types/docsv7.ts`（对外只读投影）刻意分开：编辑态还要 status / version / hidden /
 * transport_doc_id，且**有序对象用 `[{k,v}]` 数组**（Go 的 map 不保序，数组才能保证
 * 「读出来什么样、存回去还是什么样」）。
 *
 * 一条硬约束：这里的每个字段都必须在页面上有一个能改它的控件。
 * 后端返回了、编辑器读不到，保存一次就会被零值覆盖 —— 那是最难发现的一类丢内容。
 */

export type DocStatus = "draft" | "published";
export type DocNodeKind = "protocol" | "family" | "general" | "guide";

/* ------------------------------ 目录 ------------------------------ */

export interface DocsAdminTree {
  shape: number;
  contract_version: string;
  /** 站点对外地址：预览用它替换示例代码里的 {{base_url}}。 */
  base_url: string;
  sections: DocsAdminSection[];
  totals: {
    nodes: number;
    endpoints: number;
    draft_nodes: number;
    draft_endpoints: number;
  };
}

/** 板块：L1。内容是库里的（doc_sections），可改名/改序/改色/隐藏。 */
export interface DocsAdminSection {
  key: string;
  name: string;
  en: string;
  desc: string;
  color: string;
  sort: number;
  hidden: boolean;
  version: number;
  /** 库里没有这一行（数据坏了）：单独成组提示修数据，不静默丢节点。 */
  unknown: boolean;
  nodes: DocsAdminTreeNode[];
}

export interface DocsAdminTreeNode {
  key: string;
  kind: DocNodeKind;
  title: string;
  status: DocStatus;
  hidden: boolean;
  sort: number;
  version: number;
  /** 适用模型数（只读派生）：族按前缀命中、协议按端点能力。 */
  models: number;
  endpoints: DocsAdminEndpointRef[];
}

export interface DocsAdminEndpointRef {
  key: string;
  name: string;
  method: string;
  path: string;
  badge: string;
  status: DocStatus;
  hidden: boolean;
  sort: number;
  version: number;
  /** 没绑平台端点 ⇒ 只能存草稿（发布会被 E_UNBOUND 拦）。 */
  bound: boolean;
}

export interface DocsAdminProblem {
  code: string;
  field: string;
  message: string;
  blocking: boolean;
}

/* ------------------------------ 板块 ------------------------------ */

export interface DocsAdminSectionData {
  name: string;
  en: string;
  desc: string;
  color: string;
  sort: number;
  hidden: boolean;
  version: number;
}

/* ------------------------------ 节点 ------------------------------ */

export interface DocsAdminBlock {
  type: "para" | "note" | "steps" | "bullets" | "code";
  text?: string;
  title?: string;
  items?: string[];
  lang?: string;
  code?: string;
}

export interface DocsAdminNode {
  key: string;
  kind: DocNodeKind;
  section: string;
  section_name: string;
  protocol: string;
  title: string;
  summary: string;
  sort: number;
  hidden: boolean;
  status: DocStatus;
  version: number;
  origin: string;
  updated_at: string;
  models: { id: string; name: string; vendor: string; vendor_name?: string }[];
  blocks: DocsAdminBlock[];
  endpoints: DocsAdminEndpointRef[];
  problems: DocsAdminProblem[];
}

/** 节点的可编辑内容（保存时提交的就是它）。 */
export interface DocsAdminNodeData {
  title: string;
  summary: string;
  section: string;
  protocol: string;
  hidden: boolean;
  sort: number;
  version: number;
  blocks: DocsAdminBlock[];
}

/* ------------------------------ 接口 ------------------------------ */

/** 有序对象的一项（预设体 / 响应示例）。 */
export interface DocKV {
  k: string;
  v: unknown;
}

/** 四语言示例代码。 */
export interface DocSamples {
  curl: string;
  python: string;
  node: string;
  go: string;
}

export interface DocsAdminField {
  name: string;
  type: string;
  in: string;
  required: "required" | "optional" | "conditional" | string;
  required_note?: string;
  default: string;
  /** 全部说明文字（多行）：页面按段落显示，后台就是一个多行文本框。 */
  desc: string;
  enum?: { value: string; desc?: string; deprecated?: boolean }[];
  value_source?: string;
  constraints?: { kind: string; value: number; text: string }[];
  tier: string;
  passthrough?: boolean;
  /** 返回字段专用：不进响应骨架。 */
  sketch_skip?: boolean;
  /** 「仅这些模型」的展示文本（内容，可编辑）。 */
  only_models?: string;
  example?: unknown;
}

export interface DocsAdminVariant {
  status: number;
  label: string;
  content_type: string;
  /** 逐变体的线格式：202 是 JSON 任务对象，200 二进制是 png。 */
  kind: string;
  envelope: boolean;
  returns: DocsAdminField[];
  example: DocKV[];
  raw: string;
  notes: string[];
}

export interface DocsAdminResponse {
  kind: string;
  envelope: boolean;
  content_type: string;
  variants: DocsAdminVariant[];
}

export interface DocsAdminPreset {
  key: string;
  label: string;
  summary: string;
  body: DocKV[];
  fields: string[];
  uploads: string[];
  note: string;
  samples: DocSamples | null;
}

export interface DocsAdminError {
  status: number;
  code: string;
  desc: string;
  /** seed / ai / manual：保存时必须原样带回，否则「哪些是出厂」永久丢失。 */
  source: string;
}

export interface DocsAdminEndpoint {
  node_key: string;
  key: string;
  name: string;
  summary: string;
  method: string;
  path: string;
  aliases: string[];
  transport_doc_id: string;
  method_check: string;
  path_check: string;
  phase: string;
  run_mode: string;
  run_hint: string;
  max_body: string;
  notes: string[];
  badge: string;
  sort: number;
  hidden: boolean;
  status: DocStatus;
  version: number;
  origin: string;
  contract_ver: string;
  updated_at: string;
  fields: DocsAdminField[];
  response: DocsAdminResponse;
  samples: DocSamples | null;
  samples2: DocSamples | null;
  example_title: string;
  example_title2: string;
  presets: DocsAdminPreset[];
  errors: DocsAdminError[];
  problems: DocsAdminProblem[];
}

/** 接口的可编辑内容（保存时提交的就是它）。 */
export interface DocsAdminEndpointData {
  name: string;
  summary: string;
  method: string;
  path: string;
  transport_doc_id: string;
  aliases: string[];
  run_mode: string;
  run_hint: string;
  max_body: string;
  notes: string[];
  hidden: boolean;
  sort: number;
  version: number;
  fields: DocsAdminField[];
  response: DocsAdminResponse;
  samples: DocSamples | null;
  samples2: DocSamples | null;
  example_title: string;
  example_title2: string;
  presets: DocsAdminPreset[];
  errors: DocsAdminError[];
}

/* ------------------------------ 绑定端点 ------------------------------ */

/**
 * 可绑定的平台端点。
 *
 * 不带文档名称/摘要：同一端点在各族是不同的协议，端点本身没有"唯一的名字"；
 * 标识就是 doc_id（`images/generations` / `video/submit@jimeng`）。
 */
export interface DocsAdminTransport {
  doc_id: string;
  section: string;
  section_name: string;
  method: string;
  path: string;
  phase: string;
  badge: string;
  accepts_async: boolean;
  used_by: string[];
}
