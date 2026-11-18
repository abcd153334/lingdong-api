import { readFileSync } from "node:fs";
import { join } from "node:path";

import { expect, test, type Page } from "@playwright/test";

import { expectNoHScroll, seedAuth } from "./helpers";

/**
 * 对外文档站（v7 重写版）。
 *
 * 数据一律来自 `e2e/fixtures/docs-v7/*.json` —— 真实接口的快照，由
 * `scripts/gen-docs-fixtures.mjs` 生成。手写桩曾经漏了 `notes` 字段，导致整页
 * 崩在 `undefined.length` 上，而用例当时是绿的：快照至少保证「结构等于真实响应」。
 *
 * 断言分两类，缺一不可：
 *  - **几何**：栏宽 280/392、正文 820、标题 23px、代码块深色底、参数行内边距。
 *    只断言 testid 存在是不够的 —— 样式表被删错一段时，页面照样"存在"，
 *    只是塌成一列。
 *  - **行为**：切接口换 URL、折叠树、搜索跳转、节点 URL 重定向、抽屉、调试面板。
 */

const DIR = join(process.cwd(), "e2e", "fixtures", "docs-v7");
const read = <T>(name: string): T => JSON.parse(readFileSync(join(DIR, name), "utf8")) as T;

const TREE = read<Record<string, unknown>>("tree.json");
const NODE = read<Record<string, unknown>>("node.json");
const ENDPOINT = read<Record<string, unknown>>("endpoint.json");
const IDX = read<{ node: string; endpoint: string }>("index.json");
// 第二份快照：图片生成。它守两件只在这一页成立的事（见生成脚本的注释）。
const IMG_EP = read<Record<string, unknown>>("endpoint-image.json");
const IMG_IDX = read<{ node: string; endpoint: string }>("index-image.json");

const EP_URL = `/docs/${IDX.node}/${IDX.endpoint}`;
const NODE_URL = `/docs/${IDX.node}`;

/** 桩：目录 / 节点 / 接口 / 单篇 Markdown / 站点配置。 */
async function stub(
  page: Page,
  opts: { sameOrigin?: boolean; image?: boolean; endpoint?: unknown } = {},
) {
  const tree = { ...TREE };
  if (opts.sameOrigin) {
    // 同源才能测「发送」：真实快照里的 base_url 是公网域名，浏览器会拦。
    tree.base_url = "http://127.0.0.1:5178";
  }
  await page.route("**/api/docs/tree", (r) => r.fulfill({ json: { code: 0, data: tree } }));
  await page.route("**/api/docs/nodes/*/endpoints/*/markdown", (r) =>
    r.fulfill({ contentType: "text/markdown", body: "# 接口文档\n" }),
  );
  await page.route("**/api/docs/nodes/*/endpoints/*", (r) =>
    r.fulfill({ json: { code: 0, data: opts.endpoint ?? (opts.image ? IMG_EP : ENDPOINT) } }),
  );
  await page.route("**/api/docs/nodes/*", (r) => r.fulfill({ json: { code: 0, data: NODE } }));
  await page.route("**/api/portal/config", (r) =>
    r.fulfill({ json: { code: 0, data: { site: { site_name: "灵动 API" }, api: { bases: [] } } } }),
  );
}

/** 取元素的计算外观（几何断言统一走它，避免各用例各写一套 evaluate）。 */
async function look(page: Page, sel: string) {
  return page.evaluate((s) => {
    const el = document.querySelector(s);
    if (!el) return null;
    const r = el.getBoundingClientRect();
    const cs = getComputedStyle(el);
    return {
      x: Math.round(r.x),
      y: Math.round(r.y),
      w: Math.round(r.width),
      h: Math.round(r.height),
      fs: cs.fontSize,
      bg: cs.backgroundColor,
      pad: cs.padding,
      color: cs.color,
      pl: cs.paddingLeft,
    };
  }, sel);
}

test.describe("文档站 · 三栏与几何", () => {
  test.use({ viewport: { width: 1600, height: 900 } });

  test("桌面三栏：280 / 820 / 392，标题 23px，代码块深色，参数行内边距一致", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);
    await expect(page.locator('[data-testid="docs-endpoint-v7"]')).toBeVisible();

    const side = await look(page, ".d-side");
    const main = await look(page, ".d-main-in");
    const rail = await look(page, ".d-rail");
    expect(side?.w).toBe(280);
    expect(main?.w).toBe(820);
    expect(rail?.w).toBe(392);

    expect((await look(page, ".d-ep-h1"))?.fs).toBe("23px");
    // 代码块是深色底：浅色底会让它和正文糊在一起（原型也是深色）
    expect((await look(page, ".d-code"))?.bg).toBe("rgb(30, 31, 38)");
    expect((await look(page, ".d-prow"))?.pad).toBe("11px 14px");
    // 参数名必须与说明文字有色差（不是靠字号区分）；具体色值随主题混色，不写死
    const pname = await look(page, ".d-pname");
    expect(pname?.fs).toBe("12.5px");
    expect(pname?.color).not.toBe((await look(page, ".d-pdesc"))?.color);
  });

  test("正文五块齐全且顺序固定（页头 → 注意事项 → 参数 → 响应 → 错误码）", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);
    await expect(page.locator('[data-block="head"]')).toBeVisible();

    const blocks = await page
      .locator("[data-block]")
      .evaluateAll((els) => els.map((e) => e.getAttribute("data-block")));
    expect(blocks).toEqual(["head", "notes", "params", "response", "errors"]);
  });

  test("右栏落到正文之后：y 大于正文起点，且不与正文重叠", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);
    await expect(page.locator(".d-rail")).toBeVisible();

    const main = await look(page, ".d-main");
    const rail = await look(page, ".d-rail");
    expect(rail!.x).toBeGreaterThanOrEqual(main!.x + main!.w - 1);
  });
});

test.describe("文档站 · 目录与导航", () => {
  test.use({ viewport: { width: 1600, height: 900 } });

  test("目录三层：板块可折叠、节点可折叠、接口带方法徽标", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);

    const side = page.locator(".d-side");
    await expect(side.getByText("灵动 API")).toBeVisible();
    // 当前所在板块与节点默认展开，接口行可见
    await expect(side.locator("a.d-ep").first()).toBeVisible();
    // 方法徽标是独立元素（不拼进标题文本）
    await expect(side.locator(".d-ep .d-mth").first()).toHaveText(/GET|POST|DELETE|PUT|PATCH/);
  });

  test("一页一接口：点目录另一条接口，URL 与标题同时切换", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);
    const links = page.locator(".d-side a.d-ep");
    await expect(links.first()).toBeVisible();
    expect(await links.count()).toBeGreaterThan(1);

    // 侧栏可能同时含「说明页单行链接」，取第二条带方法徽标的接口行
    const target = links.nth(1);
    const href = (await target.getAttribute("href"))!;
    await target.click();
    await expect(page).toHaveURL(new RegExp(`${href.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`));
    await expect(page.locator(".d-ep-h1")).toBeVisible();
  });

  test("节点 URL 重定向到首个接口（不闪 404）", async ({ page }) => {
    await stub(page);
    await page.goto(NODE_URL);
    await expect(page).toHaveURL(new RegExp(`/docs/${IDX.node}/[^/]+$`));
    await expect(page.locator('[data-testid="docs-endpoint-v7"]')).toBeVisible();
    // 重定向全程不应出现「没有这篇文档」
    await expect(page.getByText("没有这篇文档")).toHaveCount(0);
  });

  test("搜索：按 / 聚焦，回车跳到命中的接口", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);
    await expect(page.locator(".d-side")).toBeVisible();
    // 快捷键监听挂在 useEffect 上（paint 之后），首帧按键可能还没绑上
    await expect
      .poll(
        async () => {
          await page.keyboard.press("/");
          return page.evaluate(() => document.activeElement?.tagName ?? "");
        },
        { timeout: 3000 },
      )
      .toBe("INPUT");
    const input = page.locator(".d-side-search input");
    await expect(input).toBeFocused();
    await input.fill("chat");
    await expect(page.locator(".d-sres").first()).toBeVisible();
    await page.keyboard.press("Enter");
    await expect(page.locator(".d-side-search input")).toHaveValue("");
  });
});

test.describe("文档站 · 正文内容", () => {
  test.use({ viewport: { width: 1600, height: 900 } });

  test("参数按 in 分组，model 行内嵌模型 chips", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);

    // 参数区**只有**库里那几组：没有代码拼出来的「鉴权卡」。
    // 那张卡的文字后台改不了，是「前台有字、后台找不到输入框」的典型。
    const groups = page.locator('[data-block="params"] .d-pgroup');
    await expect(groups.first()).toBeVisible();
    expect(await groups.count()).toBeGreaterThanOrEqual(1);
    await expect(groups.first()).toContainText("参数");
    await expect(page.locator('[data-block="params"] .d-authline')).toHaveCount(0);

    // model 字段行必须出现模型 chips（可点复制），否则读者不知道该填什么
    const modelRow = page.locator(".d-prow").filter({ has: page.locator(".d-pname", { hasText: /^model$/ }) });
    await expect(modelRow.locator(".d-mchip").first()).toBeVisible();
  });

  test("注意事项默认收起，展开后能看到内容", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);
    const fold = page.locator('details[data-block="notes"]');
    await expect(fold).toBeVisible();
    await expect(fold).not.toHaveAttribute("open", "");
    await fold.locator("summary").click();
    await expect(fold.locator("li").first()).toBeVisible();
  });

  test("响应字段是树：折叠后子行消失，展开后回来", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);

    const tree = page.locator('[data-block="response"] .d-rtree').first();
    await expect(tree).toBeVisible();
    const before = await tree.locator(".d-rrow").count();
    expect(before).toBeGreaterThan(1);

    const branch = tree.locator(".d-rrow").filter({ has: page.locator("button.tri") }).first();
    await branch.locator("button.tri").click();
    expect(await tree.locator(".d-rrow").count()).toBeLessThan(before);
    await branch.locator("button.tri").click();
    expect(await tree.locator(".d-rrow").count()).toBe(before);
  });

  test("右栏：语言页签 + 响应示例 + 复制/下载 Markdown", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);

    const rail = page.locator(".d-rail");
    await expect(rail).toContainText("请求示例");
    await expect(rail.locator(".d-tabs button", { hasText: "Python" })).toBeVisible();
    await rail.locator(".d-tabs button", { hasText: "Python" }).click();
    await expect(rail.locator(".d-code").first()).toBeVisible();
    await expect(rail).toContainText("响应示例");

    await expect(page.getByRole("link", { name: "下载 .md" })).toHaveAttribute(
      "href",
      /\/api\/docs\/nodes\/.+\/endpoints\/.+\/markdown$/,
    );
  });
});

test.describe("文档站 · 在线调试", () => {
  test.use({ viewport: { width: 1600, height: 900 } });

  test("跨域部署：同样提供发送（能不能通由浏览器与后端 CORS 裁决）", async ({ page }) => {
    await stub(page); // base_url 是公网域名，与页面不同源
    await page.goto(EP_URL);
    await page.locator(".d-ep-line").getByRole("button", { name: "调试" }).click();

    await expect(page.locator(".d-rail")).toContainText("在线运行");
    // 前端不再自设同源闸：真正能不能发由浏览器与 LD_ALLOWED_ORIGINS 决定
    await expect(page.locator(".d-rail").getByRole("button", { name: "发送" })).toBeVisible();
    await expect(page.locator(".d-tgate")).toHaveCount(0);
    await expect(page.locator(".d-try-url code")).toContainText("https://lapi.example.com");
  });

  test("发不出去时给可执行的解释（不是一句 Failed to fetch）", async ({ page }) => {
    await stub(page);
    // 模拟浏览器层失败：CORS 未放行 / 断网都表现为 fetch reject
    await page.route("**/v1/**", (r) => r.abort("failed"));

    await page.goto(EP_URL);
    await page.locator(".d-ep-line").getByRole("button", { name: "调试" }).click();
    await page.locator(".d-tinput").fill("sk-test-123");
    await page.locator(".d-rail").getByRole("button", { name: "发送" }).click();

    const box = page.locator(".d-tres");
    await expect(box).toContainText("请求未发出");
    await expect(box).toContainText("LD_ALLOWED_ORIGINS");
  });

  test("填写 Key 发送：初值来自契约示例，请求头与请求体正确", async ({ page }) => {
    await stub(page, { sameOrigin: true });
    let seen: { auth?: string; body?: string; method?: string; ct?: string } = {};
    await page.route("**/v1/**", async (r) => {
      const req = r.request();
      seen = {
        auth: req.headers()["authorization"],
        body: req.postData() ?? undefined,
        method: req.method(),
        ct: req.headers()["content-type"],
      };
      return r.fulfill({ json: { ok: true, id: "chatcmpl-test" } });
    });

    await page.goto(EP_URL);
    await page.locator(".d-ep-line").getByRole("button", { name: "调试" }).click();

    // 初值必须来自 field.example：`—` 不是值（旧实现把它当默认值，chat 永远发不出）
    await expect(page.locator(".d-tcheck .val").first()).toHaveValue("gpt-4o");
    const area = page.locator(".d-tarea").first();
    await expect(area).toBeVisible();
    expect(await area.inputValue()).toContain('"role"');

    await page.locator(".d-tinput").fill("sk-test-123");
    const send = page.locator(".d-rail").getByRole("button", { name: "发送" });
    await expect(send).toBeVisible();
    await send.click();

    await expect(page.locator(".d-tres-h")).toContainText("200");
    expect(seen.auth).toBe("Bearer sk-test-123");
    expect(seen.method).toBe("POST");
    expect(seen.ct).toBe("application/json");

    const body = JSON.parse(seen.body ?? "{}");
    expect(body.model).toBe("gpt-4o");
    expect(Array.isArray(body.messages)).toBe(true);
    // `max_tokens` 的默认值是一句人话（"模型上限"），不得被当成值发出去
    expect(seen.body ?? "").not.toContain("—");
    expect(seen.body ?? "").not.toContain("模型上限");
  });

  test("JSON 请求体：自定义内容原样发出，写坏时不发请求", async ({ page }) => {
    await stub(page, { sameOrigin: true });
    const sent: string[] = [];
    await page.route("**/v1/**", async (r) => {
      sent.push(r.request().postData() ?? "");
      return r.fulfill({ json: { ok: true, id: "chatcmpl-json" } });
    });

    await page.goto(EP_URL);
    await page.locator(".d-ep-line").getByRole("button", { name: "调试" }).click();

    // 切到 JSON：应该**按当前表单拼好的那份**填进去，而不是一块空白。
    // 这一步刻意在填 Key 之前做 —— 预填只跟表单有关，跟 Key / 路径参数无关。
    await page.locator(".d-tseg").getByRole("button", { name: "JSON" }).click();
    await expect(page.locator(".d-tseg").getByRole("button", { name: "JSON" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    const area = page.locator(".d-tarea");
    await expect(area).toHaveCount(1);
    await expect(area).toHaveValue(/"messages"/);

    await page.locator(".d-tinput").fill("sk-test-123");

    // 自定义：嵌套数组 + 中文 + 数字，全部原样
    const raw =
      '{\n  "model": "gpt-4o",\n  "messages": [{"role": "user", "content": "你好，世界"}],\n  "temperature": 0.3\n}';
    await area.fill(raw);

    // 预览与发送必须读同一份（cURL 里就是这段自定义体）
    await page.locator(".d-tcurl summary").click();
    await expect(page.locator(".d-tcurl pre")).toContainText('"temperature": 0.3');

    await page.locator(".d-rail").getByRole("button", { name: "发送" }).click();
    await expect(page.locator(".d-tres-h")).toContainText("200");
    expect(sent).toHaveLength(1);
    // 逐字相等：原样发送，不被重新序列化
    expect(sent[0]).toBe(raw);

    // 写坏 JSON：面板里当场提示，一个请求都不发
    await area.fill('{"model": ');
    await expect(page.locator(".d-terr")).toContainText("JSON");
    await page.locator(".d-rail").getByRole("button", { name: "发送" }).click();
    await expect(page.locator(".d-tres-h")).toContainText("200"); // 结果区还是上一次的
    expect(sent).toHaveLength(1);
  });

  test("请求预览：Key 脱敏，且不把右栏撑出横向滚动", async ({ page }) => {
    await page.setViewportSize({ width: 1600, height: 900 });
    await stub(page, { sameOrigin: true });
    await page.goto(EP_URL);
    await page.locator(".d-ep-line").getByRole("button", { name: "调试" }).click();
    await page.locator(".d-tinput").fill("sk-test-123");

    await page.locator(".d-tcurl summary").click();
    const text = await page.locator(".d-tcurl pre").innerText();
    expect(text).toContain("curl -X POST");
    expect(text).toContain("$LD_API_KEY");
    expect(text).not.toContain("sk-test-123");

    const over = await page
      .locator(".d-rail")
      .evaluate((el) => el.scrollWidth - el.clientWidth);
    expect(over, "请求预览把右栏撑出横向滚动").toBeLessThanOrEqual(0);
  });
});

test.describe("文档站 · 响应式", () => {
  test("1024：侧栏变抽屉、右栏落到正文下方、无横向滚动", async ({ page }) => {
    await page.setViewportSize({ width: 1024, height: 820 });
    await stub(page);
    await page.goto(EP_URL);

    await expect(page.locator(".d-topbar")).toBeVisible();
    const main = await look(page, ".d-main");
    const rail = await look(page, ".d-rail");
    expect(rail!.y).toBeGreaterThan(main!.y + 100);
    await expectNoHScroll(page, "1024 宽出现横向滚动");
  });

  test("390：顶栏可见、标题 20px、抽屉可开可合、无横向滚动", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await stub(page);
    await page.goto(EP_URL);

    await expect(page.locator(".d-topbar")).toBeVisible();
    // 顶栏在**外壳**里，正文还在骨架屏时它就已经可见了 —— 不等标题就量，
    // 量到的是 null（曾经偶发红在这里，与样式无关）。
    await expect(page.locator(".d-ep-h1")).toBeVisible();
    expect((await look(page, ".d-ep-h1"))?.fs).toBe("20px");

    const side = page.locator(".d-side");
    expect((await side.boundingBox())!.x).toBeLessThan(0); // 默认在屏幕外
    await page.locator(".d-burger").click();
    await expect(page.locator(".d-app")).toHaveClass(/side-open/);
    await page.locator(".d-side-x").click();
    await expect(page.locator(".d-app")).not.toHaveClass(/side-open/);

    await expectNoHScroll(page, "390 宽出现横向滚动");
  });
});

test.describe("文档站 · 失败态", () => {
  test("目录 500：给重试而不是白屏", async ({ page }) => {
    await page.route("**/api/docs/tree", (r) => r.fulfill({ status: 500, body: "boom" }));
    await page.route("**/api/portal/config", (r) =>
      r.fulfill({ json: { code: 0, data: { site: { site_name: "灵动 API" }, api: { bases: [] } } } }),
    );
    await page.goto("/docs");
    await expect(page.getByText("文档加载失败")).toBeVisible();
    await expect(page.getByRole("button", { name: "重试" })).toBeVisible();
  });
});

/* ─────────────────────── 整改新增：滚动 / 抽屉 / 侧栏 / 顺序 ─────────────────────── */

test("注意事项逐字来自接口数据（不是代码里的常量）", async ({ page }) => {
  // 曾经的实现从 apidoc 契约读这一段：后台没有输入框，改一句话就要发版。
  // 现在它必须与接口返回的 notes 完全一致 —— 改桩里的值，页面就跟着变。
  const custom = { ...ENDPOINT, notes: ["运营写的甲条", "运营写的乙条"] };
  await stub(page, { endpoint: custom });
  await page.goto(EP_URL);

  const box = page.locator('[data-block="notes"]');
  await expect(box).toBeVisible();
  await expect(box).toContainText("运营写的甲条");
  await expect(box).toContainText("运营写的乙条");
});

test.describe("文档站 · 三栏各自滚动", () => {
  // 矮视口：保证正文与右栏都一定有内层滚动，不依赖某篇文档恰好很长
  test.use({ viewport: { width: 1600, height: 700 } });

  test("页面不滚、自绘条隐藏，三栏各滚各的", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);
    await expect(page.locator('[data-testid="docs-endpoint-v7"]')).toBeVisible();

    // ① 布局事实：这正是 PageScrollbar 判 range<=1 的同一个输入
    const delta = await page.evaluate(() => {
      const de = document.documentElement;
      return de.scrollHeight - de.clientHeight;
    });
    expect(delta).toBeLessThanOrEqual(1);

    // ② DOM 事实
    await expect(page.locator(".pg-sb")).toBeHidden();

    // ③ 行为事实：滚正文不动页面，滚右栏不动正文
    await page.locator(".d-main").evaluate((el) => { el.scrollTop = 600; });
    expect(await page.evaluate(() => window.scrollY)).toBe(0);
    const mainTop = await page.locator(".d-main").evaluate((el) => el.scrollTop);
    expect(mainTop).toBeGreaterThan(0);

    await page.locator(".d-rail").evaluate((el) => { el.scrollTop = 200; });
    expect(await page.locator(".d-main").evaluate((el) => el.scrollTop)).toBe(mainTop);
  });

  test("顶栏不在正文里（否则会被正文内边距包住）", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);
    await expect(page.locator(".d-main .d-topbar")).toHaveCount(0);
  });
});

test.describe("文档站 · 抽屉契约", () => {
  test.use({ viewport: { width: 1024, height: 820 } });

  test("关闭态 inert、打开态 dialog + 锁滚，Esc 关闭并归还焦点", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);
    const side = page.locator(".d-side");

    // 关闭态：侧栏在屏外且 inert（Tab 进不去）
    await expect(side).toHaveAttribute("inert", "");
    await expect(side).not.toHaveAttribute("role", "dialog");

    await page.locator(".d-burger").click();
    await expect(page.locator(".d-app")).toHaveClass(/side-open/);
    await expect(side).not.toHaveAttribute("inert", "");
    await expect(side).toHaveAttribute("role", "dialog");
    await expect(side).toHaveAttribute("aria-modal", "true");
    // 背景锁滚由 useOverlayBehavior → lockScroll 负责
    expect(await page.evaluate(() => document.body.style.overflow)).toBe("hidden");

    await page.keyboard.press("Escape");
    await expect(page.locator(".d-app")).not.toHaveClass(/side-open/);
    expect(await page.evaluate(() => document.body.style.overflow)).toBe("");
    await expect(page.locator(".d-burger")).toBeFocused();
  });

  test("顶栏全宽、遮罩盖在抽屉之下但在自绘条之上", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);
    await page.locator(".d-burger").click();

    const top = await look(page, ".d-topbar");
    expect(top?.w).toBe(1024);
    expect(top?.x).toBe(0);

    const z = await page.evaluate(() => ({
      scrim: Number(getComputedStyle(document.querySelector(".d-scrim")!).zIndex),
      side: Number(getComputedStyle(document.querySelector(".d-side")!).zIndex),
      // .pg-sb 是 45（components.css），遮罩必须高于它，否则 768–1100 区间滚动条压在遮罩上
      pgSb: Number(getComputedStyle(document.querySelector(".pg-sb")!).zIndex),
    }));
    expect(z.scrim).toBeGreaterThan(z.pgSb);
    expect(z.scrim).toBeLessThan(z.side);
  });
});

test.describe("文档站 · 390 抽屉", () => {
  test.use({ viewport: { width: 390, height: 844 } });

  test("窄屏抽屉宽度受 86vw 约束，点遮罩可关", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);
    await expect(page.locator(".d-side")).toHaveCSS("width", "280px");

    await page.locator(".d-burger").click();
    await expect(page.locator(".d-app")).toHaveClass(/side-open/);
    await page.locator(".d-scrim").click({ position: { x: 360, y: 400 } });
    await expect(page.locator(".d-app")).not.toHaveClass(/side-open/);
  });
});

test.describe("文档站 · 品牌与页脚", () => {
  test.use({ viewport: { width: 1600, height: 700 } });

  test("品牌回门户；管理员多一条「管理后台」", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);

    const brand = page.locator(".d-side-brand a.d-brand-home");
    await expect(brand).toHaveAttribute("href", "/home");
    await expect(page.locator(".d-brand-admin")).toHaveCount(0);

    // 管理员：seedAuth 桩 /api/portal/me 返回 role 0
    await seedAuth(page, { role: 0 });
    await stub(page);
    await page.goto(EP_URL);
    await expect(page.locator(".d-brand-admin")).toHaveAttribute("href", "/admin/dashboard");
  });

  test("门户关闭时品牌不可点（给了链接会被守卫弹回登录页）", async ({ page }) => {
    await page.route("**/api/docs/tree", (r) => r.fulfill({ json: { code: 0, data: TREE } }));
    await page.route("**/api/docs/nodes/*/endpoints/*/markdown", (r) =>
      r.fulfill({ contentType: "text/markdown", body: "# 接口文档\n" }),
    );
    await page.route("**/api/docs/nodes/*/endpoints/*", (r) => r.fulfill({ json: { code: 0, data: ENDPOINT } }));
    await page.route("**/api/docs/nodes/*", (r) => r.fulfill({ json: { code: 0, data: NODE } }));
    await page.route("**/api/portal/config", (r) =>
      r.fulfill({
        json: { code: 0, data: { site: { site_name: "灵动 API" }, portal_enabled: false, api: { bases: [] } } },
      }),
    );

    await page.goto(EP_URL);
    await expect(page.locator(".d-side-brand a.d-brand-home")).toHaveCount(0);
    await expect(page.locator(".d-side-brand .d-brand-home")).toBeVisible();
  });

  /**
   * 品牌标不许有底色。
   *
   * 原来铺的是 `background: var(--grad)`（135deg 紫渐变）：有图时透明 PNG / 方形截图
   * 会沿 8px 圆角露出紫边，没图时是一个紫色方块配白字 —— 与这一列「白底 + 1px 细线」
   * 的语言不一致。降级态（无图）用正文字色，不能再是白字（没底就看不见了）。
   */
  test("品牌标没有底色：无渐变、无背景色", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);
    const logo = page.locator(".d-brand-logo");
    const bg = await logo.evaluate((el) => {
      const cs = getComputedStyle(el);
      return { image: cs.backgroundImage, color: cs.backgroundColor };
    });
    expect(bg.image).toBe("none");
    expect(bg.color).toBe("rgba(0, 0, 0, 0)");

    // 降级首字必须看得见：颜色不能是白/近白
    const rgb = await logo.evaluate((el) => {
      const m = getComputedStyle(el).color.match(/\d+/g) ?? [];
      return m.slice(0, 3).map(Number);
    });
    const [r, g, b] = rgb;
    expect(Math.min(r, g, b)).toBeLessThan(200);
  });

  test("下载按钮常驻：列表滚到底页脚不动", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);
    const foot = page.locator(".d-side-foot");
    await expect(foot).toBeVisible();

    const before = (await foot.boundingBox())!;
    await page.locator(".d-side-list").evaluate((el) => { el.scrollTop = el.scrollHeight; });
    const after = (await foot.boundingBox())!;

    expect(after.y).toBe(before.y);
    const side = (await page.locator(".d-side").boundingBox())!;
    expect(after.y + after.height).toBeLessThanOrEqual(side.y + side.height + 1);

    // 按钮形态：有边框、有图标
    const dl = page.locator(".d-side-foot a");
    await expect(dl).toHaveCSS("height", "32px");
    await expect(dl.locator("svg")).toHaveCount(1);
  });
});

test.describe("文档站 · 搜索", () => {
  test.use({ viewport: { width: 1600, height: 900 } });

  test("输入框与菜单行背景左缘齐平，快捷键提示为 /", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);
    await expect(page.locator(".d-side-search input")).toBeVisible();

    const side = (await page.locator(".d-side").boundingBox())!;
    const input = (await page.locator(".d-side-search input").boundingBox())!;
    expect(Math.round(input.x - side.x)).toBe(10);

    await expect(page.locator(".d-search-kbd")).toHaveText("/");
  });

  /**
   * 图标 / 尺寸 / 对齐 / 内边距四件事一起守。
   *
   * 这里曾经是「一个 `⌕` 文字字形 + 按 33px 盒硬算的 top:9px」，而全局控件规则
   * `input[type="search"]{height:var(--ctl-h);padding:0 13px}` 与 `.d-side-search input`
   * 同权重、后加载 ⇒ 搜索框实际 42px、文字从 13px 起，正好压在图标下面。
   */
  test("搜索框：SVG 图标垂直居中、文字不被图标压住", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);

    const input = page.locator(".d-side-search input");
    const ico = page.locator(".d-search-ico");
    const svg = ico.locator("svg");
    await expect(svg).toHaveCount(1);
    // 图标不是文字：文字字形（⌕）会随字体变大小、且复制时混进输入内容
    await expect(ico).toHaveText("");

    const ib = (await input.boundingBox())!;
    const gb = (await svg.boundingBox())!;
    expect(Math.abs(gb.y + gb.height / 2 - (ib.y + ib.height / 2)), "图标没有垂直居中").toBeLessThanOrEqual(1.5);
    // docs 自己的紧凑档 34（与 .d-sec-h 同高），不是全局 --ctl-h 的 42/44
    expect(Math.round(ib.height)).toBe(34);

    // 文字起点必须在图标右侧留白之后：padding-left ≥ 图标右缘 + 4
    const pl = await input.evaluate((el) => parseFloat(getComputedStyle(el).paddingLeft));
    const iconRight = gb.x + gb.width - ib.x;
    expect(pl, "左内边距小于图标占位，文字会压到图标").toBeGreaterThanOrEqual(iconRight + 4);

    // 快捷键胶囊完整落在输入框内，且不被右侧 padding 挤出去
    const kb = (await page.locator(".d-search-kbd").boundingBox())!;
    expect(kb.x + kb.width).toBeLessThanOrEqual(ib.x + ib.width - 1);
    expect(Math.abs(kb.y + kb.height / 2 - (ib.y + ib.height / 2))).toBeLessThanOrEqual(1.5);
  });

  test("点正文关闭搜索下拉（拖选文字松手在框外不应误关）", async ({ page }) => {
    await stub(page);
    await page.goto(EP_URL);
    const input = page.locator(".d-side-search input");
    await input.click();
    await input.fill("chat");
    await expect(page.locator(".d-search-res")).toBeVisible();
    await page.locator(".d-main").click({ position: { x: 20, y: 400 } });
    await expect(page.locator(".d-search-res")).toHaveCount(0);
  });
});

test("板块顺序与名称：开始使用 → 通用接口 → 语言 → 图片 → 视频 → 音频 → 音乐", async ({ page }) => {
  await stub(page);
  await page.goto(EP_URL);
  await expect(page.locator(".d-sec-h").first()).toBeVisible();
  expect(await page.locator(".d-sec-h").allTextContents()).toEqual([
    "开始使用",
    "通用接口",
    "语言",
    "图片",
    "视频",
    "音频",
    "音乐",
  ]);
});

test("390：错误码表在卡片内横滚，整页不横向滚动", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await stub(page);
  await page.goto(EP_URL);

  const wrap = page.locator(".d-tblwrap");
  await expect(wrap).toBeVisible();
  const scrollable = await wrap.evaluate((el) => el.scrollWidth > el.clientWidth);
  expect(scrollable).toBe(true);
  await expectNoHScroll(page, "390 宽下错误码表把整页撑出横向滚动");
});

test("点「调试」后右栏回到顶部（不带着示例的滚动位置）", async ({ page }) => {
  await page.setViewportSize({ width: 1600, height: 700 });
  await stub(page);
  await page.goto(EP_URL);

  await page.locator(".d-rail").evaluate((el) => { el.scrollTop = 400; });
  expect(await page.locator(".d-rail").evaluate((el) => el.scrollTop)).toBeGreaterThan(0);

  await page.locator(".d-ep-line").getByRole("button", { name: "调试" }).click();
  await expect(page.locator(".d-rail")).toContainText("在线运行");
  expect(await page.locator(".d-rail").evaluate((el) => el.scrollTop)).toBe(0);
});

/* ─────────────────── 字段的模型分组说明（size / quality） ─────────────────── */

test.describe("文档站 · 字段取值说明", () => {
  const URL_IMG = `/docs/${IMG_IDX.node}/${IMG_IDX.endpoint}`;

  test.use({ viewport: { width: 1600, height: 1000 } });

  test("size 的说明就是一段字：约束与比例换算全在同一个字段说明里", async ({ page }) => {
    await stub(page, { image: true });
    await page.goto(URL_IMG);
    await expect(page.locator('[data-testid="docs-endpoint-v7"]')).toBeVisible();

    const sizeRow = page.locator(".d-prow", { has: page.locator(".d-pname", { hasText: /^size$/ }) });
    await expect(sizeRow).toBeVisible();

    // 说明是一段多行文字（后台一个文本框写的），不是单独设计的表格组件。
    // 曾经它是一张结构化参考表 + 一段分组 JSON，后台根本看不见那张表长什么样。
    const desc = sizeRow.locator(".d-pdesc").first();
    for (const want of ["3840px", "倍数", "3:1", "655,360", "8,294,400"]) {
      await expect(desc).toContainText(want);
    }

    // 反引号必须变成真正的 <code>（v7 之前是纯文本，页面上原样露出反引号）。
    const codes = await desc.locator("code").allInnerTexts();
    expect(codes).toContain("3840px");
    expect(codes).toContain("3:1");
    expect(await desc.innerText()).not.toContain("`");

    // 比例换算逐行铺开，不折叠：折叠等于让读者先点一下才知道该怎么传参。
    //
    // 断言「行数 = 文案里出现的比例数」而不是写死 15：各族的比例集不同
    // （官方每家给的档位就不一样），写死数字只会在换族或官方补档时误报，
    // 而它想拦的是"把多行压成一行"。
    const text = await desc.innerText();
    // 每行以「比例 + 像素」开头，至少十几行：压成一段就只剩 1 行。
    const lines = text.split("\n").map((l) => l.trim());
    const ratioLines = lines.filter((l) => /^\d+:\d+\s/.test(l) && /\d+x\d+/.test(l));
    expect(ratioLines.length).toBeGreaterThanOrEqual(10);
    await expect(desc).toContainText("1024x1024");
  });

  test("整族模型下架时，文档一个字都不变", async ({ page }) => {
    await stub(page, { image: true });
    // 覆盖上一层的接口桩：把「适用模型」清空，模拟这一族此刻一个模型都没有。
    await page.route("**/api/docs/nodes/*/endpoints/*", (r) =>
      r.fulfill({ json: { code: 0, data: { ...IMG_EP, models: [] } } }),
    );
    await page.goto(URL_IMG);
    await expect(page.locator('[data-testid="docs-endpoint-v7"]')).toBeVisible();

    // 字段表与说明全在（说明是文字，不依赖任何模型在不在售）。
    const sizeDesc = page
      .locator(".d-prow", { has: page.locator(".d-pname", { hasText: /^size$/ }) })
      .locator(".d-pdesc")
      .first();
    await expect(sizeDesc).toContainText("3840px");
    const qDesc = page
      .locator(".d-prow", { has: page.locator(".d-pname", { hasText: /^quality$/ }) })
      .locator(".d-pdesc")
      .first();
    // 断言"说明文字还在"而不是某一句具体措辞：措辞归后台所有，随时会改。
    await expect(qDesc).not.toBeEmpty();
    await expect(qDesc).toContainText("思考深度");
  });

  test("stream / partial_images 不出现（这一族没有声明流式出图）", async ({ page }) => {
    await stub(page, { image: true });
    await page.goto(URL_IMG);
    await expect(page.locator('[data-testid="docs-endpoint-v7"]')).toBeVisible();

    // 曾经这两个字段写着「开启后响应是 SSE」，而网关对图片端点的 stream 是主动剥掉的。
    await expect(page.locator(".d-pname", { hasText: /^stream$/ })).toHaveCount(0);
    await expect(page.locator(".d-pname", { hasText: /^partial_images$/ })).toHaveCount(0);
  });

  test("quality 的取值来自字段的可选值（low / medium / high），说明里不再自造档位", async ({
    page,
  }) => {
    await stub(page, { image: true });
    await page.goto(URL_IMG);
    const qRow = page.locator(".d-prow", { has: page.locator(".d-pname", { hasText: /^quality$/ }) });
    const desc = qRow.locator(".d-pdesc").first();

    // 取值必须在「可选值」里列出来 —— 那一块来自字段的枚举，是机器可校验的。
    // 曾经它们被写成一句正文（"只支持 low / medium / high"），还自造了 xhigh 档，
    // 而官方只有三档；正文里的取值既无法校验，也不会有 chip。
    const chips = await qRow.locator(".d-pmeta").first().innerText();
    for (const v of ["low", "medium", "high"]) expect(chips).toContain(v);
    expect(chips).not.toContain("xhigh");

    // 说明本身只说"越高质量越好"这类事实，不重复列举取值（一处真相源）。
    await expect(desc).toContainText("思考深度");
  });

});

test("390：长说明在卡片内换行，不把整页撑出横向滚动", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await stub(page, { image: true });
  await page.goto(`/docs/${IMG_IDX.node}/${IMG_IDX.endpoint}`);
  await expect(page.locator('[data-testid="docs-endpoint-v7"]')).toBeVisible();

  const desc = page.locator(".d-prow", { has: page.locator(".d-pname", { hasText: /^size$/ }) }).locator(".d-pdesc").first();
  await expect(desc).toContainText("655,360");
  await expectNoHScroll(page, "390 宽下长说明把整页撑出横向滚动");
});
