/** 托管密钥卡片（kind=hosted）：操作收敛 + 轮换入口。契约级 stub 用例。 */
import { expect, test, type Page } from "@playwright/test";
import { envelope, expectNoHScroll, seedAuth } from "./helpers";

const PRICING = {
  models: [],
  vendors: [],
  group_ratios: { default: 1, vip: 0.9 },
  quota_per_unit: 500000,
  cny_per_usd: 7.2,
};

/** 应用托管密钥：管理员封禁态（轮换不会解除，因此**不应**出现轮换按钮）。 */
const BLOCKED_KEY = {
  id: 91,
  key_prefix: "ld-hosted1",
  name: "应用托管密钥",
  group: "default",
  status: 2,
  kind: "hosted" as const,
  app_id: 7,
  // slug/name 随列表直出：轮换按钮只依赖它，不再先查 /portal/apps。
  app_slug: "demo-app",
  app_name: "演示应用",
  // 管理员封禁：轮换不会解除，文案与恢复路径与 expired 不同。
  disabled_reason: "admin_block",
  amount_limit: 100000000,
  used: 500000,
  models_allowed: null,
  modalities_allowed: null,
  allow_ips: null,
  rpm_limit: 600,
  concurrency_limit: 0,
  expired_at: "2027-05-01T00:00:00Z",
  allow_user_quota_query: true,
  created_at: "2026-11-01T08:00:00Z",
  reset_secret_at: "2026-11-01T08:00:00Z",
  scopes: ["chat:read", "chat:write", "media:read", "task:read"],
};

/** 同一把钥，但闲置超期：轮换即可恢复，因此**应该**出现轮换按钮。 */
const EXPIRED_KEY = { ...BLOCKED_KEY, id: 92, status: 2, disabled_reason: "expired", reset_secret_at: null };

/** 正常启用：轮换的主场景。 */
const ACTIVE_KEY = { ...BLOCKED_KEY, id: 93, status: 1, disabled_reason: "" };

const ROTATE_PLAIN = "ld-ROTATED0000000000000000000000000";
const ISSUE_PLAIN = "ld-ISSUED00000000000000000000000000";

/** 签发响应的 key 段（与轮换同形，只有 created/rotated 不同）。 */
function issuedKey(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: 95, prefix: "ld-default1", scopes: ["chat:write"], created: true, rotated: false,
    issued_at: "2026-11-10T02:00:00Z", status: 1, expired_at: "2027-05-09T02:00:00Z",
    amount_limit: 0, used: 0, rpm_limit: 0, previous_expires_at: null,
    ...over,
  };
}

async function stubHosted(page: Page, key: Record<string, unknown> = BLOCKED_KEY): Promise<void> {
  await page.route("**/api/pricing", (route) => route.fulfill({ json: envelope(PRICING) }));
  await page.route("**/api/portal/keys**", (route) =>
    route.fulfill({ json: envelope({ items: [key] }) }),
  );
  await page.route("**/api/portal/apps/demo-app/hosted-key", (route) =>
    route.fulfill({
      json: envelope({
        app: { id: 7, slug: "demo-app", name: "演示应用" },
        key: {
          id: key.id, prefix: "ld-hosted1", scopes: [], created: false, rotated: true,
          issued_at: "2026-11-03T00:00:00Z", status: 1, expired_at: "2027-05-01T00:00:00Z",
          amount_limit: 100000000, used: 500000, rpm_limit: 600,
          previous_expires_at: "2026-11-06T00:00:00Z",
        },
        plaintext: ROTATE_PLAIN,
      }),
    }),
  );
}

const card = (page: Page) => page.locator(".key-card");

test.describe("S3 API 密钥 · 托管密钥卡片", () => {
  test("封禁托管卡：无任何危险按钮，也不给注定失败的轮换", async ({ page }) => {
    await seedAuth(page);
    await stubHosted(page);
    await page.goto("/keys");

    await expect(card(page)).toHaveCount(1);
    await expect(card(page).getByText("应用托管")).toBeVisible();
    // 封禁态文案必须与"闲置超期"区分：后者轮换即恢复，前者必须找平台。
    await expect(card(page).getByText("已被平台封禁")).toBeVisible();
    // 危险/无效按钮一律不出现（点了也只会 409）
    for (const label of ["编辑", "重置", "删除", "禁用", "启用"]) {
      await expect(card(page).getByRole("button", { name: label, exact: true })).toHaveCount(0);
    }
    // 轮换也不行：封禁钥轮换不解除，摆一个注定失败的按钮只会让人反复点。
    await expect(card(page).getByRole("button", { name: "轮换", exact: true })).toHaveCount(0);
    // 明文不可取：无 reveal/copy
    await expect(card(page).getByRole("button", { name: "显示完整密钥" })).toHaveCount(0);
    await expect(card(page).getByRole("button", { name: "复制完整密钥" })).toHaveCount(0);
    // 权限点收敛展示（最多 3 个 + 等 N 项）
    await expect(card(page).getByText("等 4 项权限")).toBeVisible();
  });

  test("闲置超期：保留轮换（这是恢复手段）", async ({ page }) => {
    await seedAuth(page);
    await stubHosted(page, EXPIRED_KEY);
    await page.goto("/keys");

    await expect(card(page).getByText("长期未使用已停用")).toBeVisible();
    await expect(card(page).getByRole("button", { name: "轮换", exact: true })).toBeVisible();
  });

  test("轮换：确认 → 新一次性明文弹窗", async ({ page }) => {
    await seedAuth(page);
    await stubHosted(page, ACTIVE_KEY);
    await page.goto("/keys");

    await card(page).getByRole("button", { name: "轮换", exact: true }).click();
    const confirm = page.getByRole("dialog", { name: "轮换托管密钥" });
    await expect(confirm).toBeVisible();
    await confirm.getByRole("button", { name: "确认轮换", exact: true }).click();

    const secret = page.getByRole("dialog", { name: "保存托管密钥" });
    await expect(secret.locator(".key-reveal .kv > span")).toHaveText(ROTATE_PLAIN);
    // 旧密钥的重叠窗口必须回显：用户据此排期替换，而不是等应用 401 才发现
    await expect(page.getByText(/托管密钥已轮换，旧密钥可用至/)).toBeVisible();
  });
});

test.describe("S3 API 密钥 · 接入应用（换取托管密钥）", () => {
  /** 打开发起弹窗并填入标识。 */
  async function openIssue(page: Page, slug: string): Promise<void> {
    await page.goto("/keys");
    await page.getByRole("button", { name: "接入应用", exact: true }).click();
    const dlg = page.getByRole("dialog", { name: "接入应用" });
    await expect(dlg).toBeVisible();
    await dlg.getByLabel("应用标识").fill(slug);
    await dlg.getByRole("button", { name: "换取密钥", exact: true }).click();
  }

  test("首次接入：POST 体是 rotate=false，明文只回一次", async ({ page }) => {
    await seedAuth(page);
    await stubHosted(page);
    let body: unknown = null;
    await page.route("**/api/portal/apps/default/hosted-key", async (route) => {
      body = route.request().postDataJSON();
      await route.fulfill({
        json: envelope({
          app: { id: 9, slug: "default", name: "默认应用" },
          key: issuedKey(),
          plaintext: ISSUE_PLAIN,
        }),
      });
    });

    // 前后空白必须被 trim：用户从文档里复制经常带空格。
    await openIssue(page, "  default  ");

    const secret = page.getByRole("dialog", { name: "保存托管密钥" });
    await expect(secret.locator(".key-reveal .kv > span")).toHaveText(ISSUE_PLAIN);
    // 关键契约：不能带 rotate=true，否则等于调用方自己声明"我不是新用户"。
    expect(body).toEqual({ rotate: false });
  });

  test("已持有：只提示不弹空明文（否则会把空串当密钥复制走）", async ({ page }) => {
    await seedAuth(page);
    await stubHosted(page);
    await page.route("**/api/portal/apps/default/hosted-key", (route) =>
      route.fulfill({
        json: envelope({
          app: { id: 9, slug: "default", name: "默认应用" },
          key: issuedKey({ created: false, rotated: false }),
          plaintext: "",
        }),
      }),
    );

    await openIssue(page, "default");

    await expect(page.getByText(/你已经持有「默认应用」的托管密钥/)).toBeVisible();
    await expect(page.getByRole("dialog", { name: "保存托管密钥" })).toHaveCount(0);
  });

  test("移动端 375px：页头两个动作不撑出横向滚动", async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 760 });
    await seedAuth(page);
    await stubHosted(page, ACTIVE_KEY);
    await page.goto("/keys");

    // 页头从 1 个动作变 2 个（接入应用 + 创建 API KEY），375px 下最容易撑破；
    // 撑破时 title 会被压成竖排，看起来像"页面坏了"而不是"按钮多了"。
    await expect(page.getByRole("button", { name: "接入应用", exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "创建 API KEY", exact: true })).toBeVisible();
    await expectNoHScroll(page, "移动端密钥页页头动作溢出");
  });

  test("关闭自助接入：给出「联系运营」出口，不留在原地重试", async ({ page }) => {
    await seedAuth(page);
    await stubHosted(page);
    await page.route("**/api/portal/apps/locked-app/hosted-key", (route) =>
      route.fulfill({
        status: 403,
        json: envelope(null, 403, "禁止访问", "forbidden"),
      }),
    );

    await openIssue(page, "locked-app");

    await expect(page.getByText("该应用已关闭自助接入，请联系运营获取接入凭证")).toBeVisible();
    await expect(page.getByRole("dialog", { name: "保存托管密钥" })).toHaveCount(0);
  });
});
