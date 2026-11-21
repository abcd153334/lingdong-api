/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, isAdminPageRequest, seedAuth } from "./helpers";

/** S20 去假覆盖：/admin/orders（契约级 stub —— 全部 /api/admin/** 打桩，断言真实区块/行/抽屉/请求契约） */

const ORDERS = [
  {
    id: 101,
    user_id: 7,
    username: "alice",
    out_trade_no: "LD20261020001",
    trade_no: "TRD-OK-1",
    amount_cent: 5000,
    credited: 5_000_000,
    status: "paid",
    pay_channel: "易支付-支付宝",
    pay_channel_id: "epay",
    pay_method: "支付宝",
    pay_protocol: "epay",
    created_at: "2026-10-20T09:00:00+08:00",
    paid_at: "2026-10-20T09:01:00+08:00",
  },
  {
    id: 102,
    user_id: 7,
    username: "alice",
    out_trade_no: "LD20261020002",
    trade_no: "",
    amount_cent: 9900,
    credited: 9_900_000,
    status: "need_manual",
    pay_channel: "易支付-微信支付",
    pay_channel_id: "epay",
    pay_method: "微信支付",
    pay_protocol: "epay",
    created_at: "2026-10-20T10:00:00+08:00",
    paid_at: null,
  },
  {
    id: 103,
    user_id: 7,
    username: "alice",
    out_trade_no: "LD20261020003",
    trade_no: "",
    amount_cent: 1000,
    credited: 1_000_000,
    status: "pending",
    pay_channel: "易支付-支付宝",
    pay_channel_id: "epay",
    pay_method: "支付宝",
    pay_protocol: "epay",
    created_at: "2026-10-20T11:00:00+08:00",
    paid_at: null,
  },
  {
    // 原路退款已提交、结果待确认：额度已扣，钱可能已退出
    id: 104,
    user_id: 7,
    username: "alice",
    out_trade_no: "LD20261020004",
    trade_no: "TRD-OK-4",
    amount_cent: 1000,
    credited: 1_000_000,
    status: "refunding",
    pay_channel: "虎皮椒-聚合支付",
    pay_channel_id: "xunhupay",
    pay_method: "聚合支付",
    pay_protocol: "xunhupay",
    created_at: "2026-10-20T12:00:00+08:00",
    paid_at: "2026-10-20T12:01:00+08:00",
    refund_status: "pending",
    refund_mode: "gateway",
    refund_no: "RF-20261020004",
    refund_amount_cent: 1000,
    refund_reason: "重复支付",
    refund_at: "2026-10-20T12:30:00+08:00",
  },
];

/** 记录已发生的请求；管理台必须在 /api/admin/** 打桩（否则真实 401 触发全局登出） */
async function stub(page: Page, captures: { url: string; body: unknown }[] = []): Promise<string[]> {
  const calls: string[] = [];
  // 订单金额是**真实人民币收付**（amount_cent），本文件按 ¥ 断言；
  // 不桩会用到 seedAuth 的美元兜底，¥50.00 会被显示成 $50.00。
  await page.route("**/api/pricing**", (route) =>
    route.fulfill({
      json: envelope({
        models: [],
        vendors: [],
        defaults: {},
        group_ratios: { default: 1 },
        quota_per_unit: 69444,
        currency: "CNY",
        currency_symbol: "¥",
        cny_per_usd: 7.2,
        other_currency: "USD",
        other_quota_per_unit: 500000,
      }),
    }),
  );
  await page.route(
    (url) => isAdminPageRequest(url),
    (route) => {
      const req = route.request();
      const u = new URL(req.url());
      calls.push(`${req.method()} ${u.pathname}${u.search}`);

      if (u.pathname === "/api/admin/orders" && req.method() === "GET") {
        return route.fulfill({
          json: envelope({
            items: ORDERS,
            total: ORDERS.length,
            page: 1,
            page_size: 20,
            // 渠道元信息**含停用渠道**：管理台筛选必须能筛出已停用渠道的历史订单
            pay_channels: [
              { id: "epay", name: "易支付", protocol: "epay", enabled: true, methods: ["wechat", "alipay"] },
              { id: "xunhupay", name: "虎皮椒", protocol: "xunhupay", enabled: false, methods: ["聚合支付"] },
            ],
          }),
        });
      }
      if (u.pathname === "/api/admin/order/refund" && req.method() === "POST") {
        captures.push({ url: u.pathname, body: req.postDataJSON() });
        // 退款返回三态；契约级用例固定走 success 分支（pending/failed 的文案另有用例）
        return route.fulfill({
          json: envelope({
            order_id: 101,
            refund_mode: "balance",
            refund_status: "success",
            refund_amount_cent: 5000,
            quota_deducted: 5_000_000,
            message: "已扣回平台额度",
          }),
        });
      }
      if (u.pathname === "/api/admin/order/refund/confirm" && req.method() === "POST") {
        captures.push({ url: u.pathname, body: req.postDataJSON() });
        return route.fulfill({ json: envelope(null) });
      }
      if (u.pathname === "/api/admin/order/manual-credit" && req.method() === "POST") {
        captures.push({ url: u.pathname, body: req.postDataJSON() });
        return route.fulfill({ json: envelope(null) });
      }
      if (u.pathname === "/api/admin/orders/export") {
        return route.fulfill({
          status: 200,
          headers: {
            "Content-Type": "text/csv; charset=utf-8",
            "Content-Disposition": 'attachment; filename="orders.csv"',
          },
          // 表头必须与后端 orderCSV 逐字一致（19 列：…金额,币种,到账额度,下单时比例,下单时倍率,套餐,…）
          body: "\ufeff订单号,用户ID,用户名,金额,币种,到账额度,下单时比例,下单时倍率,套餐,状态,退款方式,退款状态,退款单号,支付渠道,渠道ID,协议,支付方式,创建时间,支付时间\r\nLD20261020001,7,alice,50.00,CNY,5000000,69444,1.0000,标准包,paid,,,,易支付-支付宝,epay,epay,支付宝,2026-10-20T09:00:00+08:00,2026-10-20T09:01:00+08:00\r\n",
        });
      }
      return route.fulfill({ json: envelope(null) });
    },
  );
  return calls;
}

/** 仅桌面内联的筛选控件位于 .pbar-fields（Sheet 内的同名字段不参与） */
const fields = (page: Page) => page.locator(".pbar-fields");

test.describe("S20 订单管理 · 管理台（契约级）", () => {
  test("列表渲染：订单号 / 用户 / 金额 / 状态（含人工处理）/ 支付渠道", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);
    await page.goto("/admin/orders");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("订单管理");
    await expect(page.locator(".ph-desc")).toHaveText("充值订单查询、退款与人工处理");

    const paidRow = page.locator(".tbl tbody tr", { hasText: "LD20261020001" });
    await expect(paidRow).toContainText("#7 alice");
    await expect(paidRow).toContainText("¥50.00");
    await expect(paidRow.locator(".badge")).toHaveText("已支付");
    await expect(paidRow).toContainText("易支付-支付宝");

    const manualRow = page.locator(".tbl tbody tr", { hasText: "LD20261020002" });
    await expect(manualRow.locator(".badge")).toHaveText("人工处理");

    // 行操作按状态收敛：详情 / 退款（paid 与 need_manual 都可退款）/ 人工处理（need_manual）
    //
    // need_manual 也提供退款，是因为「上游已退款」的订单必须能扣回额度；
    // 它只允许「仅回退平台余额」（原路退回对上游可能已退过的单是二次退款）。
    await expect(paidRow.getByRole("button", { name: "详情" })).toHaveClass(/t-act/);
    await expect(paidRow.getByRole("button", { name: "退款" })).toBeVisible();
    await expect(paidRow.getByRole("button", { name: "人工处理" })).toHaveCount(0);
    await expect(manualRow.getByRole("button", { name: "人工处理" })).toBeVisible();
    await expect(manualRow.getByRole("button", { name: "退款" })).toBeVisible();

    // 原型「关闭订单」无后端能力 → 不渲染（pending 行仅有「详情」）
    const pendingRow = page.locator(".tbl tbody tr", { hasText: "LD20261020003" });
    await expect(pendingRow.getByRole("button", { name: "关闭订单" })).toHaveCount(0);
    await expect(pendingRow.locator(".t-acts button")).toHaveCount(1);
  });

  test("退款中订单：展示原路退款信息，可人工确认结果（POST /order/refund/confirm）", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const captures: { url: string; body: unknown }[] = [];
    await stub(page, captures);
    await page.goto("/admin/orders");

    const row = page.locator(".tbl tbody tr", { hasText: "LD20261020004" });
    await expect(row.locator(".badge")).toHaveText("退款中");
    // 退款中不再提供「退款」（重复提交会造成二次退款）
    await expect(row.getByRole("button", { name: "退款", exact: true })).toHaveCount(0);

    await row.getByRole("button", { name: "详情" }).click();
    const drawer = page.getByRole("dialog", { name: "订单详情" });
    await expect(drawer).toContainText("② 退款信息");
    await expect(drawer).toContainText("原路退回");
    await expect(drawer).toContainText("RF-20261020004");
    await drawer.evaluate((el) =>
      Promise.all(el.getAnimations({ subtree: true }).map((a) => a.finished)).then(() => undefined),
    );

    await drawer.getByRole("button", { name: "确认已退款" }).click();
    const dlg = page.getByRole("dialog", { name: "确认退款结果" });
    await expect(dlg).toBeVisible();
    await dlg.locator("textarea").fill("虎皮椒后台显示退款成功");
    await dlg.getByRole("button", { name: "确认" }).click();
    await expect
      .poll(() => captures.some((c) => c.url === "/api/admin/order/refund/confirm"))
      .toBe(true);
    expect(captures.find((c) => c.url === "/api/admin/order/refund/confirm")?.body).toMatchObject({
      order_id: 104,
      result: "success",
      reason: "虎皮椒后台显示退款成功",
    });
  });

  test("筛选：状态与渠道下发查询参数（对齐原型筛选条）", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page);
    await page.goto("/admin/orders");

    // 原型筛选条为状态 / 渠道 / 时间；后端仅支持状态与渠道，故只渲染两项（时间能力缺失见报告）
    await expect(fields(page).locator("select")).toHaveCount(2);
    await expect(fields(page).locator('input[type="search"]')).toHaveCount(0);

    await fields(page).getByLabel("状态").selectOption("need_manual");
    await expect
      .poll(() => calls.some((c) => c.startsWith("GET /api/admin/orders?") && c.includes("status=need_manual")))
      .toBe(true);

    // 渠道筛选（选项与门户收银台同源，精确匹配透传）
    await fields(page).getByLabel("渠道").selectOption("epay");
    await expect
      .poll(() => calls.some((c) => c.startsWith("GET /api/admin/orders?") && c.includes("pay_channel_id=epay")))
      .toBe(true);
  });

  test("导出 CSV：携带当前筛选请求 /orders/export", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page);
    await page.goto("/admin/orders");

    await fields(page).getByLabel("状态").selectOption("paid");
    await expect.poll(() => calls.some((c) => c.includes("status=paid"))).toBe(true);

    await page.getByRole("button", { name: "导出 CSV" }).click();
    await expect
      .poll(
        () =>
          calls.some(
            (c) => c.startsWith("GET /api/admin/orders/export?") && c.includes("status=paid"),
          ),
      )
      .toBe(true);
  });

  test("行操作：退款 / 人工处理直接打开弹窗并提交真实 mutation", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const captures: { url: string; body: unknown }[] = [];
    await stub(page, captures);
    await page.goto("/admin/orders");

    // paid 行 → 退款
    await page
      .locator(".tbl tbody tr", { hasText: "LD20261020001" })
      .getByRole("button", { name: "退款" })
      .click();
    const refundDialog = page.getByRole("dialog", { name: "人工退款" });
    await expect(refundDialog).toBeVisible();
    await refundDialog.locator("textarea").fill("重复支付，用户申请退款");
    await refundDialog.getByRole("button", { name: "确认退款" }).click();
    await expect
      .poll(() => captures.some((c) => c.url === "/api/admin/order/refund"))
      .toBe(true);
    const refundBody = captures.find((c) => c.url === "/api/admin/order/refund")?.body as {
      order_id: number;
      reason: string;
    };
    expect(refundBody.order_id).toBe(101);
    expect(refundBody.reason).toBe("重复支付，用户申请退款");

    // need_manual 行 → 人工处理
    await page
      .locator(".tbl tbody tr", { hasText: "LD20261020002" })
      .getByRole("button", { name: "人工处理" })
      .click();
    const creditDialog = page.getByRole("dialog", { name: "人工入账" });
    await expect(creditDialog).toBeVisible();
    await creditDialog.locator("textarea").fill("线下核实已付款，超限转人工");
    await creditDialog.getByRole("button", { name: "确认入账" }).click();
    await expect
      .poll(() => captures.some((c) => c.url === "/api/admin/order/manual-credit"))
      .toBe(true);
    const creditBody = captures.find((c) => c.url === "/api/admin/order/manual-credit")?.body as {
      order_id: number;
      reason: string;
    };
    expect(creditBody.order_id).toBe(102);
    expect(creditBody.reason).toBe("线下核实已付款，超限转人工");
  });

  test("详情抽屉 → 人工退款：原因必填拦截，填写后 POST 契约", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const captures: { url: string; body: unknown }[] = [];
    await stub(page, captures);
    await page.goto("/admin/orders");

    // 退款入口仅对已支付订单渲染（页面契约：need_manual 不显示退款块）
    await page.locator(".tbl tbody tr", { hasText: "LD20261020001" }).getByRole("button", { name: "详情" }).click();
    const drawer = page.getByRole("dialog", { name: "订单详情" });
    await expect(drawer).toBeVisible();
    await expect(drawer).toContainText("LD20261020001");
    await expect(drawer).toContainText("② 人工退款");

    // 等高负载下抽屉入场过渡结束再交互：否则 Playwright 偶发判定 element not stable 并重试到超时
    await drawer.evaluate((el) =>
      Promise.all(el.getAnimations({ subtree: true }).map((a) => a.finished)).then(() => undefined),
    );

    // 打开退款弹窗，空原因提交被拦截
    await drawer.getByRole("button", { name: "退款" }).click();
    const refundDialog = page.getByRole("dialog", { name: "人工退款" });
    await expect(refundDialog).toBeVisible();
    await refundDialog.getByRole("button", { name: "确认退款" }).click();
    await expect(page.getByText("请填写退款原因")).toBeVisible();
    expect(captures.length).toBe(0);

    // 填写原因 → POST /api/admin/order/refund
    await refundDialog.locator("textarea").fill("重复支付，用户申请退款");
    await refundDialog.getByRole("button", { name: "确认退款" }).click();
    await expect
      .poll(() => captures.some((c) => c.url === "/api/admin/order/refund"))
      .toBe(true);
    const body = captures[0].body as { order_id: number; reason: string };
    expect(body.order_id).toBe(101);
    expect(body.reason).toBe("重复支付，用户申请退款");
    await expect(page.getByText("订单 LD20261020001 已退款")).toBeVisible();
  });

  test("need_manual 订单：人工入账块 → 空原因拦截 → 提交 POST 契约", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const captures: { url: string; body: unknown }[] = [];
    await stub(page, captures);
    await page.goto("/admin/orders");

    // need_manual 同时提供「退款（仅回退平台余额）」与「人工入账」：
    // 上游已退款的单要扣回额度，丢单的单要补入账，两种处置都可能对，由运营按凭据选。
    await page
      .locator(".tbl tbody tr", { hasText: "LD20261020002" })
      .getByRole("button", { name: "详情" })
      .click();
    const drawer = page.getByRole("dialog", { name: "订单详情" });
    await expect(drawer).toBeVisible();
    await expect(drawer).toContainText("② 人工退款");
    await expect(drawer).toContainText("③ 人工入账");

    await drawer.getByRole("button", { name: "人工入账" }).click();
    const creditDialog = page.getByRole("dialog", { name: "人工入账" });
    await expect(creditDialog).toBeVisible();

    // 空原因提交被拦截，零请求
    await creditDialog.getByRole("button", { name: "确认入账" }).click();
    await expect(page.getByText("请填写入账原因")).toBeVisible();
    expect(captures.length).toBe(0);

    // 填写原因 → POST /api/admin/order/manual-credit
    await creditDialog.locator("textarea").fill("线下核实已付款，超限转人工");
    await creditDialog.getByRole("button", { name: "确认入账" }).click();
    await expect
      .poll(() => captures.some((c) => c.url === "/api/admin/order/manual-credit"))
      .toBe(true);
    const body = captures[0].body as { order_id: number; reason: string };
    expect(body.order_id).toBe(102);
    expect(body.reason).toBe("线下核实已付款，超限转人工");
    await expect(page.getByText("订单 LD20261020002 已入账")).toBeVisible();
  });
});

const APP_OPTIONS_ORDERS = [
  { id: 5, slug: "web-portal", name: "web-portal", display_name: "官网门户", status: 1 },
  { id: 9, slug: "shadow", name: "影画", display_name: "", status: 2 },
];

test.describe("订单管理 · 按应用筛选", () => {
  test("「平台直连」传 0、某应用传 id；表格出现应用列", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page);
    await page.route("**/api/admin/apps/options**", (route) =>
      route.fulfill({ json: envelope({ items: APP_OPTIONS_ORDERS, truncated: false }) }),
    );
    await page.goto("/admin/orders");
    await expect(page.locator(".tbl thead")).toContainText("应用");

    // 平台直连 = app_id 0：这是唯一能单独统计"用户自充"的筛选，
    // 被当成空值吞掉就会静默变成"筛全量"。
    await page.getByRole("combobox", { name: "应用" }).click();
    await page.getByRole("option", { name: "平台直连" }).click();
    await expect
      .poll(() => calls.some((c) => c.startsWith("GET /api/admin/orders?") && c.includes("app_id=0")))
      .toBe(true);

    await page.getByRole("combobox", { name: "应用" }).click();
    await page.getByRole("option", { name: /官网门户/ }).click();
    await expect
      .poll(() => calls.some((c) => c.startsWith("GET /api/admin/orders?") && c.includes("app_id=5")))
      .toBe(true);
  });
});
