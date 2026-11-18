/**
 * 文档站 v7 类型（与后端 `docdocs/v7_view.go` 一一对应）。
 *
 * 为什么新开一个文件而不是改 `types/docs.ts`：旧类型服务的是「族页 + Tab」那套结构，
 * 现在要整块换掉；并存期间混在一个文件里，任何一次改动都分不清是给旧页还是新页。
 */

/** 目录形状版本：与后端 `apidoc.CatalogShape` 必须一致（不一致就显式报错）。 */
export const DOC_V7_SHAPE = 7;

export interface DocV7Catalog {
  contract_version: string;
  shape: number;
  base_url: string;
  sections: DocV7Section[];
}

export interface DocV7Section {
  id: string;
  en: string;
  name: string;
  desc: string;
  color: string;
  nodes: DocV7CatalogNode[];
}

export interface DocV7CatalogNode {
  key: string;
  kind: "protocol" | "family" | "general" | "guide";
  title: string;
  summary: string;
  /** 适用模型数量（侧栏不渲染，供调试与 AI 读取）。 */
  models: number;
  status: "draft" | "published";
  endpoints: DocV7EndpointRef[];
}

/** 侧栏第三层 / 接口页身份：一个接口一行，点击进入独立页面。 */
export interface DocV7EndpointRef {
  key: string;
  name: string;
  method: string;
  path: string;
  badge: string;
}

export interface DocV7Node {
  key: string;
  kind: string;
  section: string;
  section_name: string;
  title: string;
  summary: string;
  /** 适用模型：族按前缀命中、协议篇按端点能力派生；空数组时前端整段隐藏。 */
  models: DocV7NodeModel[];
  endpoints: DocV7EndpointRef[];
  /** 说明页正文；非说明页恒为 []（后端不加 omitempty，键一定存在）。 */
  blocks: DocV7Block[];
}

export interface DocV7NodeModel {
  id: string;
  name: string;
  vendor: string;
  vendor_name: string;
}

export interface DocV7Block {
  type: "para" | "note" | "steps" | "bullets" | "code";
  text?: string;
  title?: string;
  items?: string[];
  lang?: string;
  code?: string;
}

export interface DocV7Endpoint {
  node: { key: string; title: string; section: string; section_name: string };
  key: string;
  kind: string;
  name: string;
  summary: string;
  method: string;
  path: string;
  aliases: string[];
  phase: string;
  /** 契约机器码：sync / submit / read。分支判据只能用它（`phase` 是中文文案）。 */
  phase_code?: string;
  /** 同一路径还受理 `async: true`（图像生成这类"同步/异步同路径"的端点）。 */
  accepts_async?: boolean;
  transport: string;
  run_mode: string;
  run_hint?: string;
  max_body?: string;
  /** 接口级注意事项（计费口径 / 超时 / 409 语义），由后端从契约派生。 */
  notes: string[];
  fields: DocV7Field[];
  response: DocV7Response;
  presets: DocV7Preset[];
  errors: DocV7Error[];
  models: DocV7NodeModel[];
  samples?: DocV7Samples;
  samples2?: DocV7Samples;
  example_title?: string;
  example_title2?: string;
  published: boolean;
}

export interface DocV7Field {
  name: string;
  type: string;
  in: string;
  required: "required" | "optional" | "conditional";
  required_note?: string;
  default: string;
  /** 全部说明文字（多行）：页面上按段落显示，后台是一个多行文本框。 */
  desc: string;
  enum?: { value: string; desc?: string; deprecated?: boolean }[];
  value_source?: string;
  constraints?: { kind: string; value: number; text: string }[];
  tier: string;
  passthrough?: boolean;
  /** 该字段不进响应骨架（同一位置二选一的返回字段）。 */
  sketch_skip?: boolean;
  /** 只对哪些模型生效；空 = 本接口全部适用。 */
  only_models?: string;
  example?: unknown;
}

export interface DocV7Response {
  kind: string;
  envelope: boolean;
  content_type?: string;
  variants: DocV7Variant[];
}

export interface DocV7Variant {
  status: number;
  label: string;
  content_type?: string;
  returns: DocV7Field[];
  example?: unknown;
  raw?: string;
  notes?: string[];
}

export interface DocV7Preset {
  key: string;
  label: string;
  summary: string;
  body: unknown;
  fields: string[];
  uploads?: string[];
  note?: string;
  samples?: DocV7Samples;
}

export interface DocV7Error {
  status: number;
  code: string;
  desc: string;
}

export interface DocV7Samples {
  curl: string;
  python: string;
  node: string;
  go: string;
}
