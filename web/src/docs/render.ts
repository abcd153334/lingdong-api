/**
 * 文档站的**唯一判定层**：分组、必填、默认值、chip、响应树、鉴权、可调试性
 * 全部只在这里算一次。组件只负责把结果画出来。
 *
 * 为什么集中：这些规则在「正文参数区」「右栏示例」「在线运行表单」「工作台预览」
 * 四个地方各用一遍。复制四份的后果不是多写代码，而是四处对同一份数据给出不同
 * 结论 —— 读者看到「可选」的字段在试跑面板里是必填，或者预览里少了半棵树。
 *
 * 这里只放纯函数：不 import React、不读 window（除 canTry 的同源判定）、不发请求。
 */

import { jsonBodyError } from "@/lib/jsonBody";
import type {
  DocV7Endpoint,
  DocV7Field,
  DocV7Samples,
  DocV7Variant,
} from "@/types/docsv7";

/* ────────────────────────── 请求参数分组 ────────────────────────── */

export interface FieldGroup {
  id: string;
  label: string;
  rows: DocV7Field[];
}

/**
 * 显示顺序固定为 path → query → header → body → form。
 *
 * 不用「按 in 首次出现顺序」：同一份契约的字段在库里按写入顺序排列，
 * 顺序会随最后一次保存漂移，页面上参数分组的先后就会莫名变化。
 */
const GROUP_ORDER = ["path", "query", "header", "body", "form"] as const;

const GROUP_LABEL: Record<string, string> = {
  path: "Path 参数",
  query: "Query 参数",
  header: "Header 参数",
  body: "Body 参数",
  form: "Form 参数",
};

/**
 * 按 `in` 分组；空组不产出；未知 `in` 追加末尾。
 *
 * 这里**不再剔除 Authorization / Content-Type**：那些行是库里的内容，
 * 后台能编辑它们，页面就必须照原样显示。以前为了让页面顶部的「鉴权卡」
 * 不重复而把它们藏掉 —— 那张卡是代码按路径拼出来的固定文案，后台改不了，
 * 结果是「读者看到鉴权说明，运营却在后台找不到对应的输入框」。
 * 现在鉴权信息只有两个来源：这个参数表里的 header 行，以及示例代码里的请求头。
 */
export function groupFields(fields: DocV7Field[]): FieldGroup[] {
  const buckets = new Map<string, DocV7Field[]>();
  for (const f of fields) {
    const list = buckets.get(f.in);
    if (list) list.push(f);
    else buckets.set(f.in, [f]);
  }
  const out: FieldGroup[] = [];
  for (const id of GROUP_ORDER) {
    const rows = buckets.get(id);
    if (rows && rows.length > 0) {
      out.push({ id, label: GROUP_LABEL[id], rows });
      buckets.delete(id);
    }
  }
  // 契约里新增了一个没人预料到的 `in`（例如 `cookie`）：追加而不是丢弃。
  // 丢掉的表现是「文档里少了几个参数」，而没有任何报错。
  for (const [id, rows] of buckets) {
    out.push({ id, label: `${id} 参数`, rows });
  }
  return out;
}

export function fieldCount(groups: FieldGroup[]): number {
  return groups.reduce((n, g) => n + g.rows.length, 0);
}

/* ────────────────────────── 必填 / 默认值 ────────────────────────── */

export type RequiredTag = { text: string; cls: string };

export function requiredTag(f: DocV7Field): RequiredTag {
  if (f.required === "conditional") return { text: "条件必填", cls: "cond" };
  if (f.required === "required") return { text: "必填", cls: "req" };
  return { text: "可选", cls: "opt" };
}

/**
 * 默认值：库里写什么就显示什么。
 *
 * 这里曾有一张「这些串不算默认值」的抑制表（`—` / `无` / `模型上限` …），
 * 它会静默吞掉运营写下的内容 —— 后台填了「模型上限」，前台什么都不显示，
 * 而两边都不会报错。取值该不该展示，由写这行字的人决定，不由前端猜。
 */
export function defaultText(f: DocV7Field): string | null {
  const v = String(f.default ?? "").trim();
  return v === "" ? null : v;
}

/* ────────────────────────── tier 与 chip ────────────────────────── */

/**
 * tier 是字段的**归属层级**，回答「这个字段是谁定的」。
 *
 * 四个值全部显示（曾经只显示 V 与 N，理由是"P 占绝大多数，逐行挂等于刷噪音"）。
 * 那个理由有个前提：读者知道"没标注的就是平台字段"。但实际反馈是**没人知道** ——
 * 于是"哪些字段是平台自己发明的"变成一个说不清的问题，而它恰恰是接入时最想搞清楚的。
 *
 * 代价用样式解决而不是靠隐藏：P 用弱化样式（见 chipList 的调用方），
 * 视觉上沉下去，但信息在。
 *
 * `standard` 是历史值（早期把 S 写成 full word），一并映射到"标准字段"，
 * 避免老数据在页面上什么都不显示。
 */
function tierChip(tier: string): string | null {
  switch (tier) {
    case "P":
      return "平台字段";
    case "S":
    case "standard":
      return "标准字段";
    case "V":
      return "厂商扩展";
    case "N":
      return "兼容写法";
  }
  return null;
}

/**
 * `value_source` 里这些说明「值不由模型决定」，因此不打「取值随模型」。
 *
 * `group_members` 指 model 字段（它的取值就是模型清单，已有 chips 表达），
 * 再挂一个「取值随模型」等于同一句话说两遍。
 */
const SOURCE_SELF_EVIDENT = new Set(["static", "none", "", "group_members"]);

/** 字段行尾的 chips，顺序即信息优先级：先「能不能用这个值」，再「它是什么」。 */
export function chipList(f: DocV7Field): string[] {
  const out: string[] = [];
  if (f.only_models) out.push(`仅 ${f.only_models}`);
  for (const c of f.constraints ?? []) if (c.text) out.push(c.text);
  const tier = tierChip(f.tier);
  if (tier) out.push(tier);
  if (f.passthrough) out.push("原样转发");
  if (f.value_source && !SOURCE_SELF_EVIDENT.has(f.value_source)) out.push("取值随模型");
  if (f.sketch_skip) out.push("不进响应骨架");
  return out;
}

/** 示例值：对象与数组不展开（一行放不下，展开会把参数卡撑成半屏）。 */
function exampleText(v: unknown): string | null {
  if (v === undefined || v === null || v === "") return null;
  if (Array.isArray(v)) return `[${v.length} 项]`;
  if (typeof v === "object") return "{…}";
  return String(v);
}

/**
 * 字段是否要展示示例：与默认值相同、或是 model 字段（已有 chips）时不展示。
 *
 * 这三种情况下示例不提供任何新信息，只是把同一行重复一遍。
 */
export function showExample(f: DocV7Field): string | null {
  if (f.name === "model") return null;
  const text = exampleText(f.example);
  if (text === null) return null;
  if (text === defaultText(f)) return null;
  return text;
}

/* ────────────────────────── 响应树 ────────────────────────── */

export interface TreeNode {
  key: string;
  name: string;
  arr: boolean;
  children: TreeNode[];
  row: DocV7Field | null;
}

/**
 * 扁平的 `contents[].url` 名字 → 树。
 *
 * 同一 `key` 复用节点：`choices[].message.content` 与 `choices[].finish_reason`
 * 共享同一个 `choices[]` 父节点，各自建一份会让父行出现两次。
 *
 * `sketch_skip` 的字段连带其子树一起剔除：父行不显示时，孤儿子行没有归属，
 * 渲染出来会变成顶部一层无名的缩进行。
 */
export function buildTree(rows: DocV7Field[]): TreeNode[] {
  const skipped: string[] = [];
  for (const r of rows) if (r.sketch_skip) skipped.push(r.name);
  const visible = skipped.length
    ? rows.filter((r) => !skipped.some((s) => r.name === s || r.name.startsWith(`${s}.`)))
    : rows;

  const root: TreeNode[] = [];
  const map = new Map<string, TreeNode>();
  for (const r of visible) {
    const segs = String(r.name || "").split(".");
    let parent: TreeNode | null = null;
    let key = "";
    segs.forEach((seg, i) => {
      const arr = seg.includes("[]");
      const name = seg.replace(/\[\]/g, "");
      key += (i ? "." : "") + name + (arr ? "[]" : "");
      let node = map.get(key);
      if (!node) {
        node = { key, name, arr, children: [], row: null };
        map.set(key, node);
        (parent ? parent.children : root).push(node);
      }
      if (i === segs.length - 1) node.row = r;
      parent = node;
    });
  }
  return root;
}

/* ────────────────────────── 鉴权 ────────────────────────── */

export interface AuthInfo {
  /** `x-api-key` 还是 `Authorization: Bearer`。 */
  header: string;
  scheme: string;
  hint: string;
  /**
   * 是否必须提供密钥。
   *
   * 产物代理（`/v1/media/{token}`）与文件直链（`/f/{id}/{name}`）的凭证就是
   * 地址本身 —— 强制填 Key 会把"公开可看"的接口伪装成"必须注册"。
   */
  required: boolean;
}

/**
 * 鉴权方式由路径决定（`/v1/messages` 是 Anthropic 原生协议，认 `x-api-key`）。
 *
 * 契约里 Authorization 字段本来就在，但它只写「API Key」，不区分两种头；
 * 让读者在 Claude 页照抄 Bearer 会拿到 401。
 */
export function authOf(doc: Pick<DocV7Endpoint, "path">): AuthInfo {
  // 免鉴权：链接本身即凭证。判据只有路径，与 authOf 的既有做法同源。
  if (doc.path.startsWith("/v1/media/") || doc.path.startsWith("/f/")) {
    return {
      header: "Authorization",
      scheme: "无",
      hint: "不需要密钥：链接本身即凭证，过期时间写在地址里。",
      required: false,
    };
  }
  if (doc.path.startsWith("/v1/messages")) {
    return {
      header: "x-api-key",
      scheme: "x-api-key / Bearer",
      hint: "请求头 x-api-key: <API Key>，也接受 Authorization: Bearer <API Key>。",
      required: true,
    };
  }
  return {
    header: "Authorization",
    scheme: "Bearer",
    hint: "请求头 Authorization: Bearer <API Key>。",
    required: true,
  };
}

/* ────────────────────────── 页头标签 ────────────────────────── */

export type Tag = { text: string; tone: "" | "sync" | "stream" };

/**
 * 标签直接取后端的 `phase`（同步 / 异步提交 / 任务查询），不在这里二次推断。
 *
 * tone 的判据是 `phase_code`（sync/submit/read）而不是中文：中文文案随时可改，
 * 拿它做分支等同于把展示文本当契约。
 *
 * 「支持异步」是**能力**，与「形态」是两个维度：同一篇（图像生成）既可能同步出图，
 * 也可能加 `async: true` 转任务，所以单独一枚标签，不覆盖 phase。
 */
export function phaseTags(
  doc: Pick<DocV7Endpoint, "phase" | "phase_code" | "run_mode" | "accepts_async">,
): Tag[] {
  const out: Tag[] = [];
  const code = doc.phase_code ?? "";
  const sync = code ? code === "sync" : doc.phase === "同步";
  if (doc.phase) out.push({ text: doc.phase, tone: sync ? "sync" : "" });
  if (doc.accepts_async && (code === "" || code === "sync")) {
    out.push({ text: "支持异步", tone: "" });
  }
  if (doc.run_mode === "sse") out.push({ text: "流式 SSE", tone: "stream" });
  return out;
}

/* ────────────────────────── 示例选择 ────────────────────────── */

/**
 * 请求示例的唯一来源：预设自己的 samples 优先，其次接口级。
 *
 * 预设（如「参考图生视频」）的示例体与接口级不同，混用会让读者点了预设却看到
 * 默认示例。两者都为空时返回 null，由调用方渲染空态 —— 不要返回空字符串，
 * 否则代码块会显示成一个空白框。
 */
export function samplesOf(doc: DocV7Endpoint, presetIndex: number): DocV7Samples | null {
  const p = doc.presets?.[presetIndex];
  if (p?.samples) return p.samples;
  return doc.samples ?? null;
}

/* ────────────────────────── 响应变体 ────────────────────────── */

export type VariantKind = "raw" | "json" | "empty";

export function variantKind(v: DocV7Variant): VariantKind {
  if (v.raw) return "raw";
  if (v.example !== undefined && v.example !== null) return "json";
  return "empty";
}

/** 变体页签文案：状态码 + 种类，不加「错误/JSON」这类需要读者翻译的词。 */
export function variantLabel(v: DocV7Variant): string {
  const kind = variantKind(v);
  if (kind === "raw") return `${v.status} 原文`;
  if (kind === "empty") return `${v.status} 无示例`;
  return `${v.status} JSON`;
}

/** 状态圆点：≥400 红、≥300 黄、其余绿（与全站状态色一致）。 */
export function statusTone(status: number): "" | "a" | "e" {
  if (status >= 400) return "e";
  if (status >= 300) return "a";
  return "";
}

/**
 * 空响应字段时的说明，区分「本来就无固定结构」与「有示例但没写字段」。
 *
 * 两种情况对读者的下一步不同：前者去看右栏响应示例，后者说明文档缺内容。
 */
export function emptyReturnsText(hasExample: boolean): string {
  return hasExample
    ? "该响应没有固定字段，结构见响应示例。"
    : "该响应没有固定字段，也没有示例。";
}

/* ────────────────────────── 在线运行可用性 ────────────────────────── */

export interface TryGate {
  ok: boolean;
  /** 不可用时展示的原因（一句话，说明下一步做什么）。 */
  reason?: string;
}

/**
 * 判断这个接口能不能在浏览器里直接发。**只剩三道闸，每条都是"真的发不出去"**：
 *
 * 1. `run_mode === "none"`：契约明确说这条不提供在线试跑，原因来自 **`run_hint`**
 *    （每篇不同、后台可编辑）。此前这里写死一句话，于是 15 篇各有各的原因被抹平成
 *    同一句"该接口不适合在线调试"—— 读者看不出到底缺什么。
 * 2. `transport === "multipart"`：本面板不做文件选择，发出去一定缺文件。
 *    **取值是 "multipart" 而不是 "multipart/form-data"**（见 docdocs/v7_samples.go:transportOf）。
 * 3. 没有基址：拼不出 URL。
 *
 * **不同源不再拦**。跨域能不能通由浏览器与后端 `LD_ALLOWED_ORIGINS` 裁决 ——
 * 前端再判一次就是第二套真相源，而且更严（`api.x.com` 与 `www.x.com` 是常态）。
 * 真发不出去时，结果区会给可操作的解释（见 `statusHint` 与 TryPanel 的 catch）。
 */
export function canTry(
  doc: Pick<DocV7Endpoint, "run_mode" | "transport" | "run_hint">,
  baseUrl: string,
): TryGate {
  if (doc.run_mode === "none") {
    return { ok: false, reason: doc.run_hint?.trim() || "这条接口不支持在线调试。" };
  }
  if (doc.transport === "multipart") {
    return { ok: false, reason: "表单上传接口请在服务端按示例调用。" };
  }
  if (!baseUrl) {
    return { ok: false, reason: "未配置接口地址，无法在线调试。" };
  }
  return { ok: true };
}

/* ────────────────────────── 在线运行：URL 与请求体 ────────────────────────── */

/** 路径参数占位符（契约里写作 `{id}` / `{index}`）。 */
export function pathParams(path: string): string[] {
  return [...path.matchAll(/\{([^}]+)\}/g)].map((m) => m[1]);
}

/**
 * 把路径参数填进路径。缺值时返回 null，由调用方拦截并指名缺哪个。
 *
 * 值必须 `encodeURIComponent`：任务号里出现 `/` 或空格时，不编码会拼出**另一条路由**
 * （`/v1/tasks/a/b` 打到别的 handler，读者看到 404 却以为是文档写错了）。
 *
 * 不在这里给默认值（比如 `task_x`）：拿一个不存在的任务号去请求，读者会看到
 * 一个 404，然后以为是文档写错了。
 */
export function fillPath(path: string, values: Record<string, string>): string | null {
  for (const n of pathParams(path)) {
    if (!values[n]?.trim()) return null;
  }
  return path.replace(/\{([^}]+)\}/g, (_, n: string) => encodeURIComponent(values[n].trim()));
}

/** 缺哪个路径参数（用于提示语，按契约声明顺序取第一个）。 */
export function missingPathParam(path: string, values: Record<string, string>): string | null {
  for (const n of pathParams(path)) {
    if (!values[n]?.trim()) return n;
  }
  return null;
}

/* ---------------- 表单行与勾选（UI 与发送共用的唯一实现） ---------------- */

/**
 * 表单里的键：`位置:字段名`。
 *
 * 必须带位置前缀：`model` 可能同时出现在 body 与 query，只用字段名会互相覆盖，
 * 症状是"改了 query 的 model，body 的也跟着变"。
 */
export function fieldKey(f: DocV7Field): string {
  return `${f.in}:${f.name}`;
}

/** 在线运行表单可编辑的行：只有 body 里的标量字段（嵌套结构交给示例代码）。 */
export function tryRows(fields: DocV7Field[]): DocV7Field[] {
  return fields.filter(
    (f) => f.in === "body" && !f.name.includes(".") && !f.name.includes("["),
  );
}

/** 可编辑的 query 行。 */
export function queryRows(fields: DocV7Field[]): DocV7Field[] {
  return fields.filter((f) => f.in === "query");
}

/**
 * 可编辑的 header 行：排除鉴权头与 Content-Type。
 *
 * 两种鉴权头都由密钥输入框唯一提供（`/v1/messages` 认 `x-api-key`，其余认
 * `Authorization`）—— 让契约里的同名行再填一次，一次请求就会带两个鉴权头，
 * 网关按哪个都不是。Content-Type 由方法决定，同理。
 */
export function headerRows(fields: DocV7Field[]): DocV7Field[] {
  const skip = new Set(["authorization", "x-api-key", "content-type"]);
  return fields.filter((f) => f.in === "header" && !skip.has(f.name.toLowerCase()));
}

/**
 * 默认勾选：只有必填项。query 与 body 同一规则 ——
 * 此前 query 一律不勾，导致"必填 query 永远发不出去"。
 */
export function defaultOn(f: DocV7Field): boolean {
  return f.required === "required";
}

export function fieldOn(f: DocV7Field, picked: Record<string, boolean>): boolean {
  return picked[fieldKey(f)] ?? defaultOn(f);
}

/* ---------------- 初值 ---------------- */

/** 这些类型无法用单行输入框表达。 */
const COMPLEX_TYPES = new Set(["array", "object", "file"]);

export function isComplexType(type: string): boolean {
  return COMPLEX_TYPES.has(type);
}

/** 预设示例体（右栏示例里的第一个），只用于 body 位置的初值。 */
export function presetBodyOf(doc: Pick<DocV7Endpoint, "presets">): unknown {
  return doc.presets?.[0]?.body;
}

/**
 * 输入框初值。三个来源，按"是不是机器可用值"排序：
 *
 *  ① `presets[i].body`：一次完整调用的真实组合（`{model, prompt, size}` 是配套的）；
 *  ② `field.example`：契约里唯一的机器值（chat 的 `model="gpt-4o"`、`messages=[...]`）；
 *  ③ `default`：**只在按本字段类型能落地时才用**（见 `usableDefault`）；
 *  ④ 空串。
 *
 * 旧实现是「default 优先」，而 `default` 在库里是**给人看的文案**：
 * `"—"` 表示"没有默认值"，`"模型上限"` 是一句说明。把它当值会发出
 * `model: "—"`、`max_tokens: "—"`（整型字段收到字符串）、`messages: "—"`
 * （`JSON.parse` 直接抛错）—— chat 页因此永远发不出去。
 *
 * `presetBody` 只对 `in === "body"` 生效：query 与 body 是两个键空间，
 * 用同一份预设体会串值。
 */
export function initialValue(f: DocV7Field, presetBody: unknown): string {
  if (f.in === "body" && presetBody && typeof presetBody === "object" && !Array.isArray(presetBody)) {
    const v = (presetBody as Record<string, unknown>)[f.name];
    if (v !== undefined && v !== null) return textOf(v);
  }
  if (f.example !== undefined && f.example !== null && f.example !== "") {
    return textOf(f.example);
  }
  return usableDefault(f) ?? "";
}

function textOf(v: unknown): string {
  return typeof v === "object" ? JSON.stringify(v, null, 2) : String(v);
}

/**
 * `default` 能不能当值用：判据是**类型**，不是词表。
 *
 * 布尔只认 `true/false`，数字只认能解析出有限数的串，其余类型一律不取 ——
 * `"—"`、`"模型默认"`、`"自动检测"` 因此自然落空，不需要维护一张黑名单
 * （黑名单永远漏：文案是运营写的）。
 */
function usableDefault(f: DocV7Field): string | null {
  const t = (f.default ?? "").trim();
  if (t === "") return null;
  if (f.type === "boolean") return t === "true" || t === "false" ? t : null;
  if (f.type === "integer" || f.type === "number") {
    return Number.isFinite(Number(t)) ? t : null;
  }
  return null;
}

/** 输入框现值：用户改过用改后的，否则按初值规则算。UI 与发送必须共用这一个。 */
export function fieldValue(
  f: DocV7Field,
  presetBody: unknown,
  values: Record<string, string>,
): string {
  return values[fieldKey(f)] ?? initialValue(f, presetBody);
}

/**
 * 输入框是字符串，按字段类型还原成 JSON 值。
 *
 * 返回 Result 而不是"猜一个"：`max_tokens` 填了「模型上限」时，旧实现会把它当
 * 字符串发出去，上游返回 400 `参数错误`，读者以为是自己写错了字段名。
 * 现在停在面板里，指名是哪个字段要什么类型。
 */
export type Coerced = { ok: true; value: unknown } | { ok: false; message: string };

export function coerce(raw: string, type: string): Coerced {
  const t = raw.trim();
  if (type === "boolean") {
    if (t === "true") return { ok: true, value: true };
    if (t === "false") return { ok: true, value: false };
    return { ok: false, message: "需要 true 或 false" };
  }
  if (type === "integer" || type === "number") {
    const n = Number(t);
    if (t === "" || !Number.isFinite(n)) return { ok: false, message: "需要数字" };
    return { ok: true, value: type === "integer" ? Math.trunc(n) : n };
  }
  // string / any / 契约将来新增的类型：原样字符串。
  // 不报错的理由：网关对透传字段不做类型校验，报错只会让新类型字段永远调不通。
  return { ok: true, value: raw };
}

/* ---------------- 唯一构造点 ---------------- */

/** 表单里的原始输入（全部是字符串，input 的天然形态）。 */
export interface TryDraft {
  key: string;
  /** 路径参数，键是 `{id}` 里的名字 */
  path: Record<string, string>;
  /** 键 = `fieldKey(f)` */
  values: Record<string, string>;
  /** 键 = `fieldKey(f)` */
  picked: Record<string, boolean>;
  /**
   * 请求体从哪来：`form`（默认，按字段拼）或 `json`（原样发送 `json`）。
   *
   * 二选一而不是"合并"：两种来源同时生效就必然出现"表单改了但发的是旧 JSON"
   * 这类无法解释的现象 —— 同一时刻只能有一个真相源。
   */
  bodyMode?: BodyMode;
  /** 仅 `bodyMode === "json"` 时读取；**原样发送**，不重新序列化。 */
  json?: string;
}

export type BodyMode = "form" | "json";

export interface BuiltTryRequest {
  method: string;
  url: string;
  headers: Record<string, string>;
  /** JSON 文本；该请求不带体时为 undefined（不是空串） */
  body?: string;
}

export type BuildTryResult =
  | { ok: true; req: BuiltTryRequest }
  | { ok: false; message: string };

/** 带体的方法（GET/HEAD/DELETE 一律不带体：部分网关会丢弃或直接 400）。 */
const BODY_METHODS = new Set(["POST", "PUT", "PATCH"]);

/** 这个方法有没有请求体 —— UI 用它决定要不要渲染「请求体」整节。 */
export function bodyAllowed(method?: string): boolean {
  return BODY_METHODS.has((method || "POST").toUpperCase());
}

/**
 * 请求体 JSON 的校验与排版：实现在 `lib/jsonBody.ts`（与后台试运行共用一份），
 * 这里只做转发，让文档站的调用方不必知道它住在哪。
 */
export { jsonBodyError, prettyJson } from "@/lib/jsonBody";

/**
 * 表单 → 请求体。
 *
 * 单独抽出来是因为「切到 JSON 时按表单预填」不该依赖 Key 与路径参数：
 * 那两件事与请求体无关，用 `buildTryRequest` 兜的话，没填 Key 的人一切换就只看到
 * `{}`，会以为表单的值丢了（真踩过）。
 *
 * `buildTryRequest` 是它唯一的调用者之一，两者不允许各写一份拼装逻辑。
 */
export function formBodyOf(
  doc: Pick<DocV7Endpoint, "fields" | "presets">,
  values: Record<string, string>,
  picked: Record<string, boolean>,
): { ok: true; body: string } | { ok: false; message: string } {
  const presetBody = presetBodyOf(doc);
  const obj: Record<string, unknown> = {};
  for (const f of tryRows(doc.fields)) {
    if (!fieldOn(f, picked)) continue;
    const raw = fieldValue(f, presetBody, values);
    if (raw.trim() === "") continue;
    if (isComplexType(f.type)) {
      try {
        obj[f.name] = JSON.parse(raw);
      } catch {
        return { ok: false, message: `${f.name} 不是合法的 JSON` };
      }
      continue;
    }
    const c = coerce(raw, f.type);
    if (!c.ok) return { ok: false, message: `${f.name}：${c.message}` };
    obj[f.name] = c.value;
  }
  return { ok: true, body: JSON.stringify(obj) };
}

/**
 * 构造一次真实请求 —— **预览与发送唯一的来源**。
 *
 * 屏幕上看到的 cURL 与实际发出去的那一份由这里同时产出；任何"另拼一份"的写法
 * 迟早分叉，而读者正是拿着复制出来的那份去写代码。
 *
 * 硬约定：
 *  - `Content-Type: application/json` 与 body 由**方法**决定，不看字段数量：
 *    全部取消勾选时 POST 也要发 `{}` 并带头（旧实现两者都不带，网关回 415）。
 *  - **请求体只有一个来源**：`bodyMode === "json"` 时原样发送读者写的文本，
 *    一个字段都不从表单补；否则按勾选的字段拼。两种模式互不参与，
 *    否则"表单里改了值、发出去的却是旧 JSON"将无法从界面解释。
 *  - 自定义 header 覆盖同名默认头（忽略大小写），不允许出现两个 `Authorization`。
 *  - 值留空视同"不传"，让服务端给出明确的缺参错误，而不是收到一个空串。
 */
export function buildTryRequest(
  doc: Pick<DocV7Endpoint, "method" | "path" | "fields" | "presets" | "run_mode">,
  baseUrl: string,
  draft: TryDraft,
): BuildTryResult {
  const key = draft.key.trim();
  const auth = authOf(doc);
  if (auth.required && !key) return { ok: false, message: "请先填入 API Key" };

  const missing = missingPathParam(doc.path, draft.path);
  if (missing) return { ok: false, message: `请填写路径参数 ${missing}` };
  const path = fillPath(doc.path, draft.path);
  if (!path) return { ok: false, message: "路径参数不完整" };

  const qs = new URLSearchParams();
  for (const f of queryRows(doc.fields)) {
    if (!fieldOn(f, draft.picked)) continue;
    const v = fieldValue(f, null, draft.values);
    if (v.trim() !== "") qs.set(f.name, v);
  }

  const headers: Record<string, string> = {};
  const put = (name: string, value: string) => {
    for (const k of Object.keys(headers)) {
      if (k.toLowerCase() === name.toLowerCase()) delete headers[k];
    }
    headers[name] = value;
  };
  if (key) {
    if (auth.header === "x-api-key") {
      put("x-api-key", key);
      put("anthropic-version", "2023-06-01");
    } else {
      put("Authorization", `Bearer ${key}`);
    }
  }
  for (const f of headerRows(doc.fields)) {
    if (!fieldOn(f, draft.picked)) continue;
    const v = fieldValue(f, null, draft.values);
    if (v.trim() !== "") put(f.name, v);
  }

  const method = (doc.method || "POST").toUpperCase();
  let body: string | undefined;
  if (BODY_METHODS.has(method)) {
    if (draft.bodyMode === "json") {
      const raw = (draft.json ?? "").trim();
      const bad = jsonBodyError(raw);
      if (bad) return { ok: false, message: bad };
      // 原样发送：读者要验证的正是"我改的这一份"，重新序列化会抹掉键顺序，
      // 也让"发出去的是不是屏幕上这段"变成一个无法自证的问题。
      body = raw === "" ? "{}" : raw;
    } else {
      const fb = formBodyOf(doc, draft.values, draft.picked);
      if (!fb.ok) return fb;
      body = fb.body;
    }
    put("Content-Type", "application/json");
  }

  const base = baseUrl.replace(/\/+$/, "");
  const url = `${base}${path}${qs.size ? `?${qs.toString()}` : ""}`;
  return { ok: true, req: { method, url, headers, body } };
}

/* ---------------- 可读化 ---------------- */

/** shell 单引号包裹：内容里的 `'` 写成 `'\''`，其余原样（不做变量展开）。 */
function shq(s: string): string {
  return `'${s.replace(/'/g, `'\\''`)}'`;
}

/**
 * 把已构造的请求渲染成可粘贴的 cURL。密钥**一律**替换成 `$LD_API_KEY`：
 * 这一段会被复制进聊天记录、工单和代码仓库。
 */
export function curlOf(req: BuiltTryRequest): string {
  const lines = [`curl -X ${req.method} ${shq(req.url)}`];
  for (const [k, v] of Object.entries(req.headers)) {
    const lower = k.toLowerCase();
    const shown =
      lower === "authorization"
        ? "Bearer $LD_API_KEY"
        : lower === "x-api-key"
          ? "$LD_API_KEY"
          : v;
    lines.push(`  -H ${shq(`${k}: ${shown}`)}`);
  }
  if (req.body !== undefined) lines.push(`  -d ${shq(req.body)}`);
  return lines.join(" \\\n");
}

/**
 * 失败时的一句下一步。2xx/3xx 不需要解释，返回 null。
 *
 * 只写"读者能做的事"：解释上游为什么会这样属于客服，不属于面板。
 */
export function statusHint(status: number): string | null {
  if (status >= 200 && status < 400) return null;
  if (status === 400) return "参数被拒绝：对照字段说明检查取值与必填项。";
  if (status === 401) return "密钥无效或已被停用：确认这把 Key 属于本站且状态正常。";
  if (status === 403) return "密钥无权调用：检查模型白名单、IP 白名单与分组。";
  if (status === 404) return "路径或资源不存在：核对路径参数，以及该模型是否支持这个能力。";
  if (status === 409) return "当前状态不允许该操作：先查一次状态再试。";
  if (status === 429) return "触发限流或额度不足：稍后重试，或检查账户余额。";
  if (status >= 500) return "服务端错误：稍后重试；持续失败请联系支持。";
  return null;
}

/**
 * 读完整段响应的等待上限。
 *
 * 流式与出图/视频类是长任务：30 秒会把**已经成功并已计费**的请求掐断，
 * 读者看到"超时"却不知道结果其实出来了。
 */
export function timeoutOf(doc: Pick<DocV7Endpoint, "run_mode">): number {
  return doc.run_mode === "sse" || doc.run_mode === "binary" ? 180_000 : 30_000;
}
