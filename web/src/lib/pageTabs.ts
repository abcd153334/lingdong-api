import { useCallback, useEffect } from "react";
import type { IconName } from "@/components/common/Icon";
import { useNavigate, useSearch } from "@tanstack/react-router";

/* ============================================================
   页签状态 ↔ URL（L1：页面级并列视图，可深链/可分享）
   ------------------------------------------------------------
   设计约束（每条都有踩坑依据，改动前先读）：
   1) 读用 useSearch({ strict: false })：useMatch 取的是「离页面最近的 match」，
      页面组件就渲染在叶子 match 里，拿到的正是本路由的 validated search。
      **不可在 layout 组件里调用**（那里拿到的是父级 match 的 {}）。
   2) 写用 navigate({ search: (…) => next })：router-core 的 buildLocation 里
      `const to = dest.to ? String(dest.to) : "."`，不传 to 就是「原地改 search」；
      search 传函数时会拿到当前 search 再合并，避免吃掉页面其它参数。
   3) 默认值即删参数：qss.encode 显式跳过 undefined，delete 后 URL 干净，
      `/admin/config` 与 `?tab=billing` 收敛成同一个规范 URL。
   4) replace 而非 push：页签是同一文档内的视图状态，push 会让「返回」
      要按 N 次才离开页面。需要 push 的页面用 opts.history 显式覆盖。
   5) resetScroll: false：切页签不滚回顶部（否则粘性保存条会「跳一下」）。
   6) 纠偏只在「有值但非法/已停用」时发生，且必须放 useEffect（渲染期导航会死循环）。
   7) tabs 必须是模块级常量：内联数组每次 render 新建，会让 setTab 抖动。
   8) 页签 key 禁用纯数字：router 的 qss.decode 走 toValue()，`?tab=7` 回来是 number。
   ============================================================ */

export interface PageTab<K extends string = string> {
  key: K;
  label: string;
  /** 计数徽标：仅视觉提示，渲染时 aria-hidden（不能进可访问名） */
  count?: number;
  /** 未保存标记（小圆点），同样 aria-hidden */
  mark?: boolean;
  /** 停用态：不可点击、URL 命中时回落 fallback、方向键跳过 */
  disabled?: boolean;
  /** 前置图标（透传给 Tabs；漏传的表现是"图标静默不渲染"） */
  icon?: IconName;
  /** 单项附加类名（透传给 Tabs，仅用于逐项定位的布局，不用于配色） */
  className?: string;
}

export interface UsePageTabsOptions {
  /** URL 参数名，默认 "tab"；同页第二组必须命名（"rec" / "scene"） */
  param?: string;
  /** 默认 "replace" */
  history?: "replace" | "push";
  /** 非法值是否回写纠偏，默认 true */
  canonicalize?: boolean;
}

/**
 * useNavigate 的 search 泛型在「同页多路由共用本 hook」的场景下无法收敛
 * （T 退化为 string 时 search 会被收窄成 never），此处用一个窄类型收口。
 * 这是本文件唯一的类型断言点，不要扩散到调用方。
 */
export type LooseNavigate = (opts: {
  search?: () => Record<string, unknown>;
  replace?: boolean;
  resetScroll?: boolean;
  hash?: string;
}) => unknown;

/**
 * 页面需要在 hook 之外做「原地改 search」（如 #fragment 兼容）时用它，
 * 避免每个页面各写一份 `as unknown as`。
 */
export function useLooseNavigate(): LooseNavigate {
  return useNavigate() as unknown as LooseNavigate;
}

/** router 的 qss.decode 会把 "7" 还原成 number、"true" 还原成 boolean、"" 还原成空串 */
function readParam(search: Record<string, unknown>, param: string): string | undefined {
  const v = search[param];
  if (typeof v === "string") return v.trim() === "" ? undefined : v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  return undefined; // 数组（?tab=a&tab=b）与对象一律视为非法
}

export interface PageTabsState<K extends string> {
  tab: K;
  setTab: (k: K) => void;
}

export function usePageTabs<K extends string>(
  tabs: readonly PageTab<K>[],
  fallback: K,
  opts: UsePageTabsOptions = {},
): PageTabsState<K> {
  const { param = "tab", history = "replace", canonicalize = true } = opts;
  const search = useSearch({ strict: false }) as Record<string, unknown>;
  const navigate = useNavigate() as unknown as LooseNavigate;
  const raw = readParam(search, param);

  // 命中必须是「存在且未停用」，否则回落默认页签
  const hit = raw === undefined ? undefined : tabs.find((t) => t.key === raw && !t.disabled);
  const valid = hit !== undefined;
  const tab = (hit?.key ?? fallback) as K;

  const setTab = useCallback(
    (key: K) => {
      const target = tabs.find((t) => t.key === key);
      if (!target || target.disabled || key === tab) return;
      const next: Record<string, unknown> = { ...search };
      if (key === fallback) delete next[param];
      else next[param] = key;
      void navigate({ search: () => next, replace: history === "replace", resetScroll: false });
    },
    [tabs, tab, fallback, param, history, navigate, search],
  );

  useEffect(() => {
    if (!canonicalize || raw === undefined || valid) return;
    const next: Record<string, unknown> = { ...search };
    delete next[param];
    void navigate({ search: () => next, replace: true, resetScroll: false });
  }, [canonicalize, raw, valid, param, search, navigate]);

  return { tab, setTab };
}
