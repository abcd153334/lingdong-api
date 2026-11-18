/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, PORTAL_CONFIG, seedAuth } from "./helpers";

/** S19 静态内容页（/about /agreement）：公开可读、配置正文渲染、空占位、页脚与注册页链接接线 */

/** 带正文的门户配置（空行分段两段，段内换行保留） */
const CONFIG_WITH_CONTENT = {
  ...PORTAL_CONFIG,
  about_content: "第一段关于内容\n换行保留\n\n第二段关于内容",
  agreement_content: "协议第一条：服务说明\n\n协议第二条：责任边界",
};

async function stubConfig(page: Page, config: typeof PORTAL_CONFIG): Promise<void> {
  await page.route("**/api/portal/config**", (route) => route.fulfill({ json: envelope(config) }));
}

test.describe("S19 静态内容页", () => {
  test("未登录访问 /about 渲染配置正文（空行分段）", async ({ page }) => {
    await stubConfig(page, CONFIG_WITH_CONTENT);
    await page.goto("/about");
    await expect(page.getByRole("heading", { name: "关于" })).toBeVisible();
    await expect(page.getByText("第一段关于内容")).toBeVisible();
    await expect(page.getByText("第二段关于内容")).toBeVisible();
    await expect(page.getByText("换行保留")).toBeVisible();
  });

  test("未登录访问 /about 未配置时显示尚未配置占位", async ({ page }) => {
    await stubConfig(page, PORTAL_CONFIG);
    await page.goto("/about");
    await expect(page.getByText("站点尚未配置关于页内容")).toBeVisible();
  });

  test("未登录访问 /agreement 渲染正文、生效日期与空占位分支", async ({ page }) => {
    await stubConfig(page, CONFIG_WITH_CONTENT);
    await page.goto("/agreement");
    await expect(page.getByRole("heading", { name: "用户协议" })).toBeVisible();
    const effDate = page.locator(".eff-date");
    await expect(effDate).toContainText("生效日期");
    await expect(effDate).toContainText("2026-10-01");
    const [docBox, effBox] = await Promise.all([
      page.locator(".doc").boundingBox(),
      effDate.boundingBox(),
    ]);
    expect(docBox && effBox && effBox.y >= docBox.y + docBox.height).toBe(true);
    await expect(page.getByText("协议第一条：服务说明")).toBeVisible();
    await expect(page.getByText("协议第二条：责任边界")).toBeVisible();
  });

  test("/agreement 未配置时显示尚未配置占位", async ({ page }) => {
    await stubConfig(page, PORTAL_CONFIG);
    await page.goto("/agreement");
    await expect(page.getByText("站点尚未配置用户协议内容")).toBeVisible();
  });

  test("注册页《用户协议》链接跳转 /agreement（不触发勾选框）", async ({ page }) => {
    await stubConfig(page, CONFIG_WITH_CONTENT);
    await page.goto("/register");
    await page.getByRole("link", { name: "《用户协议》" }).click();
    await expect(page).toHaveURL(/\/agreement$/);
    await expect(page.getByRole("heading", { name: "用户协议" })).toBeVisible();
  });

  test("门户页脚「关于我们 / 用户协议」链接可达", async ({ page }) => {
    await seedAuth(page);
    await page.goto("/home");
    await page.getByRole("link", { name: "关于我们" }).click();
    await expect(page).toHaveURL(/\/about$/);
    await expect(page.getByRole("heading", { name: "关于" })).toBeVisible();

    await page.goBack();
    await page.getByRole("link", { name: "用户协议" }).click();
    await expect(page).toHaveURL(/\/agreement$/);
    await expect(page.getByRole("heading", { name: "用户协议" })).toBeVisible();
  });
});
