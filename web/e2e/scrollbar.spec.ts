/**
 * 滚动条契约（overlay 模型）：
 *   ① 页面滚动条**不占布局** —— 有滚动条时 `clientWidth` 仍等于视口宽，顶栏右边界
 *      到屏幕最右；无滚动条时同理，`.pg-sb` 自绘条隐藏。
 *   ② 自绘条必须真的能滚（拖拇指改变 `scrollY`），否则等于把滚动条阉掉。
 *   ③ 锁滚动**不得**让页面宽度变化（此时连补偿都不需要，因为从来不占位）。
 *
 * 为什么断言 `clientWidth` 与顶栏右边界：占位滚动条会把这两者同时减 10 ——
 * 顶栏右侧那块"留白"就是这 10px。用 `.models-grid`（`.p-main` 里居中、max-width
 * 1160）断言是测不出来的：视口 1280 时它怎么都放得下，永远为真。
 */
import { expect, test } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

/**
 * 必须看到**真实滚动条**：headless 默认带 --hide-scrollbars，会让滚动条宽度恒为 0，
 * 补偿逻辑、以及"到底有没有横跳"全都测不出来（用例会绿得毫无意义）。
 */
test.use({ launchOptions: { ignoreDefaultArgs: ["--hide-scrollbars"] } });

/** 最小价目快照：只要能把模型卡铺满一屏（必须有滚动条）、能点开「接入示例」抽屉即可 */
const PRICING = {
  models: [
    {
      id: "gpt-4o",
      display_name: "GPT-4o",
      vendor: "openai",
      vendor_name: "OpenAI",
      vendor_color: "#10a37f",
      modality: "chat",
      billing_mode: "tokens",
      prices: { input: 5_000_000, output: 10_000_000, cache: 2_500_000 },
      spec_prices: null,
      is_default: true,
      endpoints: ["/v1/chat/completions"],
    },
    {
      id: "glm-4",
      display_name: "GLM-4",
      vendor: "openai",
      vendor_name: "OpenAI",
      vendor_color: "#10a37f",
      modality: "chat",
      billing_mode: "tokens",
      prices: { input: 150_000, output: 600_000, cache: 75_000 },
      spec_prices: null,
      is_default: false,
      endpoints: ["/v1/chat/completions"],
    },
    {
      id: "doubao-seedream-4.0",
      display_name: "Seedream 4.0",
      vendor: "openai",
      vendor_name: "OpenAI",
      vendor_color: "#10a37f",
      modality: "image",
      billing_mode: "spec",
      prices: null,
      spec_prices: {
        kind: "per_image",
        tier_price: { "1K": 100_000 },
        max_n: 4,
      },
      is_default: false,
      endpoints: ["/v1/images/generations"],
    },
    {
      id: "tts-1",
      display_name: "TTS-1",
      vendor: "openai",
      vendor_name: "OpenAI",
      vendor_color: "#10a37f",
      modality: "audio",
      billing_mode: "call",
      prices: { call: 40_000 },
      spec_prices: null,
      is_default: false,
      endpoints: ["/v1/audio/speech"],
    },
  ],
  vendors: [
    { id: "openai", name: "OpenAI", color: "#10a37f", priority: 1, sort: 1, builtin: true },
  ],
  group_ratios: { default: 1 },
  scale: 1_000_000,
  currency: "USD",
  currency_symbol: "$",
  cny_per_usd: 7.2,
  other_currency: "CNY",
  other_quota_per_unit: 69444,
};

/** `#root` 铺满 body，任何「多补/漏补一个滚动条宽度」都会直接反映在它身上 */
function rootWidth(page: import("@playwright/test").Page): Promise<number> {
  return page.evaluate(() =>
    Math.round(document.getElementById("root")!.getBoundingClientRect().width),
  );
}

/** 顶栏右边界：顶栏是 sticky 且在 body 内容流里，宽度直接等于 body 内容宽 */
function topRight(page: import("@playwright/test").Page): Promise<number> {
  return page.evaluate(() =>
    Math.round(document.querySelector(".p-top")!.getBoundingClientRect().right),
  );
}

async function openPortalModels(
  page: import("@playwright/test").Page,
  height: number,
): Promise<void> {
  await seedAuth(page);
  await page.route("**/api/pricing", (r) => r.fulfill({ json: envelope(PRICING) }));
  await page.setViewportSize({ width: 1280, height });
  await page.goto("/models");
  await expect(page.locator(".model-card")).toHaveCount(4);
}

test.describe("滚动条", () => {
  test("有滚动条：页面不占位，顶栏铺满，自绘条可拖拽", async ({ page }) => {
    // 矮视口保证一定有滚动（否则测的就不是这条路径）
    await openPortalModels(page, 300);

    const m = await page.evaluate(() => ({
      innerW: window.innerWidth,
      clientW: document.documentElement.clientWidth,
    }));
    // 页面条必须 overlay：占位型滚动条会把这 10px 从可视宽度里扣掉
    expect(m.clientW, "页面滚动条又占布局了").toBe(m.innerW);
    expect(await rootWidth(page)).toBe(1280);
    expect(await topRight(page), "顶栏右侧缺一块（滚动条占位）").toBe(1280);

    // 自绘条登场，且拇指比例合理（内容 1085 高 / 视口 300，拇指应远小于轨道）
    const track = page.locator(".pg-sb");
    await expect(track).toBeVisible();
    const thumbH = await page
      .locator(".pg-sb-thumb")
      .evaluate((el) => Math.round(el.getBoundingClientRect().height));
    expect(thumbH).toBeGreaterThanOrEqual(28);
    expect(thumbH).toBeLessThan(300 / 2);

    // 拖拇指到底：必须真的滚起来（自绘条不是装饰）
    const box = (await page.locator(".pg-sb-thumb").boundingBox())!;
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.mouse.down();
    await page.mouse.move(box.x + box.width / 2, 290, { steps: 8 });
    await page.mouse.up();
    const y = await page.evaluate(() => Math.round(window.scrollY));
    expect(y, "拖动自绘条没有滚动页面").toBeGreaterThan(100);

    // 锁滚动：不占位 → 不需要补内边距，宽度与顶栏右边界都不动
    await page.evaluate(() => window.scrollTo(0, 0));
    await page
      .locator(".model-card", { hasText: "gpt-4o" })
      .getByRole("button", { name: "接入示例" })
      .click();
    await expect(page.getByRole("dialog", { name: /接入示例 · gpt-4o/ })).toBeVisible();
    expect(await page.evaluate(() => document.body.style.paddingRight)).toBe("");
    expect(await rootWidth(page)).toBe(1280);
    expect(await topRight(page)).toBe(1280);
  });

  test("无滚动条：自绘条隐藏，页面照样铺满", async ({ page }) => {
    await openPortalModels(page, 2000);

    await expect(page.locator(".pg-sb")).toBeHidden();
    expect(await rootWidth(page)).toBe(1280);
    expect(await topRight(page), "无滚动条时顶栏右侧出现留白").toBe(1280);

    // 本来就没有滚动条：锁滚动不该凭空补内边距（会把内容压窄）
    await page
      .locator(".model-card", { hasText: "gpt-4o" })
      .getByRole("button", { name: "接入示例" })
      .click();
    await expect(page.getByRole("dialog", { name: /接入示例 · gpt-4o/ })).toBeVisible();
    expect(await page.evaluate(() => document.body.style.paddingRight)).toBe("");
    expect(await topRight(page)).toBe(1280);
  });

  test("管理台顶栏同样铺满（顶栏右边界 = 视口宽）", async ({ page }) => {
    // 门户与管理台是两套 chrome（.p-top / .a-top），但都是 body 流里的横条，
    // 滚动条一占位就同时缺一块。用 clientWidth 做判据：它等于可视宽度。
    await seedAuth(page, { role: 0 });
    await page.setViewportSize({ width: 1280, height: 2000 });
    await page.goto("/admin/dashboard");

    const top = page.locator(".a-top");
    await expect(top).toBeVisible();
    const [right, clientWidth] = await page.evaluate(() => [
      Math.round(document.querySelector(".a-top")!.getBoundingClientRect().right),
      document.documentElement.clientWidth,
    ]);
    expect(right, "管理台顶栏右侧被切掉一条").toBe(clientWidth);
    expect(clientWidth).toBe(1280);
  });
});
