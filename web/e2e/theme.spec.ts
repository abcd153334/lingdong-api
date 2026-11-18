/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test } from "@playwright/test";
import { seedAuth } from "./helpers";

test.describe("主题系统", () => {
  test("切换深色：写入 html[data-theme] 与 localStorage，刷新后保持", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await page.goto("/home");
    await expect(page.locator("html")).toHaveAttribute("data-theme", "light");

    await page.getByRole("button", { name: /深色主题/ }).click();
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    expect(await page.evaluate(() => localStorage.getItem("ld-theme"))).toBe("dark");

    await page.reload();
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    // 深色下背景令牌生效（卡片/文字对比度切换）
    const bg = await page.evaluate(
      () => getComputedStyle(document.body).backgroundColor,
    );
    expect(bg).toBe("rgb(11, 13, 18)");

    await page.getByRole("button", { name: /浅色主题/ }).click();
    await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
    expect(await page.evaluate(() => localStorage.getItem("ld-theme"))).toBe("light");
  });

  test("滚动条自绘样式与 color-scheme 一起跟随主题", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await page.goto("/home");

    // 伪元素的计算值可以读（即使 headless 默认带了 --hide-scrollbars：
    // 那只是不画出来，样式仍然参与层叠）。
    const probe = () =>
      page.evaluate(() => {
        const de = document.documentElement;
        const body = document.body;
        const cs = getComputedStyle(de);
        // 内部容器用 `*` 那套伪元素（10px、占位是对的）；页面级那一条已被
        // `html::-webkit-scrollbar{width:0}` 改成 overlay，宽度必须读到 0。
        const bar = getComputedStyle(body, "::-webkit-scrollbar");
        const thumb = getComputedStyle(body, "::-webkit-scrollbar-thumb");
        const track = getComputedStyle(body, "::-webkit-scrollbar-track");
        const pageScrollbarWidth = getComputedStyle(de).scrollbarWidth;
        const dot = document.querySelector<HTMLElement>(".pg-sb-thumb");
        return {
          scheme: cs.colorScheme,
          gutter: cs.scrollbarGutter,
          width: bar.width,
          height: bar.height,
          thumb: thumb.backgroundColor,
          track: track.backgroundColor,
          pageScrollbarWidth,
          // 自绘页面条的拇指颜色必须与原生同一令牌，否则两处会随主题漂移
          dot: dot ? getComputedStyle(dot).backgroundColor : null,
        };
      });

    const light = await probe();
    // color-scheme 写在 :root 上才管得到整棵树（日期弹层、resize 手柄、滚动条）
    expect(light.scheme).toBe("light");
    // 必须是浏览器默认 `auto`（有滚动条才占位）。写成 `stable` 会让没有滚动条的
    // 页面也恒定少一个滚动条宽 —— 顶栏右侧凭空出现一条空白，见 scrollbar.spec.ts。
    expect(light.gutter).toBe("auto");
    expect(light.width).toBe("10px");
    // 横向条必须同样被定义：只写 width 时这里会是 UA 默认（约 15px）
    expect(light.height).toBe("10px");
    // 页面级那一条必须走 overlay：`scrollbar-width: none` 才同时去掉绘制与占位。
    // 用 `::-webkit-scrollbar{width:0}` 不行 —— computed 是 0，但 clientWidth 仍少 10。
    expect(light.pageScrollbarWidth, "页面滚动条又变成占位型了").toBe("none");
    expect(light.thumb).toBe("rgba(100, 116, 139, 0.32)");
    expect(light.track).toBe("rgba(0, 0, 0, 0)");
    // 自绘条与原生同色：颜色只认 --sb-thumb 一处，深色切换时自动跟随
    expect(light.dot).toBe(light.thumb);

    await page.getByRole("button", { name: /深色主题/ }).click();
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");

    const dark = await probe();
    // 沿用亮色 thumb 会在深底上变成一条突兀的灰白条，所以必须整套换色
    expect(dark.scheme).toBe("dark");
    expect(dark.thumb).toBe("rgba(148, 163, 184, 0.28)");
    expect(dark.track).toBe("rgba(0, 0, 0, 0)");
    // 深色下自绘条必须跟着换色（沿用亮色会在深底上变成一条突兀的灰白条）
    expect(dark.dot).toBe(dark.thumb);
  });

  test("管理台与门户共用同一主题作者，跨端一致", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await page.goto("/home");
    await page.getByRole("button", { name: /深色主题/ }).click();
    await page.goto("/admin/dashboard");
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    // 用自动重试的 toHaveCSS 断言深色卡片底色：一次性 evaluate 会在卡片重建的空窗期读到空值
    await expect(page.locator(".card").first()).toHaveCSS("background-color", "rgb(20, 24, 36)");
  });
});
