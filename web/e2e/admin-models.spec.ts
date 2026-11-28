/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, isAdminPageRequest, seedAuth } from "./helpers";

/** S20 去假覆盖：/admin/models（契约级 stub —— 全部 /api/admin/** 打桩，断言真实区块/行/请求契约） */

const ICON_OPENAI = "https://cdn.test/api/icons/b/openai-gpt?v=abc";
/** 夹具图标所在域名（假域名，需要单独接管，否则 <img> 必然 onError 回落首字） */
const ICON_HOST = "https://cdn.test/**";

const VENDORS = [
  // icon 与 seed 一致：出厂内置分组都带图标，「清除自己的图标 → 回落到供应商」才有画面可验。
  // `icon` 是编辑回填用的令牌，`icon_url` 才是渲染用的地址（前端不拼路径）。
  {
    id: "openai",
    name: "OpenAI",
    color: "#10a37f",
    icon: "builtin:openai-gpt",
    icon_url: ICON_OPENAI,
    priority: 1,
    sort: 1,
    builtin: true,
    model_count: 1,
  },
  {
    id: "custom",
    name: "自定义组",
    color: "#6366f1",
    icon: "",
    icon_url: "",
    priority: 5,
    sort: 5,
    builtin: false,
    model_count: 1,
  },
];

const GPT4O = {
  id: "gpt-4o",
  display_name: "GPT-4o",
  vendor: "openai",
  vendor_name: "OpenAI",
  vendor_color: "#10a37f",
  modality: "chat",
  billing_mode: "tokens",
  prices: { input: 3_000_000, output: 15_000_000 },
  model_ratio: 1.25,
  completion_ratio: null,
  cache_ratio: null,
  create_cache_ratio: null,
  create_cache_ratio_1h: null,
  image_ratio: null,
  audio_ratio: null,
  audio_completion_ratio: null,
  model_price_quota: null,
  spec_prices: null,
  enabled: true,
  is_default: true,
  endpoints: null,
  bindings: 2,
  version: 1,
  input_price: 3,
  output_price: 15,
  cache_price: null,
  cache_create_price: null,
  image_price: null,
  audio_price: null,
  audio_output_price: null,
  call_price: null,
};

const FLUX = {
  ...GPT4O,
  id: "flux-img",
  display_name: "",
  vendor: "custom",
  vendor_name: "自定义组",
  vendor_color: "#6366f1",
  modality: "image",
  billing_mode: "spec",
  model_ratio: null,
  // 默认档刻意留空：它是「不传 size 时按哪一档计费」，与兜底价是两个概念
  //（docs/计费与结算口径.md：金额只在 tier_price 里，默认档只回答「按哪一档」）。
  spec_prices: { kind: "per_image", tier_price: { "2K": 3000 }, max_n: 4 },
  enabled: false,
  is_default: false,
  bindings: 0,
  input_price: null,
  output_price: null,
};

/**
 * 视频模型价目夹具：四档秒价（含 2k）+ 旧的「参考视频按条」价 + 一个参考视频**每秒**价。
 *
 * 旧按条价是刻意保留的：它用来验证「一旦填了每秒价，保存时按条价不再下发」的互斥规则。
 */
const METASO_H3 = {
  ...GPT4O,
  id: "metaso-h3",
  display_name: "Metaso H3",
  vendor: "custom",
  vendor_name: "自定义组",
  vendor_color: "#6366f1",
  modality: "video",
  billing_mode: "spec",
  model_ratio: null,
  spec_prices: {
    kind: "per_video",
    second_price: { "480p": 18000, "720p": 27000, "1080p": 42000, "2k": 45000 },
    ref_image_price: 15000,
    ref_video_price: 9000,
    ref_audio_price: 6000,
    ref_video_second_price: { "720p": 6000 },
    ref_image_free: 5,
    max_seconds: 30,
    long_after_seconds: 15,
    long_video_multiplier: 2_000_000,
  },
  enabled: true,
  is_default: false,
  bindings: 1,
  input_price: null,
  output_price: null,
};

/**
 * 固定档位视频模型夹具（可灵式）：上游没有 resolution 参数，画质由型号决定。
 *
 * `resolution_fixed` 是 `buildSpec` 白名单重建里最容易漏的键：漏了就是「只改个显示名
 * → 该模型所有不传 resolution 的请求在 task 层直接 400」。
 */
const KLING_STD = {
  ...GPT4O,
  id: "kling-v3-std",
  display_name: "可灵 3.0 标准",
  vendor: "custom",
  vendor_name: "自定义组",
  vendor_color: "#6366f1",
  modality: "video",
  billing_mode: "spec",
  model_ratio: null,
  spec_prices: {
    kind: "per_video",
    second_price: { "480p": 9600, "720p": 12000, "1080p": 18000 },
    resolution_fixed: "720p",
    max_seconds: 10,
  },
  enabled: false,
  is_default: false,
  bindings: 1,
  input_price: null,
  output_price: null,
};

/** 左栏与渠道条用的那一条渠道（右栏按 `?c=1` 落到它）。 */
const CHANNEL = {
  id: 1,
  name: "OpenAI 官方",
  icon: "",
  icon_url: "",
  adapter_id: "openai",
  base_url: "https://api.openai.com",
  multi_key_mode: "polling",
  switch_key_retry: false,
  groups: ["default"],
  weight: 10,
  priority: 1,
  rpm_limit: 500,
  max_concurrency: 50,
  inflight: 0,
  inflight_limit: 50,
  status: 1,
  auto_ban: true,
  balance_cny: null,
  key_count: 2,
  key_enabled_count: 2,
  version: 1,
  health: { success_count: 0, fail_count: 0, fail_rate: 0, last_error: "", last_test_at: null },
  adapter_config: {},
  adapter_options: null,
  bound_models: [],
};

/** 记录已发生的请求；管理台必须在 /api/admin/** 打桩（否则真实 401 触发全局登出） */
async function stub(page: Page, captures: { url: string; body: unknown }[] = []): Promise<string[]> {
  const calls: string[] = [];
  upstreamSearches.length = 0;
  // 可灵图标存档：PUT 存进来、下一次 GET 原样吐回去。
  // 没有它就测不到「清除」——清除按钮只在**已有图标**时出现。
  let klingIcon = "";
  await page.route(
    (url) => isAdminPageRequest(url),
    (route) => {
      const req = route.request();
      const u = new URL(req.url());
      calls.push(`${req.method()} ${u.pathname}${u.search}`);

      if (u.pathname === "/api/admin/models" && req.method() === "GET") {
        return route.fulfill({
          json: envelope({
            // icon 是派生值（自己的 or 供应商的），icon_own 才是模型自己的 ——
            // 表单回填读的是 icon_own，少了它「重开弹窗」就退回「沿用供应商」。
            items: [
              GPT4O,
              FLUX,
              METASO_H3,
              {
                ...KLING_STD,
                icon: klingIcon,
                icon_own: klingIcon,
                icon_url: klingIcon ? ICON_OPENAI : "",
              },
            ],
            vendors: VENDORS,
            tag_palette: [],
          }),
        });
      }
      // 左栏（适配器 → 渠道级联）与渠道条：合并页首屏就会拉这两个。
      if (u.pathname === "/api/admin/adapters" && req.method() === "GET") {
        return route.fulfill({
          json: envelope({
            items: [
              {
                id: "openai",
                name: "openai",
                label: "OpenAI",
                vendor: "openai",
                caps: ["text"],
                status: 1,
                source: "builtin",
                editable: false,
                deletable: false,
                channel_count: 1,
                endpoints: [],
                supports_declarative: true,
                supports_options: false,
                default_base_url: "https://api.openai.com",
              },
            ],
          }),
        });
      }
      if (u.pathname === "/api/admin/channels" && req.method() === "GET") {
        return route.fulfill({
          json: envelope({
            items: [CHANNEL],
            adapters: [],
            groups: ["default"],
            total: 1,
            monitor: { auto_disable: true, fail_threshold: 0.5, fail_window: 10, auto_enable: true, probe_interval_min: 5 },
          }),
        });
      }
      if (u.pathname === "/api/admin/models/kling-v3-std" && req.method() === "PUT") {
        const body = req.postDataJSON() as { icon?: string };
        captures.push({ url: u.pathname, body });
        klingIcon = body.icon ?? "";
        return route.fulfill({
          json: envelope({
            item: { ...KLING_STD, icon: klingIcon, icon_own: klingIcon, icon_url: klingIcon ? ICON_OPENAI : "" },
          }),
        });
      }
      if (u.pathname === "/api/admin/models/flux-img" && req.method() === "PUT") {
        captures.push({ url: u.pathname, body: req.postDataJSON() });
        return route.fulfill({ json: envelope({ item: FLUX }) });
      }
      if (u.pathname === "/api/admin/models/metaso-h3" && req.method() === "PUT") {
        captures.push({ url: u.pathname, body: req.postDataJSON() });
        return route.fulfill({ json: envelope({ item: METASO_H3 }) });
      }
      if (u.pathname === "/api/admin/vendors" && req.method() === "GET") {
        return route.fulfill({ json: envelope({ items: VENDORS }) });
      }
      // 图标库（IconPicker 展开时才请求，见 IconLibrary 的 useAdminIconsQuery）
      if (u.pathname === "/api/admin/icons" && req.method() === "GET") {
        return route.fulfill({
          json: envelope({
            builtin: [
              {
                key: "openai-gpt",
                name: "OpenAI",
                group: "llm",
                aliases: ["openai"],
                // 图库项自带地址：渲染不再依赖前端拼 /api/icons/...
                url: ICON_OPENAI,
              },
            ],
            uploaded: [],
          }),
        });
      }
      if (u.pathname === "/api/admin/models/gpt-4o" && req.method() === "PATCH") {
        captures.push({ url: u.pathname, body: req.postDataJSON() });
        return route.fulfill({ json: envelope({ enabled: false }) });
      }
      // 模型保存走 PUT（带 version 乐观锁）。没有这条分支时保存会落到兜底
      // envelope(null)，"填入后保存"就断不了请求体。
      if (u.pathname === "/api/admin/models/gpt-4o" && req.method() === "PUT") {
        captures.push({ url: u.pathname, body: req.postDataJSON() });
        return route.fulfill({ json: envelope({ item: GPT4O }) });
      }
      // 绑定只有**单条**写入口：PUT/DELETE /models/:id/bindings/:channelId
      if (/^\/api\/admin\/models\/gpt-4o\/bindings\/\d+$/.test(u.pathname)) {
        captures.push({ url: u.pathname, body: req.postDataJSON() });
        return route.fulfill({ json: envelope({ bindings: 1, warnings: [] }) });
      }
      // 上游参考价：只读查询。**站点币种是 USD**（helpers 注入 currency:"USD"），
      // 所以 *_micro 必须与美元同值（$2.5 → 2_500_000 微）；写死成人民币折算值
      // 会把契约锁在一个"USD 站点按 7.2 折算"的假前提上。
      if (u.pathname === "/api/admin/upstream/search") {
        const kw = u.searchParams.get("keyword") ?? "";
        upstreamSearches.push({ keyword: kw, vendor: u.searchParams.get("vendor") ?? "" });
        return route.fulfill({
          json: envelope({
            items: [
              {
                id: "openai/gpt-4o",
                name: "gpt-4o",
                provider_id: "openai",
                provider_name: "OpenAI",
                modality: "chat",
                deprecated: false,
                provider_count: 3,
                input_usd: 2.5,
                output_usd: 10,
                cache_usd: null,
                input_micro: 2_500_000,
                output_micro: 10_000_000,
                cache_micro: null,
              },
            ],
            meta: {
              source: "models.dev",
              fetched_at: "2026-11-26T02:00:00Z",
              stale: false,
              currency: "USD",
              fx_rate: 1,
              matched_keyword: kw,
              total: 1,
              with_price: 1,
            },
          }),
        });
      }
      return route.fulfill({ json: envelope(null) });
    },
  );
  return calls;
}

/** 合并页：用 `?c=` 落到一条渠道，右栏就是它的模型（没有独立的模型列表页了）。 */
/** 记录本用例里打过的上游参考价请求（关键词/供应商偏好）。 */
const upstreamSearches: { keyword: string; vendor: string }[] = [];

async function openModels(page: Page) {
  await page.goto("/admin/model-service?c=1");
  await expect(page.locator(".ms-chanbar b")).toHaveText("OpenAI 官方");
  await expect(page.locator(".tbl tbody tr").first()).toBeVisible();
}

test.describe("S20 模型管理 · 管理台（契约级）", () => {
  test("列表渲染：工具条 + 模型行的价目概要 / 供应商 / 本渠道", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);
    await openModels(page);

    // 没有页头标题行（页面四边贴边）：h1 只给读屏，视觉上是左栏 + 右栏
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("模型服务");
    // 渠道条：当前渠道的身份 + 四个渠道动作
    const bar = page.locator(".ms-chanbar");
    await expect(bar).toContainText("OpenAI 官方");
    await expect(bar).toContainText("https://api.openai.com");
    for (const name of ["编辑渠道", "测试连通", "停用渠道", "删除渠道"]) {
      await expect(bar.getByRole("button", { name })).toBeVisible();
    }
    // 工具条只有右栏有搜索与筛选（左栏刻意不做）
    await expect(page.locator(".pbar-fields input")).toHaveCount(1);
    await expect(page.locator(".pbar-fields .esel")).toHaveCount(1);
    for (const name of ["新建模型", "绑定已有模型", "供应商"]) {
      await expect(page.locator(".pbar .acts").getByRole("button", { name })).toBeVisible();
    }
    // 旧的「上游同步」入口已删除：上游参考价现在只从模型编辑弹窗底栏进入，
    // 留在工具条上等于同一件事两个入口（且那个入口根本不知道你在编辑哪个模型）。
    await expect(page.getByRole("button", { name: "上游同步" })).toHaveCount(0);

    const gptRow = page.locator(".tbl tbody tr", { hasText: "gpt-4o" });
    await expect(gptRow).toContainText("GPT-4o");
    await expect(gptRow).toContainText("OpenAI");
    await expect(gptRow).toContainText("入 $3 · 出 $15");
    // 本渠道列：未绑定 → 「—」；绑定后显示上游名与权重
    await expect(gptRow).toContainText("—");

    const fluxRow = page.locator(".tbl tbody tr", { hasText: "flux-img" });
    await expect(fluxRow).toContainText("图片按张");
  });

  test("启停开关：PATCH enabled=false 并提示", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const captures: { url: string; body: unknown }[] = [];
    await stub(page, captures);
    await openModels(page);

    await page.locator(".tbl tbody tr", { hasText: "gpt-4o" }).getByRole("button", { name: "停用模型" }).click();
    // 渠道视图里的停用是**全局**的：先确认"所有渠道一起下线"，
    // 否则它会被理解成"只关本渠道"（关本渠道要用「绑定设置」里的绑定启停）。
    const scope = page.getByRole("dialog", { name: "停用模型（全部渠道）" });
    await expect(scope).toBeVisible();
    await expect(scope).toContainText("所有渠道");
    await scope.getByRole("button", { name: "全部渠道停用" }).click();
    await expect
      .poll(() => captures.some((c) => c.url === "/api/admin/models/gpt-4o" && c.body?.enabled === false))
      .toBe(true);
    await expect(page.getByText("已禁用 gpt-4o")).toBeVisible();
  });

  test("绑定：在渠道内给模型配置上游名与权重 → 单条 PUT 契约", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const captures: { url: string; body: unknown }[] = [];
    await stub(page, captures);
    await openModels(page);

    const row = page.locator(".tbl tbody tr", { hasText: "gpt-4o" });
    await row.getByRole("button", { name: "绑定到本渠道" }).click();
    const dialog = page.getByRole("dialog", { name: "绑定 · gpt-4o" });
    await expect(dialog).toBeVisible();

    await dialog.locator("#bd-upstream").fill("gpt-4o-2024");
    await dialog.locator("#bd-weight").fill("3");
    await dialog.getByRole("button", { name: "绑定" }).click();

    await expect
      .poll(() => captures.some((c) => c.url === "/api/admin/models/gpt-4o/bindings/1"))
      .toBe(true);
    const body = captures.find((c) => c.url === "/api/admin/models/gpt-4o/bindings/1")?.body as {
      upstream_model: string;
      weight: number;
      priority: number;
      enabled: boolean;
    };
    // 单条绑定：只写这一条渠道，其他渠道的行不受影响（整组替换会压平它们的权重/分层）
    expect(body.upstream_model).toBe("gpt-4o-2024");
    expect(body.weight).toBe(3);
    expect(body.priority).toBe(1);
    expect(body.enabled).toBe(true);
    await expect(page.getByText("已绑定到本渠道")).toBeVisible();
  });

  test("新建模型：在渠道里建 → 同一请求带上 initial_binding（建完就在右栏）", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const captures: { url: string; body: unknown }[] = [];
    await stub(page, captures);
    await openModels(page);

    await page.getByRole("button", { name: "新建模型" }).click();
    const dialog = page.getByRole("dialog", { name: "新建模型" });
    await dialog.locator("#mf-id").fill("m-new");
    // 按 token 计费必须填输入价，否则保存被前端拦下（根本不会发请求）
    await dialog.locator("#mf-p-input").fill("1");
    await dialog.locator("#mf-weight").fill("2");

    // 绑定与模型在**同一个事务**里创建：否则建完还要回列表找它再绑一次，
    // 而那份列表是按渠道过滤的 —— 新模型根本不在里面。
    const reqPromise = page.waitForRequest(
      (r) => r.url().endsWith("/api/admin/models") && r.method() === "POST",
    );
    await dialog.getByRole("button", { name: "保存" }).click();
    const body = (await reqPromise).postDataJSON() as {
      id: string;
      initial_binding?: { channel_id: number; upstream_model: string; weight: number; enabled: boolean };
    };
    expect(body.id).toBe("m-new");
    // 上游名留空 ⇒ 前端补成模型 ID（后端不猜，空值直接 400）
    expect(body.initial_binding).toEqual({
      channel_id: 1,
      upstream_model: "m-new",
      weight: 2,
      enabled: true,
    });
  });

  test("新建模型：空模型 ID 被拦截且不发请求", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page);
    await openModels(page);

    await page.getByRole("button", { name: "新建模型" }).click();
    const dialog = page.getByRole("dialog", { name: "新建模型" });
    await expect(dialog).toBeVisible();
    await dialog.getByRole("button", { name: "保存" }).click();

    await expect(page.getByText("模型 ID 必填")).toBeVisible();
    expect(calls.some((c) => c.startsWith("POST /api/admin/models"))).toBe(false);
  });

  test("上游参考价：打开即按当前模型的上游名查询，只读不写库", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const captures: { url: string; body: unknown }[] = [];
    const calls = await stub(page, captures);
    await openModels(page);

    await page
      .locator(".tbl tbody tr", { hasText: "gpt-4o" })
      .getByRole("button", { name: "编辑模型" })
      .click();
    const form = page.getByRole("dialog", { name: "编辑模型 gpt-4o" });
    await expect(form).toBeVisible();

    await form.getByRole("button", { name: "上游参考价" }).click();
    // 内层是 portal 到 body 的二级弹窗：只能用 page 级定位，套 form.locator 会找不到
    const panel = page.getByRole("dialog", { name: "上游参考价" });
    await expect(panel).toBeVisible();

    // 自动搜索用的是当前模型名，不需要用户再敲一遍
    await expect(panel.locator("#up-kw")).toHaveValue("gpt-4o");
    await expect.poll(() => upstreamSearches.length).toBeGreaterThan(0);
    expect(upstreamSearches[0]).toEqual({ keyword: "gpt-4o", vendor: "openai" });

    // 双口径 + 口径声明：站点是 USD，所以只显示美元价，不出现汇率行
    const row = panel.locator(".tbl tbody tr", { hasText: "gpt-4o" });
    await expect(row).toContainText("$2.5");
    await expect(panel).toContainText("不是你这条渠道的结算价");
    await expect(panel).not.toContainText("1 美元 ≈");

    // 查询是只读的：打开面板不能产生任何模型写请求
    expect(captures.some((c) => c.url.startsWith("/api/admin/models"))).toBe(false);
    expect(calls.some((c) => c.startsWith("GET /api/admin/upstream/search"))).toBe(true);
  });

  test("上游参考价：单项填入只改表单，保存时才带着新价提交", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const captures: { url: string; body: unknown }[] = [];
    await stub(page, captures);
    await openModels(page);

    await page
      .locator(".tbl tbody tr", { hasText: "gpt-4o" })
      .getByRole("button", { name: "编辑模型" })
      .click();
    const form = page.getByRole("dialog", { name: "编辑模型 gpt-4o" });
    await form.getByRole("button", { name: "上游参考价" }).click();

    const panel = page.getByRole("dialog", { name: "上游参考价" });
    const row = panel.locator(".tbl tbody tr", { hasText: "gpt-4o" });
    // 输入价与输出价有报价 → 各一个「填入」；缓存价上游没报 → 没有按钮
    await expect(row.getByRole("button", { name: "填入" })).toHaveCount(2);
    await row.getByRole("button", { name: "填入" }).first().click();

    // 填入后仍是表单草稿：输入框已是新价，但还没有任何 PUT
    await expect(form.locator("#mf-p-input")).toHaveValue("2.5");
    expect(captures.some((c) => c.url === "/api/admin/models/gpt-4o")).toBe(false);

    await panel.getByRole("button", { name: "关闭" }).click();
    await form.getByRole("button", { name: "保存" }).click();

    await expect
      .poll(() => captures.filter((c) => c.url === "/api/admin/models/gpt-4o").length)
      .toBe(1);
    const saved = captures.find((c) => c.url === "/api/admin/models/gpt-4o")!.body as {
      input_price?: number;
      version?: number;
    };
    // 量纲：表单里是"2.5"，提交必须是 2_500_000 微（少乘 1e6 = 全站计费归零）
    expect(saved.input_price).toBe(2_500_000);
    expect(typeof saved.version).toBe("number");
  });

  test("上游参考价：按规格计费的模型只比价、不给填入", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);
    await openModels(page);

    await page
      .locator(".tbl tbody tr", { hasText: "flux-img" })
      .getByRole("button", { name: "编辑模型" })
      .click();
    const form = page.getByRole("dialog", { name: "编辑模型 flux-img" });
    await form.getByRole("button", { name: "上游参考价" }).click();

    const panel = page.getByRole("dialog", { name: "上游参考价" });
    await expect(panel).toBeVisible();
    await expect(panel.locator(".tbl tbody tr", { hasText: "gpt-4o" })).toBeVisible();
    // 把 token 价塞进「按张」字段是量纲事故，所以整列填入都不渲染
    await expect(panel.getByRole("button", { name: "填入" })).toHaveCount(0);
    await expect(panel).toContainText("参考价仅用于比价");
  });

  test("上游参考价：新建模型无名字时不发请求，手输后才查", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page);
    await openModels(page);

    await page.getByRole("button", { name: "新建模型" }).click();
    const form = page.getByRole("dialog", { name: "新建模型" });
    await form.getByRole("button", { name: "上游参考价" }).click();

    const panel = page.getByRole("dialog", { name: "上游参考价" });
    await expect(panel.locator("#up-kw")).toHaveValue("");
    await expect(panel).toContainText("输入模型名查询参考价");
    // 空关键词不该打后端（会白拉一次 7MB 快照）
    expect(calls.some((c) => c.startsWith("GET /api/admin/upstream/search"))).toBe(false);

    await panel.locator("#up-kw").fill("claude");
    await panel.locator("#up-kw").press("Enter");
    await expect.poll(() => upstreamSearches.some((s) => s.keyword === "claude")).toBe(true);
  });

  test("上游参考价：Esc 只关面板，不关掉背后的模型表单", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);
    await openModels(page);

    await page
      .locator(".tbl tbody tr", { hasText: "gpt-4o" })
      .getByRole("button", { name: "编辑模型" })
      .click();
    const form = page.getByRole("dialog", { name: "编辑模型 gpt-4o" });
    await form.getByRole("button", { name: "上游参考价" }).click();
    await expect(page.getByRole("dialog", { name: "上游参考价" })).toHaveClass(/open/);

    await page.keyboard.press("Escape");
    // 弹层栈只有栈顶响应 Esc：内层关掉，外层表单还在（草稿不能丢）
    await expect(page.getByRole("dialog", { name: "上游参考价" })).not.toHaveClass(/open/);
    await expect(form).toHaveClass(/open/);
  });

  test("上游参考价：375px 底栏入口独占一行，不压字不溢出", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);
    await page.setViewportSize({ width: 375, height: 780 });
    await openModels(page);

    await page
      .locator(".tbl tbody tr", { hasText: "gpt-4o" })
      .getByRole("button", { name: "编辑模型" })
      .click();
    const form = page.getByRole("dialog", { name: "编辑模型 gpt-4o" });
    await expect(form.getByRole("button", { name: "上游参考价" })).toBeVisible();

    // 4 个按钮等分 335px 每个只有 76px，而「上游参考价」需约 99px —— 所以入口必须独占一行
    const geom = await form.locator(".dl-foot").evaluate((el) => {
      const start = el.querySelector(".dl-foot__start") as HTMLElement;
      const foot = el as HTMLElement;
      return {
        overflow: foot.scrollWidth - foot.clientWidth,
        startW: Math.round(start.getBoundingClientRect().width),
        footW: Math.round(foot.getBoundingClientRect().width),
        padL: parseFloat(getComputedStyle(foot).paddingLeft),
        padR: parseFloat(getComputedStyle(foot).paddingRight),
      };
    });
    expect(geom.overflow).toBeLessThanOrEqual(0);
    expect(geom.startW).toBeGreaterThanOrEqual(geom.footW - geom.padL - geom.padR - 1);
  });

  test("图片档位：目录拉不到时五个档位不缺档，未设默认档就地提示", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page, []);
    await openModels(page);

    await page.locator(".tbl tbody tr", { hasText: "flux-img" }).getByRole("button", { name: "编辑模型" }).click();
    const dialog = page.getByRole("dialog", { name: "编辑模型 flux-img" });
    await expect(dialog).toBeVisible();

    // 目录接口返回 null（断网/降级）→ 回落本地常量。少一档 = 运营在表单里根本
    // 看不到那一行，结论会变成「这个档位不支持」，而不是「前端漏了个值」。
    // 只取「带单价输入框」的行：表格末尾还有一行「自定义档位」（输入框在 th 里）
    // 和一行「默认档」（select），混进来会把档位数量断言变成数行数。
    const tierRows = dialog.locator(".price-matrix tbody tr").filter({ has: page.locator("td input") });
    // 五档：1.5K 是正式档位（官方与 1K 同价、效果更优），不是 1K 的别名。
    await expect(tierRows.locator("th")).toHaveText(["1K", "1.5K", "2K", "3K", "4K"]);

    // 默认档没配时必须当场说清后果：运行期不再替用户挑档，
    // 「不传 size」的请求会直接被拒绝（这是运营唯一能看到这件事的地方）。
    await expect(dialog.getByLabel("默认档")).toHaveValue("");
    const hint = dialog.locator(".hint.warn");
    await expect(hint).toContainText("未设置默认档");
    await expect(hint).toContainText("会被拒绝");
  });

  /**
   * 默认档 =「不传 size / auto 时按哪一档计费」，**必须命中自己的价目**（保存期硬错）。
   *
   * 这条曾经靠「兜底价恰好等于某个档位单价」反推默认档 —— 改一次档价就会悄悄换档，
   * 账单上看不出来。现在默认档是独立字段，所以页面必须在运营点保存之前就说它错了：
   * 提示与拦截是同一个判据（`buildSpecPrices`），不是两套。
   */
  test("图片档位：默认档不在价目里必须当场提示且拦住保存", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const captures: { url: string; body: unknown }[] = [];
    await stub(page, captures);
    // 价目只有 1K / 2K，默认档却指着 8K（历史遗留或手填错）
    await page.route(
      (url) => new URL(url).pathname === "/api/admin/models",
      (route) =>
        route.fulfill({
          json: envelope({
            items: [
              {
                ...FLUX,
                spec_prices: {
                  kind: "per_image",
                  tier_price: { "1K": 3000, "2K": 4500 },
                  resolution_fixed: "8K",
                  max_n: 4,
                },
              },
            ],
            vendors: VENDORS,
            total: 1,
          }),
        }),
    );

    await openModels(page);
    await page.locator(".tbl tbody tr", { hasText: "flux-img" }).getByRole("button", { name: "编辑模型" }).click();
    const dialog = page.getByRole("dialog", { name: "编辑模型 flux-img" });
    await expect(dialog).toBeVisible();

    // 就地告警：写在价目下方，不依赖运营去点保存才发现
    await expect(dialog.locator(".hint.warn")).toContainText("默认档 8K 已不在价目里");

    const before = captures.length;
    await dialog.getByRole("button", { name: "保存" }).click();
    // 拦住而不是把坏数据发出去：后端会 400，但那时运营已经离开价目区、只能看到一句红字
    await expect(page.getByText(/默认档「8K」不在已配置的档位里/)).toBeVisible();
    expect(captures.slice(before).some((c) => c.url.includes("/api/admin/models/flux-img"))).toBe(false);
  });

  test("图片档位：价目全部填 0 必须二次确认后才提交", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const captures: { url: string; body: unknown }[] = [];
    await stub(page, captures);
    await openModels(page);

    await page.locator(".tbl tbody tr", { hasText: "flux-img" }).getByRole("button", { name: "编辑模型" }).click();
    const dialog = page.getByRole("dialog", { name: "编辑模型 flux-img" });
    await expect(dialog).toBeVisible();

    // 5 个档位单价全部清零：这是「把这个模型白送出去」的那一次点击。
    // 只取 `td input` —— 表格里还有「自定义档位」（input 在 th 里）
    // 和「默认档」select，按 tr 遍历会命中没有 input 的行而超时。
    const priceInputs = dialog.locator(".price-matrix tbody tr td input");
    await expect(priceInputs).toHaveCount(5);
    const count = await priceInputs.count();
    for (let i = 0; i < count; i += 1) {
      await priceInputs.nth(i).fill("0");
    }
    await dialog.getByRole("button", { name: "保存" }).click();

    // 0 不是「未配置」，而是明确的免费：没有这道确认，一次手滑就是永久免费
    const confirm = page.getByRole("dialog", { name: "确认 0 价" });
    await expect(confirm).toBeVisible();
    await expect(confirm).toContainText("完全免费");
    await confirm.getByRole("button", { name: "确认为免费" }).click();

    await expect.poll(() => captures.some((c) => c.url === "/api/admin/models/flux-img")).toBe(true);
    const body = captures.find((c) => c.url === "/api/admin/models/flux-img")?.body as {
      spec_prices: { tier_price: Record<string, number>; resolution_fixed?: string };
    };
    // 确认之后按原样提交：0 是合法值，前端不得替运营「修正」成别的数
    expect(body.spec_prices.tier_price).toEqual({ "1K": 0, "1.5K": 0, "2K": 0, "3K": 0, "4K": 0 });
    // 没有「兜底价」这个字段了：金额只在 tier_price 里，默认档没选就不下发
    //（下发空串会让后端把「按哪一档」和「多少钱」又缠在一起）
    expect(body.spec_prices.resolution_fixed).toBeUndefined();
  });

  test("视频价目：2k 档与参考视频每秒价原样带回，按条价不再下发", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const captures: { url: string; body: unknown }[] = [];
    await stub(page, captures);
    await openModels(page);

    const row = page.locator(".tbl tbody tr", { hasText: "metaso-h3" });
    await expect(row).toContainText("视频按秒");
    await row.getByRole("button", { name: "编辑模型" }).click();

    const dialog = page.getByRole("dialog", { name: "编辑模型 metaso-h3" });
    await expect(dialog).toBeVisible();

    // 回填必须看到库里的真实值（漏列任何一个 = 保存一次就丢一个配置）
    await expect(dialog.locator("#mf-freeimg")).toHaveValue("5");
    await expect(dialog.locator("#mf-maxsec")).toHaveValue("30");
    await expect(dialog.locator("#mf-longafter")).toHaveValue("15");
    await expect(dialog.locator("#mf-longmult")).toHaveValue("2");
    // 已有按秒价 → 按条价字段被禁用：两条公式互斥，可编辑就会「看起来配了、实际没生效」
    await expect(dialog.locator("#mf-refvid")).toBeDisabled();

    await dialog.locator("#mf-refsec-1080p").fill("0.009");
    await dialog.getByRole("button", { name: "保存" }).click();

    await expect.poll(() => captures.some((c) => c.url === "/api/admin/models/metaso-h3")).toBe(true);
    const body = captures.find((c) => c.url === "/api/admin/models/metaso-h3")?.body as {
      spec_prices: Record<string, unknown>;
    };
    const spec = body.spec_prices;
    // 2k 必须原样带回：只回写三档会让「只改个显示名」也静默删掉 2K 价
    expect(spec.second_price).toEqual({
      "480p": 18000,
      "720p": 27000,
      "1080p": 42000,
      "2k": 45000,
    });
    expect(spec.ref_video_second_price).toEqual({ "720p": 6000, "1080p": 9000 });
    expect("ref_video_price" in spec).toBe(false);
    expect(spec.ref_image_free).toBe(5);
    expect(spec.max_seconds).toBe(30);
    expect(spec.long_after_seconds).toBe(15);
    // 倍率存**微比例**：2 倍 = 2000000（表单按倍数填，提交时换算）
    expect(spec.long_video_multiplier).toBe(2_000_000);
  });

  test("固定档位：只改显示名也必须把 resolution_fixed 原样带回", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const captures: { url: string; body: unknown }[] = [];
    await stub(page, captures);
    await openModels(page);

    const row = page.locator(".tbl tbody tr", { hasText: "kling-v3-std" });
    await row.getByRole("button", { name: "编辑模型" }).click();

    const dialog = page.getByRole("dialog", { name: "编辑模型 kling-v3-std" });
    await expect(dialog).toBeVisible();
    // 回填：库里配了固定档就必须能看到，否则运营会以为「没配」
    await expect(dialog.locator("#mf-fixedtier")).toHaveValue("720p");

    // 只改显示名——最容易「顺手丢配置」的路径
    await dialog.locator("#mf-name").fill("可灵 3.0 标准（改名）");
    await dialog.getByRole("button", { name: "保存" }).click();

    await expect.poll(() => captures.some((c) => c.url === "/api/admin/models/kling-v3-std")).toBe(true);
    const body = captures.find((c) => c.url === "/api/admin/models/kling-v3-std")?.body as {
      spec_prices: Record<string, unknown>;
    };
    const spec = body.spec_prices;
    // buildSpec 是白名单重建：漏了这一个键 = 该模型所有不传 resolution 的请求 400
    expect(spec.resolution_fixed).toBe("720p");
    expect(spec.second_price).toEqual({ "480p": 9600, "720p": 12000, "1080p": 18000 });
    expect(spec.max_seconds).toBe(10);
  });

  test("固定档位：选了一个没有单价的档位就地拦下，不发请求", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const captures: { url: string; body: unknown }[] = [];
    await stub(page, captures);
    await openModels(page);

    await page.locator(".tbl tbody tr", { hasText: "kling-v3-std" })
      .getByRole("button", { name: "编辑" })
      .click();
    const dialog = page.getByRole("dialog", { name: "编辑模型 kling-v3-std" });

    // 2k 档没有配单价：固定到它会让结算找不到单价（冻结成功、结算每轮失败）
    await dialog.locator("#mf-fixedtier").selectOption("2k");
    await dialog.getByRole("button", { name: "保存" }).click();

    await expect(page.getByText(/固定档位 2k 没有配置每秒单价/)).toBeVisible();
    expect(captures.some((c) => c.url === "/api/admin/models/kling-v3-std")).toBe(false);
  });

  /**
   * 图标全链路（选择 → 保存契约 → 清除）。
   *
   * 只验「界面上有个图库」是不够的：真正的坑是**提交时丢键**（`icon` 恒提交
   * 为空串就等于每次保存都把图标清掉），以及「清除」走了 `undefined`
   * （后端语义是"不修改"，于是永远清不掉）。所以两次保存都要看请求体。
   */
  test("图标：选中内置图标随保存提交，清除后提交空串", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const captures: { url: string; body: unknown }[] = [];
    await stub(page, captures);
    // 夹具里的图标地址是 `https://cdn.test/...`（假域名），真实浏览器会把请求发出去、
    // 拿回 DNS 失败 → `<img onError>` 按契约回落首字，于是「有没有出图」永远测不出来。
    // 这里把该域名接管成一张 1x1 透明图：断言看的才是"组件渲染了 img"，不是"网络通不通"。
    await page.route(ICON_HOST, (route) =>
      route.fulfill({
        status: 200,
        contentType: "image/gif",
        body: Buffer.from("R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7", "base64"),
      }),
    );
    await openModels(page);

    await page.locator(".tbl tbody tr", { hasText: "kling-v3-std" })
      .getByRole("button", { name: "编辑" })
      .click();
    const dialog = page.getByRole("dialog", { name: "编辑模型 kling-v3-std" });

    // 触发器是**一块可点的图标砖**，来源说明在 title 上（名字与图标同一行，放不下小字）。
    // 未设置且供应商也没图标 → 「使用首字」；不是笼统的"已设置/未设置"。
    const trigger = dialog.getByRole("button", { name: "更换图标" });
    await expect(trigger).toHaveAttribute("title", "使用首字");

    await trigger.click();
    // 面板 portal 到 body（.dialog 带 transform，就地定位会被二次缩放/裁剪），
    // 所以**不能**用 dialog.locator 去找它。
    const panel = page.locator(".ipop");
    await expect(panel).toBeVisible();
    await panel.getByRole("button", { name: "OpenAI" }).click();
    await expect(trigger).toHaveAttribute("title", "内置图标");
    // 预览必须真出图：这一帧用的是图库回包里的 `url`，该令牌还没被任何实体投影过。
    // 断言 `src` 就等于服务端给的地址（前端不许再拼 /api/icons/...）。
    await expect(trigger.locator("img")).toHaveAttribute("src", ICON_OPENAI);

    await dialog.getByRole("button", { name: "保存" }).click();
    await expect.poll(() => captures.length).toBeGreaterThan(0);
    const first = captures.find((c) => c.url === "/api/admin/models/kling-v3-std")?.body as {
      icon?: string;
    };
    expect(first.icon).toBe("builtin:openai-gpt");

    // 重新打开：图标从列表回填（这一步顺带验证「保存后 invalidate 让新值回到界面」）
    await expect(dialog).toBeHidden();
    await page.locator(".tbl tbody tr", { hasText: "kling-v3-std" })
      .getByRole("button", { name: "编辑" })
      .click();
    await expect(trigger).toHaveAttribute("title", "内置图标");

    // 清除只有一条路径：面板底部的「使用首字」（提交空串）。
    // 提交 undefined 会被后端当成"不修改"，图标永远清不掉。
    captures.length = 0;
    await trigger.click();
    await page.locator(".ipop").getByRole("button", { name: "使用首字" }).click();
    // 可灵挂在「自定义组」上，而这个组夹具没有图标 → 清掉自己的图标后是「使用首字」。
    // （挂 OpenAI 的模型这里会是「沿用供应商」—— 文案本来就该随回落来源变。）
    await expect(trigger).toHaveAttribute("title", "使用首字");
    await dialog.getByRole("button", { name: "保存" }).click();
    await expect.poll(() => captures.length).toBeGreaterThan(0);
    const second = captures.find((c) => c.url === "/api/admin/models/kling-v3-std")?.body as {
      icon?: string;
    };
    expect(second.icon).toBe("");
  });
});
