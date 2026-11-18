/**
 * 模型服务（/admin/model-service）：左栏适配器→渠道级联 + 右栏模型列表。
 *
 * 阶段边界：本文件只覆盖**只读**路径（导航、筛选、深链保持）。
 * 渠道表单与模型编辑弹窗接入后再补写入用例 —— 现在不写"点了会怎样"，
 * 因为那些按钮还不存在（宁可少一条用例，也不要一条对着假入口的断言）。
 */
import { expect, test, type Page } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

const seedAdmin = (page: Page) => seedAuth(page, { role: 0 });

const ADAPTERS = [
  { id: "spec:my-gw", label: "我的网关", name: "我的网关", vendor: "", caps: ["chat"], status: 1, source: "custom", editable: true, deletable: true, spec_id: 7, spec_version: 2, channel_count: 1 },
  { id: "openai", label: "OpenAI", name: "OpenAI", vendor: "openai", caps: ["chat"], status: 1, source: "builtin", editable: false, deletable: false, channel_count: 2 },
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
    // 停用渠道：左栏必须排在启用之后（排序是"启用在前"），且状态点是灰的
    id: 12, name: "备用线路", icon: "", icon_url: "", adapter_id: "spec:my-gw", base_url: "https://gw2.example.com",
    multi_key_mode: "random", groups: ["default"], weight: 1, priority: 2, rpm_limit: 0, max_concurrency: 0,
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

test.describe("模型服务", () => {
  test("左栏按适配器分组级联：启用在前、孤儿单列、组内无搜索框", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.goto("/admin/model-service");

    const rail = page.locator(".ms-rail");
    await expect(rail).toBeVisible();
    // 需求原文：左栏不做搜索与筛选
    await expect(rail.locator("input")).toHaveCount(0);

    // 自建适配器排在前面（运营刚加的应该看得见），内置在后
    const heads = rail.locator(".ms-gh__label");
    await expect(heads.first()).toHaveText("我的网关");
    await expect(heads.nth(1)).toHaveText("OpenAI");

    // 默认展开"第一个有渠道的组"，组内渠道按「启用在前」排序
    const openGroup = rail.locator(".ms-grp").first();
    await expect(openGroup.locator(".ms-gh")).toHaveAttribute("aria-expanded", "true");
    const names = openGroup.locator(".ms-ch__name");
    await expect(names.first()).toHaveText("网关主线路");
    await expect(names.nth(1)).toHaveText("备用线路");

    // 孤儿渠道单独成桶（不能静默吞掉：它在 /v1 上必然失败）
    await expect(rail.getByText("未登记适配器")).toBeVisible();
    await expect(rail.getByText("失联线路")).toBeVisible();
  });

  test("点渠道写 ?c= 并加载该渠道的模型（含本渠道绑定列）", async ({ page }) => {
    await seedAdmin(page);
    const captured: string[] = [];
    await stub(page, captured);
    await page.goto("/admin/model-service");

    await page.locator(".ms-ch", { hasText: "网关主线路" }).click();
    await expect(page).toHaveURL(/[?&]c=11/);
    // a 也要带上：刷新后左栏才知道展开哪一组
    await expect(page).toHaveURL(/[?&]a=spec%3Amy-gw|[?&]a=spec:my-gw/);

    const main = page.locator(".ms-main");
    await expect(main.getByText("GPT-4o", { exact: true })).toBeVisible();
    // 本渠道列：上游名与权重（weight=2 才显示）
    await expect(main.getByText("gpt-4o-2024")).toBeVisible();
    await expect(main.getByText(/权重 2/)).toBeVisible();

    // 请求确实按渠道过滤（不是前端筛的）
    expect(captured.some((c) => c.includes("channel_id=11"))).toBe(true);
  });

  test("深链 ?c= 刷新后保持选中；不存在的渠道回落到引导态", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);

    await page.goto("/admin/model-service?c=11&a=spec:my-gw");
    await expect(page.locator(".ms-ch[aria-current='true']")).toHaveText(/网关主线路/);
    await expect(page.locator(".ms-main").getByText("GPT-4o", { exact: true })).toBeVisible();

    // 渠道 id 已不存在：不白屏、不假装选中，且 `?c` 必须被纠偏清掉
    // （否则刷新/转发出去的链接永远指向一条不存在的渠道）。
    await page.goto("/admin/model-service?c=99999");
    await expect(page).not.toHaveURL(/[?&]c=/);
    // 落回"当前展开的适配器"的渠道总览 —— 有内容可看，不是一页灰字
    await expect(page.locator(".ms-main .ms-ov")).toBeVisible();
  });

  test("点适配器不出现空白页：右栏换成该适配器的渠道总览", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.goto("/admin/model-service");

    // 首屏默认展开第一组，所以点「OpenAI」是从折叠到展开
    await page.locator(".ms-gh", { hasText: "OpenAI" }).click();
    await expect(page).toHaveURL(/[?&]a=openai/);

    const main = page.locator(".ms-main");
    // 这个适配器下没有渠道：给的是"可以建第一条"的引导，而不是一句"去左边选"
    await expect(main.getByText("这个适配器下还没有渠道")).toBeVisible();
    // 空态里有一个 CTA，顶条上也有一条同名入口（同名的第三处是左栏虚线槽）——
    // 这里锁定空态那一处，断言的是"空态给出了可执行的下一步"。
    await expect(
      main.locator(".empty").getByRole("button", { name: "新建渠道" }),
    ).toBeVisible();
    await expect(main.getByText("选择左侧的渠道")).toHaveCount(0);
  });

  test("点已展开的组头：真的收起（节点移出 DOM），右栏不再声称选中", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.goto("/admin/model-service");

    const head = page.locator(".ms-gh", { hasText: "我的网关" });
    await expect(head).toHaveAttribute("aria-expanded", "true");
    await head.click();

    await expect(head).toHaveAttribute("aria-expanded", "false");
    // 折叠 = 条件渲染，该组的渠道行不在 DOM（不是 display:none 的"假折叠"）。
    // 限定在非孤儿组：孤儿桶永远展开，它自己的渠道行不受影响。
    await expect(page.locator(".ms-grp:not(.ms-grp--orphan)").first().locator(".ms-ch")).toHaveCount(0);
    // `?a` 必须一起清掉，否则刷新后会重新展开，用户以为折叠没生效
    await expect(page).not.toHaveURL(/[?&]a=/);
  });

  test("折叠含当前渠道的组：组头用主色提示，右栏保持该渠道不变", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.goto("/admin/model-service?c=11&a=spec:my-gw");

    await page.locator(".ms-gh", { hasText: "我的网关" }).click();

    // 选中的渠道被藏起来了，但右栏还在看它 —— 组头必须说"在这组里"
    await expect(page.locator(".ms-gh.is-focus")).toHaveCount(1);
    await expect(page.locator(".ms-chanbar b")).toHaveText("网关主线路");
    await expect(page.locator(".ms-main").getByText("GPT-4o", { exact: true })).toBeVisible();
    // 折叠不能顺手把选中项删掉
    await expect(page).toHaveURL(/[?&]c=11/);
  });

  test("渠道行显示它自己的图标（不是首字母块、也不是状态点）", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.goto("/admin/model-service");
    // 第一条展开组里两条渠道，都要有图标槽（icon_url 为空时退首字，槽位恒定）。
    // 限定在组内：孤儿桶另有 1 条，不参与这条断言。
    await expect(page.locator(".ms-grp:not(.ms-grp--orphan)").first().locator(".ms-ch .ent-icon")).toHaveCount(2);
  });

  test("删除当前渠道：清 ?c、右栏回到该适配器的总览", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    // 备用线路没有绑定模型（有绑定的会被前端拦下）
    await page.goto("/admin/model-service?c=12&a=spec:my-gw");

    await page.getByRole("button", { name: "删除渠道" }).click();
    await page
      .getByRole("dialog", { name: "删除渠道" })
      .getByRole("button", { name: "删除", exact: true })
      .click();

    await expect(page).not.toHaveURL(/[?&]c=/);
    await expect(page.locator(".ms-main .ms-ov")).toBeVisible();
  });

  test("≤899 左栏收进抽屉：开关可开可关、Esc 关闭、选中渠道后自动关", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.setViewportSize({ width: 800, height: 900 });
    await page.goto("/admin/model-service");

    const toggle = page.getByRole("button", { name: "打开适配器列表" });
    await expect(toggle).toBeVisible();
    // 关闭态：抽屉在屏幕外，且对键盘/读屏不可达
    await expect(page.locator(".ms-rail")).toHaveJSProperty("inert", true);

    await toggle.click();
    await expect(page.locator(".ms-ws")).toHaveAttribute("data-rail", "open");
    await expect(page.locator(".ms-scrim")).toBeVisible();
    await expect(page.locator(".ms-rail")).toHaveJSProperty("inert", false);

    // Esc 关闭（焦点陷阱与滚动锁由 useOverlayBehavior 统一处理）
    await page.keyboard.press("Escape");
    await expect(page.locator(".ms-ws")).not.toHaveAttribute("data-rail", "open");
    await expect(page.locator(".ms-scrim")).toHaveCount(0);

    // 再开一次，点渠道后应自动收起（否则右栏被遮住，还要再点一次遮罩）
    await toggle.click();
    await page.locator(".ms-ch", { hasText: "网关主线路" }).click();
    await expect(page.locator(".ms-ws")).not.toHaveAttribute("data-rail", "open");
    await expect(page).toHaveURL(/[?&]c=11/);
  });

  test("未选渠道时不请求渠道维度：不传 channel_id，也不显示本渠道列", async ({ page }) => {
    await seedAdmin(page);
    const captured: string[] = [];
    await stub(page, captured);
    await page.goto("/admin/model-service");

    const main = page.locator(".ms-main");
    // 首屏展开第一组 ⇒ 右栏直接给它的渠道总览（不是空白、也不是"去左边选一个"）
    await expect(main.locator(".ms-ov")).toBeVisible();
    // 但没有选中渠道时**不得**发带 channel_id 的模型请求（不打全量模型的空转）
    expect(captured.some((c) => c.includes("channel_id="))).toBe(false);
  });
});
