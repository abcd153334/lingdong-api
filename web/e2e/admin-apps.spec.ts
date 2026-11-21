/**
 * 应用管理（卡片列表 + 管理弹窗）：契约级 stub。
 *
 * 与旧版的区别：列表不再是表格（`.tbl` 9 列），而是卡片 + 弹窗六页签。
 * 这里守的是**请求契约**（打到哪个端点、带什么 body）与关键交互
 * （页签切换、抽屉挂载点、二次确认），不是像素。
 */
import { expect, test, type Page } from "@playwright/test";
import { envelope, expectNoHScroll, seedAuth } from "./helpers";

const seedAdmin = (page: Page) => seedAuth(page, { role: 0 });

const APPS = [
  {
    id: 2,
    slug: "web-portal",
    name: "官网门户",
    description: "面向 C 端用户的站点",
    status: 1,
    allow_self_issue: true,
    access_mode: "self_service",
    display_name: "官网门户",
    tagline: "面向 C 端用户的站点",
    logo_url: "",
    maintenance_mode: false,
    maintenance_message: "",
    created_at: "2026-10-20 10:00:00",
    key_count: 5,
    active_key_count: 4,
    user_count: 3,
    used: 12_345_678,
    paid_cent: 21_600,
    paid_count: 3,
    calls_7d: 1880,
    notice_count: 2,
    latest_release: { version: "2.1.0", published_at: "2026-10-21 09:10:00", force_update: false },
  },
  {
    // 停用 + 维护中 + 关闭自助签发：三个徽标必须并列显示，不能互相覆盖
    id: 3,
    slug: "legacy-app",
    name: "旧版应用",
    description: "",
    status: 2,
    allow_self_issue: false,
    access_mode: "approval",
    display_name: "旧版应用",
    tagline: "",
    logo_url: "",
    maintenance_mode: true,
    maintenance_message: "已迁移",
    created_at: "2026-10-21 09:30:00",
    key_count: 0,
    active_key_count: 0,
    user_count: 0,
    used: 0,
    paid_cent: 0,
    paid_count: 0,
    calls_7d: 0,
    notice_count: 0,
    latest_release: null,
  },
];

/** 品牌端点（GET 与 PUT 同形）：只有这 10 个字段。 */
const APP_BRAND = {
  display_name: "官网门户",
  tagline: "面向 C 端用户的站点",
  intro: "门户应用简介",
  logo_icon: "",
  logo_url: "",
  contact_name: "张运营",
  contact_email: "ops@example.com",
  support_url: "https://example.com/help",
  privacy_url: "",
  terms_url: "",
};

/**
 * 统计端点：只有趋势与在线指标。
 *
 * 四格汇总（消费/充值/密钥/用户）**不在这里** —— 它们随列表卡片下发，
 * 概览页签直接用列表行，所以这份桩里没有 agg。
 */
const APP_STATS = {
  trend: Array.from({ length: 7 }, (_, i) => ({
    date: `2026-11-0${i + 3}`,
    requests: 100 + i * 10,
    errors: i,
    charge: 1_000_000 + i,
  })),
  online: {
    online: "online",
    last_call_at: "2026-11-09 20:20",
    calls_24h: 3241,
    errors_24h: 13,
    error_rate: 0.004,
    charge_24h: 128_600_000,
  },
};

const NOTICES = [
  {
    id: 11,
    title: "11 月 12 日 02:00 起网关例行升级",
    content: "升级期间可能短暂 5xx",
    level: "critical",
    enabled: true,
    starts_at: "2026-11-10T00:00:00Z",
    ends_at: "2026-11-13T23:59:00Z",
    active: true,
    created_at: "2026-11-09 10:00:00",
  },
];

const RELEASES = [
  {
    id: 21,
    version: "2.4.0",
    release_notes: "修复批量下载偶发失败",
    download_url: "https://dl.example.com/2.4.0.exe",
    force_update: false,
    status: 1,
    published_at: null,
    created_at: "2026-11-09 10:00:00",
  },
];

interface Captured {
  method: string;
  path: string;
  body: unknown;
}

async function stub(page: Page, captured: Captured[]) {
  await page.route("**/api/admin/**", (route) => {
    const req = route.request();
    const u = new URL(req.url());
    const method = req.method();

    if (u.pathname === "/api/admin/apps" && method === "GET") {
      return route.fulfill({ json: envelope({ items: APPS, total: APPS.length, page: 1, page_size: 20 }) });
    }
    if (u.pathname === "/api/admin/apps" && method === "POST") {
      captured.push({ method, path: u.pathname, body: req.postDataJSON() });
      return route.fulfill({ json: envelope({ id: 9, slug: "new-app" }) });
    }
    if (/^\/api\/admin\/apps\/\d+$/.test(u.pathname) && method === "PUT") {
      captured.push({ method, path: u.pathname, body: req.postDataJSON() });
      return route.fulfill({ json: envelope(null) });
    }
    if (/^\/api\/admin\/apps\/\d+\/status$/.test(u.pathname) && method === "PATCH") {
      captured.push({ method, path: u.pathname, body: req.postDataJSON() });
      return route.fulfill({ json: envelope(null) });
    }
    if (/^\/api\/admin\/apps\/\d+\/brand$/.test(u.pathname) && method === "GET") {
      return route.fulfill({ json: envelope(APP_BRAND) });
    }
    if (/^\/api\/admin\/apps\/\d+\/brand$/.test(u.pathname) && method === "PUT") {
      captured.push({ method, path: u.pathname, body: req.postDataJSON() });
      return route.fulfill({ json: envelope({ ...APP_BRAND, display_name: "灵动画影" }) });
    }
    if (/^\/api\/admin\/apps\/\d+\/stats$/.test(u.pathname) && method === "GET") {
      return route.fulfill({ json: envelope(APP_STATS) });
    }
    if (/^\/api\/admin\/apps\/\d+\/maintenance$/.test(u.pathname) && method === "PATCH") {
      captured.push({ method, path: u.pathname, body: req.postDataJSON() });
      return route.fulfill({ json: envelope({ maintenance_mode: true, maintenance_message: "" }) });
    }
    if (/^\/api\/admin\/apps\/\d+\/notices$/.test(u.pathname) && method === "GET") {
      return route.fulfill({ json: envelope({ items: NOTICES, total: NOTICES.length, page: 1, page_size: 20 }) });
    }
    if (/^\/api\/admin\/apps\/\d+\/notices$/.test(u.pathname) && method === "POST") {
      captured.push({ method, path: u.pathname, body: req.postDataJSON() });
      return route.fulfill({ json: envelope({ ...NOTICES[0], id: 12 }) });
    }
    if (/^\/api\/admin\/apps\/\d+\/releases$/.test(u.pathname) && method === "GET") {
      // current_release_id：UI 用它标出"当前对外下发"的版本（可能并存多个已发布）。
      return route.fulfill({
        json: envelope({
          items: RELEASES,
          total: RELEASES.length,
          page: 1,
          page_size: 20,
          current_release_id: 21,
        }),
      });
    }
    if (/^\/api\/admin\/apps\/\d+\/releases\/\d+\/status$/.test(u.pathname) && method === "PATCH") {
      captured.push({ method, path: u.pathname, body: req.postDataJSON() });
      return route.fulfill({ json: envelope(null) });
    }
    if (/^\/api\/admin\/apps\/\d+\/keys$/.test(u.pathname) && method === "GET") {
      // 45 把密钥、本页 20 条：用来验证分页条真的能翻到第 2 页。
      const page = Number(u.searchParams.get("page") || "1");
      const items = Array.from({ length: 20 }, (_, i) => ({
        id: (page - 1) * 20 + i + 1,
        user_id: 100 + i,
        username: `user${(page - 1) * 20 + i + 1}`,
        key_prefix: `sk-${page}-${i}`,
        status: 1,
        used: 1_000_000,
        scopes: [],
        created_at: "2026-11-09T10:00:00Z",
        rotated_at: "",
      }));
      return route.fulfill({ json: envelope({ items, total: 45, page, page_size: 20 }) });
    }
    return route.fulfill({ json: envelope(null) });
  });
}

test.describe("应用管理（卡片 + 弹窗）", () => {
  test("卡片列表：两张卡、状态徽标并列、聚合口径、侧栏与面包屑同步", async ({ page }) => {
    await seedAdmin(page);
    await stub(page, []);
    await page.goto("/admin/apps");

    await expect(page.getByRole("heading", { level: 1 })).toHaveText("应用管理");
    await expect(page.locator(".a-side nav a.active")).toContainText("应用管理");
    await expect(page.locator(".a-top .crumb b")).toHaveText("应用管理");

    const cards = page.locator('[data-testid="app-card"]');
    await expect(cards).toHaveCount(2);

    const portal = cards.filter({ hasText: "web-portal" });
    // 密钥列是「启用/全部」，用户数是去重持有者
    await expect(portal).toContainText("4/5");
    await expect(portal).toContainText("1,880");
    await expect(portal).toContainText("v2.1.0");

    // 停用 + 维护中 + 关闭自助签发 三个徽标并列，不互相覆盖
    const legacy = cards.filter({ hasText: "legacy-app" });
    await expect(legacy.locator(".badge", { hasText: "停用" })).toHaveCount(1);
    await expect(legacy.locator(".badge", { hasText: "维护中" })).toHaveCount(1);
    await expect(legacy.locator(".badge", { hasText: "关闭自助签发" })).toHaveCount(1);
    // 没有托管密钥时数字必须是 0，不能留空
    await expect(legacy).toContainText("0/0");
    // 从未发布版本时明确显示，而不是空白
    await expect(legacy).toContainText("未发布版本");
  });

  test("新建：POST /admin/apps 带 slug 与名称", async ({ page }) => {
    await seedAdmin(page);
    const captured: Captured[] = [];
    await stub(page, captured);
    await page.goto("/admin/apps");

    // 页头与筛选条各有一个「新建应用」，取页头那个（第一个）
    await page.getByRole("button", { name: "新建应用" }).first().click();
    const dlg = page.getByRole("dialog", { name: "新建应用" });
    await expect(dlg).toBeVisible();

    await dlg.getByLabel("应用标识").fill("new-app");
    await dlg.getByLabel("名称").fill("新应用");
    await dlg.locator("textarea").fill("给接入方的一行说明");
    await dlg.getByRole("button", { name: "创建" }).click();

    await expect.poll(() => captured.length).toBeGreaterThan(0);
    expect(captured[0]).toMatchObject({ method: "POST", path: "/api/admin/apps" });
    expect(captured[0].body).toEqual({
      slug: "new-app",
      name: "新应用",
      description: "给接入方的一行说明",
      allow_self_issue: true,
    });
  });

  test("管理弹窗：五个页签可切换，概览给出身份、指标与危险区", async ({ page }) => {
    await seedAdmin(page);
    await stub(page, []);
    await page.goto("/admin/apps");

    await page
      .locator('[data-testid="app-card"]', { hasText: "web-portal" })
      .getByRole("button", { name: "管理" })
      .click();
    const dlg = page.getByRole("dialog", { name: /应用管理/ });
    await expect(dlg).toBeVisible();

    // 页签清单必须包含第六个「插件」：漏了它，新增页签被裁也不会红。
    for (const name of ["概览", "品牌", "公告", "版本", "插件"]) {
      await expect(dlg.getByRole("tab", { name, exact: true })).toBeVisible();
    }
    // ---- 结构：页签住顶栏，不再单独占一行 ----
    await expect(dlg.locator(".dl-head").getByRole("tablist")).toHaveCount(1);
    // 顶栏是**下划线**页签（不是胶囊）：胶囊那套底色 + 描边在这一档已被淘汰
    await expect(dlg.locator(".dl-head .tabs")).toHaveCount(1);
    await expect(dlg.locator(".dl-head .pill-tabs")).toHaveCount(0);
    // 顶栏**没有标题**：名字只走 aria-label（`getByRole("dialog", { name })` 仍命中）
    await expect(dlg.locator(":scope > .dl-head > .dl-title")).toHaveCount(0);
    // 旧的吸附层必须彻底消失：留着它就是"页签另起一行"
    await expect(dlg.locator(".page-tabs-bar")).toHaveCount(0);
    // 页签写在 DOM 里的顺序 = 视觉顺序：静态五项，然后「插件」清单
    expect(
      await dlg.getByRole("tab").evaluateAll((els) => els.map((e) => e.getAttribute("aria-label"))),
    ).toEqual(["概览", "品牌", "公告", "版本", "插件"]);
    // 「插件」恒靠最右：页签条还有空位时必须顶到右缘（不是紧跟「版本」）
    {
      const tl = (await dlg.getByRole("tablist", { name: "应用管理" }).boundingBox())!;
      const pin = (await dlg.getByRole("tab", { name: "插件", exact: true }).boundingBox())!;
      expect(tl.x + tl.width - (pin.x + pin.width), "「插件」没有靠最右").toBeLessThanOrEqual(2);
    }

    // ---- 底栏已移除：关闭只剩右上角 X（+ Esc + 遮罩） ----
    // `:scope >` 是必须的：编辑弹窗也挂在同一个 dialog 的 DOM 里，
    // 它自带 .dl-head/.dl-foot，不限定就会把内层的底栏算到自己头上。
    await expect(dlg.locator(":scope > .dl-foot")).toHaveCount(0);
    await expect(dlg.locator(":scope > .dl-head > .dl-close")).toHaveCount(1);

    // 概览：应用标识不可改 + 停用入口在弹窗里（不在卡片上）
    // 断言限定在「概览」面板内：编辑基本信息弹窗也在同一个 dialog 的 DOM 里
    // （关闭态 visibility:hidden），不限定会命中两个 .mono。
    const overview = dlg.getByRole("tabpanel", { name: "概览" });
    await expect(overview.locator(".mono", { hasText: "web-portal" })).toBeVisible();
    await expect(overview.getByRole("button", { name: "停用应用" })).toBeVisible();
    // 底栏的「查看密钥」搬到概览动作行 —— 应用被筛掉时列表卡片入口不可达
    await expect(overview.getByRole("button", { name: "查看密钥" })).toBeVisible();

    await dlg.getByRole("tab", { name: "品牌" }).click();
    await expect(dlg.getByLabel("展示名称")).toHaveValue("官网门户");

    // 切回概览不应报错（页签是局部 state，不写 URL）
    await dlg.getByRole("tab", { name: "概览" }).click();
    await expect(page).toHaveURL(/\/admin\/apps$/);
  });

  test("品牌：保存走 PUT .../brand，body 只含品牌字段", async ({ page }) => {
    await seedAdmin(page);
    const captured: Captured[] = [];
    await stub(page, captured);
    await page.goto("/admin/apps");

    await page
      .locator('[data-testid="app-card"]', { hasText: "web-portal" })
      .getByRole("button", { name: "管理" })
      .click();
    const dlg = page.getByRole("dialog", { name: /应用管理/ });
    await dlg.getByRole("tab", { name: "品牌" }).click();

    await dlg.getByLabel("展示名称").fill("灵动画影");
    await dlg.getByRole("button", { name: "保存品牌信息" }).click();

    await expect.poll(() => captured.length).toBeGreaterThan(0);
    expect(captured[0]).toMatchObject({ method: "PUT", path: "/api/admin/apps/2/brand" });
    expect(captured[0].body).toMatchObject({
      display_name: "灵动画影",
      contact_email: "ops@example.com",
      support_url: "https://example.com/help",
    });
  });

  test("公告：抽屉挂到 body（不被弹窗裁切），新建走 POST", async ({ page }) => {
    await seedAdmin(page);
    const captured: Captured[] = [];
    await stub(page, captured);
    await page.goto("/admin/apps");

    await page
      .locator('[data-testid="app-card"]', { hasText: "web-portal" })
      .getByRole("button", { name: "管理" })
      .click();
    const dlg = page.getByRole("dialog", { name: /应用管理/ });
    await dlg.getByRole("tab", { name: "公告" }).click();

    await expect(dlg.getByText("11 月 12 日 02:00 起网关例行升级")).toBeVisible();
    await dlg.getByRole("button", { name: "新建公告" }).click();

    const drawer = page.getByRole("dialog", { name: "新建公告" });
    await expect(drawer).toBeVisible();
    // 嵌套抽屉必须 portal：否则会被 .dl-body 的 overflow 裁掉。
    // 用 aria-label 精确定位到"新建公告"这一个（页面上还有一把关闭状态的密钥抽屉）。
    await expect(page.locator('body > aside.drawer[aria-label="新建公告"]')).toHaveCount(1);

    await drawer.getByLabel("标题").fill("新公告");
    await drawer.locator("textarea").fill("公告正文");
    await drawer.getByRole("button", { name: "保存" }).click();

    await expect.poll(() => captured.length).toBeGreaterThan(0);
    expect(captured[0]).toMatchObject({ method: "POST", path: "/api/admin/apps/2/notices" });
    expect(captured[0].body).toMatchObject({ title: "新公告", content: "公告正文", level: "info", enabled: true });
  });

  test("版本：草稿发布走 PATCH .../status {status:2}，需二次确认", async ({ page }) => {
    await seedAdmin(page);
    const captured: Captured[] = [];
    await stub(page, captured);
    await page.goto("/admin/apps");

    await page
      .locator('[data-testid="app-card"]', { hasText: "web-portal" })
      .getByRole("button", { name: "管理" })
      .click();
    const dlg = page.getByRole("dialog", { name: /应用管理/ });
    await dlg.getByRole("tab", { name: "版本" }).click();

    await expect(dlg.getByText("v2.4.0")).toBeVisible();
    await dlg.getByRole("button", { name: "发布" }).click();

    const confirm = page.getByRole("dialog", { name: "发布 v2.4.0？" });
    await expect(confirm).toBeVisible();
    await confirm.getByRole("button", { name: "发布" }).click();

    await expect.poll(() => captured.length).toBeGreaterThan(0);
    expect(captured[0]).toMatchObject({
      method: "PATCH",
      path: "/api/admin/apps/2/releases/21/status",
      body: { status: 2 },
    });
  });

  test("概览：开启维护需确认，走 PATCH .../maintenance", async ({ page }) => {
    await seedAdmin(page);
    const captured: Captured[] = [];
    await stub(page, captured);
    await page.goto("/admin/apps");

    await page
      .locator('[data-testid="app-card"]', { hasText: "web-portal" })
      .getByRole("button", { name: "管理" })
      .click();
    const dlg = page.getByRole("dialog", { name: /应用管理/ });

    // 维护模式已在概览页签内（原「在线」页签已并入）：打开弹窗即可见，无需切页签。
    await dlg.getByRole("checkbox", { name: "开启维护模式" }).check();
    await dlg.getByRole("button", { name: "保存维护设置" }).click();

    const confirm = page.getByRole("dialog", { name: "开启维护模式？" });
    await expect(confirm).toBeVisible();
    await confirm.getByRole("button", { name: "开启维护" }).click();

    await expect.poll(() => captured.length).toBeGreaterThan(0);
    expect(captured[0]).toMatchObject({ method: "PATCH", path: "/api/admin/apps/2/maintenance" });
    expect(captured[0].body).toMatchObject({ enabled: true });
  });

  /**
   * 嵌套弹窗必须拿到**自己**的内边距。
   *
   * 回归来源（2026-11-20）：宿主弹窗的样式写成了后代选择器
   * （`.dialog.app-manage-dialog .dl-body{padding:0}`），而编辑弹窗就渲染在宿主
   * `.dl-body` 的**里面**（Dialog 不传 portal ⇒ 内联渲染）—— 于是子弹窗一起拿到
   * padding:0 / overflow:hidden / 顶栏 padding 0，表现为标题贴顶被裁、表单贴边错位，
   * 看上去像"表单组件坏了"。样式层已改直接子选择器，这条用例守的就是它。
   *
   * 判据只认几何：内边距、顶栏高度、弹窗宽度三者都必须回到 `.dialog` 的默认口径。
   */
  test("编辑应用信息：嵌套弹窗有自己的内边距，不被宿主弹窗串味", async ({ page }) => {
    await seedAdmin(page);
    await stub(page, []);
    await page.goto("/admin/apps");
    await page
      .locator('[data-testid="app-card"]', { hasText: "web-portal" })
      .getByRole("button", { name: "管理" })
      .click();
    const dlg = page.getByRole("dialog", { name: /应用管理/ });
    await dlg.getByRole("button", { name: "编辑基本信息" }).click();
    const edit = page.getByRole("dialog", { name: "编辑应用信息" });
    await expect(edit).toBeVisible();
    await expect(edit).toHaveCSS("opacity", "1");

    const m = await edit.evaluate((el) => {
      const body = el.querySelector(":scope > .dl-body") as HTMLElement;
      const head = el.querySelector(":scope > .dl-head") as HTMLElement;
      const grid = el.querySelector(".form-grid") as HTMLElement;
      const bs = getComputedStyle(body);
      return {
        width: Math.round(el.getBoundingClientRect().width),
        padTop: bs.paddingTop,
        padLeft: bs.paddingLeft,
        overflowY: bs.overflowY,
        headPadTop: getComputedStyle(head).paddingTop,
        headH: Math.round(head.getBoundingClientRect().height),
        cols: getComputedStyle(grid).gridTemplateColumns.split(" ").length,
      };
    });
    expect(m.width, "size=md 的宽度被宿主改掉了").toBe(440);
    expect(m.padTop, "内边距被宿主的 padding:0 覆盖了").toBe("20px");
    expect(m.padLeft).toBe("20px");
    expect(m.overflowY, "子弹窗内容区必须自己滚").toBe("auto");
    expect(m.headPadTop, "顶栏内边距被宿主的 0 覆盖了").toBe("16px");
    expect(m.headH, "顶栏塌成一条标题线").toBeGreaterThan(40);
    // 版式：标识 | 名称两列，说明与开关各占整行
    expect(m.cols, "表单没走两列栅格").toBe(2);
  });

  test("管理弹窗：概览里的「在线」能力与统计同源，不再单独占一个页签", async ({ page }) => {
    await seedAdmin(page);
    await stub(page, []);
    await page.goto("/admin/apps");
    await page
      .locator('[data-testid="app-card"]', { hasText: "web-portal" })
      .getByRole("button", { name: "管理" })
      .click();
    const dlg = page.getByRole("dialog", { name: /应用管理/ });

    // 「在线」页签已并入概览：既不能有同名页签，也要能在概览里读到运行态。
    await expect(dlg.getByRole("tab", { name: "在线" })).toHaveCount(0);
    const panel = dlg.getByRole("tabpanel", { name: "概览" });
    await expect(panel.getByRole("heading", { name: /运行态/ })).toBeVisible();
    // 维护开关与停用按钮必须同屏可达（原来分居两页，运营要来回切）
    await expect(panel.getByRole("checkbox", { name: "开启维护模式" })).toBeVisible();
    await expect(panel.getByRole("button", { name: "停用应用" })).toBeVisible();
  });

  test("移动端 375px：卡片单列且无横向滚动", async ({ page }) => {
    await seedAdmin(page);
    await stub(page, []);
    await page.setViewportSize({ width: 375, height: 780 });
    await page.goto("/admin/apps");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("应用管理");
    await expect(page.locator('[data-testid="app-card"]')).toHaveCount(2);
    await expectNoHScroll(page);
  });

  test("新建应用：取消后再打开，表单不残留上一次的输入", async ({ page }) => {
    await seedAdmin(page);
    await stub(page, []);
    await page.goto("/admin/apps");

    await page.getByRole("button", { name: "新建应用" }).first().click();
    const dlg = page.getByRole("dialog", { name: "新建应用" });
    await dlg.getByLabel("应用标识").fill("left-over");
    await dlg.getByLabel("名称").fill("残留的名称");
    await dlg.getByRole("button", { name: "取消" }).click();

    await page.getByRole("button", { name: "新建应用" }).first().click();
    // Dialog 关闭不卸载子树，靠 key 重建清空；不重建的话这里会带着 left-over，
    // 再点创建必撞 app_slug_exists（而应用没有删除入口）。
    await expect(dlg.getByLabel("应用标识")).toHaveValue("");
    await expect(dlg.getByLabel("名称")).toHaveValue("");
  });

  test("新建应用：非法标识在前端拦下，不发请求", async ({ page }) => {
    await seedAdmin(page);
    const captured: Captured[] = [];
    await stub(page, captured);
    await page.goto("/admin/apps");

    await page.getByRole("button", { name: "新建应用" }).first().click();
    const dlg = page.getByRole("dialog", { name: "新建应用" });
    // 大写与下划线都不合法；标识创建后不可改，必须在提交前挡住。
    await dlg.getByLabel("应用标识").fill("My_App");
    await dlg.getByLabel("名称").fill("测试");
    await dlg.getByRole("button", { name: "创建" }).click();

    await expect(dlg.getByText(/只能是小写字母/)).toBeVisible();
    expect(captured).toHaveLength(0);
  });

  test("密钥抽屉：45 把密钥可翻到第 2 页（不再只显示前 20 把）", async ({ page }) => {
    await seedAdmin(page);
    const captured: Captured[] = [];
    await stub(page, captured);
    await page.goto("/admin/apps");

    await page
      .locator('[data-testid="app-card"]', { hasText: "web-portal" })
      .getByRole("button", { name: "密钥" })
      .click();
    const drawer = page.getByRole("dialog", { name: /托管密钥/ });
    await expect(drawer.getByText("共 45 条")).toBeVisible();

    await drawer.getByRole("button", { name: "2", exact: true }).click();
    await expect.poll(() => captured.length).toBeGreaterThanOrEqual(0);
    // 第 2 页的第一行前缀来自 page=2 的桩数据。
    await expect(drawer.getByText("sk-2-0…")).toBeVisible();
  });

  /**
   * 页签保活。
   *
   * 品牌有九个字段，运营常"填一半去看一眼公告"。此前 PageTabs 只渲染当前面板，
   * 切走即卸载 → useState 草稿销毁，切回来是服务器旧值，用户只会当成 bug 重填。
   * 这里守两件事：内容还在、页签上有未保存圆点（否则用户不知道有未保存的改动）。
   */
  test("品牌：切到公告再切回来，未保存的草稿还在", async ({ page }) => {
    await seedAdmin(page);
    await stub(page, []);
    await page.goto("/admin/apps");

    await page
      .locator('[data-testid="app-card"]', { hasText: "web-portal" })
      .getByRole("button", { name: "管理" })
      .click();
    const dlg = page.getByRole("dialog", { name: /应用管理/ });
    await dlg.getByRole("tab", { name: "品牌" }).click();

    await dlg.getByLabel("展示名称").fill("临时改的名字");
    // 小圆点是 aria-hidden 的纯视觉提示，所以按类名定位而不是可访问名。
    await expect(dlg.getByRole("tab", { name: "品牌" }).locator(".tab-dot")).toBeVisible();

    await dlg.getByRole("tab", { name: "公告" }).click();
    await dlg.getByRole("tab", { name: "品牌" }).click();

    await expect(dlg.getByLabel("展示名称")).toHaveValue("临时改的名字");
  });
  /**
   * 弹窗几何（桌面）：五个页签内容高差极大（概览带趋势图，公告只有一行），
   * 而 .dialog 是 fixed + translate 的自高度居中盒 —— 外壳不定高时，
   * 切页签会上下各弹半格。jsdom 不做布局，这条只能由 Playwright 守。
   */
  test("管理弹窗：切五个页签，外壳高度与上边缘恒定", async ({ page }) => {
    await seedAdmin(page);
    await stub(page, []);
    await page.goto("/admin/apps");
    await page
      .locator('[data-testid="app-card"]', { hasText: "web-portal" })
      .getByRole("button", { name: "管理" })
      .click();

    const dlg = page.getByRole("dialog", { name: /应用管理/ });
    await expect(dlg).toHaveCSS("opacity", "1"); // 桌面入场是 opacity 0→1 + scale(.95→1)
    const base = (await dlg.boundingBox())!;
    for (const name of ["品牌", "公告", "版本", "插件", "概览"]) {
      await dlg.getByRole("tab", { name, exact: true }).click();
      const box = (await dlg.boundingBox())!;
      expect(Math.abs(box.height - base.height), `${name} 高度变了`).toBeLessThanOrEqual(1);
      expect(Math.abs(box.y - base.y), `${name} 上边缘变了`).toBeLessThanOrEqual(1);
    }
  });

  /**
   * 滚动权在**当前页签面板**里，页签条不在滚动容器内。
   * 若哪天有人把 overflow 挪回 .dl-body，页签条会跟着内容滚走 —— 这条会红。
   */
  test("管理弹窗：滚当前页签，页签条不跟着滚走", async ({ page }) => {
    await seedAdmin(page);
    await stub(page, []);
    await page.goto("/admin/apps");
    await page
      .locator('[data-testid="app-card"]', { hasText: "web-portal" })
      .getByRole("button", { name: "管理" })
      .click();

    const dlg = page.getByRole("dialog", { name: /应用管理/ });
    await dlg.getByRole("tab", { name: "品牌" }).click();
    const panel = dlg.locator(".page-tabs > .tab-panel:not([hidden])");
    // 先证明"有得滚"，否则下面是个空断言。
    // 用 poll 而不是取一次值：两个 spec 并行跑时，面板可能还没完成首次布局
    // （clientHeight=0 ⇒ 差值也是 0），一次取值会偶发红；poll 仍要求最终真的溢出。
    await expect
      .poll(async () => panel.evaluate((el) => el.scrollHeight - el.clientHeight))
      .toBeGreaterThan(50);

    // 页签条用角色定位：它已经搬进顶栏，类名 .page-tabs-bar 在本弹窗不复存在
    const tablist = dlg.getByRole("tablist", { name: "应用管理" });
    // 结构不变量优先于坐标：页签条不在任何滚动容器里，"跟着滚"在结构上就不可能。
    // 只测坐标的话，把 overflow 挪回 .dl-body 而内容刚好不够高时仍会"通过"。
    expect(await tablist.evaluate((el) => el.closest(".tab-panel") === null)).toBe(true);

    const bar = (await tablist.boundingBox())!;
    // 用 evaluate 直接写 scrollTop：mouse.wheel 要命中坐标，容易被别的层吃掉
    await panel.evaluate((el) => {
      el.scrollTop = 260;
    });
    expect(await panel.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);

    const bar2 = (await tablist.boundingBox())!;
    expect(Math.abs(bar2.y - bar.y)).toBeLessThanOrEqual(1);
  });

  /**
   * 滚动条必须贴着弹窗外壳的内沿。
   *
   * 结构根因：内边距曾挂在 `.dl-body`（padding:20px），而滚动发生在它内部的
   * `.page-tabs > .tab-panel` —— 滚动条于是悬在右边 20px 处，上下还各短一截。
   * 现在 padding 下沉到滚动容器自身，滚动条轨道右缘 = 外壳内沿（只差 1px 边框）。
   *
   * 两条断言缺一不可：先证明**确实有**竖向滚动条（否则内容不够高时永远"贴边"），
   * 再量距离。
   */
  test("管理弹窗：滚动条贴外壳内沿（不是悬空 20px）", async ({ page }) => {
    // 视口压矮：弹窗高度 = min(88vh, 780)，内容必须真的溢出，
    // 否则"贴边"是内容不够长时的假象（断言第一条就会挡下来）。
    await page.setViewportSize({ width: 1280, height: 700 });
    await seedAdmin(page);
    await stub(page, []);
    await page.goto("/admin/apps");
    await page
      .locator('[data-testid="app-card"]', { hasText: "web-portal" })
      .getByRole("button", { name: "管理" })
      .click();

    const dlg = page.getByRole("dialog", { name: /应用管理/ });
    const panel = dlg.locator(".page-tabs > .tab-panel:not([hidden])");
    // 用默认的「概览」（最长的一页：名片 + 四格 + 图 + 维护 + 应用状态）。
    // 原用例切的是「品牌」，概览重排后品牌表单在 700px 下已经放得下，
    // 前置条件（必须真的溢出）会直接判红 —— 那是页面变短，不是滚动条坏了。
    // poll 而不是取一次值：切页签/首次布局未完成时 clientHeight 可能是 0。
    await expect
      .poll(async () => panel.evaluate((el) => el.scrollHeight - el.clientHeight))
      .toBeGreaterThan(50);
    const m = await panel.evaluate((el) => {
      const d = el.closest(".dialog")!.getBoundingClientRect();
      const p = el.getBoundingClientRect();
      return {
        gap: Math.round(d.right - p.right),
        overflow: el.scrollHeight - el.clientHeight,
        bar: el.offsetWidth - el.clientWidth,
      };
    });

    // 前置条件：内容必须真的溢出，否则"贴边"毫无意义。
    // 注意**不能**用 offsetWidth-clientWidth 判断有没有滚动条：无头 Chromium 用的是
    // overlay 条（不占布局宽度），同一份代码在桌面 Chrome 上是 10px 占位条 ——
    // 按宽度断言会在两种环境下给出相反结论。
    expect(m.overflow, "内容没溢出，本断言失效").toBeGreaterThan(50);
    expect(m.bar, "overlay 与占位条的实际宽度（仅记录，不作判据）").toBeGreaterThanOrEqual(0);

    // 滚动容器右缘 = 外壳内沿（只差 .dialog 的 1px 边框）。
    // padding 若留在 .dl-body，这里会是 ≈21（20 + 1）—— 就是本次要根治的"悬空"。
    expect(m.gap, "滚动条没有贴到外壳内沿").toBeLessThanOrEqual(2);
  });

  /**
   * 375px：底部 Sheet 必须**满宽**。
   * 这里守的是 CSS 特异性陷阱：.dialog.app-manage-dialog（0,2,0）会压过
   * @media 里 0,1,0 的 .dialog{width:100%}，漏写就会变成 345px 左贴边、右侧漏 30px。
   */
  test("管理弹窗：375px 五个页签都可点、逐页签无横向溢出、高度恒定", async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 780 });
    await seedAdmin(page);
    await stub(page, []);
    await page.goto("/admin/apps");
    await page
      .locator('[data-testid="app-card"]', { hasText: "web-portal" })
      .getByRole("button", { name: "管理" })
      .click();

    const dlg = page.getByRole("dialog", { name: /应用管理/ });
    // ≤767 的 .dialog 恒为 opacity:1（入场走 translate），等不到 opacity 变化，只能轮询盒稳定
    await expect.poll(async () => (await dlg.boundingBox())!.height).toBeLessThanOrEqual(740);
    const base = (await dlg.boundingBox())!;
    expect(base.width, "Sheet 没有满宽").toBe(375);

    // 手机上顶栏单行：没有标题，页签不折行（无障碍名来自 aria-label）
    await expect(dlg.locator(":scope > .dl-head > .dl-title")).toHaveCount(0);
    const head = (await dlg.locator(":scope > .dl-head").boundingBox())!;
    expect(head.height, "顶栏折成两行了").toBeLessThan(90);
    // 五个页签在 375px 下刚好放得下（装了插件就会横滚）。
    // 判据必须是「所有页签同一行」而不是「有溢出」：内容刚好放下时溢出量是 0，
    // 拿溢出量当单行判据，会把「刚好放得下」误判成缺陷。
    const tops = await dlg
      .getByRole("tab")
      .evaluateAll((els) => els.map((e) => Math.round(e.getBoundingClientRect().top)));
    expect(new Set(tops).size, "顶栏页签折行了").toBe(1);

    // 页签清单必须包含第六个「插件」：漏了它，新增页签被裁也不会红。
    for (const name of ["概览", "品牌", "公告", "版本", "插件"]) {
      await dlg.getByRole("tab", { name, exact: true }).click();
      await expect(dlg.getByRole("tab", { name, exact: true })).toBeInViewport(); // 第 5 项不被裁
      const panel = dlg.locator(".page-tabs > .tab-panel:not([hidden])");
      expect(await panel.evaluate((el) => el.scrollWidth - el.clientWidth)).toBeLessThanOrEqual(1);
      const box = (await dlg.boundingBox())!;
      expect(Math.abs(box.height - base.height), `${name} 高度变了`).toBeLessThanOrEqual(1);
    }
    // 与逐页签断言互补：.dl-body 是 overflow:hidden，页面级断言抓不到面板内溢出
    await expectNoHScroll(page);
  });

  /**
   * 密钥抽屉分页条：45 条 / 3 页时它本来放得下；100 页时第 1 页也只有 6 个按钮、
   * 照样挤得下。真正会顶出容器的是「首末 + 省略号 + 上下页」7 个按钮那一档
   * （第 2 页起）—— 所以必须翻到第 2 页再量，否则这条是空断言。
   */
  test("密钥抽屉：2000 把密钥（100 页）时 375px 分页条换行不横向溢出", async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 780 });
    await seedAdmin(page);
    await stub(page, []);
    // 后注册的 route 优先生效，覆盖 stub 里的 45 条
    await page.route("**/api/admin/apps/*/keys*", (route) => {
      const p = Number(new URL(route.request().url()).searchParams.get("page") || "1");
      const items = Array.from({ length: 20 }, (_, i) => ({
        id: (p - 1) * 20 + i + 1,
        user_id: 100 + i,
        username: `user${(p - 1) * 20 + i + 1}`,
        key_prefix: `sk-${p}-${i}`,
        status: 1,
        used: 1_000_000,
        scopes: [],
        created_at: "2026-11-09T10:00:00Z",
        rotated_at: "",
      }));
      return route.fulfill({ json: envelope({ items, total: 2000, page: p, page_size: 20 }) });
    });
    await page.goto("/admin/apps");
    await page
      .locator('[data-testid="app-card"]', { hasText: "web-portal" })
      .getByRole("button", { name: "密钥" })
      .click();

    const drawer = page.getByRole("dialog", { name: /托管密钥/ });
    await expect(drawer.getByText("共 2000 条")).toBeVisible();
    // 翻到第 2 页：第 1 页的页码窗口是 [1,2,…,100] 共 6 个按钮，7 个按钮（第 2 页起
    // 是 [1,2,3,…,100]）才真的挤不进 331px —— 第 1 页量出来永远是"放得下"。
    await drawer.getByRole("button", { name: "下一页" }).click();
    await expect(drawer.locator(".pager button.on")).toHaveText("2");
    const pager = drawer.locator(".pager");
    // 不能量 scrollWidth：`.pager button::after` 是 44px 触控命中区（components.css:275），
    // 每行最后一个按钮会稳定溢出 7px —— 那是设计不是布局破。
    // 也不能只看右缘：`justify-content:flex-end` 下不换行时内容是往**左**溢出的。
    // 所以两侧都量，全部子元素必须落在分页条自己的盒子里。
    const fit = await pager.evaluate((el) => {
      const box = el.getBoundingClientRect();
      const kids = [...el.children].map((k) => k.getBoundingClientRect());
      return {
        left: Math.round(Math.min(...kids.map((k) => k.left)) - box.left),
        right: Math.round(Math.max(...kids.map((k) => k.right)) - box.right),
      };
    });
    expect(fit.left, "分页条内容向左溢出").toBeGreaterThanOrEqual(-1);
    expect(fit.right, "分页条内容向右溢出").toBeLessThanOrEqual(1);
    await expectNoHScroll(page);
  });
});
