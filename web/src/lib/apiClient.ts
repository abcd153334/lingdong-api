import { useAuthStore } from "@/stores/auth";
import { apiUrl } from "@/lib/apiBase";
import { csrfHeaders } from "@/lib/csrf";
import type { ApiResponse, PageResult } from "@/types";

/** T21 分页每页条数（冻结契约） */
export const DEFAULT_PAGE_SIZE = 20;

/** 业务错误（code!=0 或 HTTP 非 2xx） */
export class ApiError extends Error {
  readonly code: number;
  /** 机器可判读的业务错误码（C-06，如 username_taken）；未下发时为空串 */
  readonly errorCode: string;
  /**
   * 失败响应里的结构化 data。
   *
   * 目前只有"探测闸门"用到（缺哪些目标、为什么）。只取 message 的后果是
   * 前端无法逐条渲染，运营只能看到一句"需要先探测"而无从下手。
   */
  readonly data: unknown;

  constructor(code: number, message: string, errorCode = "", data: unknown = null) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.errorCode = errorCode;
    this.data = data;
  }
}

/** 4xx 属确定性失败：不重试（供 Query retry 判定） */
export function isClientError(error: unknown): boolean {
  return error instanceof ApiError && error.code >= 400 && error.code < 500;
}

let unauthorizedHandler: (() => void) | null = null;
let forbiddenHandler: ((message: string) => void) | null = null;
let mfaSetupHandler: (() => void) | null = null;
let portalDisabledHandler: (() => void) | null = null;

/** 注册 401 全局拦截处理（由应用入口注入，如跳转 /login） */
export function setUnauthorizedHandler(handler: () => void): void {
  unauthorizedHandler = handler;
}

/** 注册 403 全局拦截处理（由应用入口注入，如 toast 提示） */
export function setForbiddenHandler(handler: (message: string) => void): void {
  forbiddenHandler = handler;
}

/**
 * 注册「必须先绑定两步验证」的处理（403 mfa_setup_required）。
 *
 * 为什么不复用 forbiddenHandler：那条会把所有 403 都变成 toast。而这个码的
 * 语义是「换个页面就能继续」，前端必须**导航**到绑定页 —— 只弹提示的话，
 * 管理员会停在满屏 403 的管理台里反复点同一个按钮。
 */
export function setMFASetupHandler(handler: () => void): void {
  mfaSetupHandler = handler;
}

/**
 * 注册「门户端已关闭」的处理（403 portal_disabled）。
 *
 * 它有两种来源：会员会话被服务端作废、或某个门户端点被关闭的门户闸拒绝。
 * 两者对前端的含义相同 —— 这个会话在门户上已经没有意义了，所以走和 401
 * 同样的收尾（清本地态 + 交给路由守卫裁决），而不是弹一个 toast 让人停在死页面上。
 *
 * 具体回调由入口注入（失效门户模式缓存），避免 apiClient ↔ portalMode 循环依赖。
 */
export function setPortalDisabledHandler(handler: () => void): void {
  portalDisabledHandler = handler;
}

function handleUnauthorized(): void {
  useAuthStore.getState().clear();
  if (unauthorizedHandler) {
    unauthorizedHandler();
    return;
  }
  if (typeof window !== "undefined" && window.location.pathname !== "/login") {
    window.location.assign("/login");
  }
}

export interface RequestOptions extends Omit<RequestInit, "body"> {
  body?: unknown;
  /** T21 分页/筛选参数：undefined 值自动忽略 */
  query?: Record<string, string | number | boolean | undefined | null>;
}

/** T21 分页请求参数 */
export interface PageQuery {
  page?: number;
  page_size?: number;
  /** 其余筛选字段 */
  [key: string]: string | number | boolean | undefined | null;
}

/** 拼接请求地址并挂载查询参数（undefined/null/空串自动忽略） */
function resolveURL(path: string, query?: PageQuery): string {
  const url = new URL(
    apiUrl(path),
    typeof window !== "undefined" ? window.location.origin : "http://localhost",
  );
  if (query) {
    for (const [key, value] of Object.entries(query)) {
      if (value !== undefined && value !== null && value !== "") {
        url.searchParams.set(key, String(value));
      }
    }
  }
  return url.toString();
}

/**
 * 统一 fetch 选项：**所有**请求都带 `credentials: "include"`。
 *
 * 凭证是 httpOnly Cookie，不带这一项时浏览器不会发它，表现为「登录成功后
 * 每个接口都 401」——而 Cookie 明明在。这是从 Bearer 迁移到 Cookie 最容易
 * 漏的一处，所以下面三个 fetch 调用点都统一走这个函数，不各写一遍。
 */
function withCredentials(init: RequestInit): RequestInit {
  return { ...init, credentials: "include" };
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { body, query, headers, ...rest } = options;

  const url = resolveURL(path, query);
  const method = (rest.method ?? "GET").toUpperCase();

  const finalHeaders: Record<string, string> = {
    "Content-Type": "application/json",
    ...csrfHeaders(method),
    ...(headers as Record<string, string> | undefined),
  };

  let res: Response;
  try {
    res = await fetch(
      url.toString(),
      withCredentials({
        ...rest,
        headers: finalHeaders,
        body: body === undefined ? undefined : JSON.stringify(body),
      }),
    );
  } catch {
    throw new ApiError(-1, "网络异常，请检查连接后重试");
  }

  return parseResponse<T>(res, path);
}

/**
 * multipart 上传（图标等二进制表单）。
 *
 * 为什么不复用 request：request 恒设 `Content-Type: application/json` 并
 * `JSON.stringify` 请求体。对 FormData 这样做有两个后果 ——
 * 手写的 `application/json` 头会让服务端按 JSON 解析表单（`c.FormFile` 直接失败），
 * 而边界串 boundary 又必须由浏览器自己生成，前端无法拼出来。
 *
 * 所以这里**不设 Content-Type**，其余（凭证、CSRF、错误体解析、401 语义）
 * 与 request 完全共用：两条路径各写一份错误处理，必然出现「上传失败时不提示」
 * 或「上传时 401 不登出」这种不对称缺陷。
 */
export async function upload<T>(path: string, form: FormData): Promise<T> {
  let res: Response;
  try {
    res = await fetch(
      resolveURL(path),
      withCredentials({
        method: "POST",
        headers: { ...csrfHeaders("POST") },
        body: form,
      }),
    );
  } catch {
    throw new ApiError(-1, "网络异常，请检查连接后重试");
  }
  return parseResponse<T>(res, path);
}

/**
 * 统一响应体解析与会话拦截（request / upload 的**唯一**实现）。
 *
 * 搬出来而不是复制：401 会话失效、403 权限提示、mfa_setup_required 导航
 * 这三条是全局语义，任何一条在新通道里漏掉，表现都是「这个接口的行为
 * 和别的接口不一样」，且只在会话过期时才复现。
 */
/**
 * 401 的**唯一**判据：哪些 401 代表"你的会话失效"。
 *
 * 401 至少有四种含义：会话失效 / 目标凭证被禁用 / 目标凭证已被轮换 / 口令错。
 * 改造前这里写的是"除 invalid_credential 之外都算掉线"，于是任何一个讲**目标**
 * 凭证的 401（例如"你要轮换的这把钥被平台封禁了"）都会被当成"你的登录掉了"：
 * 清空本地态 → 守卫重跑 → 因为 loaded 仍为 true，`loadSession` 直接返回 null
 * 不再探测 → 用户带着完全有效的会话被踢回登录页。症状与真实掉线一模一样。
 *
 * 所以判据必须是**白名单**：只有服务端明确表示"会话"的两个码才算。
 * 新增端点若返回别的 401 码，正确处理是就地提示业务错误，而不是登出。
 */
const SESSION_INVALID_CODES = new Set(["", "unauthorized", "session_expired"]);

function isSessionExpired(status: number, errorCode: string, path: string): boolean {
  if (status !== 401) return false;
  // 公开认证端点（登录/注册/找回）的 401 是"表单填错了"，由页面内联展示。
  if (path.startsWith("/portal/auth/")) return false;
  return SESSION_INVALID_CODES.has(errorCode);
}

async function parseResponse<T>(res: Response, path: string): Promise<T> {
  let payload: ApiResponse<T> | null = null;
  try {
    payload = (await res.json()) as ApiResponse<T>;
  } catch {
    payload = null;
  }

  const errorCode = payload?.error_code ?? "";
  const bizCode = payload?.code ?? res.status;

  // 公开认证端点（登录/注册/找回）的错误由页面内联展示：表单就在旁边，
  // 再走一次全局拦截会重复提示，且 403 的 toast 文案与页面文案可能打架。
  const isPublicAuth = path.startsWith("/portal/auth/");

  // 401 有两种语义：① 会话失效（error_code 为 unauthorized / session_expired）
  // → 清会话并让守卫裁决；② 其它（invalid_credential 登录口令错、key_disabled
  // 目标钥被封、key_retired 该换钥了…）→ 属业务错误，绝不能登出用户。
  // 判据集中在 isSessionExpired，raw 下载路径共用同一份。
  if (isSessionExpired(res.status, errorCode, path)) {
    handleUnauthorized();
    throw new ApiError(401, payload?.message || "登录已过期，请重新登录", errorCode);
  }

  if (res.status === 403 && errorCode === "mfa_setup_required") {
    const message = payload?.message || "请先完成两步验证绑定";
    mfaSetupHandler?.();
    throw new ApiError(403, message, errorCode);
  }

  // 门户端关闭：与「会话失效」同构收尾（清本地态 + 交给守卫），不弹 toast。
  // 放在通用 403 之前，否则会员会停在一片「门户端已关闭」的提示里不动。
  if (res.status === 403 && errorCode === "portal_disabled") {
    portalDisabledHandler?.();
    handleUnauthorized();
    throw new ApiError(403, payload?.message || "门户端已关闭，仅管理员可登录", errorCode);
  }

  if (res.status === 403) {
    const message = payload?.message || "没有权限执行该操作";
    if (!isPublicAuth) forbiddenHandler?.(message);
    throw new ApiError(403, message, errorCode);
  }

  if (!payload || bizCode !== 0) {
    throw new ApiError(
      bizCode,
      payload?.message || `请求失败（HTTP ${res.status}）`,
      errorCode,
      payload?.data ?? null,
    );
  }
  return payload.data;
}

/**
 * 从 Content-Disposition 解析文件名（缺失返回 null）。
 *
 * RFC 6266：同时出现 `filename=` 与 `filename*=` 时，**`filename*` 优先** ——
 * 前者只能是 ASCII（服务端一般填英文兜底名），后者带 `UTF-8''` 声明、能表达中文。
 * 曾经只用一个正则取第一个命中，于是"中文文件名"永远被那个英文兜底名顶掉。
 */
export function filenameFrom(disposition: string | null): string | null {
  if (!disposition) return null;
  const ext = /filename\*\s*=\s*utf-8''([^;]+)/i.exec(disposition);
  if (ext) {
    try {
      return decodeURIComponent(ext[1].trim().replace(/^"|"$/g, ""));
    } catch {
      // 编码坏了就退回 ASCII 名，不要因为文件名让整个下载失败
    }
  }
  const plain = /filename\s*=\s*"?([^";]+)"?/i.exec(disposition);
  return plain ? plain[1].trim() : null;
}

/**
 * CSV 导出下载（S11）：凭会话 Cookie 取回 Blob 并触发浏览器保存。
 * 失败体仍走统一错误体解析，401 与会话失效同 request 语义。
 */
export async function downloadCSV(
  path: string,
  query: PageQuery | undefined,
  fallbackName: string,
): Promise<void> {
  let res: Response;
  try {
    res = await fetch(resolveURL(path, query), withCredentials({}));
  } catch {
    throw new ApiError(-1, "网络异常，请检查连接后重试");
  }
  if (!res.ok) {
    let message = `导出失败（HTTP ${res.status}）`;
    let code: number = res.status;
    let errorCode = "";
    try {
      const payload = (await res.json()) as ApiResponse<unknown>;
      message = payload?.message || message;
      code = payload?.code ?? code;
      errorCode = payload?.error_code ?? "";
    } catch {
      /* 非 JSON 错误体：保留 HTTP 文案 */
    }
    // 与 request 共用同一份"什么才算会话失效"的判据（曾经三处各写一遍）。
    if (isSessionExpired(res.status, errorCode, path)) handleUnauthorized();
    throw new ApiError(code, message, errorCode);
  }
  const blob = await res.blob();
  const objectURL = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = objectURL;
  anchor.download = filenameFrom(res.headers.get("Content-Disposition")) ?? fallbackName;
  anchor.click();
  // 异步 revoke：与 click 同步 revoke 会与下载管道初始化竞态（高负载下下载静默失败）
  window.setTimeout(() => URL.revokeObjectURL(objectURL), 4_000);
}

/**
 * 带鉴权的二进制取回（任务产物 / 管理台排障下载共用）。
 *
 * 这些端点挂会话鉴权，浏览器直链导航虽然会带 Cookie，但 401 时会渲染成
 * 一段 JSON 错误页而不是提示；故用带凭证的 fetch 取回 Blob 后在新标签打开。
 * 后端未转存产物时按「降级代理模式实时拉上游」回传字节流，故 fetch 可一次性取回。
 */
async function fetchAuthedBlob(path: string): Promise<Blob> {
  let res: Response;
  try {
    res = await fetch(resolveURL(path), withCredentials({}));
  } catch {
    throw new ApiError(-1, "网络异常，请检查连接后重试");
  }
  if (!res.ok) {
    let message = `取回失败（HTTP ${res.status}）`;
    let code: number = res.status;
    let errorCode = "";
    try {
      const payload = (await res.json()) as ApiResponse<unknown>;
      message = payload?.message || message;
      code = payload?.code ?? code;
      errorCode = payload?.error_code ?? "";
    } catch {
      /* 非 JSON 错误体：保留 HTTP 文案 */
    }
    // 与 request 共用同一份"什么才算会话失效"的判据（曾经三处各写一遍）。
    if (isSessionExpired(res.status, errorCode, path)) handleUnauthorized();
    throw new ApiError(code, message, errorCode);
  }
  return res.blob();
}

/**
 * 取回任务产物的 Blob（供详情页内联预览）。
 *
 * 门户与管理台各一个路由组，但形状一致：都是 `/tasks/<id>/contents/<index>`。
 * 门户曾有 `/content`（= 第 0 个）的兼容入口，已删——两条路径等于两套前端分支，
 * 且 index 越大越容易走到没人测过的那条。
 */
export async function fetchTaskContentBlob(id: string, index: number, admin: boolean): Promise<Blob> {
  const base = admin ? "/admin/tasks" : "/portal/tasks";
  return fetchAuthedBlob(`${base}/${id}/contents/${index}`);
}

/**
 * 取回同步调用产物的 Blob（门户详情内联预览）。
 *
 * 上游直链不下发到前端（域名本身就是供应链信息），因此统一走平台代理端点；
 * 端点会做归属校验（越权 404）、体积上限与 MIME 白名单。
 */
export async function fetchUsageMediaBlob(usageId: number, index: number): Promise<Blob> {
  return fetchAuthedBlob(`/portal/usage/${usageId}/media/${index}`);
}

export const apiClient = {
  get: <T>(path: string, options?: RequestOptions) =>
    request<T>(path, { ...options, method: "GET" }),
  /** T21 分页查询（GET 透传 page/page_size 与筛选参数） */
  getPage: <T>(path: string, query?: PageQuery, options?: RequestOptions) =>
    request<PageResult<T>>(path, {
      ...options,
      method: "GET",
      query: { page: 1, page_size: DEFAULT_PAGE_SIZE, ...query },
    }),
  post: <T>(path: string, body?: unknown, options?: RequestOptions) =>
    request<T>(path, { ...options, method: "POST", body }),
  put: <T>(path: string, body?: unknown, options?: RequestOptions) =>
    request<T>(path, { ...options, method: "PUT", body }),
  /** S2 契约使用 PATCH（/portal/me、/admin/users/{id}）；冻结层原缺此方法，按需补齐 */
  patch: <T>(path: string, body?: unknown, options?: RequestOptions) =>
    request<T>(path, { ...options, method: "PATCH", body }),
  delete: <T>(path: string, options?: RequestOptions) =>
    request<T>(path, { ...options, method: "DELETE" }),
};
