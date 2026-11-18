/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, expectNoHScroll, isAdminPageRequest, seedAuth } from "./helpers";

/** GET /api/admin/redeems fixture（status 由后端派生） */
const REDEEMS = [
  {
    id: 11,
    code: "LD-AAAA-BBBB-CCCC",
    batch: "双十一",
    quota: 1000000,
    status: "available",
    used_by: null,
    used_at: null,
    expires_at: "2026-11-30T23:59:59+08:00",
    created_at: "2026-10-20T10:00:00+08:00",
  },
  {
    id: 12,
    code: "LD-DDDD-EEEE-FFFF",
    batch: "双十一",
    quota: 1000000,
    status: "used",
    used_by: 7,
    used_at: "2026-10-20T11:00:00+08:00",
    expires_at: "2026-11-30T23:59:59+08:00",
    created_at: "2026-10-20T10:00:00+08:00",
  },
  {
    id: 13,
    code: "LD-GGGG-HHHH-IIII",
    batch: "旧活动",
    quota: 500000,
    status: "expired",
    used_by: null,
    used_at: null,
    expires_at: "2026-10-01T23:59:59+08:00",
    created_at: "2026-09-01T10:00:00+08:00",
  },
];

// 单码上限复用余额上限（微）：¥5,000,000 —— 小于「面额 60000000」又大于「面额 1000000」，
// 这样三条面额用例的先后顺序才各自命中各自的校验
const ADMIN_CONFIG = { balance_limit: 5_000_000_000_000 };

/** 记录已发生的请求；管理台必须在 /api/admin/** 打桩（否则真实 401 触发全局登出） */
async function stub(page: Page, posts: { url: string; body: unknown }[] = []): Promise<string[]> {
  const calls: string[] = [];
  await page.route(
    (url) => isAdminPageRequest(url),
    (route) => {
      const req = route.request();
      const u = new URL(req.url());
      calls.push(`${req.method()} ${u.pathname}${u.search}`);

      if (u.pathname === "/api/admin/redeems" && req.method() === "POST") {
        posts.push({ url: u.pathname, body: req.postDataJSON() });
        // 生成响应现在直接带明文码：结果卡不再依赖"按批次查列表"的第二次请求。
        return route.fulfill({
          json: envelope({
            created: 5,
            codes: [
              "LD-AAAA-BBBB-CCCC",
              "LD-DDDD-EEEE-FFFF",
              "LD-GGGG-HHHH-IIII",
              "LD-JJJJ-KKKK-LLLL",
              "LD-MMMM-NNNN-OOOO",
            ],
          }),
        });
      }
      if (u.pathname === "/api/admin/redeems/delete") {
        posts.push({ url: u.pathname, body: req.postDataJSON() });
        return route.fulfill({ json: envelope({ deleted: 1, skipped: 1 }) });
      }
      if (u.pathname === "/api/admin/redeems") {
        return route.fulfill({
          json: envelope({ items: REDEEMS, total: REDEEMS.length, page: 1, page_size: 20 }),
        });
      }
      if (u.pathname === "/api/admin/redeems/export") {
        return route.fulfill({
          status: 200,
          headers: {
            "Content-Type": "text/csv; charset=utf-8",
            "Content-Disposition": 'attachment; filename="redeem-codes.csv"',
          },
          body: "\ufeffCode,Batch,Quota,Status\r\nLD-AAAA-BBBB-CCCC,双十一,1000000,available\r\n",
        });
      }
      if (u.pathname === "/api/admin/config") {
        return route.fulfill({ json: envelope({ config: ADMIN_CONFIG }) });
      }
      return route.fulfill({ json: envelope(null) });
    },
  );
  return calls;
}

const isoDay = (offsetDays: number) =>
  new Date(Date.now() + offsetDays * 86_400_000).toISOString().slice(0, 10);

test.describe("S16 兑换码管理 · 管理台", () => {
  test("列表渲染派生状态与使用者回填（原型无筛选条）", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);
    await page.goto("/admin/redeems");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("兑换码");
    await expect(page.locator(".pbar")).toHaveCount(0);

    // 三种派生状态标签 + 使用者回填
    await expect(page.locator(".tbl tbody tr", { hasText: "LD-AAAA-BBBB-CCCC" }).locator(".badge")).toHaveText("可用");
    const usedRow = page.locator(".tbl tbody tr", { hasText: "LD-DDDD-EEEE-FFFF" });
    await expect(usedRow.locator(".badge")).toHaveText("已用");
    await expect(usedRow).toContainText("#7");
    await expect(page.locator(".tbl tbody tr", { hasText: "LD-GGGG-HHHH-IIII" }).locator(".badge")).toHaveText("已过期");
  });

  test("生成表单：字段级校验拦截且不发请求；成功后真实渲染生成结果", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const posts: { url: string; body: unknown }[] = [];
    const calls = await stub(page, posts);
    // 单码上限来自 /api/admin/config：必须等它落地再提交，否则 cap=0 时
    // 「面额不得超过单码上限」这条校验根本不会触发（假通过）。
    // 用响应等待而不是找页面文案 —— 说明文案已经是零冗余的，不能拿来当锚点。
    const cfgLoaded = page.waitForResponse((r) => r.url().includes("/api/admin/config"));
    await page.goto("/admin/redeems");
    await cfgLoaded;

    const gen = page.getByRole("button", { name: "生成兑换码" });

    // 数量越界（1001，不裁剪）
    await page.getByLabel("面额").fill("1000000");
    await page.getByLabel("数量").fill("1001");
    await page.getByLabel("有效期至").fill(isoDay(30));
    await page.getByLabel("批次名").fill("双十一");
    await gen.click();
    await expect(page.getByText("数量取值范围为 1-1000")).toBeVisible();
    expect(posts).toHaveLength(0);

    // 面额非法：金额输入框（MoneyInput）不接受负号，"-1" 会被清空 → 走到「格式不正确」这条
    // （负数在 UI 上不可达，但「非空但解析不出金额」必须照样拦下，否则会拿空值去下单）
    await page.getByLabel("数量").fill("10");
    await page.getByLabel("面额").fill("-1");
    await gen.click();
    await expect(page.getByText(/面额格式不正确/)).toBeVisible();
    expect(posts).toHaveLength(0);

    // 面额超单码上限（来自 /api/admin/config）
    await page.getByLabel("面额").fill("60000000");
    await gen.click();
    await expect(page.getByText(/面额不得超过单码上限/)).toBeVisible();
    expect(posts).toHaveLength(0);

    // 有效期早于今日
    await page.getByLabel("面额").fill("1000000");
    await page.getByLabel("有效期至").fill(isoDay(-1));
    await gen.click();
    await expect(page.getByText("有效期不得早于今日")).toBeVisible();
    expect(posts).toHaveLength(0);

    // 批次名必填
    await page.getByLabel("有效期至").fill(isoDay(30));
    await page.getByLabel("批次名").fill("");
    await gen.click();
    await expect(page.getByText("批次名必填")).toBeVisible();
    expect(posts).toHaveLength(0);

    // 全部合法 → 只发一次 POST，且 body 与表单一致
    await page.getByLabel("批次名").fill("双十一");
    await gen.click();
    await expect(page.getByText("已生成 5 个兑换码（批次：双十一）")).toBeVisible({ timeout: 15_000 });
    expect(posts).toHaveLength(1);
    expect(posts[0].url).toBe("/api/admin/redeems");
    // 面额走「十进制金额 → 微」：输入 1000000（货币单位）= 1e12 微，载荷字段名是 amount 不是 quota
    expect(posts[0].body).toEqual({
      batch: "双十一",
      amount: 1_000_000_000_000,
      count: 10,
      expires_at: isoDay(30),
    });

    // 生成结果卡：标题取真实 created 计数，码列表直接取 POST 响应里的 codes
    await expect(page.getByText(/批次「双十一」已生成 5 个/)).toBeVisible();
    await expect(page.locator(".code-list")).toContainText("LD-AAAA-BBBB-CCCC");

    // 结果卡「导出 CSV」按批次发起导出请求
    await page.getByRole("button", { name: "导出 CSV" }).click();
    await expect
      .poll(() => calls.some((c) => c.startsWith("GET /api/admin/redeems/export?") && c.includes("batch=")))
      .toBe(true);
  });

  test("批量作废：二次确认 → POST ids + 跳过计数提示", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const posts: { url: string; body: unknown }[] = [];
    await stub(page, posts);
    await page.goto("/admin/redeems");

    // 全选本页 → 已选计数
    await page.getByRole("button", { name: "全选本页" }).click();
    await expect(page.getByText(/已选 3 条/)).toBeVisible();

    // 取消一条
    await page.getByLabel("选择兑换码 LD-GGGG-HHHH-IIII").uncheck();
    await expect(page.getByText(/已选 2 条/)).toBeVisible();

    await page.getByRole("button", { name: /^作废（2）$/ }).click();
    const confirm = page.getByRole("dialog", { name: "作废兑换码" });
    await expect(confirm).toBeVisible();
    await confirm.getByRole("button", { name: "确认作废", exact: true }).click();

    await expect(page.getByText(/已作废 1 个，跳过 1 个/)).toBeVisible({ timeout: 15_000 });
    expect(posts).toHaveLength(1);
    expect(posts[0].url).toBe("/api/admin/redeems/delete");
    expect(posts[0].body).toEqual({ ids: [11, 12] });
    // 作废后清空选中
    await expect(page.getByText(/已选/)).toHaveCount(0);
  });

  test("作废取消不产生请求；未选中时按钮禁用", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const posts: { url: string; body: unknown }[] = [];
    await stub(page, posts);
    await page.goto("/admin/redeems");

    await expect(page.getByRole("button", { name: /^作废$/ })).toBeDisabled();

    await page.getByRole("button", { name: "全选本页" }).click();
    await page.getByRole("button", { name: /^作废（3）$/ }).click();
    const confirm = page.getByRole("dialog", { name: "作废兑换码" });
    await confirm.getByRole("button", { name: "取消", exact: true }).click();
    await expect(confirm).toBeHidden();
    expect(posts).toHaveLength(0);
  });

  test("移动端 375px：无横向溢出且生成表单可操作", async ({ page }) => {
    await page.setViewportSize({ width: 375, height: 812 });
    await seedAuth(page, { role: 0 });
    await stub(page);
    await page.goto("/admin/redeems");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("兑换码");

    await expectNoHScroll(page);

    await page.getByLabel("面额").scrollIntoViewIfNeeded();
    await page.getByLabel("面额").fill("2000000");
    await expect(page.getByLabel("面额")).toHaveValue("2000000");
  });
});
