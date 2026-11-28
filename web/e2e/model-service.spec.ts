/**
 * 模型服务（/admin/model-service）：左栏「适配器卡片」+ 右侧「渠道浮层」+ 默认全部模型。
 *
 * 这一版替换掉原来的级联列表用例。四处必须锁死的语义（都会"安静地错"）：
 *  1. 打开浮层**不动**右栏（`?c` 不变）—— 点一下适配器就清空右栏是旧实现的经典缺陷；
 *  2. 选中渠道**必关**浮层（否则盖住刚切过去的表格）；
 *  3. 无 `?c` 就是全部模型，且请求**不带** `channel_id`（前端不该先打一次全量再打一次）；
 *  4. 浮层左缘与左栏右缘零间隙、顶边对齐所点卡片，切卡片时整体跟随。
 */
import { expect, test, type Page } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

const seedAdmin = (page: Page) => seedAuth(page, { role: 0 });

const ADAPTERS = [
  { id: "spec:my-gw", label: "我的网关", name: "我的网关", vendor: "", caps: ["chat"], status: 1, source: "custom", editable: true, deletable: true, spec_id: 7, spec_version: 2, channel_count: 2 },
  { id: "openai", label: "OpenAI", name: "OpenAI", vendor: "openai", caps: ["chat"], status: 1, source: "builtin", editable: false, deletable: false, channel_count: 0 },
];

const CHANNELS = [
  {
    id: 11, name: "网关主线路", icon: "", icon_url: "", adapter_id: "spec:my-gw", base_url: "https://gw.example.com",
    multi_key_mode: "random", groups: ["default"], weight: 1, priority: 1, rpm_limit: 0, max_concurrency: 0,
    inflight: 0, inflight_limit: 0, status: 1, auto_ban: true, switch_key_retry: false, key_count: 2, key_enabled_count: 2,
    version: 1, health: {}, adapter_config: {}, adapter_options: null,
    bound_models: [
      { model: "gpt-4o", alias: "gpt-4o", weight: 1, enabled: true },
      { model: "gpt-4o-mini", alias: "mini", weight: 1, enabled: false },
    ],
    created_at: "", updated_at: "",
  },
  {
    // 停用渠道：浮层里必须给出状态徽标（否则看起来一切正常）
    id: 12, name: "备用线路", icon: "", icon_url: "", adapter_id: "spec:my-gw", base_url: "https://gw2.example.com",
    multi_key_mode: "random", groups: ["default"], weight: 6, priority: 2, rpm_limit: 0, max_concurrency: 0,
    inflight: 0, inflight_limit: 0, status: 2, auto_ban: true, switch_key_retry: false, key_count: 1, key_enabled_count: 1,
    version: 1, health: {}, adapter_config: {}, adapter_options: null,
    bound_models: [], created_at: "", updated_at: "",
  },
  {
    // 孤儿：adapter_id 不在适配器列表里（规格被停用/删除后的残留）
    id: 13, name: "失联线路", icon: "", icon_url: "", adapter_id: "spec:gone", base_url: "https://old.example.com",
    multi_key_mode: "random", groups: ["default"], weight: 1, priority: 1, rpm_limit: 0, max_concurrency: 0,
    inflight: 0, inflight_limit: 0, status: 1, auto_ban: true, switch_key_retry: false, key_count: 1, key_enabled_count: 1,
    version: 1, health: {}, adapter_config: {}, adapter_options: null,
    bound_models: [], created_at: "", updated_at: "",
  },
];

const MODEL = {
  id: "gpt-4o",
  display_name: "GPT-4o",
  description: "",
  vendor: "openai",
  vendor_name: "OpenAI",
  vendor_color: "#10a37f",
  icon: "",
  icon_own: "",
  icon_url: "",
  modality: "chat",
  billing_mode: "tokens",
  enabled: true,
  is_default: false,
  is_free: false,
  prices: { input: 3_000_000, output: 15_000_000 },
  spec_prices: null,
  endpoints: null,
  capabilities: null,
  tags: [],
  context_window: 0,
  max_output_tokens: 0,
  version: 1,
  bindings: 1,
  binding: { upstream_model: "gpt-4o-2024", weight: 2, priority: 1, enabled: true },
};

async function stub(page: Page, captured: string[] = []) {
  await page.route("**/api/admin/**", async (route, req) => {
    const u = new URL(req.url());
    const p = u.pathname;
    if (p === "/api/admin/adapters") return route.fulfill({ json: envelope({ items: ADAPTERS }) });
    if (p === "/api/admin/channels") {
      captured.push(`GET ${p}?${u.searchParams.toString()}`);
      return route.fulfill({ json: envelope({ items: CHANNELS, adapters: ADAPTERS, groups: ["default"], total: 3 }) });
    }
    if (p === "/api/admin/models") {
      captured.push(`GET ${p}?${u.searchParams.toString()}`);
      // 只有带 channel_id 时才回 binding —— 与后端行为一致（没有渠道上下文就没有 binding）
      const ch = u.searchParams.get("channel_id");
      return route.fulfill({
        json: envelope({
          items: ch ? [MODEL] : [{ ...MODEL, binding: undefined }],
          vendors: [{ id: "openai", name: "OpenAI", icon_url: "", color: "#10a37f" }],
          tag_palette: [],
          total: 1,
        }),
      });
    }
    return route.fulfill({ json: envelope(null) });
  });
}

/** 打开某张适配器卡片上的浮层（并返回浮层 locator）。 */
async function openFlyout(page: Page, label: string) {
  await page.locator(".ms-adp", { hasText: label }).click();
  const fly = page.getByRole("dialog", { name: `「${label}」的渠道` });
  await expect(fly).toBeVisible();
  return fly;
}

test.describe("模型服务", () => {
  test("默认就是「全部模型」：左栏只有适配器、右栏是跨渠道模型表", async ({ page }) => {
    await seedAdmin(page);
    const captured: string[] = [];
    await stub(page, captured);
    await page.goto("/admin/model-service");

    const rail = page.locator(".ms-rail");
    await expect(rail).toBeVisible();
    // 需求原文：左栏不做搜索与筛选
    await expect(rail.locator("input")).toHaveCount(0);
    // 左栏里不能出现任何渠道名（渠道属于浮层）
    await expect(rail).not.toContainText("网关主线路");

    // 自建适配器排在前面，内置在后，孤儿垫底
    const names = page.locator(".ms-adp__name");
    await expect(names.first()).toHaveText("我的网关");
    await expect(names.nth(1)).toHaveText("OpenAI");
    await expect(names.last()).toHaveText("未登记适配器");

    // 默认视图 = 全部模型，且第一屏**没有**打开浮层
    await expect(page.locator(".ms-chanbar b")).toHaveText("全部模型");
    await expect(page.locator("#ms-fly")).toHaveCount(0);
    await expect(page).not.toHaveURL(/[?&]c=/);

    // 全量请求确实不带 channel_id（不是先打一次全量再打一次渠道）
    expect(captured.some((c) => c.includes("/api/admin/models") && !c.includes("channel_id="))).toBe(true);
    // 行内不能有「绑定到本渠道」这种没有渠道上下文就没有意义的死入口
    await expect(page.locator(".ms-main").getByText("绑定到本渠道")).toHaveCount(0);
    // 「新建模型」同理：模型必须带着 initial_binding 与渠道一起落库，
    // 没有渠道上下文时建出来的是绑不上也用不了的孤儿模型。
    await expect(page.locator(".pbar .acts").getByRole("button", { name: "新建模型" })).toHaveCount(0);
    await expect(page.locator(".pbar .acts").getByRole("button", { name: "绑定已有模型" })).toHaveCount(0);
    // 第四列是「可路由渠道」：后端下发的是 启用绑定 ∩ 启用渠道，不是绑定总条数
    await expect(page.locator(".ms-main thead")).toContainText("可路由渠道");
  });

  test("浮层贴住左栏右缘、顶边对齐所点卡片，且切卡片时整体跟随", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.goto("/admin/model-service");

    await openFlyout(page, "我的网关");

    const geo = await page.evaluate(() => {
      const rail = document.querySelector(".ms-rail")!.getBoundingClientRect();
      const fly = document.querySelector("#ms-fly")!.getBoundingClientRect();
      const card = document.querySelector('[data-adapter="spec:my-gw"]')!.getBoundingClientRect();
      return { gap: fly.left - rail.right, dTop: fly.top - card.top, railW: rail.width };
    });
    expect(Math.abs(geo.gap)).toBeLessThanOrEqual(1);
    expect(Math.abs(geo.dTop)).toBeLessThanOrEqual(1);
    expect(geo.railW).toBe(200);

    // 打开浮层不能动右栏
    await expect(page).not.toHaveURL(/[?&]c=/);
    await expect(page.locator(".ms-chanbar b")).toHaveText("全部模型");

    // 切到另一张卡：面板整体位移（不是内容原地替换）
    const top1 = await page.locator("#ms-fly").evaluate((el) => el.getBoundingClientRect().top);
    await page.locator(".ms-adp", { hasText: "OpenAI" }).click();
    const fly2 = page.getByRole("dialog", { name: "「OpenAI」的渠道" });
    await expect(fly2).toBeVisible();
    const top2 = await page.locator("#ms-fly").evaluate((el) => el.getBoundingClientRect().top);
    const card2 = await page
      .locator('[data-adapter="openai"]')
      .evaluate((el) => el.getBoundingClientRect().top);
    expect(top2).not.toBe(top1);
    expect(Math.abs(top2 - card2)).toBeLessThanOrEqual(1);
  });

  test("点渠道：写 ?c、关浮层、右栏切渠道视图并带 channel_id", async ({ page }) => {
    await seedAdmin(page);
    const captured: string[] = [];
    await stub(page, captured);
    await page.goto("/admin/model-service");

    const fly = await openFlyout(page, "我的网关");
    // 浮层里是卡片（不是表格），停用渠道带状态徽标
    await expect(fly).toContainText("2 条渠道");
    // 模型计数必须区分"绑定总数 / 可用数"：主线路挂了 2 个模型，其中 1 个绑定已停用
    await expect(fly.locator(".ms-flych", { hasText: "网关主线路" })).toContainText("1/2 个模型");
    await expect(fly.locator(".ms-flych", { hasText: "备用线路" })).toContainText("已停用");

    await fly.locator(".ms-flych", { hasText: "网关主线路" }).click();

    await expect(page).toHaveURL(/[?&]c=11/);
    // 选中即关：浮层不能盖住刚切过去的表格
    await expect(page.locator("#ms-fly")).toHaveCount(0);
    await expect(page.locator(".ms-chanbar b")).toHaveText("网关主线路");

    const main = page.locator(".ms-main");
    await expect(main.getByText("GPT-4o", { exact: true })).toBeVisible();
    // 本渠道列：上游名与权重（weight=2 才显示）
    await expect(main.getByText("gpt-4o-2024")).toBeVisible();
    await expect(main.getByText(/权重 2/)).toBeVisible();
    // 该卡记住"右栏在看它下面的渠道"
    await expect(page.locator('[data-adapter="spec:my-gw"]')).toHaveAttribute("aria-current", "true");

    expect(captured.some((c) => c.includes("channel_id=11"))).toBe(true);
  });

  test("深链 ?c=：一次请求直达渠道视图；不存在的渠道清掉 c 回全部模型", async ({ page }) => {
    await seedAdmin(page);
    const captured: string[] = [];
    await stub(page, captured);

    await page.goto("/admin/model-service?c=11");
    await expect(page.locator(".ms-chanbar b")).toHaveText("网关主线路");
    await expect(page.locator(".ms-main").getByText("GPT-4o", { exact: true })).toBeVisible();
    await expect(page.locator('[data-adapter="spec:my-gw"]')).toHaveAttribute("aria-current", "true");
    // 只发一次 /admin/models：不能"先按全量发一次、拿到渠道再发一次"
    expect(captured.filter((c) => c.includes("/api/admin/models")).length).toBe(1);

    await page.goto("/admin/model-service?c=99999");
    await expect(page).not.toHaveURL(/[?&]c=/);
    await expect(page.locator(".ms-chanbar b")).toHaveText("全部模型");
    await expect(page.locator(".ms-fly")).toHaveCount(0);
  });

  test("再点同一张卡即关闭，且不动右栏正在看的渠道", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.goto("/admin/model-service?c=11");

    await openFlyout(page, "我的网关");
    await page.locator(".ms-adp", { hasText: "我的网关" }).click();
    await expect(page.locator("#ms-fly")).toHaveCount(0);
    // 收浮层 ≠ 取消选中
    await expect(page).toHaveURL(/[?&]c=11/);
    await expect(page.locator(".ms-chanbar b")).toHaveText("网关主线路");
  });

  test("空适配器：浮层给「还没有渠道」，底部仍有唯一的新建入口", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.goto("/admin/model-service");

    const fly = await openFlyout(page, "OpenAI");
    await expect(fly.getByText("这个适配器下还没有渠道")).toBeVisible();
    await expect(fly.getByRole("button", { name: "新建渠道" })).toHaveCount(1);
    // 未开浮层时全页没有「新建渠道」
    await page.locator(".ms-all").click();
    await expect(page.getByRole("button", { name: "新建渠道" })).toHaveCount(0);
  });

  test("点「新建渠道」先关浮层再开抽屉（浮层层级高于抽屉，不关会盖住它）", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.goto("/admin/model-service");

    const fly = await openFlyout(page, "我的网关");
    await fly.getByRole("button", { name: "新建渠道" }).click();

    await expect(page.getByRole("dialog", { name: /新建渠道/ })).toBeVisible();
    await expect(page.locator("#ms-fly")).toHaveCount(0);
  });

  test("孤儿桶：浮层底部是改绑说明，不提供新建", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.goto("/admin/model-service");

    const fly = await openFlyout(page, "未登记适配器");
    await expect(fly).toContainText("失联线路");
    await expect(fly.getByRole("button", { name: "新建渠道" })).toHaveCount(0);
    await expect(fly).toContainText("适配器已失效");
  });

  test("删除当前渠道：清 ?c、右栏回全部模型", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.goto("/admin/model-service?c=12");

    await page.getByRole("button", { name: "删除渠道" }).click();
    await page
      .getByRole("dialog", { name: "删除渠道" })
      .getByRole("button", { name: "删除", exact: true })
      .click();

    await expect(page).not.toHaveURL(/[?&]c=/);
    await expect(page.locator(".ms-chanbar b")).toHaveText("全部模型");
  });

  test("≤899 左栏收进抽屉：钻取渠道 → 选中后自动收起，返回可回适配器列表", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.setViewportSize({ width: 800, height: 900 });
    await page.goto("/admin/model-service");

    const toggle = page.getByRole("button", { name: "打开适配器列表" });
    await expect(toggle).toBeVisible();
    await expect(page.locator(".ms-rail")).toHaveJSProperty("inert", true);

    await toggle.click();
    await expect(page.locator(".ms-ws")).toHaveAttribute("data-rail", "open");
    await expect(page.locator(".ms-scrim")).toBeVisible();
    // 抽屉里点适配器 = 钻取（同一块地方换内容，不叠第二层）
    await page.locator(".ms-adp", { hasText: "我的网关" }).click();
    await expect(page.locator(".ms-rail").getByRole("button", { name: "适配器", exact: true })).toBeVisible();
    await expect(page.locator(".ms-fly")).toHaveCount(0);

    // 返回：回到适配器列表
    await page.locator(".ms-rail").getByRole("button", { name: "适配器", exact: true }).click();
    await expect(page.locator(".ms-adp", { hasText: "OpenAI" })).toBeVisible();

    // 再钻取并选渠道 → 抽屉自动收起
    await page.locator(".ms-adp", { hasText: "我的网关" }).click();
    await page.locator(".ms-flych", { hasText: "网关主线路" }).click();
    await expect(page.locator(".ms-ws")).not.toHaveAttribute("data-rail", "open");
    await expect(page).toHaveURL(/[?&]c=11/);
  });

  test("375px：表格卡片态、无横向滚动，浮层不出现在移动端", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.setViewportSize({ width: 375, height: 780 });
    await page.goto("/admin/model-service");

    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth,
    );
    expect(overflow).toBeLessThanOrEqual(1);
    // 表格在 ≤767 走卡片态（data-label 生效）
    await expect(page.locator('.ms-main td[data-label="模型"]').first()).toBeVisible();
    await expect(page.locator("#ms-fly")).toHaveCount(0);
  });
});
