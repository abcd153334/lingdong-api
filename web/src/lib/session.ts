import { apiClient } from "@/lib/apiClient";
import { useAuthStore } from "@/stores/auth";
import type { MeInfo, User } from "@/types";

/**
 * 会话探测（前端唯一入口）。
 *
 * 凭证是 httpOnly Cookie，前端**读不到也判断不了**是否已登录；唯一的判据是
 * 问一次服务端 `/portal/me`。因此：
 * - 应用启动时探测一次（main.tsx）；
 * - 路由守卫在需要登录的路由上探测一次（router.tsx，`loaded` 之后不再重复）；
 * - 登录成功后直接把服务端返回的用户写进 store，避免多一次往返。
 *
 * 并发合并：首屏多个路由/组件同时调用时只发一个请求。否则刷新页面会打三次
 * `/portal/me`，其中两次必然在 401/200 之间抖动（服务端会话续期是写操作）。
 */
let inflight: Promise<User | null> | null = null;

/** 把 /portal/me 的投影收敛成 store 里的 User（字段裁剪的唯一处） */
export function toUser(me: MeInfo): User {
  return {
    id: me.id,
    username: me.username,
    role: me.role,
    balance: me.balance,
    email: me.email,
    phone: me.phone,
    group: me.group,
    avatar_url: me.avatar_url,
    // 必须搬：临时口令的强制跳转（守卫读这个字段）不能只在「刚登录」那一次生效。
    // 漏了它，刷新页面后管理员/用户就停在满屏 403 的页面上。
    must_change_password: me.must_change_password,
  };
}

/**
 * 探测当前会话。
 *
 * `force` 为真时忽略缓存重查（改密/换绑后需要刷新本地用户信息）。
 * 失败一律返回 null 并清空 store：401 就是"没登录"，网络错误也不能让 UI
 * 停留在"以为已登录"的状态（后续每个请求都会失败，用户看到满屏报错）。
 */
export function loadSession(force = false): Promise<User | null> {
  const state = useAuthStore.getState();
  if (!force && state.loaded) return Promise.resolve(state.user);
  if (inflight) return inflight;

  inflight = apiClient
    .get<MeInfo>("/portal/me")
    .then((me) => {
      const user = toUser(me);
      useAuthStore.getState().setUser(user);
      return user;
    })
    .catch(() => {
      useAuthStore.getState().clear();
      return null;
    })
    .finally(() => {
      inflight = null;
    });

  return inflight;
}

/** 清空本地鉴权态（登出、会话失效后调用；服务端 Cookie 由登出接口清除） */
export function clearSession(): void {
  useAuthStore.getState().clear();
}
