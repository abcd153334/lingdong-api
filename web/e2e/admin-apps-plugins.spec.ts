import { expect, test, type Page } from "@playwright/test";

import { envelope, expectNoHScroll, seedAuth } from "./helpers";

/**
 * 应用插件（声明式）的端到端闭环。
 *
 * 守的是三件在 jsdom 里量不到的事：
 *  1. 泛化路由 `/admin/apps/:id/p/:key` 能直达、面包屑与侧栏高亮正确；
 *  2. 一条记录**只**下发声明的字段（多传一个键后端会 422，前端不该偷偷补默认值）；
 *  3. 卸载与清除是两个不同动作：卸载后数据仍在，清除才有二次确认。
 */

const APP = {
  id: 2,
  slug: "web-portal",
  name: "官网门户",
  display_name: "官网门户",
  description: "",
  status: 1,
  allow_self_issue: true,
  access_mode: "open",
  logo_url: "",
  maintenance_mode: false,
  maintenance_message: "",
  created_at: "2026-11-01T10:00:00Z",
  key_count: 2,
  active_key_count: 2,
  user_count: 3,
  used: 0,
  paid_cent: 0,
  paid_count: 0,
  calls_7d: 12,
  notice_count: 1,
  latest_release: null,
};

const PLUGIN = {
  key: "ticket",
  name: "工单",
  version: "1.0.0",
  api_version: 1,
  icon: "",
  icon_url: "",
  config: [{ key: "slaHours", label: "响应时限（小时）", type: "int", required: true, maxLen: 0, default: 24 }],
  limits: { maxRecords: 20000 },
  entities: [
    {
      key: "ticket",
      name: "工单",
      unique: "ticketNo",
      fields: [
        { key: "ticketNo", label: "单号", type: "string", required: true, maxLen: 32 },
        { key: "title", label: "标题", type: "string", required: true, maxLen: 80 },
        {
          key: "status",
          label: "状态",
          type: "enum",
          required: true,
          maxLen: 0,
          options: ["open", "doing", "done"],
          default: "open",
        },
        { key: "due", label: "期望完成", type: "date", required: false, maxLen: 0 },
        { key: "detail", label: "详情", type: "text", required: false, maxLen: 2000 },
      ],
      list: {
        columns: [
          { key: "ticketNo", label: "单号", type: "string" },
          { key: "title", label: "标题", type: "string" },
          { key: "status", label: "状态", type: "enum" },
          { key: "due", label: "期望完成", type: "date" },
        ],
        actions: ["create", "update", "delete", "export"],
      },
      // 对外只读只公开这两列（单号 + 标题），详情/状态不对外
      public: ["ticketNo", "title"],
    },
  ],
};

const SAMPLE = {
  apiVersion: 1,
  key: "ticket",
  name: "工单",
  version: "1.0.0",
  entities: [
    {
      key: "ticket",
      name: "工单",
      fields: [{ key: "ticketNo", label: "单号", type: "string", required: true, maxLen: 32 }],
      list: { columns: ["ticketNo"], actions: ["create"] },
    },
  ],
};

type Record_ = { record_key: string; data: Record<string, unknown>; version: number; created_at: string; updated_at: string };

type Captured = { method: string; path: string; body: unknown };

/** 可变状态：用例之间不复位会互相污染（同一个 worker 复用 page 时尤其明显）。 */
function fresh() {
  return {
    installed: false,
    status: 1,
    publicEnabled: false,
    records: [] as Record_[],
    captured: [] as Captured[],
    purged: 0,
  };
}

async function stub(page: Page, st: ReturnType<typeof fresh>) {
  await seedAuth(page, { role: 0 });
  const ts = "2026-11-18 10:00:00";
  const install = () => ({
    plugin_key: "ticket",
    name: "工单",
    version: "1.0.0",
    plugin_version: "1.0.0",
    icon: "",
    status: st.status,
    public_enabled: st.publicEnabled,
    config: { slaHours: 12 },
    lock_version: 3,
    record_count: st.records.length,
    updated_at: ts,
  });
  const detail = () => ({
    install: install(),
    app: { id: APP.id, slug: APP.slug, name: APP.name },
    plugin: PLUGIN,
    drift: null,
    record_count: st.records.length,
    catalog_disabled: false,
  });

  await page.route("**/api/admin/**", async (route) => {
    const req = route.request();
    const url = new URL(req.url());
    const p = url.pathname;
    const m = req.method();
    if (m !== "GET") {
      st.captured.push({ method: m, path: p, body: req.postDataJSON() });
    }

    if (p === "/api/admin/apps" && m === "GET") {
      return route.fulfill({ json: envelope({ items: [APP], total: 1, page: 1, page_size: 20 }) });
    }
    if (p === "/api/admin/apps/2/plugins" && m === "GET") {
      return route.fulfill({ json: envelope({ items: st.installed ? [install()] : [] }) });
    }
    if (p === "/api/admin/apps/2/plugins" && m === "POST") {
      st.installed = true;
      st.status = 1;
      return route.fulfill({ json: envelope({ plugin_key: "ticket", status: 1, lock_version: 1 }) });
    }
    if (p === "/api/admin/apps/2/plugins/ticket" && m === "GET") {
      if (!st.installed) return route.fulfill({ status: 404, json: { code: 404, message: "未安装", error_code: "plugin_not_installed", data: null } });
      return route.fulfill({ json: envelope(detail()) });
    }
    if (p === "/api/admin/apps/2/plugins/ticket" && m === "PATCH") {
      const body = req.postDataJSON() as { status?: number; public_enabled?: boolean };
      if (body.status) st.status = body.status;
      if (typeof body.public_enabled === "boolean") st.publicEnabled = body.public_enabled;
      return route.fulfill({
        json: envelope({
          plugin_key: "ticket",
          status: st.status,
          public_enabled: st.publicEnabled,
          lock_version: 4,
        }),
      });
    }
    if (p === "/api/admin/apps/2/plugins/ticket" && m === "DELETE") {
      st.status = 3;
      return route.fulfill({ json: envelope({ plugin_key: "ticket", status: 3 }) });
    }
    if (p === "/api/admin/apps/2/plugins/ticket/data" && m === "DELETE") {
      const n = st.records.length;
      st.records = [];
      st.purged += 1;
      return route.fulfill({ json: envelope({ deleted: n }) });
    }
    if (p === "/api/admin/apps/2/plugins/ticket/records" && m === "GET") {
      if (st.status === 3) {
        return route.fulfill({ status: 404, json: { code: 404, message: "未安装", error_code: "plugin_not_installed", data: null } });
      }
      return route.fulfill({
        json: envelope({ items: st.records, total: st.records.length, page: 1, page_size: 20 }),
      });
    }
    if (p === "/api/admin/apps/2/plugins/ticket/records" && m === "POST") {
      const body = req.postDataJSON() as { entity: string; data: Record<string, unknown> };
      if (body.data.ticketNo === "DUP") {
        return route.fulfill({
          status: 409,
          json: {
            code: 409,
            message: "该值已被占用",
            error_code: "plugin_record_conflict",
            data: { problems: [{ path: "data.ticketNo", message: "该值已被占用" }], breaking: false },
          },
        });
      }
      const rec = {
        record_key: "01K0000000000000000000TEST",
        data: body.data,
        version: 1,
        created_at: ts,
        updated_at: ts,
      };
      st.records = [...st.records, rec];
      return route.fulfill({ json: envelope(rec) });
    }
    if (p === "/api/admin/app-plugins" && m === "GET") {
      return route.fulfill({
        json: envelope({
          items: [
            {
              key: "ticket",
              name: "工单",
              version: "1.0.0",
              api_version: 1,
              status: 1,
              source: "import",
              icon: "",
              icon_url: "",
              installs: st.installed ? 1 : 0,
              lock_version: 1,
              updated_at: ts,
            },
          ],
          total: 1,
          page: 1,
          page_size: 50,
        }),
      });
    }
    if (p === "/api/admin/app-plugins/sample" && m === "GET") {
      return route.fulfill({ json: envelope({ manifest: SAMPLE }) });
    }
    if (p === "/api/admin/app-plugins/ticket" && m === "GET") {
      return route.fulfill({
        json: envelope({
          plugin: PLUGIN,
          manifest: SAMPLE,
          status: 1,
          source: "import",
          lock_version: 1,
          installs: st.installed ? 1 : 0,
          updated_at: ts,
        }),
      });
    }
    if (p === "/api/admin/app-plugins/ticket" && m === "PUT") {
      const dry = url.searchParams.get("dry_run") === "1";
      return route.fulfill({
        json: envelope({
          key: "ticket",
          name: "工单",
          version: "1.0.0",
          lock_version: 1,
          created: true,
          applied: !dry,
          problems: [],
          breaking: [],
          affected: [],
        }),
      });
    }
    return route.fulfill({ json: envelope(null) });
  });
}

async function openManage(page: Page) {
  await page.goto("/admin/apps");
  await page
    .locator('[data-testid="app-card"]', { hasText: "web-portal" })
    .getByRole("button", { name: "管理" })
    .click();
  return page.getByRole("dialog", { name: /应用管理/ });
}

test.describe("应用插件：安装、导入与数据隔离", () => {
  test("可安装 → 安装 → 已安装清单出现，点「打开」进泛化路由", async ({ page }) => {
    const st = fresh();
    await stub(page, st);
    const dlg = await openManage(page);
    await dlg.getByRole("tab", { name: "插件" }).click();

    await expect(dlg.getByText("工单")).toBeVisible();
    await dlg.getByRole("button", { name: "安装", exact: true }).click();

    // 插件声明了必填配置，所以安装前先弹配置框：必填项不填，后端会 422。
    const cfg = page.getByRole("dialog", { name: /安装「工单」/ });
    await cfg.getByLabel("响应时限（小时）").fill("12");
    await cfg.getByRole("button", { name: "安装", exact: true }).click();

    // 安装成功后会刷新清单：出现「启用中」与「打开」。
    await expect(dlg.getByText("启用中")).toBeVisible();
    await dlg.getByRole("button", { name: "打开" }).click();

    // 卸载/启用这些动作只在插件页里有意义，跳转必须落到泛化路由。
    await expect(page).toHaveURL(/\/admin\/apps\/2\/p\/ticket$/);
    await expect(page.getByRole("heading", { level: 1 })).toContainText("工单");
  });

  test("导入：填入示例 → 校验（不落库）→ 导入", async ({ page }) => {
    const st = fresh();
    await stub(page, st);
    /**
     * 这个键在目录里还不存在，所以详情接口必然是 404（真实后端也是这样）。
     * 旧实现拿这条 GET 去"探"存在性 ⇒ 控制台一条红字 404，且拿不到版本。
     * 现在版本来自列表的 `q` 精确命中，这条 404 不该再被请求；
     * 显式 404 掉它之后仍要求 `expected_version === 1`，正好把"探测"钉死。
     */
    await page.route("**/api/admin/app-plugins/ticket", (route) =>
      route.request().method() === "GET"
        ? route.fulfill({
            status: 404,
            json: { code: 404, message: "插件不存在", error_code: "not_found", data: null },
          })
        : route.fallback(),
    );
    const dlg = await openManage(page);
    await dlg.getByRole("tab", { name: "插件" }).click();
    await dlg.getByRole("button", { name: "导入插件" }).click();

    const imp = page.getByRole("dialog", { name: "导入插件" });
    await imp.getByRole("button", { name: "填入示例" }).click();
    await expect(imp.locator("#plugin-import-text")).toHaveValue(/"apiVersion": 1/);

    await imp.getByRole("button", { name: "校验" }).click();
    await expect(imp.getByText("没有发现问题，可以导入")).toBeVisible();

    const reqPromise = page.waitForRequest((r) => r.url().includes("/app-plugins/ticket") && r.method() === "PUT");
    await imp.getByRole("button", { name: "导入", exact: true }).click();
    const body = reqPromise.then((r) => r.postDataJSON() as { expected_version: number; manifest: unknown });
    const sent = await body;
    // 升级必须带当前版本号：不带就是"无条件覆盖"，AI 的旧稿会抹掉运营的微调。
    expect(sent.expected_version).toBe(1);
    expect((sent.manifest as { key: string }).key).toBe("ticket");
  });

  test("新建记录：body 只含声明字段；唯一值冲突落到字段下方", async ({ page }) => {
    const st = fresh();
    st.installed = true;
    await stub(page, st);
    await page.goto("/admin/apps/2/p/ticket");
    await expect(page.getByRole("heading", { level: 1 })).toContainText("工单");

    await page.getByRole("button", { name: "新建" }).click();
    const dlg = page.getByRole("dialog", { name: /新建工单/ });
    await dlg.getByLabel("单号").fill("T-1");
    await dlg.getByLabel("标题").fill("打不开");
    await dlg.getByLabel("状态").selectOption("open");

    const reqPromise = page.waitForRequest((r) => r.url().endsWith("/records") && r.method() === "POST");
    await dlg.getByRole("button", { name: "保存" }).click();
    const body = (await reqPromise) as unknown as { postDataJSON: () => { entity: string; data: Record<string, unknown> } };
    const sent = body.postDataJSON();
    expect(sent.entity).toBe("ticket");
    // 只下发填过的字段：空串一律不传（服务端把空串与不传视为同一件事）。
    expect(Object.keys(sent.data).sort()).toEqual(["status", "ticketNo", "title"]);
    await expect(page.getByText("T-1")).toBeVisible();

    // 唯一值冲突：错误必须贴在「单号」下面，而不是只弹一个 toast。
    await page.getByRole("button", { name: "新建" }).click();
    const dlg2 = page.getByRole("dialog", { name: /新建工单/ });
    await dlg2.getByLabel("单号").fill("DUP");
    await dlg2.getByLabel("标题").fill("重复");
    await dlg2.getByLabel("状态").selectOption("open");
    await dlg2.getByRole("button", { name: "保存" }).click();
    await expect(dlg2.getByText("该值已被占用")).toBeVisible();
  });

  test("配置对话框回填已保存的值（字段描述是异步到的）", async ({ page }) => {
    const st = fresh();
    st.installed = true;
    await stub(page, st);
    const dlg = await openManage(page);
    await dlg.getByRole("tab", { name: "插件" }).click();

    await dlg.getByRole("button", { name: "配置" }).click();
    const cfg = page.getByRole("dialog", { name: /配置「工单」/ });
    // 服务端存的是 12。若输入框空着，管理员会以为没配过而重填 ——
    // 根因是「字段描述异步到」+「useState 初始化只跑一次」。
    await expect(cfg.getByLabel("响应时限（小时）")).toHaveValue("12");

    await cfg.getByLabel("响应时限（小时）").fill("24");
    const reqPromise = page.waitForRequest((r) => r.url().endsWith("/plugins/ticket") && r.method() === "PATCH");
    await cfg.getByRole("button", { name: "保存" }).click();
    const body = (await reqPromise).postDataJSON() as { config: Record<string, unknown>; expected_version: number };
    expect(body.config).toEqual({ slaHours: 24 });
  });

  test("搜索框文案跟着实体声明走", async ({ page }) => {
    const st = fresh();
    st.installed = true;
    await stub(page, st);
    await page.goto("/admin/apps/2/p/ticket");
    // 后端搜的是「编号 + 业务唯一值」两列；写死「按编号搜索」会让人以为搜不了单号。
    await expect(page.getByLabel("按单号或编号搜索")).toBeVisible();
  });

  test("对外只读：打开开关给出免凭证地址，且只公开白名单字段", async ({ page }) => {
    const st = fresh();
    st.installed = true;
    await stub(page, st);
    await page.goto("/admin/apps/2/p/ticket");

    const sw = page.getByRole("checkbox", { name: "允许终端免凭证读取本插件数据" });
    await expect(sw).not.toBeChecked();
    // 关闭态不显示地址（显示的是"开启后会怎样"）
    await expect(page.locator(".plugin-public__url")).toHaveCount(0);

    await sw.click();
    await expect(page.locator(".plugin-public__url")).toContainText("/api/apps/web-portal/p/ticket/ticket");
    // 开关走的是安装级字段，与 status 同一个 PATCH 但语义独立
    const last = st.captured.at(-1)!;
    expect((last.body as { public_enabled?: boolean }).public_enabled).toBe(true);
    expect((last.body as { status?: number }).status).toBeUndefined();
  });

  test("卸载后数据仍在；清除数据要二次确认", async ({ page }) => {
    const st = fresh();
    st.installed = true;
    st.records = [
      { record_key: "01K0000000000000000000AAA", data: { ticketNo: "T-9", title: "旧单", status: "open" }, version: 1, created_at: "", updated_at: "" },
    ];
    await stub(page, st);
    const dlg = await openManage(page);
    await dlg.getByRole("tab", { name: "插件" }).click();
    await expect(dlg.getByText("启用中")).toBeVisible();

    await dlg.getByRole("button", { name: "卸载" }).click();
    await page.getByRole("button", { name: "卸载", exact: true }).last().click();
    await expect(dlg.getByText("已卸载")).toBeVisible();

    // 清除数据必须先过二次确认；确认前的文案要说清"永久删除"。
    await dlg.getByRole("button", { name: "清除数据" }).click();
    const confirm = page.getByRole("dialog").filter({ hasText: "永久删除" });
    await expect(confirm).toBeVisible();
    await page.getByRole("button", { name: "永久删除" }).click();
    await expect.poll(() => st.purged).toBe(1);
  });

  test("375px：第六个页签在视口内，插件面板不横向溢出", async ({ page }) => {
    const st = fresh();
    st.installed = true;
    await stub(page, st);
    await page.setViewportSize({ width: 375, height: 780 });
    const dlg = await openManage(page);

    const tab = dlg.getByRole("tab", { name: "插件" });
    await tab.click();
    await expect(tab).toBeInViewport();
    const overflow = await dlg
      .locator(".page-tabs > .tab-panel:not([hidden])")
      .evaluate((el) => el.scrollWidth - el.clientWidth);
    expect(overflow).toBeLessThanOrEqual(1);
    await expectNoHScroll(page);
  });
});
