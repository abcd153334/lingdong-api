/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

/**
 * S6 多模态模型广场：图片档位价目、语音转写 multipart 端点、重排/审核端点与模态标签。
 * 数据为与后端 /api/pricing 结构一致的 fixture（tokens / call / spec 三种计费模式各覆盖）。
 */
const base = {
  // 价目只有两种形态：`prices`（tokens/call 的 9 条 lane）或 `spec_prices`；金额一律**微**
  prices: null,
  spec_prices: null,
  is_default: false,
};

const PRICING = {
  models: [
    {
      ...base,
      id: "gpt-image-1",
      display_name: "GPT Image 1",
      vendor: "openai",
      vendor_name: "OpenAI",
      vendor_color: "#10a37f",
      modality: "image",
      billing_mode: "spec",
      spec_prices: {
        kind: "per_image",
        tier_price: { "1K": 12_000, "2K": 24_000, "4K": 48_000 },
        max_n: 10,
      },
      endpoints: ["/v1/images/generations"],
    },
    {
      ...base,
      id: "whisper-1",
      display_name: "Whisper 语音转写",
      vendor: "openai",
      vendor_name: "OpenAI",
      vendor_color: "#10a37f",
      modality: "audio",
      billing_mode: "call",
      prices: { call: 12_000 },
      endpoints: ["/v1/audio/transcriptions"],
    },
    {
      ...base,
      id: "qwen3-reranker",
      display_name: "Qwen3 Reranker",
      vendor: "qwen",
      vendor_name: "通义千问",
      vendor_color: "#615ced",
      modality: "rerank",
      billing_mode: "tokens",
      prices: { input: 100_000 },
      endpoints: ["/v1/rerank"],
    },
    {
      ...base,
      id: "omni-moderation",
      display_name: "Omni Moderation",
      vendor: "openai",
      vendor_name: "OpenAI",
      vendor_color: "#10a37f",
      modality: "moderation",
      billing_mode: "call",
      prices: { call: 0 },
      endpoints: ["/v1/moderations"],
    },
  ],
  vendors: [
    { id: "openai", name: "OpenAI", color: "#10a37f", priority: 1, sort: 1, builtin: true },
    { id: "qwen", name: "通义千问", color: "#615ced", priority: 2, sort: 2, builtin: true },
  ],
  group_ratios: { default: 1 },
  // 币种口径显式声明（fixture 按美元换算），否则回落人民币上下文、断言随环境漂移
  scale: 1_000_000,
  currency: "USD",
  currency_symbol: "$",
  cny_per_usd: 7.2,
  other_currency: "CNY",
  other_quota_per_unit: 69444,
};

async function stubPricing(page: Page): Promise<void> {
  await page.route("**/api/pricing", (route) => route.fulfill({ json: envelope(PRICING) }));
}

const card = (page: Page, id: string) => page.locator(".model-card", { hasText: id });

test.describe("S6 多模态模型广场 · 端点与档位价目", () => {
  test("图片模型：卡片起价 + 抽屉三档档位价目", async ({ page }) => {
    await seedAuth(page);
    await stubPricing(page);

    await page.goto("/models");
    await expect(card(page, "gpt-image-1").locator(".price-rows")).toContainText("$0.012 / 张");

    await card(page, "gpt-image-1").getByRole("button", { name: "完整价目" }).click();
    const drawer = page.getByRole("dialog", { name: /完整价目 · gpt-image-1/ });
    await expect(drawer).toBeVisible();
    for (const tier of ["1K", "2K", "4K"]) {
      await expect(drawer.locator(".price-matrix")).toContainText(tier);
    }
    await drawer.getByRole("button", { name: "关闭" }).click();

    await card(page, "gpt-image-1").getByRole("button", { name: "接入示例" }).click();
    const curl = page.getByRole("dialog", { name: /接入示例 · gpt-image-1/ });
    await expect(curl.locator(".code-block")).toContainText("/v1/images/generations");
  });

  test("语音转写：接入示例为 multipart transcriptions 端点", async ({ page }) => {
    await seedAuth(page);
    await stubPricing(page);

    await page.goto("/models");
    await card(page, "whisper-1").getByRole("button", { name: "接入示例" }).click();
    const drawer = page.getByRole("dialog", { name: /接入示例 · whisper-1/ });
    await expect(drawer.locator(".code-block")).toContainText("/v1/audio/transcriptions");
    await expect(drawer.locator(".code-block")).toContainText("multipart/form-data");
  });

  test("重排 / 审核：curl 端点与按次计费提示", async ({ page }) => {
    await seedAuth(page);
    await stubPricing(page);

    await page.goto("/models");

    await card(page, "qwen3-reranker").getByRole("button", { name: "接入示例" }).click();
    const rerank = page.getByRole("dialog", { name: /接入示例 · qwen3-reranker/ });
    await expect(rerank.locator(".code-block")).toContainText("/v1/rerank");
    await rerank.getByRole("button", { name: "关闭" }).click();

    await expect(card(page, "omni-moderation").locator(".price-rows")).toContainText("按次计费");
    await card(page, "omni-moderation").getByRole("button", { name: "接入示例" }).click();
    const moderation = page.getByRole("dialog", { name: /接入示例 · omni-moderation/ });
    await expect(moderation.locator(".code-block")).toContainText("/v1/moderations");
  });
});
