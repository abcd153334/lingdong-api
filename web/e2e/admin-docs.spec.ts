import { expect, test, type Page } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

/**
 * 管理台 · 文档编辑页（/admin/docs）e2e。
 *
 * 全部 stub，桩件按真实响应形状构造。只断言「看起来对、落地会错」的几件事：
 *  1. 左栏是三级级联（板块 → 节点 → 接口），选中进 URL；
 *  2. 右栏顶部能切「编辑 / 预览」，预览走**后端投影**（与公开站同一套组件）；
 *  3. 改一个字就出现未保存标记，保存发的是**整篇**（字段/响应/示例全量）；
 *  4. 未绑定平台端点的体检结论在编辑页就看得见，不用等点发布；
 *  5. 板块名是库里的内容：改完保存发到 /admin/docs/sections/:key。
 */

const FIELD = (name: string, extra: Record<string, unknown> = {}) => ({
  name,
  type: "string",
  in: "body",
  required: "optional",
  default: "",
  desc: `${name} 的说明`,
  tier: "standard",
  ...extra,
});

const SAMPLES = {
  curl: "curl -X POST '{{base_url}}/v1/video/generations' -d '{}'",
  python: "requests.post('{{base_url}}/v1/video/generations')",
  node: "fetch('{{base_url}}/v1/video/generations')",
  go: "http.Post(\"{{base_url}}/v1/video/generations\", \"\", nil)",
};

const EP = {
  node_key: "videos-kling",
  key: "video-submit",
  name: "创建视频生成任务",
  summary: "提交后立即返回任务号。",
  method: "POST",
  path: "/v1/video/generations",
  aliases: [],
  transport_doc_id: "",
  method_check: "",
  path_check: "",
  phase: "sync",
  run_mode: "json",
  run_hint: "",
  max_body: "",
  notes: [],
  badge: "",
  sort: 0,
  hidden: false,
  status: "draft",
  version: 2,
  origin: "manual",
  contract_ver: "",
  updated_at: "2026-11-12T10:00:00+08:00",
  fields: [FIELD("model", { required: "required" }), FIELD("prompt")],
  response: {
    kind: "json",
    envelope: false,
    content_type: "",
    variants: [
      {
        status: 200,
        label: "成功",
        content_type: "",
        kind: "json",
        envelope: false,
        returns: [FIELD("url")],
        example: [],
        raw: "",
        notes: [],
      },
    ],
  },
  samples: SAMPLES,
  samples2: null,
  example_title: "",
  example_title2: "",
  presets: [],
  errors: [],
  problems: [
    {
      code: "E_UNBOUND",
      field: "transport_doc_id",
      message: "未绑定平台已登记的端点，只能存草稿；绑定后才能发布。",
      blocking: true,
    },
  ],
};

const TREE = {
  shape: 7,
  contract_version: "6",
  base_url: "https://api.example.com",
  sections: [
    {
      key: "videos",
      name: "视频",
      en: "Videos",
      desc: "",
      color: "#6366f1",
      sort: 40,
      hidden: false,
      version: 1,
      unknown: false,
      nodes: [
        {
          key: "videos-kling",
          kind: "family",
          title: "KLING",
          status: "draft",
          hidden: false,
          sort: 0,
          version: 2,
          models: 1,
          endpoints: [
            {
              key: "video-submit",
              name: "创建视频生成任务",
              method: "POST",
              path: "/v1/video/generations",
              badge: "async",
              status: "draft",
              hidden: false,
              sort: 0,
              version: 2,
              bound: false,
            },
          ],
        },
      ],
    },
  ],
  totals: { nodes: 1, endpoints: 1, draft_nodes: 1, draft_endpoints: 1 },
};

const NODE = {
  key: "videos-kling",
  kind: "family",
  section: "videos",
  section_name: "视频",
  protocol: "",
  title: "KLING",
  summary: "",
  sort: 0,
  hidden: false,
  status: "draft",
  version: 2,
  origin: "manual",
  updated_at: "2026-11-12T10:00:00+08:00",
  models: [{ id: "kling-v2", name: "Kling V2", vendor: "kling" }],
  blocks: [],
  endpoints: TREE.sections[0].nodes[0].endpoints,
  problems: [],
};

/** 后端投影（公开站只吃这个形状）。 */
const PROJECTED = {
  node: { key: "videos-kling", title: "KLING", section: "videos", section_name: "视频" },
  key: "video-submit",
  kind: "",
  name: "创建视频生成任务",
  summary: "提交后立即返回任务号。",
  method: "POST",
  path: "/v1/video/generations",
  aliases: [],
  phase: "同步",
  transport: "json",
  run_mode: "json",
  notes: [],
  fields: [FIELD("model", { required: "required" }), FIELD("prompt")],
  response: {
    kind: "json",
    envelope: false,
    variants: [{ status: 200, label: "成功", returns: [FIELD("url")], example: [] }],
  },
  samples: SAMPLES,
  presets: [],
  errors: [],
  models: [],
  published: false,
};

async function stub(page: Page) {
  await seedAuth(page, { role: 0 });
  await page.route("**/api/admin/docs/tree", (r) => r.fulfill({ json: envelope(TREE) }));
  await page.route("**/api/admin/docs/transports", (r) =>
    r.fulfill({
      json: envelope([
        {
          doc_id: "videos/submit",
          section: "videos",
          section_name: "视频",
          method: "POST",
          path: "/v1/video/generations",
          phase: "submit",
          badge: "async",
          accepts_async: true,
          used_by: [],
        },
      ]),
    }),
  );
  await page.route("**/api/admin/docs/nodes/videos-kling/endpoints/video-submit/preview", (r) =>
    r.fulfill({ json: envelope(PROJECTED) }),
  );
  await page.route("**/api/admin/docs/nodes/videos-kling/endpoints/video-submit", (r) =>
    r.fulfill({ json: envelope(EP) }),
  );
  await page.route("**/api/admin/docs/nodes/videos-kling", (r) => r.fulfill({ json: envelope(NODE) }));
}

test.describe("文档编辑页", () => {
  /**
   * 从空白页点进三级：这条用例守的是「左栏能不能点开」。
   *
   * 所有其它用例都直接深链 `?node=&ep=`，那条路径会由「选中项所在板块自动展开」
   * 兜住，于是「组头点不开」这个 bug 一路绿灯 —— 直到有人真的从空页进来，
   * 看到七个板块一行字都没有。组头必须自己会折叠/展开，不能只靠选中态。
   */
  test("从空页点开左栏：箭头展开、行主体选中，三级都能进右栏", async ({ page }) => {
    await stub(page);
    await page.goto("/admin/docs");

    const rail = page.getByRole("complementary", { name: "文档目录" });
    // 首屏就摊开第一个有节点的板块：进页面看得见东西，不用先猜着点一下
    const secRow = rail.locator('[data-level="1"]').filter({ hasText: "视频" }).first();
    await expect(secRow.locator(".adp-row__caret")).toHaveAttribute("aria-expanded", "true");
    await expect(rail.getByRole("button", { name: /KLING/ })).toBeVisible();

    // 箭头只管开合：收起后节点不在 DOM 里，选中与 URL 一点没动
    await secRow.locator(".adp-row__caret").click();
    await expect(secRow.locator(".adp-row__caret")).toHaveAttribute("aria-expanded", "false");
    await expect(rail.getByRole("button", { name: /KLING/ })).toHaveCount(0);
    await expect(page).not.toHaveURL(/node=/);
    await secRow.locator(".adp-row__caret").click();
    await expect(rail.getByRole("button", { name: /KLING/ })).toBeVisible();

    // 节点行也有箭头（默认收起 → 接口不在 DOM），点主体进右栏
    const nodeRow = rail.locator('[data-level="2"]').filter({ hasText: "KLING" }).first();
    await expect(nodeRow.locator(".adp-row__caret")).toHaveAttribute("aria-expanded", "false");
    await nodeRow.locator(".adp-row__hit").click();
    await expect(page).toHaveURL(/node=videos-kling/);
    await expect(page.getByRole("tabpanel")).toContainText("基本信息");
    // 选中之后这条路径自动摊开：否则选中的东西在左栏看不见
    await expect(nodeRow.locator(".adp-row__caret")).toHaveAttribute("aria-expanded", "true");

    // 接口 → 右栏
    await rail.getByRole("button", { name: /创建视频生成任务/ }).click();
    await expect(page).toHaveURL(/ep=video-submit/);
    await expect(page.getByLabel("接口名")).toHaveValue("创建视频生成任务");
  });

  test("左栏是级联树：每行都有箭头列，箭头只动展开、主体只动选中", async ({ page }) => {
    await seedAuth(page, { role: 0 });

    // 两块各两个节点 —— 只有多块多节点才看得出"是级联还是两段列表"。
    const mkEp = (key: string, name: string) => ({
      key,
      name,
      method: "POST",
      path: `/v1/${key}`,
      badge: "",
      status: "published",
      hidden: false,
      sort: 0,
      version: 1,
      bound: true,
    });
    const tree = {
      ...TREE,
      sections: [
        {
          ...TREE.sections[0],
          nodes: [
            {
              ...TREE.sections[0].nodes[0],
              endpoints: [mkEp("kling-a", "可灵甲"), mkEp("kling-b", "可灵乙")],
            },
            {
              ...TREE.sections[0].nodes[0],
              key: "videos-seedance",
              title: "SEEDANCE",
              endpoints: [mkEp("seedance-a", "Seedance 甲")],
            },
          ],
        },
        {
          key: "images",
          name: "图片",
          en: "Images",
          desc: "",
          color: "#22c55e",
          sort: 30,
          hidden: false,
          version: 1,
          unknown: false,
          nodes: [
            {
              ...TREE.sections[0].nodes[0],
              key: "images-a",
              title: "A 族",
              endpoints: [mkEp("img-a", "A 族生成")],
            },
          ],
        },
      ],
      totals: { nodes: 3, endpoints: 4, draft_nodes: 0, draft_endpoints: 0 },
    };
    await page.route("**/api/admin/docs/tree", (r) => r.fulfill({ json: envelope(tree) }));
    await page.route("**/api/admin/docs/transports", (r) => r.fulfill({ json: envelope([]) }));
    await page.route("**/api/admin/docs/check", (r) => r.fulfill({ json: envelope([]) }));
    await page.route("**/api/admin/docs/nodes/*/endpoints/*", (r) => r.fulfill({ json: envelope(EP) }));
    await page.route("**/api/admin/docs/nodes/*", (r) => r.fulfill({ json: envelope(NODE) }));

    await page.goto("/admin/docs");
    const rail = page.getByRole("complementary", { name: "文档目录" });
    // 行定位统一按 data-level：一级=板块，二级=节点，三级=接口
    const sec = (name: string) => rail.locator('[data-level="1"]').filter({ hasText: name }).first();
    const node = (title: string) => rail.locator('[data-level="2"]').filter({ hasText: title }).first();

    // ① 首屏只摊开一条路径：视频开着，图片关着（节点全收起）
    await expect(sec("视频").locator(".adp-row__caret")).toHaveAttribute("aria-expanded", "true");
    await expect(sec("图片").locator(".adp-row__caret")).toHaveAttribute("aria-expanded", "false");
    await expect(rail.locator('[data-level="2"]').filter({ hasText: "A 族" })).toHaveCount(0);

    // ② 两级各自独立开合：打开图片后视频**仍然开着**（级联菜单不是手风琴）
    await sec("图片").locator(".adp-row__caret").click();
    await expect(sec("图片").locator(".adp-row__caret")).toHaveAttribute("aria-expanded", "true");
    await expect(sec("视频").locator(".adp-row__caret")).toHaveAttribute("aria-expanded", "true");
    await expect(node("A 族")).toBeVisible();

    // ③ 箭头只动展开：收起图片，URL 与右栏一个字节都不变
    await sec("图片").locator(".adp-row__caret").click();
    await expect(sec("图片").locator(".adp-row__caret")).toHaveAttribute("aria-expanded", "false");
    await expect(page).not.toHaveURL(/node=|ep=|s=/);

    // ④ 节点行也有箭头，且默认收起：接口不在 DOM 里（不是被 CSS 藏起来）
    await expect(node("A 族")).toHaveCount(0);
    await sec("图片").locator(".adp-row__caret").click();
    await expect(node("A 族").locator(".adp-row__caret")).toHaveAttribute("aria-expanded", "false");
    await expect(rail.getByRole("button", { name: "A 族生成" })).toHaveCount(0);

    // ⑤ 展开节点只动展开态，不动选中
    await node("A 族").locator(".adp-row__caret").click();
    await expect(rail.getByRole("button", { name: "A 族生成" })).toBeVisible();
    await expect(page).not.toHaveURL(/node=/);

    // ⑥ 点行主体才选中它，右栏跟着换
    await node("A 族").locator(".adp-row__hit").click();
    await expect(page).toHaveURL(/node=images-a/);
    await expect(page.getByRole("tabpanel")).toContainText("基本信息");

    // ⑦ 叶子行也占住箭头列（三级文字才成阶梯，而不是靠"有没有箭头"暗示层级）
    await expect(rail.locator(".adp-row--ep .adp-caret--leaf")).toHaveCount(1);

    // ⑧ 手动收起当前节点：接口消失，但选中还在（箭头说了算，选中说了不算）
    await node("A 族").locator(".adp-row__caret").click();
    await expect(rail.getByRole("button", { name: "A 族生成" })).toHaveCount(0);
    await expect(page).toHaveURL(/node=images-a/);

    // ⑨ 换到另一个板块的节点：旧节点的展开位不丢，新节点自动摊开
    await node("SEEDANCE").locator(".adp-row__hit").click();
    await expect(page).toHaveURL(/node=videos-seedance/);
    await expect(rail.getByRole("button", { name: "Seedance 甲" })).toBeVisible();
  });

  test("两栏：左栏三级级联，右栏编辑；切到预览是后端投影", async ({ page }) => {
    await stub(page);
    await page.goto("/admin/docs?node=videos-kling&ep=video-submit");

    // 左栏：板块 → 节点 → 接口，三级都在
    const rail = page.getByRole("complementary", { name: "文档目录" });
    await expect(rail.getByRole("button", { name: /视频/ }).first()).toBeVisible();
    await expect(rail.getByRole("button", { name: /KLING/ })).toBeVisible();
    await expect(rail.getByRole("button", { name: /创建视频生成任务/ })).toBeVisible();

    // 右栏默认是编辑态
    await expect(page.getByRole("tab", { name: "编辑" })).toHaveAttribute("aria-selected", "true");
    await expect(page.getByRole("tabpanel")).toContainText("基本信息");

    // 切到预览：出现公开站的嵌入形态（同一套渲染组件）
    await page.getByRole("tab", { name: "预览" }).click();
    const embed = page.getByTestId("docs-embed");
    await expect(embed).toBeVisible();
    await expect(embed).toContainText("创建视频生成任务");
    // 嵌入形态没有文档站侧栏，否则窄栏里再切一半
    await expect(embed.locator(".d-side")).toHaveCount(0);
  });

  test("改一个字就出现未保存标记，保存发整篇（字段与示例都在）", async ({ page }) => {
    await stub(page);
    let saved: Record<string, unknown> | null = null;
    await page.route("**/api/admin/docs/nodes/videos-kling/endpoints/video-submit", async (r) => {
      if (r.request().method() !== "PUT") return r.fulfill({ json: envelope(EP) });
      saved = JSON.parse(r.request().postData() || "{}") as Record<string, unknown>;
      return r.fulfill({ json: envelope(EP) });
    });

    await page.goto("/admin/docs?node=videos-kling&ep=video-submit");
    const name = page.getByLabel("接口名");
    await name.fill("创建视频任务");

    await expect(page.getByText("未保存")).toBeVisible();
    // 有未保存改动时不许直接发布（发的会是库里的旧内容）
    await expect(page.getByRole("button", { name: /^发布$/ })).toBeDisabled();

    await page.getByRole("button", { name: "保存" }).click();
    await expect.poll(() => saved).not.toBeNull();
    const body = saved as unknown as Record<string, unknown>;
    expect(body.name).toBe("创建视频任务");
    // 整篇提交：字段表、响应、示例代码都在同一个 body 里
    expect(Array.isArray(body.fields)).toBe(true);
    expect((body.fields as unknown[]).length).toBe(2);
    expect(body.samples).toMatchObject({ curl: expect.stringContaining("{{base_url}}") });
    expect(body.version).toBe(2);
  });

  test("未绑定平台端点的体检结论在编辑页就看得见", async ({ page }) => {
    await stub(page);
    await page.goto("/admin/docs?node=videos-kling&ep=video-submit");
    await expect(page.getByText("E_UNBOUND")).toBeVisible();
    await expect(page.getByText(/未绑定平台已登记的端点/)).toBeVisible();
  });

  test("板块名来自库：改名后保存打到 /sections/:key", async ({ page }) => {
    await stub(page);
    let body: Record<string, unknown> | null = null;
    await page.route("**/api/admin/docs/sections/videos", async (r) => {
      body = JSON.parse(r.request().postData() || "{}") as Record<string, unknown>;
      return r.fulfill({
        json: envelope({ ...TREE.sections[0], name: String(body.name), version: 2 }),
      });
    });

    await page.goto("/admin/docs?s=videos");
    await expect(page.getByRole("tabpanel")).toContainText("板块");
    await page.getByLabel("名称").fill("图像");
    await page.getByRole("button", { name: "保存" }).click();
    await expect.poll(() => body).not.toBeNull();
    expect((body as unknown as Record<string, unknown>).name).toBe("图像");
  });

  test("1023 以下左栏变抽屉，选中后自动收起", async ({ page }) => {
    await page.setViewportSize({ width: 900, height: 800 });
    await stub(page);
    await page.goto("/admin/docs?node=videos-kling&ep=video-submit");

    // 抽屉形态下默认收起（display:none）。用类名定位：打开时它是 role=dialog，
    // 关闭时是 complementary —— 按 role 找会在两次状态之间换 locator。
    const rail = page.locator(".adp-rail");
    await expect(rail).not.toBeVisible();

    await page.getByRole("button", { name: "打开文档目录" }).click();
    await expect(rail).toBeVisible();
    await rail.getByRole("button", { name: /KLING/ }).click();
    await expect(rail).not.toBeVisible();
  });
});
