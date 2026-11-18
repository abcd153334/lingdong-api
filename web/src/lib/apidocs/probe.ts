/**
 * 试运行的真实请求执行器。
 *
 * 为什么**绝不能**走 `apiClient`：
 *
 *  1. `apiClient` 在 401 时会触发全局登出（清 store + 跳 `/login`）。
 *     而这里 401 的含义是「你贴的密钥不对」—— 用管理台会话过期的方式处理，
 *     会让管理员在试一条接口时被踢出后台，而且他会以为是系统挂了。
 *  2. `apiClient` 会注入管理台会话票。`/api/user/*` 认的是**接入方的 API KEY**，
 *     带上会话票会让请求"看起来通过了鉴权"，验证不出接入方真实会遇到的形状。
 *  3. 响应可能是 CSV / 二进制（`usage.export` 之类的导出接口），
 *     `apiClient` 会按 JSON 信封解析并抛 `ApiError`。
 *
 * 因此这里用裸 `fetch`，`credentials: "omit"`（连 Cookie 都不带），
 * 并且**不把非 2xx 当成异常**：HTTP 状态是结果的一部分（试运行要能看清 400/403 的
 * 错误体长什么样），只有网络层失败才抛错。
 */

import type { BuiltRequest } from "./request";

/** 一次试运行的结果（不管是成功还是 4xx/5xx，都返回它而不是抛错） */
export interface ProbeResult {
  /** HTTP 2xx */
  ok: boolean;
  status: number;
  statusText: string;
  /** 从发出到收到响应头 + 读完体的耗时（毫秒，取整） */
  ms: number;
  /** 响应头里的 Content-Type（可能为空） */
  contentType: string;
  /** 响应体文本；非文本类型时为一句说明（见 bodyNote） */
  body: string;
  /** 响应体字节数（读到的，不是 Content-Length） */
  bytes: number;
  /** 实际生效的地址（重定向后由 fetch 给出；拿不到时用请求地址） */
  url: string;
  /**
   * 二进制预览地址（仅当调用方要 `wantBinary` 且响应确实是媒体类型时）。
   *
   * 由 `URL.createObjectURL` 生成，**调用方负责 revoke**（组件卸载时），
   * 否则每试一次就往内存里留一份图片/音频。
   */
  blobUrl?: string;
}

/** 试运行的调用参数：鉴权头形态与是否要二进制预览。 */
export interface ProbeOptions {
  /**
   * 真实密钥写进哪个头。
   *
   * 默认 `authorization`（Bearer）；`/v1/messages` 认 `x-api-key`，
   * 传 `"x-api-key"` —— 否则这条 Anthropic 协议接口必 401。
   */
  authHeader?: "authorization" | "x-api-key";
  /** 响应是媒体类型时额外产出 `blobUrl`（图片/音频/视频预览与下载）。 */
  wantBinary?: boolean;
}

/**
 * 响应体最多读取的字节数。
 *
 * 试运行是给"看一眼"用的：导出类接口可能返回几十 MB 的 CSV，
 * 塞进 DOM 会让页面卡死。超过上限只报告大小，不渲染内容。
 */
export const MAX_BODY_BYTES = 64 * 1024;

/** 这些 Content-Type 直接按二进制处理（不尝试转文本）。 */
function isBinaryType(ct: string): boolean {
  const t = ct.toLowerCase();
  if (t.startsWith("text/")) return false;
  if (t.includes("json") || t.includes("xml") || t.includes("javascript")) return false;
  if (t.includes("x-www-form-urlencoded")) return false;
  return true;
}

/** 试运行真正需要的最小请求形状（`missing` 是构造侧的校验信息，发送不需要）。 */
export type ProbePayload = Pick<BuiltRequest, "method" | "url" | "headers" | "body">;

/**
 * 执行一次试运行。
 *
 * apiKey 为空时不带鉴权头（免鉴权端点就该这样）；
 * 非空时按 `opts.authHeader` 决定写哪个头：默认 `Authorization: Bearer`，
 * `x-api-key` 用于 Anthropic 原生协议（`/v1/messages`）。
 */
export async function probeRequest(
  req: ProbePayload,
  apiKey: string,
  signal?: AbortSignal,
  opts?: ProbeOptions,
): Promise<ProbeResult> {
  const authHeader = opts?.authHeader ?? "authorization";
  const headers: Record<string, string> = {};
  for (const [k, v] of Object.entries(req.headers)) {
    // 契约里的鉴权头只是给片段用的占位符，真实值由 apiKey 决定。
    // 两种都剔：否则一次请求会同时带 Authorization 与 x-api-key。
    const lower = k.toLowerCase();
    if (lower === "authorization" || lower === "x-api-key") continue;
    headers[k] = v;
  }
  const key = apiKey.trim();
  if (key) {
    if (authHeader === "x-api-key") headers["x-api-key"] = key;
    else headers.Authorization = `Bearer ${key}`;
  }

  const started = now();
  const res = await fetch(req.url, {
    method: req.method,
    headers,
    body: req.body,
    // 连 Cookie 都不带：这条请求代表"接入方的调用"，不是"管理员的会话"。
    credentials: "omit",
    // 不缓存：试运行要看到当下的真实结果。
    cache: "no-store",
    signal,
  });

  const contentType = res.headers.get("content-type") ?? "";

  // 媒体响应：直接把字节交给 blob，调用方拿到的是可预览/可下载的地址。
  // 不走文本预览 —— 图片转成乱码塞进 <pre> 既看不懂又占内存。
  if (opts?.wantBinary && isBinaryType(contentType)) {
    const blob = await res.blob();
    const elapsed = now() - started;
    return {
      ok: res.ok,
      status: res.status,
      statusText: res.statusText,
      ms: Math.round(elapsed),
      contentType,
      body: `（二进制内容 ${formatBytes(blob.size)}，${contentType || "未知类型"}）`,
      bytes: blob.size,
      url: res.url || req.url,
      blobUrl: URL.createObjectURL(blob),
    };
  }

  const raw = await res.arrayBuffer();
  const elapsed = now() - started;
  const bytes = raw.byteLength;

  let body: string;
  if (bytes > MAX_BODY_BYTES) {
    body = `（响应 ${formatBytes(bytes)}，只预览前 ${formatBytes(MAX_BODY_BYTES)}）\n\n${decodePreview(raw, contentType)}`;
  } else if (isBinaryType(contentType) && bytes > 0) {
    body = `（二进制内容 ${formatBytes(bytes)}，${contentType || "未知类型"}）`;
  } else {
    body = decodePreview(raw, contentType);
  }

  return {
    ok: res.ok,
    status: res.status,
    statusText: res.statusText,
    ms: Math.round(elapsed),
    contentType,
    body,
    bytes,
    url: res.url || req.url,
  };
}

/** 只解前 MAX_BODY_BYTES，避免为一个巨大文件构造整段字符串。 */
function decodePreview(raw: ArrayBuffer, contentType: string): string {
  const slice = raw.slice(0, MAX_BODY_BYTES);
  const text = new TextDecoder("utf-8", { fatal: false }).decode(slice);
  if (contentType.toLowerCase().includes("json")) return prettifyJSON(text);
  return text;
}

/** JSON 美化；不是合法 JSON 时原样返回（不吞掉服务端返回的纯文本错误）。 */
export function prettifyJSON(text: string): string {
  const t = text.trim();
  if (!t) return "";
  if (!t.startsWith("{") && !t.startsWith("[")) return text;
  try {
    return JSON.stringify(JSON.parse(t), null, 2);
  } catch {
    return text;
  }
}

/** 字节数的可读形式（只在提示里用）。 */
export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

/** 单调时钟优先：Date.now 会被系统时间调整影响，测出的耗时可能为负。 */
function now(): number {
  return typeof performance !== "undefined" ? performance.now() : Date.now();
}
