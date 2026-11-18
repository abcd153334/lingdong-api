import { apiClient } from "@/lib/apiClient";
import type { PortalConfig } from "@/types";

/**
 * 门户端是否开启（前端唯一读取点）。
 *
 * 为什么不是直接读 `usePortalConfigQuery`：路由守卫在 React 之外执行，
 * 而「关闭门户」决定的是「这个路径能不能进」。用一份与 session.ts 同构的
 * 模块级缓存（并发合并 + 30s TTL）让守卫拿到确定答案，且首屏只多发一次请求。
 *
 * 判据只有一处：`portal_enabled !== false` —— **缺字段 = 开启**。
 * 后端同样以「缺键 = 开启」兜底（conf.Bool 的默认值），两边语义必须一致：
 * 把正常站点挡在门外，比多放一次请求糟糕得多。
 */
const TTL = 30_000;

let cached: { enabled: boolean; at: number } | null = null;
let inflight: Promise<boolean> | null = null;
/** 仅单测使用：固定模式，避免每个用例都发一次必然失败的请求 */
let override: boolean | null = null;

export function loadPortalMode(force = false): Promise<boolean> {
  if (override !== null) return Promise.resolve(override);
  if (!force && cached && Date.now() - cached.at < TTL) return Promise.resolve(cached.enabled);
  if (inflight) return inflight;

  inflight = apiClient
    .get<PortalConfig>("/portal/config")
    .then((cfg) => {
      const enabled = cfg.portal_enabled !== false;
      cached = { enabled, at: Date.now() };
      return enabled;
    })
    // 请求失败不能把站点关掉：回落上一次已知值，没有就按开启。
    .catch(() => cached?.enabled ?? true)
    .finally(() => {
      inflight = null;
    });

  return inflight;
}

/**
 * 让下一次读取重新拉取。
 *
 * 三处必须调用：保存运营模式（配置已变）、登录成功（刚关闭时缓存可能仍是 true）、
 * 命中 403 `portal_disabled`（本端缓存与后端不一致）。
 */
export function invalidatePortalMode(): void {
  cached = null;
}

/** 单测钩子：传 null 恢复真实读取。 */
export function __setPortalModeForTest(v: boolean | null): void {
  override = v;
  cached = null;
}
