/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { canHScroll, envelope, expectNoHScroll, seedAuth } from "./helpers";

const WIDTHS = [375, 768, 1024, 1440];
/** 22 个业务页：门户 7 + 管理台 11 + 公开静态内容 2 + 页签深链 2 */
const PATHS = [
  "/home",
  "/models",
  "/keys",
  "/usage",
  "/files",
  "/topup",
  "/settings",
  "/admin/dashboard",
  "/admin/users",
  // 两栏（适配器→渠道级联 / 模型表）：窄屏时左栏转抽屉，最容易横向溢出
  "/admin/model-service",
  // 三栏 grid（会话列表 / 对话 / 提案）：窄屏最容易横向溢出的新页面
  "/admin/agent",
  "/admin/orders",
  "/admin/redeems",
  "/admin/logs",
  "/admin/tasks",
  "/admin/config",
  "/admin/audit",
  // 接入应用（卡片列表 + 管理弹窗）：弹窗内页签化后，插件数据页也在弹窗里，
  // 它的 375 无溢出由 admin-apps-plugins.spec.ts 覆盖。
  "/admin/apps",
  "/about",
  "/agreement",
  // 页签深链：验证「7 个页签 / 卡内两组页签」在窄屏横滚时仍无布局溢出
  "/admin/config?tab=security",
  "/topup?tab=redeem",
];
const SHOT_WIDTHS = [375, 768, 1440];

function slug(path: string): string {
  return path.replace(/[^\w]+/g, "_").replace(/^_+|_+$/g, "") || "root";
}

/** 关键页含门户与管理台，统一以管理员会话进入以通过路由守卫 */
const seedAdmin = (page: Parameters<typeof seedAuth>[0]) => seedAuth(page, { role: 0 });

/**
 * 数据页通用桩：`/api/**` 未单独打桩的接口统一返回空载荷（envelope(null)），
 * 使数据页渲染空态/降级态而非因后端 401 触发全局登出。
 * 必须在 seedAdmin 之前注册：后注册者优先，故 seedAuth 的专用桩仍会覆盖本通用桩。
 */
async function stubApi(page: Page): Promise<void> {
  await page.route(
    (url) => url.pathname.startsWith("/api/"),
    (route) => route.fulfill({ json: envelope(null) }),
  );
}

/** 逐档逐页断言无横向溢出 */
test.describe("响应式无横向滚动", () => {
  for (const width of WIDTHS) {
    test(`宽度 ${width}px 下关键页无横向滚动`, async ({ page }) => {
      // 断言对象是「布局溢出」（测量 documentElement.scrollWidth），与页面何时完成加载无关。
      // 本用例在 30s 内连续导航 22 个页面，工装负载下（VDI + Defender 实时扫描）单页变慢即可
      // 拖垮整档（实测 /admin/tasks 的 goto 停在 load 事件超时），故改用 domcontentloaded 并放宽预算。
      // 22 路径 × 4 档：追加页签深链后整体耗时上升，故放宽到 150s（仍低于全局 30s×默认）
      test.setTimeout(150_000);
      await stubApi(page);
      await seedAdmin(page);
      await page.setViewportSize({ width, height: 900 });
      for (const path of PATHS) {
        await page.goto(path, { waitUntil: "domcontentloaded" });
        await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
        // 溢出是**持续状态**，但「h1 可见」还不等于布局定型：vite dev 的 CSS 由 JS 注入，
        // domcontentloaded 之后仍有最后一次重排（实测 /home 顶栏右簇此刻 right=789 / vw=768，
        // 1.5s 后回到 768 且无一元素越界）。一次性测量会把这种重排前的中间态判成布局 bug。
        // 故轮询到稳定：真实溢出轮询到超时仍溢出 → 判据没有放松，只是去掉了测量时机的不确定性。
        await expect
          .poll(() => canHScroll(page), {
            message: `${path} 在 ${width}px 横向溢出`,
            timeout: 5_000,
          })
          .toBe(false);
      }
    });
  }
});

/** 三档 × 双主题留档截图（验收证据） */
test.describe("验收截图（三档 × 双主题）", () => {
  for (const width of SHOT_WIDTHS) {
    for (const theme of ["light", "dark"] as const) {
      test(`截图 ${width}px ${theme}`, async ({ page }) => {
        await seedAdmin(page);
        await page.setViewportSize({ width, height: 900 });
        await page.goto("/home");
        if (theme === "dark") {
          await page.getByRole("button", { name: /深色主题/ }).click();
          await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
        }
        await page.screenshot({
          path: `e2e/artifacts/home-${width}-${theme}.png`,
          fullPage: true,
        });
        await page.goto("/__showcase");
        await page.screenshot({
          path: `e2e/artifacts/showcase-${width}-${theme}-${slug("/__showcase")}.png`,
          fullPage: true,
        });
      });
    }
  }
});

/** S2 账号页 375px 触控/溢出基线 */
test.describe("375px 账号页无横向滚动", () => {
  test.use({ viewport: { width: 375, height: 780 } });

  const ME = {
    id: 1,
    username: "alice",
    role: 0,
    status: 1,
    group: "default",
    balance: 123456,
    frozen: 0,
    used: 1000,
    request_count: 7,
    key_count: 1,
    key_count_enabled: 1,
    group_discount: 1_000_000,
    group_configured: false,
    email: "alice@example.com",
    phone: "13800001234",
  };

  test("公开页：登录 / 注册", async ({ page }) => {
    for (const path of ["/login", "/register"]) {
      await page.goto(path);
      await expect(page.locator(".auth-card")).toBeVisible();
      await expectNoHScroll(page, `${path} 溢出`);
    }
  });

  test("受保护页：用户中心 / 用户管理", async ({ page }) => {
    await seedAdmin(page);
    await page.route("**/api/portal/me", (route) => route.fulfill({ json: envelope(ME) }));
    await page.route("**/api/admin/users**", (route) =>
      route.fulfill({
        json: envelope({ items: [], total: 0, page: 1, page_size: 20 }),
      }),
    );
    for (const path of ["/settings", "/admin/users"]) {
      await page.goto(path);
      await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
      await expectNoHScroll(page, `${path} 溢出`);
    }
  });
});

/**
 * T-18：375px 下代表性控件的实际命中区 ≥44×44。
 * 命中区 = 元素视觉盒 ∪ ::after 扩展盒（components.css 以 min-width/min-height:44px 居中扩展），
 * 故断言「视觉尺寸与 ::after 最小尺寸取大」后的盒 ≥44，并校验 ::after 扩展确实生效。
 */
test.describe("375px 触控目标 ≥44px", () => {
  test.use({ viewport: { width: 375, height: 780 } });

  /** 一行模型数据（形状对齐 /admin/model-service 右栏契约），使行内 .t-act 渲染 */
  const MODEL = {
    id: "gpt-4o",
    display_name: "GPT-4o",
    vendor: "openai",
    vendor_name: "OpenAI",
    vendor_color: "#10a37f",
    modality: "chat",
    quota_type: 0,
    model_ratio: 1.25,
    completion_ratio: null,
    cache_ratio: null,
    create_cache_ratio: null,
    create_cache_ratio_1h: null,
    image_ratio: null,
    audio_ratio: null,
    audio_completion_ratio: null,
    model_price_quota: null,
    spec_prices: null,
    is_default: true,
    endpoints: null,
    input_price: 3,
    output_price: 15,
    cache_price: null,
    cache_create_price: null,
    image_price: null,
    audio_price: null,
    audio_output_price: null,
    call_price: null,
  };
  const VENDORS = [
    { id: "openai", name: "OpenAI", color: "#10a37f", priority: 1, sort: 1, builtin: true },
  ];

  interface HitBox {
    expanded: boolean;
    w: number;
    h: number;
    left: number;
    top: number;
    right: number;
    bottom: number;
  }

  /** 计算控件命中区（视觉盒 ∪ ::after 扩展盒）；隐藏控件（0×0）不计入 */
  async function hitBoxes(page: Page, selector: string): Promise<HitBox[]> {
    return page.locator(selector).evaluateAll((els) =>
      els
        .map((el): HitBox => {
          const r = el.getBoundingClientRect();
          const after = getComputedStyle(el, "::after");
          const expanded = after.content !== "none" && after.position === "absolute";
          const minW = expanded ? Number.parseFloat(after.minWidth) : 0;
          const minH = expanded ? Number.parseFloat(after.minHeight) : 0;
          const w = Math.max(r.width, Number.isFinite(minW) ? minW : 0);
          const h = Math.max(r.height, Number.isFinite(minH) ? minH : 0);
          const cx = r.left + r.width / 2;
          const cy = r.top + r.height / 2;
          return {
            expanded,
            w,
            h,
            left: cx - w / 2,
            top: cy - h / 2,
            right: cx + w / 2,
            bottom: cy + h / 2,
          };
        })
        .filter((b) => b.w > 0 && b.h > 0),
    );
  }

  test("头部按钮 / 顶栏图标按钮 / 行内图标按钮：命中区 ≥44×44 且互不遮挡", async ({ page }) => {
    await stubApi(page);
    await seedAdmin(page);
    // 覆盖通用桩给出至少一行数据（后注册者优先）。
    // 渠道与适配器也要给：`?c=1` 只有在渠道列表里找得到时才渲染右栏的表格，
    // 否则页面停在引导态，行内按钮一个都不存在（断言会以"未匹配到控件"失败）。
    await page.route("**/api/admin/models**", (route) =>
      route.fulfill({ json: envelope({ items: [MODEL], vendors: VENDORS, tag_palette: [] }) }),
    );
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
              channel_count: 1,
              endpoints: [],
            },
          ],
        }),
      }),
    );
    await page.route("**/api/admin/channels**", (route) =>
      route.fulfill({
        json: envelope({
          items: [
            {
              id: 1,
              name: "OpenAI 官方",
              icon: "",
              icon_url: "",
              adapter_id: "openai",
              base_url: "https://api.openai.com",
              multi_key_mode: "random",
              groups: ["default"],
              weight: 1,
              priority: 1,
              rpm_limit: 0,
              max_concurrency: 0,
              status: 1,
              auto_ban: true,
              balance_cny: null,
              key_count: 1,
              key_enabled_count: 1,
              adapter_config: {},
              adapter_options: null,
              bound_models: [],
            },
          ],
          adapters: [],
          groups: ["default"],
          total: 1,
        }),
      }),
    );

    await page.goto("/admin/model-service?c=1");
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();

    const groups: [string, string, number][] = [
      // 合并页没有页头，动作按钮在筛选条右侧（.pbar .acts）与渠道条上；
      // 只量筛选条那一组：它是每页都有的稳定位置。
      ["页面动作按钮", ".pbar .acts .btn", 44],
      ["顶栏图标按钮", ".a-top .icon-btn", 44],
      // 表格行内图标按钮：卡片态视觉 36 + 间距 8 = 44，::after 扩到 44 后相邻命中区正好相接。
      // 桌面表格态是 28×28 的鼠标目标（WCAG 2.5.8 AA ≥24），44 的触控标准在本档保证。
      ["行内图标按钮", ".tbl tbody tr .t-act", 44],
    ];
    for (const [name, selector, min] of groups) {
      const boxes = await hitBoxes(page, selector);
      expect(boxes.length, `${name}（${selector}）未匹配到控件`).toBeGreaterThan(0);
      for (const b of boxes) {
        expect(b.expanded, `${name} 缺少 ::after 命中区扩展`).toBe(true);
        expect(Math.round(b.w), `${name} 命中区宽 ${b.w}px`).toBeGreaterThanOrEqual(min);
        expect(Math.round(b.h), `${name} 命中区高 ${b.h}px`).toBeGreaterThanOrEqual(min);
      }
      // 扩展后的命中区不得相互遮挡（二维无交叠）
      for (let i = 0; i < boxes.length; i++) {
        for (let j = i + 1; j < boxes.length; j++) {
          const a = boxes[i];
          const c = boxes[j];
          const overlap =
            a.left < c.right - 0.5 &&
            c.left < a.right - 0.5 &&
            a.top < c.bottom - 0.5 &&
            c.top < a.bottom - 0.5;
          expect(overlap, `${name} 命中区相互遮挡（${i}×${j}）`).toBe(false);
        }
      }
    }
  });
});
