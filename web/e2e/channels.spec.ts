/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

/**
 * 适配器能力视图 fixture。
 *
 * `source` / `label` / `channel_count` 是契约字段：适配器页按 source 分「内置/我的」
 * 两组、按 channel_count 显示引用数。桩件漏了它们，页面就是两块空白，
 * 用例还会「通过」——那是假绿。
 */
const ADAPTERS = [
  {
    id: "openai",
    name: "openai",
    label: "OpenAI",
    vendor: "openai",
    caps: ["text", "embedding", "passthrough"],
    status: 1,
    source: "builtin",
    editable: false,
    deletable: false,
    channel_count: 1,
    endpoints: [],
    supports_declarative: true,
  },
  // 只有声明 supports_options 的适配器才渲染渠道私有配置块（AutoDL 工作流映射）
  {
    id: "autodl",
    name: "autodl",
    label: "AutoDL",
    vendor: "autodl",
    caps: ["video"],
    source: "builtin",
    editable: false,
    deletable: false,
    channel_count: 0,
    endpoints: [],
    status: 1,
    supports_declarative: false,
    supports_options: true,
  },
  // 第二个私有配置形状：用于验证「按 adapter_id 分流」，两份表单不得互相串写
  {
    id: "metaso",
    name: "metaso",
    label: "Metaso",
    vendor: "metaso",
    caps: ["video"],
    source: "builtin",
    editable: false,
    deletable: false,
    channel_count: 0,
    endpoints: [],
    status: 1,
    supports_declarative: false,
    supports_options: true,
  },
  // 第三套形状（多米）：鉴权前缀 / 路径 / 透传白名单 / 模型覆盖，键集与上面两套完全不重叠
  {
    id: "duomiapi",
    name: "duomiapi",
    label: "多米API",
    vendor: "duomiapi",
    caps: ["video", "image", "edit_image"],
    source: "builtin",
    editable: false,
    deletable: false,
    channel_count: 0,
    endpoints: [],
    status: 1,
    supports_declarative: false,
    supports_options: true,
  },
];

/** 多米渠道 fixture：无前缀鉴权 + 自定义 kling 前缀 + 收窄的白名单 + 一条模型覆盖 */
const DUOMI_CHANNEL = {
  id: 12,
  name: "多米API 视频",
  adapter_id: "duomiapi",
  base_url: "https://duomiapi.com",
  multi_key_mode: "random",
  groups: ["default"],
  weight: 5,
  priority: 1,
  rpm_limit: 0,
  max_concurrency: 0,
  status: 2,
  auto_ban: true,
  balance_cny: null,
  key_count: 1,
  health: { fail_rate: 0, success_count: 0, fail_count: 0 },
  adapter_config: {},
  adapter_options: {
    auth_prefix: "-",
    paths: { "kling.prefix": "/api/video/kling/v2/videos/" },
    metadata_allow: ["aigc_watermark", "generate_audio"],
    model_overrides: { "seedance-lite": { protocol: "seedance", upstream_model: "doubao-seedance-lite" } },
  },
  bound_models: [],
};

/** Metaso 渠道 fixture：标准/优先通道由渠道开关决定，四档映射含一档未开通 */
const METASO_CHANNEL = {
  id: 9,
  name: "Metaso 优先通道",
  adapter_id: "metaso",
  base_url: "https://metaso.cn",
  multi_key_mode: "random",
  groups: ["default"],
  weight: 5,
  priority: 1,
  rpm_limit: 0,
  max_concurrency: 0,
  status: 1,
  auto_ban: true,
  balance_cny: null,
  key_count: 1,
  health: { fail_rate: 0, success_count: 0, fail_count: 0 },
  adapter_config: {},
  adapter_options: {
    fast_mode: true,
    context_ir_enabled: false,
    resolution_map: { "480p": "480P", "720p": "768P", "1080p": "", "2k": "2K" },
    query_path: "/api/minimax/v3/query/video_generation/",
  },
  bound_models: [],
};

/** AutoDL 渠道 fixture：adapter_options 已配置（用于验证编辑回填与 PUT 契约） */
const AUTODL_CHANNEL = {
  id: 7,
  name: "AutoDL 官方",
  adapter_id: "autodl",
  base_url: "https://autodl.art",
  multi_key_mode: "random",
  groups: ["default"],
  weight: 5,
  priority: 1,
  rpm_limit: 0,
  max_concurrency: 0,
  status: 2,
  auto_ban: true,
  balance_cny: null,
  key_count: 1,
  health: { fail_rate: 0, success_count: 0, fail_count: 0 },
  adapter_config: {},
  adapter_options: {
    workflows: {
      multimodal: "wf_mm",
      image: "wf_img",
      text: "wf_txt",
      first_last: "wf_fl",
    },
    resolution_map: { "480p": "480p", "720p": "768p", "1080p": "" },
  },
  bound_models: [],
};

/** 渠道列表 fixture（覆盖启用 / 自动禁用、有绑定 / 无绑定） */
const CHANNELS = [
  {
    id: 1,
    name: "OpenAI 官方",
    adapter_id: "openai",
    base_url: "https://api.openai.com",
    // 官网：渠道条外链与编辑抽屉的跳转按钮都读这两个字段
    website: "https://openai.com",
    website_url: "https://openai.com",
    multi_key_mode: "polling",
    groups: ["default", "vip"],
    weight: 10,
    priority: 1,
    rpm_limit: 500,
    max_concurrency: 50,
    status: 1,
    auto_ban: true,
    balance_cny: 12580.5,
    key_count: 2,
    key_enabled_count: 2,
    health: { fail_rate: 0.02, success_count: 12, fail_count: 1, last_test_at: "2026-10-19T08:00:00Z" },
    // 滑窗 8 样本 / 5 失败（62.5%）但样本不足 10 → 不熔断，渠道仍启用（P2-22 的解释场景）
    window: { total: 8, fails: 5, fail_rate: 0.625 },
    // 声明式配置：用于验证编辑回填与「不改动时原样提交」
    adapter_config: {
      auth_header: "x-api-key",
      auth_prefix: "none",
      path_overrides: { "/v1/chat/completions": "/v4/chat" },
    },
    bound_models: [{ model: "gpt-4o", alias: "", weight: 10 }],
  },
  {
    id: 2,
    name: "火山方舟",
    adapter_id: "builtin:doubao",
    base_url: "https://ark.cn-beijing.volces.com",
    multi_key_mode: "random",
    groups: ["default"],
    weight: 8,
    priority: 1,
    rpm_limit: 300,
    max_concurrency: 20,
    status: 3,
    auto_ban: true,
    balance_cny: 4320,
    key_count: 1,
    health: { fail_rate: 0.4, success_count: 6, fail_count: 4, last_test_at: "2026-10-19T07:00:00Z" },
    // 滑窗 20 样本 / 12 失败（60% > 50% 且样本足够）→ 已自动禁用，与 status=3 自洽
    window: { total: 20, fails: 12, fail_rate: 0.6 },
    adapter_config: {},
    bound_models: [],
  },
];

/** 密钥池明细 fixture：一把正常 + 一把带别名且有冷却（覆盖徽标分支） */
const KEY_ITEMS = [
  {
    index: 0,
    key_id: "kid-1",
    fingerprint: "sk-aaa…1111",
    alias: "",
    enabled: true,
    decrypt_ok: true,
    cooldown_reason: "",
    cooldown_remaining_sec: 0,
    last_error_status: 0,
    last_error_at: "",
  },
  {
    index: 1,
    key_id: "kid-2",
    fingerprint: "sk-bbb…2222",
    alias: "备用",
    enabled: true,
    decrypt_ok: true,
    cooldown_reason: "auth",
    cooldown_remaining_sec: 42,
    last_error_status: 401,
    last_error_at: "2026-11-11T08:00:00Z",
  },
];

interface StubOptions {
  /** 连通测试结果：默认成功 */
  testOk?: boolean;
  /** 覆盖列表数据（默认 CHANNELS）；用于只关心单个渠道的场景，避免改动共享 fixture */
  channels?: unknown[];
}

/** 统一挂载 /api/admin/** 桩件，返回已发生的请求记录 */
async function stubAdmin(page: Page, opts: StubOptions = {}): Promise<string[]> {
  const calls: string[] = [];
  await page.route(
    (url) => url.pathname.startsWith("/api/admin/"),
    (route) => {
      const { pathname, search } = new URL(route.request().url());
      const method = route.request().method();
      // 带上 query：分页/排序/筛选都是「服务端契约」，必须断言真实下发的参数
      calls.push(`${method} ${pathname}${search}`);

      if (pathname === "/api/admin/channels" && method === "GET") {
        return route.fulfill({
          json: envelope({
            items: opts.channels ?? CHANNELS,
            adapters: ADAPTERS,
            groups: ["default", "vip"],
            total: 42,
            monitor: { auto_disable: true, fail_threshold: 0.5, fail_window: 10, auto_enable: true, probe_interval_min: 5 },
          }),
        });
      }
      if (pathname === "/api/admin/channels" && method === "POST") {
        return route.fulfill({ json: envelope({ item: CHANNELS[0] }) });
      }
      // 密钥池：读明细 + 原子写。桩件必须分开处理，
      // 否则面板会拿到 envelope(null) 而渲染成"没有密钥"，用例假绿。
      if (/^\/api\/admin\/channels\/\d+\/keys$/.test(pathname) && method === "GET") {
        return route.fulfill({
          json: envelope({
            version: 1,
            items: KEY_ITEMS,
            key_count: 2,
            broken_count: 0,
            key_enabled_count: 2,
          }),
        });
      }
      if (/^\/api\/admin\/channels\/\d+\/keys$/.test(pathname) && method === "PATCH") {
        return route.fulfill({
          json: envelope({
            version: 2,
            items: KEY_ITEMS,
            key_count: 5,
            broken_count: 0,
            key_enabled_count: 5,
          }),
        });
      }
      if (/^\/api\/admin\/channels\/\d+$/.test(pathname) && method === "PUT") {
        return route.fulfill({ json: envelope({ item: CHANNELS[0] }) });
      }
      if (/^\/api\/admin\/channels\/\d+$/.test(pathname) && method === "DELETE") {
        return route.fulfill({ json: envelope(null) });
      }
      if (pathname === "/api/admin/channel/test") {
        const result = opts.testOk === false
          ? { ok: false, message: "上游返回 401", latency_ms: 12, key_index: -1, key_count: 2 }
          : { ok: true, message: "连通正常（密钥 #1）", latency_ms: 42, key_index: 0, key_count: 2 };
        return route.fulfill({ json: envelope({ result }) });
      }
      if (pathname === "/api/admin/channel/status") {
        return route.fulfill({ json: envelope({ item: CHANNELS[1] }) });
      }
      // 右栏模型列表：合并页一进来就会拉（`?c=` 选中渠道后带 channel_id）。
      // 渠道用例不关心模型，给空列表 + 供应商/色板即可，缺了它会整块报错。
      if (pathname === "/api/admin/models" && method === "GET") {
        return route.fulfill({
          json: envelope({ items: [], total: 0, vendors: [], tag_palette: [] }),
        });
      }
      if (pathname === "/api/admin/adapters") {
        return route.fulfill({ json: envelope({ items: ADAPTERS }) });
      }
      return route.fulfill({ json: envelope(null) });
    },
  );
  return calls;
}

/**
 * 打开某条渠道。
 *
 * 合并页没有"渠道列表"了：左栏是导航，右栏是模型。用 `?c=<id>` 直接落到该渠道，
 * 等价于运营点左栏那一下 —— 顺带钉死深链可用（刷新/发给同事都要能用）。
 */
async function openChannel(page: Page, name = "OpenAI 官方") {
  // 按名字在**全部** fixture 里找 id：私有配置那几条用例只桩了一条渠道
  // （AUTODL_CHANNEL / METASO_CHANNEL），不在默认 CHANNELS 里。
  const ch =
    [...CHANNELS, DUOMI_CHANNEL, METASO_CHANNEL, AUTODL_CHANNEL].find((c) => c.name === name) ??
    DUOMI_CHANNEL;
  await page.goto(`/admin/model-service?c=${ch.id}`);
  await expect(page.locator(".ms-chanbar b")).toHaveText(name);
}
/**
 * 「新建渠道」的唯一入口在适配器浮层底部（左栏里不再有虚线槽）。
 *
 * `?c=` 深链下，右栏正在看的那条渠道所属的适配器卡片带 `aria-current="true"`，
 * 据此定位 —— 不需要知道 fixture 里 adapter_id 与展示名的映射。
 */
async function openNewChannel(page: Page) {
  await page.locator('.ms-adp[aria-current="true"]').first().click();
  await page.getByRole("button", { name: "新建渠道", exact: true }).click();
}

test.describe("S4 渠道与适配器 · 管理台", () => {

  test("连通测试成功：展示命中密钥与耗时", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stubAdmin(page);

    await openChannel(page);
    await page.getByRole("button", { name: "测试连通" }).click();

    await expect(page.getByText(/连通正常（密钥 #1）/)).toBeVisible();
    expect(calls).toContain("POST /api/admin/channel/test");
  });

  test("连通测试失败：展示上游错误原因", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page, { testOk: false });

    await openChannel(page);
    await page.getByRole("button", { name: "测试连通" }).click();

    await expect(page.getByText(/连通失败：上游返回 401/)).toBeVisible();
  });

  test("编辑渠道：改名称走 PUT 并提示已更新", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stubAdmin(page);

    await openChannel(page, "OpenAI 官方");
    await page.getByRole("button", { name: "编辑渠道" }).click();

    const drawer = page.getByRole("dialog", { name: /编辑渠道 · OpenAI 官方/ });
    await expect(drawer).toBeVisible();
    await drawer.locator("#chz-name").fill("OpenAI 官方（改）");
    await drawer.getByRole("button", { name: "保存" }).click();

    await expect(page.getByText("渠道已更新")).toBeVisible();
    expect(calls).toContain("PUT /api/admin/channels/1");
  });

  test("官网：渠道条与 Base URL 旁都有跳转入口，且可编辑/清空", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);
    await openChannel(page, "OpenAI 官方");

    // 渠道条：不打开抽屉也能跳
    const barLink = page.locator(".ms-chanbar__site");
    await expect(barLink).toHaveAttribute("href", "https://openai.com");
    await expect(barLink).toHaveAttribute("target", "_blank");
    await expect(barLink).toHaveAttribute("rel", /noopener/);

    await page.getByRole("button", { name: "编辑渠道" }).click();
    const drawer = page.getByRole("dialog", { name: /编辑渠道 · OpenAI 官方/ });
    await expect(drawer.locator("#chz-site")).toHaveValue("https://openai.com");

    // Base URL 右侧的外链按钮：与输入框同一行（.field-row），且是新窗口打开
    const go = drawer.locator(".field-row__go");
    await expect(go).toHaveAttribute("href", "https://openai.com");
    await expect(go).toHaveAttribute("target", "_blank");
    await expect(go).toHaveAttribute("rel", "noopener noreferrer");

    // 清空 = 显式提交空串（省略会变成"保持不变"，官网就永远删不掉）
    await drawer.locator("#chz-site").fill("");
    const req = page.waitForRequest(
      (r) => r.url().endsWith("/api/admin/channels/1") && r.method() === "PUT",
    );
    await drawer.getByRole("button", { name: "保存" }).click();
    const body = (await req).postDataJSON() as { website?: string };
    expect(body.website).toBe("");
  });

  test("删除有绑定模型的渠道被前置拦截", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stubAdmin(page);

    await openChannel(page, "OpenAI 官方");
    await page.getByRole("button", { name: "删除渠道" }).click();

    await expect(page.getByText(/仍绑定 1 个模型/)).toBeVisible();
    expect(calls).not.toContain("DELETE /api/admin/channels/1");
  });

  test("新建渠道：填写密钥池走 POST 并提示已创建", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stubAdmin(page);

    await openChannel(page);
    await openNewChannel(page);

    const drawer = page.getByRole("dialog", { name: "新建渠道" });
    await drawer.locator("#chz-name").fill("新上游");
    await drawer.locator("#chz-url").fill("https://api.example.com");
    await drawer.locator("#chz-keys").fill("sk-aaa\nsk-bbb");
    await drawer.getByRole("button", { name: "保存" }).click();

    await expect(page.getByText("渠道已创建")).toBeVisible();
    expect(calls).toContain("POST /api/admin/channels");
  });

  test("声明式配置：不填高级配置时请求体不含该字段", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);

    await openChannel(page);
    await openNewChannel(page);
    const drawer = page.getByRole("dialog", { name: "新建渠道" });
    await drawer.locator("#chz-name").fill("纯标准上游");
    await drawer.locator("#chz-url").fill("https://api.example.com");
    await drawer.locator("#chz-keys").fill("sk-aaa");

    const reqPromise = page.waitForRequest(
      (r) => r.url().includes("/api/admin/channels") && r.method() === "POST",
    );
    await drawer.getByRole("button", { name: "保存" }).click();
    const body = (await reqPromise).postDataJSON() as Record<string, unknown>;
    // 空配置时不带该字段：保证与改造前的请求体逐字节一致（旧后端也能收）。
    expect("adapter_config" in body).toBe(false);
  });

  test("声明式配置：新建时随 POST 提交请求改写", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);

    await openChannel(page);
    await openNewChannel(page);
    const drawer = page.getByRole("dialog", { name: "新建渠道" });
    await drawer.locator("#chz-name").fill("声明式上游");
    await drawer.locator("#chz-url").fill("https://api.example.com");
    await drawer.locator("#chz-keys").fill("sk-aaa");
    await drawer.locator("#chz-patch").fill('{"thinking":{"type":"enabled"}}');

    const reqPromise = page.waitForRequest(
      (r) => r.url().includes("/api/admin/channels") && r.method() === "POST",
    );
    await drawer.getByRole("button", { name: "保存" }).click();
    const body = (await reqPromise).postDataJSON() as Record<string, any>;
    expect(body.adapter_config.request_patch).toEqual({ thinking: { type: "enabled" } });
  });

  test("声明式配置：非法 JSON 就地报错且不发请求", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stubAdmin(page);

    await openChannel(page);
    await openNewChannel(page);
    const drawer = page.getByRole("dialog", { name: "新建渠道" });
    await drawer.locator("#chz-name").fill("坏 JSON");
    await drawer.locator("#chz-url").fill("https://api.example.com");
    await drawer.locator("#chz-keys").fill("sk-aaa");
    await drawer.locator("#chz-patch").fill("{");

    // 实时校验：错误就地显示在该字段下，不必等保存
    await expect(drawer.locator(".form-item", { hasText: "请求体改写" })).toContainText("不是合法 JSON");
    await drawer.getByRole("button", { name: "保存" }).click();
    expect(calls).not.toContain("POST /api/admin/channels");
  });

  test("声明式配置：场景预设一键填充", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);

    await openChannel(page);
    await openNewChannel(page);
    const drawer = page.getByRole("dialog", { name: "新建渠道" });
    await drawer.getByRole("button", { name: "Azure 风格", exact: true }).click();

    await expect(drawer.locator("#chz-auth-header")).toHaveValue("api-key");
    await expect(drawer.locator("#chz-auth-prefix")).toHaveValue("none");
  });

  test("声明式配置：编辑回填且不改动时原样提交", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);

    await openChannel(page, "OpenAI 官方");
    await page.getByRole("button", { name: "编辑渠道" }).click();
    const drawer = page.getByRole("dialog", { name: /编辑渠道 · OpenAI 官方/ });

    await expect(drawer.locator("#chz-auth-header")).toHaveValue("x-api-key");
    await expect(drawer.locator("#chz-paths")).toHaveValue(/\/v4\/chat/);

    const reqPromise = page.waitForRequest(
      (r) => r.url().includes("/api/admin/channels/1") && r.method() === "PUT",
    );
    await drawer.getByRole("button", { name: "保存" }).click();
    const body = (await reqPromise).postDataJSON() as Record<string, any>;
    expect(body.adapter_config.auth_header).toBe("x-api-key");
    expect(body.adapter_config.path_overrides["/v1/chat/completions"]).toBe("/v4/chat");
  });

  test("声明式配置：清空后显式提交 null（否则清不掉）", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);

    await openChannel(page, "OpenAI 官方");
    await page.getByRole("button", { name: "编辑渠道" }).click();
    const drawer = page.getByRole("dialog", { name: /编辑渠道 · OpenAI 官方/ });
    await drawer.locator("#chz-auth-header").fill("");
    await drawer.locator("#chz-auth-prefix").fill("");
    await drawer.locator("#chz-paths").fill("");

    const reqPromise = page.waitForRequest(
      (r) => r.url().includes("/api/admin/channels/1") && r.method() === "PUT",
    );
    await drawer.getByRole("button", { name: "保存" }).click();
    const body = (await reqPromise).postDataJSON() as Record<string, any>;
    expect(body.adapter_config).toBeNull();
  });


  test("删除无绑定渠道：必须二次确认，取消则不发请求", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stubAdmin(page);

    await openChannel(page, "火山方舟");
    await page.getByRole("button", { name: "删除渠道" }).click();

    const dialog = page.getByRole("dialog", { name: "删除渠道" });
    await expect(dialog).toBeVisible();
    // 文案必须点名渠道 + 密钥数，避免误删线上渠道
    await expect(dialog).toContainText("火山方舟");
    await expect(dialog).toContainText("1 个密钥");

    await dialog.getByRole("button", { name: "取消" }).click();
    await expect(dialog).toHaveCount(0);
    expect(calls.some((c) => c.startsWith("DELETE"))).toBe(false);

    // 取消后再删一次：必须真的发出 DELETE
    await page.getByRole("button", { name: "删除渠道" }).click();
    await page.getByRole("dialog", { name: "删除渠道" }).getByRole("button", { name: "删除" }).click();
    await expect(page.getByText("渠道已删除")).toBeVisible();
    expect(calls).toContain("DELETE /api/admin/channels/2");
  });



  test("适配器私有配置：仅 supports_options 的适配器渲染工作流映射", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);

    await openChannel(page);
    await openNewChannel(page);
    const drawer = page.getByRole("dialog", { name: "新建渠道" });

    // 默认 openai（fixture 未声明 supports_options）→ 整块不渲染，避免"配了没人读"
    await expect(drawer.getByText("上游工作流映射（AutoDL）")).toHaveCount(0);

    await drawer.locator("#chz-adapter").selectOption("autodl");
    await expect(drawer.getByText("上游工作流映射（AutoDL）")).toBeVisible();
    // 未配置时回填内置默认：运营一眼能看到真实生效的 workflow ID
    await expect(drawer.locator("#chz-wf-multimodal")).toHaveValue("minimax_h3_zm_u24");
    await expect(drawer.locator("#chz-wf-first_last")).toHaveValue("minimax_h3_lightx2v");
    // 1080p 默认「不支持」（上游最高 768p）
    await expect(drawer.locator("#chz-res-1080p")).toHaveValue("");

    // 切回不支持私有配置的适配器后必须收起：否则会把旧值带给不认识的适配器（后端 400）
    await drawer.locator("#chz-adapter").selectOption("openai");
    await expect(drawer.getByText("上游工作流映射（AutoDL）")).toHaveCount(0);
  });

  test("适配器私有配置：编辑回填并按显式全量提交 adapter_options", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page, { channels: [AUTODL_CHANNEL] });

    await openChannel(page, "AutoDL 官方");
    await page.getByRole("button", { name: "编辑渠道" }).click();
    const drawer = page.getByRole("dialog", { name: /编辑渠道 · AutoDL 官方/ });
    await expect(drawer.locator("#chz-wf-multimodal")).toHaveValue("wf_mm");
    await expect(drawer.locator("#chz-res-720p")).toHaveValue("768p");

    const reqPromise = page.waitForRequest(
      (r) => r.url().includes("/api/admin/channels/7") && r.method() === "PUT",
    );
    await drawer.getByRole("button", { name: "保存" }).click();
    const body = (await reqPromise).postDataJSON() as Record<string, any>;
    expect(body.adapter_options.workflows).toEqual({
      multimodal: "wf_mm",
      image: "wf_img",
      text: "wf_txt",
      first_last: "wf_fl",
    });
    // 720p → 768p 是口径约定；1080p 的空串必须原样提交（空 = 不支持，不能被当成"没填"丢掉）
    expect(body.adapter_options.resolution_map["720p"]).toBe("768p");
    expect(body.adapter_options.resolution_map["1080p"]).toBe("");
  });

  test("适配器私有配置：工作流 ID 重复就地报错且不发请求", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stubAdmin(page);

    await openChannel(page);
    await openNewChannel(page);
    const drawer = page.getByRole("dialog", { name: "新建渠道" });
    await drawer.locator("#chz-adapter").selectOption("autodl");
    await drawer.locator("#chz-name").fill("AutoDL 新建");
    await drawer.locator("#chz-url").fill("https://autodl.art");
    await drawer.locator("#chz-keys").fill("tok-aaa");
    // 与「多模态」的默认工作流撞车：同一 ID 服务两种模式，必有一种跑错
    await drawer.locator("#chz-wf-image").fill("minimax_h3_zm_u24");

    await expect(drawer.locator(".form-item", { hasText: "图片参考（图生视频）" })).toContainText("重复");
    await drawer.getByRole("button", { name: "保存" }).click();
    expect(calls).not.toContain("POST /api/admin/channels");
  });

  test("适配器私有配置：Metaso 与 AutoDL 表单按适配器分流，互不串写", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);

    await openChannel(page);
    await openNewChannel(page);
    const drawer = page.getByRole("dialog", { name: "新建渠道" });

    await drawer.locator("#chz-adapter").selectOption("metaso");
    await expect(drawer.getByText("通道选择与请求选项（Metaso / MiniMax-H3）")).toBeVisible();
    // 两个适配器共用同一个 adapter_options 列：同时渲染两套表单必然写脏
    await expect(drawer.getByText("上游工作流映射（AutoDL）")).toHaveCount(0);
    // 四档默认全开（按上游价目表），2K 必须能看到
    await expect(drawer.locator("#chz-metaso-res-720p")).toHaveValue("768P");
    await expect(drawer.locator("#chz-metaso-res-2k")).toHaveValue("2K");
    // 标准通道：默认关且不出现倒挂警告
    await expect(drawer.locator("#chz-metaso-fast")).not.toBeChecked();
    await expect(drawer.getByText(/该渠道会按优先通道下单/)).toHaveCount(0);

    await drawer.locator("#chz-metaso-fast").check();
    await expect(drawer.getByText(/该渠道会按优先通道下单/)).toBeVisible();

    // 切到 AutoDL：Metaso 块收起、AutoDL 块出现（两边表单态各自独立）
    await drawer.locator("#chz-adapter").selectOption("autodl");
    await expect(drawer.getByText("通道选择与请求选项（Metaso / MiniMax-H3）")).toHaveCount(0);
    await expect(drawer.getByText("上游工作流映射（AutoDL）")).toBeVisible();
  });

  test("适配器私有配置：Metaso 编辑回填并提交四档映射与查询路径", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page, { channels: [METASO_CHANNEL] });

    await openChannel(page, "Metaso 优先通道");
    await page.getByRole("button", { name: "编辑渠道" }).click();
    const drawer = page.getByRole("dialog", { name: /编辑渠道 · Metaso 优先通道/ });

    await expect(drawer.locator("#chz-metaso-fast")).toBeChecked();
    await expect(drawer.locator("#chz-metaso-cir")).toHaveValue("off");
    await expect(drawer.locator("#chz-metaso-res-480p")).toHaveValue("480P");
    // 未开通档位是空串，必须原样回填（否则运营一保存就把它悄悄改成默认的 1080P）
    await expect(drawer.locator("#chz-metaso-res-1080p")).toHaveValue("");
    await expect(drawer.locator("#chz-metaso-qp")).toHaveValue("/api/minimax/v3/query/video_generation/");

    const reqPromise = page.waitForRequest(
      (r) => r.url().includes("/api/admin/channels/9") && r.method() === "PUT",
    );
    await drawer.getByRole("button", { name: "保存" }).click();
    const body = (await reqPromise).postDataJSON() as Record<string, any>;

    expect(body.adapter_options.fast_mode).toBe(true);
    expect(body.adapter_options.context_ir_enabled).toBe(false);
    expect(body.adapter_options.resolution_map).toEqual({
      "480p": "480P",
      "720p": "768P",
      "1080p": "",
      "2k": "2K",
    });
    expect(body.adapter_options.query_path).toBe("/api/minimax/v3/query/video_generation/");
  });

  test("适配器私有配置：多米与 Metaso/AutoDL 三套表单按适配器分流，互不串写", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);

    await openChannel(page);
    await openNewChannel(page);
    const drawer = page.getByRole("dialog", { name: "新建渠道" });

    await drawer.locator("#chz-adapter").selectOption("duomiapi");
    await expect(drawer.getByText("请求选项（多米API）")).toBeVisible();
    // 三套表单共用同一个 adapter_options 列：任何一份同时渲染都会写脏别人的键
    await expect(drawer.getByText("上游工作流映射（AutoDL）")).toHaveCount(0);
    await expect(drawer.getByText("通道选择与请求选项（Metaso / MiniMax-H3）")).toHaveCount(0);
    // 默认「三态」是内置默认（不下发），此时不出现自定义输入框
    await expect(drawer.locator("#chz-duomi-auth")).toHaveValue("default");
    await expect(drawer.locator("#chz-duomi-auth-custom")).toHaveCount(0);
    // 白名单默认放行「音轨 + 平台水印」（与后端内置默认一致）；AIGC 水印是历史键，默认不放行
    await expect(drawer.locator("#chz-duomi-md-generate_audio")).toBeChecked();
    await expect(drawer.locator("#chz-duomi-md-watermark")).toBeChecked();
    await expect(drawer.locator("#chz-duomi-md-aigc_watermark")).not.toBeChecked();

    // 「无前缀」态才渲染自定义框（"-" 是保留值，不能与空白混淆）
    await drawer.locator("#chz-duomi-auth").selectOption("custom");
    await expect(drawer.locator("#chz-duomi-auth-custom")).toBeVisible();

    // 切到 AutoDL：多米块收起（表单态各自独立，切回来时值仍在）
    await drawer.locator("#chz-adapter").selectOption("autodl");
    await expect(drawer.getByText("请求选项（多米API）")).toHaveCount(0);
    await expect(drawer.getByText("上游工作流映射（AutoDL）")).toBeVisible();
    await drawer.locator("#chz-adapter").selectOption("duomiapi");
    await expect(drawer.locator("#chz-duomi-auth")).toHaveValue("custom");
  });

  test("适配器私有配置：多米编辑回填并原样提交四块配置", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page, { channels: [DUOMI_CHANNEL] });

    await openChannel(page, "多米API 视频");
    await page.getByRole("button", { name: "编辑渠道" }).click();
    const drawer = page.getByRole("dialog", { name: /编辑渠道 · 多米API 视频/ });

    // "-" 与空串都回填成「无前缀」（后端归一语义一致）
    await expect(drawer.locator("#chz-duomi-auth")).toHaveValue("none");
    // 路径键带点（kling.prefix），DOM id 已把点换成 `-`（否则 #id.class 选择器失效）
    await expect(drawer.locator("#chz-duomi-path-kling-prefix")).toHaveValue("/api/video/kling/v2/videos/");
    // 未配置的路径留空（跟随内置默认，不把默认值钉死进库）
    await expect(drawer.locator("#chz-duomi-path-seedance-submit")).toHaveValue("");
    await expect(drawer.locator("#chz-duomi-md-generate_audio")).toBeChecked();
    await expect(drawer.locator("#chz-duomi-ov")).toHaveValue(/seedance-lite/);

    const reqPromise = page.waitForRequest(
      (r) => r.url().includes("/api/admin/channels/12") && r.method() === "PUT",
    );
    await drawer.getByRole("button", { name: "保存" }).click();
    const body = (await reqPromise).postDataJSON() as Record<string, any>;

    // 三态与保留值：不下发改动过的默认，改过的必须原样落库
    expect(body.adapter_options.auth_prefix).toBe("-");
    expect(body.adapter_options.paths).toEqual({ "kling.prefix": "/api/video/kling/v2/videos/" });
    // 白名单按固定顺序输出（内容相同 → 字节相同，便于比对与回滚）
    expect(body.adapter_options.metadata_allow).toEqual(["aigc_watermark", "generate_audio"]);
    expect(body.adapter_options.model_overrides).toEqual({
      "seedance-lite": { protocol: "seedance", upstream_model: "doubao-seedance-lite" },
    });
  });
});

/**
 * 密钥池：编辑态不再有"整体替换"的输入框，密钥行直接平铺在抽屉里。
 *
 * 这组用例守的是**入口的形态**与**一条 op 的原子性**：任何一条回退到
 * "编辑抽屉里一个 textarea"或"改名与换值拆成两次请求"，主诉就会原样复现。
 */
test.describe("密钥池：平铺、行内编辑与批量录入", () => {
  test("编辑抽屉没有密钥输入框，密钥行直接平铺", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);
    await openChannel(page);

    await page.getByRole("button", { name: "编辑渠道" }).click();
    const drawer = page.getByRole("dialog", { name: /编辑渠道/ });

    // 曾经的 textarea 提示写着「填写将整体替换」——那就是"改一把要重录全部"的来源。
    await expect(drawer.locator("#chz-keys")).toHaveCount(0);
    await expect(drawer.getByText(/已存 2 把 · 可用 2 把/)).toBeVisible();
    await expect(drawer.getByText("sk-aaa…1111")).toBeVisible();
    // 不再有第二层抽屉：页面上只有编辑渠道这一个 dialog。
    await expect(page.getByRole("dialog")).toHaveCount(1);
  });

  test("行内编辑：同时改别名与密钥 → 一条 replace 带 alias", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);
    await openChannel(page);

    await page.getByRole("button", { name: "编辑渠道" }).click();
    const drawer = page.getByRole("dialog", { name: /编辑渠道/ });
    const firstKey = drawer.locator(".ms-key").first();
    await firstKey.getByRole("button", { name: "编辑密钥" }).click();

    await drawer.getByLabel("别名").fill("主账号");
    await drawer.getByLabel("新密钥").fill("sk-new-0001");

    const reqPromise = page.waitForRequest((r) => r.url().includes("/keys") && r.method() === "PATCH");
    await drawer.locator(".ms-key__band").getByRole("button", { name: "保存" }).click();
    // 换明文不可逆，必须先确认（确认弹窗的按钮文案是「替换」）。
    await page.getByRole("button", { name: "替换", exact: true }).click();

    const body = (await reqPromise).postDataJSON() as { ops: Record<string, unknown>[] };
    // 一条 op 同时改两样：拆成 replace + meta 时 meta 指向的是已被换掉的旧 keyID，后端必然 400。
    expect(body.ops).toEqual([
      { op: "replace", key_id: "kid-1", key: "sk-new-0001", alias: "主账号" },
    ]);
  });

  test("行内编辑：只改别名 → 一条 meta（不碰明文）", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);
    await openChannel(page);

    await page.getByRole("button", { name: "编辑渠道" }).click();
    const drawer = page.getByRole("dialog", { name: /编辑渠道/ });
    await drawer.locator(".ms-key").first().getByRole("button", { name: "编辑密钥" }).click();
    await drawer.getByLabel("别名").fill("主账号");

    const reqPromise = page.waitForRequest((r) => r.url().includes("/keys") && r.method() === "PATCH");
    await drawer.locator(".ms-key__band").getByRole("button", { name: "保存" }).click();

    const body = (await reqPromise).postDataJSON() as { ops: Record<string, unknown>[] };
    expect(body.ops).toEqual([{ op: "meta", key_id: "kid-1", alias: "主账号" }]);
    await expect(page.getByText("别名已更新")).toBeVisible();
  });

  test("批量粘贴 3 行 → 一次 PATCH 带 3 条 add", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);
    await openChannel(page);

    await page.getByRole("button", { name: "编辑渠道" }).click();
    const drawer = page.getByRole("dialog", { name: /编辑渠道/ });
    await drawer.getByRole("button", { name: "添加密钥" }).click();
    await drawer.locator("#chz-key-add").fill("sk-1\nsk-2\nsk-3");

    const reqPromise = page.waitForRequest(
      (r) => r.url().includes("/keys") && r.method() === "PATCH",
    );
    await drawer.getByRole("button", { name: "添加", exact: true }).click();
    const body = (await reqPromise).postDataJSON() as { ops: { op: string; key: string }[] };

    // 一次请求三条 op：拆成三次会让"删了还没加"的中间态被线上请求看到。
    expect(body.ops).toHaveLength(3);
    expect(body.ops.every((o) => o.op === "add")).toBe(true);
    await expect(page.getByText("已新增 3 把")).toBeVisible();
  });

  test("375px 底部 Sheet：密钥行不横向溢出（状态徽标换行，动作不被挤出）", async ({ page }) => {
    // 抽屉内容宽在 375px 下只有 305px：第二把 key 多一个「重置冷却」时，
    // 曾经的布局会把指纹压成三行、并把删除按钮挤出抽屉（滚不出来也点不到）。
    await page.setViewportSize({ width: 375, height: 740 });
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);
    await openChannel(page);

    await page.getByRole("button", { name: "编辑渠道" }).click();
    const drawer = page.getByRole("dialog", { name: /编辑渠道/ });
    await expect(drawer.locator(".ms-key")).toHaveCount(2);

    const m = await drawer.evaluate((el) => {
      const body = el.querySelector(".dr-body") as HTMLElement;
      return {
        body: body.scrollWidth - body.clientWidth,
        // 动作组必须贴齐行右边界（不是越出去）。row.scrollWidth 会被 .t-acts
        // 自身的 4px 内距污染，只认几何位置。
        endGap: [...el.querySelectorAll(".ms-key__top")].map((r) =>
          Math.round(
            (r.querySelector(".ms-key__end") as HTMLElement).getBoundingClientRect().right -
              r.getBoundingClientRect().right,
          ),
        ),
      };
    });
    expect(m.body).toBe(0);
    expect(m.endGap).toEqual([0, 0]);
    // 指纹始终单行：被压缩时会变高（换行）而不是变宽。
    const fpHeight = await drawer.locator(".ms-key__fp").last().evaluate(
      (n) => n.getBoundingClientRect().height,
    );
    expect(fpHeight).toBeLessThan(24);
  });

  test("新建粘贴脱敏指纹：就地报错且不发请求", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stubAdmin(page);
    await openChannel(page);

    await openNewChannel(page);
    const drawer = page.getByRole("dialog", { name: "新建渠道" });
    await drawer.locator("#chz-name").fill("新上游");
    await drawer.locator("#chz-url").fill("https://api.example.com");
    // 面板上显示的就是这个形状，运营最自然的动作就是复制它。
    await drawer.locator("#chz-keys").fill("sk-abc…wxyz");
    await drawer.getByRole("button", { name: "保存" }).click();

    await expect(drawer.getByText(/脱敏后的指纹/)).toBeVisible();
    expect(calls).not.toContain("POST /api/admin/channels");
  });

  test("新建粘贴重复行：提交的 keys 只剩一条", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);
    await openChannel(page);

    await openNewChannel(page);
    const drawer = page.getByRole("dialog", { name: "新建渠道" });
    await drawer.locator("#chz-name").fill("重复上游");
    await drawer.locator("#chz-url").fill("https://api.example.com");
    await drawer.locator("#chz-keys").fill("sk-aaa\nsk-aaa");

    const reqPromise = page.waitForRequest(
      (r) => r.url().endsWith("/api/admin/channels") && r.method() === "POST",
    );
    await drawer.getByRole("button", { name: "保存" }).click();
    const body = (await reqPromise).postDataJSON() as { keys: string[] };
    expect(body.keys).toEqual(["sk-aaa"]);
  });
});
