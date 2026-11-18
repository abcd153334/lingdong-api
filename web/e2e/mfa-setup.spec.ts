/** S20 契约级（stub）用例：page.route 打桩后端响应；真实后端联调见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

const SECRET = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ";

/** 绑定页的状态桩件（setup/enable 由各用例自己打，因为要断言请求体） */
async function stubMfa(page: Page, opts: { enabled?: boolean } = {}): Promise<void> {
  await page.route("**/api/portal/auth/mfa/status", (route) =>
    route.fulfill({
      json: envelope({
        enabled: opts.enabled ?? false,
        policy: "optional",
        backup_codes_left: opts.enabled ? 3 : 0,
      }),
    }),
  );
}

test.describe("两步验证绑定页", () => {
  test("生成密钥 → 扫不出码可手抄 → 输码开启 → 备份码只显示一次", async ({ page }) => {
    await seedAuth(page);
    await stubMfa(page);
    const enableBodies: unknown[] = [];
    await page.route("**/api/portal/auth/mfa/enable", (route) => {
      enableBodies.push(route.request().postDataJSON());
      return route.fulfill({
        json: envelope({
          backup_codes: ["ABCDE-FGHJK", "CDEFG-HJKLM", "EFGHJ-KLMNP", "GHJKL-MNPQR", "JKLMN-PQRST"],
        }),
      });
    });

    await page.goto("/mfa/setup");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("两步验证");

    // 未生成密钥前不该出现输入框（避免用户对着空 secret 输码）
    await expect(page.locator("#mfa-code")).toHaveCount(0);

    // 没有密码就不发请求：绑定验证器是重认证操作（防「拿到一个已登录的浏览器」）
    const setupBodies: unknown[] = [];
    await page.route("**/api/portal/auth/mfa/setup", (route) => {
      setupBodies.push(route.request().postDataJSON());
      return route.fulfill({
        json: envelope({
          secret: SECRET,
          otpauth_uri: `otpauth://totp/%E7%81%B5%E5%8A%A8:alice?secret=${SECRET}&issuer=x&digits=6&period=30`,
        }),
      });
    });
    await page.getByRole("button", { name: "生成密钥" }).click();
    await expect(page.getByText("请先输入当前账户密码")).toBeVisible();
    expect(setupBodies).toHaveLength(0);

    await page.locator("#mfa-password").fill("Passw0rd!");
    await page.getByRole("button", { name: "生成密钥" }).click();
    await expect.poll(() => setupBodies.length).toBe(1);
    expect(setupBodies[0]).toEqual({ password: "Passw0rd!" });

    // 密钥必须可见可抄：二维码扫不出（相机坏、桌面端没手机）时这是唯一出路
    await expect(page.locator("#mfa-secret")).toHaveValue(SECRET);

    // 空码不发请求
    await page.getByRole("button", { name: "确认开启" }).click();
    await expect(page.getByText("请输入身份验证器上的 6 位动态码")).toBeVisible();
    expect(enableBodies).toHaveLength(0);

    await page.locator("#mfa-code").fill("123456");
    await page.getByRole("button", { name: "确认开启" }).click();
    await expect.poll(() => enableBodies.length).toBe(1);
    expect(enableBodies[0]).toEqual({ code: "123456", password: "Passw0rd!" });

    // 备份码只出现这一次：没勾选「我已保存」时「完成」必须不可点
    await expect(page.getByText("ABCDE-FGHJK")).toBeVisible();
    const done = page.getByRole("button", { name: "完成" });
    await expect(done).toBeDisabled();
    await page.getByRole("checkbox").check();
    await expect(done).toBeEnabled();
    await done.click();
    await expect(page).toHaveURL(/\/home/);
  });

  test("已开启时直接显示状态，不重复提供绑定入口", async ({ page }) => {
    await seedAuth(page);
    await stubMfa(page, { enabled: true });

    await page.goto("/mfa/setup");
    await expect(page.getByText("已开启")).toBeVisible();
    await expect(page.getByText(/剩余备份码 3 个/)).toBeVisible();
    await expect(page.getByRole("button", { name: "生成密钥" })).toHaveCount(0);
  });

  test("策略要求管理员绑定时给出说明（否则用户不知道为什么被拦）", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await page.route("**/api/portal/auth/mfa/status", (route) =>
      route.fulfill({
        json: envelope({ enabled: false, policy: "required_for_admin", backup_codes_left: 0 }),
      }),
    );

    await page.goto("/mfa/setup");
    await expect(page.getByText(/站点要求管理员开启两步验证/)).toBeVisible();
  });
});
