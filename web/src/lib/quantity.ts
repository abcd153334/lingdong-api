/**
 * 数量类输入的解析（张数 / 秒数 / 倍率）。
 *
 * 为什么单独一个模块：系统里同时存在四种「数字」，它们的量纲完全不同 ——
 *
 *   金额   1 元 = 1e6 微     → 必须用 `microOf`（lib/money）换算
 *   张数   1 就是 1 张        → 不换算
 *   秒数   1 就是 1 秒        → 不换算
 *   倍率   1.5 倍存 1500000 微比例 → 换算，但目标不是金额
 *
 * 曾经只有一个 `numOrNull`（内部是 microOf），被顺手用在了全部四种上，
 * 于是 `max_n = 4` 提交成 `4000000`、`max_seconds = 30` 提交成 `30000000`，
 * 被后端 `1–128` / `1–3600` 全部拒绝 —— 图片模型 100% 保存失败。
 *
 * 防复发的做法不是"记得别用错"，而是**让函数名自带量纲**：
 * 看到 `countOfInput` 就知道它不碰金额，看到 `microOf` 就知道它只服务金额。
 */

import { getMoneyCtx, microOf } from "@/lib/money";

/**
 * 单次图片张数上限 / 单次时长上限。
 *
 * 必须与后端同名常量逐字一致，否则「前端放行、后端拒绝」或反之都会出现：
 *   - `billing.MaxImageN`（backend/internal/service/billing/specfacts.go）
 *   - `billing.MaxPriceSeconds`（backend/internal/service/billing/specprices.go）
 *
 * 一致性由 `src/test/units.test.ts` 直接读后端源码比对，不靠人记。
 */
export const MAX_IMAGE_N = 128;
export const MAX_PRICE_SECONDS = 3600;

/** 解析结果：区分「未填」「填了但不合法」「合法」三态。 */
export type Parsed<T> = { ok: true; value: T } | { ok: false; error: string };

const INT_RE = /^\d+$/;

/**
 * 张数 / 秒数输入 → 整数原值（**绝不换算**）。
 *
 * 三态语义：
 *   空串        → `{ ok: true, value: null }`  留空 = 不配置（由后端回落默认上限）
 *   非法        → `{ ok: false }`              带可展示的中文原因
 *   在区间内    → `{ ok: true, value }`
 */
export function countOfInput(
  raw: string,
  opt: { label: string; min: number; max: number },
): Parsed<number | null> {
  const t = raw.trim();
  if (t === "") return { ok: true, value: null };
  if (!INT_RE.test(t)) {
    return { ok: false, error: `${opt.label}必须是非负整数（不能带小数点或其它字符）` };
  }
  const v = Number(t);
  if (!Number.isSafeInteger(v)) {
    return { ok: false, error: `${opt.label}数值过大` };
  }
  if (v < opt.min || v > opt.max) {
    return { ok: false, error: `${opt.label}必须在 ${opt.min}–${opt.max} 之间` };
  }
  return { ok: true, value: v };
}

/**
 * 倍率输入（按「倍」填）→ 微比例（1.5 → 1500000）。
 *
 * 与金额共用 `microOf` 的十进制解析，但返回值的含义是比例而不是钱，
 * 所以单独开一个入口 —— 调用点一眼能看出"这里存的是比例"。
 */
export function ratioOfInput(
  raw: string,
  opt: { label: string; min: number; max: number },
): Parsed<number | null> {
  const t = raw.trim();
  if (t === "") return { ok: true, value: null };
  const micro = microOf(t);
  if (micro === null) {
    return { ok: false, error: `${opt.label}格式不正确（只能填数字，最多 6 位小数）` };
  }
  const scale = getMoneyCtx().scale;
  const ratio = micro / scale;
  if (ratio < opt.min || ratio > opt.max) {
    return { ok: false, error: `${opt.label}必须在 ${opt.min}–${opt.max} 之间` };
  }
  return { ok: true, value: micro };
}
