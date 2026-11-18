import type { UserUsageStat } from "@/types";
import { moneyOf } from "./money";
import { formatNum } from "./format";

/**
 * 门户主页/用量页的统计卡副文案。
 *
 * 为什么独立成纯函数而不是写在 JSX 里：
 *  1. 这三段文案都有分支（空态/进行中/无充值），塞进 JSX 就只能靠 e2e 断言，
 *     而 e2e 又依赖文案本身 —— 改一个字就要同时改三处；
 *  2. 首页与「用量」页必须说同一句话（同一个数字两种说法是典型的误导来源）。
 *
 * 所有函数在 `stat` 为 undefined（加载中/失败）时**不返回 0 相关的文案**：
 * 那里由 StatCard 的三态接管，副文案不该假装知道数字。
 */

/** 失败率（百分数，保留 1 位小数）。requests=0 时返回 null —— 不显示「0%」。 */
export function failRate(stat?: UserUsageStat): number | null {
  if (!stat || stat.requests <= 0) return null;
  return (stat.errors / stat.requests) * 100;
}

/** token 数的紧凑写法：1.2M / 12.3K / 999。 */
function formatTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1000) return `${(n / 1000).toFixed(1)}K`;
  return String(n);
}

/**
 * 从 RFC3339 里取「业务时区的 HH:MM」。
 *
 * 刻意不用 new Date().toLocaleTimeString()：那会按**浏览器时区**渲染，
 * 而数字是按业务时区算出来的。用户在别的时区打开页面时，
 * 「截至 03:32」配着北京时间的数字，等于让用户怀疑数字本身。
 */
function clockOf(iso?: string): string | null {
  if (!iso) return null;
  const m = /T(\d{2}:\d{2})/.exec(iso);
  return m ? m[1] : null;
}

/**
 * 「今日请求」卡副文案：成功 / 失败（含失败率）/ 进行中。
 *
 * 三个数同源同一窗口（usages 明细按 status 分类），相加恒等于 requests。
 * 旧的 `sub="成功/失败合计（含冻结中）"` 只是一句口径说明，用户看不出今天到底跑成什么样。
 */
export function todaySub(stat?: UserUsageStat): string {
  if (!stat) return "";
  if (stat.requests <= 0) return "今日暂无调用";

  // 成功数由减法得出：requests = settled + errors + pending（每个请求恰属一类）。
  // Math.max 是防御：脏数据（比如手工改库）下不能显示「成功 -2」。
  const ok = Math.max(0, stat.requests - stat.errors - stat.pending);
  const rate = failRate(stat);
  const parts = [`成功 ${formatNum(ok)}`];
  parts.push(
    rate === null
      ? `失败 ${formatNum(stat.errors)}`
      : `失败 ${formatNum(stat.errors)}（${rate.toFixed(1)}%）`,
  );
  if (stat.pending > 0) parts.push(`${formatNum(stat.pending)} 进行中`);
  return parts.join(" · ");
}

/**
 * 「今日费用」卡副文案：已结算金额 / token / 结算中笔数 / 取数时刻。
 *
 * 「已结算」这三个字是刻意的：pending 行的 charge 还是 0，费用要等结算完才进这个数。
 * 写成「实时」会让用户以为刷新一下就该包含刚发出的请求。
 */
export function chargeSub(stat?: UserUsageStat, generatedAt?: string): string {
  if (!stat) return "";
  if (stat.requests <= 0) return "今日暂无消费";

  const parts = [stat.charge > 0 ? `已结算 ${moneyOf(stat.charge)}` : "暂无已结算费用"];
  const tokens = stat.prompt_tokens + stat.completion_tokens;
  if (tokens > 0) parts.push(`${formatTokens(tokens)} tokens`);
  if (stat.pending > 0) parts.push(`${formatNum(stat.pending)} 笔结算中`);
  const clock = clockOf(generatedAt);
  if (clock) parts.push(`截至 ${clock}`);
  return parts.join(" · ");
}

/**
 * 「本月费用」卡副文案：自然月累计 + 本月充值。
 *
 * 充值刻意与消耗并排：用户真正的疑问是「这个月我花了多少、充了多少」，
 * 两个数分开在两页时只能靠记忆对比。充值口径是**到账额度**（含赠送、已扣退款），
 * 与钱包「资金流水」页签一致。
 *
 * `stat` 只用于「数据还没到」的判定：加载中必须返回空串，否则卡片会在
 * 三态把数值换成 `—` 的同时，副文案斩钉截铁地说「本月暂无充值」——
 * 一句与事实无关的断言，正好出现在最不该下结论的时刻。
 */
export function monthSub(stat?: UserUsageStat, recharge?: number): string {
  if (!stat) return "";
  const parts = ["自然月累计"];
  if (recharge && recharge > 0) {
    parts.push(`充值 ${moneyOf(recharge)}`);
  } else {
    parts.push("本月暂无充值");
  }
  return parts.join(" · ");
}
