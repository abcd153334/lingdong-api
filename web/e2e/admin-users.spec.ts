/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

/** 管理台会话（role=0） */
const seedAdmin = (page: Parameters<typeof seedAuth>[0]) => seedAuth(page, { role: 0 });

const USERS = [
  {
    id: 2,
    username: "alice",
    email: "",
    phone: "",
    group: "default",
    status: 1,
    role: 1,
    balance: 3_000_000,
    used: 1_000_000,
    balance_limit: 0,
    request_count: 12,
    key_count: 1,
    created_at: "2026-09-01 09:00:00",
  },
  {
    id: 3,
    username: "bob",
    email: "",
    phone: "",
    group: "vip",
    status: 2,
    role: 1,
    balance: 0,
    used: 0,
    balance_limit: 0,
    request_count: 0,
    key_count: 0,
    created_at: "2026-09-02 09:00:00",
  },
];

const TRANSACTIONS = {
  items: [
    {
      id: 9,
      type: "adjust",
      amount: 1_000_000,
      balance_after: 3_000_000,
      reason: "内测补偿",
      created_at: "2026-09-03 10:00:00",
    },
  ],
  total: 1,
  page: 1,
  page_size: 5,
};

const KEYS = {
  items: [
    {
      id: 7,
      key_prefix: "sk-abcd",
      name: "生产密钥",
      group: "default",
      status: 1,
      amount_limit: 0,
      used: 0,
      models_allowed: [],
      modalities_allowed: [],
      rpm_limit: 60,
      concurrency_limit: 5,
      expired_at: null,
      created_at: "2026-09-03 09:00:00",
    },
  ],
};

const LOGS = {
  items: [
    {
      id: 103,
      created_at: "2026-09-04 12:00:00",
      user_id: 2,
      username: "alice",
      key_id: 7,
      key_name: "生产密钥",
      model: "gpt-4o",
      channel_id: 1,
      channel_name: "openai",
      modality: "chat",
      kind: "consume",
      status: "success",
      http_status: 200,
      prompt_tokens: 100,
      completion_tokens: 50,
      total_tokens: 150,
      charge: 500000,
      latency_ms: 800,
      error_msg: "",
      is_stream: false,
    },
  ],
  total: 1,
  page: 1,
  page_size: 5,
};

/** 用户管理桩件：按 pathname 分流列表 / 流水 / 密钥 / 调用日志 */
async function stubAdminUsers(page: Page): Promise<void> {
  await page.route("**/api/admin/users**", (route) => {
    const { pathname } = new URL(route.request().url());
    if (pathname.endsWith("/transactions")) return route.fulfill({ json: envelope(TRANSACTIONS) });
    if (pathname.endsWith("/keys")) return route.fulfill({ json: envelope(KEYS) });
    return route.fulfill({ json: envelope({ items: USERS, total: USERS.length, page: 1, page_size: 20 }) });
  });
  // 只拦列表（带查询串）：`/admin/logs/:id` 若也被这个 glob 吃掉，
  // 详情接口会拿到分页体，抽屉静默空白。
  await page.route("**/api/admin/logs?**", (route) => route.fulfill({ json: envelope(LOGS) }));
}

test.describe("S17 用户管理 · 详情抽屉与调账入口", () => {
  test("操作列可用：详情 / 调账 / 禁用启用均为文字按钮", async ({ page }) => {
    await seedAdmin(page);
    await stubAdminUsers(page);

    await page.goto("/admin/users");
    await expect(page.locator(".ph-desc")).toHaveText("余额调账、状态管控与密码重置");

    const row = page.locator(".tbl tbody tr", { hasText: "alice" });
    await expect(row.getByRole("button", { name: "详情" })).toHaveClass(/t-act/);
    await expect(row.getByRole("button", { name: "调账" })).toHaveClass(/t-act/);
    await expect(row.getByRole("button", { name: "禁用" })).toBeVisible();

    // 列集合回归原型（8 列，无「KEY 数」）
    const head = page.locator(".tbl thead");
    await expect(head).toContainText("用户名");
    await expect(head).toContainText("分组");
    await expect(head).toContainText("余额");
    await expect(head).toContainText("累计消耗");
    await expect(head).toContainText("请求数");
    await expect(head).toContainText("注册");
    await expect(head).not.toContainText("KEY 数");

    // 禁用态用户显示「启用」；重置密码 / 删除无后端能力，不渲染
    const bobRow = page.locator(".tbl tbody tr", { hasText: "bob" });
    await expect(bobRow.getByRole("button", { name: "启用" })).toBeVisible();
    await expect(row.getByRole("button", { name: "重置密码" })).toHaveCount(0);
    await expect(row.getByRole("button", { name: "删除" })).toHaveCount(0);

    // 筛选条回归原型：关键词 / 分组 / 状态 + 「重置」按钮（「＋新建用户」无后端能力，不渲染）
    await expect(page.locator(".pbar-fields input[type='search']")).toHaveCount(1);
    await expect(page.locator(".pbar-fields select")).toHaveCount(2);
    await expect(page.locator(".pbar .acts").getByRole("button", { name: "重置" })).toBeVisible();
    await expect(page.getByRole("button", { name: "＋ 新建用户" })).toHaveCount(0);
  });

  test("禁用/启用：行操作二次确认后 PATCH 状态", async ({ page }) => {
    await seedAdmin(page);
    await stubAdminUsers(page);

    const patches: { url: string; body: unknown }[] = [];
    await page.route("**/api/admin/users/*", async (route) => {
      if (route.request().method() !== "PATCH") return route.fallback();
      patches.push({
        url: new URL(route.request().url()).pathname,
        body: route.request().postDataJSON(),
      });
      await route.fulfill({ json: envelope(null) });
    });

    await page.goto("/admin/users");
    const row = page.locator(".tbl tbody tr", { hasText: "alice" });
    await row.getByRole("button", { name: "禁用" }).click();
    const dialog = page.getByRole("dialog", { name: "禁用用户" });
    await expect(dialog).toContainText("alice");
    await dialog.getByRole("button", { name: "确认" }).click();

    await expect.poll(() => patches.length).toBe(1);
    expect(patches[0].url).toBe("/api/admin/users/2");
    expect(patches[0].body).toEqual({ status: 2 });
    await expect(page.getByText("用户已禁用")).toBeVisible();
  });

  test("详情抽屉：三块真实数据且密钥不含明文", async ({ page }) => {
    await seedAdmin(page);
    await stubAdminUsers(page);

    await page.goto("/admin/users");
    await page.locator(".tbl tbody tr", { hasText: "alice" }).getByRole("button", { name: "详情" }).click();

    const drawer = page.locator(".drawer.open");
    await expect(drawer).toContainText("① 基本信息");
    await expect(drawer).toContainText("② 余额流水");
    await expect(drawer).toContainText("③ 密钥列表");
    await expect(drawer).toContainText("④ 最近调用");
    // 已彻底移除「待 S9 / S3 / S5 接入」桩文案
    await expect(drawer).not.toContainText("待 S9");

    // ② 余额流水
    await expect(drawer).toContainText("调账");
    await expect(drawer).toContainText("内测补偿");
    // ③ 密钥列表：仅索引前缀
    await expect(drawer).toContainText("生产密钥");
    await expect(drawer).toContainText("sk-abcd");
    // ④ 最近调用
    await expect(drawer).toContainText("gpt-4o");
    await expect(drawer).toContainText("150");

    // 抽屉内三块各自成表（基本信息为 kv-grid，不计入）
    await expect(drawer.locator(".tbl")).toHaveCount(3);

    await drawer.getByRole("button", { name: "关闭" }).click();
    await expect(page.locator(".drawer.open")).toHaveCount(0);
  });

  test("调账入口可用：打开调账弹窗并提交校验", async ({ page }) => {
    await seedAdmin(page);
    await stubAdminUsers(page);
    const adjustBodies: unknown[] = [];
    await page.route("**/api/admin/users/*/quota", (route) => {
      adjustBodies.push(route.request().postDataJSON());
      return route.fulfill({ json: envelope(null) });
    });

    await page.goto("/admin/users");
    await page.locator(".tbl tbody tr", { hasText: "alice" }).getByRole("button", { name: "调账" }).click();

    await expect(page.locator(".dialog.open")).toContainText("额度调账");

    // 原因未填 → 按钮**直接禁用**（调账是不可撤销的余额写入，不留给「点了再弹 toast」）
    await page.locator("#adj-amt").fill("1000000");
    const confirmAdjust = page.getByRole("button", { name: "确认调账" });
    await expect(confirmAdjust).toBeDisabled();
    expect(adjustBodies).toHaveLength(0);

    await page.locator("#adj-reason").fill("内测补偿");
    await expect(confirmAdjust).toBeEnabled();
    await confirmAdjust.click();
    await expect(page.getByText(/审计已记录/)).toBeVisible();
    expect(adjustBodies).toHaveLength(1);
    // 输入框填的是**金额**（货币单位），载荷是**微**：1e6 × 1e6 = 1e12 微
    expect(adjustBodies[0]).toEqual({ amount: 1_000_000_000_000, reason: "内测补偿" });
  });
});

const APP_OPTIONS_USERS = [
  { id: 5, slug: "web-portal", name: "web-portal", display_name: "官网门户", status: 1 },
  { id: 9, slug: "shadow", name: "影画", display_name: "", status: 2 },
];

test.describe("S17 用户管理 · 按应用筛选（只筛不加列）", () => {
  test("选应用后请求带 app_id；表头不出现「应用」列", async ({ page }) => {
    await seedAdmin(page);
    await stubAdminUsers(page);
    await page.route("**/api/admin/apps/options**", (route) =>
      route.fulfill({ json: envelope({ items: APP_OPTIONS_USERS, truncated: false }) }),
    );
    const seen: string[] = [];
    // 只拦带查询串的列表：/keys、/transactions 仍走上面的桩
    await page.route("**/api/admin/users?**", (route) => {
      seen.push(route.request().url());
      return route.fulfill({
        json: envelope({ items: USERS, total: USERS.length, page: 1, page_size: 20 }),
      });
    });

    await page.goto("/admin/users");
    // 用户池是全局的：不显示「所属应用」列（一个用户可同时在多个应用下持有密钥）
    await expect(page.locator(".tbl thead")).not.toContainText("应用");

    await page.getByRole("combobox", { name: "应用" }).click();
    await page.getByRole("option", { name: /官网门户/ }).click();

    await expect.poll(() => seen.some((u) => u.includes("app_id=5"))).toBe(true);
  });
});
