import { readFileSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";

import { discounted } from "@/lib/money";

/**
 * 充值链路的**跨端契约门禁**。
 *
 * 这一组断言都对应真实发生过的缺陷，共同点是「类型/单位在两端各写一遍，漂了没人报错」：
 *   1. 前端把金额发成 JSON number，后端字段是字符串 → `ShouldBindJSON` 直接 400，
 *      用户看到的是「参数错误」，而自定义充值整条路走不通；
 *   2. 前端顺手把「微」当金额发出去 → 后端按元解析，金额放大 1e6 倍；
 *   3. 余额上限 0 = 不限制，页面却当成「上限为 0」→ 每一次充值都判超限，按钮永远点不动。
 */

const WEB = process.cwd();
const SRC = path.join(WEB, "src");
const REPO = path.resolve(WEB, "..");
const read = (p: string) => readFileSync(p, "utf8");

describe("充值链路门禁", () => {
  it("TopupBody.amount 必须是十进制字符串", () => {
    const api = read(path.join(SRC, "lib/api/index.ts"));
    const m = /export interface TopupBody \{([\s\S]*?)\n\}/.exec(api);
    expect(m, "找不到 TopupBody").not.toBeNull();
    expect(m![1]).toMatch(/amount\?:\s*string/);
    expect(m![1], "amount 又变回 number 了：后端 money.Decimal 会拒绝").not.toMatch(/amount\?:\s*number/);
  });

  it("自定义充值必须提交十进制串，而不是微", () => {
    const topup = read(path.join(SRC, "pages/portal/Topup.tsx"));
    // 唯一正确的写法：从「分」反推十进制串（保证预览到账 == 下单金额）
    expect(topup).toMatch(/amount:\s*microToInput\(/);
    // 事故写法：amount 直接给微
    expect(topup).not.toMatch(/amount:\s*amtMicro\b/);
  });

  it("后端 topupReq.Amount 走 money.Decimal（同时接受字符串与 number）", () => {
    const order = read(path.join(REPO, "backend/internal/controller/order.go"));
    const m = /type topupReq struct \{([\s\S]*?)\n\}/.exec(order);
    expect(m, "找不到 topupReq").not.toBeNull();
    expect(m![1]).toMatch(/Amount\s+money\.Decimal/);
  });

  it("余额上限 0 = 不限制，任何「超上限」判断都必须先判 > 0", () => {
    // 门户收银台：0 直接判超限时，未设上限的站连自定义充值都下不了单
    const topup = read(path.join(SRC, "pages/portal/Topup.tsx"));
    expect(topup).toMatch(/balanceCap\s*>\s*0\s*&&/);
    expect(topup, "又直接拿 cfg.balance_limit 比大小了").not.toMatch(/balance\s*\+\s*previewCredit\s*>\s*cfg\.balance_limit/);
  });
});

/**
 * 退款与「分/微」历史口径门禁。
 *
 * 退款涉及两个**不同量纲**的金额，混用一次就是谎报：
 *   - refund_amount_cent：真的回到渠道的钱（分）；仅回退余额时必须为 0；
 *   - refund_credited   ：实际扣回的平台额度（微）。
 * 旧实现把 refund_amount_cent 恒写成订单金额，对账与客诉都据此以为「钱已经退给用户了」。
 */
describe("退款口径门禁", () => {
  it("后端：余额模式不得再把订单金额写进 refund_amount_cent", () => {
    const refund = read(path.join(REPO, "backend/internal/service/order/refund.go"));
    expect(refund).toMatch(/amountCent\s*=\s*0/);
    expect(refund).toMatch(/"refund_credited":\s*deducted/);
    // 事故写法：无条件写订单金额
    expect(refund, "refund_amount_cent 又恒写订单金额了").not.toMatch(/"refund_amount_cent":\s*o\.AmountCent/);
  });

  it("后端：确认退款失败必须要求上游核实凭证", () => {
    const svc = read(path.join(REPO, "backend/internal/service/order/refund.go"));
    expect(svc).toMatch(/result == RefundStateFailed && evidence == ""/);
    expect(svc).toMatch(/errs\.RefundEvidenceRequired/);
  });

  it("后端：退款成功必须作废该订单尚未转出的邀请返利", () => {
    const refund = read(path.join(REPO, "backend/internal/service/order/refund.go"));
    const notify = read(path.join(REPO, "backend/internal/service/order/notify.go"));
    // 五条路径：仅回退余额 / 原路退款成功 / 上游回调确认退款 / 人工确认成功 / 上游单独退款。
    // 最后一条（upstream_only）曾漏调：钱在网关退给用户、返利却仍可转出 = 直接资损。
    const calls = (s: string) => (s.match(/reclaimAffiliate\(tx,\s*o\.ID\)/g) ?? []).length;
    expect(calls(refund), "refund.go 的退款路径少调了作废").toBe(3);
    expect(calls(notify), "notify.go 少调了作废（含 upstream_only）").toBe(2);
    // 必须包在存档点里：否则作废失败会把整笔退款回滚
    expect(refund).toMatch(/tx\.Transaction\(func\(ntx \*gorm\.DB\) error \{/);
    // 作废只能落 voided，**不得**从余额倒扣（返利额度是派生量，没有欠款可追）
    expect(refund).toMatch(/affiliate\.VoidTx\(/);
    expect(read(path.join(REPO, "backend/internal/service/affiliate/affiliate.go"))).toMatch(
      /SET voided = true, voided_at = now\(\)/,
    );
  });

  it("前端：套餐售价必须走十进制定点解析，不得 Number()*100", () => {
    const pkgs = read(path.join(SRC, "pages/admin/Packages.tsx"));
    expect(pkgs).toMatch(/minorOf\(form\.priceYuan\)/);
    expect(pkgs, "又用浮点乘法算分了").not.toMatch(/Number\(form\.priceYuan\)\s*\*\s*100/);
  });

  it("前端：文档体检结论数组必须有兜底（后端曾下发 null 导致整页白屏）", () => {
    // 文档编辑页把「体检结论」渲染在编辑面板底部；
    // 事故是后端把"没有结论"下发成 null，前端直接 .length ⇒ 整页白屏、HTTP 200。
    const bar = read(path.join(SRC, "components/admin/docs/ProblemsPanel.tsx"));
    expect(bar).toMatch(/problems \?\? \[\]/);
    expect(bar, "又直接读 problems.length 了").not.toMatch(/if \(!problems\.length\)/);
  });
});

describe("门户展示价与账单同舍入", () => {
  /**
   * 折后价是**展示口径**，但它必须与后端 `money.MulDiv` 逐微一致。
   *
   * 后端 MulDiv 的舍入是「半值进位」（`2r >= c` → q++），前端用 `Math.round`。
   * 两者对正数同义；一旦有人把它"优化"成 `Math.floor`（看起来更保守），
   * 每个折后单价都会少 1 微 —— 单笔看不出来，按月汇总就是对不上账的那部分差额。
   */
  it("半值必须进位（与 money.MulDiv 的 2r>=c 同义）", () => {
    // 2 × 0.25 = 0.5 → 1；3 × 0.5 = 1.5 → 2
    expect(discounted(2, 250_000)).toBe(1);
    expect(discounted(3, 500_000)).toBe(2);
  });

  it("整除与零折扣（1e6 = 不打折）保持原值", () => {
    expect(discounted(14000, 500_000)).toBe(7000);
    expect(discounted(14000, 1_000_000)).toBe(14000);
  });

  it("非零小数按最近整数进位，不截断", () => {
    // 14001 × 0.333333 = 4666.57… → 4667（截断会得 4666）
    expect(discounted(14001, 333_333)).toBe(4667);
  });
});
