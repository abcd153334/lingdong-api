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

/**
 * 站点级公开数据：与「当前是谁」无关，切号后继续复用。
 *
 * `portal/config` 是站点配置（币种/认证策略），`portal/packages` 是套餐目录，
 * `pricing` 是公开价目表 —— 三者都不带 uid。
 *
 * **不在此列的 portal 键都会被清**，特别是：
 *   - `portal/defaults`：按当前用户的密钥返回"哪些模型可用"，是账户级；
 *   - `portal/announcements`：带个人已读标记，是账户级。
 */
const ACCOUNT_PUBLIC_KEYS: readonly (readonly unknown[])[] = [
  ["pricing"],
  ["portal", "config"],
  ["portal", "packages"],
];

/** 只依赖 queryKey 的最小子集：直接当 predicate 传时与 TanStack 的 Query 泛型兼容。 */
type KeyedQuery = { queryKey: readonly unknown[] };

/** 是否为「账户级」查询（portal/admin 命名空间下，且不在公开白名单里）。 */
export function isAccountQuery(query: KeyedQuery): boolean {
  const key = query.queryKey;
  const head = key?.[0];
  if (head !== "portal" && head !== "admin") return false;
  return !ACCOUNT_PUBLIC_KEYS.some((allowed) => allowed.every((part, i) => key[i] === part));
}

/**
 * 清掉账户级缓存（会话失效与登录成功时调用）。
 *
 * 为什么必须清：queryKey 不含用户维度，`gcTime` 默认 5 分钟。共享终端上
 * A 掉线后 B 登录，5 分钟内会看到 A 的余额、密钥、订单、被授权的模型。
 *
 * 顺序不能反：先 `cancelQueries` 再 `removeQueries`。否则在途请求返回后会
 * 把数据重新写回缓存，用户仍能看到上一个账号的内容。
 *
 * 用 predicate 而不是 `queryClient.clear()`：后者会连公开价目表一起删，
 * 引发一轮无意义的公开数据重取。
 */
export function clearAccountQueries(): void {
  void queryClient.cancelQueries({ predicate: isAccountQuery });
  queryClient.removeQueries({ predicate: isAccountQuery });
}
