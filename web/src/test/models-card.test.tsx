import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { ModelsPage } from "@/pages/portal/Models";
import type { PricingSnapshot } from "@/types";

/**
 * 模型广场卡片契约：
 *  - 标题是**模型名称**（display_name），不是 ID；
 *  - ID 单独一行，并且有复制按钮；
 *  - 不得出现「可用渠道」这类供应链信息。
 *
 * 这里把定价查询整块打桩：卡片是纯展示，不需要真实请求。
 */
const PRICING: PricingSnapshot = {
  models: [
    {
      id: "gpt-image-2-vip",
      display_name: "GPT Image 2 高速版",
      vendor: "openai",
      vendor_name: "OpenAI",
      vendor_color: "#10a37f",
      icon_url: "https://api.example.com/api/icons/b/openai-gpt?v=abc",
      modality: "image",
      billing_mode: "spec",
      // 按张计费不走 lane 价目，价格全在 spec_prices 里（单位微）
      prices: {},
      spec_prices: {
        kind: "per_image",
        tier_price: { "1K": 6000, "2K": 12000, "4K": 24000 },
        resolution_fixed: "4K",
        max_n: 1,
      },
      is_default: false,
      endpoints: ["images/generations"],
      is_free: false,
      tags: [
        { label: "限时免费", color_hex: "#E11D48" },
        { label: "夜间折扣", color_hex: "#0EA5E9" },
        { label: "订阅优先", color_hex: "#7C3AED" },
      ],
      created_at: 1700000000,
    },
  ],
  vendors: [
    {
      id: "openai",
      name: "OpenAI",
      color: "#10a37f",
      icon_url: "https://api.example.com/api/icons/b/openai-gpt?v=abc",
    },
  ],
  defaults: { image: "gpt-image-2-vip" },
  scale: 1_000_000,
  currency: "USD",
  currency_symbol: "$",
  cny_per_usd: 7.2,
  other_currency: "CNY",
};

vi.mock("@/lib/api", () => ({
  usePricingQuery: () => ({
    data: PRICING,
    isLoading: false,
    isError: false,
    error: null,
    refetch: () => undefined,
  }),
  useMeQuery: () => ({ data: { group: "default", group_discount: 1_000_000 } }),
}));

vi.mock("@/lib/site", () => ({
  useSite: () => ({ siteName: "灵动 API", apiBaseUrl: "https://api.example.com" }),
}));

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ModelsPage />
    </QueryClientProvider>,
  );
}

describe("模型广场卡片", () => {
  const writeText = vi.fn().mockResolvedValue(undefined);

  beforeEach(() => {
    writeText.mockClear();
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText },
    });
  });

  it("标题渲染模型名称，ID 单独一行", () => {
    const { container } = renderPage();
    const cardEl = container.querySelector(".model-card");
    expect(cardEl).toBeTruthy();
    expect(cardEl!.querySelector(".m-name .m-title")?.textContent).toBe("GPT Image 2 高速版");
    expect(cardEl!.querySelector(".m-id code")?.textContent).toBe("gpt-image-2-vip");
  });

  it("复制按钮把纯模型 ID 写入剪贴板", async () => {
    renderPage();
    // 用 fireEvent 而不是 userEvent：userEvent.setup() 会用自己的剪贴板桩覆盖
    // navigator.clipboard，导致断言对象被替换（不是组件没调用）。
    fireEvent.click(
      screen.getByRole("button", { name: /^复制模型 ID gpt-image-2-vip$/ }),
    );
    await Promise.resolve();
    expect(writeText).toHaveBeenCalledWith("gpt-image-2-vip");
  });

  it("卡片与筛选条都不出现「可用渠道」与供应商模型数", () => {
    const { container } = renderPage();
    expect(container.textContent).not.toContain("可用渠道");
    const chips = container.querySelector(".model-card");
    expect(chips?.textContent ?? "").not.toMatch(/OpenAI · GPT Image 2 高速版/);
    // 供应商胶囊只显示名称与色点，不带数字
    const bar = container.querySelector(".pbar");
    expect(bar?.textContent ?? "").not.toMatch(/OpenAI\d/);
  });

  it("标签渲染在徽章行并按 max 折叠，不挤进名称行", () => {
    const { container } = renderPage();
    const cardEl = container.querySelector(".model-card")!;
    // 名称行只放图标与标题：标签塞进去会把 ellipsis 的模型名压成两三个字
    expect(cardEl.querySelector(".m-name .model-tag")).toBeNull();
    const chips = cardEl.querySelector(".chips")!;
    const tags = chips.querySelectorAll(".model-tag");
    expect(tags).toHaveLength(2); // max=2
    expect(tags[0].textContent).toBe("限时免费");
    expect(chips.querySelector(".model-tag-list__more")?.textContent).toBe("+1");
    // 渲染色来自 color_hex，不是前端色表
    expect((tags[0] as HTMLElement).style.getPropertyValue("--model-tag-fg")).toBe("#E11D48");
  });
});
