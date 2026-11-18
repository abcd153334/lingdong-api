import dayjs from "dayjs";

import type { BillingMode, Modality, PricingModel } from "@/types";
import {
  amount2,
  amountAt,
  amountOf,
  minorToMoney,
  moneyOf,
  setMoneyCtx,
} from "@/lib/money";

/**
 * 注入金额口径（S10 接 system_config；S3 起由 /api/pricing、/api/portal/config 顺带注入）。
 *
 * 只有一个可变量：`scale`（1 货币单位 = 多少微）。真实状态全部在 lib/money.ts，
 * 本文件只做转发，保证两处不会各维护一套常量。
 */
export function setBillingConstants(opts: {
  scale?: number;
  cnyPerUsd?: number;
  currency?: string;
  otherCurrency?: string;
}): void {
  setMoneyCtx({
    scale: opts.scale,
    cnyPerUsd: opts.cnyPerUsd,
    currency: opts.currency === "CNY" || opts.currency === "USD" ? opts.currency : undefined,
    otherCurrency:
      opts.otherCurrency === "CNY" || opts.otherCurrency === "USD" ? opts.otherCurrency : undefined,
  });
}

/** 微 → 金额数字（最多 4 位小数，不带符号）。表格/图表/输入框联动都用它。 */
export function formatAmount(micro: number | null | undefined): string {
  return amountOf(micro);
}

/** 微 → 金额数字（固定 2 位小数）。标价、余额、套餐价这类"整钱"场景。 */
export function formatAmount2(micro: number | null | undefined): string {
  return amount2(micro);
}

/** 微 → 带符号金额（最多 4 位小数）。 */
export function formatMoney(micro: number | null | undefined): string {
  return moneyOf(micro);
}

/** 微 → 带符号金额，按**行内快照**币种（历史记录必须用它）。 */
export function formatMoneyAt(micro: number | null | undefined, currency?: string | null): string {
  return amountAt(micro, currency, 4);
}

/**
 * 「分」金额 → 带符号金额（币种可选，缺省用当前结算币种）。
 *
 * 订单金额是支付渠道的「分」，与余额的「微」不是同一量纲：这里直接 /100，
 * 不绕微（绕一次就多一次取整机会）。
 */
export function formatCent(cent: number | null | undefined, currency?: string | null): string {
  return minorToMoney(cent, currency);
}

/** 微 → 金额数值（图表需要 number 而非文本）。 */
export function amountNum(micro: number | null | undefined): number {
  if (micro === null || micro === undefined || !Number.isFinite(micro)) return 0;
  return Number(amountOf(micro).replace(/,/g, "")) || 0;
}

/** 千分位数字（最多 digits 位小数） */
export function formatNum(value: number, digits = 0): string {
  return value.toLocaleString("en-US", { maximumFractionDigits: digits });
}

/**
 * 字节数 → 可读单位（二进制前缀，与磁盘/容器口径一致：1 GiB = 1024³）。
 *
 * 负数与非有限值一律「—」：容量监控里出现它们只有一种解释 —— 采集失败，
 * 显示成 0 B 会被误读成「磁盘空了」。
 */
export function formatBytes(bytes: number, digits = 1): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "—";
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KiB", "MiB", "GiB", "TiB", "PiB"];
  let v = bytes / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v.toFixed(digits)} ${units[i]}`;
}

/**
 * 时间格式化（dayjs）。
 *
 * **必须同时吃得下秒级时间戳**：任务对象（`taskview.View`）的时间字段是 Unix 秒，
 * 而 dayjs 默认把数字当**毫秒**。`dayjs(1730000000)` 会渲染成 1970 年 ——
 * 页面不报错、时间全错，是最难发现的一类 bug。判据用 1e11：
 * 毫秒时间戳从 1973 年起就大于 1e11，秒级则要用到公元 5138 年才追平。
 */
export function formatDateTime(value: string | number | Date): string {
  const v = typeof value === "number" && Math.abs(value) < 1e11 ? value * 1000 : value;
  return dayjs(v).format("YYYY-MM-DD HH:mm:ss");
}

/** 邮箱脱敏展示：首字符 + *** + @域名；空串原样返回 */
export function maskEmail(email: string): string {
  const value = (email ?? "").trim();
  if (!value) return "";
  const at = value.indexOf("@");
  if (at <= 0) return value;
  return `${value[0]}***${value.slice(at)}`;
}

/** 手机号脱敏展示：前 3 + **** + 后 4；空串原样返回 */
export function maskPhone(phone: string): string {
  const value = (phone ?? "").trim();
  if (!value) return "";
  if (value.length < 8) return value;
  return `${value.slice(0, 3)}****${value.slice(-4)}`;
}

/** 模态中文名（8 类） */
export const MODALITY_LABELS: Record<Modality, string> = {
  chat: "对话",
  image: "图像",
  video: "视频",
  audio: "音频",
  music: "音乐",
  embedding: "向量",
  rerank: "重排",
  moderation: "审核",
};

export function modalityLabel(m: string): string {
  return MODALITY_LABELS[m as Modality] ?? m;
}

/** 资金流水类型中文名（§4.2.9 冻结枚举；门户钱包与管理台用户详情共用） */
export const TRANSACTION_LABELS: Record<string, string> = {
  recharge: "充值",
  redeem: "兑换",
  freeze: "冻结",
  release: "解冻",
  settle: "结算",
  refund: "退款",
  refund_revert: "退款冲正",
  adjust: "调账",
  invite: "邀请",
};

/**
 * 计费方式标签（按模态细分图片按张 / 视频按秒）。
 *
 * 只按 `billing_mode` 分支，其余信息仅用于把 `spec` 说清楚 ——
 * 不要在这里读价格数值：那会让"同一计费方式"的标签随价格变化而变。
 */
export function billingModeLabel(m: Pick<PricingModel, "billing_mode" | "modality" | "spec_prices">): string {
  const mode: BillingMode = m.billing_mode;
  if (mode === "tokens") return "按用量";
  if (mode === "call") return "按次";
  if (m.spec_prices?.kind === "per_video") return "视频按秒";
  // 音乐/歌词按**实际交付条数**计费：0 条免单，n 条收 n 份。
  if (m.spec_prices?.kind === "per_clip") return "音乐按条";
  // 存量口径：一次调用一个固定价，不随产物条数放大。
  if (m.spec_prices?.kind === "per_task") return "音乐按次";
  // 音频按**秒**计费：与视频按秒同字面，但单价来自 per_second_price（没有档位）。
  if (m.spec_prices?.kind === "per_second") return "语音按秒";
  return "图片按张";
}

/** 兼容旧名（调用点太多，一次性改名会把改造 diff 淹没）；新代码用 billingModeLabel。 */
export const quotaTypeLabel = billingModeLabel;

/**
 * 套餐角标文案：三种枚举共用同一视觉（.pkg-ribbon），只换文案。
 * 管理台下拉与门户卡片共用这一份，避免两处各写一遍导致文案漂移。
 */
export const PACKAGE_BADGE_LABELS: Record<"recommend" | "hot" | "value", string> = {
  recommend: "推荐",
  hot: "热销",
  value: "超值",
};
