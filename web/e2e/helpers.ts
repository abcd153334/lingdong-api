import { expect, type Page } from "@playwright/test";

/** zustand-persist（stores/auth.ts）在 localStorage 的键 */
export const LD_AUTH_KEY = "ld-auth";

/** 管理台默认落地页（仪表盘）会请求 /api/admin/data/board 的空看板桩件 */
export const EMPTY_BOARD = {
  today: {
    requests: 0,
    // 金额字段是 `recharge` / `charge` / `cost`（后端 `stats.TodayView`，单位微）。
    // 旧名 `recharge_cny` / `charge_quota` / `cost_quota` 已被删除，留在桩件里
    // 等于给后端伪造一份不存在的契约：看到的人会以为还有这个字段。
    recharge: 0,
    charge: 0,
    cost: 0,
    cost_collected: false,
    success_rate: 1,
    channels_online: 0,
    channels_total: 0,
    price_missing_count: 0,
  },
  trend: [],
  fail_top: [],
  channels: [],
  boards: {
    d7: { user_top: [], model_top: [], users: [] },
    d30: { user_top: [], model_top: [], users: [] },
  },
};

/**
 * 门户公开配置桩件（S10/S18）：`/api/portal/config`。
 * group_ratios 仅 default 一项，够 chrome 与主页降级链使用。
 */
export const PORTAL_CONFIG = {
  site: {
    site_name: "灵动 API",
    logo_image: "",
    favicon_image: "",
    site_url: "",
    api_base_urls: [],
    contact_email: "",
    footer_text: "",
    icp_beian: "",
  },
  // 对外地址的展示口径：桩件默认「都没配置 → 用当前请求 origin」。
  // 组件读的是 api.bases，不再是裸的 api_base_url。
  api: { bases: ["https://api.example.com"], source: "request" },
  // 运营模式：桩件默认「门户开启」。关闭态的用例必须自己覆盖这一项 ——
  // 缺字段在代码里等价于开启，那是有意为之的兜底，不是让用例偷懒的地方。
  portal_enabled: true,
  group_ratios: { default: 1 },
  about_content: "",
  agreement_content: "",
  // 认证策略桩：默认「开放注册 + 不强制联系方式验证 + 自建验证码」。
  // 验证码场景按 always 配：桩件里没有真实后端，答案无从校验，
  // 用例只需把输入框填上任意值（服务端由 /api/portal/auth/* 桩件直接放行）。
  auth: {
    register_mode: "open",
    contact_verify: "off",
    contact_channels: ["email"],
    // 生效集合（策略 ∩ 就绪）。默认给单邮箱 = 注册/找回默认走邮箱，
    // 与既有用例的 #rg-target / #rst-target 前提一致；
    // 要测「手机优先」的用例必须显式给 ["sms","email"]（顺序由**后端**决定，
    // 前端 resolveChannel 不排序 —— 桩件给未排序的数组测不出优先级）。
    available_contact_channels: ["email"],
    captcha_register: "always",
    captcha_login: "after_fails",
    captcha_reset: "always",
    login_fail_threshold: 5,
  },
  custom_topup_enabled: true,
  min_topup: 1,
  // 两态桩：默认为「通道开启」，需验证禁用态的用例自行覆盖（见 settings.spec 的 S18 用例）。
  // 若默认关闭，则「换绑/绑定入口可点击」的用例前提与桩件矛盾而必然失败。
  email_enabled: true,
  sms_enabled: true,
};

/**
 * 空数据版门户聚合桩件：`/api/portal/overview`。
 *
 * 字段名必须与 `types.UserUsageStat` 逐字一致（`charge` 而不是旧的 `charge_quota`）。
 * 曾经这里写着 `charge_quota`，于是默认桩件下首页金额恒为 0，而**没有任何用例断言过它**，
 * 这个不一致藏了很久 —— 新增用例只要断言金额就会立刻暴露。
 *
 * `generated_at` 给固定值：文案里的「截至 14:32」需要一个确定的来源。
 */
export const EMPTY_OVERVIEW = {
  today: { requests: 0, errors: 0, pending: 0, charge: 0, prompt_tokens: 0, completion_tokens: 0 },
  month: { requests: 0, errors: 0, pending: 0, charge: 0, prompt_tokens: 0, completion_tokens: 0 },
  month_recharge: 0,
  trend: [],
  active_tasks: 0,
  generated_at: "2026-11-02T14:32:00+08:00",
};

/**
 * 默认模型桩件：`/api/portal/defaults`（8 类模态固定键，model 为空串 = 未配置）。
 *
 * 必须显式打桩：Home 会调用它，漏桩时请求经 vite 代理打到真后端得到 401，
 * 全局 401 处理会**登出**，于是所有用例一起变红，而报错信息只是「找不到 h1」。
 */
export const EMPTY_DEFAULTS: Record<string, { model: string; any_key_allowed: boolean }> = {
  chat: { model: "", any_key_allowed: false },
  image: { model: "", any_key_allowed: false },
  video: { model: "", any_key_allowed: false },
  audio: { model: "", any_key_allowed: false },
  music: { model: "", any_key_allowed: false },
  embedding: { model: "", any_key_allowed: false },
  rerank: { model: "", any_key_allowed: false },
  moderation: { model: "", any_key_allowed: false },
};

/** 门户调用记录桩件（S5）：`/api/portal/usage`（modality_dist 给空数组，保持结构完整） */
export const EMPTY_USAGE = {
  items: [],
  total: 0,
  page: 1,
  page_size: 20,
  // 首页「类型分布」卡消费这个字段。缺字段与「窗口内真的没有调用」在渲染上
  // 无法区分，桩件必须与后端响应同构，否则空态断言会变成对「字段缺失」的断言。
  modality_dist: [],
};

/**
 * 登录设备桩件（`GET /api/portal/auth/sessions`）。
 *
 * 刻意给两台（一台 current）：只有一台时「退出全部设备」按钮按设计不渲染，
 * 想验证那条路径的用例必须先有多台的前提。
 */
export const E2E_SESSIONS = [
  {
    id: 1,
    current: true,
    ip: "127.0.0.1",
    ua: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/122.0 Safari/537.36",
    created_at: "2026-01-01T08:00:00Z",
    last_seen_at: "2026-01-01T10:00:00Z",
    expires_at: "2026-01-08T08:00:00Z",
  },
  {
    id: 2,
    current: false,
    ip: "10.0.0.9",
    ua: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Safari/605.1.15",
    created_at: "2025-12-31T08:00:00Z",
    last_seen_at: "2025-12-31T09:00:00Z",
    expires_at: "2026-01-07T08:00:00Z",
  },
];

/**
 * 「管理台页面会打的接口」判定，供各 spec 自建的桩件当路由谓词用。
 *
 * ⚠️ **必须排除 `/api/portal/me`**：凭证搬进 httpOnly Cookie 之后，
 * 「有没有登录」的唯一来源就是它。桩件把它一起吞成 `envelope(null)`，
 * `loadSession()` 就拿到 null → 守卫判定未登录 → 整页跳登录页，
 * 表现是「所有管理台用例突然全红，报找不到 h1」——而接口桩件本身看着没问题。
 */
export function isAdminPageRequest(url: URL): boolean {
  if (url.pathname === "/api/portal/me") return false;
  return url.pathname.startsWith("/api/admin/") || url.pathname.startsWith("/api/portal/");
}

export interface SeedAuthOptions {
  /** 0 = 管理员，1 = 普通用户（默认 1） */
  role?: 0 | 1;
  /** 已废弃：凭证不再由前端持有，保留字段只为不改动几十处调用点 */
  token?: string;
  username?: string;
  quota?: number;
}

/**
 * 打桩「已登录」：受守卫的 /home、/admin/* 可直接访问。
 *
 * 实现方式从「往 localStorage 播种 token」换成「打桩 /api/portal/me」——
 * 凭证现在在 httpOnly Cookie 里，前端**没有任何**可写的登录态；
 * 守卫唯一能问的是服务端。
 */
export async function seedAuth(page: Page, options: SeedAuthOptions = {}): Promise<void> {
  const { role = 1, username = "e2e", quota } = options;
  // 不再播种 localStorage：凭证已经是 httpOnly Cookie，前端读不到也写不了。
  // 登录态的唯一来源是下面的 `/api/portal/me` 桩件（守卫的 loadSession 会打它）。

  // chrome 与默认落地页会请求下列接口；后端可达且未打桩时返回 401 触发全局登出，
  // 故统一打桩，使用例与后端可达性无关（需要自定义数据的用例在其后再次注册覆盖，后者优先）。
  //
  // ⚠️ 下面两个**兜底**必须先注册（Playwright 后注册优先，先注册 = 最低优先级）：
  // 放最后会把用例自己桩的 `/api/admin/data/board`、`/api/pricing` 全部抢掉，
  // 表现是「原本绿的 layout/admin-orders 突然红」。
  //
  // ① 管理台兜底：不桩时请求打到 vite 代理 → 真后端 → 401 → 全局登出
  //    （页面变登录页，报错却是「元素找不到」）。返回 envelope(null) 而非 401：
  //    页面走空态/降级，需要真实结构的用例自行再注册即可覆盖。
  await page.route("**/api/admin/**", (route) => route.fulfill({ json: envelope(null) }));
  // ② 计费口径兜底：页面启动由 `/api/pricing` 注入币种与 qpc（main.tsx → setMoneyCtx）。
  //    不桩会回落默认人民币上下文，而**全部用例的金额断言都按美元写**，
  //    于是同一批用例随「本机后端当前币种」翻红翻绿 —— 那不是测试失败，是测试在测环境。
  await page.route("**/api/pricing**", (route) =>
    route.fulfill({
      json: envelope({
        models: [],
        vendors: [],
        defaults: {},
        group_ratios: { default: 1 },
        quota_per_unit: 500_000,
        currency: "USD",
        currency_symbol: "$",
        cny_per_usd: 7.2,
        other_currency: "CNY",
        other_quota_per_unit: 69444,
      }),
    }),
  );
  await page.route("**/api/portal/me", (route) =>
    route.fulfill({
      json: envelope({
        id: 1,
        username,
        role,
        status: 1,
        group: "default",
        balance: quota ?? 5_000_000,
        frozen: 0,
        used: 0,
        request_count: 0,
        key_count: 0,
        key_count_enabled: 0,
        // 折扣相关字段必须给：Home 读 group_discount 渲染「分组 · 折扣」徽标，
        // 缺了会走 ?? getMoneyCtx().scale 的兜底，徽标恒显示「无折扣」而不报错。
        group_discount: 1_000_000,
        group_configured: false,
        email: "",
        phone: "",
      }),
    }),
  );
  // 认证写端点兜底：登出/改密/发码/绑定的成功体各不相同，这里只兜「不 401」，
  // 需要精确交互的用例自行再注册（后注册优先）。
  await page.route("**/api/portal/auth/**", (route) => {
    const u = new URL(route.request().url());
    // 图形验证码：一次返回 {id, image(dataURL)}。桩件给 1×1 PNG，
    // 样式上被拉成 40px 高，用例只看"有没有图、能不能填"。
    if (u.pathname.endsWith("/auth/captcha")) {
      return route.fulfill({ json: envelope(E2E_CAPTCHA) });
    }
    // 设备列表必须给出 `sessions` 字段：兜底体的 expired_in 会被
    // `data.sessions.map` 读到 undefined，用户中心整块渲染失败（HTTP 200，无报错日志）。
    // 只拦 GET 的列表本身，`/sessions/:id/revoke` 仍走下面的通用体。
    if (u.pathname.endsWith("/auth/sessions") && route.request().method() === "GET") {
      return route.fulfill({ json: envelope({ sessions: E2E_SESSIONS }) });
    }
    // 二次验证状态：默认「未开启」。给显式桩件而不是让兜底体兜住 ——
    // 兜底体没有 `enabled`，卡片会渲染成 undefined 驱动的中间态（看着像未开启，
    // 但 `backup_codes_left` 也是 undefined，断言任何数字都会飘）。
    if (u.pathname.endsWith("/auth/mfa/status")) {
      return route.fulfill({
        json: envelope({ enabled: false, policy: "optional", backup_codes_left: 0 }),
      });
    }
    return route.fulfill({ json: envelope({ expires_in: 60, reset_ticket: "e2e-ticket" }) });
  });
  await page.route("**/api/portal/config**", (route) => route.fulfill({ json: envelope(PORTAL_CONFIG) }));
  await page.route("**/api/portal/overview**", (route) => route.fulfill({ json: envelope(EMPTY_OVERVIEW) }));
  await page.route("**/api/portal/defaults**", (route) => route.fulfill({ json: envelope(EMPTY_DEFAULTS) }));
  // 列表与详情分开：`/usage/102` 同样满足 "**/api/portal/usage**" 的 glob，
  // 若一起返回分页体，抽屉会读到 undefined 并**一直显示"加载中"**（不报错、测试却绿）。
  // 详情在这里显式 404：需要详情的用例必须自己打桩，漏打会立刻红。
  await page.route("**/api/portal/usage**", (route) => {
    const u = new URL(route.request().url());
    if (/^\/api\/portal\/usage\/\d+$/.test(u.pathname)) {
      return route.fulfill({ status: 200, json: { code: 404, message: "not_found", data: null } });
    }
    return route.fulfill({ json: envelope(EMPTY_USAGE) });
  });
  await page.route("**/api/portal/announcements**", (route) =>
    route.fulfill({ json: envelope({ enabled: false, items: [] }) }),
  );
  // 标记已读：必须晚于上面的 announcements 通配注册，才能在此 URL 上优先命中。
  // 响应与 GET 同形状（enabled/items），前端拿它整份替换缓存。
  await page.route("**/api/portal/announcements/read", (route) =>
    route.fulfill({ json: envelope({ enabled: false, items: [] }) }),
  );
  await page.route("**/api/admin/data/board", (route) =>
    route.fulfill({ json: envelope(EMPTY_BOARD) }),
  );
}

/**
 * 填人机验证答案。
 *
 * 验证码已改为服务端出图（`<img src>`），答案只存在 Redis，前端**读不到**；
 * 所以 e2e 不再「读图后填入」，而是直接填一个固定值 —— 真正的校验由后端
 * 用例覆盖（`LD_CAPTCHA_TEST_CODE` 桩），前端用例只验证「字段出现了、能提交」。
 */
/** 图形验证码的桩件（1×1 PNG，够让 <img> 有 src）。 */
export const E2E_CAPTCHA = {
  id: "e2e-captcha-id",
  image:
    "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
};

/** 三处验证码输入框的 id（与 CaptchaField 的传参一致）。 */
const CAPTCHA_INPUT = {
  register: "#rg-captcha",
  reset: "#rst-captcha",
  login: "#ld-captcha",
} as const;

/**
 * 填图形验证码。
 *
 * `optional: true` 用于 after_fails 的登录场景：首屏没有验证码，
 * 只有服务端回了 captcha_required 才会出现 —— 此时不填。
 */
export async function fillCaptcha(
  page: Page,
  code = "12345",
  scene: "login" | "register" | "reset" = "login",
  opts: { optional?: boolean } = {},
): Promise<void> {
  const sel = CAPTCHA_INPUT[scene];
  const field = page.locator(sel);
  if (opts.optional) {
    await field.waitFor({ state: "visible", timeout: 1500 }).catch(() => undefined);
    if ((await field.count()) === 0) return;
  } else {
    await field.waitFor({ state: "visible", timeout: 15000 });
  }
  await field.fill(code);
}

/** 统一响应体 {code,message,data}；errorCode 为可选业务错误码（C-06） */
export function envelope(
  data: unknown,
  code = 0,
  message = "",
  errorCode = "",
): {
  code: number;
  message: string;
  error_code: string;
  data: unknown;
} {
  return { code, message, error_code: errorCode, data };
}

/**
 * 断言页面**没有**横向滚动。
 *
 * 不要用 `scrollWidth - clientWidth === 0` 判断：页面滚动条是 overlay 方案
 * （`html` 上不占布局，视觉由 `PageScrollbar` 自绘），一旦有人把占位滚动条
 * （`scrollbar-gutter: stable` / `html::-webkit-scrollbar{width:0}`）加回来，
 * 这个差值会被平移 10px —— `=== 0` 直接红，改成 `<= 0` 又会把「刚好溢出 10px」放过。
 *
 * 直接问浏览器能不能横向滚：与滚动条方案、页面缩放、设备像素比都无关，
 * 而且正好等于「用户会不会看到横向滚动」这件事。
 */
export function canHScroll(page: Page): Promise<boolean> {
  return page.evaluate(() => {
    const de = document.documentElement;
    const prev = de.scrollLeft;
    de.scrollLeft = 1; // 无 scroll-behavior:smooth 时同步生效
    const moved = de.scrollLeft > 0;
    de.scrollLeft = prev;
    return moved;
  });
}

/** 一次性的「没有横向滚动」断言；需要等布局定型时用 `expect.poll(() => canHScroll(page))`。 */
export async function expectNoHScroll(page: Page, note = "页面出现横向滚动"): Promise<void> {
  expect(await canHScroll(page), note).toBe(false);
}
