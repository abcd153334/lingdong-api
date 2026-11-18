/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

/** 已有密钥（启用中，未设白名单与额度上限外的字段） */
const KEY = {
  id: 1,
  key_prefix: "ld-8xaBSWJh",
  name: "默认密钥",
  group: "default",
  status: 1,
  kind: "user" as const,
  amount_limit: 1000000,
  used: 250000,
  models_allowed: null,
  modalities_allowed: null,
  allow_ips: null,
  rpm_limit: 60,
  concurrency_limit: 5,
  expired_at: null,
  allow_user_quota_query: true,
  created_at: "2026-10-01T08:00:00Z",
  reset_secret_at: null,
};

const PLAIN = "ld-8xaBSWJh00000000000000000000000000";
const NEW_PLAIN = "ld-NEWKEY000000000000000000000000000";
const RESET_PLAIN = "ld-RESET0000000000000000000000000000";

const PRICING = {
  models: [],
  vendors: [],
  group_ratios: { default: 1, vip: 0.9 },
  quota_per_unit: 500000,
  cny_per_usd: 7.2,
};

interface StubOptions {
  /** 列表返回的密钥集合（默认仅 KEY） */
  items?: (typeof KEY)[];
}

/** 确定性 Mock：/api/pricing + /api/portal/keys 全生命周期 */
async function stubKeys(page: Page, options: StubOptions = {}): Promise<void> {
  const items = options.items ?? [KEY];
  await page.route("**/api/pricing", (route) => route.fulfill({ json: envelope(PRICING) }));
  // 门户配置：本页用到品牌（标题）与对外地址（地址栏）。
  // 两条地址 = 多域名部署的常态，用来验证「逐条展示 + 各自复制」。
  await page.route("**/api/portal/config", (route) =>
    route.fulfill({
      json: envelope({
        site: { site_name: "灵动 API", logo_image: "", favicon_image: "", site_url: "" },
        api: { bases: ["https://api.example.com", "https://api2.example.com"], source: "config" },
      }),
    }),
  );
  await page.route("**/api/portal/keys**", async (route) => {
    const req = route.request();
    const path = new URL(req.url()).pathname;
    const method = req.method();

    if (path.endsWith("/keys/1/plaintext") && method === "GET") {
      return route.fulfill({ json: envelope({ plaintext: PLAIN }) });
    }
    if (path.endsWith("/keys/1/reset") && method === "POST") {
      return route.fulfill({ json: envelope({ plaintext: RESET_PLAIN }) });
    }
    if (path.endsWith("/keys/1/status")) {
      return route.fulfill({ json: envelope(null) });
    }
    if (path.endsWith("/keys/1") && method === "DELETE") {
      return route.fulfill({ json: envelope(null) });
    }
    if (path.endsWith("/keys") && method === "POST") {
      const body = JSON.parse(req.postData() ?? "{}");
      return route.fulfill({
        json: envelope({ key: { ...KEY, id: 2, name: body.name, key_prefix: "ld-NEWKEY0" }, plaintext: NEW_PLAIN }),
      });
    }
    return route.fulfill({ json: envelope({ items }) });
  });
}

const card = (page: Page) => page.locator(".key-card");

test.describe("S3 API 密钥 · 三态与生命周期", () => {
  test.use({ permissions: ["clipboard-read", "clipboard-write"] });

  test("OpenAI 兼容地址栏：逐条展示并补 /v1、可复制、空态也在", async ({ page }) => {
    await seedAuth(page);
    // 没有任何密钥（空态）也要能看到地址：新用户拿不到地址就只能去猜域名
    await stubKeys(page, { items: [] });

    await page.goto("/keys");
    await expect(page.getByText("还没有密钥")).toBeVisible();

    const bar = page.locator(".card", { hasText: "OpenAI 兼容地址" });
    await expect(bar.getByRole("heading", { name: "OpenAI 兼容地址" })).toBeVisible();

    // 库里存的是 origin（产物直链 /f/、回调 /api/pay/ 都依赖它不带 /v1），
    // /v1 只在展示层追加；两条地址逐条列出，让用户自选线路。
    const rows = bar.locator(".gr-row--url");
    await expect(rows).toHaveCount(2);
    await expect(rows.nth(0).locator("input")).toHaveValue("https://api.example.com/v1");
    await expect(rows.nth(1).locator("input")).toHaveValue("https://api2.example.com/v1");

    await bar.getByRole("button", { name: "复制地址 1" }).click();
    await expect(page.getByText("已复制到剪贴板")).toBeVisible();
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe("https://api.example.com/v1");
  });

  test("三态显示：脱敏 → 明文回显 → 再次隐藏 → 复制", async ({ page }) => {
    await seedAuth(page);
    await stubKeys(page);

    await page.goto("/keys");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("API 密钥");
    await expect(card(page)).toHaveCount(1);

    // 默认脱敏：前缀 + 掩码
    const text = card(page).locator(".key-line .kv-text");
    await expect(text).toHaveText("ld-8xaBSWJh••••••••");

    // 点击显示 → 完整明文
    await card(page).getByRole("button", { name: "显示完整密钥", exact: true }).click();
    await expect(text).toHaveText(PLAIN);

    // 再次点击隐藏
    await card(page).getByRole("button", { name: "隐藏完整密钥", exact: true }).click();
    await expect(text).toHaveText("ld-8xaBSWJh••••••••");

    // 复制 → toast + 剪贴板内容一致
    await card(page).getByRole("button", { name: "复制完整密钥", exact: true }).click();
    await expect(page.getByText("已复制到剪贴板")).toBeVisible();
    const clip = await page.evaluate(() => navigator.clipboard.readText());
    expect(clip).toBe(PLAIN);
  });

  test("创建密钥 → 一次性明文弹窗 → 关闭后不可回看", async ({ page }) => {
    await seedAuth(page);
    await stubKeys(page);

    await page.goto("/keys");
    await page.getByRole("button", { name: "创建 API KEY", exact: true }).click();

    const dialog = page.getByRole("dialog", { name: "创建密钥" });
    await expect(dialog).toBeVisible();
    await dialog.locator("#k-name").fill("生产密钥");
    await dialog.getByRole("button", { name: "保存", exact: true }).click();

    const secret = page.getByRole("dialog", { name: "保存你的 API 密钥" });
    await expect(secret).toBeVisible();
    await expect(secret.locator(".key-reveal .kv > span")).toHaveText(NEW_PLAIN);

    await secret.getByRole("button", { name: "我已妥善保存", exact: true }).click();
    await expect(secret).toBeHidden();
  });

  test("表单校验：名称必填 + IP 段合法性", async ({ page }) => {
    await seedAuth(page);
    await stubKeys(page);

    await page.goto("/keys");
    await page.getByRole("button", { name: "创建 API KEY", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "创建密钥" });
    const save = dialog.getByRole("button", { name: "保存", exact: true });

    // 名称缺失 → 拒绝
    await save.click();
    await expect(page.getByText("请填写密钥名称")).toBeVisible();

    // 非法 IP → 拒绝
    await dialog.locator("#k-name").fill("测试密钥");
    await dialog.locator("#k-ip").fill("999.1.1.1");
    await save.click();
    await expect(page.getByText(/不是合法的 IP 段/)).toBeVisible();

    // 合法 CIDR → 通过
    await dialog.locator("#k-ip").fill("203.0.113.0/24");
    await save.click();
    await expect(page.getByRole("dialog", { name: "保存你的 API 密钥" })).toBeVisible();
  });

  test("重置密钥：二次确认 → 新一次性明文", async ({ page }) => {
    await seedAuth(page);
    await stubKeys(page);

    await page.goto("/keys");
    await card(page).getByRole("button", { name: "重置", exact: true }).click();

    const confirm = page.getByRole("dialog", { name: "重置密钥" });
    await expect(confirm).toBeVisible();
    await confirm.getByRole("button", { name: "确认重置", exact: true }).click();

    const secret = page.getByRole("dialog", { name: "保存你的 API 密钥" });
    await expect(secret.locator(".key-reveal .kv > span")).toHaveText(RESET_PLAIN);
  });

  test("禁用与删除：二次确认与结果提示", async ({ page }) => {
    await seedAuth(page);
    await stubKeys(page);

    await page.goto("/keys");
    // 禁用
    await card(page).getByRole("button", { name: "禁用", exact: true }).click();
    const disable = page.getByRole("dialog", { name: "禁用密钥" });
    await expect(disable).toBeVisible();
    await disable.getByRole("button", { name: "确认", exact: true }).click();
    await expect(page.getByText("密钥已禁用")).toBeVisible();

    // 删除
    await card(page).getByRole("button", { name: "删除", exact: true }).click();
    const del = page.getByRole("dialog", { name: "删除密钥" });
    await del.getByRole("button", { name: "确认删除", exact: true }).click();
    await expect(page.getByText("密钥已删除")).toBeVisible();
  });
});
