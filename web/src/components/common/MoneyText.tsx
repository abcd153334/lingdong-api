import { cn } from "@/lib/utils";
import { amountAt, defaultSymbol, getMoneyCtx, moneyAt, otherMoneyOf } from "@/lib/money";

/**
 * 是否展示「次币种参考金额」（默认关）。
 *
 * 默认关是产品决策：一列金额同时显示 ¥ 与 $ 会让表格变成两行数字，
 * 而用户真正需要知道的是"我付了多少钱"。运营需要双币参考时由配置打开。
 *
 * 用模块级开关而不是 React context：金额单元格一次渲染几百个，
 * 挂 context 要么每格多一次订阅，要么把 provider 塞进每个页面。
 */
let secondaryEnabled = false;

export function setSecondaryCurrencyEnabled(on: boolean): void {
  secondaryEnabled = on;
}

export function isSecondaryCurrencyEnabled(): boolean {
  return secondaryEnabled;
}

export interface MoneyTextProps {
  /** 后端金额（微） */
  micro: number | null | undefined;
  /** 正数加 "+" 号（资金变动场景） */
  signed?: boolean;
  className?: string;
  /**
   * 行内快照的币种（历史记录必须传）。
   *
   * 只影响**符号**：金额本身是微，不存在"再乘一次基准"的换算 ——
   * 缺了它，一条美元时代的历史记录会显示成 ¥ 符号，金额对了但单位错了。
   */
  currency?: string;
  /** 小数位（默认 4；余额、标价这类"整钱"传 2） */
  digits?: number;
}

/** 金额展示（微 → 行内/当前币种，等宽数字） */
export function MoneyText({ micro, signed, className, currency, digits = 4 }: MoneyTextProps) {
  if (micro === null || micro === undefined) {
    return <span className={cn("muted", className)}>—</span>;
  }
  const sign = signed && micro > 0 ? "+" : "";
  return (
    <span className={cn("mono", className)}>
      {sign}
      {moneyAt(micro, currency, digits)}
    </span>
  );
}

export interface AmountTextProps {
  micro: number | null | undefined;
  signed?: boolean;
  className?: string;
  digits?: number;
}

/** 金额展示（纯数字，不带符号；表头已有币种说明时用） */
export function AmountText({ micro, signed, className, digits = 4 }: AmountTextProps) {
  if (micro === null || micro === undefined) {
    return <span className={cn("muted", className)}>—</span>;
  }
  const sign = signed && micro > 0 ? "+" : "";
  return (
    <span className={cn("mono", className)}>
      {sign}
      {amountAt(micro, null, digits)}
    </span>
  );
}

/**
 * 金额列（表格费用列统一使用）。
 *
 * 主行 = 行内/当前币种金额；次行 = 另一种币种参考（默认关，见 secondaryEnabled）。
 */
export function QuotaUnit({ micro, signed, className, currency, digits = 4 }: MoneyTextProps) {
  if (micro === null || micro === undefined) {
    return <span className={cn("muted", className)}>—</span>;
  }
  const sign = signed && micro > 0 ? "+" : "";
  return (
    <span className={cn("quota-unit", className)}>
      <span className="q">
        {sign}
        {moneyAt(micro, currency, digits)}
      </span>
      {secondaryEnabled && (
        <span className="y">
          {sign}
          {otherMoneyOf(micro, 2)}
        </span>
      )}
    </span>
  );
}

/** 次币种参考金额（单独使用，不含主行）。 */
export function OtherMoneyText({
  micro,
  className,
}: {
  micro: number | null | undefined;
  className?: string;
}) {
  if (micro === null || micro === undefined) {
    return <span className={cn("muted", className)}>—</span>;
  }
  return <span className={cn("mono", className)}>{otherMoneyOf(micro, 2)}</span>;
}

/** 当前币种符号（表格表头「金额(¥)」之类）。 */
export function currentSymbolNow(): string {
  return defaultSymbol(getMoneyCtx().currency);
}
