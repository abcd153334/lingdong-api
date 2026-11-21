import { expect, test, type Page } from "@playwright/test";

import { envelope, expectNoHScroll, seedAuth } from "./helpers";

/**
 * 应用插件（声明式）的端到端闭环。
 *
 * 守的是四件在 jsdom 里量不到的事：
 *  1. 已安装的插件在**应用管理弹窗顶部**各占一个页签，「打开」只是切页签、不跳路由；
 *  2. 两个插件面板同时保活时 DOM id 仍唯一（`idPrefix` 按宿主插件区分）；
 *  3. 一条记录**只**下发声明的字段（多传一个键后端会 422，前端不该偷偷补默认值）；
 *  4. 卸载与清除是两个不同动作：卸载后数据仍在、页签消失，清除才有二次确认。
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
        { key: "ticketNo", label: "单号", type: "string", required: true, maxLen: 32, placeholder: "T-0001" },
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
        { key: "detail", label: "详情", type: "text", required: false, maxLen: 2000, help: "只给内部看" },
      ],
      // 分组表单：后端投影后每段都是"字段恰好一次"（前端不自己排版）
      form: {
        sections: [
          { title: "基本信息", columns: 2, fields: ["ticketNo", "title"] },
          { title: "处理", columns: 1, fields: ["status", "due", "detail"] },
        ],
      },
      list: {
        columns: [
          { key: "ticketNo", label: "单号", type: "string" },
          { key: "title", label: "标题", type: "string" },
          { key: "status", label: "状态", type: "enum" },
          { key: "due", label: "期望完成", type: "date" },
        ],
        actions: ["create", "update", "delete", "export", "batch_delete"],
        // 可排序列 + 可筛选列：前端只认这份白名单（表头箭头的唯一依据）。
        sort: [{ key: "due", label: "期望完成", type: "date", order: "asc" }],
        filters: [{ key: "status", label: "状态", type: "enum", op: "eq" }],
      },
      // 对外只读只公开这两列（单号 + 标题），详情/状态不对外
      public: ["ticketNo", "title"],
    },
  ],
};

/** 第二个插件：只用来验证"两个面板同时保活"时 id 不撞。 */
const PLUGIN_ASSET = {
  key: "asset",
  name: "资产",
  version: "1.0.0",
  api_version: 1,
  icon: "",
  icon_url: "",
  config: [],
  limits: { maxRecords: 20000 },
  entities: [
    {
      key: "asset",
      name: "资产",
      unique: "no",
      fields: [{ key: "no", label: "编号", type: "string", required: true, maxLen: 16 }],
      list: {
        columns: [{ key: "no", label: "编号", type: "string" }],
        actions: ["create"],
        sort: [],
        filters: [],
      },
      form: { sections: [{ title: "", columns: 1, fields: ["no"] }] },
      public: [],
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
    /** 再装一个"资产"插件：用来把两个插件面板同时挂起来 */
    twoPlugins: false,
    status: 1,
    publicEnabled: false,
    /** 用"旧后端的描述"应答：没有 sort/filters/form，也没有 config_missing */
    legacy: false,
    records: [] as Record_[],
    captured: [] as Captured[],
    purged: 0,
  };
}

async function stub(page: Page, st: ReturnType<typeof fresh>) {
  await seedAuth(page, { role: 0 });
  const ts = "2026-11-18 10:00:00";
  const installOf = (key: string, name: string) => ({
    plugin_key: key,
    name,
    version: "1.0.0",
    plugin_version: "1.0.0",
    icon: "",
    status: st.status,
    public_enabled: st.publicEnabled,
    config: { slaHours: 12 },
    lock_version: 3,
    record_count: key === "ticket" ? st.records.length : 0,
    updated_at: ts,
  });
  /**
   * 旧后端（本次升级之前）的描述：`list` 里没有 sort/filters、实体没有 form、
   * 详情没有 config_missing。前端必须退化成"能力更少"而不是崩
   * —— 曾经直接读 `data.config_missing.length`，对着一台没重启的后端整页白屏。
   */
  const legacyDescriptor = <T,>(plugin: T): T => {
    const p = JSON.parse(JSON.stringify(plugin)) as Record<string, unknown>;
    for (const e of (p.entities ?? []) as Record<string, unknown>[]) {
      const list = (e.list ?? {}) as Record<string, unknown>;
      delete list.sort;
      delete list.filters;
      e.list = list;
      delete e.form;
    }
    return p as T;
  };

  const detailOf = (plugin: typeof PLUGIN | typeof PLUGIN_ASSET) => {
    const body: Record<string, unknown> = {
      install: installOf(plugin.key, plugin.name),
      app: { id: APP.id, slug: APP.slug, name: APP.name },
      plugin,
      drift: null,
      config_missing: [] as string[],
      record_count: plugin.key === "ticket" ? st.records.length : 0,
      catalog_disabled: false,
    };
    if (!st.legacy) return body;
    delete body.config_missing;
    body.plugin = legacyDescriptor(plugin);
    return body;
  };
  const installs = () => {
    if (!st.installed) return [];
    return st.twoPlugins ? [installOf("ticket", "工单"), installOf("asset", "资产")] : [installOf("ticket", "工单")];
  };

  await page.route("**/api/admin/**", async (route) => {
    const req = route.request();
    const url = new URL(req.url());
    const p = url.pathname;
    const m = req.method();
    if (m !== "GET") {
      st.captured.push({ method: m, path: p, body: req.postDataJSON() });
    }

    if (p === "/api/admin/app-plugins/dev-doc.md" && m === "GET") {
      // 真实后端：text/markdown + attachment + RFC 5987 中文文件名
      return route.fulfill({
        status: 200,
        headers: {
          "Content-Type": "text/markdown; charset=utf-8",
          "Content-Disposition":
            "attachment; filename=\"plugin-dev-manual.md\"; filename*=UTF-8''%E5%BA%94%E7%94%A8%E6%8F%92%E4%BB%B6%E5%BC%80%E5%8F%91%E6%89%8B%E5%86%8C.md",
        },
        body: "# 应用插件开发手册\n",
      });
    }
    if (p === "/api/admin/apps" && m === "GET") {
      return route.fulfill({ json: envelope({ items: [APP], total: 1, page: 1, page_size: 20 }) });
    }
    if (p === "/api/admin/apps/2/plugins" && m === "GET") {
      return route.fulfill({ json: envelope({ items: installs() }) });
    }
    if (p === "/api/admin/apps/2/plugins" && m === "POST") {
      st.installed = true;
      st.status = 1;
      return route.fulfill({ json: envelope({ plugin_key: "ticket", status: 1, lock_version: 1 }) });
    }
    if (p === "/api/admin/apps/2/plugins/ticket" && m === "GET") {
      if (!st.installed) return route.fulfill({ status: 404, json: { code: 404, message: "未安装", error_code: "plugin_not_installed", data: null } });
      return route.fulfill({ json: envelope(detailOf(PLUGIN)) });
    }
    if (p === "/api/admin/apps/2/plugins/asset" && m === "GET") {
      if (!st.twoPlugins) return route.fulfill({ status: 404, json: { code: 404, message: "未安装", error_code: "plugin_not_installed", data: null } });
      return route.fulfill({ json: envelope(detailOf(PLUGIN_ASSET)) });
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
    if (p === "/api/admin/apps/2/plugins/asset/records" && m === "GET") {
      return route.fulfill({ json: envelope({ items: [], total: 0, page: 1, page_size: 20 }) });
    }
    if (p === "/api/admin/apps/2/plugins/ticket/records/batch-delete" && m === "POST") {
      const body = req.postDataJSON() as { entity: string; record_keys: string[] };
      const before = st.records.length;
      st.records = st.records.filter((r) => !body.record_keys.includes(r.record_key));
      return route.fulfill({ json: envelope({ deleted: before - st.records.length }) });
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

/**
 * 打开某个插件的页签：管理 → 插件 → 打开。
 * 旧实现是一条独立路由（`page.goto("/admin/apps/2/p/ticket")`），现在必须走这条真实路径 ——
 * 直接 goto 会让"页签没接上"的回归永远测不出来。
 */
async function openPluginTab(page: Page, name = "工单") {
  const dlg = await openManage(page);
  await dlg.getByRole("tab", { name: "插件", exact: true }).click();
  await dlg.getByRole("button", { name: "打开" }).first().click();
  await expect(dlg.getByRole("tab", { name })).toHaveAttribute("aria-selected", "true");
  return dlg;
}

test.describe("应用插件：页签、安装、导入与数据隔离", () => {
  test("安装后出现插件页签；「打开」只切页签，不跳路由", async ({ page }) => {
    const st = fresh();
    await stub(page, st);
    const dlg = await openManage(page);
    await dlg.getByRole("tab", { name: "插件", exact: true }).click();

    // 未安装：只有五个静态页签（概览/品牌/公告/版本 + 插件）
    await expect(dlg.getByRole("tab")).toHaveCount(5);
    await expect(dlg.getByText("工单")).toBeVisible();

    await dlg.getByRole("button", { name: "安装", exact: true }).click();
    // 插件声明了必填配置，所以安装前先弹配置框：必填项不填，后端会 422。
    const cfg = page.getByRole("dialog", { name: /安装「工单」/ });
    await cfg.getByLabel("响应时限（小时）").fill("12");
    await cfg.getByRole("button", { name: "安装", exact: true }).click();
    await expect(dlg.getByText("启用中")).toBeVisible();

    await dlg.getByRole("button", { name: "打开" }).click();

    // 关键：停在弹窗里、页签被选中、面板有数据区；URL 不变（旧路由已删除）。
    await expect(dlg.getByRole("tab", { name: "工单" })).toHaveAttribute("aria-selected", "true");
    await expect(page).toHaveURL(/\/admin\/apps$/);
    await expect(dlg.getByLabel("按单号或编号搜索")).toBeVisible();
    // 弹窗里不允许出现第二个 h1（页面级 h1 由 Apps 提供）
    await expect(page.getByRole("heading", { level: 1 })).toHaveCount(1);
  });

  test("开发文档：一键下载 markdown，文件名取自 Content-Disposition", async ({ page }) => {
    const st = fresh();
    await stub(page, st);
    const dlg = await openManage(page);
    await dlg.getByRole("tab", { name: "插件", exact: true }).click();

    const [download] = await Promise.all([
      page.waitForEvent("download"),
      dlg.getByRole("button", { name: "开发文档" }).click(),
    ]);
    // 中文名走 filename*（filename= 只放 ASCII 兜底），前端解码后应得到中文名。
    expect(download.suggestedFilename()).toBe("应用插件开发手册.md");
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
    await dlg.getByRole("tab", { name: "插件", exact: true }).click();
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
    const dlg = await openPluginTab(page);

    await dlg.getByRole("button", { name: "新建" }).click();
    // 记录表单是**弹窗里的弹窗**：必须 portal 到 body，否则会被外层弹窗定位并裁掉。
    const form = page.getByRole("dialog", { name: /新建工单/ });
    await expect(form).toBeVisible();
    expect(await form.evaluate((el) => el.parentElement === document.body)).toBe(true);
    await form.getByLabel("单号").fill("T-1");
    await form.getByLabel("标题").fill("打不开");
    await form.getByLabel("状态").selectOption("open");

    const reqPromise = page.waitForRequest((r) => r.url().endsWith("/records") && r.method() === "POST");
    await form.getByRole("button", { name: "保存", exact: true }).click();
    const body = (await reqPromise) as unknown as { postDataJSON: () => { entity: string; data: Record<string, unknown> } };
    const sent = body.postDataJSON();
    expect(sent.entity).toBe("ticket");
    // 只下发填过的字段：空串一律不传（服务端把空串与不传视为同一件事）。
    expect(Object.keys(sent.data).sort()).toEqual(["status", "ticketNo", "title"]);
    await expect(dlg.getByText("T-1")).toBeVisible();

    // 唯一值冲突：错误必须贴在「单号」下面，而不是只弹一个 toast。
    await dlg.getByRole("button", { name: "新建" }).click();
    const form2 = page.getByRole("dialog", { name: /新建工单/ });
    await form2.getByLabel("单号").fill("DUP");
    await form2.getByLabel("标题").fill("重复");
    await form2.getByLabel("状态").selectOption("open");
    await form2.getByRole("button", { name: "保存", exact: true }).click();
    await expect(form2.getByText("该值已被占用")).toBeVisible();
  });

  test("分组表单：段头/两列/字段说明；「保存并继续」保持打开并清空", async ({ page }) => {
    const st = fresh();
    st.installed = true;
    await stub(page, st);
    const dlg = await openPluginTab(page);

    await dlg.getByRole("button", { name: "新建" }).click();
    const form = page.getByRole("dialog", { name: /新建工单/ });

    // 段头是后端投影下发的标题：前端不造字，也不自己算"哪些字段没排进去"。
    await expect(form.getByRole("heading", { name: "基本信息" })).toBeVisible();
    await expect(form.getByRole("heading", { name: "处理" })).toBeVisible();

    // 说明与占位都来自字段描述（不是写死在前端的）
    await expect(form.getByText("只给内部看")).toBeVisible();
    await expect(form.getByLabel("单号")).toHaveAttribute("placeholder", "T-0001");

    // 先 fill 再量坐标：fill 会等到元素几何**稳定**（弹窗有入场动画），
    // 直接量会拿到动画中间帧的 y，断言会以 4px 的误差随机失败。
    await form.getByLabel("单号").fill("T-1");
    await form.getByLabel("标题").fill("甲");

    // 两列段：两个控件同一行、右侧那个确实在右边（列宽由容器决定，控件保持 100%）。
    const no = await form.getByLabel("单号").boundingBox();
    const title = await form.getByLabel("标题").boundingBox();
    expect(Math.abs((no?.y ?? 0) - (title?.y ?? 0))).toBeLessThanOrEqual(2);
    expect(title?.x ?? 0).toBeGreaterThan(no?.x ?? 0);
    await form.getByLabel("状态").selectOption("open");
    const posted = page.waitForRequest((r) => r.url().endsWith("/records") && r.method() === "POST");
    await form.getByRole("button", { name: "保存并继续" }).click();
    await posted;

    // 不关窗，但表单回到初始值（宿主换 key 重挂，不自己 reset）
    await expect(form).toBeVisible();
    await expect(form.getByLabel("单号")).toHaveValue("");
    await expect(form.getByLabel("标题")).toHaveValue("");
  });

  test("配置对话框回填已保存的值（字段描述是异步到的）", async ({ page }) => {
    const st = fresh();
    st.installed = true;
    await stub(page, st);
    const dlg = await openManage(page);
    await dlg.getByRole("tab", { name: "插件", exact: true }).click();

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
    const dlg = await openPluginTab(page);
    // 后端搜的是「编号 + 业务唯一值」两列；写死「按编号搜索」会让人以为搜不了单号。
    await expect(dlg.getByLabel("按单号或编号搜索")).toBeVisible();
  });

  test("两个插件页签同时保活时，DOM id 仍唯一", async ({ page }) => {
    const st = fresh();
    st.installed = true;
    st.twoPlugins = true;
    await stub(page, st);
    const dlg = await openManage(page);
    await dlg.getByRole("tab", { name: "插件", exact: true }).click();

    // 依次打开两个插件：第二个打开后第一个面板仍保活（keepMounted）
    await expect(dlg.getByRole("button", { name: "打开" })).toHaveCount(2);
    await dlg.getByRole("button", { name: "打开" }).nth(0).click();
    await expect(dlg.getByRole("tab", { name: "工单" })).toHaveAttribute("aria-selected", "true");
    await dlg.getByRole("tab", { name: "插件", exact: true }).click();
    await dlg.getByRole("button", { name: "打开" }).nth(1).click();
    await expect(dlg.getByRole("tab", { name: "资产" })).toHaveAttribute("aria-selected", "true");

    // 顺序不变量：**「插件」清单恒在最右**，已装插件的数据页签排在它左边。
    // （反过来的排法会让管理入口随安装数量被挤到中间，每装一个插件都要重新找。）
    // 用 aria-label 取名字：.tab__txt 被截断、.tab-count 会污染 textContent。
    expect(
      await dlg.getByRole("tab").evaluateAll((els) => els.map((e) => e.getAttribute("aria-label"))),
    ).toEqual(["概览", "品牌", "公告", "版本", "工单", "资产", "插件"]);
    // 光有顺序不够：装了插件之后「插件」仍必须顶在页签条右缘（不是紧跟「资产」）。
    {
      const tl = (await dlg.getByRole("tablist", { name: "应用管理" }).boundingBox())!;
      const pin = (await dlg.getByRole("tab", { name: "插件", exact: true }).boundingBox())!;
      expect(tl.x + tl.width - (pin.x + pin.width), "装了插件后「插件」不再靠最右").toBeLessThanOrEqual(2);
    }

    // 两个面板都在 DOM 里（一个 hidden），id 不允许重复：字段 id 带宿主插件 key。
    const dupes = await page.evaluate(() => {
      const seen = new Set<string>();
      const dup = new Set<string>();
      document.querySelectorAll("[id]").forEach((el) => {
        const id = el.id;
        if (!id) return;
        if (seen.has(id)) dup.add(id);
        seen.add(id);
      });
      return [...dup];
    });
    expect(dupes, `重复 id：${dupes.join(", ")}`).toEqual([]);
    await expect(page.getByRole("heading", { level: 1 })).toHaveCount(1);
  });

  test("对外只读：打开开关给出免凭证地址，且只公开白名单字段", async ({ page }) => {
    const st = fresh();
    st.installed = true;
    await stub(page, st);
    const dlg = await openPluginTab(page);

    const sw = dlg.getByRole("checkbox", { name: "允许终端免凭证读取本插件数据" });
    await expect(sw).not.toBeChecked();
    // 关闭态不显示地址（显示的是"开启后会怎样"）
    await expect(dlg.locator(".plugin-public__url")).toHaveCount(0);

    await sw.click();
    await expect(dlg.locator(".plugin-public__url")).toContainText("/api/apps/web-portal/p/ticket/ticket");
    // 开关走的是安装级字段，与 status 同一个 PATCH 但语义独立
    const last = st.captured.at(-1)!;
    expect((last.body as { public_enabled?: boolean }).public_enabled).toBe(true);
    expect((last.body as { status?: number }).status).toBeUndefined();
  });

  test("卸载后页签消失、数据仍在；清除数据要二次确认", async ({ page }) => {
    const st = fresh();
    st.installed = true;
    st.records = [
      { record_key: "01K0000000000000000000AAA", data: { ticketNo: "T-9", title: "旧单", status: "open" }, version: 1, created_at: "", updated_at: "" },
    ];
    await stub(page, st);
    const dlg = await openPluginTab(page);

    // 先回到「插件」页签执行卸载
    await dlg.getByRole("tab", { name: "插件", exact: true }).click();
    await expect(dlg.getByText("启用中")).toBeVisible();
    await dlg.getByRole("button", { name: "卸载" }).click();
    await page.getByRole("button", { name: "卸载", exact: true }).last().click();
    await expect(dlg.getByText("已卸载")).toBeVisible();

    // 卸载 ⇒ 页签消失（数据仍在，清除入口留在清单里）
    await expect(dlg.getByRole("tab", { name: "工单" })).toHaveCount(0);
    await expect(dlg.getByRole("tab")).toHaveCount(5);

    // 清除数据必须先过二次确认；确认前的文案要说清"永久删除"。
    await dlg.getByRole("button", { name: "清除数据" }).click();
    const confirm = page.getByRole("dialog").filter({ hasText: "永久删除" });
    await expect(confirm).toBeVisible();
    await page.getByRole("button", { name: "永久删除" }).click();
    await expect.poll(() => st.purged).toBe(1);
  });

  test("旧后端描述（缺 sort/filters/form/config_missing）不崩，退化成单列表单", async ({ page }) => {
    const st = fresh();
    st.installed = true;
    st.legacy = true;
    st.records = [
      {
        record_key: "01K0000000000000000000AAA",
        data: { ticketNo: "T-1", title: "一", status: "open" },
        version: 1,
        created_at: "2026-11-18 10:00:00",
        updated_at: "2026-11-18 10:00:00",
      },
    ];
    const failures: string[] = [];
    page.on("pageerror", (e) => failures.push(e.message));
    await stub(page, st);

    // 页签能打开、表格照常出数据（旧描述里 list 只有 columns/actions）
    const dlg = await openPluginTab(page);
    await expect(dlg.getByText("T-1")).toBeVisible();

    // 新建：没有分段声明 ⇒ 单段单列、无段头，但每个字段都在
    await dlg.getByRole("button", { name: "新建" }).click();
    const form = page.getByRole("dialog", { name: /新建工单/ });
    await expect(form.getByLabel("单号")).toBeVisible();
    await expect(form.getByLabel("详情")).toBeVisible();
    await expect(form.getByRole("heading", { name: "基本信息" })).toHaveCount(0);

    // 没有 config_missing 时不该有任何横幅，更不该抛异常（错误边界会接住整页）
    expect(failures).toEqual([]);
  });

  test("排序、筛选、只读详情与批量删除：参数与请求体", async ({ page }) => {
    const st = fresh();
    st.installed = true;
    st.records = [
      {
        record_key: "01K0000000000000000000AAA",
        data: { ticketNo: "T-1", title: "一", status: "open", due: "2026-02-01" },
        version: 1,
        created_at: "2026-11-18 10:00:00",
        updated_at: "2026-11-18 10:00:00",
      },
      {
        record_key: "01K0000000000000000000BBB",
        data: { ticketNo: "T-2", title: "二", status: "done", due: "2026-03-01" },
        version: 1,
        created_at: "2026-11-18 10:01:00",
        updated_at: "2026-11-18 10:01:00",
      },
    ];
    await stub(page, st);
    const dlg = await openPluginTab(page);

    // ① 筛选：控件由 manifest 的 list.filters 决定（这里是状态下拉）
    const filtered = page.waitForRequest((r) => r.url().includes("f_status=open"));
    await dlg.getByLabel("状态").selectOption("open");
    await filtered;

    // ② 排序：只有白名单里的列可点。初始方向来自 manifest（asc），点一下变 desc。
    const sorted = page.waitForRequest((r) => decodeURIComponent(r.url()).includes("sort=due:desc"));
    await dlg.getByRole("columnheader", { name: "期望完成" }).click();
    await sorted;

    // ③ 只读详情：行点击打开，没有保存按钮、字段不可改
    await dlg.getByRole("button", { name: "查看" }).first().click();
    const view = page.getByRole("dialog", { name: /查看工单/ });
    await expect(view).toBeVisible();
    await expect(view.getByRole("button", { name: "保存" })).toHaveCount(0);
    await expect(view.getByLabel("单号")).toBeDisabled();
    // 右上角 X 与页脚「关闭」同名，取页脚那个（last）
    await view.getByRole("button", { name: "关闭" }).last().click();

    // ④ 批量删除：勾选两行 → 确认 → 一次请求带两个 key
    await dlg.getByRole("checkbox", { name: `选择 ${st.records[0].record_key}` }).check();
    await dlg.getByRole("checkbox", { name: `选择 ${st.records[1].record_key}` }).check();
    const batched = page.waitForRequest(
      (r) => r.url().endsWith("/records/batch-delete") && r.method() === "POST",
    );
    await dlg.getByRole("button", { name: /删除所选/ }).click();
    // 确认框是根部宿主弹窗：按标题定位，别用 page 级「删除」——行内动作也叫删除。
    const confirm = page.getByRole("dialog", { name: /删除所选的 2 条/ });
    await confirm.getByRole("button", { name: "删除" }).click();
    const body = (await batched).postDataJSON() as { entity: string; record_keys: string[] };
    expect(body.entity).toBe("ticket");
    expect(body.record_keys.sort()).toEqual([
      "01K0000000000000000000AAA",
      "01K0000000000000000000BBB",
    ]);
    await expect.poll(() => st.records.length).toBe(0);
  });

  test("375px：页签在视口内，插件面板不横向溢出", async ({ page }) => {
    const st = fresh();
    st.installed = true;
    await stub(page, st);
    await page.setViewportSize({ width: 375, height: 780 });
    const dlg = await openManage(page);

    const tab = dlg.getByRole("tab", { name: "插件", exact: true });
    await tab.click();
    await expect(tab).toBeInViewport();
    await expectNoHScroll(page);

    // 插件面板（工具栏 + 表格 + 分页）在 375 下同样不能横向溢出
    await dlg.getByRole("button", { name: "打开" }).click();
    const pluginTab = dlg.getByRole("tab", { name: "工单" });
    await expect(pluginTab).toBeInViewport();
    const overflow = await dlg
      .locator(".page-tabs > .tab-panel:not([hidden])")
      .evaluate((el) => el.scrollWidth - el.clientWidth);
    expect(overflow).toBeLessThanOrEqual(1);
    await expectNoHScroll(page);
  });
});
