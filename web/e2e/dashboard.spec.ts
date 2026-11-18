/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, isAdminPageRequest, seedAuth } from "./helpers";

/** GET /api/admin/data/board fixture（S11 契约冻结：单请求下发全量） */
function boardFixture() {
  return {
    today: {
      requests: 1234,
      recharge: 720_000_000,
      charge: 250_000,
      cost: 0,
      cost_collected: false,
      success_rate: 0.987,
      channels_online: 3,
      channels_total: 4,
      price_missing_count: 2,
    },
    trend: [
      { date: "2026-10-14", recharge: 144000000, charge: 100000, cost: 0 },
      { date: "2026-10-15", recharge: 0, charge: 124000, cost: 0 },
      { date: "2026-10-16", recharge: 288000000, charge: 160000, cost: 0 },
      { date: "2026-10-17", recharge: 0, charge: 82000, cost: 0 },
      { date: "2026-10-18", recharge: 144000000, charge: 180000, cost: 0 },
      { date: "2026-10-19", recharge: 72000000, charge: 154000, cost: 0 },
      { date: "2026-10-20", recharge: 720000000, charge: 500000, cost: 0 },
    ],
    fail_top: [
      { label: "claude-sonnet", value: 12 },
      { label: "gpt-4o", value: 5 },
    ],
    channel_top: [
      { label: "OpenAI 主线路", value: 180000 },
      { label: "火山方舟", value: 70000 },
    ],
    channels: [
      { id: 1, name: "OpenAI 主线路", status: 1, auto_ban: true, fail_rate: 0.01, success_count: 99, fail_count: 1 },
      { id: 2, name: "火山方舟", status: 3, auto_ban: true, fail_rate: 0.72, success_count: 28, fail_count: 72 },
    ],
    boards: {
      d7: {
        user_top: [{ label: "alice", value: 0.5 }],
        model_top: [{ label: "gpt-4o", value: 12.5 }],
        users: [{ user_id: 1, username: "alice", group: "default", requests: 120, charge: 500_000 }],
      },
      d30: { user_top: [], model_top: [], users: [] },
    },
  };
}

/** 全绿基线：无未配价模型、渠道均健康、无失败 —— 用于断言零告警空态 */
function healthyBoardFixture() {
  const board = boardFixture();
  board.today.price_missing_count = 0;
  board.today.channels_online = 4;
  board.today.channels_total = 4;
  board.channels = board.channels.map((c) => ({
    ...c,
    status: 1,
    fail_rate: 0,
    success_count: 100,
    fail_count: 0,
  }));
  board.fail_top = [];
  return board;
}

/** 管理台页面必须打桩 /api/admin/**（否则真实 401 触发全局登出）；记录已发生的请求 */
async function stub(page: Page, board: unknown): Promise<string[]> {
  const calls: string[] = [];
  // 仪表盘的「今日收入/成本/毛利」是**真实人民币收付**（只有趋势图统一折算成美元），
  // 因此本文件锁人民币口径；不桩会用 seedAuth 的美元兜底，¥720 会被显示成 $720。
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
      const { pathname } = new URL(route.request().url());
      calls.push(`${route.request().method()} ${pathname}`);

      if (pathname === "/api/admin/data/board") {
        return route.fulfill({ json: envelope(board) });
      }
      if (pathname === "/api/portal/announcements") {
        return route.fulfill({ json: envelope({ enabled: false, items: [] }) });
      }
      if (pathname === "/api/portal/config") {
        return route.fulfill({
          json: envelope({
            site: { site_name: "灵动 API", logo_image: "" },
            group_ratios: { default: 1 },
            auth: {
              register_mode: "open",
              contact_verify: "off",
              contact_channels: ["email"],
              captcha_register: "always",
              captcha_login: "after_fails",
              captcha_reset: "always",
              login_fail_threshold: 5,
            },
            custom_topup_enabled: true,
            min_topup: 1,
            email_enabled: false,
          }),
        });
      }
      return route.fulfill({ json: envelope(null) });
    },
  );
  return calls;
}

const BOARD_CALL = "GET /api/admin/data/board";

test.describe("S11 仪表盘 · 管理台", () => {
  test("今日指标单位口径、告警与区间看板切换", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page, boardFixture());
    await page.goto("/admin/dashboard");

    await expect(page.getByRole("heading", { level: 1 })).toHaveText("仪表盘");
    await expect(page.locator(".stat-card")).toHaveCount(6);

    // 单位口径（G6）：收入/成本/毛利以元（¥）、趋势图统一美元
    await expect(page.locator(".stat-card", { hasText: "今日收入" }).locator(".value")).toHaveText("¥720.00");
    await expect(page.locator(".stat-card", { hasText: "今日成本" }).locator(".value")).toHaveText("—");
    await expect(page.locator(".stat-card", { hasText: "今日毛利" }).locator(".value")).toHaveText("—");
    await expect(page.locator(".stat-card", { hasText: "成功率" }).locator(".value")).toHaveText("98.7%");
    await expect(page.locator(".stat-card", { hasText: "在线渠道" }).locator(".value")).toHaveText("3/4");

    // 未配价 / 熔断两类告警条，动作右对齐跳转处理页
    await expect(page.locator(".alert-banner.warn")).toContainText("2");
    await expect(page.locator(".alert-banner.warn .right")).toHaveText("去处理 →");
    await expect(page.locator(".alert-banner.danger")).toContainText("1");

    // 趋势卡：收入 vs 成本（单图单单位·美元）
    await expect(
      // 标题里的币种跟随结算配置（不再写死「美元」）
      page.locator(".card", { hasText: "收入 vs 成本（近 7 天" }),
    ).toBeVisible();

    // 渠道健康：健康点 + 熔断态标签 + 失败率
    const health = page.locator(".card", { hasText: "渠道健康" });
    await expect(health.locator(".chan-row", { hasText: "火山方舟" }).locator(".badge")).toHaveText("自动禁用");
    await expect(health.locator(".chan-row", { hasText: "火山方舟" })).toContainText("72.0%");
    await expect(health.locator(".chan-row", { hasText: "火山方舟" }).locator(".health-dot.bad")).toHaveCount(1);

    // 数据看板 d7：用户聚合明细（quota → 当前结算币种金额）+ 用户/模型双 Top
    // 金额一律跟随 moneyCtx（本文件锁人民币；消耗 500_000 微 = ¥0.50），不再写死 $
    const boardCard = page.locator(".card", { hasText: "数据看板" });
    await expect(boardCard.locator("tbody tr", { hasText: "alice" })).toContainText("¥0.5");
    await expect(boardCard.getByText("用户消耗 Top")).toBeVisible();
    await expect(boardCard.getByText("模型消耗 Top")).toBeVisible();
    await expect(boardCard.locator("tbody tr", { hasText: "alice" })).toContainText("100%");

    // d7/d30 同响应下发 → 切档零新增请求
    const before = calls.filter((c) => c === BOARD_CALL).length;
    await page.getByRole("tab", { name: "近 30 天" }).click();
    await expect(page.getByText("该区间暂无用户消耗排行")).toBeVisible();
    await expect(page.getByText("该区间暂无模型消耗")).toBeVisible();
    await expect(page.getByText("该区间暂无用户消耗", { exact: true })).toBeVisible();
    expect(calls.filter((c) => c === BOARD_CALL).length).toBe(before);
  });

  test("价格齐备且渠道全健康时无告警，失败榜显空态", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page, healthyBoardFixture());
    await page.goto("/admin/dashboard");

    await expect(page.getByRole("heading", { level: 1 })).toHaveText("仪表盘");
    await expect(page.locator(".alert-banner")).toHaveCount(0);
    await expect(page.getByText("近 7 天无失败调用")).toBeVisible();
    await expect(page.locator(".stat-card", { hasText: "在线渠道" }).locator(".value")).toHaveText("4/4");
    await expect(page.locator(".stat-card", { hasText: "今日成本" }).locator(".sub")).toHaveText("成本采集未启用");
  });
});
