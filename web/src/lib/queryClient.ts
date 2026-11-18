import { MutationCache, QueryCache, QueryClient } from "@tanstack/react-query";
import type { Mutation, Query } from "@tanstack/react-query";
import { toast } from "sonner";
import { isClientError } from "@/lib/apiClient";

/**
 * 双 toast 去重（P2-16）。
 *
 * 现象：页面在 `mutate(vars, { onError })` 里 toast 一次，全局 `MutationCache.onError`
 * 又 toast 一次，用户一次失败看到两条（文案还不一定一致）。
 *
 * 为什么不用 `meta.silent` 在调用点关掉全局提示：TanStack v5 的 `mutate()` 选项里
 * **没有** `meta`（只能在 useMutation 定义处设置），而同一个 hook 在不同页面有的自己
 * 处理错误、有的不处理，写死在 hook 上必然有一边是错的。
 *
 * 因此采用两层：
 *   1. 页面统一用 `error.message` 作为 toast 文案（约定见 lib/format 的 apiErrorMessage），
 *      全局这里用同一份文案 → 相同文案在 1.2s 内只显示一次；
 *   2. 确实要完全自管的 mutation 可在 useMutation 上标 `meta:{silent:true}`。
 */
const DEDUPE_MS = 1200;
let lastToast = { message: "", at: 0 };

/** 统一错误文案：优先后端 message，其余回落为通用文案。 */
export function apiErrorMessage(error: unknown, fallback = "请求失败，请稍后重试"): string {
  return error instanceof Error && error.message ? error.message : fallback;
}

function toastError(error: unknown): void {
  const message = apiErrorMessage(error);
  const now = Date.now();
  if (message === lastToast.message && now - lastToast.at < DEDUPE_MS) return;
  lastToast = { message, at: now };
  toast.error(message);
}

/** 静默查询（meta.silent=true）：全站共享但可缺失的基础数据，失败不打扰用户 */
function toastQueryError(error: unknown, query: Query<unknown, unknown, unknown, readonly unknown[]>): void {
  if (query.meta?.silent === true) return;
  toastError(error);
}

function toastMutationError(
  error: unknown,
  _vars: unknown,
  _ctx: unknown,
  mutation: Mutation<unknown, unknown, unknown, unknown>,
): void {
  if (mutation.meta?.silent === true) return;
  toastError(error);
}

/** TanStack Query 默认配置：4xx 不重试，错误统一经 sonner 提示 */
export const queryClient = new QueryClient({
  queryCache: new QueryCache({ onError: toastQueryError }),
  mutationCache: new MutationCache({ onError: toastMutationError }),
  defaultOptions: {
    queries: {
      retry: (failureCount, error) => !isClientError(error) && failureCount < 1,
      staleTime: 30_000,
      refetchOnWindowFocus: false,
    },
    mutations: {
      retry: 0,
    },
  },
});
