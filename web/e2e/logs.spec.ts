/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, isAdminPageRequest, seedAuth } from "./helpers";

/** GET /api/admin/logs fixture（明细 + 归档共用；kind 由后端派生） */
const LOG_ITEMS = [
  {
    id: 1001,
    created_at: "2026-10-20T10:00:00+08:00",
    user_id: 7,
    username: "alice",
    key_id: 3,
    key_name: "prod-key",
    model: "gpt-4o",
    channel_id: 1,
    channel_name: "OpenAI 主线路",
    modality: "chat",
    kind: "consume",
    status: "settled",
    http_status: 200,
    prompt_tokens: 100,
    completion_tokens: 200,
    total_tokens: 300,
    charge: 250000,
    latency_ms: 842,
    error_msg: "",
    is_stream: true,
    ip: "1.2.3.4",
  },
  {
    id: 1002,
    created_at: "2026-10-20T09:30:00+08:00",
    user_id: 8,
    username: "bob",
    key_id: 4,
    key_name: "test-key",
    model: "claude-sonnet",
    channel_id: 2,
    channel_name: "火山方舟",
    modality: "chat",
    kind: "upstream_error",
    status: "released",
    http_status: 502,
    prompt_tokens: 0,
    completion_tokens: 0,
    total_tokens: 0,
    charge: 0,
    latency_ms: 1500,
    error_msg: "upstream 502 bad gateway",
    is_stream: false,
    ip: "5.6.7.8",
  },
];

/**
 * 调用记录详情 fixture（C1）：列表是裁剪投影，详情才带原始报文。
 *
 * `resp_error` 刻意给上游原文（含主机名）：详情页要能看清"上游到底说了什么"，
 * 而列表里的 error_msg 只是 500 字符摘要。
 */
function logDetail(id: number) {
  const base = LOG_ITEMS.find((i) => i.id === id) ?? LOG_ITEMS[0];
  return {
    ...base,
    id,
    elapsed_ms: base.latency_ms,
    upstream_ms: base.latency_ms - 100,
    first_token_ms: 180,
    attempts: 1,
    started_at: base.created_at,
    finished_at: base.created_at,
    request_id: "trace_1001",
    upstream_request_id: "req_up_9",
    call_kind: "sync",
    usage_semantic: "openai",
    tier_unit_price_quota: 0,
    // 详情是**未截顶**的账：charge_before_cap 是结算前的计算值，frozen 是冻结额。
    // 两个字段都叫 `charge`/`frozen`（旧名 charge_quota / frozen_quota 已删除）。
    charge_before_cap: base.charge,
    frozen: base.charge,
    usage_source: "upstream",
    anomaly: "",
    call_io: {
      req_raw: '{"model":"gpt-4o","messages":[{"role":"user","content":"今天天气如何"}],"authorization":"******"}',
      req_bytes: 88,
      req_sha256: "d17c",
      req_truncated: false,
      resp_raw: '{"choices":[{"message":{"content":"晴，26 度"}}]}',
      resp_error: "",
      resp_bytes: 44,
      sse_frames: 0,
      media: [],
    },
  };
}

const LOG_ANALYTICS = {
  kinds: [
    { label: "consume", value: 12 },
    { label: "upstream_error", value: 3 },
  ],
  errors: [{ label: "claude-sonnet", value: 3, share: 0.75 }],
};

const AUDIT_ITEM = {
  id: 9,
  created_at: "2026-10-20T09:00:00+08:00",
  admin_id: 1,
  actor: "admin",
  action: "user.update",
  target_type: "user",
  target_id: "7",
  detail: { status: 1 },
};

/** 库内实际动作集合（数据驱动筛选，避免枚举漂移） */
const AUDIT_META = {
  actions: [
    { action: "user.update", value: 3 },
    { action: "channel.create", value: 1 },
  ],
  retention_days: 90,
};

/** 管理台页面必须打桩 /api/admin/**（否则真实 401 触发全局登出）；记录已发生的请求 */
async function stub(page: Page, opts?: { detail?: (id: number) => unknown }): Promise<string[]> {
  const calls: string[] = [];
  const csv = (name: string) => ({
    status: 200,
    headers: {
      "Content-Type": "text/csv; charset=utf-8",
      "Content-Disposition": `attachment; filename="${name}"`,
    },
    body: "\ufeff时间,用户,KEY\r\n",
  });

  await page.route(
    (url) => isAdminPageRequest(url),
    (route) => {
      const u = new URL(route.request().url());
      calls.push(`${route.request().method()} ${u.pathname}${u.search}`);

      // 详情必须**先于**列表判断：`/admin/logs/1001` 不等于 `/admin/logs`，
      // 但下面的 fallback 是 `envelope(null)`，会让抽屉永远停在"加载中"。
      const detailMatch = u.pathname.match(/^\/api\/admin\/logs\/(\d+)$/);
      if (detailMatch) {
        const id = Number(detailMatch[1]);
        return route.fulfill({
          json: envelope(opts?.detail ? opts.detail(id) : logDetail(id)),
        });
      }
      if (u.pathname === "/api/admin/logs") {
        return route.fulfill({
          json: envelope({ items: LOG_ITEMS, total: 2, page: 1, page_size: 20 }),
        });
      }
      if (u.pathname === "/api/admin/apps/options") {
        return route.fulfill({
          json: envelope({
            items: [
              { id: 5, slug: "web-portal", name: "web-portal", display_name: "官网门户", status: 1 },
            ],
            truncated: false,
          }),
        });
      }
      if (u.pathname === "/api/admin/logs/analytics") {
        return route.fulfill({ json: envelope(LOG_ANALYTICS) });
      }
      if (u.pathname === "/api/admin/logs/export") {
        return route.fulfill(csv("logs.csv"));
      }
      if (u.pathname === "/api/admin/audits") {
        return route.fulfill({
          json: envelope({ items: [AUDIT_ITEM], total: 1, page: 1, page_size: 20 }),
        });
      }
      if (u.pathname === "/api/admin/audits/meta") {
        return route.fulfill({ json: envelope(AUDIT_META) });
      }
      if (u.pathname === "/api/admin/audits/export") {
        return route.fulfill(csv("audits.csv"));
      }
      if (u.pathname === "/api/portal/announcements") {
        return route.fulfill({ json: envelope({ enabled: false, items: [] }) });
      }
      if (u.pathname === "/api/portal/config") {
        return route.fulfill({
          json: envelope({
            site: { site_name: "灵动 API", logo_image: "" },
            group_ratios: { default: 1 },
          }),
        });
      }
      return route.fulfill({ json: envelope(null) });
    },
  );
  return calls;
}

/** 仅桌面内联的筛选控件位于 .pbar-fields（Sheet 内的同名字段不参与） */
const fields = (page: Page) => page.locator(".pbar-fields");

test.describe("S11 调用日志 · 管理台", () => {
  test("明细筛选、事件类型/状态标签、详情抽屉与 CSV 导出", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page);
    await page.goto("/admin/logs");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("调用日志");
    await expect(page.locator(".ph-desc")).toHaveText("全部调用记录查询与分析");
    await expect(page.locator(".count-line")).toContainText("共 2 条");

    // 明细行：派生事件类型 + HTTP 状态
    const row = page.locator(".tbl tbody tr", { hasText: "claude-sonnet" });
    await expect(row.locator(".badge", { hasText: "上游错误" })).toHaveText("上游错误");
    await expect(row.locator(".badge", { hasText: "502" })).toHaveText("502");

    // 原型区块顺序：表格在前，分析卡在后
    const tableY = (await page.locator(".tbl").boundingBox())!.y;
    const chartY = (await page.locator(".grid.grid-2").boundingBox())!.y;
    expect(tableY).toBeLessThan(chartY);

    // 分析卡（类型分布 + 错误排行 share）
    await expect(page.locator(".card", { hasText: "错误排行" })).toContainText("75.0%");

    // 事件类型筛选 → 请求携带 kind
    await fields(page).getByLabel("事件类型").selectOption("upstream_error");
    await expect
      .poll(() => calls.some((c) => c.includes("/api/admin/logs?") && c.includes("kind=upstream_error")))
      .toBe(true);

    // 模型关键字筛选
    await fields(page).getByLabel("模型").fill("claude");
    await expect.poll(() => calls.some((c) => c.includes("/api/admin/logs?") && c.includes("model=claude"))).toBe(true);

    // 导出 CSV：断言 /export 请求发出（先于抽屉，避免抽屉遮罩拦截点击；
    // download/toast 链在多 worker 高负载下受 route fulfill 延迟影响不稳定，不作断言）
    await page.getByRole("button", { name: "导出 CSV" }).click();
    await expect.poll(() => calls.some((c) => c.startsWith("GET /api/admin/logs/export?"))).toBe(true);

    // 详情抽屉（C1）：标题带记录 id，内容是详情接口的完整视图
    await page.locator(".tbl tbody tr", { hasText: "gpt-4o" }).getByRole("button", { name: "详情" }).click();
    const drawer = page.getByRole("dialog", { name: "调用明细 #1001" });
    await expect(drawer).toBeVisible();
    // 入站与出站分两段：排障要能看出平台改了什么（2026-11-04 的教训）
    await expect(drawer).toContainText("客户端入站");
    await expect(drawer).toContainText("今天天气如何");
    // 出站段恒渲染（没有采集到时报「未采集」，而不是整段消失）
    await expect(drawer).toContainText("实际出站");
    // 流式记录的标题是「上游返回（流式拼接）」——这里只锁住"有输出原文"这件事
    await expect(drawer).toContainText("上游返回（");

    // R3：排障信息默认折叠 —— 渠道 / 请求 ID / 上游单号都在里面，展开后才进 DOM
    await expect(drawer.getByText("trace_1001")).toHaveCount(0);
    await drawer.getByRole("button", { name: /排障信息/ }).click();
    await expect(drawer).toContainText("trace_1001");
    await expect(drawer).toContainText("OpenAI 主线路");
    await expect(drawer).toContainText("req_up_9");

    // 扣费区默认折叠：先看标题，展开后看金额
    await expect(drawer).toContainText("扣费明细");
    await drawer.getByRole("button", { name: /扣费明细/ }).click();
    await expect(drawer).toContainText("实收");
  });

  test("异步调用详情：任务信息分区展示进度与管理台原文", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    // 详情由 stub 的 detail 注入，避免与列表/详情共用的断言路由抢匹配。
    await stub(page, {
      detail: (id) => ({
        ...logDetail(id),
        call_kind: "async",
        task_id: 55,
        // 任务对象与 /v1/tasks/{id} 同一个结构体（后端 taskview.View）：
        // error 恒为分类文案；排障原文走**伴随字段** task_error_msg（只有管理台下发）。
        task: {
          id: "55",
          object: "task",
          model: "gpt-4o",
          modality: "video",
          status: "failed",
          // timeout 是唯一需要额外解释的诊断标记（其余 kind 与 modality 同值）。
          kind: "timeout",
          progress: 100,
          error: { message: "任务处理失败，已全额返还" },
          created_at: 1790000000,
          completed_at: 1790000300,
          contents: [],
          usage: { images: 0, seconds: 0, ref_images: 0 },
        },
        task_error_msg: "提交失败：vendor.example.com 拒绝",
      }),
    });

    await page.goto("/admin/logs");
    await page
      .locator(".tbl tbody tr", { hasText: "gpt-4o" })
      .getByRole("button", { name: "详情" })
      .click();

    const drawer = page.getByRole("dialog", { name: "调用明细 #1001" });
    await expect(drawer).toContainText("任务信息");
    // 失败任务默认展开：终态不画满格进度条，而是「已结束」。
    await expect(drawer).toContainText("已结束");
    await expect(drawer).toContainText("轮询网络失败超限");
    // 管理台看的是失败原文（门户拿到的是分类文案，见 usage.spec）。
    await expect(drawer).toContainText("提交失败：vendor.example.com 拒绝");
    await expect(drawer).toContainText("任务未完成，没有产物");
  });

  test("明细/归档切换携带 scope", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page);
    await page.goto("/admin/logs");

    await expect(page.locator(".count-line")).toContainText("共 2 条");
    await expect(page.locator(".tbl")).toBeVisible();
    await page.getByRole("tab", { name: "归档" }).click();
    await expect(page).toHaveURL(/tab=archive/);
    await expect
      .poll(() => calls.some((c) => c.includes("/api/admin/logs?") && c.includes("scope=archive")))
      .toBe(true);
  });
});

test.describe("S11 审计日志 · 管理台", () => {
  test("动作集合数据驱动、详情 JSON 与 CSV 导出", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page);
    await page.goto("/admin/audit");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("审计日志");
    await expect(page.locator(".ph-desc")).toHaveText("管理员操作留痕");
    await expect(page.locator(".count-line")).toContainText("保留 90 天");

    // 动作下拉由 /audits/meta 数据驱动（「全部动作」+ 2 个库内动作）
    const action = fields(page).getByLabel("动作");
    await expect(action.locator("option")).toHaveCount(3);
    await action.selectOption("user.update");
    await expect
      .poll(() => calls.some((c) => c.includes("/api/admin/audits?") && c.includes("action=user.update")))
      .toBe(true);

    // 导出 CSV（先于抽屉，避免抽屉遮罩拦截点击）
    await page.getByRole("button", { name: "导出 CSV" }).click();
    await expect.poll(() => calls.some((c) => c.startsWith("GET /api/admin/audits/export?"))).toBe(true);

    // 行 + 详情手风琴（detail JSON pretty）
    const row = page.locator(".au-row", { hasText: "user.update" });
    await expect(row).toContainText("admin");
    await expect(row.locator(".json-view")).toBeHidden();
    await row.locator("summary").click();
    await expect(row.locator(".json-view")).toBeVisible();
    await expect(row.locator(".json-view")).toContainText('"status": 1');
  });
});

test.describe("调用日志 · 按应用筛选", () => {
  test("应用筛选同时进入列表与图表请求（不能只筛列表）", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page);
    await page.goto("/admin/logs");
    await expect(page.locator(".tbl thead")).toContainText("应用");

    await page.getByRole("combobox", { name: "应用" }).click();
    await page.getByRole("option", { name: /官网门户/ }).click();

    // 列表与 analytics 是**两条**请求：只改 where() 不改 AnalyticsData 时，
    // 图表会停在"全站口径"而列表已经收敛 —— 这个用例就是那条防线的端到端版本。
    await expect
      .poll(() => calls.some((c) => c.startsWith("GET /api/admin/logs?") && c.includes("app_id=5")))
      .toBe(true);
    await expect
      .poll(() =>
        calls.some((c) => c.startsWith("GET /api/admin/logs/analytics") && c.includes("app_id=5")),
      )
      .toBe(true);
  });
});
