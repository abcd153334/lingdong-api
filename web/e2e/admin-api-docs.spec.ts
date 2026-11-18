/**
 * 开放接口手册（三栏：目录 / 契约 / 参考）+ 试运行。
 *
 * 这页的数据**只能**来自后端 apicontract（运行期守卫与文档同源）。
 * 用例刻意不校验"文档里写了 41 个接口"这种数字，只校验「页面上显示的就是后端下发的」——
 * 数字的正确性由后端 apicontract 的门禁守，前端重复一遍只会变成第二处真相源。
 *
 * 试运行的断言分两类，不能混：
 *   1. **不该发请求的**（收起态、必填缺失、不可试运行、二次确认取消）：必须证明
 *      "一个请求都没出去" —— 只断言按钮禁用是不够的，那可能是 CSS 假象。
 *   2. **该发请求的**：用 page.route 截住并断言**请求本身**（方法、头、无 Cookie），
 *      再断言响应如何渲染。只看渲染会漏掉"页面显示成功但发错了头"。
 */
import { expect, test, type Page } from "@playwright/test";
import { envelope, expectNoHScroll, seedAuth } from "./helpers";

const seedAdmin = (page: Page) => seedAuth(page, { role: 0 });

/** 一条端点的完整契约（字段与后端 adminAPIDocsItem 一一对应）。 */
type Item = Record<string, unknown>;

function ep(patch: Item): Item {
  return {
    key: "x.y",
    group: "认证",
    title: "某接口",
    method: "GET",
    path: "/api/user/x",
    auth: "reader",
    scope: "",
    scope_enforced: false,
    media: "json",
    idempotency: "",
    notes: "",
    params: [],
    returns: [],
    errors: [],
    no_data: false,
    sortable: [],
    request_example: {},
    response_example: {},
    try: { callable: true, hint: "", risk: "safe" },
    ...patch,
  };
}

const REGISTER = ep({
  key: "auth.register",
  group: "认证",
  title: "注册",
  method: "POST",
  path: "/api/user/register",
  auth: "open",
  params: [
    {
      name: "username",
      in: "body",
      type: "string",
      required: "required",
      required_note: "",
      desc: "登录名",
      note: "4 到 32 位",
      default: "",
      enum: [],
      example: "alice",
      constraints: [{ kind: "max_length", value: 32, text: "最多 32 个字" }],
    },
    {
      name: "channel",
      in: "body",
      type: "string",
      required: "optional",
      required_note: "",
      desc: "注册渠道",
      note: "",
      default: "web",
      enum: ["web", "app"],
      example: "",
      constraints: [],
    },
  ],
  returns: [{ name: "uid", type: "integer", desc: "用户编号", note: "", enum: [] }],
  errors: [{ code: "register_closed", when: "站点已关闭注册" }],
  request_example: { username: "alice" },
  response_example: { uid: 7 },
});

const SELF = ep({
  key: "account.self",
  group: "账户",
  title: "查看自己的账户",
  path: "/api/user/self",
  auth: "reader",
  returns: [{ name: "balance", type: "integer", desc: "余额", note: "单位：微", enum: [] }],
});

/** 仅会话票：不可试运行，`hint` 必须原样展示。 */
const HOSTED_KEY = ep({
  key: "apps.hosted_key",
  group: "账户",
  title: "签发托管密钥",
  method: "POST",
  path: "/api/user/apps/:slug/hosted-key",
  auth: "session",
  try: { callable: false, hint: "这条接口必须用门户会话，密钥不能代做", risk: "safe" },
});

/** 路径参数 + 写操作 + 高风险：用来验二次确认与路径替换。 */
const KEY_DELETE = ep({
  key: "keys.delete",
  group: "密钥",
  title: "删除密钥",
  method: "DELETE",
  path: "/api/user/keys/:id",
  auth: "writer",
  scope: "keys:write",
  scope_enforced: true,
  no_data: true,
  params: [
    {
      name: "id",
      in: "path",
      type: "integer",
      required: "required",
      required_note: "",
      desc: "密钥编号",
      note: "",
      default: "",
      enum: [],
      example: 3,
      constraints: [],
    },
  ],
  try: { callable: true, hint: "", risk: "confirm" },
});

/** 支持排序：`sort` 参数由后端按 `sortable` 自动生成，前端只负责渲染。 */
const USAGE = ep({
  key: "usage.list",
  group: "用量",
  title: "调用记录",
  path: "/api/user/usage",
  auth: "reader",
  sortable: ["created_at", "amount"],
  params: [
    {
      name: "page",
      in: "query",
      type: "integer",
      required: "optional",
      required_note: "",
      desc: "页码",
      note: "",
      default: "1",
      enum: [],
      example: 1,
      constraints: [],
    },
    {
      name: "sort",
      in: "query",
      type: "string",
      required: "optional",
      required_note: "",
      desc: "排序字段，前缀 - 表示倒序",
      note: "只支持一个字段；不传按 id 倒序",
      default: "",
      enum: ["created_at", "-created_at", "amount", "-amount"],
      example: "",
      constraints: [],
    },
  ],
  returns: [{ name: "items[].id", type: "integer", desc: "记录 ID", note: "", enum: [] }],
});

const CATALOG = {
  groups: ["认证", "账户", "密钥", "用量"],
  items: [REGISTER, SELF, HOSTED_KEY, KEY_DELETE, USAGE],
  not_exposed: [{ method: "", path: "", title: "渠道配置", reason: "接入方不需要知道渠道怎么配" }],
  counts: { total: 5, open: 1, reader: 2, writer: 1, session: 1 },
  hosted_scopes: ["self:read", "models:read"],
  callable: 4,
  links: {
    catalog: "/api/admin/api-docs",
    openapi: "/api/admin/api-docs/openapi.json",
    markdown: "/api/admin/api-docs/markdown",
    swagger_ui: "/api/admin/api-docs/ui/",
  },
};

async function stub(page: Page, opts: { fail?: boolean } = {}): Promise<void> {
  // 试运行面板会问「管理员自己有没有可用密钥」：这里给空列表，让它走手填。
  // 不 stub 就会打到真后端并 401，401 的全局处理会把会话判死，后续断言跟着红。
  await page.route("**/api/portal/keys", (route) =>
    route.fulfill({ json: envelope({ items: [] }) }),
  );
  await page.route("**/api/admin/api-docs**", (route) => {
    const u = new URL(route.request().url());
    if (opts.fail) {
      return route.fulfill({
        json: { code: 403, message: "无权限查看接口契约", error_code: "forbidden", data: null },
      });
    }
    if (u.pathname === "/api/admin/api-docs/openapi.json") {
      return route.fulfill({
        body: JSON.stringify({ openapi: "3.1.0" }),
        contentType: "application/json",
        headers: { "Content-Disposition": 'attachment; filename="lingdong-openapi.json"' },
      });
    }
    if (u.pathname === "/api/admin/api-docs/markdown") {
      return route.fulfill({
        body: "# 灵动 API",
        contentType: "text/markdown",
        headers: { "Content-Disposition": 'attachment; filename="lingdong-api.md"' },
      });
    }
    return route.fulfill({ json: envelope(CATALOG) });
  });
}

/** 打开某条端点（深链，省去在目录里点）。 */
const openEndpoint = async (page: Page, key: string) => {
  await page.goto(`/admin/api-docs?ep=${key}`);
};

test.describe("开放接口手册（管理台）", () => {
  test("三栏骨架：目录按后端顺序分组，默认选中第一条", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.goto("/admin/api-docs");

    await expect(page.getByRole("heading", { level: 1 })).toHaveText("开放接口");
    await expect(page.locator(".a-side nav a.active")).toContainText("开放接口");
    await expect(page.locator(".a-top .crumb b")).toHaveText("开放接口");

    // 三栏都在
    await expect(page.locator(".adx-nav")).toBeVisible();
    await expect(page.locator(".adx-main")).toBeVisible();
    await expect(page.locator(".adx-ref")).toBeVisible();

    // 分组标题按后端 groups 顺序（不是字母序）
    await expect(page.locator(".adx-nav .adx-grp > .h")).toHaveText(["认证", "账户", "密钥", "用量"]);

    // 没有 ?ep= 时默认第一条：标题必须是「注册」，不是空白
    await expect(page.locator(".adx-head .ttl")).toHaveText("注册");
    await expect(page.locator(".adx-head .adx-path")).toContainText("/api/user/register");
  });

  test("深链 ?ep= 选中对应端点；未知 key 回落到第一条而不是白屏", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);

    await openEndpoint(page, "keys.delete");
    await expect(page.locator(".adx-head .ttl")).toHaveText("删除密钥");
    // 选中项要有 aria-current，否则键盘与读屏用户不知道自己在哪
    await expect(page.locator('.adx-item[aria-current="true"]')).toContainText("/keys/:id");

    await openEndpoint(page, "nope.nope");
    await expect(page.locator(".adx-head .ttl")).toHaveText("注册");
  });

  test("目录搜索：按路径缩小列表，清空后恢复", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.goto("/admin/api-docs");

    const search = page.getByLabel("搜索接口");
    await search.fill("keys");
    await expect(page.locator(".adx-item")).toHaveCount(1);
    await expect(page.locator(".adx-item")).toContainText("/keys/:id");

    await search.fill("zzz");
    await expect(page.getByText("没有匹配的接口")).toBeVisible();

    await search.fill("");
    await expect(page.locator(".adx-item")).toHaveCount(5);
  });

  test("参数表：位置/类型/三态必填/枚举/默认值/约束都来自契约", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await openEndpoint(page, "auth.register");

    const rows = page.getByTestId("apidoc-params").locator("tbody tr");
    await expect(rows).toHaveCount(2);

    const user = page.getByTestId("apidoc-params").locator("tbody tr", { hasText: "username" });
    await expect(user).toContainText("请求体");
    await expect(user).toContainText("string");
    await expect(user).toContainText("必填");
    await expect(user).toContainText("4 到 32 位");
    await expect(user).toContainText("最多 32 个字");

    const channel = page.getByTestId("apidoc-params").locator("tbody tr", { hasText: "channel" });
    // 可选 + 枚举逐项列出 + 默认值
    await expect(channel).toContainText("可选");
    // 用 `^…$` 锚定：默认值那格也含 "web"（"默认 web"），子串匹配会命中两个
    await expect(channel.locator("code").filter({ hasText: /^web$/ })).toBeVisible();
    await expect(channel.locator("code").filter({ hasText: /^app$/ })).toBeVisible();
    await expect(channel).toContainText("默认 web");
  });

  test("排序参数：sort 由 sortable 生成，升降两种形态都要能选", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await openEndpoint(page, "usage.list");

    const row = page.getByTestId("apidoc-params").locator("tbody tr", { hasText: "sort" });
    await expect(row).toContainText("可选");
    await expect(row).toContainText("倒序");
    // 前导 `-` 的枚举值必须原样渲染出来：若被当成空值/非法值过滤掉，
    // 用户只能选升序，会以为这条接口不支持倒序，而服务端其实支持。
    await expect(row.locator("code").filter({ hasText: /^-amount$/ })).toBeVisible();
    await expect(row.locator("code").filter({ hasText: /^amount$/ })).toBeVisible();
  });

  test("返回区：no_data 端点说「没有返回数据」，有返回的端点列出字段与失败码", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);

    await openEndpoint(page, "keys.delete");
    await expect(page.locator(".adx-sec", { hasText: "返回" }).first()).toContainText("没有返回数据");

    await openEndpoint(page, "auth.register");
    await expect(page.locator(".adx-sec", { hasText: "返回" }).first()).toContainText("uid");
    const errs = page.locator(".adx-sec", { hasText: "失败码" });
    await expect(errs).toContainText("register_closed");
    await expect(errs).toContainText("站点已关闭注册");
    // 请求 / 响应示例
    await expect(page.getByTestId("apidoc-req-example")).toContainText("alice");
    await expect(page.getByTestId("apidoc-resp-example")).toContainText("uid");
  });

  test("试运行默认收起：不展开就一个请求都不发", async ({ page }) => {
    const sent: string[] = [];
    await seedAdmin(page);
    await stub(page);
    await page.route("**/api/user/**", (route) => {
      sent.push(route.request().url());
      return route.fulfill({ json: envelope({}) });
    });

    await openEndpoint(page, "account.self");
    await expect(page.getByTestId("apidoc-try")).toBeVisible();
    // 收起态看不到发送按钮
    await expect(page.getByRole("button", { name: "发送" })).toHaveCount(0);
    await page.waitForTimeout(150);
    expect(sent).toEqual([]);

    await page.getByRole("button", { name: "展开" }).click();
    await expect(page.getByRole("button", { name: "发送" })).toBeVisible();
    expect(sent).toEqual([]);
  });

  test("必填缺失时禁止发送，并写明缺哪些；填上后放行", async ({ page }) => {
    const sent: string[] = [];
    await seedAdmin(page);
    await stub(page);
    await page.route("**/api/user/**", (route) => {
      sent.push(route.request().url());
      return route.fulfill({ json: envelope({ ok: 1 }) });
    });

    await openEndpoint(page, "keys.delete");
    await page.getByRole("button", { name: "展开" }).click();

    const send = page.getByRole("button", { name: "发送" });
    await expect(send).toBeDisabled();
    await expect(page.locator(".missing")).toContainText("还缺：id");

    // 填上路径参数后按钮放行（此时还没点，所以仍未发出任何请求）
    await page.getByLabel(/id（路径）/).fill("42");
    await expect(send).toBeEnabled();
    expect(sent).toEqual([]);
  });

  test("试运行 JSON 模式：手写体原样发出，写坏时禁止发送", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);

    const bodies: string[] = [];
    await page.route("**/api/user/register", async (route) => {
      bodies.push(route.request().postData() ?? "");
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ code: 0, message: "success", data: { uid: 9 } }),
      });
    });

    await openEndpoint(page, "auth.register");
    await page.getByRole("button", { name: "展开" }).click();

    // 管理台刻意不预填示例值（一次误点就是真实写操作）：空表单切过去应当就是 {}
    await page.getByRole("button", { name: /^JSON$/ }).click();
    const area = page.getByTestId("apidoc-try").locator("textarea");
    await expect(area).toHaveCount(1);
    await expect(area).toHaveValue("{}");
    // 表单里的 body 参数这时不该再显示（否则会让人以为两处都会发）
    await expect(page.getByLabel(/username（/)).toHaveCount(0);

    // 先回表单填一个值，再切回来：应当带过去（"由表单生成"的唯一来源）
    await page.getByRole("button", { name: /^表单$/ }).click();
    await page.getByLabel(/username（/).fill("abc");
    await page.getByRole("button", { name: /^JSON$/ }).click();
    await expect(area).toHaveValue(/"username": "abc"/);

    const raw = '{\n  "username": "自定义名",\n  "channel": "app",\n  "note": [1, 2]\n}';
    await area.fill(raw);

    await page.getByRole("button", { name: "发送" }).click();
    await expect(page.getByTestId("apidoc-try-result")).toContainText("200");
    expect(bodies).toHaveLength(1);
    expect(bodies[0]).toBe(raw);

    // 写坏：就地标红 + 发送禁用，一个请求都不再发
    await area.fill('{"username": ');
    await expect(page.locator(".adx-err")).toContainText("JSON");
    await expect(page.getByRole("button", { name: "发送" })).toBeDisabled();
    expect(bodies).toHaveLength(1);
  });

  test("试运行：请求真的发出（Bearer 密钥、无 Cookie），响应按状态与耗时渲染", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);

    let seen: { method: string; auth: string; cookie: string } | undefined;
    await page.route("**/api/user/self**", (route) => {
      const r = route.request();
      seen = {
        method: r.method(),
        auth: r.headers()["authorization"] ?? "",
        cookie: r.headers()["cookie"] ?? "",
      };
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ code: 0, message: "success", data: { balance: 123 } }),
      });
    });

    await openEndpoint(page, "account.self");
    await page.getByRole("button", { name: "展开" }).click();
    await page.getByLabel("接入密钥").fill("sk-e2e");
    await page.getByRole("button", { name: "发送" }).click();

    const result = page.getByTestId("apidoc-try-result");
    await expect(result).toBeVisible();
    await expect(result).toContainText("200");
    await expect(result).toContainText("ms");
    // JSON 被美化（换行 + 缩进），而不是一整行原文
    await expect(result.locator("pre")).toContainText('"balance": 123');

    expect(seen?.method).toBe("GET");
    expect(seen?.auth).toBe("Bearer sk-e2e");
    // 关键：不带管理台 Cookie。带了就代表"用会话代发"，验证不出接入方的真实处境。
    expect(seen?.cookie ?? "").toBe("");
  });

  test("高风险端点：取消二次确认则不发请求；确认后才发", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);

    const sent: string[] = [];
    await page.route("**/api/user/keys/**", (route) => {
      sent.push(route.request().method());
      return route.fulfill({ status: 204, body: "" });
    });

    await openEndpoint(page, "keys.delete");
    await page.getByRole("button", { name: "展开" }).click();
    await page.getByLabel(/id（路径）/).fill("9");
    await page.getByLabel("接入密钥").fill("sk-e2e");

    await page.getByRole("button", { name: "发送" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toContainText("会真的发到生产环境");
    await dialog.getByRole("button", { name: /取消/ }).click();
    await page.waitForTimeout(150);
    expect(sent).toEqual([]);

    await page.getByRole("button", { name: "发送" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "仍然发送" }).click();
    await expect.poll(() => sent.length).toBe(1);
    expect(sent[0]).toBe("DELETE");
    await expect(page.getByTestId("apidoc-try-result")).toContainText("204");
  });

  test("仅会话票端点：只给原因，不渲染发送按钮", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await openEndpoint(page, "apps.hosted_key");

    const blocked = page.getByTestId("apidoc-try-blocked");
    await expect(blocked).toBeVisible();
    await expect(blocked).toContainText("必须用门户会话");
    await expect(page.getByRole("button", { name: "发送" })).toHaveCount(0);
  });

  test("参考栏：四个视图地址来自后端，Swagger UI 在前缀下打开", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.goto("/admin/api-docs");

    const ref = page.locator(".adx-ref");
    await expect(ref).toContainText("鉴权级别");
    await expect(ref).toContainText("托管密钥");
    await expect(ref).toContainText("self:read");
    // 不开放清单必须给出原因，而不是让接入方猜
    await expect(ref).toContainText("渠道配置");
    await expect(ref).toContainText("接入方不需要知道渠道怎么配");

    // Swagger UI 走 apiUrl 拼地址（`/api` 前缀只出现一次）
    const popup = page.context().waitForEvent("page");
    await ref.getByRole("button", { name: "打开试调用页" }).click();
    const opened = await popup;
    expect(opened.url()).toContain("/api/admin/api-docs/ui/");
    expect(opened.url()).not.toContain("/api/api/");
    await opened.close();
  });

  test("导出：OpenAPI 与 Markdown 各自请求对应端点", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.goto("/admin/api-docs");

    const openapiReq = page.waitForRequest((r) => r.url().includes("/api/admin/api-docs/openapi.json"));
    await page.getByRole("button", { name: "OpenAPI" }).click();
    await openapiReq;

    const mdReq = page.waitForRequest((r) => r.url().includes("/api/admin/api-docs/markdown"));
    await page.getByRole("button", { name: "Markdown" }).click();
    await mdReq;
  });

  test("加载失败：显示后端原话，不是空白页", async ({ page }) => {
    await seedAdmin(page);
    await stub(page, { fail: true });
    await page.goto("/admin/api-docs");

    await expect(page.getByText("接口契约加载失败")).toBeVisible();
    await expect(page.getByText("无权限查看接口契约").first()).toBeVisible();
  });

  test("移动端 375px：目录成横向条，页面无横向滚动", async ({ page }) => {
    await seedAdmin(page);
    await stub(page);
    await page.setViewportSize({ width: 375, height: 780 });
    await page.goto("/admin/api-docs");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("开放接口");
    await expect(page.locator(".adx-main")).toBeVisible();

    await expectNoHScroll(page);
  });
});
