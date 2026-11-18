import { expect, test, type Page } from "@playwright/test";
import { envelope, PORTAL_CONFIG, seedAuth } from "./helpers";

/**
 * S22 运营模式（纯服务端）：门户站点关闭后，页面与守卫的表现。
 *
 * 全部走 stub：这个用例守的是**前端路由决策**，不是后端闸门
 * （后端的四条边界由 internal/router/portal_mode_test.go 守）。
 *
 * 桩件顺序：Playwright 后注册优先，所以 `seedAuth` 的门户配置桩必须
 * 被下面的 `portal_enabled:false` 覆盖 —— 先 seedAuth，再注册关闭态。
 */
async function closedPortal(page: Page, role: 0 | 1 = 0) {
  await seedAuth(page, { role, username: role === 0 ? "root" : "member" });
  await page.route("**/api/portal/config**", (route) =>
    route.fulfill({ json: envelope({ ...PORTAL_CONFIG, portal_enabled: false }) }),
  );
  // 管理台兜底：落地到 /admin/dashboard 后页面会打一批 /api/admin/*，
  // 不通桩就会 401 → 全局登出 → 断言看到的是登录页。
  await page.route("**/api/admin/**", (route) => route.fulfill({ json: envelope(null) }));
}

test.describe("纯服务端模式（门户关闭）", () => {
  test("匿名进 /home 被送回登录页", async ({ page }) => {
    await closedPortal(page, 1);
    // 匿名 = /portal/me 401：单独覆盖成 401，seedAuth 默认是「已登录」
    await page.route("**/api/portal/me", (route) =>
      route.fulfill({ status: 401, json: { code: 401, message: "未授权", error_code: "unauthorized", data: null } }),
    );
    await page.goto("/home");
    await expect(page).toHaveURL(/\/login/);
  });

  test("注册入口关闭：/register 直接回登录页", async ({ page }) => {
    await closedPortal(page, 0);
    await page.goto("/register");
    await expect(page).toHaveURL(/\/login/);
  });

  test("管理员访问门户页被送到管理台", async ({ page }) => {
    await closedPortal(page, 0);
    await page.goto("/home");
    await expect(page).toHaveURL(/\/admin\/dashboard/);
  });

  test("存量会员的会话被送回登录页", async ({ page }) => {
    await closedPortal(page, 1);
    await page.goto("/keys");
    await expect(page).toHaveURL(/\/login/);
  });

  test("两步验证绑定页仍可达（强制管理员绑定时的唯一自救通道）", async ({ page }) => {
    await closedPortal(page, 0);
    await page.goto("/mfa/setup");
    // 不放行这条，会形成「403 mfa_setup_required → 绑定页 → 重定向 → 403」的死循环
    await expect(page).toHaveURL(/\/mfa\/setup/);
    await expect(page.getByRole("heading", { name: "两步验证" })).toBeVisible();
  });

  test("公开内容页保持可访问", async ({ page }) => {
    await closedPortal(page, 1);
    await page.goto("/about");
    await expect(page).toHaveURL(/\/about/);
    await page.goto("/docs");
    await expect(page).toHaveURL(/\/docs/);
  });

  test("登录页收起注册入口并说明当前模式", async ({ page }) => {
    await closedPortal(page, 1);
    await page.goto("/login");
    await expect(page.getByText("当前为纯服务端模式，仅管理员可登录")).toBeVisible();
    await expect(page.getByRole("link", { name: "立即注册" })).toHaveCount(0);
    // 忘记密码是管理员的唯一无会话自救入口，必须保留
    await expect(page.getByRole("link", { name: "忘记密码？" })).toBeVisible();
  });

  test("管理员登录后直接落管理台", async ({ page }) => {
    await closedPortal(page, 0);
    await page.route("**/api/portal/auth/login", (route) =>
      route.fulfill({
        json: envelope({
          user: { id: 1, username: "root", role: 0, group: "default", balance: 0, email: "", phone: "" },
          expires_in: 604_800,
        }),
      }),
    );

    await page.goto("/login");
    await page.fill("#ld-account", "root");
    await page.fill("#ld-password", "Passw0rd!");
    await page.getByRole("button", { name: "登 录" }).click();
    await expect(page).toHaveURL(/\/admin\/dashboard/);
  });

  test("登录回跳只接受管理台内部路径", async ({ page }) => {
    await closedPortal(page, 0);
    await page.route("**/api/portal/auth/login", (route) =>
      route.fulfill({
        json: envelope({
          user: { id: 1, username: "root", role: 0, group: "default", balance: 0, email: "", phone: "" },
          expires_in: 604_800,
        }),
      }),
    );

    // ?redirect=/home 是门户守卫可能塞进来的：关闭态下不能照单全收，
    // 否则落地即被守卫再弹一次（用户看到两次跳转）。
    await page.goto("/login?redirect=%2Fhome");
    await page.fill("#ld-account", "root");
    await page.fill("#ld-password", "Passw0rd!");
    await page.getByRole("button", { name: "登 录" }).click();
    await expect(page).toHaveURL(/\/admin\/dashboard/);
  });
});
