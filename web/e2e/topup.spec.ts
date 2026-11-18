/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, expectNoHScroll, seedAuth } from "./helpers";

/** 门户用户信息（钱包 hero 与门户 chrome 共用） */
const ME = {
  id: 1,
  username: "e2e",
  role: 1,
  status: 1,
  group: "default",
  balance: 10000000,
  frozen: 0,
  used: 1200000,
  request_count: 0,
  key_count: 0,
  email: "",
  phone: "",
};

/** 套餐 + 钱包配置（基础/赠送/到账总额均由服务端下发） */
const PACKAGES = {
  items: [
    { id: 1, name: "基础包", price_cent: 3000, base: 30000000, bonus: 0, credited: 30000000 },
    // badge 由后端下发：只有第二档带角标，用来断言「空值不渲染 DOM」
    {
      id: 2,
      name: "标准包",
      price_cent: 10000,
      base: 100000000,
      bonus: 2000000,
      credited: 102000000,
      badge: "recommend",
    },
  ],
  custom_topup_enabled: true,
  min_topup: 1,
  balance_limit: 0,
  // 到账 = round(金额 × 充值倍率 × 额度刻度)：两个乘数都要下发，缺一就会与后端算错
  scale: 1000000,
  recharge_multiplier: 1,
  currency: "USD",
  currency_symbol: "$",
  cny_per_usd: 7.2,
  // 门户只下发 {method, label}：承接渠道（易支付/虎皮椒）属于供应链信息，用户端不出现
  pay_options: [
    { method: "wechat", label: "微信支付" },
    { method: "alipay", label: "支付宝" },
  ],
};

const AFF = {
  aff_code: "ABCD2345",
  aff_count: 3,
  // 一段冷却中、一段可转出：两种金额都非 0，页面读错字段会立刻显示 ¥0.00
  aff_available: 200_000,
  aff_cooling: 900_000,
  aff_history: 1_100_000,
  enabled: true,
  min_transfer: 100_000,
  inviter_rate: 100_000,
  invitee_amount: 5_000_000,
  max_rewards: 3,
  hold_hours: 48,
};

/** 订单桩件：pending（可续付场景用 expired）+ 已支付结果（门户投影，无渠道字段） */
const ORDERS = [
  {
    id: 42,
    out_trade_no: "LD2026102000000000001",
    trade_no: "",
    amount_cent: 10000,
    credited: 100000000,
    status: "pending",
    pay_method: "wechat",
    pay_method_label: "微信支付",
    created_at: "2026-10-20T04:00:00Z",
    paid_at: null,
  },
  {
    id: 41,
    out_trade_no: "LD2026102000000000000",
    trade_no: "",
    amount_cent: 3000,
    credited: 30000000,
    status: "expired",
    pay_method: "alipay",
    pay_method_label: "支付宝",
    created_at: "2026-10-20T03:00:00Z",
    paid_at: null,
  },
];

/** 管理台订单桩件：在门户投影之上补回归属与渠道字段（管理台本就该看到这些） */
const ADMIN_ORDERS = ORDERS.map((o) => ({
  ...o,
  user_id: 1,
  username: "e2e",
  pay_channel: "易支付-微信支付",
  pay_channel_id: "epay",
  pay_protocol: "epay",
}));

const TOPUP_RESULT = {
  id: 42,
  out_trade_no: "LD2026102000000000001",
  amount_cent: 10000,
  credited: 100000000,
  pay_method: "wechat",
  pay_method_label: "微信支付",
  pay_url: "https://pay.example.com/submit.php?pid=1000&out_trade_no=LD2026102000000000001",
  cashier_url: "https://pay.example.com/submit.php?pid=1000&out_trade_no=LD2026102000000000001",
  qr_ref: "",
  // 服务端下发剩余秒数，前端据此倒计时（不用本地时钟减 expire_at）
  expires_in: 300,
};

/** 支付状态开关：置 true 后订单接口返回 paid（模拟回调到账） */
interface StubState {
  paid: boolean;
  redeemError: boolean;
}

/** 打桩：钱包页所需的全部门户端点（按路径分流，避免路由顺序耦合） */
async function stubWallet(page: Page, state: StubState): Promise<void> {
  // 计费口径属于页面启动时的全局注入（main.tsx → /api/pricing）。
  // 不桩件时余额符号与折算会跟着"本机后端的当前配置"变化，等于测试在测环境。
  // 本文件按 USD 口径断言（$10 / ¥72.00），因此这里锁 USD。
  await page.route("**/api/pricing", (route) =>
    route.fulfill({
      json: envelope({
        models: [],
        vendors: [],
        scale: 1000000,
        currency: "USD",
        currency_symbol: "$",
        cny_per_usd: 7.2,
        other_currency: "CNY",
        other_quota_per_unit: 69444,
        defaults: {},
      }),
    }),
  );

  await page.route("**/api/portal/me", (route) => route.fulfill({ json: envelope(ME) }));

  await page.route("**/api/portal/packages**", (route) =>
    route.fulfill({ json: envelope(PACKAGES) }),
  );

  await page.route("**/api/portal/aff**", (route) => {
    const { pathname } = new URL(route.request().url());
    if (pathname.endsWith("/records")) {
      return route.fulfill({
        json: envelope({ items: [], total: 0, page: 1, page_size: 20 }),
      });
    }
    return route.fulfill({ json: envelope(AFF) });
  });

  await page.route("**/api/portal/transactions**", (route) =>
    route.fulfill({ json: envelope({ items: [], total: 0, page: 1, page_size: 20 }) }),
  );

  await page.route("**/api/portal/orders**", (route) => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith("/continue")) {
      return route.fulfill({
        json: envelope({ ...TOPUP_RESULT, id: 41, amount_cent: 3000, credited: 30000000 }),
      });
    }
    const status = url.searchParams.get("status") ?? "";
    const items = ORDERS.filter((o) => !status || o.status === status).map((o) =>
      state.paid && o.id === 42 ? { ...o, status: "paid", paid_at: "2026-10-20T04:01:00Z" } : o,
    );
    return route.fulfill({
      json: envelope({ items, total: items.length, page: 1, page_size: 20 }),
    });
  });

  await page.route("**/api/portal/topup", (route) =>
    route.fulfill({ json: envelope({ ...TOPUP_RESULT, credited: 100000000 }) }),
  );

  await page.route("**/api/portal/redeem", (route) =>
    state.redeemError
      ? route.fulfill({
          json: envelope(null, 400, "兑换码无效", "redeem_invalid"),
        })
      : route.fulfill({ json: envelope({ amount: 2000000 }) }),
  );
}

test.describe("S9 钱包 · 门户", () => {
  test("套餐直达收银台 → 支付到账 → 成功态", async ({ page }) => {
    const state: StubState = { paid: false, redeemError: false };
    await seedAuth(page);
    await stubWallet(page, state);

    await page.goto("/topup");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("钱包");
    // 页头文案回归原型口径，且无 actions（兑换码入口在充值与兑换码 Tabs 内）
    await expect(page.locator("#pageDesc")).toHaveText("充值 · 兑换码 · 邀请奖励 · 订单实时入账");
    await expect(page.locator(".ph-acts")).toHaveCount(0);

    // 余额卡：主口径为 $，附带 ¥ 折算 / 冻结 / 累计消耗
    await expect(page.getByTestId("wallet-balance")).toHaveText("$10.00");
    await expect(page.getByTestId("wallet-hero")).toContainText("¥72.00");
    await expect(page.getByTestId("wallet-hero")).toContainText("累计消耗");

    // 套餐卡：价签符号跟随结算币种（USD 部署下必须是 $，写死 ¥ 会让 $30 的档位显示成 ¥30）
    const cards = page.getByTestId("pkg-card");
    await expect(cards).toHaveCount(2);
    await expect(cards.first()).toContainText("$30.00");
    // 赠送为 0 的档位：不渲染「基础 / 赠送」整块（+$0.00 是噪音，且会让人以为赠送丢了）
    await expect(cards.first().locator(".p-detail")).toHaveCount(0);
    await expect(cards.first()).not.toContainText("赠送");
    await expect(cards.nth(1)).toContainText("$100.00");
    await expect(cards.nth(1).locator(".p-detail")).toHaveCount(1);
    await expect(cards.nth(1)).toContainText("+$2");
    // 角标：只有带 badge 的卡片渲染节点，且文案来自 PACKAGE_BADGE_LABELS
    await expect(page.locator(".pkg-ribbon")).toHaveCount(1);
    await expect(cards.first().locator(".pkg-ribbon")).toHaveCount(0);
    await expect(cards.nth(1).locator(".pkg-ribbon")).toHaveText("推荐");

    // 点击套餐 → 收银台确认（金额 / 拆分 / 支付方式）
    await cards.nth(1).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toContainText("收银台");
    await expect(dialog).toContainText("应付金额");
    await expect(dialog).toContainText("$100.00");
    await expect(dialog).toContainText("基础额度");
    await expect(dialog).toContainText("到账合计");
    // 卡片按 data-method 精确定位（用户只看到方式名，没有承接渠道小字）
    await expect(dialog.locator('[data-method="wechat"]')).toContainText("微信支付");
    await expect(dialog.locator('[data-method="alipay"]')).toContainText("支付宝");
    // 品牌图标：两种方式各一个，品牌色由 data-brand 决定
    await expect(dialog.locator('.pay-ch-ic[data-brand="wechat"]')).toHaveCount(1);
    await expect(dialog.locator('.pay-ch-ic[data-brand="alipay"]')).toHaveCount(1);
    // 收银台不得暴露承接渠道（易支付/虎皮椒这类聚合支付商名）
    await expect(dialog).not.toContainText("易支付");

    // 去支付 → 支付中（订单号 / 收银台地址 / 倒计时）
    await dialog.getByRole("button", { name: "去支付" }).click();
    const paying = page.getByTestId("cashier-pay");
    await expect(paying).toBeVisible();
    await expect(paying).toContainText("LD2026102000000000001");
    await expect(paying).toContainText("剩余 04:");
    // 方式名展示中文（回归「拿快照串去 options.value 里找、永不命中」）
    await expect(paying).toContainText("微信支付");
    await expect(paying).not.toContainText("易支付-微信支付");
    const cashier = paying.getByRole("link", { name: "打开支付页面" });
    await expect(cashier).toHaveAttribute("href", /submit\.php/);

    // 回调到账（订单接口转 paid）→ 我已完成支付 → 成功态
    state.paid = true;
    await dialog.getByRole("button", { name: "我已完成支付" }).click();
    await expect(page.getByTestId("cashier-success")).toContainText("支付成功");
    await expect(page.getByTestId("cashier-success")).toContainText("LD2026102000000000001");

    await dialog.getByRole("button", { name: "完成" }).click();
    await expect(dialog).not.toBeVisible();
  });

  test("虎皮椒扫码：收银台展示后端代理后的二维码", async ({ page }) => {
    const state: StubState = { paid: false, redeemError: false };
    await seedAuth(page);
    await stubWallet(page, state);

    // 虎皮椒下单返回 qr_ref（上游图片地址），二维码接口返回后端代理后的 data URL
    await page.route("**/api/portal/topup", (route) =>
      route.fulfill({
        json: envelope({
          ...TOPUP_RESULT,
          pay_method: "aggregate",
          pay_method_label: "聚合支付",
          pay_url: "",
          cashier_url: "",
          qr_ref: "https://api.xunhupay.com/qr/1.png",
        }),
      }),
    );
    await page.route("**/api/portal/orders/42/qrcode", (route) =>
      route.fulfill({
        json: envelope({
          qr: { mode: "image", data_url: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg==" },
          expires_in: 300,
        }),
      }),
    );

    await page.goto("/topup");
    await page.getByTestId("pkg-card").nth(1).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "去支付" }).click();

    const pane = page.getByTestId("pay-qr-image");
    await expect(pane).toBeVisible();
    // 必须是后端代理的 data URL：直接指向上游会被防盗链/混合内容/img 无法带 Bearer 头挡住
    await expect(pane.locator("img")).toHaveAttribute("src", /^data:image\/png;base64,/);
    await expect(pane).toContainText("扫码支付");
  });

  test("官方直连扫码：内容型二维码由前端渲染，并保留复制链接", async ({ page }) => {
    const state: StubState = { paid: false, redeemError: false };
    await seedAuth(page);
    await stubWallet(page, state);

    // 微信 Native / 支付宝当面付返回的都是**内容**（不是图片地址），
    // 后端必须原样透传（不能去拉它），前端本地画成二维码。
    await page.route("**/api/portal/topup", (route) =>
      route.fulfill({
        json: envelope({
          ...TOPUP_RESULT,
          pay_method: "wechat",
          pay_method_label: "微信支付",
          pay_url: "",
          cashier_url: "",
          qr_ref: "weixin://wxpay/bizpayurl?pr=abc123",
        }),
      }),
    );
    await page.route("**/api/portal/orders/42/qrcode", (route) =>
      route.fulfill({
        json: envelope({
          qr: { mode: "content", content: "weixin://wxpay/bizpayurl?pr=abc123" },
          expires_in: 300,
        }),
      }),
    );

    await page.goto("/topup");
    await page.getByTestId("pkg-card").nth(1).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name: "去支付" }).click();

    const pane = page.getByTestId("pay-qr-content");
    await expect(pane).toBeVisible();
    // 二维码真的画出来了（不是裂图、也不是只给一串链接）
    await expect(pane.getByTestId("pay-qr-rendered")).toBeVisible();
    await expect(pane.getByTestId("pay-qr-rendered")).toHaveAttribute("src", /^data:image\/png;base64,/);
    // 画不出来时仍要能复制：按钮必须还在
    await expect(pane.getByRole("button", { name: "复制链接" })).toBeVisible();
  });

  test("兑换码：成功入账与无效码提示", async ({ page }) => {
    const state: StubState = { paid: false, redeemError: false };
    await seedAuth(page);
    await stubWallet(page, state);

    await page.goto("/topup");
    await page.getByRole("tab", { name: "兑换码" }).click();
    await expect(page).toHaveURL(/tab=redeem/);

    const input = page.locator("#redeem-code");
    await input.fill("abcd-efgh-ijkl");
    // 输入即大写（对齐冻结格式）
    await expect(input).toHaveValue("ABCD-EFGH-IJKL");

    await page.getByRole("button", { name: "兑换", exact: true }).click();
    await expect(page.getByText("兑换成功，已入账 $2")).toBeVisible();

    // 无效码：后端返回 error_code=redeem_invalid，提示后端文案
    state.redeemError = true;
    await input.fill("ZZZZ-ZZZZ-ZZZZ");
    await page.getByRole("button", { name: "兑换", exact: true }).click();
    await expect(page.getByText("兑换码无效").first()).toBeVisible();
  });

  test("旧书签 #redeem：收敛为 ?tab=redeem 且首帧就停在兑换码", async ({ page }) => {
    const state: StubState = { paid: false, redeemError: false };
    await seedAuth(page);
    await stubWallet(page, state);

    // Home 的两处 hash="redeem" 快捷入口与 Settings 的旧链接最终都落到这个 URL
    await page.goto("/topup#redeem");

    await expect(page.getByRole("tab", { name: "兑换码" })).toHaveAttribute("aria-selected", "true");
    await expect(page.locator("#redeem-code")).toBeVisible();
    // fragment 被收敛成 query（规范 URL），刷新后仍停兑换码
    await expect(page).toHaveURL(/tab=redeem/);
    await expect(page).not.toHaveURL(/#redeem/);

    await page.reload();
    await expect(page.getByRole("tab", { name: "兑换码" })).toHaveAttribute("aria-selected", "true");
  });

  test("订单筛选与过期续付", async ({ page }) => {
    const state: StubState = { paid: false, redeemError: false };
    await seedAuth(page);
    await stubWallet(page, state);

    await page.goto("/topup");
    await page.getByRole("tab", { name: "充值订单" }).click();
    await expect(page).toHaveURL(/rec=orders/);

    // 默认全量：2 笔（pending + expired），仅过期单可续付
    await expect(page.locator("tbody tr")).toHaveCount(2);
    await expect(page.getByRole("button", { name: "续付" })).toBeVisible();

    // 状态筛选透传到服务端：仅剩过期单
    await page.getByLabel("订单状态").selectOption("paid");
    await expect(page.locator("tbody tr")).toHaveCount(0);

    await page.getByLabel("订单状态").selectOption("expired");
    await expect(page.locator("tbody tr")).toHaveCount(1);
    await expect(page.locator("tbody tr").first()).toContainText("LD2026102000000000000");

    // 续付 → 生成新支付单并进入收银台
    await page.getByRole("button", { name: "续付" }).click();
    const paying = page.getByTestId("cashier-pay");
    await expect(paying).toBeVisible();
    await expect(paying).toContainText("续付订单 LD2026102000000000000");
    await expect(paying).toContainText("预计入账");
  });
});

test.describe("S9 钱包 · 管理台", () => {
  test("订单管理：筛选 / 明细 / 人工退款原因必填", async ({ page }) => {
    const refundBodies: Record<string, unknown>[] = [];
    await seedAuth(page, { role: 0 });
    await page.route("**/api/portal/me", (route) =>
      route.fulfill({ json: envelope({ ...ME, username: "admin", role: 0 }) }),
    );
    // 订单页支付方式筛选项来自 /admin/orders 的 pay_channels；
    // 这里同时桩 /portal/packages 与 /admin/config，避免以假 token 打真实后端 401 触发全局登出
    await page.route("**/api/portal/packages", (route) =>
      route.fulfill({ json: envelope({ packages: [], pay_options: [{ method: "alipay", label: "支付宝" }] }) }),
    );
    await page.route("**/api/admin/config", (route) =>
      route.fulfill({ json: envelope({ config: { recharge_ratio: 69444, recharge_multiplier: 1 } }) }),
    );
    await page.route("**/api/admin/orders**", (route) =>
      route.fulfill({
        json: envelope({
          items: [{ ...ADMIN_ORDERS[0], status: "paid", paid_at: "2026-10-20T04:01:00Z" }],
          total: 1,
          page: 1,
          page_size: 20,
          pay_channels: [{ id: "epay", name: "易支付", protocol: "epay", enabled: true, methods: ["wechat"] }],
        }),
      }),
    );
    await page.route("**/api/admin/order/refund", (route) => {
      refundBodies.push(route.request().postDataJSON() as Record<string, unknown>);
      return route.fulfill({ json: envelope(null) });
    });

    await page.goto("/admin/orders");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("订单管理");
    await expect(page.locator(".pbar-fields select")).toHaveCount(2);
    // 原型筛选条仅「状态 + 渠道」两个下拉（原「用户 ID」搜索已按原型收敛）
    await expect(page.locator('.pbar input[type="search"]')).toHaveCount(0);

    const head = page.locator(".tbl thead");
    await expect(head).toContainText("订单号");
    await expect(head).toContainText("到账金额");
    await expect(head).toContainText("操作");
    await expect(page.locator(".tbl tbody tr")).toHaveCount(1);
    await expect(page.locator(".tbl tbody tr").first()).toContainText("已支付");

    // 明细抽屉 → 退款弹窗（行操作也有同名「退款」，必须先收口到抽屉内）
    const drawer = page.getByRole("dialog", { name: "订单详情" });
    await page.getByRole("button", { name: "详情" }).click();
    await expect(drawer).toBeVisible();
    await drawer.getByRole("button", { name: "退款", exact: true }).click();

    // 原因必填：空提交被前端拦截（不发请求）
    await page.getByRole("button", { name: "确认退款" }).click();
    await expect(page.getByText("请填写退款原因").first()).toBeVisible();
    expect(refundBodies).toHaveLength(0);

    // 填写原因后提交，body 为 {order_id, reason}
    await page.locator("#rf-reason").fill("重复支付，用户申请退款");
    await page.getByRole("button", { name: "确认退款" }).click();
    await expect(page.getByText(/LD2026102000000000001 已退款/).first()).toBeVisible();
    expect(refundBodies[0]).toMatchObject({ order_id: 42, reason: "重复支付，用户申请退款" });
  });
});

test.describe("S9 钱包 · 375px", () => {
  test.use({ viewport: { width: 375, height: 800 } });

  test("移动端无横向滚动，套餐双列，邀请奖励与资金记录纵向堆叠", async ({ page }) => {
    const state: StubState = { paid: false, redeemError: false };
    await seedAuth(page);
    await stubWallet(page, state);

    await page.goto("/topup");
    await expect(page.getByTestId("wallet-balance")).toHaveText("$10.00");

    // 套餐双列（移动端不塌成单列，保持卡片紧凑）
    const cards = page.getByTestId("pkg-card");
    await expect(cards).toHaveCount(2);
    const first = await cards.first().boundingBox();
    const second = await cards.nth(1).boundingBox();
    expect(first && second && Math.abs(first.y - second.y) < 4, "套餐应同排双列").toBeTruthy();

    await expect(page.getByTestId("aff-code")).toHaveText("ABCD2345");

    await expectNoHScroll(page, "375px 出现横向溢出");
  });
});
