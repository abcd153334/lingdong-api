import { apiUrl } from "@/lib/apiBase";

/**
 * CSRF 双提交令牌（客户端唯一读取点）。
 *
 * 服务端在会话建立时种一个**非 HttpOnly** 的 `ld_csrf` Cookie，要求所有
 * 状态变更请求把它原样放进 `X-CSRF-Token` 头。攻击者能替受害者发请求，
 * 但读不到这个 Cookie（同源策略），因此伪造不出头。
 *
 * 为什么不用「从 localStorage 取」：那样令牌就不是服务端下发的了，
 * 也无法随会话一起失效（登出后残留的旧令牌会继续被当成合法值）。
 */

const CSRF_COOKIE = "ld_csrf";
const CSRF_HEADER = "X-CSRF-Token";

/** 需要携带 CSRF 头的方法（GET/HEAD/OPTIONS 是安全方法，不校验） */
const UNSAFE_METHODS = new Set(["POST", "PUT", "PATCH", "DELETE"]);

/** 读取 CSRF Cookie（不存在返回空串） */
export function csrfToken(): string {
  if (typeof document === "undefined") return "";
  const hit = document.cookie
    .split(";")
    .map((part) => part.trim())
    .find((part) => part.startsWith(`${CSRF_COOKIE}=`));
  if (!hit) return "";
  return decodeURIComponent(hit.slice(CSRF_COOKIE.length + 1));
}

/** 按方法返回应当附加的 CSRF 头（安全方法返回空对象） */
export function csrfHeaders(method: string): Record<string, string> {
  if (!UNSAFE_METHODS.has(method.toUpperCase())) return {};
  const token = csrfToken();
  return token ? { [CSRF_HEADER]: token } : {};
}
