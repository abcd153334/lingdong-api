/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Locator } from "@playwright/test";
import { envelope, expectNoHScroll, seedAuth } from "./helpers";

test.describe("门户 chrome（桌面）", () => {
  test("顶栏导航齐全、当前页高亮、移动 Tab Bar 隐藏", async ({ page }) => {
    await seedAuth(page);
    await page.goto("/home");
    await expect(page.locator(".p-top")).toBeVisible();
    await expect(page.locator(".p-nav a")).toHaveCount(6);
    await expect(page.locator(".p-nav a.active")).toHaveText("主页");
    await expect(page.locator(".p-tabbar")).toBeHidden();
    // 主页页头为动态问候语（原型 home.html 语义），不再是导航名
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(/，e2e$/);
  });

  test("顶部导航可完成 SPA 跳转并同步高亮", async ({ page }) => {
    await seedAuth(page);
    await page.goto("/home");
    await page.locator(".p-nav a", { hasText: "模型广场" }).click();
    await expect(page).toHaveURL(/\/models$/);
    await expect(page.locator(".p-nav a.active")).toHaveText("模型广场");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("模型广场");
  });

  test("导航 hover 与选中是同一种形态：同一色块，只差文字色", async ({ page }) => {
    await seedAuth(page);
    await page.goto("/home");
    const active = page.locator(".p-nav a.active");
    const other = page.locator(".p-nav a", { hasText: "模型广场" });
    const bg = (loc: Locator) => loc.evaluate((el) => getComputedStyle(el).backgroundColor);
    const color = (loc: Locator) => loc.evaluate((el) => getComputedStyle(el).color);

    // 选中项本身就是色块（旧实现是个透明底 + 悬在下沿的渐变线，与 hover 两套语言）
    const activeBg = await bg(active);
    expect(activeBg).not.toBe("rgba(0, 0, 0, 0)");
    const activeColor = await color(active);
    // 选中项不得再挂第二条指示器：色块 + 下划线同时存在时，「当前页」反而不像被选中
    const after = await active.evaluate((el) => getComputedStyle(el, "::after").content);
    expect(["none", "normal"]).toContain(after);

    // hover 与选中同底色（transition 0.18s，用 poll 等过渡结束）
    await other.hover();
    await expect.poll(() => bg(other), { message: "hover 底色应与选中一致" }).toBe(activeBg);
    // 但文字色必须不同，否则选中态就没有可辨识标记
    await expect
      .poll(() => color(other), { message: "hover 文字色必须区别于选中" })
      .not.toBe(activeColor);
  });

  test("钱包余额入口与公告铃铛、主题入口均可用", async ({ page }) => {
    await seedAuth(page);
    await page.goto("/home");
    await expect(page.locator(".balance-pill")).toBeVisible();
    await expect(page.getByRole("button", { name: /公告/ })).toBeVisible();
    await expect(page.getByRole("button", { name: /深色主题/ })).toBeVisible();
  });

  test("顶栏导航每项带图标；文档入口移到页脚一行（与关于我们、用户协议同处）", async ({
    page,
  }) => {
    await seedAuth(page);
    await page.goto("/home");

    // 图标与文字一一对应：6 项导航 6 个 svg（少一个就是漏传 icon 或渲染失败）
    await expect(page.locator(".p-nav a")).toHaveCount(6);
    await expect(page.locator(".p-nav a svg")).toHaveCount(6);
    // 文档不再占顶栏宽度
    await expect(page.locator(".p-nav a", { hasText: "文档" })).toHaveCount(0);

    const foot = page.locator(".p-foot-links");
    await expect(foot.locator("a")).toHaveCount(3);
    await expect(foot.getByRole("link", { name: "API 文档" })).toHaveAttribute("href", "/docs");
    await expect(foot.getByRole("link", { name: "关于我们" })).toHaveAttribute("href", "/about");
    await expect(foot.getByRole("link", { name: "用户协议" })).toHaveAttribute(
      "href",
      "/agreement",
    );
  });
});

test.describe("门户 chrome（1024px 窄桌面）", () => {
  test.use({ viewport: { width: 1024, height: 800 } });

  test("≤1199px 顶栏图标让位隐藏，6 项导航不挤出横向滚动条", async ({ page }) => {
    await seedAuth(page);
    await page.goto("/home");
    // 图标是装饰：1024px 下带图标时导航可用宽度刚好被占满（余量 0），
    // 站点名一长就会把顶栏挤出横向滚动条 ⇒ 该档只留文字。
    await expect(page.locator(".p-nav a")).toHaveCount(6);
    await expect(page.locator(".p-nav a svg").first()).toBeHidden();
    await expectNoHScroll(page);
  });
});

test.describe("门户 ↔ 管理台互通（S14）", () => {
  test("T1 管理员在门户用户菜单可见「切到管理台」并跳转仪表盘", async ({ page }) => {
    await seedAuth(page, { role: 0, username: "admin" });
    await page.goto("/home");
    await page.getByRole("button", { name: "用户菜单" }).click();
    const entry = page.getByRole("menuitem", { name: /切到管理台/ });
    await expect(entry).toBeVisible();
    await entry.click();
    await expect(page).toHaveURL(/\/admin\/dashboard$/);
  });

  test("T2 普通用户门户不存在管理台入口（DOM 无该节点）", async ({ page }) => {
    await seedAuth(page, { role: 1 });
    await page.goto("/home");
    await page.getByRole("button", { name: "用户菜单" }).click();
    await expect(page.getByRole("menuitem", { name: /切到管理台/ })).toHaveCount(0);
    await expect(page.locator('.p-tabbar a[href="/admin/dashboard"]')).toHaveCount(0);
  });

  test("T4 普通用户直访管理台被守卫重定向", async ({ page }) => {
    await seedAuth(page, { role: 1 });
    await page.goto("/admin/dashboard");
    await expect(page).toHaveURL(/\/login/);
  });
});

test.describe("门户 ↔ 管理台互通（移动端 375px）", () => {
  test.use({ viewport: { width: 375, height: 780 } });

  test("T3 管理员移动端 Tab 含「管理台」且触控目标 ≥44px、无横向滚动", async ({ page }) => {
    await seedAuth(page, { role: 0, username: "admin" });
    await page.goto("/home");
    const tabs = page.locator(".p-tabbar a");
    await expect(tabs).toHaveCount(6);
    await expect(tabs.filter({ hasText: "管理台" })).toBeVisible();

    const heights = await tabs.evaluateAll((els) =>
      els.map((e) => e.getBoundingClientRect().height),
    );
    expect(Math.min(...heights)).toBeGreaterThanOrEqual(44);

    await expectNoHScroll(page);
  });
});

test.describe("门户 chrome（移动端 375px）", () => {
  test.use({ viewport: { width: 375, height: 780 } });

  test("底部 Tab Bar 显示、5 项、触控目标 ≥44px 且无横向滚动", async ({ page }) => {
    await seedAuth(page);
    await page.goto("/home");
    await expect(page.locator(".p-nav")).toBeHidden();
    await expect(page.locator(".p-tabbar")).toBeVisible();
    await expect(page.locator(".p-tabbar a")).toHaveCount(5);
    await expect(page.locator(".p-tabbar a.active")).toContainText("主页");

    const heights = await page
      .locator(".p-tabbar a")
      .evaluateAll((els) => els.map((e) => e.getBoundingClientRect().height));
    expect(Math.min(...heights)).toBeGreaterThanOrEqual(44);

    await expectNoHScroll(page);
  });

  test("Tab Bar 可跳转到「我的」（mobile-only 导航项）", async ({ page }) => {
    await seedAuth(page);
    // 用户中心会拉取 /portal/me：伪造会话下真实接口必然 401 并触发全局登出跳 /login，
    // 故此处打桩，使断言与后端可达性无关（否则该用例只在后端不可达时才通过）。
    await page.route("**/api/portal/me", (route) =>
      route.fulfill({
        json: envelope({
          id: 1, username: "e2e", role: 1, status: 1, group: "default",
          balance: 0, frozen: 0, used: 0, request_count: 0,
          key_count: 0, key_count_enabled: 0,
          group_discount: 1_000_000, group_configured: false,
          email: "", phone: "",
        }),
      }),
    );
    await page.goto("/home");
    await page.locator(".p-tabbar a", { hasText: "我的" }).click();
    await expect(page).toHaveURL(/\/settings$/);
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("用户中心");
  });
});
