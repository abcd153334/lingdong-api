import { create } from "zustand";
import type { User } from "@/types";

interface AuthState {
  /** 当前登录用户；null 表示未登录（或尚未探测完） */
  user: User | null;
  /**
   * 是否已完成一次会话探测。
   *
   * 为什么必须有这个位：凭证现在在 httpOnly Cookie 里，**前端读不到**，
   * 「未登录」与「还没问过服务端」在 store 里长得一模一样。路由守卫若把
   * 后者当成前者，刷新任意受保护页面都会先闪一下登录页。
   */
  loaded: boolean;
  setUser: (user: User | null) => void;
  markLoaded: () => void;
  clear: () => void;
}

/**
 * 鉴权态（服务端会话）。
 *
 * 与改造前的差别：不再持有 token，也不再 persist。
 * - 令牌在 httpOnly Cookie 里，JS 读不到（XSS 拿不走凭证）；
 * - 用户信息是**每次启动从 /portal/me 取的**，不是 localStorage 里那份陈旧的快照
 *   （旧实现里管理员被降权后，本地还显示着"管理员"徽标）。
 */
export const useAuthStore = create<AuthState>((set) => ({
  user: null,
  loaded: false,
  setUser: (user) => set({ user, loaded: true }),
  markLoaded: () => set({ loaded: true }),
  clear: () => set({ user: null, loaded: true }),
}));
