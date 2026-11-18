/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

/** 门户密钥 fixture（用于密钥列名称解析） */
const KEYS = {
  items: [
    {
      id: 7,
      key_prefix: "sk-abcd",
      name: "生产密钥",
      group: "default",
      status: 1,
      amount_limit: 0,
      used: 0,
      models_allowed: null,
      modalities_allowed: null,
      allow_ips: null,
      rpm_limit: 60,
      concurrency_limit: 5,
      expired_at: null,
      allow_user_quota_query: true,
      created_at: "2026-10-01 10:00:00",
      reset_secret_at: null,
    },
  ],
};

/** 调用记录 fixture（覆盖已结算/已释放、对话/向量） */
const USAGES = [
  {
    id: 102,
    key_id: 7,
    created_at: "2026-10-19 12:00:00",
    model: "gpt-4o",
    modality: "chat",
    status: "settled",
    http_status: 200,
    prompt_tokens: 1200,
    completion_tokens: 800,
    charge: 1_500_000,
    latency_ms: 1234,
    is_stream: false,
    error_summary: "",
  },
  {
    id: 101,
    key_id: 7,
    created_at: "2026-10-19 11:50:00",
    model: "text-embedding-3-large",
    modality: "embedding",
    status: "released",
    http_status: 500,
    prompt_tokens: 0,
    completion_tokens: 0,
    charge: 0,
    latency_ms: 300,
    is_stream: false,
    error_summary: "服务暂时不可用，本次未扣费",
  },
];

/**
 * 调用记录详情 fixture（C1）：后端口户投影 `logs.PortalDetailView`。
 *
 * 关键口径：call_io 里**没有** req_raw / resp_raw / resp_error，media 里**没有** url ——
 * 门户拿到的是统计量 + 抽出的纯文本，产物字节走 `/api/portal/usage/:id/media/:index`。
 */
function usageDetail(id: number) {
  return {
    ...(USAGES.find((u) => u.id === id) ?? USAGES[0]),
    id,
    elapsed_ms: 1234,
    service_ms: 1100,
    first_token_ms: 260,
    attempts: 2,
    started_at: "2026-10-19 12:00:00",
    finished_at: "2026-10-19 12:00:01",
    call_kind: "sync",
    // 后端详情会回填展示名（门户也能看到自己的密钥名）
    key_name: "生产密钥",
    request_id: "trace_abc",
    usage_semantic: "openai",
    tier_unit_price_quota: 0,
    charge_before_cap: 1500000,
    frozen: 1_600_000,
    anomaly: "",
    call_io: {
      // 用户自己发的 + 平台交给用户的：门户详情的「请求内容 / 响应内容」直接渲染这两段。
      req_raw: '{"model":"gpt-4o","messages":[{"role":"user","content":"你好"}]}',
      req_bytes: 96,
      req_truncated: false,
      req_source: "raw",
      body_mode: "full",
      resp_body:
        '{"choices":[{"message":{"role":"assistant","content":"你好，有什么可以帮你？"}}]}',
      resp_bytes: 78,
      sse_frames: 0,
      output_text: "你好，有什么可以帮你？",
      media: [
        {
          kind: "image",
          mime: "image/png",
          bytes: 2048,
          artifact_index: -1,
        },
        {
          kind: "audio",
          mime: "audio/mpeg",
          bytes: 4096,
          artifact_index: -1,
          note: "二进制音频已完整发送，内容未留存",
        },
      ],
    },
  };
}

const ME = {
  id: 1,
  username: "e2e",
  role: 1,
  status: 1,
  group: "default",
  balance: 5_000_000,
  frozen: 0,
  used: 0,
  request_count: 2,
  key_count: 1,
  key_count_enabled: 1,
  email: "",
  phone: "",
};

/**
 * 汇总条桩件（S17）：今日/本月均取 charge（微，1 货币单位 = 1e6 微）。
 *
 * `errors`/`pending` 必须给：副文案由 `requests - errors - pending` 反推成功数，
 * 漏字段会渲染成「失败 NaN（NaN%）」。
 */
const OVERVIEW = {
  today: {
    requests: 2,
    errors: 1,
    pending: 0,
    charge: 1_500_000,
    prompt_tokens: 1200,
    completion_tokens: 800,
  },
  month: {
    requests: 2,
    errors: 1,
    pending: 0,
    charge: 1_500_000,
    prompt_tokens: 1200,
    completion_tokens: 800,
  },
  month_recharge: 2_000_000,
  trend: [],
  active_tasks: 0,
  generated_at: "2026-11-02T14:32:00+08:00",
};

/** 桩件时间戳（空格分隔）→ Date */
const at = (s: string) => new Date(s.replace(" ", "T"));

/**
 * 按查询参数过滤 usage 桩件（服务端筛选语义）。
 * 覆盖 S17：日期区间过滤、导出端点（按 pathname 分流）。
 */
async function stubUsage(page: Page, capture?: (url: URL) => void): Promise<void> {
  await page.route("**/api/portal/me", (route) => route.fulfill({ json: envelope(ME) }));
  await page.route("**/api/portal/keys", (route) => route.fulfill({ json: envelope(KEYS) }));
  await page.route("**/api/portal/overview**", (route) => route.fulfill({ json: envelope(OVERVIEW) }));
  // 计费口径属于页面启动时的全局注入（main.tsx → /api/pricing）。
  // 不桩件时这组断言会跟着"本机后端的当前配置"变化，等于测试在测环境。
  await page.route("**/api/pricing", (route) =>
    route.fulfill({
      json: envelope({
        models: [],
        vendors: [],
        quota_per_unit: 69444,
        currency: "CNY",
        currency_symbol: "¥",
        cny_per_usd: 7.2,
        other_currency: "USD",
        other_quota_per_unit: 500000,
        defaults: {},
      }),
    }),
  );
  await page.route("**/api/portal/usage**", (route) => {
    const url = new URL(route.request().url());
    capture?.(url);
    // 详情端点必须**先于**列表分支判断：`/usage/102` 也满足 "**/api/portal/usage**"，
    // 落到列表分支会返回分页体（code:0 但结构不对），抽屉里读到 undefined 还不会报错 ——
    // 这类"桩件太宽"造成的假绿是最难查的一类测试缺陷。
    const detailMatch = url.pathname.match(/^\/api\/portal\/usage\/(\d+)$/);
    if (detailMatch) {
      route.fulfill({ json: envelope(usageDetail(Number(detailMatch[1]))) });
      return;
    }
    if (url.pathname.endsWith("/export")) {
      route.fulfill({
        status: 200,
        contentType: "text/csv",
        body: "时间,模型\n2026-10-19 12:00:00,gpt-4o\n",
      });
      return;
    }
    const modality = url.searchParams.get("modality") ?? "";
    const status = url.searchParams.get("status") ?? "";
    const model = url.searchParams.get("model") ?? "";
    const keyId = url.searchParams.get("key_id") ?? "";
    const start = url.searchParams.get("start");
    const end = url.searchParams.get("end");
    let items = USAGES;
    if (modality) items = items.filter((u) => u.modality === modality);
    if (status) items = items.filter((u) => u.status === status);
    if (model) items = items.filter((u) => u.model.includes(model));
    if (keyId) items = items.filter((u) => String(u.key_id) === keyId);
    if (start) items = items.filter((u) => at(u.created_at) >= new Date(start));
    if (end) items = items.filter((u) => at(u.created_at) <= new Date(end));

    // 只返回分页三件套：`modality_dist` 已不再被本页消费（类型分布卡在首页），
    // 桩件里保留一份没人读的聚合只会让「前端到底依赖什么」变模糊。
    route.fulfill({
      json: envelope({ items, total: items.length, page: 1, page_size: 20 }),
    });
  });

  // 同步产物代理端点：门户不直连上游，图片字节由平台转出。
  // 必须注册在列表路由之后（Playwright 后注册优先），否则会被 `**/api/portal/usage**` 吞掉。
  await page.route("**/api/portal/usage/*/media/*", (route) =>
    route.fulfill({
      status: 200,
      contentType: "image/png",
      body: Buffer.from(
        "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
        "base64",
      ),
    }),
  );
}

test.describe("S5 调用记录 · 明细与筛选", () => {
  test("列表展示：费用双币换算、状态与类型徽标", async ({ page }) => {
    await seedAuth(page);
    await stubUsage(page);

    await page.goto("/usage");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("调用记录");

    // 汇总条：今日请求（次数）/ 今日费用 / 本月费用（quota → USD）
    const strip = page.locator(".stat-strip .stat-card");
    await expect(strip).toHaveCount(3);
    await expect(strip.nth(0)).toContainText("今日请求");
    await expect(strip.nth(0)).toContainText("2");
    // 与首页同一套副文案：成功数由 requests - errors - pending 反推，失败率同源
    await expect(strip.nth(0)).toContainText("成功 1 · 失败 1（50.0%）");
    await expect(strip.nth(1)).toContainText("今日费用");
    await expect(strip.nth(1)).toContainText("¥1.5");
    await expect(strip.nth(1)).toContainText("截至 14:32");
    await expect(strip.nth(2)).toContainText("本月费用");
    // 本月卡把「自然月累计消耗」与「本月到账充值」并排，口径与钱包流水一致
    await expect(strip.nth(2)).toContainText("自然月累计 · 充值 ¥2");

    const rows = page.locator(".tbl tbody tr");
    await expect(rows).toHaveCount(2);

    // gpt-4o 已结算：charge=1500000 微 = ¥1.5（金额口径已统一为微，不再除 quota_per_unit）
    // （次币种参考列默认关闭，因此不再断言 .y 那一行）
    const chat = page.locator(".tbl tbody tr", { hasText: "gpt-4o" });
    await expect(chat.locator(".quota-unit .q")).toContainText("¥1.5");
    await expect(chat).toContainText("已成功");
    await expect(chat.locator(".badge.badge-primary")).toContainText("对话");

    // released 行 → 失败（未计费）
    const emb = page.locator(".tbl tbody tr", { hasText: "text-embedding-3-large" });
    await expect(emb).toContainText("失败");
    await expect(emb.locator(".badge.badge-info")).toContainText("向量");
  });

  test("密钥列显示名称而非 ID", async ({ page }) => {
    await seedAuth(page);
    await stubUsage(page);

    await page.goto("/usage");
    await expect(page.locator(".tbl tbody tr").first()).toContainText("生产密钥");
  });

  test("按类型筛选：仅剩向量调用", async ({ page }) => {
    await seedAuth(page);
    await stubUsage(page);

    await page.goto("/usage");
    await expect(page.locator(".tbl tbody tr")).toHaveCount(2);

    // pbar-fields 内 select 顺序：密钥 / 类型 / 状态 / 时间
    await page.locator(".pbar-fields select").nth(1).selectOption("embedding");

    await expect(page.locator(".tbl tbody tr")).toHaveCount(1);
    await expect(page.locator(".tbl tbody tr").first()).toContainText("text-embedding-3-large");
    // 类型分布卡已迁到首页（/home 的整行「类型分布」卡），调用记录页不再有它。
    // 反向断言防回退：两处都放会让同一指标出现两个页面、两套口径。
    await expect(page.getByText("类型分布")).toHaveCount(0);
  });
});

test.describe("S17 调用记录 · 日期区间 / 导出 / 明细抽屉", () => {
  test("日期区间：按本地日界转 RFC3339 下发，超出数据区间回落空态", async ({ page }) => {
    await seedAuth(page);
    const lists: URL[] = [];
    await stubUsage(page, (u) => {
      if (!u.pathname.endsWith("/export")) lists.push(u);
    });

    await page.goto("/usage");
    await expect(page.locator(".tbl tbody tr")).toHaveCount(2);

    await page.locator('input[aria-label="开始日期"]').fill("2026-10-20");
    await expect(page.locator(".tbl tbody tr")).toHaveCount(0);
    await expect(page.locator(".empty")).toContainText("暂无调用记录");

    const sent = lists[lists.length - 1];
    const start = sent.searchParams.get("start");
    // 当日 00:00:00 本地日界（而非 UTC 零点，避免时区截断丢当天数据）
    expect(new Date(start as string).getTime()).toBe(new Date("2026-10-20T00:00:00").getTime());

    await page.locator('input[aria-label="开始日期"]').fill("2026-10-19");
    await expect(page.locator(".tbl tbody tr")).toHaveCount(2);
  });

  test("时间范围布局：起止输入同排、与筛选控件等高、合并在一个外框内", async ({ page }) => {
    await seedAuth(page);
    await stubUsage(page);
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto("/usage");

    const start = page.locator('input[aria-label="开始日期"]');
    const end = page.locator('input[aria-label="结束日期"]');
    await expect(start).toBeVisible();
    await expect(end).toBeVisible();

    // 合并为一个范围控件（单一外框 + 中间破折号）
    await expect(page.locator(".pbar-range")).toHaveCount(1);
    await expect(page.locator(".pbar-range input")).toHaveCount(2);

    const sb = await start.boundingBox();
    const eb = await end.boundingBox();
    const gb = await page.locator(".pbar-range").boundingBox();
    const selb = await page.locator(".pbar-fields select").first().boundingBox();
    expect(sb && eb && gb && selb, "筛选控件未渲染").toBeTruthy();

    // 起止同一行（旧实现每个日期独占一行，纵向相差一个控件高度）
    expect(Math.abs((sb as { y: number }).y - (eb as { y: number }).y), "起止日期不在同一行").toBeLessThanOrEqual(1);
    // 外框与相邻 select 等高（旧实现 date 沿用全局 42px，与 36px 的 select 不齐）
    expect(Math.round((gb as { height: number }).height), "时间范围外框与 select 不等高").toBe(
      Math.round((selb as { height: number }).height),
    );
    // 内框随外框收缩，两输入等高
    expect(Math.round((sb as { height: number }).height)).toBe(Math.round((eb as { height: number }).height));
    // 不撑满整行（全局 input[type=date] 是 width:100%，必须被筛选条覆盖）
    const bar = await page.locator(".pbar").boundingBox();
    expect((sb as { width: number }).width, "日期输入宽度异常（疑似回退为 100%）").toBeLessThan((bar as { width: number }).width / 4);
  });

  test("区间颠倒：不下发 end 并显式提示", async ({ page }) => {
    await seedAuth(page);
    const lists: URL[] = [];
    await stubUsage(page, (u) => {
      if (!u.pathname.endsWith("/export")) lists.push(u);
    });

    await page.goto("/usage");
    await page.locator('input[aria-label="开始日期"]').fill("2026-10-19");
    await page.locator('input[aria-label="结束日期"]').fill("2026-10-18");

    await expect(page.locator(".hint.warn")).toContainText("开始日期不得晚于结束日期");
    // 仅按开始日期筛选：10-18 的行未命中，10-19 的两行保留
    await expect(page.locator(".tbl tbody tr")).toHaveCount(2);

    const sent = lists[lists.length - 1];
    expect(sent.searchParams.get("start")).toBeTruthy();
    expect(sent.searchParams.get("end")).toBeNull();
  });

  test("导出 CSV：命中门户导出端点并携带当前筛选", async ({ page }) => {
    await seedAuth(page);
    const exports: URL[] = [];
    await stubUsage(page, (u) => {
      if (u.pathname.endsWith("/export")) exports.push(u);
    });

    await page.goto("/usage");
    await page.locator(".pbar-fields select").nth(1).selectOption("embedding");
    await expect(page.locator(".tbl tbody tr")).toHaveCount(1);

    // 断言导出请求发出且携带当前筛选（download/toast 链在多 worker 高负载下
    // 受 Playwright route fulfill 延迟影响不稳定，不作断言）
    await page.getByRole("button", { name: "导出 CSV" }).click();

    await expect.poll(() => exports).toHaveLength(1);
    expect(exports[0].searchParams.get("modality")).toBe("embedding");
  });

  test("行点击：打开明细抽屉并展示全字段", async ({ page }) => {
    await seedAuth(page);
    await stubUsage(page);

    await page.goto("/usage");
    // 等行渲染完再点：高负载下"先空态后数据"会造成元素反复挂载/卸载（element not stable）
    const row = page.locator(".tbl tbody tr", { hasText: "gpt-4o" });
    await expect(row).toBeVisible();
    await row.click();

    const drawer = page.locator(".drawer.open");
    await expect(drawer).toContainText("调用明细 #102");
    await expect(drawer).toContainText("gpt-4o");
    await expect(drawer).toContainText("生产密钥");

    // 门户展示的是**用户自己的**两份报文：他发出去的、以及平台返回给他的。
    // 管理台的「客户端入站 / 实际出站」两段仍不出现（那是服务商侧信息）。
    await expect(drawer).toContainText("请求内容");
    await expect(drawer).toContainText("我发出的请求");
    await expect(drawer).toContainText("messages");
    await expect(drawer).toContainText("响应内容");
    await expect(drawer).toContainText("我收到的响应");
    await expect(drawer).not.toContainText("客户端入站");
    await expect(drawer).not.toContainText("实际出站");
    // 供应链信息一个都不能漏进来（与后端 portal_leak_test 同一口径）。
    await expect(drawer).not.toContainText("上游");
    await expect(drawer).not.toContainText("渠道");
    await expect(drawer).not.toContainText("适配器");
    // 门户的"结果"是抽出的纯文本输出
    await expect(drawer).toContainText("输出文本");
    await expect(drawer).toContainText("有什么可以帮你");
    // 报文摘要行：只有三个统计量
    await expect(drawer).toContainText("请求 96 B");
    await expect(drawer).toContainText("响应 78 B");
    // 耗时拆分（总耗时 / 服务 / 首字 / 尝试次数），文案不含"上游/换渠道"
    await expect(drawer).toContainText("服务 1.10s");
    await expect(drawer).toContainText("首字延迟");
    await expect(drawer).toContainText("尝试 2 次");
    await expect(drawer).not.toContainText("换渠道");

    // R3：排障信息默认折叠 —— 请求 ID 属于排障线索，展开后才进 DOM
    await expect(drawer.getByText("trace_abc")).toHaveCount(0);
    await drawer.getByRole("button", { name: /排障信息/ }).click();
    await expect(drawer).toContainText("trace_abc");

    // R3：扣费明细默认只显示标题，展开后才有明细行
    await expect(drawer).toContainText("扣费明细");
    await drawer.getByRole("button", { name: /扣费明细/ }).click();
    await expect(drawer).toContainText("冻结额");

    await drawer.getByRole("button", { name: "关闭" }).click();
    await expect(page.locator(".drawer.open")).toHaveCount(0);
  });

  test("媒体结果可预览，Esc 只关预览层", async ({ page }) => {
    await seedAuth(page);
    await stubUsage(page);

    await page.goto("/usage");
    const row = page.locator(".tbl tbody tr", { hasText: "gpt-4o" });
    await expect(row).toBeVisible();
    await row.click();

    const drawer = page.locator(".drawer.open");
    const cell = drawer.locator(".cd-media-item").first();
    await expect(cell).toBeVisible();
    await cell.click();

    const lb = page.locator(".lb");
    await expect(lb).toBeVisible();
    // 不可预览的媒体只给说明，不给播放器（零转存：内容从不落盘）
    await expect(lb).toContainText("1 / 2");

    await page.keyboard.press("Escape");
    await expect(lb).toHaveCount(0);
    // 关键断言：Esc 不能连带关掉抽屉（两个弹层的 keydown 必须隔离）
    await expect(page.locator(".drawer.open")).toHaveCount(1);
  });

  test("异步任务的调用详情：任务信息来自 task，产物不再读 call_io.media", async ({ page }) => {
    await seedAuth(page);
    await stubUsage(page);
    const base = usageDetail(102);
    // 注册在 stubUsage 之后（Playwright 后注册优先），否则详情会落到列表分支。
    await page.route("**/api/portal/usage/102", (route) =>
      route.fulfill({
        json: envelope({
          ...base,
          call_kind: "async",
          task_id: 55,
          // 异步任务不再写媒体快照：产物只在 task.contents（这里故意为空，验证空态原因）。
          call_io: { ...base.call_io, media: [], output_text: "", resp_body: "" },
          // 任务对象与 /v1/tasks/{id} **同一个结构体**（后端 taskview.View）：
          // id 是十进制字符串、时间是 Unix 秒、error 恒为对象、contents 恒为数组。
          task: {
            id: "55",
            object: "task",
            model: "gpt-4o",
            modality: "video",
            status: "failed",
            kind: "video",
            progress: 100,
            error: { message: "任务处理失败，已全额返还" },
            created_at: 1760858400,
            completed_at: 1760858700,
            deadline_at: 1760862000,
            contents: [],
            usage: { images: 0, seconds: 5, ref_images: 0 },
          },
        }),
      }),
    );

    await page.goto("/usage");
    const row = page.locator(".tbl tbody tr", { hasText: "gpt-4o" });
    await expect(row).toBeVisible();
    await row.click();

    const drawer = page.locator(".drawer.open");
    // 失败任务：任务信息默认展开，进度不再画满格，而是「已结束」。
    await expect(drawer).toContainText("任务信息");
    await expect(drawer).toContainText("已结束");
    await expect(drawer).toContainText("任务处理失败，已全额返还");
    await expect(drawer).toContainText("任务未完成，没有产物");
    await expect(drawer).toContainText("超时时间");
    // 门户仍不能看到任何供应链字样（任务信息分区同样受白名单约束）。
    await expect(drawer).not.toContainText("上游");
    await expect(drawer).not.toContainText("渠道");
    await expect(drawer).not.toContainText("适配器");
  });
});


test.describe("门户 chrome · 顶栏常驻与页签吸附", () => {
  /**
   * 40 行记录：现有桩件只有 2 行，页面不足一屏就滚不动，
   * 而这条缺陷**只在滚过一屏后**才出现（#root 的包含块边界）。
   */
  const MANY = Array.from({ length: 40 }, (_, i) => ({
    ...USAGES[0],
    id: 500 + i,
    created_at: `2026-10-19 ${String(9 + (i % 10)).padStart(2, "0")}:${String(i % 60).padStart(2, "0")}:00`,
  }));

  /** 覆盖列表端点（后注册优先），其余桩件沿用 stubUsage。 */
  async function stubLongList(page: Page): Promise<void> {
    await seedAuth(page);
    await stubUsage(page);
    await page.route("**/api/portal/usage**", (route) => {
      const url = new URL(route.request().url());
      // 详情 `/usage/:id` 与导出 `/usage/export` 必须放行给 stubUsage 的桩件：
      // 落到这里会拿到分页体（code:0 但结构不对），是最难查的那种假绿。
      if (url.pathname !== "/api/portal/usage") return route.fallback();
      return route.fulfill({
        json: envelope({ items: MANY, total: MANY.length, page: 1, page_size: 40 }),
      });
    });
  }

  const pin = (page: Page) =>
    page.evaluate(() => {
      const bar = document.querySelector(".page-tabs-bar")!;
      const top = document.querySelector(".p-top")!;
      const r = bar.getBoundingClientRect();
      const t = top.getBoundingClientRect();
      return {
        position: getComputedStyle(bar).position,
        barTop: Math.round(r.top),
        topTop: Math.round(t.top),
        topBottom: Math.round(t.bottom),
        scrollable: document.documentElement.scrollHeight - window.innerHeight,
      };
    });

  test("深滚后顶栏仍在视口顶部，页签贴合其下沿（不留 64px 空白）", async ({ page }) => {
    await stubLongList(page);
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/usage");
    await expect(page.locator(".tbl tbody tr")).toHaveCount(40);

    const before = await pin(page);
    expect(before.position, "页签条必须是 sticky，否则滚动时会整条滚走").toBe("sticky");
    expect(before.scrollable, "页面必须真的能滚过一屏，否则测不到吸附").toBeGreaterThan(900);

    await page.evaluate(() => window.scrollTo(0, 1400));
    const pinned = await pin(page);
    // 根因断言：顶栏不能被包含块带走（#root 若又写成 height:100% 就会变成负数）
    expect(pinned.topTop, "顶栏滚出了视口：检查 app.css 的 #root 是否为 min-height").toBe(0);
    // 空白带断言：页签条上沿必须贴住顶栏下沿（允许 1px 边框舍入）
    expect(
      Math.abs(pinned.barTop - pinned.topBottom),
      `顶栏与页签之间出现留白（顶栏 ${pinned.topTop}~${pinned.topBottom}，页签 ${pinned.barTop}）`,
    ).toBeLessThanOrEqual(1);
    // 页签不能滑到顶栏下面被挡住
    expect(pinned.barTop).toBeGreaterThan(0);
  });

  test("移动端 375：吸附加 52px 顶栏，且无横向溢出", async ({ page }) => {
    await stubLongList(page);
    await page.setViewportSize({ width: 375, height: 720 });
    await page.goto("/usage");
    // 必须先等列表渲染完：页签条没到吸附点就滚不动（scrollTo 会被夹到当前 scrollHeight）
    await expect(page.locator(".tbl tbody tr")).toHaveCount(40);
    await page.evaluate(() => window.scrollTo(0, 900));
    expect(await page.evaluate(() => window.scrollY), "页面没有真的滚动").toBeGreaterThan(800);

    const m = await pin(page);
    expect(m.topTop).toBe(0);
    // 移动端顶栏压到 52px（tokens.css 的 --topbar-h 媒体查询）
    expect(m.topBottom).toBeLessThanOrEqual(53);
    expect(Math.abs(m.barTop - m.topBottom)).toBeLessThanOrEqual(1);
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth),
      "375px 出现横向滚动",
    ).toBe(false);
  });
});
