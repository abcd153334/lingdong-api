/** 充值套餐管理（契约级 stub）：列表 / 新建 / 编辑清零 / 启停 / 删除保护 / 移动端不溢出 */
import { expect, test, type Page } from "@playwright/test";
import { envelope, expectNoHScroll, seedAuth } from "./helpers";

/** 管理台路由守卫要求已登录且 role=0 */
const seedAdmin = (page: Page) => seedAuth(page, { role: 0 });

/** 当前充值上限 $1,000（balance_limit 是**微**：$1000 = 1_000_000_000） */
const CAP = 1_000_000_000;

const PKGS = [
  {
    id: 1,
    name: "体验包",
    price_cent: 7200,
    base: 9_999_936,
    bonus: 1_000_064,
    credited: 11_000_000,
    enabled: true,
    sort_order: 10,
    badge: "",
    order_count: 128,
    created_at: "2026-10-20T10:00:00+08:00",
    updated_at: "2026-10-20T10:00:00+08:00",
  },
  {
    id: 2,
    name: "标准包",
    price_cent: 21600,
    base: 29_999_808,
    bonus: 3_000_192,
    credited: 33_000_000,
    enabled: true,
    sort_order: 20,
    badge: "recommend",
    order_count: 37,
    created_at: "2026-10-20T10:00:00+08:00",
    updated_at: "2026-10-20T10:00:00+08:00",
  },
  {
    // 有引用 → 删除按钮必须置灰（后端也会 409，但前端不该给出可点的假象）
    id: 3,
    name: "专业包",
    price_cent: 43200,
    base: 59_999_616,
    bonus: 9_000_384,
    credited: 69_000_000,
    enabled: false,
    sort_order: 30,
    badge: "",
    order_count: 12,
    created_at: "2026-10-20T10:00:00+08:00",
    updated_at: "2026-10-20T10:00:00+08:00",
  },
  {
    // 无引用 + 到账额度超上限：删除可用，且到账列要红标
    id: 4,
    name: "企业包",
    price_cent: 7_200_000,
    base: 9_999_936_000,
    bonus: 64_000,
    credited: 10_000_000_000,
    enabled: true,
    sort_order: 50,
    badge: "",
    order_count: 0,
    created_at: "2026-10-20T10:00:00+08:00",
    updated_at: "2026-10-20T10:00:00+08:00",
  },
];

/** 列表响应：把「计算所需配置」一并下发（前端不硬编码常量） */
const LIST = {
  items: PKGS,
  total: PKGS.length,
  page: 1,
  page_size: 20,
  balance_limit: CAP,
  // 到账 = 实付分→微 + 赠送微，没有倍率也没有刻度；只下发 scale（1 单位 = 多少微）
  scale: 1_000_000,
  recharge_multiplier: 1,
  min_price_cny_cent: 100,
  max_price_cny_cent: 10_000_000,
  max_bonus_quota: 1_000_000_000,
};

interface Captured {
  method: string;
  path: string;
  body: unknown;
}

async function stub(page: Page, captured: Captured[]): Promise<void> {
  await page.route("**/api/admin/**", (route) => {
    const req = route.request();
    const u = new URL(req.url());
    const method = req.method();

    if (u.pathname === "/api/admin/packages" && method === "GET") {
      return route.fulfill({ json: envelope(LIST) });
    }
    if (u.pathname === "/api/admin/packages" && method === "POST") {
      captured.push({ method, path: u.pathname, body: req.postDataJSON() });
      return route.fulfill({ json: envelope({ item: PKGS[0], warning: null }) });
    }
    if (/^\/api\/admin\/packages\/\d+$/.test(u.pathname) && method === "PUT") {
      captured.push({ method, path: u.pathname, body: req.postDataJSON() });
      return route.fulfill({ json: envelope({ item: PKGS[1], warning: null }) });
    }
    if (/^\/api\/admin\/packages\/\d+\/status$/.test(u.pathname) && method === "PATCH") {
      captured.push({ method, path: u.pathname, body: req.postDataJSON() });
      return route.fulfill({ json: envelope({ item: PKGS[1] }) });
    }
    if (/^\/api\/admin\/packages\/\d+$/.test(u.pathname) && method === "DELETE") {
      captured.push({ method, path: u.pathname, body: null });
      return route.fulfill({ json: envelope(null) });
    }
    return route.fulfill({ json: envelope(null) });
  });
}

test.describe("充值套餐（管理台）", () => {
  test("列表：9 列齐备、停用与超上限可视化、有引用的删除置灰", async ({ page }) => {
    await seedAdmin(page);
    const captured: Captured[] = [];
    await stub(page, captured);
    await page.goto("/admin/packages");

    await expect(page.getByRole("heading", { level: 1 })).toHaveText("充值套餐");
    // 侧栏入口与面包屑同步（新菜单项的唯一强断言）
    await expect(page.locator(".a-side nav a.active")).toContainText("充值套餐");
    await expect(page.locator(".a-top .crumb b")).toHaveText("充值套餐");

    const rows = page.locator(".tbl tbody tr");
    await expect(rows).toHaveCount(4);

    // 表头：9 列（排序/名称/售价/基础/赠送/到账/订单数/状态/操作）
    await expect(page.locator(".tbl thead th")).toHaveCount(9);
    await expect(page.locator(".tbl thead")).toContainText("到账合计");
    await expect(page.locator(".tbl thead")).toContainText("订单数");

    // 标准包：角标 + 两位小数展示
    const std = rows.filter({ hasText: "标准包" });
    await expect(std).toContainText("推荐");
    await expect(std).toContainText("$30.00");
    await expect(std).toContainText("$33.00");

    // 停用态
    await expect(rows.filter({ hasText: "专业包" }).locator(".badge")).toHaveText("停用");

    // 超上限：企业包到账 $10,000.00 > 上限 $1,000
    const corp = rows.filter({ hasText: "企业包" });
    await expect(corp).toContainText("超上限");
    await expect(corp.getByRole("button", { name: "删除" })).toBeEnabled();

    // 有引用的三行：删除置灰 + title 带笔数
    for (const name of ["体验包", "标准包", "专业包"]) {
      await expect(rows.filter({ hasText: name }).getByRole("button", { name: "删除" })).toBeDisabled();
    }
    await expect(rows.filter({ hasText: "体验包" }).getByRole("button", { name: "删除" })).toHaveAttribute(
      "title",
      /128 笔订单/,
    );
  });

  test("新建：价格按「分」、赠送按金额（微）提交，且默认 enabled=true", async ({ page }) => {
    await seedAdmin(page);
    const captured: Captured[] = [];
    await stub(page, captured);
    await page.goto("/admin/packages");

    await page.getByRole("button", { name: "新建套餐" }).click();
    const drawer = page.getByRole("dialog", { name: "新建套餐" });
    await expect(drawer).toBeVisible();

    await drawer.getByLabel("套餐名称").fill("测试包");
    await drawer.getByLabel("售价").fill("30");
    await drawer.getByLabel("赠送金额").fill("1");
    // 排序留空 → body 不应出现 sort_order（后端按 max+10 追加到末尾）
    await drawer.getByRole("button", { name: "保存" }).click();

    await expect.poll(() => captured.length).toBeGreaterThan(0);
    expect(captured[0]).toMatchObject({
      method: "POST",
      path: "/api/admin/packages",
    });
    expect(captured[0].body).toEqual({
      name: "测试包",
      price_cent: 3000,
      // 赠送按**金额**填（1 = 1 个结算货币单位 = 1_000_000 微）；到账 = 实付分→微 + 赠送微
      bonus: 1_000_000,
      badge: "",
      enabled: true,
    });
  });

  test("编辑：排序清零必须显式发送 sort_order: 0（不能当成「未传」）", async ({ page }) => {
    await seedAdmin(page);
    const captured: Captured[] = [];
    await stub(page, captured);
    await page.goto("/admin/packages");

    await page.locator(".tbl tbody tr", { hasText: "标准包" }).getByRole("button", { name: "编辑" }).click();
    const drawer = page.getByRole("dialog", { name: /编辑套餐/ });
    await expect(drawer).toBeVisible();
    // 预填：赠送是金额（微 → 十进制文本），币种精度内的往返不丢微
    await expect(drawer.getByLabel("赠送金额")).toHaveValue("3.000192");

    await drawer.getByLabel("排序").fill("0");
    await drawer.getByRole("button", { name: "保存" }).click();

    await expect.poll(() => captured.length).toBeGreaterThan(0);
    expect(captured[0].method).toBe("PUT");
    expect(captured[0].body).toMatchObject({ sort_order: 0 });
  });

  test("启停：走独立 PATCH 接口，不夹带其他字段", async ({ page }) => {
    await seedAdmin(page);
    const captured: Captured[] = [];
    await stub(page, captured);
    await page.goto("/admin/packages");

    await page.locator(".tbl tbody tr", { hasText: "标准包" }).getByRole("button", { name: "停用" }).click();

    await expect.poll(() => captured.length).toBeGreaterThan(0);
    expect(captured[0]).toMatchObject({
      method: "PATCH",
      path: "/api/admin/packages/2/status",
      body: { enabled: false },
    });
  });

  test("删除：无引用走确认弹窗后 DELETE；有引用点不动", async ({ page }) => {
    await seedAdmin(page);
    const captured: Captured[] = [];
    await stub(page, captured);
    await page.goto("/admin/packages");

    // 有引用：按钮 disabled，点击不产生请求
    const guarded = page.locator(".tbl tbody tr", { hasText: "体验包" }).getByRole("button", { name: "删除" });
    await expect(guarded).toBeDisabled();
    await guarded.click({ force: true });
    await page.waitForTimeout(150);
    expect(captured).toHaveLength(0);

    // 无引用：确认后 DELETE
    await page.locator(".tbl tbody tr", { hasText: "企业包" }).getByRole("button", { name: "删除" }).click();
    const dlg = page.getByRole("dialog", { name: "删除套餐" });
    await expect(dlg).toBeVisible();
    await dlg.getByRole("button", { name: "删除" }).click();

    await expect.poll(() => captured.length).toBeGreaterThan(0);
    expect(captured[0]).toMatchObject({ method: "DELETE", path: "/api/admin/packages/4" });
  });

  test("移动端 375px：页面无横向滚动", async ({ page }) => {
    await seedAdmin(page);
    await stub(page, []);
    await page.setViewportSize({ width: 375, height: 780 });
    await page.goto("/admin/packages");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("充值套餐");

    await expectNoHScroll(page);
  });
});

/**
 * 表格布局契约（768px，**有数据**）。
 *
 * 为什么单独一条：既有的「响应式无横向滚动」扫描全部用空数据桩，表格恒为空态 ——
 * 列的压缩、溢出、按钮换行全在"有行"时才发生，所以那套用例一个都抓不到。
 * 这里用套餐页的真实 4 行数据把三条硬契约钉死。
 */
/**
 * 计算样式的背景色是否不透明。
 * 浏览器可能返回 rgb()/rgba()/color(srgb …)/color(srgb … / a)/oklab(…)，逐个判 alpha。
 */
function isOpaque(bg: string): boolean {
  if (bg === "rgba(0, 0, 0, 0)" || bg === "transparent") return false;
  const rgba = bg.match(/^rgba\(([^)]+)\)$/);
  if (rgba) {
    const parts = rgba[1].split(",").map((s) => s.trim());
    return parts.length < 4 || Number(parts[3]) === 1;
  }
  const withSlash = bg.match(/\/\s*([\d.]+)\s*\)$/);
  if (withSlash) return Number(withSlash[1]) === 1;
  // rgb(r g b) / color(srgb …) / oklab(…) 不带 alpha 通道 → 不透明
  return true;
}

test.describe("表格布局契约（768px）", () => {
  test.use({ viewport: { width: 768, height: 900 } });

  test("名称列不被压塌、操作列吸右可达、表头不溢出、按钮不换行", async ({ page }) => {
    await seedAdmin(page);
    await stub(page, []);
    await page.goto("/admin/packages");
    await expect(page.locator(".tbl tbody tr")).toHaveCount(4);

    const m = await page.evaluate(() => {
      const ths = [...document.querySelectorAll<HTMLElement>(".tbl th")].filter(
        (t) => t.offsetParent !== null,
      );
      const nameCell = document.querySelector<HTMLElement>('.tbl td[data-label="名称"] .t-card-min')!;
      const pin = document.querySelector<HTMLElement>(".tbl td.is-pin")!;
      const acts = document.querySelector<HTMLElement>(".tbl .t-acts")!;
      return {
        thOverflow: ths.filter((t) => t.scrollWidth > t.clientWidth + 1).map((t) => t.textContent),
        nameW: Math.round(nameCell.getBoundingClientRect().width),
        pinRight: Math.round(pin.getBoundingClientRect().right),
        vw: window.innerWidth,
        wrap: getComputedStyle(acts).flexWrap,
        rows: new Set(
          [...acts.querySelectorAll(".t-act")].map((a) => Math.round(a.getBoundingClientRect().top)),
        ).size,
      };
    });

    expect(m.thOverflow, "表头文字溢出会压到相邻表头").toEqual([]);
    expect(m.nameW, "名称列被压塌（竖排单字的成因）").toBeGreaterThanOrEqual(150);
    expect(m.pinRight, "操作列被推出可视区 —— 吸右失效").toBeLessThanOrEqual(m.vw + 1);
    expect(m.wrap, "操作按钮允许换行，会挤成多行").toBe("nowrap");
    expect(m.rows, "操作按钮折成了多行").toBe(1);
    await expectNoHScroll(page);
  });
});

/**
 * 悬停底色必须是**不透明表面**。
 *
 * 操作列吸右时是 `position:sticky`：它靠自身背景遮住横向滚到它下面的列。
 * 一旦悬停底色是半透明（曾用 `--primary-bg` = 10% 透明），下层列的文字会透出来，
 * 与图标按钮叠在一起 —— 768px 实测「启用」标签穿进操作列。
 * 这里不依赖「当前是否真的溢出」：不透明是穿透的必要条件，任何宽度都该成立。
 */
test.describe("表格悬停表面契约", () => {
  test("表格态：吸右操作列悬停底色不透明、且与普通单元格同色", async ({ page }) => {
    await page.setViewportSize({ width: 768, height: 900 });
    await seedAdmin(page);
    await stub(page, []);
    await page.goto("/admin/packages");
    await expect(page.locator(".tbl tbody tr")).toHaveCount(4);

    const row = page.locator(".tbl tbody tr").nth(1);
    await row.hover();
    // 行上有 transition:.12s，不等会读到过渡起始帧的白色
    await page.waitForTimeout(260);

    const m = await page.evaluate(() => {
      const tr = document.querySelectorAll<HTMLElement>(".tbl tbody tr")[1];
      const pin = tr.querySelector<HTMLElement>("td.is-pin")!;
      const plain = tr.querySelectorAll<HTMLElement>("td")[1]; // 名称列（无 data-p，任何断点都可见）
      return {
        pin: getComputedStyle(pin).backgroundColor,
        plain: getComputedStyle(plain).backgroundColor,
        pos: getComputedStyle(pin).position,
      };
    });

    expect(m.pos, "操作列没有吸右，本条断言失去前提").toBe("sticky");
    expect(isOpaque(m.pin), `吸右操作列悬停底色半透明会透出下层列：${m.pin}`).toBe(true);
    expect(m.pin, "吸右格与普通格悬停底色不一致（会出现白块/异色块）").toBe(m.plain);
  });

  test("卡片态：悬停底色由整卡承载，操作行不能是白块", async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 900 });
    await seedAdmin(page);
    await stub(page, []);
    await page.goto("/admin/packages");
    await expect(page.locator(".tbl tbody tr")).toHaveCount(4);

    await page.locator(".tbl tbody tr").nth(1).hover();
    await page.waitForTimeout(260);

    const m = await page.evaluate(() => {
      const tr = document.querySelectorAll<HTMLElement>(".tbl tbody tr")[1];
      const pin = tr.querySelector<HTMLElement>("td.is-pin")!;
      return {
        tr: getComputedStyle(tr).backgroundColor,
        pin: getComputedStyle(pin).backgroundColor,
        pos: getComputedStyle(pin).position,
      };
    });

    expect(m.pos, "卡片态不该再吸右").toBe("static");
    expect(isOpaque(m.tr), `卡片悬停底色半透明：${m.tr}`).toBe(true);
    expect(
      m.pin === "rgba(0, 0, 0, 0)" || m.pin === m.tr,
      `操作行必须是整卡底色的一部分（现在是独立色块）：${m.pin}`,
    ).toBe(true);
  });
});
