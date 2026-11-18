/**
 * 金额的**唯一出口**（计费整改 v2）。
 *
 * 口径只有一条：接口里的金额一律是 int64「微」，1 结算货币单位 = `scale` 微。
 * 库里存多少微、接口下发多少微、页面就显示多少钱 —— 中间**不存在**任何倍率、
 * 刻度、qpc 之类的换算量。旧实现的 `quota ÷ qpc` 是把记账单位和展示金额混成
 * 两个概念，任何一次配置调整都会让历史账单自行变价，这就是改掉它的原因。
 *
 * `scale` 从接口下发（/api/pricing、/api/portal/config 的 `scale` 字段）而不是
 * 写死 1e6：两边各写一个常数，某天改动只会改一边，症状是所有金额差 10 的倍数。
 *
 * 只做三件事：
 *   1. 微 → 十进制字符串（展示）；
 *   2. 十进制字符串 → 微（表单入参，与后端 money.Parse 同规则）；
 *   3. 支付渠道的最小单位（分）与微互转（必须整除）。
 */

/** 受支持的结算币种。 */
export type Currency = "CNY" | "USD";

/** 金额上下文（由 /api/pricing 或 /api/portal/config 注入）。 */
export interface MoneyCtx {
  /** 当前结算币种 */
  currency: Currency;
  /** 另一种币种（用于「≈ $10.00」这类参考展示） */
  otherCurrency: Currency;
  /** 人民币兑美元汇率。**只用于参考换算**，永不参与扣费。 */
  cnyPerUsd: number;
  /** 1 货币单位 = 多少微（后端 money.Scale，恒 1e6；下发是为了不写死） */
  scale: number;
}

/**
 * 默认上下文：人民币 + 1e6 微。
 *
 * 首屏（接口还没回来）必须已经能正确显示金额：如果默认 scale 写成 1，
 * 第一帧会把 ¥7.20 显示成 ¥7200000，用户会以为余额被放大了百万倍。
 */
const DEFAULT_CTX: MoneyCtx = {
  currency: "CNY",
  otherCurrency: "USD",
  cnyPerUsd: 7.2,
  scale: 1_000_000,
};

let ctx: MoneyCtx = { ...DEFAULT_CTX };

/** 读取当前上下文（只读副本，防止调用方就地改写导致状态漂移）。 */
export function getMoneyCtx(): MoneyCtx {
  return { ...ctx };
}

/** 币种的内置符号（运营不可自定义：符号跟着币种走，改符号没有合法用途）。 */
export function defaultSymbol(currency: string | undefined | null): string {
  return currency === "USD" ? "$" : "¥";
}

/**
 * 注入上下文。缺省字段保留原值（不同端点只下发部分字段）。
 *
 * scale 必须 > 1：读到 0/1 会让所有金额放大百万倍或变成 Infinity，
 * 而那种页面看起来只是"数字很大"，很容易被当成排版问题忽略。非法值一律忽略。
 */
export function setMoneyCtx(next: Partial<MoneyCtx>): void {
  const merged: MoneyCtx = { ...ctx };
  if (next.currency === "CNY" || next.currency === "USD") merged.currency = next.currency;
  if (next.otherCurrency === "CNY" || next.otherCurrency === "USD") {
    merged.otherCurrency = next.otherCurrency;
  }
  if (typeof next.cnyPerUsd === "number" && Number.isFinite(next.cnyPerUsd) && next.cnyPerUsd > 0) {
    merged.cnyPerUsd = next.cnyPerUsd;
  }
  if (typeof next.scale === "number" && Number.isFinite(next.scale) && next.scale >= 100) {
    merged.scale = next.scale;
  }
  // 币种变了但调用方没给"另一种币种" → 派生。否则会出现「币种是 USD、参考行还是 $」
  // 这种两边同币种的无意义展示。
  if (merged.currency !== ctx.currency && !next.otherCurrency) {
    merged.otherCurrency = merged.currency === "CNY" ? "USD" : "CNY";
  }
  ctx = merged;
}

/** 测试用：把上下文恢复为默认值。 */
export function resetMoneyCtx(): void {
  ctx = { ...DEFAULT_CTX };
}

/** 当前结算币种。 */
export function currentCurrency(): Currency {
  return ctx.currency;
}

/** 当前符号（用于拼接「¥7.20 / 1M」这类文案）。 */
export function currentSymbol(): string {
  return defaultSymbol(ctx.currency);
}

/* --------------------------- 解析（字符串 → 微） --------------------------- */

/** 微能表达的最大小数位，与后端 money.MaxDecimals 一致。 */
const MAX_DECIMALS = 6;

/**
 * 当前 scale 对应的小数位数（1e6 → 6）。
 *
 * 派生而不写死：scale 由后端下发，写死 6 位会让"某天 scale 改成 1e8"变成
 * 全站金额少两位的静默错误。
 */
function decimals(): number {
  return Math.max(0, Math.round(Math.log10(ctx.scale)));
}

/**
 * 十进制金额字符串 → 微。非法返回 null。
 *
 * 严格照抄后端 money.Parse 的规则：只允许数字与至多一个小数点，不接受正负号、
 * 指数、千分位；整数部分 ≤ 15 位，小数部分 ≤ 6 位。
 *
 * **绝不用 Number(input) 再乘 scale**：0.1 这类值二进制无法精确表示，
 * 前端算出的微与后端解析出的微差 1，用户看到「预计到账 ¥10.00」实际到账 ¥9.99。
 */
export function microOf(input: string | number | null | undefined): number | null {
  if (input === null || input === undefined) return null;
  const s = String(input).trim();
  if (!s) return null;
  if (!/^\d+(\.\d*)?$/.test(s)) return null;
  const [intPart, fracRaw = ""] = s.split(".");
  const frac = fracRaw.replace(/0+$/, "");
  if (intPart.length > 15) return null;
  if (frac.length > MAX_DECIMALS) return null;
  // scale 恒为 10 的幂；用字符串拼接而非乘法，避免大数乘法的精度损失。
  const digits = decimals();
  const padded = (frac + "0".repeat(digits)).slice(0, digits);
  const v = Number(intPart + padded);
  if (!Number.isSafeInteger(v)) return null;
  return v;
}

/* --------------------------- 格式化（微 → 文本） --------------------------- */

/** 数值 → 千分位字符串；极小非零值显示 `<0.0001` 而不是 0.0000。 */
function fmtAmount(value: number, maxDigits: number, minDigits = 0): string {
  if (!Number.isFinite(value)) return "—";
  // 非零但小于最小可显示位：显示 0.0000 会被读成"免费"，
  // 而它其实是"有费用但这个量级显示不出来"，含义完全不同。
  if (value !== 0 && Math.abs(value) < Math.pow(10, -maxDigits)) {
    return `<${Math.pow(10, -maxDigits).toFixed(maxDigits)}`;
  }
  return value.toLocaleString("en-US", {
    minimumFractionDigits: minDigits,
    maximumFractionDigits: maxDigits,
  });
}

/** 微 → 金额数值（当前 scale）。 */
function toAmount(micro: number): number {
  return micro / ctx.scale;
}

/**
 * 微 → 当前币种金额（带符号，最多 4 位小数）。
 *
 * 4 位是刻意的：单次调用常见金额是 0.0032 这种量级，固定 2 位会让绝大多数
 * 调用记录显示成 ¥0.00，表格看起来像全部免费。
 */
export function moneyOf(micro: number | null | undefined): string {
  if (micro === null || micro === undefined || !Number.isFinite(micro)) return "—";
  return `${defaultSymbol(ctx.currency)}${fmtAmount(toAmount(micro), 4)}`;
}

/** 微 → 当前币种金额（固定 2 位；标价、余额、套餐价这类"整钱"场景）。 */
export function money2(micro: number | null | undefined): string {
  if (micro === null || micro === undefined || !Number.isFinite(micro)) return "—";
  return `${defaultSymbol(ctx.currency)}${fmtAmount(toAmount(micro), 2, 2)}`;
}

/** 微 → 纯数字串（不带符号，最多 4 位；表格/图表/拼接用）。 */
export function amountOf(micro: number | null | undefined): string {
  if (micro === null || micro === undefined || !Number.isFinite(micro)) return "—";
  return fmtAmount(toAmount(micro), 4);
}

/**
 * 图表 Y 轴刻度（入参是**金额数值**，不是微）。
 *
 * 为什么不直接用 moneyOf：刻度上万元时 `¥12,345.6789` 会把 56px 的轴挤爆
 * （TrendChart 的 YAxis 宽度，见 Charts.tsx），
 * 而几厘钱的刻度又会全变成 `¥0.00`（读起来像免费）。这里按量级换单位：
 * ≥1 亿 → 亿，≥1 万 → 万，<1 用 2 位有效数字。
 *
 * 用「万/亿」而不是 K/M：整站是中文界面，`1.2万` 比 `12.3K` 更符合阅读习惯。
 */
export function moneyAxis(value: number): string {
  if (!Number.isFinite(value)) return "";
  const sign = defaultSymbol(ctx.currency);
  const abs = Math.abs(value);
  if (abs === 0) return `${sign}0`;
  if (abs >= 1e8) return `${sign}${(value / 1e8).toFixed(1)}亿`;
  if (abs >= 1e4) return `${sign}${(value / 1e4).toFixed(1)}万`;
  if (abs >= 1) return `${sign}${fmtAmount(value, 2)}`;
  if (abs >= 0.01) return `${sign}${fmtAmount(value, 3)}`;
  // 极小值：toPrecision 可能吐出科学计数法（1e-7），那时退回定点。
  const s = value.toPrecision(2);
  return `${sign}${s.includes("e") ? value.toFixed(6) : s}`;
}

/** 微 → 纯数字串（不带符号，固定 2 位）。 */
export function amount2(micro: number | null | undefined): string {
  if (micro === null || micro === undefined || !Number.isFinite(micro)) return "—";
  return fmtAmount(toAmount(micro), 2, 2);
}

/**
 * 微 + 行内快照币种 → 金额文本。
 *
 * 历史记录必须用**行内**币种：那条记录是按当时的币种扣的钱，
 * 用当前币种符号展示等于给历史账单换了个单位。缺省（老行/实时数据）取当前币种
 * —— 与后端 logs.currencyOr 的回落规则一致。
 */
export function moneyAt(
  micro: number | null | undefined,
  currency?: string | null,
  digits = 4,
): string {
  if (micro === null || micro === undefined || !Number.isFinite(micro)) return "—";
  const cur = currency === "CNY" || currency === "USD" ? currency : ctx.currency;
  return `${defaultSymbol(cur)}${fmtAmount(toAmount(micro), digits)}`;
}

/** 微 + 行内币种 → 纯数字串。 */
export function amountAt(
  micro: number | null | undefined,
  currency?: string | null,
  digits = 4,
): string {
  if (micro === null || micro === undefined || !Number.isFinite(micro)) return "—";
  return fmtAmount(toAmount(micro), digits);
}

/**
 * 微 → 另一种币种的参考金额（「≈ $1.00」）。
 *
 * 换算只用 cnyPerUsd，不带任何精度截断以外的处理：这是纯展示，不参与记账。
 * 当前是人民币 → 除以汇率得美元；当前是美元 → 乘汇率得人民币。
 */
export function otherMoneyOf(micro: number | null | undefined, digits = 2): string {
  if (micro === null || micro === undefined || !Number.isFinite(micro)) return "—";
  const base = toAmount(micro);
  const rate = ctx.cnyPerUsd > 0 ? ctx.cnyPerUsd : 1;
  const v = ctx.currency === "CNY" ? base / rate : base * rate;
  return `${defaultSymbol(ctx.otherCurrency)}${fmtAmount(v, digits, digits)}`;
}

/**
 * 微 → 表单可编辑的十进制字符串（去掉无意义的尾随 0）。
 *
 * `7.2` 而不是 `7.200000`：管理员改价时看到一串尾随 0 会以为精度要求是 6 位，
 * 手一抖就能改出 7.200001 这种无意义的价格。
 */
export function microToInput(micro: number | null | undefined): string {
  if (micro === null || micro === undefined || !Number.isFinite(micro)) return "";
  const digits = decimals();
  const negative = micro < 0;
  const abs = Math.abs(micro);
  const intPart = Math.floor(abs / ctx.scale);
  const frac = String(abs % ctx.scale)
    .padStart(digits, "0")
    .replace(/0+$/, "");
  return `${negative ? "-" : ""}${intPart}${frac ? "." + frac : ""}`;
}

/* ---------------------------- 支付渠道单位换算 ---------------------------- */

/** 1 分（支付渠道最小单位）= 多少微。 */
function microPerCent(): number {
  return ctx.scale / 100;
}

/**
 * 分 → 微。**必须整除**（scale 不是 100 的倍数时返回 null）。
 *
 * 订单金额在库里是「分」，余额是「微」，两者之间的换算只能发生在这一个函数里。
 */
export function microFromMinor(minor: number | null | undefined): number | null {
  if (minor === null || minor === undefined || !Number.isFinite(minor)) return null;
  const per = microPerCent();
  if (!Number.isInteger(per) || per <= 0) return null;
  return Math.round(minor) * per;
}

/**
 * 十进制文本 → 分。
 *
 * 套餐售价这类字段的用户输入是**元**，而库里存的是**分**。旧写法
 * `Math.round(Number(text) * 100)` 有两个问题：
 *   - 二进制浮点：`Number("9.995") * 100 = 999.4999...`，四舍五入的方向不可预期；
 *   - 无法表达"精度超过分"：输入 9.999 会被静默按 1000 分存库，运营以为卖了 9.999 元。
 * 这里先按十进制定点解析成微，再要求能被「每分多少微」整除 —— 除不尽就是非法输入，
 * 由调用方明确报错，绝不暗改金额。
 */
export function minorOf(input: string | number | null | undefined): number | null {
  const micro = microOf(input);
  if (micro === null) return null;
  const per = microPerCent();
  if (!Number.isInteger(per) || per <= 0) return null;
  if (micro % per !== 0) return null;
  return micro / per;
}

/** 微 → 分（除不尽返回 null，绝不做"四舍五入到分"这种暗改金额的操作）。 */
export function minorFromMicro(micro: number | null | undefined): number | null {
  if (micro === null || micro === undefined || !Number.isFinite(micro)) return null;
  const per = microPerCent();
  if (!Number.isInteger(per) || per <= 0) return null;
  if (micro % per !== 0) return null;
  return micro / per;
}

/**
 * 订单金额（**分**）→ 带符号金额。
 *
 * 订单金额是真实收付金额，与余额的微不是同一量纲：它按「分」记账，
 * 展示时直接 /100，不要绕微（绕一次就多一次取整机会）。
 */
export function minorToMoney(minor: number | null | undefined, currency?: string | null): string {
  if (minor === null || minor === undefined || !Number.isFinite(minor)) return "—";
  const cur = currency === "CNY" || currency === "USD" ? currency : ctx.currency;
  return `${defaultSymbol(cur)}${fmtAmount(minor / 100, 2, 2)}`;
}

/**
 * 套餐到账额（微）。
 *
 * 到账 = 售价（分→微）+ 赠送（微）。**不存在充值倍率**：旧实现里
 * 「售价 × 倍率 × 刻度」是三个可以互相抵消的错误配置的乘积，用户永远算不出自己该到账多少。
 */
export function packageCreditOf(
  priceCent: number | null | undefined,
  bonusMicro: number | null | undefined,
): { base: number; credit: number } {
  const base = microFromMinor(priceCent) ?? 0;
  const bonus = Number.isFinite(bonusMicro) && (bonusMicro ?? 0) > 0 ? Math.round(bonusMicro as number) : 0;
  return { base, credit: base + bonus };
}

/* ------------------------------ 折扣展示 ------------------------------ */

/**
 * 计费分组折扣（微比例，1e6 = 不打折）→ 「9 折」/「无折扣」。
 *
 * 折扣在**扣费时**由后端按微比例乘算，前端只展示。用「几折」而不是「减 10%」：
 * 运营配置界面上写的就是折扣率，两种说法并存必然出现"到底按哪个算"的争议。
 */
export function groupDiscountText(discount: number | null | undefined): string {
  if (discount === null || discount === undefined || !Number.isFinite(discount)) return "无折扣";
  if (discount >= ctx.scale) return "无折扣";
  // 0 / 负数一律显示「无折扣」：计费侧 `applyDiscount` 就是这样处理的
  // （`d <= 0` 视为不打折，未知分组 fail-safe 到不打折）。
  // 这里曾写「免费」——与计费方向相反，用户会以为整组免单而实际按原价扣款。
  if (discount <= 0) return "无折扣";
  const tenths = (discount / ctx.scale) * 10;
  const text = tenths.toFixed(2).replace(/\.?0+$/, "");
  return `${text} 折`;
}

/** 计费分组折扣 → 百分比文本（「90%」），用于与折扣率并列展示。 */
export function groupDiscountPercent(discount: number | null | undefined): string {
  if (discount === null || discount === undefined || !Number.isFinite(discount)) return "100%";
  return ratioPercent(discount);
}

/**
 * 微比例 → 百分比文本（1e6 = 100%）。**唯一实现**，折扣与邀请返利比例共用。
 *
 * 各自 `.toFixed()` 一遍就是两套舍入口径：折扣显示 89.5%、返利显示 89.50%，
 * 运营会以为两个数不是一个东西。
 */
export function ratioPercent(micro: number | null | undefined): string {
  if (micro === null || micro === undefined || !Number.isFinite(micro)) return "0%";
  const pct = (micro / ctx.scale) * 100;
  return `${pct.toFixed(pct % 1 === 0 ? 0 : 2)}%`;
}

/**
 * 折后单价（微）= base × discount ÷ scale，四舍五入。
 *
 * 舍入方式与后端 `money.MulDiv` 完全一致（余数 ≥ 除数一半就进位），
 * 这样门户上「¥1.25 → 我的价格 ¥1.13」与结算时真正扣的钱是同一个数。
 * 这里只用于**展示**：真正的扣费永远在后端算。
 */
export function discounted(base: number | null | undefined, discount: number | null | undefined): number | null {
  if (base === null || base === undefined || !Number.isFinite(base)) return null;
  if (discount === null || discount === undefined || !Number.isFinite(discount)) return null;
  if (discount === ctx.scale) return base;
  return Math.round((base * discount) / ctx.scale);
}

