import { expect, test } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

/**
 * 管理台「账户设置」——现在是**头像菜单里的弹窗**，`/admin/account` 只是它的落地页。
 *
 * 守三件事：
 *   1. 侧栏不再有这一项，入口在用户菜单里；
 *   2. 弹窗按「资料 / 账户安全 / 登录设备」分区，身份信息只读；
 *   3. 临时口令态只暴露「改密」—— 其余入口后端一律 403，摆在界面上就是死按钮。
 */
test.describe("管理台账户设置（弹窗）", () => {
  test("从头像菜单打开弹窗，三分区可切换，身份只读", async ({ page }) => {
    await seedAuth(page, { role: 0, username: "root" });
    await page.goto("/admin/dashboard");

    // 入口已从侧栏移到用户菜单
    await expect(page.locator(".a-side nav a", { hasText: "账户设置" })).toHaveCount(0);
    await page.getByRole("button", { name: "用户菜单" }).click();
    await page.getByRole("menuitem", { name: "账户设置" }).click();

    const dlg = page.getByRole("dialog", { name: "账户设置" });
    await expect(dlg).toBeVisible();

    // 默认分区：资料（头像 + 身份 + 联系方式）
    await expect(dlg.getByText("计费分组")).toBeVisible();
    await expect(dlg.locator(".uc-row", { hasText: "用户名" }).locator("input")).toHaveCount(0);
    await expect(dlg.locator(".avatar-pick__btn")).toBeVisible();

    await dlg.getByRole("button", { name: "账户安全" }).click();
    await expect(dlg.getByText("登录密码")).toBeVisible();

    await dlg.getByRole("button", { name: "登录设备" }).click();
    await expect(dlg.getByTestId("device-list")).toBeVisible();

    // 关闭后回到原页面（不是被带去仪表盘）
    await dlg.getByRole("button", { name: "关闭" }).click();
    await expect(page).toHaveURL(/\/admin\/dashboard/);
    await expect(page.getByRole("dialog", { name: "账户设置" })).toBeHidden();
  });

  test("改密抽屉可用（与用户中心同一实现），Esc 只关抽屉", async ({ page }) => {
    await seedAuth(page, { role: 0, username: "root" });
    await page.goto("/admin/account");

    const dlg = page.getByRole("dialog", { name: "账户设置" });
    await expect(dlg).toBeVisible();
    await dlg.getByRole("button", { name: "账户安全" }).click();
    await dlg.getByRole("button", { name: "修改" }).click();

    await expect(page.locator("#pw-old")).toBeVisible();
    await expect(page.locator("#pw-new")).toBeVisible();
    await expect(page.locator("#pw-new2")).toBeVisible();

    // 两层弹层：Esc 只关最上层的抽屉，弹窗仍在（overlay 栈的验收点）
    await page.keyboard.press("Escape");
    await expect(page.locator("#pw-old")).toBeHidden();
    await expect(dlg).toBeVisible();
  });

  test("临时口令态只留改密，且不请求公告", async ({ page }) => {
    await seedAuth(page, { role: 0, username: "root" });
    // seedAuth 的 me 桩件不带该字段：这里覆盖成「初始口令未改」
    await page.route("**/api/portal/me", (route) =>
      route.fulfill({
        json: envelope({
          id: 1,
          username: "root",
          role: 0,
          status: 1,
          group: "default",
          balance: 0,
          frozen: 0,
          used: 0,
          request_count: 0,
          key_count: 0,
          key_count_enabled: 0,
          group_discount: 1_000_000,
          group_configured: false,
          email: "",
          phone: "",
          must_change_password: true,
        }),
      }),
    );

    // 任意管理台页面都会被守卫送到账户页，弹窗自动打开
    await page.goto("/admin/dashboard");
    await expect(page).toHaveURL(/\/admin\/account/);
    // 抽屉自动打开：临时口令的唯一可用操作就是改密
    await expect(page.locator("#pw-old")).toBeVisible();

    // 分区导航与关闭按钮都不该出现（无处可去 + 关掉也会被守卫弹回来）
    const dlg = page.getByRole("dialog", { name: "账户设置" });
    await expect(dlg.getByRole("button", { name: "账户安全" })).toHaveCount(0);
    await expect(dlg.getByRole("button", { name: "关闭" })).toHaveCount(0);
    // 头像入口在临时口令态是必 403 的死按钮，不渲染
    await expect(dlg.locator(".avatar-pick__btn")).toHaveCount(0);

    // 设备列表 / 两步验证 在这个状态下后端全 403，不能渲染
    await expect(page.getByTestId("device-list")).toHaveCount(0);
    await expect(page.getByTestId("mfa-card")).toHaveCount(0);
    // 公告铃同理（/api/portal/announcements 必 403）
    await expect(page.getByRole("button", { name: /站点公告/ })).toHaveCount(0);
  });
});
