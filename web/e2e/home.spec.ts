/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test } from "@playwright/test";
import { EMPTY_USAGE, envelope, seedAuth } from "./helpers";

/** 门户主页（S15）端到端：Bento 工作台渲染、数据口径、快捷入口与公告横幅联动 */

/**
 * 概览桩件。
 *
 * `errors`/`pending` 必须给：卡片副文案是「成功 N · 失败 M（x%）· K 进行中」，
 * 由 `requests - errors - pending` 反推成功数。漏字段时 `(undefined/12)*100` 是 NaN，
 * 页面会显示「失败 NaN（NaN%）」—— 而 TypeScript 拦不住（桩件是 unknown）。
 *
 * 三个数刻意满足 12 = 9 + 2 + 1，让「相加恒等于总数」这条口径在用例里成立。
 */
const OVERVIEW = {
  today: {
    requests: 12,
    errors: 2,
    pending: 1,
    charge: 1_000_000,
    prompt_tokens: 1200,
    completion_tokens: 800,
  },
  month: {
    requests: 30,
    errors: 3,
    pending: 0,
    charge: 5_000_000,
    prompt_tokens: 3000,
    completion_tokens: 2000,
  },
  month_recharge: 7_200_000,
  trend: [
    { date: "2026-10-18", charge: 500_000, recharge: 0 },
    { date: "2026-10-19", charge: 600_000, recharge: 0 },
    { date: "2026-10-20", charge: 1_000_000, recharge: 7_200_000 },
  ],
  active_tasks: 2,
  // 副文案里的「截至 14:32」取这个字段（业务时区的 HH:MM），给固定值避免断言随当前时间漂移
  generated_at: "2026-11-02T14:32:00+08:00",
};

/**
 * 概览桩件 + 计费口径桩件。
 *
 * 必须一并桩 `/api/pricing`：页面启动时用它注入币种与 qpc（main.tsx → setMoneyCtx），
 * 不桩就回落到默认人民币上下文，本文件按美元写的断言（$17 / ¥122.40 / $2.4691）
 * 会变成「随本机后端配置漂移」 —— 那不是测试失败，是测试在测环境。
 * 需要别的口径的用例在后注册同路由即可覆盖（Playwright 后注册优先）。
 */
function stubOverview(page: Parameters<typeof seedAuth>[0], data: unknown, onCall?: (url: string) => void) {
  void page.route("**/api/pricing**", (route) =>
    route.fulfill({
      json: envelope({
        models: [],
        vendors: [],
        group_ratios: { default: 1 },
        scale: 1_000_000,
        currency: "USD",
        currency_symbol: "$",
        cny_per_usd: 7.2,
        other_currency: "CNY",
        other_quota_per_unit: 69444,
      }),
    }),
  );
  return page.route("**/api/portal/overview**", (route) => {
    onCall?.(route.request().url());
    return route.fulfill({ json: envelope(data) });
  });
}

test.describe("门户主页 Bento 工作台", () => {
  test("T5 渲染工作台全部区块，不残留占位页", async ({ page }) => {
    await seedAuth(page);
    await page.goto("/home");
    await expect(page.locator(".balance-card .bl")).toContainText("账户余额");
    await expect(page.locator(".balance-card .bv")).toBeVisible();
    await expect(page.getByText("今日请求", { exact: true })).toBeVisible();
    await expect(page.getByText("今日费用", { exact: true })).toBeVisible();
    await expect(page.getByText("本月费用", { exact: true })).toBeVisible();
    await expect(page.getByText("API KEY", { exact: true })).toBeVisible();
    await expect(page.getByRole("heading", { name: /用量趋势/ })).toBeVisible();
    // 类型分布从「调用记录」页迁来，与趋势相邻构成分析区（不再出现在 /usage）。
    await expect(page.getByRole("heading", { name: /类型分布/ })).toBeVisible();
    await expect(page.getByRole("heading", { name: /快捷操作/ })).toBeVisible();
    await expect(page.getByRole("heading", { name: /最近调用/ })).toBeVisible();
    await expect(page.getByText(/待 S\d+ 实现/)).toHaveCount(0);
  });

  test("T5b 类型分布：条宽按最大项归一，展示次数与金额", async ({ page }) => {
    await seedAuth(page);
    await stubOverview(page, OVERVIEW);
    // 后注册优先：覆盖 seedAuth 的 EMPTY_USAGE（它只有空数组）
    await page.route("**/api/portal/usage**", (route) =>
      route.fulfill({
        json: envelope({
          ...EMPTY_USAGE,
          modality_dist: [
            { modality: "chat", requests: 8, charge: 1_500_000 },
            { modality: "image", requests: 2, charge: 500_000 },
          ],
        }),
      }),
    );
    await page.goto("/home");

    const card = page.getByTestId("modality-dist");
    await expect(card).toContainText("按消耗排序");
    await expect(card).toContainText("对话");
    await expect(card).toContainText("8 次");
    await expect(card).toContainText("2 次");
    // 分母是「最大项」而不是总额：第一项恒为 100%，第二项 500000/1500000 ≈ 33%
    await expect(card.locator(".progress i").nth(0)).toHaveAttribute("style", /width: 100%/);
    await expect(card.locator(".progress i").nth(1)).toHaveAttribute("style", /width: 33%/);
    // 空态与失败态文案都不能出现（有数据时）
    await expect(card).not.toContainText("这段时间没有调用记录");

    // 位置契约：整行（与趋势卡同宽）且排在趋势卡下方 —— 保证「趋势 → 分布」的阅读顺序，
    // 也防止有人把它改成 span-2 后在 4 列网格里留出空洞。
    const trendBox = await page.getByTestId("trend-card").boundingBox();
    const distBox = await card.boundingBox();
    expect(trendBox && distBox, "趋势卡或分布卡未渲染").toBeTruthy();
    expect(Math.abs((distBox as { width: number }).width - (trendBox as { width: number }).width)).toBeLessThanOrEqual(1);
    expect((distBox as { y: number }).y).toBeGreaterThanOrEqual(
      (trendBox as { y: number; height: number }).y + (trendBox as { y: number; height: number }).height,
    );
  });

  test("T6 余额/冻结/今日/本月按接口口径渲染", async ({ page }) => {
    await seedAuth(page, { username: "alice", quota: 17_000_000 });
    await page.route("**/api/portal/me", (route) =>
      route.fulfill({
        json: envelope({
          id: 1,
          username: "alice",
          role: 1,
          status: 1,
          group: "default",
          balance: 17_000_000,
          frozen: 2_469_100,
          used: 0,
          request_count: 42,
          key_count: 3,
          key_count_enabled: 1,
          email: "",
          phone: "",
        }),
      }),
    );
    await stubOverview(page, OVERVIEW);
    await page.goto("/home");

    // 余额数值锁定 .balance-card：头部「钱包余额」胶囊同样展示 $17，全局 getByText 会 strict 冲突
    const balance = page.locator(".balance-card");
    await expect(balance.getByText("$17")).toBeVisible(); // 8500000 / 500000
    await expect(balance.getByText("¥122.40")).toBeVisible(); // 17 USD × 7.2
    await expect(balance.getByText("$2.4691")).toBeVisible(); // 冻结 1234567 / 500000
    await expect(page.locator(".stat-card", { hasText: "今日费用" })).toContainText("$1");
    await expect(page.locator(".stat-card", { hasText: "本月费用" })).toContainText("$5");
    await expect(page.locator(".stat-card", { hasText: "今日请求" })).toContainText("12");
    await expect(page.locator(".balance-card")).toContainText("42"); // 累计结算

    // 副文案口径：三个分类数相加 = 总数，失败率按 errors/requests，余额卡改说「消费/结算」
    const todayCard = page.locator(".stat-card", { hasText: "今日请求" });
    await expect(todayCard).toContainText("成功 9 · 失败 2（16.7%） · 1 进行中");
    const chargeCard = page.locator(".stat-card", { hasText: "今日费用" });
    await expect(chargeCard).toContainText("已结算 $1");
    await expect(chargeCard).toContainText("2.0K tokens");
    await expect(chargeCard).toContainText("1 笔结算中");
    await expect(chargeCard).toContainText("截至 14:32");
    await expect(page.locator(".stat-card", { hasText: "本月费用" })).toContainText(
      "自然月累计 · 充值 $7.2",
    );
    await expect(page.locator(".stat-card", { hasText: "API KEY" })).toContainText("1 个启用");
    await expect(page.locator(".stat-card", { hasText: "API KEY" })).toContainText("共 3 个");
  });

  test("T7 趋势区间切换发出 days=30 请求", async ({ page }) => {
    await seedAuth(page);
    const urls: string[] = [];
    await stubOverview(page, OVERVIEW, (url) => urls.push(url));
    await page.goto("/home");
    await expect(page.locator(".pill-tabs .tab.active")).toHaveText("7 天");

    // 区间切换是 L2 视图开关（不进 URL），但必须是有语义的 role=tab
    await page.getByRole("tab", { name: "30 天" }).click();
    await expect.poll(() => urls.some((u) => u.includes("days=30"))).toBe(true);
    await expect(page.locator(".pill-tabs .tab.active")).toHaveText("30 天");
  });

  test("T8 余额低于阈值时显示不足横幅并可去充值", async ({ page }) => {
    await seedAuth(page, { quota: 1_000_000 });
    await stubOverview(page, OVERVIEW);
    await page.goto("/home");
    await expect(page.getByText(/账户余额不足/)).toBeVisible();
    await expect(page.getByRole("link", { name: "去充值" })).toBeVisible();
  });

  test("T9 余额充足时不显示不足横幅", async ({ page }) => {
    await seedAuth(page, { quota: 9_000_000 });
    await stubOverview(page, OVERVIEW);
    await page.goto("/home");
    await expect(page.locator(".balance-card .bl")).toContainText("账户余额");
    await expect(page.getByText(/账户余额不足/)).toHaveCount(0);
  });

  test("T10 快捷入口直达 API KEY 创建弹窗", async ({ page }) => {
    await seedAuth(page);
    await page.route("**/api/portal/keys**", (route) =>
      route.fulfill({ json: envelope({ items: [], total: 0 }) }),
    );
    await page.route("**/api/pricing**", (route) =>
      route.fulfill({
        json: envelope({
          models: [],
          vendors: [],
          group_ratios: { default: 1 },
          // 币种口径显式声明（断言按美元），否则回落人民币上下文、随环境漂移
          scale: 1_000_000,
          currency: "USD",
          currency_symbol: "$",
          cny_per_usd: 7.2,
          other_currency: "CNY",
          other_quota_per_unit: 69444,
        }),
      }),
    );
    await page.goto("/home");
    await page.getByRole("link", { name: /创建 API KEY/ }).first().click();
    await expect(page).toHaveURL(/\/keys\?create=1/);
    await expect(page.getByRole("dialog", { name: "创建密钥" })).toBeVisible();
  });

  test("T11 最近调用行点击跳转调用记录", async ({ page }) => {
    await seedAuth(page);
    await stubOverview(page, OVERVIEW);
    // 调用记录页的「密钥」筛选会拉取密钥列表，未打桩将 401 触发全局登出
    await page.route("**/api/portal/keys**", (route) =>
      route.fulfill({ json: envelope({ items: [], total: 0 }) }),
    );
    await page.route("**/api/portal/usage**", (route) =>
      route.fulfill({
        json: envelope({
          ...EMPTY_USAGE,
          items: [
            {
              id: 1,
              model: "gpt-4o",
              modality: "chat",
              prompt_tokens: 10,
              completion_tokens: 5,
              charge: 1000,
              status: "settled",
              created_at: "2026-10-20T10:00:00+08:00",
            },
          ],
          total: 1,
        }),
      }),
    );
    await page.route("**/api/portal/tasks**", (route) =>
      route.fulfill({ json: envelope({ items: [], total: 0 }) }),
    );
    // 「各类型默认模型」卡会请求 /api/pricing；不打桩时会透传到真实后端并渲染出 GPT-4o，
    // 与最近调用行的 gpt-4o 构成 strict-mode 双命中。打桩为空目录使该卡走空态，用例与后端可达性无关。
    await page.route("**/api/pricing**", (route) =>
      route.fulfill({
        json: envelope({
          models: [],
          vendors: [],
          group_ratios: { default: 1 },
          // 币种口径显式声明（断言按美元），否则回落人民币上下文、随环境漂移
          scale: 1_000_000,
          currency: "USD",
          currency_symbol: "$",
          cny_per_usd: 7.2,
          other_currency: "CNY",
          other_quota_per_unit: 69444,
        }),
      }),
    );
    await page.goto("/home");
    await expect(page.getByText("gpt-4o")).toBeVisible();
    await page.getByText("gpt-4o").click();
    await expect(page).toHaveURL(/\/usage$/);
  });

  test("T12 公告横幅：仅在未读时出现，「查看」打开铃铛弹层且不跳页", async ({ page }) => {
    await seedAuth(page);
    // 正文里带一段 HTML：公告由管理员填写，横幅必须走转义渲染（原样注入就是存储型 XSS）
    const items = [
      {
        id: 9,
        content: "系统将于今晚维护 <img src=x onerror=alert(1)>",
        publishDate: "2026-10-20T09:00:00+08:00",
        type: "notice",
        extra: "",
        read: false,
      },
    ];
    await page.route(
      (url) => url.pathname.startsWith("/api/portal/announcements"),
      (route) => {
        if (route.request().method() === "POST") {
          return route.fulfill({
            json: envelope({ enabled: true, items: items.map((i) => ({ ...i, read: true })) }),
          });
        }
        return route.fulfill({ json: envelope({ enabled: true, items }) });
      },
    );

    await page.goto("/home");
    const banner = page.locator(".alert-banner", { hasText: "最新公告" });
    await expect(banner).toBeVisible();
    await expect(banner.locator("img")).toHaveCount(0);

    await banner.getByRole("button", { name: "查看" }).click();
    const panel = page.getByRole("dialog", { name: "站点公告" });
    await expect(panel).toContainText("系统将于今晚维护");
    await expect(page).toHaveURL(/\/home$/);
    await expect(panel.locator("img")).toHaveCount(0);

    // 整组已读后横幅立即消失（横幅与铃铛共用同一份 query cache，无需额外刷新）
    await page.keyboard.press("Escape");
    await expect(page.locator(".alert-banner", { hasText: "最新公告" })).toHaveCount(0);
  });

  test("T13 各类型默认模型卡按 /portal/defaults 渲染默认项（非空态）", async ({ page }) => {
    await seedAuth(page);
    await stubOverview(page, OVERVIEW);
    // 默认模型的唯一真相源是 /portal/defaults。**不能**再用 pricing.models.is_default：
    // 那个字段全站没有任何写入口，从 /api/pricing 里筛它永远筛出空集（旧用例正是这么写的，
    // 于是它测的是「计费目录里恰好有 is_default 标记」这件不存在的事）。
    await page.route("**/api/portal/defaults**", (route) =>
      route.fulfill({
        json: envelope({
          chat: { model: "gpt-4o", any_key_allowed: true },
          image: { model: "dall-e-3", any_key_allowed: true },
          // 音频未配置（空串）：卡片只列真的配了模型的模态，不能因为它是 8 个固定键就列出来
          audio: { model: "", any_key_allowed: false },
          video: { model: "", any_key_allowed: false },
          music: { model: "", any_key_allowed: false },
          embedding: { model: "", any_key_allowed: false },
          rerank: { model: "", any_key_allowed: false },
          moderation: { model: "", any_key_allowed: false },
        }),
      }),
    );
    await page.route("**/api/pricing**", (route) =>
      route.fulfill({
        json: envelope({
          // pricing 在这张卡里**只**提供图标与品牌色，不参与「谁是默认模型」的判定
          models: [
            { id: "gpt-4o", display_name: "GPT-4o", modality: "chat", is_default: true },
            { id: "dall-e-3", display_name: "DALL·E 3", modality: "image", is_default: true },
            { id: "whisper-1", display_name: "Whisper 1", modality: "audio", is_default: false },
          ],
          vendors: [],
          group_ratios: { default: 1 },
          // 币种口径显式声明（断言按美元），否则回落人民币上下文、随环境漂移
          scale: 1_000_000,
          currency: "USD",
          currency_symbol: "$",
          cny_per_usd: 7.2,
          other_currency: "CNY",
          other_quota_per_unit: 69444,
        }),
      }),
    );
    await page.goto("/home");

    const card = page.getByTestId("default-models");
    await expect(card.getByRole("heading", { name: /默认模型/ })).toBeVisible();
    // 渲染真实默认项（原型展示模型 ID），而非空态
    await expect(card).toContainText("gpt-4o");
    await expect(card).toContainText("dall-e-3");
    // 未配置的模态（audio）与「pricing 里有但没配成默认」的模型都不进入该卡；空态文案不出现
    await expect(card).not.toContainText("whisper-1");
    await expect(card).not.toContainText("暂未配置默认模型");
  });

  test("T14 概览接口失败时四张卡显示 —、可重试，且不出现 $0", async ({ page }) => {
    await seedAuth(page);
    await page.route("**/api/portal/overview**", (route) =>
      route.fulfill({ status: 500, json: envelope(null, 500, "内部错误") }),
    );
    await page.goto("/home");

    // 四张指标卡合并进一张 2 栏大卡（.kpi-card），但仍是四个独立的 StatCard：
    // 合并只改容器，三态与重试必须逐个保留。
    const cards = page.locator(".kpi-card .stat-card");
    await expect(cards).toHaveCount(4);
    // 三张 KPI 卡绑 /portal/overview（挂），API KEY 卡绑 /portal/me（正常）——
    // 「同一屏的卡可以有不同的数据来源」正是它们必须有各自三态、而不是共用一处判定的原因。
    const broken = page.locator(".kpi-card .stat-card[role=alert]");
    await expect(broken).toHaveCount(3);
    // 值必须是「—」而不是 0：0 是在断言「今天确实没调用」，接口挂了没有这个资格。
    // 这条用例的失败模式正是旧实现的缺陷 —— 三张卡齐刷刷显示 0，与真空数据无法区分。
    await expect(broken.locator(".value")).toHaveText(["—", "—", "—"]);
    await expect(broken.filter({ hasText: "加载失败" })).toHaveCount(3);
    await expect(page.locator(".kpi-card .stat-card", { hasText: "$0" })).toHaveCount(0);
    // 绑 /portal/me 的卡不受影响：来源不同就不能被牵连（反之亦然）
    await expect(page.locator(".stat-card", { hasText: "API KEY" })).toHaveText(/0 个启用/);
    // 余额卡里的「在途异步任务」同源 overview，同样不能显示 0
    await expect(page.locator(".balance-card .bs")).toContainText("在途异步任务 —");
    // 余额本身来自 /portal/me（没挂），不能被 overview 的失败拖成 —
    await expect(page.locator(".balance-card .bv")).toHaveText("$5.00");

    // 重试必须真的重新取数：换成成功桩件后点一次，卡片恢复真实数值
    await page.unroute("**/api/portal/overview**");
    await stubOverview(page, OVERVIEW);
    await cards.first().getByRole("button", { name: "重试" }).click();
    await expect(cards.first().locator(".value")).toHaveText("12");
  });

  test("T15 真空数据渲染 0 与空态文案，而不是 —", async ({ page }) => {
    // seedAuth 的兜底桩件就是 EMPTY_OVERVIEW / EMPTY_DEFAULTS，即「新注册用户从未调用」
    await seedAuth(page);
    await page.goto("/home");

    const todayCard = page.locator(".stat-card", { hasText: "今日请求" });
    await expect(todayCard.locator(".value")).toHaveText("0");
    await expect(todayCard).toContainText("今日暂无调用");
    await expect(page.locator(".stat-card", { hasText: "今日费用" })).toContainText("今日暂无消费");
    await expect(page.locator(".stat-card", { hasText: "本月费用" })).toContainText("本月暂无充值");
    // 未创建密钥：区分「0 个启用」与「共 0 个」没有意义，改为给创建入口
    const keyCard = page.locator(".stat-card", { hasText: "API KEY" });
    await expect(keyCard.locator(".value")).toHaveText("0 个启用");
    await expect(keyCard).toContainText("尚未创建");
    // 全零趋势走空态，而不是画一副空坐标轴（空轴看起来像渲染失败）
    await expect(page.getByText("这段时间没有消耗或充值")).toBeVisible();
    await expect(page.getByTestId("default-models")).toContainText("暂未配置默认模型");
  });

  test("T16 趋势图双系列：图例与 Y 轴刻度都带币种", async ({ page }) => {
    await seedAuth(page);
    await stubOverview(page, OVERVIEW);
    await page.goto("/home");

    // 两条线必须各有图例，否则「消耗/充值」在图上无法区分
    const legend = page.locator(".chart .legend");
    await expect(legend).toContainText("消耗（$）");
    await expect(legend).toContainText("充值（$）");
    // Y 轴刻度带币种：纯「1.2万」看不出是元还是美元
    const ticks = page.locator(".recharts-yAxis .recharts-cartesian-axis-tick-value");
    await expect(ticks.first()).toBeVisible();
    await expect(ticks.first()).toContainText("$");
  });

  test("T17 未授权模态显示 ⚠ 未授权，已授权的不显示", async ({ page }) => {
    await seedAuth(page);
    await stubOverview(page, OVERVIEW);
    await page.route("**/api/portal/defaults**", (route) =>
      route.fulfill({
        json: envelope({
          chat: { model: "gpt-4o", any_key_allowed: true },
          image: { model: "dall-e-3", any_key_allowed: false },
        }),
      }),
    );
    // 「管理 →」/「⚠ 未授权」都指向密钥页，该页会拉密钥列表；漏桩会 401 触发全局登出
    await page.route("**/api/portal/keys**", (route) =>
      route.fulfill({ json: envelope({ items: [], total: 0 }) }),
    );
    await page.goto("/home");

    const card = page.getByTestId("default-models");
    // 已授权行不显示徽标 —— 每行都挂 ⚠ 等于没有信息
    await expect(card.locator(".flex-between", { hasText: "gpt-4o" })).not.toContainText("未授权");
    const deny = card.getByRole("link", { name: /未授权/ });
    await expect(deny).toHaveCount(1);
    await expect(card).toContainText("⚠ 未授权");
    await deny.click();
    await expect(page).toHaveURL(/\/keys/);
  });

  test("T18 最近调用接口失败时显示失败与重试，而不是「今日暂无调用」", async ({ page }) => {
    await seedAuth(page);
    await stubOverview(page, OVERVIEW);
    await page.route("**/api/portal/usage**", (route) =>
      route.fulfill({ status: 500, json: envelope(null, 500, "内部错误") }),
    );
    await page.goto("/home");

    await expect(page.getByText("最近调用加载失败")).toBeVisible();
    await expect(page.getByRole("button", { name: "重试" }).last()).toBeVisible();
    // 类型分布与「最近调用」同源，失败时必须一起报错，且不能谎报成「没有调用」
    await expect(page.getByText("类型分布加载失败")).toBeVisible();
    await expect(page.getByText("这段时间没有调用记录")).toHaveCount(0);
    // 失败绝不能被说成「今天没有调用」
    await expect(page.getByText("今日暂无调用")).toHaveCount(0);
  });

  test("T19 API KEY 卡区分启用数与总数", async ({ page }) => {
    await seedAuth(page);
    await stubOverview(page, OVERVIEW);
    await page.route("**/api/portal/me", (route) =>
      route.fulfill({
        json: envelope({
          id: 1,
          username: "e2e",
          role: 1,
          status: 1,
          group: "default",
          balance: 5_000_000,
          frozen: 0,
          used: 0,
          request_count: 0,
          key_count: 3,
          key_count_enabled: 1,
          group_discount: 1_000_000,
          group_configured: false,
          email: "",
          phone: "",
        }),
      }),
    );
    await page.goto("/home");

    const card = page.locator(".stat-card", { hasText: "API KEY" });
    // 主数值是「启用数」而不是「总数」：停用的 KEY 调不通，把它算进能力概览是虚报
    await expect(card.locator(".value")).toHaveText("1 个启用");
    await expect(card).toContainText("共 3 个");
    await expect(card.getByRole("link", { name: /管理/ })).toBeVisible();
  });
});
