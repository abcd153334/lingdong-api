import { expect, test, type Page } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

/**
 * 管理台 · 文档编辑页**几何**回归（零溢出）。
 *
 * 为什么单独一份：此前所有用例都只断言"看得到什么"，于是
 * 「给控件写死 width，被 components.css 的 `input[type]{width:100%}` 反杀」
 * 这类事故一路绿灯 —— 输入框 794px、错误码表把后两列推出屏幕，测试全绿。
 *
 * 这份用例只守一条铁律：**右栏与顶栏在任何宽度下都不许横向溢出**。
 * 数据取最坏样本（14 字段 / 24 可选值 / 8 错误码 / 2 调用方式 / 2 响应变体），
 * 铺开后再量，避免"折叠态本来就不占宽"的假阴性。
 */

const FIELD = (name: string, extra: Record<string, unknown> = {}) => ({
  name,
  type: "string",
  in: "body",
  required: "optional",
  default: "",
  desc: `${name} 的说明。`,
  tier: "standard",
  ...extra,
});

const ENUM24 = Array.from({ length: 24 }, (_, i) => ({
  value: `value-${i + 1}`,
  desc: `第 ${i + 1} 个取值的说明`,
  deprecated: i === 3,
}));

/** 14 个请求字段：其中 modality 带 24 个可选值（最宽的行） */
const FIELDS = [
  FIELD("model", { required: "required", example: "gpt-image-2" }),
  FIELD("prompt", { type: "string", required: "required" }),
  FIELD("modality", { enum: ENUM24, value_source: "static" }),
  FIELD("size", { constraints: [{ kind: "text", value: 0, text: "最长边不超过 3840px" }] }),
  FIELD("aspect_ratio", { required: "conditional", required_note: "与 size 至少给一个" }),
  FIELD("n", { type: "integer", default: "1" }),
  FIELD("quality", { tier: "V" }),
  FIELD("background", { enum: [{ value: "transparent", desc: "透明" }, { value: "opaque", desc: "不透明" }] }),
  FIELD("output_format", { enum: [{ value: "url", desc: "链接" }, { value: "b64_json", desc: "内联" }] }),
  FIELD("response_format", { enum: [{ value: "url", desc: "链接" }] }),
  FIELD("seed", { type: "integer" }),
  FIELD("style", { only_models: "gpt-image-2" }),
  FIELD("image", { type: "file", in: "form" }),
  FIELD("mask", { type: "file", in: "form", passthrough: true }),
];

const ERRORS = Array.from({ length: 8 }, (_, i) => ({
  status: 400 + i,
  code: `error_code_${i + 1}`,
  desc: `第 ${i + 1} 条错误码：什么情况下出现、怎么处理。`,
  source: i % 2 === 0 ? "seed" : "manual",
}));

const TREE = {
  shape: 7,
  contract_version: "6",
  base_url: "https://api.example.com",
  sections: [
    {
      key: "images",
      name: "图片",
      en: "Images",
      desc: "",
      color: "#6366f1",
      sort: 10,
      hidden: false,
      version: 1,
      unknown: false,
      nodes: [
        {
          key: "images-gpt-image",
          kind: "family",
          title: "GPT Image",
          status: "published",
          hidden: false,
          sort: 10,
          version: 1,
          models: 2,
          endpoints: [
            {
              key: "ep-images-generations",
              name: "图片生成",
              method: "POST",
              path: "/v1/images/generations",
              badge: "",
              status: "published",
              hidden: false,
              sort: 10,
              version: 1,
              bound: true,
            },
          ],
        },
      ],
    },
  ],
  totals: { nodes: 1, endpoints: 1, draft_nodes: 0, draft_endpoints: 0 },
};

const EP = {
  node_key: "images-gpt-image",
  key: "ep-images-generations",
  name: "图片生成",
  summary: "文生图 / 图生图统一入口。",
  method: "POST",
  path: "/v1/images/generations",
  aliases: [],
  transport_doc_id: "",
  method_check: "",
  path_check: "",
  phase: "sync",
  run_mode: "json",
  run_hint: "",
  max_body: "20MB",
  notes: ["传 async: true 时同步返回 202。"],
  badge: "",
  sort: 10,
  hidden: false,
  status: "published",
  version: 1,
  origin: "seed",
  contract_ver: "",
  updated_at: "2026-11-14T10:00:00+08:00",
  fields: FIELDS,
  response: {
    kind: "json",
    envelope: false,
    content_type: "",
    variants: [
      {
        status: 200,
        label: "成功（同步出图）",
        content_type: "image/png",
        kind: "json",
        envelope: false,
        returns: [FIELD("created"), FIELD("data[].url"), FIELD("usage")],
        example: [],
        raw: "",
        notes: ["默认行为：本次请求内等到出图。"],
      },
      {
        status: 202,
        label: "已受理（异步）",
        content_type: "",
        kind: "json",
        envelope: false,
        returns: [FIELD("task_id"), FIELD("status")],
        example: [],
        raw: "",
        notes: [],
      },
    ],
  },
  samples: { curl: "curl '{{base_url}}/v1/images/generations'", python: "", node: "", go: "" },
  samples2: null,
  example_title: "",
  example_title2: "",
  presets: [
    {
      key: "image-t2i",
      label: "文生图",
      summary: "只给描述，直接出图。",
      body: [
        { k: "model", v: "gpt-image-2" },
        { k: "prompt", v: "赛博朋克风格的城市夜景" },
        { k: "size", v: "2K" },
      ],
      fields: ["model", "prompt", "size"],
      uploads: [],
      note: "",
      samples: null,
    },
    {
      key: "image-i2i",
      label: "图生图（带参考图）",
      summary: "",
      body: [],
      fields: [],
      uploads: ["image"],
      note: "",
      samples: null,
    },
  ],
  errors: ERRORS,
  problems: [],
};

/** 后端投影（预览页签吃这个形状） */
const PROJECTED = {
  node: { key: "images-gpt-image", title: "GPT Image", section: "images", section_name: "图片" },
  key: "ep-images-generations",
  kind: "",
  name: "图片生成",
  summary: "文生图 / 图生图统一入口。",
  method: "POST",
  path: "/v1/images/generations",
  aliases: [],
  phase: "同步",
  transport: "json",
  run_mode: "json",
  notes: [],
  fields: [FIELD("model", { required: "required" }), FIELD("prompt", { required: "required" })],
  response: {
    kind: "json",
    envelope: false,
    variants: [{ status: 200, label: "成功", returns: [FIELD("url")], example: [] }],
  },
  samples: { curl: "curl '{{base_url}}/v1/images/generations'", python: "", node: "", go: "" },
  presets: [],
  errors: [],
  models: [],
  published: true,
};

async function stub(page: Page) {
  await seedAuth(page, { role: 0 });
  await page.route("**/api/admin/docs/tree", (r) => r.fulfill({ json: envelope(TREE) }));
  await page.route("**/api/admin/docs/transports", (r) => r.fulfill({ json: envelope([]) }));
  await page.route("**/api/admin/docs/nodes/images-gpt-image/endpoints/ep-images-generations", (r) =>
    r.fulfill({ json: envelope(EP) }),
  );
  // 预览走 POST …/preview（后端投影）；不桩会打到真后端 → 401 → 预览区只剩错误态
  await page.route("**/api/admin/docs/nodes/images-gpt-image/endpoints/ep-images-generations/preview", (r) =>
    r.fulfill({ json: envelope(PROJECTED) }),
  );
  await page.route("**/api/admin/docs/nodes/images-gpt-image", (r) =>
    r.fulfill({
      json: envelope({
        key: "images-gpt-image",
        kind: "family",
        section: "images",
        section_name: "图片",
        protocol: "",
        title: "GPT Image",
        summary: "",
        sort: 10,
        hidden: false,
        status: "published",
        version: 1,
        origin: "seed",
        updated_at: "2026-11-14T10:00:00+08:00",
        models: [],
        blocks: [],
        endpoints: TREE.sections[0].nodes[0].endpoints,
        problems: [],
      }),
    }),
  );
}

/** 量「可见的那个 .adp-body」与顶栏：横向溢出必须为 0，且没有控件跑出可视区 */
async function overflow(page: Page) {
  return page.evaluate(() => {
    const bodies = Array.from(document.querySelectorAll<HTMLElement>(".adp-body"));
    const body = bodies.find((b) => b.offsetParent !== null) ?? null;
    const bad: { cls: string; right: number; limit: number }[] = [];
    let bodyOver = 0;
    if (body) {
      bodyOver = body.scrollWidth - body.clientWidth;
      const box = body.getBoundingClientRect();
      for (const el of Array.from(body.querySelectorAll<HTMLElement>("input,select,textarea,button"))) {
        const r = el.getBoundingClientRect();
        if (r.width === 0 && r.height === 0) continue;
        if (r.right > box.right + 1 || r.left < box.left - 1) {
          bad.push({ cls: String(el.className).slice(0, 40), right: Math.round(r.right), limit: Math.round(box.right) });
        }
      }
    }
    const top = document.querySelector<HTMLElement>(".adp-top");
    return {
      docOver: document.documentElement.scrollWidth - document.documentElement.clientWidth,
      bodyOver,
      topOver: top ? top.scrollWidth - top.clientWidth : 0,
      bad,
    };
  });
}

test.describe("文档编辑页 · 零溢出", () => {
  for (const width of [1440, 1280, 1024, 768, 375]) {
    test(`${width}px：顶栏与右栏都不横向溢出`, async ({ page }) => {
      await page.setViewportSize({ width, height: 900 });
      await stub(page);
      await page.goto("/admin/docs?node=images-gpt-image&ep=ep-images-generations");
      await expect(page.getByLabel("接口名")).toHaveValue("图片生成");

      // 折叠态先量一次（顶栏与卡片头）
      let m = await overflow(page);
      expect(m.docOver, "整页横向滚动").toBeLessThanOrEqual(0);
      expect(m.topOver, "顶栏横向溢出").toBeLessThanOrEqual(0);

      // 铺开全部字段卡（最坏情况：14 字段 + 24 可选值 + 8 错误码）
      await page.getByRole("button", { name: "全部展开" }).click();
      await expect(page.locator(".adp-fld.on").first()).toBeVisible();

      m = await overflow(page);
      expect(m.docOver, "整页横向滚动").toBeLessThanOrEqual(0);
      expect(m.topOver, "顶栏横向溢出").toBeLessThanOrEqual(0);
      expect(m.bodyOver, "右栏内部横向滚动").toBeLessThanOrEqual(0);
      expect(m.bad, `跑出右栏可视区的控件：${JSON.stringify(m.bad)}`).toHaveLength(0);

      // 标题不许被压成「图…」：徽标与动作图标化都要给标题让路
      const title = await page
        .locator(".adp-top__title")
        .evaluate((el) => ({ scroll: el.scrollWidth, client: el.clientWidth, text: el.textContent }));
      expect(title.text, "标题存在").toBeTruthy();
      expect(title.scroll, `标题被截断（${title.text}）`).toBeLessThanOrEqual(title.client + 1);
    });
  }

  test("页签是一行：图标与文字同一基线，胶囊不被撑成正圆", async ({ page }) => {
    await stub(page);
    await page.goto("/admin/docs?node=images-gpt-image&ep=ep-images-generations");
    await expect(page.locator(".adp-top").getByRole("tablist")).toBeVisible();

    /**
     * 曾经的事故：`<Tabs>` 加了 `icon`，但 `.tab` 仍是浏览器默认的块级按钮，
     * 而 lucide 的 `<svg>` 是 `display:block` ⇒ 图标独占一行、文字掉到第二行，
     * 胶囊被撑成 56×47 的正圆，页签看起来像两个歪掉的圆按钮。
     * 静态门禁看不出来（类名都在、样式也都解析成功），只有量几何才能发现。
     */
    const boxes = await page.locator(".adp-top .tab").evaluateAll((els) =>
      els.map((el) => {
        const icon = el.querySelector("svg")?.getBoundingClientRect();
        const txt = el.querySelector(".tab__txt")?.getBoundingClientRect();
        const r = el.getBoundingClientRect();
        return {
          display: getComputedStyle(el).display,
          w: Math.round(r.width),
          h: Math.round(r.height),
          // 图标与文字的垂直中心差：同一行 ⇒ ≈0
          dy: icon && txt ? Math.round(Math.abs(icon.top + icon.height / 2 - (txt.top + txt.height / 2))) : -1,
          // 文字必须在图标右侧（竖排时是下方 ⇒ 左边更靠左）
          rightOfIcon: icon && txt ? txt.left >= icon.right - 1 : false,
        };
      }),
    );

    expect(boxes).toHaveLength(2);
    for (const b of boxes) {
      // 计算值可能是 flex（按钮作为 flex item 会被块级化），只要是 flex 容器即可
      expect(b.display, "页签内部布局必须是 flex 行").toContain("flex");
      expect(b.h, "页签高度（两行时会到 47）").toBeLessThanOrEqual(36);
      expect(b.w, "页签宽度（正圆时是 56）").toBeGreaterThan(b.h);
      expect(b.dy, "取不到图标或文字").toBeGreaterThanOrEqual(0);
      expect(b.dy <= 2, "图标与文字不在同一基线").toBe(true);
      expect(b.rightOfIcon, "文字应在图标右侧，而不是下方").toBe(true);
    }
  });

  test("带图标的控件必须自己排成一行（不靠 svg 的 display:block 撞运气）", async ({ page }) => {
    await stub(page);
    await page.goto("/admin/docs?node=images-gpt-image&ep=ep-images-generations");
    await expect(page.getByLabel("接口名")).toHaveValue("图片生成");

    /**
     * 通用判据：任何元素只要**直接**包着 `<svg>`，就必须自己是 flex/grid 容器。
     * lucide 的 svg 是 `display:block`，落在块级容器里会把文字挤到下一行 ——
     * 页签那次事故（胶囊被撑成正圆）就是这一条没守住，其它按钮只是碰巧用了 flex。
     */
    const scan = async () =>
      page.evaluate(() => {
        const roots = [
          document.querySelector(".adp-top"),
          document.querySelector(".adp-rail__list"),
          document.querySelector(".adp-body"),
          document.querySelector(".d-embed"),
        ];
        const bad: { tag: string; cls: string; display: string }[] = [];
        for (const root of roots) {
          if (!root) continue;
          for (const el of Array.from(root.querySelectorAll("*"))) {
            if (!el.querySelector(":scope > svg")) continue;
            const display = getComputedStyle(el).display;
            // 不渲染的元素不参与排版（行尾按钮默认 display:none，hover 才出现），
            // 它们的 flex 由用例末尾的 hover 复扫负责。
            if (display === "none") continue;
            if (!["flex", "inline-flex", "grid", "inline-grid"].includes(display)) {
              bad.push({ tag: el.tagName, cls: String(el.className).slice(0, 50), display });
            }
          }
        }
        return bad;
      });

    expect(await scan(), "编辑页里有元素把 svg 当块级用").toHaveLength(0);
    // 行尾按钮默认不渲染（hover 才出现）：放出来再扫一遍，否则它永远躲过这一条
    await page.locator(".adp-rail .adp-row").first().hover();
    expect(await scan(), "hover 后出现的按钮把 svg 当块级用").toHaveLength(0);
    await page.getByRole("tab", { name: "预览" }).click();
    await expect(page.getByTestId("docs-embed")).toBeVisible();
    expect(await scan(), "预览页里有元素把 svg 当块级用").toHaveLength(0);
  });

  test("左栏是级联：每行都有箭头列，文字列一级一个台阶，行内不溢出", async ({ page }) => {
    await stub(page);

    // 三个板块 × 多节点：只有多级同屏才量得出"阶梯"（单块单节点时随便排都能对上）
    const mkEp = (key: string, name: string, method = "POST") => ({
      key,
      name,
      method,
      path: `/v1/${key}`,
      badge: "",
      status: "published",
      hidden: false,
      sort: 10,
      version: 1,
      bound: true,
    });
    const mkNode = (key: string, title: string, eps: unknown[]) => ({
      ...TREE.sections[0].nodes[0],
      key,
      title,
      endpoints: eps,
    });
    await page.route("**/api/admin/docs/tree", (r) =>
      r.fulfill({
        json: envelope({
          ...TREE,
          sections: [
            TREE.sections[0],
            {
              ...TREE.sections[0],
              key: "videos",
              name: "视频",
              sort: 20,
              nodes: [
                mkNode("videos-kling", "KLING", [mkEp("video-submit", "创建视频任务")]),
                mkNode("videos-seedance", "SEEDANCE", [mkEp("video-seedance", "Seedance 任务")]),
              ],
            },
            {
              ...TREE.sections[0],
              key: "audio",
              name: "音频",
              sort: 30,
              nodes: [mkNode("audio-whisper", "Whisper", [mkEp("audio-transcribe", "转写", "GET")])],
            },
          ],
        }),
      }),
    );

    await page.goto("/admin/docs?node=images-gpt-image&ep=ep-images-generations");
    await expect(page.getByLabel("接口名")).toHaveValue("图片生成");

    const m = await page.evaluate(() => {
      const list = document.querySelector<HTMLElement>(".adp-rail__list")!;
      const rows = Array.from(list.querySelectorAll<HTMLElement>(".adp-row")).map((row) => {
        const label = row.querySelector<HTMLElement>(".adp-row__label")!;
        const caret = row.querySelector<HTMLElement>(".adp-row__caret");
        return {
          level: row.dataset.level ?? "",
          left: Math.round(label.getBoundingClientRect().left),
          // 箭头槽必须每行都在（叶子是 .adp-caret--leaf 的占位，宽 20）
          caret: caret ? Math.round(caret.getBoundingClientRect().width) : 0,
          over: row.scrollWidth - row.clientWidth,
        };
      });
      return {
        rows,
        listOver: list.scrollWidth - list.clientWidth,
        width: list.clientWidth,
      };
    });

    // 三级都在屏上，否则量不到阶梯
    expect(new Set(m.rows.map((r) => r.level))).toEqual(new Set(["1", "2", "3"]));
    expect(m.width).toBeGreaterThan(200);

    // ① 每一行都有箭头列（一级/二级是按钮，三级是等宽占位）
    for (const r of m.rows) {
      expect(r.caret, `第 ${r.level} 级缺箭头列`).toBe(20);
      expect(r.over, `第 ${r.level} 级行内溢出`).toBeLessThanOrEqual(0);
    }
    // ② 左栏自身不横向滚动
    expect(m.listOver, "左栏横向滚动").toBeLessThanOrEqual(0);

    // ③ 文字列成阶梯：58 / 76 / 108（缩进差 ≥12，且严格递增）
    const first = (lvl: string) => m.rows.find((r) => r.level === lvl)!.left;
    expect(first("2") - first("1"), "二级没有缩进").toBeGreaterThanOrEqual(12);
    expect(first("3") - first("2"), "三级没有缩进").toBeGreaterThanOrEqual(12);
    // 同一级的所有行必须对齐（否则"阶梯"只是碰巧）
    for (const lvl of ["1", "2", "3"]) {
      const xs = m.rows.filter((r) => r.level === lvl).map((r) => r.left);
      expect(new Set(xs).size, `第 ${lvl} 级文字没对齐`).toBe(1);
    }
  });

  test("页签在顶栏里：同一个 header，且没有独立的页签条", async ({ page }) => {
    await stub(page);
    await page.goto("/admin/docs?node=images-gpt-image&ep=ep-images-generations");

    // tablist 必须在顶栏内（历史实现是顶栏下面单独一行，常驻 56px）
    await expect(page.locator(".adp-top").getByRole("tablist")).toBeVisible();
    await expect(page.locator(".adp-main .page-tabs-bar")).toHaveCount(0);

    // 顶栏只有一行：高度不超过 60px（旧实现 63 + 56 = 119）
    const h = await page.locator(".adp-top").evaluate((el) => Math.round(el.getBoundingClientRect().height));
    expect(h).toBeLessThanOrEqual(60);

    // 切到预览，顶栏高度不变（不会多出一条）
    await page.getByRole("tab", { name: "预览" }).click();
    await expect(page.getByTestId("docs-embed")).toBeVisible();
    const h2 = await page.locator(".adp-top").evaluate((el) => Math.round(el.getBoundingClientRect().height));
    expect(h2).toBe(h);
  });

  test("预览区有左内边距：正文不贴栏边", async ({ page }) => {
    await stub(page);
    await page.goto("/admin/docs?node=images-gpt-image&ep=ep-images-generations");
    await page.getByRole("tab", { name: "预览" }).click();
    await expect(page.getByTestId("docs-embed")).toBeVisible();

    // `.d-embed` 是**带内边距的盒子**（border-box 从栏边开始），所以要量内容左缘：
    // 面包屑（.d-crumb）是正文第一个块。
    const gap = await page.evaluate(() => {
      const main = document.querySelector(".adp-main")!.getBoundingClientRect();
      const first = document.querySelector(".d-embed .d-crumb, .d-embed > *")!.getBoundingClientRect();
      return Math.round(first.left - main.left);
    });
    expect(gap).toBeGreaterThanOrEqual(16);
  });
});
