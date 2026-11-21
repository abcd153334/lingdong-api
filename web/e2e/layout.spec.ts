/** S8 布局契约用例（M6 栅格 / M8 重复 id 与标签单命中 / M14 键盘可达）
 *
 * 依据 `ui-consistency-refactor-plan-v3.md` §7：
 *   M6  `.grid-X` 渲染 display:grid 且列数符合断点（含 .models-grid）
 *   M8  同页无重复 id；同一可访问名单命中（files.spec:128 / F10 回归）
 *   M14 Esc 关闭弹层、初始聚焦、关闭后焦点归还、交互元素聚焦可见
 *
 * 全部为契约级（page.route 打桩）用例，与后端可达性无关。
 */
import { expect, test, type Page } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

/** 门户模型页 /api/pricing 快照（三型覆盖，保证 .models-grid 有卡片可测） */
const PRICING = {
  models: [
    {
      id: "gpt-4o",
      display_name: "GPT-4o",
      vendor: "openai",
      vendor_name: "OpenAI",
      vendor_color: "#10a37f",
      modality: "chat",
      quota_type: 0,
      model_ratio: 2.5,
      completion_ratio: 2,
      cache_ratio: 0.5,
      create_cache_ratio: null,
      create_cache_ratio_1h: null,
      image_ratio: null,
      audio_ratio: null,
      audio_completion_ratio: null,
      model_price_quota: null,
      spec_prices: null,
      is_default: true,
      endpoints: ["/v1/chat/completions"],
    },
    {
      id: "claude-3.7",
      display_name: "Claude 3.7",
      vendor: "anthropic",
      vendor_name: "Anthropic",
      vendor_color: "#d97757",
      modality: "chat",
      quota_type: 0,
      model_ratio: 3,
      completion_ratio: 3,
      cache_ratio: null,
      create_cache_ratio: null,
      create_cache_ratio_1h: null,
      image_ratio: null,
      audio_ratio: null,
      audio_completion_ratio: null,
      model_price_quota: null,
      spec_prices: null,
      is_default: false,
      endpoints: ["/v1/chat/completions"],
    },
    {
      id: "tts-1",
      display_name: "TTS-1",
      vendor: "openai",
      vendor_name: "OpenAI",
      vendor_color: "#10a37f",
      modality: "audio",
      quota_type: 1,
      model_ratio: null,
      completion_ratio: null,
      cache_ratio: null,
      create_cache_ratio: null,
      create_cache_ratio_1h: null,
      image_ratio: null,
      audio_ratio: null,
      audio_completion_ratio: null,
      model_price_quota: 20000,
      spec_prices: null,
      is_default: false,
      endpoints: ["/v1/audio/speech"],
    },
  ],
  vendors: [
    { id: "openai", name: "OpenAI", color: "#10a37f", priority: 1, sort: 1, builtin: true },
    {
      id: "anthropic",
      name: "Anthropic",
      color: "#d97757",
      priority: 2,
      sort: 2,
      builtin: true,
    },
  ],
  group_ratios: { default: 1 },
  quota_per_unit: 500000,
  cny_per_usd: 7.2,
};

/** 通用桩：未单独打桩的 /api/** 统一空载荷，避免 401 触发全局登出（须先于 seedAuth 注册） */
async function stubApi(page: Page): Promise<void> {
  await page.route(
    (url) => url.pathname.startsWith("/api/"),
    (route) => route.fulfill({ json: envelope(null) }),
  );
}

/** 取元素计算样式中的 display 与栅格列数 */
async function gridInfo(
  page: Page,
  selector: string,
): Promise<{ display: string; columns: number }> {
  return page.locator(selector).first().evaluate((el) => {
    const cs = getComputedStyle(el);
    return {
      display: cs.display,
      // 计算值形如 "444.4px 444.4px 444.4px"；auto-fit/auto-fill 已展开为具体轨道数
      columns: cs.gridTemplateColumns.split(" ").filter(Boolean).length,
    };
  });
}

/** 断言某选择器在给定宽度下的列数 */
async function expectColumns(
  page: Page,
  opts: { path: string; selector: string; width: number; columns: number },
): Promise<void> {
  const { path, selector, width, columns } = opts;
  await page.setViewportSize({ width, height: 900 });
  await page.goto(path, { waitUntil: "domcontentloaded" });
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  await expect(page.locator(selector).first(), `${path} 缺 ${selector}`).toBeVisible();
  const info = await gridInfo(page, selector);
  expect(info.display, `${path} ${selector} display 应为 grid`).toBe("grid");
  expect(info.columns, `${path} ${selector} 在 ${width}px 列数`).toBe(columns);
}

test.describe("M6 栅格契约：display:grid 且列数随断点收敛", () => {
  test("仪表盘 .grid-3（KPI 卡）：1440→3 / 768→2 / 375→1", async ({ page }) => {
    await stubApi(page);
    await seedAuth(page, { role: 0 });
    for (const [width, columns] of [
      [1440, 3],
      [768, 2],
      [375, 1],
    ] as const) {
      await expectColumns(page, { path: "/admin/dashboard", selector: ".grid-3", width, columns });
    }
  });

  test("仪表盘 .grid-2（失败 Top5 / 渠道健康）：1440→2 / 768→2 / 375→1", async ({ page }) => {
    await stubApi(page);
    await seedAuth(page, { role: 0 });
    for (const [width, columns] of [
      [1440, 2],
      [768, 2],
      [375, 1],
    ] as const) {
      await expectColumns(page, { path: "/admin/dashboard", selector: ".grid-2", width, columns });
    }
  });

  test("展示台 .grid-4：1440→4 / 768→2 / 375→1", async ({ page }) => {
    await stubApi(page);
    await seedAuth(page, { role: 0 });
    for (const [width, columns] of [
      [1440, 4],
      [768, 2],
      [375, 1],
    ] as const) {
      await expectColumns(page, { path: "/__showcase", selector: ".grid-4", width, columns });
    }
  });

  test("模型广场 .models-grid：1440→3 / 768→2 / 375→1", async ({ page }) => {
    await stubApi(page);
    await seedAuth(page, { role: 0 });
    await page.route("**/api/pricing**", (route) => route.fulfill({ json: envelope(PRICING) }));
    for (const [width, columns] of [
      [1440, 3],
      [768, 2],
      [375, 1],
    ] as const) {
      await expectColumns(page, { path: "/models", selector: ".models-grid", width, columns });
    }
  });

  test("门户主页 .defaults-grid（各类型默认模型）：1440→2 / 768→2 / 375→1", async ({ page }) => {
    await stubApi(page);
    await seedAuth(page, { role: 0 });
    await page.route("**/api/pricing**", (route) => route.fulfill({ json: envelope(PRICING) }));
    // 「谁是默认模型」的唯一真相源是 /portal/defaults（pricing 的 is_default 没有任何写入口）。
    // 不桩会走 EMPTY_DEFAULTS → 卡片渲染空态，.defaults-grid 根本不存在，栅格契约无从测量。
    await page.route("**/api/portal/defaults**", (route) =>
      route.fulfill({
        json: envelope({
          chat: { model: "gpt-4o", any_key_allowed: true },
          image: { model: "dall-e-3", any_key_allowed: true },
        }),
      }),
    );
    for (const [width, columns] of [
      [1440, 2],
      [768, 2],
      [375, 1],
    ] as const) {
      await expectColumns(page, { path: "/home", selector: ".defaults-grid", width, columns });
    }
  });

  test("门户主页首屏：资金卡与四指标大卡同排同高，趋势卡整行", async ({ page }) => {
    await stubApi(page);
    await seedAuth(page, { role: 0 });
    await page.goto("/home");
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await expect(page.locator(".kpi-card")).toBeVisible();

    const measure = () =>
      page.evaluate(() => {
        const rect = (sel: string) => document.querySelector(sel)?.getBoundingClientRect() ?? null;
        const b = rect(".balance-card");
        const k = rect(".kpi-card");
        const t = rect("[data-testid='trend-card']");
        const bento = rect(".bento");
        if (!b || !k || !t || !bento) return null;
        return {
          bTop: b.top,
          bBottom: b.bottom,
          bRight: b.right,
          kTop: k.top,
          kBottom: k.bottom,
          kLeft: k.left,
          tTop: t.top,
          tWidth: t.width,
          bentoWidth: bento.width,
        };
      });

    // 桌面：两张卡同排、等高，趋势卡另起一整行
    await page.setViewportSize({ width: 1440, height: 900 });
    const wide = await measure();
    expect(wide, "缺少 .balance-card / .kpi-card / trend-card").not.toBeNull();
    if (!wide) return;
    expect(Math.abs(wide.kTop - wide.bTop)).toBeLessThanOrEqual(1);
    // 等高：高度由较高的一侧撑开，另一侧靠 flex 把按钮贴到底
    expect(Math.abs(wide.kBottom - wide.bBottom)).toBeLessThanOrEqual(1);
    expect(wide.kLeft).toBeGreaterThan(wide.bRight);
    expect(wide.tWidth).toBeGreaterThanOrEqual(wide.bentoWidth - 2);
    expect(wide.tTop).toBeGreaterThan(wide.bBottom);

    // 768：网格降成 2 列，两卡各自整行；趋势卡仍必须撑满（span-4 在 2 列里要归一成 span-2，
    // 否则会生成两条隐式列）
    await page.setViewportSize({ width: 768, height: 900 });
    const narrow = await measure();
    expect(narrow).not.toBeNull();
    if (!narrow) return;
    expect(narrow.kTop).toBeGreaterThan(narrow.bBottom);
    expect(narrow.tWidth).toBeGreaterThanOrEqual(narrow.bentoWidth - 2);
  });

  test("模型服务：左右两栏贴边（无页面内边距、左栏定宽、右栏吃满剩余）", async ({ page }) => {
    await stubApi(page);
    await seedAuth(page, { role: 0 });
    await page.route("**/api/admin/adapters**", (route) =>
      route.fulfill({
        json: envelope({
          items: [
            {
              id: "openai",
              name: "openai",
              label: "OpenAI",
              vendor: "openai",
              caps: ["text"],
              status: 1,
              source: "builtin",
              editable: false,
              deletable: false,
              channel_count: 0,
              endpoints: [],
            },
          ],
        }),
      }),
    );
    await page.route("**/api/admin/channels**", (route) =>
      route.fulfill({
        json: envelope({ items: [], adapters: [], groups: ["default"], total: 0 }),
      }),
    );
    await page.route("**/api/admin/models**", (route) =>
      route.fulfill({ json: envelope({ items: [], total: 0, vendors: [], tag_palette: [] }) }),
    );

    for (const width of [1440, 900, 375]) {
      await page.setViewportSize({ width, height: 900 });
      await page.goto("/admin/model-service", { waitUntil: "domcontentloaded" });
      await expect(page.getByRole("heading", { level: 1 })).toHaveText("模型服务");

      const box = await page.evaluate(() => {
        const content = document.querySelector<HTMLElement>(".a-content")!;
        const ws = document.querySelector<HTMLElement>(".ms-ws")!;
        const rail = document.querySelector<HTMLElement>(".ms-rail")!;
        const main = document.querySelector<HTMLElement>(".ms-main")!;
        const cs = getComputedStyle(content);
        return {
          pad: [cs.paddingTop, cs.paddingRight, cs.paddingBottom, cs.paddingLeft].map((v) =>
            Math.round(parseFloat(v)),
          ),
          contentW: Math.round(content.getBoundingClientRect().width),
          wsW: Math.round(ws.getBoundingClientRect().width),
          railW: Math.round(rail.getBoundingClientRect().width),
          mainW: Math.round(main.getBoundingClientRect().width),
        };
      });
      // 四边完全贴边：任何一边留白都会让"两栏分界"看起来像一张卡片浮在页面上
      expect(box.pad, `${width}px 页面内边距`).toEqual([0, 0, 0, 0]);
      expect(box.wsW, `${width}px 工作区应吃满内容宽`).toBe(box.contentW);
      // 两栏左右并排：右栏 = 剩余宽度（≤899 左栏转抽屉，右栏独占）
      if (width > 899) {
        expect(box.railW, `${width}px 左栏应定宽`).toBeGreaterThanOrEqual(240);
        expect(box.railW + box.mainW, `${width}px 两栏合计应等于工作区`).toBe(box.wsW);
      } else {
        expect(box.mainW, `${width}px 右栏应独占`).toBe(box.wsW);
      }
    }
  });
});

/**
 * M8 覆盖的页面集合（门户 7 + 管理台 9 + 公开静态 2 = 18）。
 *
 * 管理台从 11 降到 9：模型管理 / 渠道管理 / 适配器三条路径合并成一个「模型服务」。
 * 插件数据页不再是独立路由（在应用管理弹窗的页签里），它的 id 唯一性由
 * `admin-apps-plugins.spec.ts` 的「两个插件页签同时保活」用例守。
 * 公告两端都是顶栏弹层，不是页面。
 */
const ID_PATHS = [
  "/home",
  "/models",
  "/keys",
  "/usage",
  "/files",
  "/topup",
  "/settings",
  "/admin/dashboard",
  "/admin/users",
  "/admin/model-service",
  "/admin/orders",
  "/admin/redeems",
  "/admin/logs",
  "/admin/tasks",
  "/admin/config",
  "/admin/audit",
  "/about",
  "/agreement",
];

test.describe("M8 同页无重复 id / 可访问名单命中", () => {
  test("18 个业务页 DOM 内 id 唯一", async ({ page }) => {
    test.setTimeout(120_000);
    await stubApi(page);
    await seedAuth(page, { role: 0 });
    for (const path of ID_PATHS) {
      await page.goto(path, { waitUntil: "domcontentloaded" });
      await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
      const dupes = await page.evaluate(() => {
        const seen = new Map<string, number>();
        document.querySelectorAll("[id]").forEach((el) => {
          const id = el.id;
          if (!id) return;
          seen.set(id, (seen.get(id) ?? 0) + 1);
        });
        return [...seen.entries()].filter(([, n]) => n > 1).map(([id, n]) => `${id}×${n}`);
      });
      expect(dupes, `${path} 存在重复 id：${dupes.join(", ")}`).toEqual([]);
    }
  });

  test("调用记录筛选条：桌面态『密钥』标签仅命中 1 个控件（F10 回归）", async ({ page }) => {
    await stubApi(page);
    await seedAuth(page, { role: 0 });
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto("/usage");
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    // Sheet 字段按需渲染（sheetOpen 前不进 DOM），故桌面态严格模式单命中
    await expect(page.getByLabel("密钥")).toHaveCount(1);
  });
});

test.describe("M14 键盘可达：Esc 关闭 / 初始聚焦 / 焦点归还 / 聚焦可见", () => {
  test("Dialog：打开即聚焦内部、Esc 关闭、焦点归还触发器", async ({ page }) => {
    await stubApi(page);
    await seedAuth(page, { role: 0 });
    await page.goto("/__showcase");
    const trigger = page.getByRole("button", { name: "打开对话框" });
    await trigger.click();

    // 展示台同时存在 FilterBar 的 Sheet（同为 .dialog 骨架），故按可访问名定位目标弹窗
    const dialog = page.getByRole("dialog", { name: "对话框标题" });
    await expect(dialog).toBeVisible();
    // 初始聚焦：焦点在弹层可见后的数帧内落入弹窗内部（rAF 补偿，非同步）
    await expect
      .poll(
        () =>
          page.evaluate(() =>
            !!document.querySelector(".dialog.open")?.contains(document.activeElement),
          ),
        { message: "打开后焦点未进入弹窗", timeout: 3000 },
      )
      .toBe(true);

    await page.keyboard.press("Escape");
    await expect(dialog).toBeHidden();
    // 关闭后焦点归还打开它的按钮
    await expect(trigger).toBeFocused();
  });

  test("Drawer：Esc 关闭且焦点归还", async ({ page }) => {
    await stubApi(page);
    await seedAuth(page, { role: 0 });
    await page.goto("/__showcase");
    const trigger = page.getByRole("button", { name: "打开抽屉" });
    await trigger.click();

    const drawer = page.locator(".drawer").first();
    await expect(drawer).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(drawer).toBeHidden();
    await expect(trigger).toBeFocused();
  });

  test("键盘 Tab 聚焦到按钮时出现 2px 主色焦点环", async ({ page }) => {
    await stubApi(page);
    await seedAuth(page, { role: 0 });
    await page.goto("/__showcase");
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();

    // 逐次 Tab 直到命中有 .btn 的行内按钮（最多 40 跳，避免依赖具体 DOM 顺序）
    let hit = false;
    for (let i = 0; i < 40; i++) {
      await page.keyboard.press("Tab");
      hit = await page.evaluate(() => {
        const el = document.activeElement as HTMLElement | null;
        return !!el && el.matches("button.btn, a.btn");
      });
      if (hit) break;
    }
    expect(hit, "Tab 40 次未聚焦到 .btn 元素").toBe(true);

    const outline = await page.evaluate(() => {
      const el = document.activeElement as HTMLElement;
      const cs = getComputedStyle(el);
      return {
        visible: el.matches(":focus-visible"),
        style: cs.outlineStyle,
        width: Number.parseFloat(cs.outlineWidth),
      };
    });
    expect(outline.visible, "键盘聚焦未命中 :focus-visible").toBe(true);
    expect(outline.style, "焦点环 outline-style 缺失").toBe("solid");
    expect(outline.width, "焦点环宽度应 ≥2px").toBeGreaterThanOrEqual(2);
  });
});

/**
 * M16 顶栏对齐契约（本次补充，防「动作组随面包屑漂移」回归）
 *
 * 背景：门户 `.p-right` 原依赖 `.p-nav` 的 flex:1 撑开，≤767px 导航隐藏后动作组回缩贴住 logo；
 * 管理台 `.a-top` 的动作按钮直接排在内联流里，桌面端紧贴面包屑、右侧留出大片空白。
 * 契约：两侧顶栏的动作组（公告/主题/头像）在任何断点都必须贴住容器右边缘（仅保留容器 padding）。
 */
test.describe("M16 顶栏对齐：动作组贴右（桌面与移动一致）", () => {
  /** 动作组右边缘到顶栏右边缘的距离（应等于顶栏右侧 padding） */
  async function rightGap(page: Page, bar: string, acts: string): Promise<number> {
    return page.evaluate(
      ([barSel, actsSel]) => {
        const b = document.querySelector(barSel as string)?.getBoundingClientRect();
        const a = document.querySelector(actsSel as string)?.getBoundingClientRect();
        if (!b || !a) return -1;
        return Math.round(b.right - a.right);
      },
      [bar, acts],
    );
  }

  test("管理台顶栏：1440→24 / 390→14 / 320→14", async ({ page }) => {
    await stubApi(page);
    await seedAuth(page, { role: 0 });
    for (const [width, pad] of [
      [1440, 24],
      [390, 14],
      [320, 14],
    ] as const) {
      await page.setViewportSize({ width, height: 800 });
      await page.goto("/admin/dashboard", { waitUntil: "domcontentloaded" });
      await expect(page.locator(".a-top .a-acts")).toBeVisible();
      const gap = await rightGap(page, ".a-top", ".a-top .a-acts");
      expect(gap, `管理台顶栏动作组在 ${width}px 未贴右（右留白 ${gap}px）`).toBeGreaterThanOrEqual(pad - 2);
      expect(gap, `管理台顶栏动作组在 ${width}px 未贴右（右留白 ${gap}px）`).toBeLessThanOrEqual(pad + 2);
    }
  });

  test("门户顶栏：1440→40 / 390→14 / 320→14（导航隐藏后不得回缩）", async ({ page }) => {
    await stubApi(page);
    await seedAuth(page);
    for (const [width, pad] of [
      [1440, 40],
      [390, 14],
      [320, 14],
    ] as const) {
      await page.setViewportSize({ width, height: 800 });
      await page.goto("/home", { waitUntil: "domcontentloaded" });
      await expect(page.locator(".p-top .p-right")).toBeVisible();
      const gap = await rightGap(page, ".p-top", ".p-top .p-right");
      expect(gap, `门户顶栏动作组在 ${width}px 未贴右（右留白 ${gap}px）`).toBeGreaterThanOrEqual(pad - 2);
      expect(gap, `门户顶栏动作组在 ${width}px 未贴右（右留白 ${gap}px）`).toBeLessThanOrEqual(pad + 2);
    }
  });

  test("管理台移动端：面包屑让位给页面标题（不出现「管理台 / 仪…」截断）", async ({ page }) => {
    await stubApi(page);
    await seedAuth(page, { role: 0 });
    await page.setViewportSize({ width: 320, height: 800 });
    await page.goto("/admin/dashboard", { waitUntil: "domcontentloaded" });
    await expect(page.locator(".a-top .crumb b")).toHaveText("仪表盘");
    await expect(page.locator(".a-top .a-crumb-root")).toBeHidden();
    // 标题完整可见：scrollWidth 未超出 clientWidth
    const clipped = await page.locator(".a-top .crumb").evaluate((el) => el.scrollWidth > el.clientWidth + 1);
    expect(clipped, "320px 下面包屑被截断").toBe(false);
  });
});
