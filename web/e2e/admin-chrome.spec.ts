/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, expectNoHScroll, seedAuth } from "./helpers";

/** 管理台路由守卫要求已登录且 role=0 */
const seedAdmin = (page: Parameters<typeof seedAuth>[0]) => seedAuth(page, { role: 0 });

/** S10 起系统配置页为真实数据页；chrome 断言只需拦截真实的 401（否则被全局登出） */
async function stubAdminApi(page: Page): Promise<void> {
  await page.route("**/api/admin/**", (route) => route.fulfill({ json: envelope(null) }));
}

test.describe("管理台 chrome（桌面）", () => {
  test("侧栏 15 项（4 组 + 3 顶级项）按分区渲染，当前项高亮，面包屑同步", async ({ page }) => {
    await seedAdmin(page);
    await page.goto("/admin/dashboard");
    await expect(page.locator(".a-side")).toBeVisible();
    await expect(page.locator(".a-side nav a")).toHaveCount(15);
    // 组头 4 个（仪表盘/系统配置/模型服务是顶级项，不画标题）；7 个分区各占一个 .nav-sec
    await expect(page.locator(".a-side .nav-group")).toHaveCount(4);
    await expect(page.locator(".a-side nav .nav-sec")).toHaveCount(7);

    // 层级由「结构」表达，不靠缩小字号：所有行仍是 40px / 13.5px 的同一规格，
    // 组头只多一个 600 字重（+ 折叠箭头），父子关系靠子项缩进与 1px 导轨。
    // 曾经两套声明逐字相同 ⇒ 展开后子项与组头完全平级（用户判定「看不出谁是谁的子项」）。
    const ui = await page.evaluate(() => {
      const group = document.querySelector<HTMLElement>(".a-side .nav-group")!;
      // 取**非当前项**做基线：选中态的 600 字重是另一回事，量到它会把断言变成假绿/假红
      const top = document.querySelector<HTMLElement>('.a-side nav a[href="/admin/config"]')!;
      // 子项取分组内的非当前项：模型服务已是**顶级项**（不再缩进），不能再拿它当"子"样本
      const child = document.querySelector<HTMLElement>('.a-side .nav-sub a[href="/admin/users"]')!;
      const active = document.querySelector<HTMLElement>(".a-side nav a.active")!;
      const sub = document.querySelector<HTMLElement>(".a-side .nav-sub")!;
      const gs = getComputedStyle(group);
      const ts = getComputedStyle(top);
      const cs = getComputedStyle(child);
      // 图标左边界：缩进是子项唯一的位置信号，只能量几何
      const iconLeft = (el: HTMLElement) =>
        Math.round(el.querySelector("svg")!.getBoundingClientRect().left);
      return {
        rowH: [group, top, child].map((el) => Math.round(el.getBoundingClientRect().height)),
        rowFont: [gs.fontSize, ts.fontSize, cs.fontSize],
        groupWeight: Number(gs.fontWeight),
        itemWeight: Number(ts.fontWeight),
        iconIndent: iconLeft(child) - iconLeft(group),
        topIndent: iconLeft(top) - iconLeft(group),
        subBorder: getComputedStyle(sub).borderLeftWidth,
        // 每个组头 = 1 个语义图标 + 1 个折叠箭头
        headerSvgs: [...document.querySelectorAll(".a-side .nav-group")].map(
          (h) => h.querySelectorAll("svg").length,
        ),
        itemSvgCounts: [...document.querySelectorAll(".a-side nav a")].map(
          (a) => a.querySelectorAll("svg").length,
        ),
        activeShadow: getComputedStyle(active).boxShadow,
        activeBg: getComputedStyle(active).backgroundColor,
      };
    });
    expect(ui.headerSvgs).toEqual([2, 2, 2, 2]);
    expect(ui.itemSvgCounts.every((n) => n === 1), "菜单项应恒带 1 个图标").toBe(true);
    expect(new Set(ui.rowH), "所有行必须同一行高").toEqual(new Set([40]));
    expect(new Set(ui.rowFont), "所有行必须同一字号").toEqual(new Set(["13.5px"]));
    expect(ui.groupWeight, "组头没有与菜单项拉开字重").toBeGreaterThanOrEqual(600);
    expect(ui.itemWeight, "菜单项不该被加粗（会与组头混淆）").toBeLessThan(600);
    expect(ui.iconIndent, "子项没有缩进，展开后看起来仍与组头平级").toBeGreaterThanOrEqual(16);
    expect(ui.topIndent, "顶级项被误缩进了（它必须是同级入口）").toBe(0);
    expect(ui.subBorder, "缩进容器缺 1px 导轨").toBe("1px");
    expect(ui.activeShadow, "高亮项又画上左侧竖条了").toBe("none");
    expect(ui.activeBg, "高亮项丢了底色").not.toBe("rgba(0, 0, 0, 0)");
    await expect(page.locator(".a-side nav a.active")).toContainText("仪表盘");
    await expect(page.locator(".a-top .crumb b")).toHaveText("仪表盘");
    // 常驻侧栏不是弹层：标成 dialog/modal 会让读屏把页面其余内容判为不可达，
    // inert 则会让导航整体不可点。两者都只在 ≤767px 的抽屉态出现。
    await expect(page.locator(".a-side")).not.toHaveAttribute("role", "dialog");
    expect(await page.locator(".a-side").evaluate((el) => (el as HTMLElement).inert)).toBe(false);
    await expect(page.locator(".mask")).toHaveCount(0);
  });

  test("侧栏跳转后高亮与面包屑同步更新", async ({ page }) => {
    await seedAdmin(page);
    await stubAdminApi(page);
    await page.goto("/admin/dashboard");
    await page.locator(".a-side nav a", { hasText: "系统配置" }).click();
    await expect(page).toHaveURL(/\/admin\/config$/);
    await expect(page.locator(".a-side nav a.active")).toContainText("系统配置");
    await expect(page.locator(".a-top .crumb b")).toHaveText("系统配置");
  });

  test("用户菜单含返回门户入口", async ({ page }) => {
    await seedAdmin(page);
    await page.goto("/admin/dashboard");
    await page.getByRole("button", { name: "用户菜单" }).click();
    await expect(page.locator(".p-user.open .menu")).toBeVisible();
    await expect(page.locator(".p-user.open .menu a", { hasText: "返回门户" })).toBeVisible();
  });

  test("分组可折叠：链接隐藏但仍在 DOM，刷新保持，进入其页面自动展开", async ({ page }) => {
    await seedAdmin(page);
    await stubAdminApi(page);
    await page.goto("/admin/dashboard");

    const head = page.getByRole("button", { name: "用户与应用" });
    const modelLink = page.locator('.a-side nav a[href="/admin/users"]');
    await expect(head).toHaveAttribute("aria-expanded", "true");
    await expect(modelLink).toBeVisible();

    await head.click();
    await expect(head).toHaveAttribute("aria-expanded", "false");
    // 关键：节点不能删（删了 15 项计数断言与跳转后高亮全废），只是不可见
    await expect(modelLink).toHaveCount(1);
    await expect(modelLink).toBeHidden();
    await expect(page.locator(".a-side nav a")).toHaveCount(15);

    // 导轨（1px border-left）挂在内容容器上 ⇒ 收起时整块 display:none，
    // 不会在组头下面留下一条没有内容的竖线。
    expect(await page.locator("#nav-sec-accounts").evaluate((el) => getComputedStyle(el).display)).toBe(
      "none",
    );

    // 偏好要落盘：刷新后仍收起
    await page.reload();
    await expect(page.getByRole("button", { name: "用户与应用" })).toHaveAttribute(
      "aria-expanded",
      "false",
    );

    // 书签/直接进该分区内的页面：必须自动展开，否则当前页在菜单里"消失"
    await page.goto("/admin/users");
    await expect(page.getByRole("button", { name: "用户与应用" })).toHaveAttribute(
      "aria-expanded",
      "true",
    );
    await expect(page.locator(".a-side nav a.active")).toContainText("用户管理");

    // 200px 档（≤1023px）：组头是 <button>，既不撑破侧栏也不塌成文字宽度。
    // 撑破 ⇒ .a-side nav 的 overflow-y:auto 会把 overflow-x 也算 auto，内部冒出横向滚动条；
    // 塌掉 ⇒ 右侧大半行点不动。两条都只能靠几何量出来。
    await page.setViewportSize({ width: 1000, height: 900 });
    // 侧栏宽度带 0.2s 过渡：立刻量会取到 232→200 的中间帧（实测 224.9）
    await expect
      .poll(async () =>
        Math.round(
          await page.locator(".a-side").evaluate((el) => el.getBoundingClientRect().width),
        ),
      )
      .toBe(200);
    const geom = await page.evaluate(() => {
      const nav = document.querySelector<HTMLElement>(".a-side nav")!;
      const head = document.querySelector<HTMLElement>(".a-side .nav-group")!;
      const cs = getComputedStyle(nav);
      return {
        sidebar: document.querySelector<HTMLElement>(".a-side")!.getBoundingClientRect().width,
        overflowX: nav.scrollWidth - nav.clientWidth,
        headW: head.getBoundingClientRect().width,
        inner:
          nav.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight),
      };
    });
    // 宽度是浮点（实测过渡收尾帧会停在 200.125），比较前取整
    expect(Math.round(geom.sidebar), "≤1023px 侧栏应为 200px").toBe(200);
    expect(geom.overflowX, "组头撑出了横向滚动条").toBeLessThanOrEqual(0);
    expect(Math.abs(geom.headW - geom.inner), "组头没撑满 nav 内容宽度").toBeLessThanOrEqual(1);
  });
});

test.describe("管理台 chrome（≤767px）", () => {
  test.use({ viewport: { width: 767, height: 900 } });

  test("侧栏默认移出视口，汉堡按钮可开合并同步关闭", async ({ page }) => {
    await seedAdmin(page);
    // 末端跳转 /admin/logs 会请求 /api/admin/logs（S11 真实数据页），需拦截以避开 401 全局登出
    await stubAdminApi(page);
    await page.goto("/admin/dashboard");
    await expect(page.locator(".a-burger")).toBeVisible();

    const before = await page.locator(".a-side").boundingBox();
    expect(before?.x ?? 0).toBeLessThanOrEqual(0);

    await page.locator(".a-burger").click();
    await expect(page.locator(".a-side")).toHaveClass(/open/);
    // 侧栏开合带过渡动画，需轮询等待位移结束再断言 x==0（瞬时读 boundingBox 会取到中间帧）
    await expect
      .poll(async () => (await page.locator(".a-side").boundingBox())?.x ?? -1)
      .toBe(0);

    await page.locator(".a-side nav a", { hasText: "调用日志" }).click();
    await expect(page).toHaveURL(/\/admin\/logs$/);
    await expect(page.locator(".a-side")).not.toHaveClass(/open/);

    await expectNoHScroll(page);
  });

  test("抽屉态是弹层：role/aria-modal、锁滚动、遮罩", async ({ page }) => {
    await seedAdmin(page);
    await stubAdminApi(page);
    await page.goto("/admin/dashboard");
    const side = page.locator(".a-side");

    // 关闭态：只是被 translateX 移出视口，DOM 仍在 ⇒ 必须 inert，
    // 否则 Tab 能一路走进看不见的菜单。
    expect(await side.evaluate((el) => (el as HTMLElement).inert)).toBe(true);
    await expect(side).not.toHaveAttribute("role", "dialog");
    await expect(page.locator(".mask")).toHaveCount(0);

    await page.locator(".a-burger").click();
    await expect(side).toHaveClass(/open/);

    await expect(side).toHaveAttribute("role", "dialog");
    await expect(side).toHaveAttribute("aria-modal", "true");
    await expect(side).toHaveAttribute("aria-label", "导航菜单");
    expect(await side.evaluate((el) => (el as HTMLElement).inert)).toBe(false);
    await expect(page.locator(".mask")).toBeVisible();
    // 背景不可滚：抽屉自己内部滚（.a-side nav overflow-y:auto），页面不能跟着滚
    await expect.poll(() => page.evaluate(() => document.body.style.overflow)).toBe("hidden");

    // Esc 关闭 + 焦点归还给触发按钮（弹层通用契约，抽屉不能例外）
    await page.keyboard.press("Escape");
    await expect(side).not.toHaveClass(/open/);
    await expect.poll(() => page.evaluate(() => document.body.style.overflow)).toBe("");
    await expect(page.locator(".a-burger")).toBeFocused();
  });

  test("抽屉内有可聚焦的关闭按钮，且 Tab 焦点环不逃出抽屉", async ({ page }) => {
    await seedAdmin(page);
    await stubAdminApi(page);
    await page.goto("/admin/dashboard");
    const side = page.locator(".a-side");

    await page.locator(".a-burger").click();
    await expect(side).toHaveClass(/open/);

    // 关闭入口：焦点被圈在抽屉里，顶栏汉堡在遮罩下层，除 Esc 外没有别的出路
    await page.getByRole("button", { name: "关闭导航" }).click();
    await expect(side).not.toHaveClass(/open/);

    await page.locator(".a-burger").click();
    await expect(side).toHaveClass(/open/);

    const inside = (): Promise<boolean> =>
      page.evaluate(() => document.activeElement?.closest(".a-side") !== null);

    // 抽屉内可聚焦项 = logo + 关闭 + 4 个组头 + 15 条导航 = 21。
    // 循环次数必须**大于**总数：等于总数会绕回起点，焦点逃逸了也照样判通过。
    for (let i = 0; i < 40; i++) await page.keyboard.press("Tab");
    expect(await inside(), "Tab 会把焦点带出抽屉（焦点陷阱失效）").toBe(true);

    // 反向：从首项 Shift+Tab 必须回到末项，而不是跑回页面顶栏
    await page.locator(".a-side .logo").focus();
    await page.keyboard.press("Shift+Tab");
    expect(await inside(), "Shift+Tab 会把焦点带出抽屉（焦点陷阱失效）").toBe(true);

    // 品牌链接是全抽屉唯一不经过 nav 的跳转入口，漏掉它就会「换了页但抽屉与焦点锁还挂着」
    await page.locator(".a-side .logo").click();
    await expect(side).not.toHaveClass(/open/);
  });

  test("抽屉内收起末组：抽屉不被关掉，Tab 焦点不逃出", async ({ page }) => {
    await seedAdmin(page);
    await stubAdminApi(page);
    await page.goto("/admin/dashboard");
    const side = page.locator(".a-side");

    await page.locator(".a-burger").click();
    await expect(side).toHaveClass(/open/);

    // 组头若误调 onClose，点一下收起分组会把整个抽屉关掉 —— 这里是唯一的防线
    const head = page.getByRole("button", { name: "文档与接口" });
    await head.click();
    await expect(head).toHaveAttribute("aria-expanded", "false");
    await expect(side).toHaveClass(/open/);
    await expect(page.locator('.a-side nav a[href="/admin/docs"]')).toBeHidden();

    // 末组收起后，DOM 里最后两个可聚焦节点（API 文档/开放接口）是 display:none，
    // 于是「最后一个可见项」变成组头按钮本身。焦点环若不过滤可见性，会把隐藏链接
    // 当成末项 ⇒ 回绕条件 active === last 永不成立 ⇒ Tab 直接逃出抽屉。
    // 必须从该边界按一次，不能连按 N 次：连按会绕回文档起点，焦点逃出去又能转回侧栏里，
    // 断言就变成假绿（实测 40 次 Tab 会碰巧落回侧栏）。
    const inside = (): Promise<boolean> =>
      page.evaluate(() => Boolean(document.activeElement?.closest(".a-side")));

    await head.focus();
    await page.keyboard.press("Tab");
    expect(await inside(), "折叠末组后 Tab 逃出了抽屉（焦点环没过滤隐藏节点）").toBe(true);
  });

  test("放大到 ≥768px 后遮罩、弹层语义与滚动锁一并撤销", async ({ page }) => {
    await seedAdmin(page);
    await stubAdminApi(page);
    await page.goto("/admin/dashboard");
    await page.locator(".a-burger").click();
    await expect(page.locator(".a-side")).toHaveClass(/open/);
    await expect(page.locator(".mask")).toBeVisible();

    // 768~1023 曾是最典型的「两把尺子」区间：侧栏已是常驻导航，
    // 遮罩却按 Tailwind 的 lg(1024) 判断而继续盖住整页。
    await page.setViewportSize({ width: 1000, height: 900 });

    await expect(page.locator(".mask")).toHaveCount(0);
    await expect(page.locator(".a-side")).not.toHaveAttribute("role", "dialog");
    expect(await page.locator(".a-side").evaluate((el) => (el as HTMLElement).inert)).toBe(false);
    await expect(page.locator(".a-side")).toBeVisible();
    // 宽屏上没有弹层了，页面必须能滚
    await expect.poll(() => page.evaluate(() => document.body.style.overflow)).toBe("");
  });
});

/**
 * 侧栏底栏的对外文档入口。
 *
 * 它**刻意不在 `<nav>` 里**：管理台导航是「能改数据的页面」，文档站是只读的对外面。
 * 放进 nav 会让 15 项计数、4 个分组与「组头/子项」几何断言全部变动，
 * 也会让人误以为它是又一个后台页 —— 所以这条同时守「入口存在」与「没有进入 nav 计数」。
 */
test.describe("管理台侧栏 · 底栏入口", () => {
  test("底栏有「API 文档」，指向 /docs，且不参与 nav 计数", async ({ page }) => {
    await seedAdmin(page);
    await stubAdminApi(page);
    await page.goto("/admin/dashboard");

    const link = page.locator('.a-side .foot a[href="/docs"]');
    await expect(link).toBeVisible();
    await expect(link).toContainText("API 文档");
    await expect(link.locator("svg")).toHaveCount(1);

    // 导航计数不受影响（全站 10 处断言依赖 15 这个数）
    await expect(page.locator(".a-side nav a")).toHaveCount(15);

    // 位置：在导航区之下、行高与导航行一致（40px / 13.5px）
    const nav = (await page.locator(".a-side nav").boundingBox())!;
    const box = (await link.boundingBox())!;
    expect(box.y).toBeGreaterThanOrEqual(nav.y + nav.height - 1);
    const font = await link.evaluate((el) => getComputedStyle(el).fontSize);
    expect(font).toBe("13.5px");
    expect(Math.round(box.height)).toBe(40);

    // 图标左边界与导航图标同一列（10 + 12）
    const iconX = await page.evaluate(() =>
      Math.round(document.querySelector('.a-side .foot a svg')!.getBoundingClientRect().left),
    );
    const navIconX = await page.evaluate(() =>
      Math.round(document.querySelector('.a-side nav a svg')!.getBoundingClientRect().left),
    );
    expect(iconX).toBe(navIconX);
  });
});
