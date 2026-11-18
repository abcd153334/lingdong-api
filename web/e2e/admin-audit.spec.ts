/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, isAdminPageRequest, seedAuth } from "./helpers";

/** S20 去假覆盖：/admin/audit（契约级 stub —— 全部 /api/admin/** 打桩，断言真实区块/行/抽屉/请求契约） */

const AUDITS = [
  {
    id: 9001,
    action: "user.update",
    admin_id: 1,
    actor: "admin",
    target_type: "user",
    target_id: "7",
    detail: { before: { role: 1 }, after: { role: 0 } },
    created_at: "2026-10-20T10:30:00+08:00",
  },
  {
    id: 9002,
    action: "redeem.generate",
    admin_id: 1,
    actor: "admin",
    target_type: "redeem",
    target_id: "100 条",
    detail: null,
    created_at: "2026-10-20T11:00:00+08:00",
  },
];

const META = {
  actions: [
    { action: "user.update", value: 5 },
    { action: "redeem.generate", value: 12 },
  ],
  retention_days: 365,
};

/** 记录已发生的请求；管理台必须在 /api/admin/** 打桩（否则真实 401 触发全局登出） */
async function stub(page: Page): Promise<string[]> {
  const calls: string[] = [];
  await page.route(
    (url) => isAdminPageRequest(url),
    (route) => {
      const req = route.request();
      const u = new URL(req.url());
      calls.push(`${req.method()} ${u.pathname}${u.search}`);

      if (u.pathname === "/api/admin/audits" && req.method() === "GET") {
        return route.fulfill({
          json: envelope({ items: AUDITS, total: AUDITS.length, page: 1, page_size: 20 }),
        });
      }
      if (u.pathname === "/api/admin/audits/meta") {
        return route.fulfill({ json: envelope(META) });
      }
      if (u.pathname === "/api/admin/audits/export") {
        return route.fulfill({
          status: 200,
          headers: {
            "Content-Type": "text/csv; charset=utf-8",
            "Content-Disposition": 'attachment; filename="audit.csv"',
          },
          body: "\ufeffTime,Actor,Action,TargetType,TargetID\r\n",
        });
      }
      return route.fulfill({ json: envelope(null) });
    },
  );
  return calls;
}

/** 仅桌面内联的筛选控件位于 .pbar-fields（Sheet 内的同名字段不参与） */
const fields = (page: Page) => page.locator(".pbar-fields");

test.describe("S20 审计日志 · 管理台（契约级）", () => {
  test("列表渲染：保留天数 / 动作徽标 / 目标 / 总数", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);
    await page.goto("/admin/audit");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("审计日志");
    await expect(page.locator(".ph-desc")).toHaveText("管理员操作留痕");
    await expect(page.locator(".count-line")).toContainText("保留 365 天");
    await expect(page.locator(".count-line")).toContainText("共 2 条");
    await expect(page.locator(".pager")).toContainText("共 2 条");

    const row = page.locator(".au-row", { hasText: "user.update" });
    await expect(row).toContainText("admin");
    await expect(row).toContainText("user #7");

    const genRow = page.locator(".au-row", { hasText: "redeem.generate" });
    await expect(genRow).toContainText("redeem #100 条");
  });

  test("动作筛选由 meta 数据驱动并下发查询参数", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page);
    await page.goto("/admin/audit");

    // 动作下拉项来自 /admin/audits/meta（含计数）
    await fields(page).getByLabel("动作").selectOption("user.update");
    await expect
      .poll(() => calls.some((c) => c.startsWith("GET /api/admin/audits?") && c.includes("action=user.update")))
      .toBe(true);
  });

  test("详情手风琴：变更内容 detail JSON 渲染与空 detail 占位", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);
    await page.goto("/admin/audit");

    const row = page.locator(".au-row", { hasText: "user.update" });
    await expect(row.locator(".json-view")).toBeHidden();
    await row.locator("summary").click();
    await expect(row.locator(".json-view")).toBeVisible();
    await expect(row.locator(".json-view")).toContainText('"role": 0');

    const genRow = page.locator(".au-row", { hasText: "redeem.generate" });
    await genRow.locator("summary").click();
    await expect(genRow.locator(".json-view")).toContainText("（无变更内容）");
  });

  test("导出 CSV：请求契约断言（携带当前筛选）", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page);
    await page.goto("/admin/audit");

    await fields(page).getByLabel("动作").selectOption("user.update");
    await expect
      .poll(() => calls.some((c) => c.includes("action=user.update")))
      .toBe(true);

    await page.getByRole("button", { name: "导出 CSV" }).click();
    await expect
      .poll(() => calls.some((c) => c.startsWith("GET /api/admin/audits/export?") && c.includes("action=user.update")))
      .toBe(true);
  });
});
