import { afterEach, describe, expect, it } from "vitest";

import type { UserUsageStat } from "@/types";
import { chargeSub, failRate, monthSub, todaySub } from "./homeStats";
import { resetMoneyCtx, setMoneyCtx } from "./money";

/**
 * 统计卡副文案单测。
 *
 * 这些文案是「同一个数字的两种说法」的唯一定义处：首页与用量页都调这里。
 * 它们的行为（空态说「暂无」而不是 0、失败率分母是 requests、成功数由减法反推）
 * 无法用「渲染出什么」来验证 —— 一个 NaN 在页面上看起来只是一段怪字符。
 */

function stat(p: Partial<UserUsageStat> = {}): UserUsageStat {
  return {
    requests: 0,
    errors: 0,
    pending: 0,
    charge: 0,
    prompt_tokens: 0,
    completion_tokens: 0,
    ...p,
  };
}

afterEach(() => resetMoneyCtx());

describe("failRate", () => {
  it("分母为 0 时返回 null，而不是 0%", () => {
    // 「0% 失败」和「没有任何请求」是两件事，后者不该给出比率
    expect(failRate(stat())).toBeNull();
    expect(failRate(undefined)).toBeNull();
  });

  it("按 errors / requests 计算", () => {
    expect(failRate(stat({ requests: 10, errors: 4 }))).toBe(40);
    expect(failRate(stat({ requests: 3, errors: 0 }))).toBe(0);
  });
});

describe("todaySub", () => {
  it("无数据时给空串（三态由 StatCard 接管），有对象但零请求时给空态文案", () => {
    expect(todaySub(undefined)).toBe("");
    expect(todaySub(stat())).toBe("今日暂无调用");
  });

  it("成功数由 requests - errors - pending 反推，三数相加恒等于总数", () => {
    expect(todaySub(stat({ requests: 12, errors: 2, pending: 1 }))).toBe(
      "成功 9 · 失败 2（16.7%） · 1 进行中",
    );
    // pending 为 0 时不出现「进行中」
    expect(todaySub(stat({ requests: 10, errors: 1 }))).toBe("成功 9 · 失败 1（10.0%）");
  });

  it("脏数据下成功数不出现负数", () => {
    // 手工改库/统计口径漂移时 errors+pending 可能超过 requests，不能显示「成功 -4」
    expect(todaySub(stat({ requests: 1, errors: 3, pending: 2 }))).toBe(
      "成功 0 · 失败 3（300.0%） · 2 进行中",
    );
  });

  it("大数走千分位", () => {
    // 3/2000 = 0.15% → 保留 1 位是 0.1%（浮点表示下 toFixed 向下），锁住即可
    expect(todaySub(stat({ requests: 2_000, errors: 3 }))).toBe("成功 1,997 · 失败 3（0.1%）");
  });
});

describe("chargeSub", () => {
  it("零请求走空态；有请求但无已结算费用时不写「¥0」", () => {
    expect(chargeSub(undefined)).toBe("");
    expect(chargeSub(stat())).toBe("今日暂无消费");
    // 全部请求还在 pending：费用要结算完才进这个数，写 ¥0 会被读成「免费」
    expect(chargeSub(stat({ requests: 2, pending: 2 }))).toBe("暂无已结算费用 · 2 笔结算中");
  });

  it("金额、token、结算中与取数时刻按顺序拼接", () => {
    expect(
      chargeSub(
        stat({
          requests: 5,
          charge: 1_500_000,
          prompt_tokens: 1200,
          completion_tokens: 800,
          pending: 1,
        }),
        "2026-11-02T14:32:00+08:00",
      ),
    ).toBe("已结算 ¥1.5 · 2.0K tokens · 1 笔结算中 · 截至 14:32");
  });

  it("币种符号跟随金额上下文，不写死", () => {
    setMoneyCtx({ currency: "USD" });
    expect(chargeSub(stat({ requests: 1, charge: 1_500_000 }))).toBe("已结算 $1.5");
  });

  it("「截至」直接取报文里的时分，不按浏览器时区换算", () => {
    // 数字是按业务时区算的；用 toLocaleTimeString 会在别的时区把「截至 03:32」
    // 配着北京时间的数字，等于让用户怀疑数字本身
    expect(chargeSub(stat({ requests: 1, charge: 1_000_000 }), "2026-11-02T03:32:00Z")).toBe(
      "已结算 ¥1 · 截至 03:32",
    );
    // 没有 generated_at（老后端/桩件）时不显示，而不是显示 NaN:NaN
    expect(chargeSub(stat({ requests: 1, charge: 1_000_000 }))).toBe("已结算 ¥1");
  });
});

describe("monthSub", () => {
  it("无数据时给空串：加载中不能说「本月暂无充值」", () => {
    // 三态把数值换成 — 的那一刻，副文案不能替接口下结论
    expect(monthSub(undefined, 0)).toBe("");
    expect(monthSub(undefined, 2_000_000)).toBe("");
  });

  it("无充值（含 0 与缺字段）时明确说明", () => {
    expect(monthSub(stat(), 0)).toBe("自然月累计 · 本月暂无充值");
    expect(monthSub(stat(), undefined)).toBe("自然月累计 · 本月暂无充值");
  });

  it("充值口径是到账额度，与消耗并排", () => {
    expect(monthSub(stat({ charge: 9_000_000 }), 2_000_000)).toBe("自然月累计 · 充值 ¥2");
  });
});
