/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, expectNoHScroll, seedAuth } from "./helpers";

/** 门户用户信息（S7 页面加载余额/预扣口径） */
const ME = {
  id: 1,
  username: "e2e",
  role: 1,
  status: 1,
  group: "default",
  balance: 5000000,
  frozen: 0,
  used: 0,
  request_count: 0,
  key_count: 0,
  key_count_enabled: 0,
  group_discount: 1_000_000,
  group_configured: false,
  email: "",
  phone: "",
};

/** 管理台用户信息（role=0 通过守卫） */
const ME_ADMIN = { ...ME, username: "admin", role: 0 };

/** 异步任务桩件（仅 1 条 queued，用于管理台表头断言；不含真实业务数据） */
const TASK = {
  id: 55,
  user_id: 3,
  key_id: 7,
  channel_id: 1,
  platform: "openai",
  model: "sora-2",
  modality: "video",
  status: "queued",
  progress: 0,
  kind: "video",
  error: "",
  frozen: 2500000,
  settled: 0,
  upstream_task_id: "up_55",
  result_url: "",
  created_at: "2026-10-19 12:00:00",
  updated_at: "2026-10-19 12:00:00",
  deadline_at: "2026-10-19 13:00:00",
};

const emptyPage = { items: [], total: 0, page: 1, page_size: 20 };

/** 门户桩：me / keys / usage / tasks（tasks 为空，覆盖空态） */
async function stubPortal(page: Page): Promise<void> {
  await page.route("**/api/portal/me", (route) => route.fulfill({ json: envelope(ME) }));
  await page.route("**/api/portal/keys**", (route) => route.fulfill({ json: envelope({ items: [] }) }));
  await page.route("**/api/portal/usage**", (route) => route.fulfill({ json: envelope(emptyPage) }));
  await page.route("**/api/portal/tasks**", (route) => route.fulfill({ json: envelope(emptyPage) }));
}

/** 管理台桩：me / tasks（tasks 返回 1 条，用于表头断言） */
async function stubAdmin(page: Page): Promise<void> {
  await page.route("**/api/portal/me", (route) => route.fulfill({ json: envelope(ME_ADMIN) }));
  await page.route("**/api/admin/tasks**", (route) =>
    route.fulfill({ json: envelope({ items: [TASK], total: 1, page: 1, page_size: 20 }) }),
  );
}

test.describe("S7 异步任务 · 门户", () => {
  test("切换「异步任务」TAB：筛选项与空态渲染，移动端无横向滚动", async ({ page }) => {
    await seedAuth(page);
    await stubPortal(page);

    await page.goto("/usage");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("调用记录");

    // 切到异步任务 TAB
    await page.getByRole("tab", { name: "异步任务" }).click();
    await expect(page).toHaveURL(/tab=tasks/);

    // 筛选条：状态下拉 + 模型关键词输入
    await expect(page.locator(".pbar-fields select")).toHaveCount(1);
    await expect(page.locator('.pbar input[type="search"]')).toHaveCount(1);

    // 无任务数据时的空态与退款说明
    await expect(page.getByText("暂无异步任务")).toBeVisible();
    await expect(page.getByText("失败、超时或已取消的任务会自动全额退回预扣额度。")).toBeVisible();

    // 移动端（375px）无横向滚动
    await page.setViewportSize({ width: 375, height: 800 });
    await expect(page.getByText("暂无异步任务")).toBeVisible();
    await expectNoHScroll(page, "/usage 异步任务 375px");
  });
});

test.describe("S7 异步任务 · 管理台", () => {
  test("异步任务页：筛选条与表头渲染", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page);

    await page.goto("/admin/tasks");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("异步任务");
    await expect(page.locator(".ph-desc")).toHaveText("任务进度查看与额度结算");

    // 筛选条对齐原型：仅「状态」一个字段（无模型/用户 ID）
    await expect(page.locator(".pbar-fields select")).toHaveCount(1);
    await expect(page.locator('.pbar input[type="search"]')).toHaveCount(0);
    await expect(page.locator('.pbar input[type="text"]')).toHaveCount(0);

    // 表头列
    const head = page.locator(".tbl thead");
    await expect(head).toContainText("ID");
    await expect(head).toContainText("状态");
    await expect(head).toContainText("进度");
    await expect(head).toContainText("费用");
    await expect(head).toContainText("操作");

    // 行：queued 任务显示取消按钮
    await expect(page.locator(".tbl tbody tr")).toHaveCount(1);
    await expect(page.locator(".tbl tbody tr").first()).toContainText("排队中");

    // 表格直挂（无多余 .card 外包）；行操作为文字按钮
    await expect(page.locator(".card .tbl")).toHaveCount(0);
    await expect(
      page.locator(".tbl tbody tr").first().getByRole("button", { name: "详情" }),
    ).toHaveClass(/t-act/);
  });

  test("expired 任务不提供「重试」：后端仅 failed 可重试", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await page.route("**/api/admin/tasks**", (route) =>
      route.fulfill({
        json: envelope({
          items: [{ ...TASK, status: "expired", progress: 100 }],
          total: 1,
          page: 1,
          page_size: 20,
        }),
      }),
    );

    await page.goto("/admin/tasks");
    const row = page.locator(".tbl tbody tr").first();
    await expect(row).toContainText("已过期");
    await expect(row.getByRole("button", { name: "重试" })).toHaveCount(0);
  });

  test("失败任务行提供「重试」，重试成功后状态转为排队中（重试入口随之消失）", async ({ page }) => {
    await seedAuth(page, { role: 0 });

    // 可变状态机：POST /retry 置为 queued，随后列表接口回读该状态，验证重试成功后的刷新
    let status = "failed";
    await page.route("**/api/admin/tasks**", (route) => {
      const req = route.request();
      if (req.method() === "POST" && req.url().includes("/retry")) {
        status = "queued";
        return route.fulfill({ json: envelope({ id: 55, status }) });
      }
      return route.fulfill({
        json: envelope({
          items: [{ ...TASK, status, error: "上游超时", upstream_task_id: "" }],
          total: 1,
          page: 1,
          page_size: 20,
        }),
      });
    });

    await page.goto("/admin/tasks");
    const row = page.locator(".tbl tbody tr").first();
    await expect(row).toContainText("失败");

    // 失败任务：提供「重试」动作，且不提供仅限 queued 的「取消」
    const retryBtn = row.getByRole("button", { name: "重试" });
    await expect(retryBtn).toBeVisible();
    await expect(retryBtn).toHaveClass(/t-act/);
    await expect(row.getByRole("button", { name: "取消" })).toHaveCount(0);

    // 点击经 confirmDialog 确认后触发重试
    await retryBtn.click();
    await page.getByRole("button", { name: "确认重试" }).click();

    // 重试成功：状态转为排队中，「重试」动作消失（后端仅 failed 可重试）
    await expect(row).toContainText("排队中");
    await expect(row.getByRole("button", { name: "重试" })).toHaveCount(0);
  });
});

test.describe("S7 异步任务 · 统一详情抽屉", () => {
  /** 成功的图像任务：contents 只在详情接口下发（列表为摘要）。 */
  const DONE = {
    ...TASK,
    id: 56,
    status: "succeeded",
    progress: 100,
    error: "",
    settled: 2500000,
    result_url: "/api/portal/tasks/56/contents/0",
    contents: [{ index: 0, content_type: "image/png", bytes: 1024, kind: "image" }],
  };

  test("门户「详情」：产物预览 + 任务信息，且不出现供应链字样", async ({ page }) => {
    await seedAuth(page);
    await page.route("**/api/portal/me", (route) => route.fulfill({ json: envelope(ME) }));
    await page.route("**/api/portal/keys**", (route) => route.fulfill({ json: envelope({ items: [] }) }));
    await page.route("**/api/portal/usage**", (route) => route.fulfill({ json: envelope(emptyPage) }));
    await page.route("**/api/portal/tasks**", (route) =>
      route.fulfill({ json: envelope({ items: [DONE], total: 1, page: 1, page_size: 20 }) }),
    );
    // 详情单独 stub，且注册在列表之后（后注册优先），否则详情会拿到列表体而静默空白。
    await page.route("**/api/portal/tasks/56", (route) => route.fulfill({ json: envelope(DONE) }));

    await page.goto("/usage?tab=tasks");
    await page
      .locator(".tbl tbody tr")
      .first()
      .getByRole("button", { name: "详情" })
      .click();

    const drawer = page.locator(".drawer.open");
    await expect(drawer).toContainText("任务详情 #56");
    await expect(drawer).toContainText("生成结果");
    await expect(drawer).toContainText("1 个结果");
    await expect(drawer.locator(".cd-media-item")).toHaveCount(1);
    await expect(drawer).toContainText("任务信息");
    await expect(drawer).toContainText("扣费明细");
    await expect(drawer).toContainText("产物有保留期");

    // 门户契约里根本没有渠道/上游/适配器字段，渲染层也不能出现这些词。
    await expect(drawer).not.toContainText("上游");
    await expect(drawer).not.toContainText("渠道");
    await expect(drawer).not.toContainText("适配器");

    // 成功任务的任务信息默认折叠：展开后才有进度与时间点。
    await drawer.getByRole("button", { name: /任务信息/ }).click();
    await expect(drawer).toContainText("100%");
    await expect(drawer).toContainText("创建时间");
    await expect(drawer).toContainText("超时时间");
  });

  test("管理台「详情」：同一抽屉 + 排障信息含归属与上游单号", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await page.route("**/api/portal/me", (route) => route.fulfill({ json: envelope(ME_ADMIN) }));
    await page.route("**/api/admin/tasks**", (route) =>
      route.fulfill({ json: envelope({ items: [TASK], total: 1, page: 1, page_size: 20 }) }),
    );
    await page.route("**/api/admin/tasks/55", (route) =>
      route.fulfill({
        json: envelope({
          ...TASK,
          contents: [{ index: 0, content_type: "image/png", bytes: 2048, kind: "image" }],
        }),
      }),
    );

    await page.goto("/admin/tasks");
    await page
      .locator(".tbl tbody tr")
      .first()
      .getByRole("button", { name: "详情" })
      .click();

    const drawer = page.locator(".drawer.open");
    await expect(drawer).toContainText("任务详情 #55");
    // queued：默认展开任务信息，金额口径是冻结。
    await expect(drawer).toContainText("任务信息");
    await expect(drawer).toContainText("冻结中");
    await expect(drawer.locator(".cd-media-item")).toHaveCount(1);

    // 管理台才有的归属与供应链字段下沉在排障信息里。
    await expect(drawer.getByText("openai · #1")).toHaveCount(0);
    await drawer.getByRole("button", { name: /排障信息/ }).click();
    await expect(drawer).toContainText("#3 · #7");
    await expect(drawer).toContainText("openai · #1");
    await expect(drawer).toContainText("up_55");
  });
});
