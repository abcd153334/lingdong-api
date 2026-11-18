/**
 * 试运行的**唯一**请求构造实现（纯函数，不碰 DOM、不碰 fetch）。
 *
 * 为什么单独抽一层：
 *
 *  1. 这一层决定"发出去的请求长什么样"，是整块功能里唯一会**改生产数据**的地方。
 *     混在组件里就只能靠点几下按钮来验证 —— 而"必填校验漏一条"这种事，
 *     点按钮是发现不了的（漏的那条恰好填了）。
 *  2. curl / fetch 两个片段必须与实际发出的请求**逐字一致**：接入方会复制粘贴，
 *     如果片段里 header 少一个、body 少一层，他们会在自己的代码里再踩一遍。
 *     因此三个出口都从同一个 BuiltRequest 派生，而不是各拼各的。
 *
 * 三条硬约定：
 *   - 路径参数从 `:id` 形态替换（契约里的写法就是 gin 写法）。**没填时保留占位符**
 *     并计入 `missing`，不抛错 —— 这个函数在用户逐字输入时也会被调用（实时预览
 *     URL），抛错会让"还没输完"变成一次崩溃。占位符留着也让人一眼看到差什么，
 *     比拼出一个 `/keys/` 的空段诚实得多。
 *   - query 只从 `in=query` 的参数来；body 只从 `in=body|form` 来。
 *     混着放会出现"body 里的字段被当成 query 拼到 URL 上"（服务端读不到，
 *     接入方以为参数名错了）。
 *   - 值一律先按 type 归一（integer → Number、array → 数组），
 *     不做隐式字符串兜底：`"3"` 与 `3` 在 JSON 里是不同的东西。
 */

import type { ApiDocEndpoint, ApiDocParam } from "@/types";
import { apiUrl } from "@/lib/apiBase";

/** 表单里的原始输入：全部是字符串（input/select/textarea 的天然形态） */
export type ParamValues = Record<string, string>;

/** 一次待发送的请求（还差一个 origin：站内路径 vs 绝对地址由调用方决定） */
export interface BuiltRequest {
  method: string;
  /** 站内绝对路径（以 `/` 开头）或跨域绝对地址，取决于 API_BASE_URL */
  url: string;
  headers: Record<string, string>;
  /** JSON 文本；该端点没有 body 时为 undefined（不是空串） */
  body?: string;
  /**
   * 必填但没填的参数名（含路径参数）。
   *
   * 非空时 UI **禁止发送**：让服务端返回 400 既浪费一次往返，
   * 也会让接入方以为参数名写错了。
   */
  missing: string[];
}

/** 契约路径（`/api/user/keys/:id`）→ API 基址之后的相对路径（`/user/keys/:id`）。 */
export function userApiPath(contractPath: string): string {
  // API_BASE_URL 已经含 `/api`（默认 `/api`，跨域部署时是 `https://host/api`），
  // 而契约路径自带 `/api` 前缀。直接拼会得到 `/api/api/user/self`。
  return contractPath.replace(/^\/api(?=\/)/, "");
}

/** 该端点是否接受请求体。 */
export function hasBody(e: ApiDocEndpoint): boolean {
  return e.params.some((p) => p.in === "body" || p.in === "form");
}

/** 按 type 归一一个原始输入；空串返回 undefined（表示"不传"）。 */
function coerce(p: ApiDocParam, raw: string): unknown {
  const v = raw.trim();
  if (v === "") return undefined;
  switch (p.type) {
    case "integer": {
      const n = Number(v);
      // 非整数不猜：`1.5` 传给 integer 会被服务端拒，前端擅自取整更糟。
      return Number.isInteger(n) ? n : v;
    }
    case "number": {
      const n = Number(v);
      return Number.isFinite(n) ? n : v;
    }
    case "boolean":
      // 只认 true/false：`1`/`yes` 在服务端的 binding 里未必认。
      return v.toLowerCase() === "true";
    case "array":
      return splitArray(v);
    default:
      return v;
  }
}

/**
 * 数组输入的分隔规则：换行或逗号，去空白、丢空项。
 *
 * 只支持这两种，不做 JSON 解析：接入方会从文档里复制一行行的值，
 * 让他写成 `["a","b"]` 才是反人性。
 */
export function splitArray(raw: string): string[] {
  return raw
    .split(/[\n,]/)
    .map((s) => s.trim())
    .filter((s) => s !== "");
}

/**
 * 替换路径参数。
 *
 * 没填的参数**保留 `:name` 占位符**并回报，而不是替换成空串：
 * 替换成空串会得到 `/keys/` 这样一个看起来正常、实际打到另一条路由的地址。
 */
function fillPath(path: string, values: ParamValues): { path: string; missing: string[] } {
  const missing: string[] = [];
  const filled = path.replace(/:([A-Za-z0-9_]+)/g, (_, name: string) => {
    const v = (values[name] ?? "").trim();
    if (v === "") {
      missing.push(name);
      return `:${name}`;
    }
    return encodeURIComponent(v);
  });
  return { path: filled, missing };
}

/** 保序去重（缺失清单要稳定，否则每次输入 key 都在抖）。 */
function dedupe(names: string[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const n of names) {
    if (seen.has(n)) continue;
    seen.add(n);
    out.push(n);
  }
  return out;
}

/**
 * 构造一次请求。
 *
 * values 的 key 是参数名（path/query/body 同一个空间）。同名跨空间的情况
 * 契约里目前没有，若将来出现，body 会覆盖 query —— 那时应该先改契约。
 *
 * `rawBody` 用来接「手写 JSON 体」：传了它就**原样当请求体**，一个字段都不从
 * values 补。两种来源二选一 —— 同时生效必然出现"表单改了、发出去的是旧 JSON"，
 * 而界面上没有任何线索能解释这件事。空串按 `{}` 处理（有体端点的 body 是必填的，
 * "不传"与"空对象"在服务端是两种结果）。
 */
export function buildRequest(
  e: ApiDocEndpoint,
  values: ParamValues,
  rawBody?: string,
): BuiltRequest {
  const { path, missing: pathMissing } = fillPath(userApiPath(e.path), values);

  // query：按契约声明顺序拼接（顺序稳定 → curl 片段可 diff）。
  const qs = new URLSearchParams();
  for (const p of e.params) {
    if (p.in !== "query") continue;
    const v = coerce(p, values[p.name] ?? "");
    if (v === undefined) continue;
    if (Array.isArray(v)) {
      // 数组型 query 用重复键（`?tag=a&tag=b`）：逗号分隔在服务端
      // 通常会被当成一个值，这是最容易踩的一个坑。
      for (const item of v) qs.append(p.name, item);
    } else {
      qs.set(p.name, String(v));
    }
  }
  const query = qs.toString();

  const bodyParams = e.params.filter((p) => p.in === "body" || p.in === "form");
  let body: string | undefined;
  if (rawBody !== undefined) {
    body = rawBody.trim() === "" ? "{}" : rawBody.trim();
  } else if (bodyParams.length) {
    const obj: Record<string, unknown> = {};
    for (const p of bodyParams) {
      const v = coerce(p, values[p.name] ?? "");
      if (v !== undefined) obj[p.name] = v;
    }
    // 空对象也发：部分端点的 body 是必填的，`{}` 与"不传"在服务端
    // 是两种结果（一个是参数错，一个是缺 body）。
    body = JSON.stringify(obj);
  }

  const headers: Record<string, string> = { Accept: "application/json" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  // 只有需要凭证时才提 Bearer：免鉴权端点带上一个假 token 也是噪音。
  if (e.auth !== "open") headers.Authorization = "Bearer <你的密钥>";

  return {
    method: e.method.toUpperCase(),
    url: apiUrl(path) + (query ? `?${query}` : ""),
    headers,
    body,
    // 路径参数与必填缺失合并去重：两处都判会重复报同一个名字。
    // 手写 JSON 体时 body 必填不再参与：那几项在界面上已经隐藏，
    // 算进 missing 只会让「发送」永远禁用（读者以为是自己没填）。
    missing: dedupe([
      ...pathMissing,
      ...missingRequired(e, values, { skipBody: rawBody !== undefined }),
    ]),
  };
}

/**
 * 必填校验。
 *
 * 条件必填（conditional）**不**在这里判：`billing.orders_create` 的
 * package_id / amount 是二选一，判据只能写在契约里给人看
 * （`required_note`）。硬编码一条"至少填一个"会在契约变化时静默错误。
 * 因此 conditional 一律不阻止发送 —— 服务端会给出准确原因。
 */
export function missingRequired(
  e: ApiDocEndpoint,
  values: ParamValues,
  opts: { skipBody?: boolean } = {},
): string[] {
  const out: string[] = [];
  for (const p of e.params) {
    if (p.required !== "required") continue;
    if (opts.skipBody && (p.in === "body" || p.in === "form")) continue;
    if ((values[p.name] ?? "").trim() === "") out.push(p.name);
  }
  return out;
}

/** 把站内路径补成绝对地址（curl / fetch 片段必须是可直接粘贴的）。 */
export function absoluteURL(url: string, origin: string): string {
  if (/^https?:\/\//i.test(url)) return url;
  return origin.replace(/\/+$/, "") + (url.startsWith("/") ? url : `/${url}`);
}

/**
 * 遮蔽凭证。
 *
 * 片段是要发到群里/贴进工单的，把真密钥带出去是最常见的泄漏方式。
 * 只遮蔽 Authorization 一类，不动业务参数。
 */
export function redactHeaders(headers: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(headers)) {
    const key = k.toLowerCase();
    if (key === "authorization" || key === "x-api-key") {
      out[k] = "Bearer <你的密钥>";
    } else {
      out[k] = v;
    }
  }
  return out;
}

/** 单引号安全包裹（curl 里 `'` 要拆成 `'\''`）。 */
function shellQuote(s: string): string {
  return `'${s.replace(/'/g, `'\\''`)}'`;
}

/** 生成可粘贴的 curl 命令。 */
export function toCurl(req: BuiltRequest, origin: string): string {
  const lines = [`curl -X ${req.method} ${shellQuote(absoluteURL(req.url, origin))}`];
  for (const [k, v] of Object.entries(redactHeaders(req.headers))) {
    lines.push(`  -H ${shellQuote(`${k}: ${v}`)}`);
  }
  if (req.body !== undefined) {
    lines.push(`  -d ${shellQuote(req.body)}`);
  }
  return lines.join(" \\\n");
}

/** 生成可粘贴的 fetch 片段（与 curl 同一份构造结果，不是另写一份）。 */
export function toFetchSnippet(req: BuiltRequest, origin: string): string {
  const headers = JSON.stringify(redactHeaders(req.headers), null, 2);
  const bodyLine = req.body !== undefined ? `,\n  body: ${JSON.stringify(req.body)}` : "";
  return `const res = await fetch(${JSON.stringify(absoluteURL(req.url, origin))}, {
  method: ${JSON.stringify(req.method)},
  headers: ${headers}${bodyLine},
});
const json = await res.json();`;
}
