/** S20 契约级（stub）用例：/admin/auth-events（认证审计）—— 渲染、筛选下发、导出契约 */
import { expect, test, type Page } from "@playwright/test";
import { envelope, isAdminPageRequest, seedAuth } from "./helpers";

/**
 * 认证审计页：与「审计日志」分成两页的核心理由是**语义不同**
 * （那边 = 管理员做了什么，这边 = 账号层面发生了什么），
 * 所以这里额外守住「事件中文名来自后端」——前端不再维护第二份映射。
 */

const EVENTS = [
  {
    id: 5003,
    uid: 7,
    account: "alice",
    event: "auth.login_failed",
    label: "登录失败",
    result: "fail",
    reason: "invalid_credential",
    ip: "203.0.113.9",
    ua: "curl/8.0.1",
    session_id: 0,
    detail: null,
    created_at: "2026-10-26T09:00:00+08:00",
  },
  {
    id: 5002,
    uid: 7,
    account: "alice",
    event: "auth.login_mfa_success",
    label: "二次验证通过",
    result: "success",
    reason: "",
    ip: "203.0.113.9",
    ua: "Mozilla/5.0 (Windows NT 10.0; Win64; x64)",
    session_id: 42,
    detail: null,
    created_at: "2026-10-26T08:59:00+08:00",
  },
  {
    id: 5001,
    uid: 0,
    account: "ghost",
    event: "auth.login",
    label: "登录成功",
    result: "success",
    reason: "",
    ip: "198.51.100.2",
    ua: "python-requests/2.31",
    session_id: 41,
    detail: null,
    created_at: "2026-10-26T08:00:00+08:00",
  },
];

const META = {
  events: [
    { event: "auth.login", label: "登录成功" },
    { event: "auth.login_failed", label: "登录失败" },
    { event: "auth.login_mfa_success", label: "二次验证通过" },
  ],
  retention_days: 90,
};

/** 管理台必须在 /api/admin/** 全量打桩，否则真实 401 会触发全局登出 */
async function stub(page: Page): Promise<string[]> {
  const calls: string[] = [];
  await page.route(
    (url) => isAdminPageRequest(url),
    (route) => {
      const req = route.request();
      const u = new URL(req.url());
      calls.push(`${req.method()} ${u.pathname}${u.search}`);

      if (u.pathname === "/api/admin/auth-events/meta") {
        return route.fulfill({ json: envelope(META) });
      }
      if (u.pathname === "/api/admin/auth-events" && req.method() === "GET") {
        const event = u.searchParams.get("event");
        const result = u.searchParams.get("result");
        const items = EVENTS.filter(
          (e) => (!event || e.event === event) && (!result || e.result === result),
        );
        return route.fulfill({
          json: envelope({ items, total: items.length, page: 1, page_size: 20 }),
        });
      }
      if (u.pathname === "/api/admin/auth-events/export") {
        return route.fulfill({
          status: 200,
          headers: {
            "Content-Type": "text/csv; charset=utf-8",
            "Content-Disposition": 'attachment; filename="auth-events.csv"',
          },
          body: "\ufeff时间,UID,账号,事件,结果\r\n",
        });
      }
      return route.fulfill({ json: envelope(null) });
    },
  );
  return calls;
}

/** 仅桌面内联的筛选控件位于 .pbar-fields（Sheet 内的同名字段不参与） */
const fields = (page: Page) => page.locator(".pbar-fields");

test.describe("S20 认证审计 · 管理台（契约级）", () => {
  test("列表渲染：保留天数 / 事件中文名 / 结果徽标 / 账号与 IP / 会话", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);
    await page.goto("/admin/auth-events");

    await expect(page.getByRole("heading", { level: 1 })).toHaveText("认证审计");
    await expect(page.locator(".count-line")).toContainText("保留 90 天");
    await expect(page.locator(".count-line")).toContainText("共 3 条");

    // 失败行：结果徽标、原因码与 IP 都要能直接读出来（排障第一眼要的就是这几列）
    const failRow = page.locator("tr", { hasText: "登录失败" });
    await expect(failRow).toContainText("#7");
    await expect(failRow).toContainText("alice");
    await expect(failRow).toContainText("失败");
    await expect(failRow).toContainText("invalid_credential");
    await expect(failRow).toContainText("203.0.113.9");

    // uid=0（账号不存在）不显示 #0：那是撞库形态，显示 #0 只会误导成「有个 0 号用户」
    const ghostRow = page.locator("tr", { hasText: "python-requests" });
    await expect(ghostRow).toContainText("ghost");
    await expect(ghostRow).not.toContainText("#0");

    // 有会话的事件显示会话号，登不上（session_id=0）的显示占位
    const mfaRow = page.locator("tr", { hasText: "二次验证通过" });
    await expect(mfaRow).toContainText("#42");
    await expect(failRow).toContainText("—");
  });

  test("筛选：事件下拉由 meta 驱动，结果筛选与筛选后计数同步", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page);
    await page.goto("/admin/auth-events");

    // 下拉项来自 /admin/auth-events/meta（中文名由后端下发）
    await expect(fields(page).getByLabel("事件").locator("option")).toContainText([
      "全部事件",
      "登录成功",
      "登录失败",
      "二次验证通过",
    ]);

    await fields(page).getByLabel("事件").selectOption("auth.login_failed");
    await expect
      .poll(() =>
        calls.some(
          (c) => c.startsWith("GET /api/admin/auth-events?") && c.includes("event=auth.login_failed"),
        ),
      )
      .toBe(true);
    await expect(page.locator(".count-line")).toContainText("共 1 条");

    // 结果筛选叠加上去：同一个 WHERE 同时作用于列表与计数
    await fields(page).getByLabel("结果").selectOption("fail");
    await expect
      .poll(() => calls.some((c) => c.includes("result=fail")))
      .toBe(true);
    await expect(page.locator(".count-line")).toContainText("共 1 条");

    await fields(page).getByLabel("结果").selectOption("success");
    await expect(page.locator(".count-line")).toContainText("共 0 条");
    await expect(page.getByText("无匹配记录")).toBeVisible();
  });

  test("导出 CSV：携带当前筛选", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page);
    await page.goto("/admin/auth-events");

    await fields(page).getByLabel("结果").selectOption("fail");
    await expect.poll(() => calls.some((c) => c.includes("result=fail"))).toBe(true);

    await page.getByRole("button", { name: "导出 CSV" }).click();
    await expect
      .poll(() =>
        calls.some(
          (c) => c.startsWith("GET /api/admin/auth-events/export?") && c.includes("result=fail"),
        ),
      )
      .toBe(true);
  });
});
