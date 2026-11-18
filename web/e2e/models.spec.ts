/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

/** GET /api/pricing 快照 fixture（与后端结构一致，覆盖 tokens / call / spec 三种计费模式） */
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
      // 价格一律是「绝对微」：$5.00 / 1M tokens = 5_000_000 微（不再有 ratio × qpc 这层换算）
      prices: { input: 5_000_000, output: 10_000_000, cache: 2_500_000 },
      spec_prices: null,
      is_default: true,
      endpoints: ["/v1/chat/completions"],
    },
    {
      id: "doubao-seedream-4.0",
      display_name: "Seedream 4.0",
      vendor: "doubao",
      vendor_name: "豆包",
      vendor_color: "#325ab4",
      modality: "image",
      billing_mode: "spec",
      prices: null,
      spec_prices: {
        kind: "per_image",
        // 键是 1K/2K/4K；金额是微/张（$0.10 = 100_000 微）
        tier_price: { "1K": 100_000, "2K": 120_000 },
        // 默认档是**档位键**，不是金额（见 docs/计费与结算口径.md §默认档）：
        // 旧字段 default_price 已删除，靠价格反推默认档会让改一次档价就悄悄换档。
        resolution_fixed: "2K",
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
    {
      id: "minimax-h3",
      display_name: "MiniMax H3",
      vendor: "custom",
      vendor_name: "自定义组",
      vendor_color: "#6366f1",
      modality: "video",
      billing_mode: "spec",
      prices: null,
      spec_prices: {
        kind: "per_video",
        second_price: { "480p": 360_000, "720p": 540_000, "1080p": 840_000, "2k": 900_000 },
        ref_image_price: 300_000,
        // 旧「按条」价刻意保留：有每秒价时必须整条不展示（两套口径互斥，都展示等于让用户算错账）
        ref_video_price: 180_000,
        ref_audio_price: 120_000,
        ref_video_second_price: { "720p": 120_000 },
        ref_image_free: 5,
        max_seconds: 30,
        long_after_seconds: 15,
        // 倍率是**微比例**：2 倍 = 2000000
        long_video_multiplier: 2_000_000,
      },
      is_default: false,
      endpoints: ["/v1/video/generations"],
    },
  ],
  vendors: [
    { id: "openai", name: "OpenAI", color: "#10a37f", priority: 1, sort: 1, builtin: true },
    { id: "doubao", name: "豆包", color: "#325ab4", priority: 2, sort: 2, builtin: true },
    { id: "custom", name: "自定义组", color: "#6366f1", priority: 5, sort: 5, builtin: false },
  ],
  group_ratios: { default: 1, vip: 0.9 },
  // 币种口径必须显式声明：fixture 的倍率按美元换算（ratio=2.5 → $5.00/1M），
  // 不声明会回落到默认人民币上下文，断言变成 ¥5.00 且随环境漂移。
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

test.describe("S3 模型广场 · 价目与筛选", () => {
  test("价目按当前计费分组换算（分组取用户生效值，不提供切换）", async ({ page }) => {
    await seedAuth(page);
    await stubPricing(page);

    await page.goto("/models");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("模型广场");
    await expect(page.locator(".model-card")).toHaveCount(4);

    // gpt-4o：输入 $2.5/1M × 2 = $5.00（分组 default ×1）
    await expect(card(page, "gpt-4o").locator(".price-rows")).toContainText("$5 / 1M tokens");

    // 分组不再由用户切换：公开价目端点已裁掉 group_ratios 全表（§4.3.0），
    // 倍率取 me.group_ratio。界面不得出现可切换分组的控件。
    await expect(page.locator(".pbar-fields select")).toHaveCount(0);
  });

  test("卡片标题是模型名称，模型 ID 单独一行且可一键复制", async ({ page, context }) => {
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
    await seedAuth(page);
    await stubPricing(page);

    await page.goto("/models");

    const c = card(page, "gpt-4o");
    // 标题 = 展示名（不是 ID），ID 单独一行
    await expect(c.locator(".m-name .m-title")).toHaveText("GPT-4o");
    await expect(c.locator(".m-id code")).toHaveText("gpt-4o");

    // 门户不得再出现「可用渠道」这类供应链信息
    await expect(page.locator(".models-grid")).not.toContainText("可用渠道");

    // 复制按钮写进剪贴板的是纯 ID
    await c.getByRole("button", { name: "复制模型 ID gpt-4o", exact: true }).click();
    const clip = await page.evaluate(() => navigator.clipboard.readText());
    expect(clip).toBe("gpt-4o");
  });

  test("模态筛选：仅显示图像类模型", async ({ page }) => {
    await seedAuth(page);
    await stubPricing(page);

    await page.goto("/models");
    await page.getByRole("group").getByRole("button", { name: "图像", exact: true }).click();

    await expect(page.locator(".model-card")).toHaveCount(1);
    await expect(card(page, "doubao-seedream-4.0")).toBeVisible();
  });

  test("按规格计费模型：卡片展示起价与默认档单价", async ({ page }) => {
    await seedAuth(page);
    await stubPricing(page);

    await page.goto("/models");
    const image = card(page, "doubao-seedream-4.0").locator(".price-rows");
    // 「起」取最低档（1K）；默认档单独一行，且写明是哪一档 ——
    // 用户按「起」估算、实际被默认档计费时，第二行要能自己解释清楚。
    await expect(image).toContainText("$0.1 / 张");
    await expect(image).toContainText("默认档（未指定尺寸）");
    await expect(image).toContainText("2K · $0.12 / 张");
  });

  test("视频价目：四档（含 2k）/ 参考视频按秒 / 免费张数 / 超时倍率", async ({ page }) => {
    await seedAuth(page);
    await stubPricing(page);

    await page.goto("/models");
    // 卡片起价取四档里最低的 480p
    const rows = card(page, "minimax-h3").locator(".price-rows");
    await expect(rows).toContainText("按秒计费 · 起");
    await expect(rows).toContainText("$0.36 / s");
    await expect(rows).toContainText("480p");

    await card(page, "minimax-h3").getByRole("button", { name: "完整价目" }).click();
    const drawer = page.getByRole("dialog", { name: /完整价目 · minimax-h3/ });
    await expect(drawer).toBeVisible();

    // 2k 是可选档但必须展示：漏掉它会让用户以为最大只有 1080p
    const secTable = drawer.locator(".price-matrix").first();
    await expect(secTable).toContainText("2k");
    await expect(secTable).toContainText("$0.9 / s");

    // 配了每秒价 → 展示按秒表，旧「按条」价（$0.1800）一个字都不能出现
    // 用 columnheader 精确定位：同一句「参考视频每秒单价」也出现在底部公式说明里
    await expect(drawer.getByRole("columnheader", { name: "参考视频每秒单价" })).toBeVisible();
    await expect(drawer).toContainText("参考视频：$0.12 / 秒");
    await expect(drawer).not.toContainText("$0.18");

    // 免费张数与超时倍率必须写进说明，否则用户按公式自算永远多算
    await expect(drawer).toContainText("参考图前 5 张免费");
    // 倍率按两位数展示（2 倍 → ×2.00）
    await expect(drawer).toContainText("时长 > 15 秒时，时长费 ×2.00");
    await expect(drawer).toContainText("参考视频每秒单价[分辨率] × 视频秒数");
  });

  test("详情抽屉：接入示例 curl", async ({ page }) => {
    await seedAuth(page);
    await stubPricing(page);

    await page.goto("/models");
    await card(page, "gpt-4o").getByRole("button", { name: "接入示例" }).click();

    const drawer = page.getByRole("dialog", { name: /接入示例 · gpt-4o/ });
    await expect(drawer).toBeVisible();
    await expect(drawer.locator(".code-block")).toContainText(
      "curl -X POST https://api.example.com/v1/chat/completions",
    );

    await drawer.getByRole("button", { name: "关闭" }).click();
    await expect(drawer).toBeHidden();
  });
});
