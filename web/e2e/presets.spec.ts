/** 内置预设「恢复出厂」契约用例：page.route 打桩，验证入口可见性、dry-run 预览与确认入参。 */
import { expect, test, type Page } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

/**
 * 内置预设（出厂默认）是**运营已有的现网配置**，恢复出厂是全站唯一会覆盖
 * 运营改名/改价/改绑定的动作。本文件守三条不变量：
 *
 *  1. 入口只在**存在内置行**时出现 —— 关掉内置预设的实例不该看到一个必然 400 的按钮；
 *  2. 打开弹窗只发 `dry_run:true`（预览），**任何情况下都不在打开时写入**；
 *  3. 确认时必带 `confirm:"RESTORE"`，且 `keep_enabled` 严格跟随勾选（默认 true）。
 */

const ADAPTERS = [
  {
    id: "duomiapi",
    name: "duomiapi",
    label: "多米API",
    vendor: "duomi",
    caps: ["text"],
    status: 1,
    source: "builtin",
    editable: false,
    deletable: false,
    channel_count: 1,
    endpoints: [],
    supports_declarative: true,
  },
];

/** 一条内置渠道：`preset_group` 是入口可见性的唯一判据 */
const BUILTIN_CHANNEL = {
  id: 21,
  name: "多米API",
  icon: "",
  icon_url: "",
  adapter_id: "duomiapi",
  base_url: "https://duomiapi.com",
  website: "https://duomiapi.com",
  website_url: "https://duomiapi.com",
  preset_group: "builtin.duomiapi",
  multi_key_mode: "polling",
  groups: ["default"],
  weight: 1,
  priority: 1,
  rpm_limit: 0,
  max_concurrency: 0,
  inflight: 0,
  inflight_limit: 0,
  status: 2,
  auto_ban: true,
  balance_cny: null,
  key_count: 0,
  key_enabled_count: 0,
  switch_key_retry: false,
  version: 1,
  health: {},
  adapter_config: {},
  adapter_options: null,
  bound_models: [],
};

/** 运营自建渠道（没有内置标记） */
const OWN_CHANNEL = {
  ...BUILTIN_CHANNEL,
  id: 22,
  name: "我的中转",
  base_url: "https://my.example.com",
  website: "",
  website_url: "",
  preset_group: "",
};

const PREVIEW = {
  dry_run: true,
  groups: [
    {
      group: "builtin.duomiapi",
      label: "多米API",
      channels: { reused: 1, created: 0, skipped: 0 },
      models: { reused: 15, created: 0, skipped: 0 },
      bindings: { reused: 15, created: 0, skipped: 0 },
    },
    {
      group: "builtin.metaso",
      label: "Metaso 标准通道",
      channels: { reused: 1, created: 0, skipped: 0 },
      models: { reused: 1, created: 0, skipped: 0 },
      bindings: { reused: 0, created: 1, skipped: 0 },
      warnings: ["渠道 #31 恢复失败：名称已被占用"],
    },
  ],
};

const APPLIED = {
  dry_run: false,
  groups: PREVIEW.groups,
};

interface StubOptions {
  channels?: unknown[];
}

/** 挂载 /api/admin/** 桩件，返回请求体记录（用于断言 preview/confirm 的真实入参） */
async function stubAdmin(page: Page, opts: StubOptions = {}): Promise<string[]> {
  const bodies: string[] = [];
  await page.route(
    (url) => url.pathname.startsWith("/api/admin/"),
    (route) => {
      const { pathname } = new URL(route.request().url());
      const method = route.request().method();

      if (pathname === "/api/admin/presets/restore" && method === "POST") {
        const raw = route.request().postData() ?? "";
        bodies.push(raw);
        const body = JSON.parse(raw || "{}") as { dry_run?: boolean };
        return route.fulfill({ json: envelope(body.dry_run ? PREVIEW : APPLIED) });
      }
      if (pathname === "/api/admin/channels" && method === "GET") {
        return route.fulfill({
          json: envelope({
            items: opts.channels ?? [BUILTIN_CHANNEL],
            adapters: ADAPTERS,
            groups: ["default"],
            total: 1,
            monitor: {
              auto_disable: true,
              fail_threshold: 0.5,
              fail_window: 10,
              auto_enable: true,
              probe_interval_min: 5,
            },
          }),
        });
      }
      if (pathname === "/api/admin/models" && method === "GET") {
        return route.fulfill({
          json: envelope({ items: [], total: 0, vendors: [], tag_palette: [] }),
        });
      }
      if (pathname === "/api/admin/adapters") {
        return route.fulfill({ json: envelope({ items: ADAPTERS }) });
      }
      return route.fulfill({ json: envelope(null) });
    },
  );
  return bodies;
}

async function openModelService(page: Page): Promise<void> {
  await page.goto("/admin/model-service");
  // 左栏就位 = 页面（含渠道/适配器两个列表）已渲染，后面的入口断言才有意义
  await expect(page.locator(".ms-rail")).toBeVisible();
}

test.describe("恢复出厂", () => {
  test("有内置行时左栏出现入口，点开只做 dry-run 预览", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const bodies = await stubAdmin(page);
    await openModelService(page);

    const entry = page.getByRole("button", { name: "恢复出厂…" });
    await expect(entry).toBeVisible();
    await entry.click();

    const dlg = page.getByRole("dialog", { name: "恢复出厂配置" });
    await expect(dlg).toBeVisible();
    // 预览按组渲染，且组名给人看的那一列是渠道出厂名，不是 slug
    await expect(dlg.getByText("多米API", { exact: false })).toBeVisible();
    await expect(dlg.getByText("Metaso 标准通道")).toBeVisible();
    // 单行失败的留痕必须展示（否则"少恢复了 1 项"无从解释）
    await expect(dlg.getByText(/名称已被占用/)).toBeVisible();
    await expect(dlg.getByText(/尚未写入/)).toBeVisible();

    expect(bodies).toHaveLength(1);
    expect(JSON.parse(bodies[0])).toMatchObject({ dry_run: true, keep_enabled: true });
    // 预览绝不能带 confirm
    expect(JSON.parse(bodies[0]).confirm).toBeUndefined();
  });

  test("确认时带 confirm=RESTORE；勾选「同时停用」后 keep_enabled=false", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const bodies = await stubAdmin(page);
    await openModelService(page);

    await page.getByRole("button", { name: "恢复出厂…" }).click();
    const dlg = page.getByRole("dialog", { name: "恢复出厂配置" });
    await expect(dlg).toBeVisible();

    // 默认不勾「同时停用」：恢复配置不该顺手把在跑的渠道停掉
    const box = dlg.getByRole("checkbox");
    await expect(box).not.toBeChecked();
    await dlg.getByRole("button", { name: "确认恢复" }).click();

    await expect.poll(() => bodies.length).toBe(2);
    expect(JSON.parse(bodies[1])).toMatchObject({
      dry_run: false,
      confirm: "RESTORE",
      keep_enabled: true,
    });
    // 真跑返回后提示改为「已生效」
    await expect(dlg.getByText(/已生效/)).toBeVisible();
  });

  test("勾选后 keep_enabled 传 false", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const bodies = await stubAdmin(page);
    await openModelService(page);

    await page.getByRole("button", { name: "恢复出厂…" }).click();
    const dlg = page.getByRole("dialog", { name: "恢复出厂配置" });
    await dlg.getByRole("checkbox").check();
    await dlg.getByRole("button", { name: "确认恢复" }).click();

    await expect.poll(() => bodies.length).toBe(2);
    expect(JSON.parse(bodies[1])).toMatchObject({ dry_run: false, keep_enabled: false });
  });

  test("没有内置行时不渲染入口（不给必然 400 的按钮）", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stubAdmin(page, { channels: [OWN_CHANNEL] });
    await openModelService(page);

    await expect(page.locator(".ms-rail")).toBeVisible();
    // 入口不渲染 —— 关掉内置预设的实例不该看到一个必然 400 的按钮
    await expect(page.getByRole("button", { name: "恢复出厂…" })).toHaveCount(0);
  });
});
