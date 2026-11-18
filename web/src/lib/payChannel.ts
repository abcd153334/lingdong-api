import type { PayChannel } from "@/types";

/**
 * 密文型参数（以密码框呈现；回显值为 Mask 占位，原样提交表示未改动）。
 *
 * 支付渠道的密文判定已改由协议 schema 的 `secret` 字段声明（见 `payChannelSchema.ts`），
 * 这里只剩云存储/邮件等「非 schema 驱动」的表单在用。
 */
export const SECRET_KEYS = new Set([
  "key",
  "app_secret",
  "api_v3_key",
  "private_key",
  "secret_key",
  "access_key",
  "access_key_id",
  "access_key_secret",
  "secret_id",
]);

/** 可选的支付方式（与后端 payment.Method 枚举一一对应）。 */
export const PAY_METHOD_ITEMS = [
  { key: "wechat", label: "微信支付" },
  { key: "alipay", label: "支付宝" },
];

/** 中文别名归一化：老配置/老缓存里可能是「微信支付」，展示时要能选中。 */
const METHOD_ALIASES: Record<string, string> = {
  wechat: "wechat",
  wx: "wechat",
  wxpay: "wechat",
  weixin: "wechat",
  微信: "wechat",
  微信支付: "wechat",
  alipay: "alipay",
  zfb: "alipay",
  支付宝: "alipay",
  支付宝支付: "alipay",
  aggregate: "aggregate",
  聚合: "aggregate",
  聚合支付: "aggregate",
};

/** 把配置里的方式原文归一化成枚举 code；认不出返回空串。 */
export function normalizeMethod(raw: string): string {
  return METHOD_ALIASES[raw.trim().toLowerCase()] ?? "";
}

/** 该渠道当前可编辑的支付方式（已归一化、去重）。 */
export function channelMethods(ch: PayChannel): string[] {
  const out: string[] = [];
  for (const raw of ch.methods) {
    const m = normalizeMethod(raw);
    if (m && !out.includes(m)) out.push(m);
  }
  return out;
}

/** 是否存在「无法识别/聚合」的存量方式（需要提示运营改成具体方式）。 */
export function hasLegacyMethod(ch: PayChannel): boolean {
  return ch.methods.some((raw) => {
    const m = normalizeMethod(raw);
    return m === "aggregate" || m === "";
  });
}

/**
 * 渠道的实际回调地址。
 *
 * 真实值由后端 `payURLs` 生成（`{api_base_urls[0]}/api/pay/{渠道ID}/notify`），
 * 与 params.callback_url **无关** —— 那个字段只在直接调用 CashierURL 时兜底。
 * 运营要复制到上游后台的是这一条，不是表单里的 callback_url。
 */
export function channelNotifyURL(notifyBase: string, channelID: string): string {
  if (!notifyBase || !channelID) return "";
  return `${notifyBase}${channelID}/notify`;
}

/**
 * 给新增渠道推荐一个稳定 ID（中文名无法 slug，所以只按协议 + 序号给建议）。
 *
 * 必须覆盖每个协议：ID **创建后不可改**（回调地址与历史订单都靠它），
 * 落到 fallback 的 `channel` 会让以后排障时分不清是哪条通道。
 */
export function suggestChannelID(protocol: string, existing: string[]): string {
  const taken = new Set(existing);
  const base =
    protocol === "xunhupay"
      ? "xunhupay"
      : protocol === "epay"
        ? "epay"
        : protocol === "wxpay"
          ? "wxpay"
          : protocol === "alipay"
            ? "alipay"
            : "channel";
  if (!taken.has(base)) return base;
  for (let i = 2; i < 100; i += 1) {
    const id = `${base}-${i}`;
    if (!taken.has(id)) return id;
  }
  return `${base}-${Date.now()}`;
}

/** 渠道 ID 规则（与后端 payChannelIDPattern 一致）。 */
export const CHANNEL_ID_PATTERN = /^[a-z0-9_-]{1,32}$/;

/* -------------------- 收银台「上次用的支付方式」 -------------------- */

/**
 * 记住上次选的支付方式，下次打开收银台默认选中。
 *
 * 存的是**方式枚举**（wechat / alipay），不是「渠道+方式」的组合：
 * 承接渠道由服务端按方式路由，同一方式今天走 A 家、明天走 B 家，
 * 用户偏好始终是「我要用微信付」—— 记住渠道只会在渠道下线时选不中。
 * 值失效（运营停用了该方式）由调用方校验后回落，这里不做判断。
 */
const LAST_METHOD_KEY = "ld-pay-method";

export function readLastMethod(): string {
  try {
    return localStorage.getItem(LAST_METHOD_KEY) ?? "";
  } catch {
    // 隐私模式下 localStorage 会抛异常：记不住偏好而已，不能影响下单。
    return "";
  }
}

export function writeLastMethod(method: string): void {
  try {
    localStorage.setItem(LAST_METHOD_KEY, method);
  } catch {
    /* 同上，静默忽略 */
  }
}
