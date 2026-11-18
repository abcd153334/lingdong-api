/** 站点公告弹层契约：铃铛按钮、尖角几何、整组已读、无障碍与移动端；门户与管理台同一实现 */
import { expect, test, type Page } from "@playwright/test";
import { envelope, expectNoHScroll, seedAuth } from "./helpers";

/** 三条公告：两条未读（其中一条带补充说明）、一条已读 */
const ANNOUNCEMENTS = {
  enabled: true,
  items: [
    {
      id: 3,
      content: "**网关维护** 10-22 02:00-04:00 期间，部分请求可能返回 503，请重试。",
      publishDate: "2026-10-20T09:00:00+08:00",
      type: "warning",
      extra: "维护窗口内不计费失败请求",
      read: false,
    },
    {
      id: 2,
      content: "新模型 `claude-sonnet-4` 已上线模型广场，支持 200K 上下文与缓存计费。",
      publishDate: "2026-10-18T09:00:00+08:00",
      type: "success",
      extra: "",
      read: false,
    },
    {
      id: 1,
      content: "充值满 100 元赠送 10% 额度，活动截止 10-31。",
      publishDate: "2026-10-15T10:00:00+08:00",
      type: "default",
      extra: "",
      read: true,
    },
  ],
};

interface StubState {
  posts: number;
  /** 每次 POST 的原始请求体（null 表示没带体） */
  bodies: (string | null)[];
}

/**
 * 打桩 GET 与 POST。
 *
 * POST 响应与 GET **同形状**（enabled + 全部已读的 items）：前端拿它整份替换缓存，
 * 服务端也不再单独回 unread —— 桩件若回 `{unread:0}`，页面会读到 `items === undefined`
 * 然后渲染成空态，而徽标反而「看起来正常」。
 */
async function stubAnnouncements(page: Page, data: unknown = ANNOUNCEMENTS): Promise<StubState> {
  const state: StubState = { posts: 0, bodies: [] };
  await page.route(
    (url) => url.pathname.startsWith("/api/portal/announcements"),
    (route) => {
      if (route.request().method() === "POST") {
        state.posts += 1;
        state.bodies.push(route.request().postData());
        const src = data as { enabled?: boolean; items?: Record<string, unknown>[] };
        const items = (src.items ?? []).map((item) => ({ ...item, read: true }));
        return route.fulfill({ json: envelope({ enabled: src.enabled ?? true, items }) });
      }
      return route.fulfill({ json: envelope(data) });
    },
  );
  return state;
}

// 必须锚定开头：面板打开后，默认类型的条目标签本身就是「公告」，
// 用 /公告/ 会同时命中「关闭公告」和条目按钮（strict mode 直接报错）。
const bellOf = (page: Page) => page.getByRole("button", { name: /^站点公告/ });
const panelOf = (page: Page) => page.getByRole("dialog", { name: "站点公告" });

test.describe("站点公告弹层 · 门户", () => {
  test("铃铛是按钮：点击开弹层、URL 不变，不跳页", async ({ page }) => {
    await seedAuth(page);
    await stubAnnouncements(page);
    await page.goto("/home");

    const bell = bellOf(page);
    await expect(bell).toBeVisible();
    await expect(bell).toHaveAttribute("aria-expanded", "false");
    await expect(bell).toHaveAttribute("aria-haspopup", "dialog");

    await bell.click();
    await expect(panelOf(page)).toBeVisible();
    await expect(page).toHaveURL(/\/home$/);
    await expect(bell).toHaveAttribute("aria-expanded", "true");
    await expect(bell).toHaveAttribute("aria-controls", "notice-panel");
    await expect(panelOf(page).locator(".notice-item")).toHaveCount(3);
  });

  test("打开即整组已读：POST 恰好一次且无请求体，徽标清空但「新」标记留到关闭", async ({ page }) => {
    await seedAuth(page);
    const state = await stubAnnouncements(page);
    await page.goto("/home");

    await expect(bellOf(page).locator(".nt-badge")).toHaveText("2");
    await bellOf(page).click();
    await expect(panelOf(page)).toBeVisible();

    await expect.poll(() => state.posts).toBe(1);
    // 要标记哪些公告由服务端从配置派生：客户端一旦传 keys，就成了第二真相源
    expect(state.bodies[0] || null).toBeNull();

    await expect(bellOf(page).locator(".nt-badge")).toHaveCount(0);
    // 已读请求成功那一帧不能连「新」标记一起抹掉，否则用户看不到哪几条是新的
    await expect(panelOf(page).locator(".notice-item.is-new")).toHaveCount(2);
    await expect(panelOf(page).locator(".ntp-sub")).toContainText("共 3 条");

    // 关闭后重新打开：全部已读，不再提交，也不再有「新」
    await page.keyboard.press("Escape");
    await expect(panelOf(page)).toHaveCount(0);
    await bellOf(page).click();
    await expect(panelOf(page).locator(".notice-item.is-new")).toHaveCount(0);
    await page.waitForTimeout(300);
    expect(state.posts, "已读请求重复提交").toBe(1);
  });

  test("展开/收起：摘要与正文不重复，补充说明仅在展开时出现", async ({ page }) => {
    await seedAuth(page);
    await stubAnnouncements(page);
    await page.goto("/home");
    await bellOf(page).click();

    const first = panelOf(page).locator(".notice-item").first();
    const head = first.locator(".notice-head");
    await expect(head).toHaveAttribute("aria-expanded", "false");
    await expect(first.locator(".notice-body")).toHaveCount(0);

    await head.click();
    await expect(head).toHaveAttribute("aria-expanded", "true");
    await expect(first.locator(".notice-body")).toContainText("维护窗口内不计费失败请求");
    // 展开后摘要隐藏，同一段文字在「可见」元素中只出现一次（隐藏节点仍留在 DOM 中）
    await expect(first.locator(".notice-tx")).toBeHidden();
    const visibleHits = await page
      .getByText("10-22 02:00-04:00")
      .evaluateAll((els) => els.filter((el) => (el as HTMLElement).offsetParent !== null).length);
    expect(visibleHits, "展开后正文与摘要重复渲染").toBe(1);

    await head.click();
    await expect(first.locator(".notice-body")).toHaveCount(0);
    await expect(first.locator(".notice-tx")).toBeVisible();
  });

  test("类型标签按 5 枚举映射（unknown 回退「公告」）", async ({ page }) => {
    await seedAuth(page);
    await stubAnnouncements(page, {
      enabled: true,
      items: [
        {
          id: 5,
          content: "重要变更",
          publishDate: "2026-10-20T09:00:00+08:00",
          type: "error",
          extra: "",
          read: false,
        },
        {
          id: 4,
          content: "进行中的活动",
          publishDate: "2026-10-19T09:00:00+08:00",
          type: "ongoing",
          extra: "",
          read: false,
        },
        {
          id: 3,
          content: "未知类型",
          publishDate: "2026-10-18T09:00:00+08:00",
          type: "notice",
          extra: "",
          read: false,
        },
      ],
    });
    await page.goto("/home");
    await bellOf(page).click();

    const rows = panelOf(page).locator(".notice-item");
    await expect(rows.nth(0).locator(".nt-tag")).toHaveText("重要");
    await expect(rows.nth(1).locator(".nt-tag")).toHaveText("进行中");
    await expect(rows.nth(2).locator(".nt-tag")).toHaveText("公告");
  });

  test("尖角指向铃铛：位置由定位 hook 下发，且落在面板宽度内", async ({ page }) => {
    await seedAuth(page);
    await stubAnnouncements(page);
    await page.goto("/home");
    await bellOf(page).click();
    await expect(panelOf(page)).toBeVisible();

    const readGeo = () =>
      page.evaluate(() => {
        const panel = document.getElementById("notice-panel") as HTMLElement;
        const bell = document.querySelector(
          ".p-right button[aria-haspopup='dialog']",
        ) as HTMLElement;
        const p = panel.getBoundingClientRect();
        const b = bell.getBoundingClientRect();
        return {
          arrowX: Number.parseFloat(getComputedStyle(panel).getPropertyValue("--pop-arrow-x")),
          left: p.left,
          right: p.right,
          bellCenter: b.left + b.width / 2,
          placement: panel.classList.contains("pop-above") ? "above" : "below",
        };
      });

    // 打开动画（ntp-in 0.16s scale .98）期间 rect 会被 transform 带偏几像素，
    // 轮询到几何收敛再断言 —— 顺便证明 hook 的重定位是稳定的。
    await expect
      .poll(async () => {
        const g = await readGeo();
        return Math.abs(g.arrowX - (g.bellCenter - g.left));
      })
      .toBeLessThanOrEqual(2);

    const geo = await readGeo();
    // 尖角中心 = 铃铛中心相对面板左缘的偏移（面板有 1px 边框，容差 2px）
    expect(geo.left).toBeLessThanOrEqual(geo.bellCenter);
    expect(geo.bellCenter).toBeLessThanOrEqual(geo.right);
    // 顶栏铃铛下方空间充足 → 面板在下、尖角在上沿
    expect(geo.placement).toBe("below");
  });

  test("375px：无横向滚动、面板不贴边、高度受限时列表内部滚动", async ({ page }) => {
    await seedAuth(page);
    await stubAnnouncements(page);
    await page.setViewportSize({ width: 375, height: 780 });
    await page.goto("/home");
    await bellOf(page).click();
    await expect(panelOf(page)).toBeVisible();

    await expectNoHScroll(page, "375px 出现横向溢出");
    const box = await page.evaluate(() => {
      const panel = document.getElementById("notice-panel") as HTMLElement;
      const p = panel.getBoundingClientRect();
      const list = panel.querySelector(".ntp-list") as HTMLElement;
      return {
        left: p.left,
        right: p.right,
        width: p.width,
        vw: document.documentElement.getBoundingClientRect().width,
        overflowY: getComputedStyle(list).overflowY,
      };
    });
    expect(box.left).toBeGreaterThanOrEqual(7);
    expect(box.right).toBeLessThanOrEqual(box.vw - 7);
    expect(box.width).toBeLessThanOrEqual(360);
    // 高度受限时靠列表内部滚动，而不是把面板顶出视口
    expect(box.overflowY).toBe("auto");
  });

  test("接口失败：显示失败态与重试，绝不说成「暂无公告」", async ({ page }) => {
    await seedAuth(page);
    await page.route("**/api/portal/announcements**", (route) =>
      route.fulfill({ status: 500, json: envelope(null, 500, "内部错误") }),
    );
    await page.goto("/home");
    await bellOf(page).click();

    await expect(panelOf(page)).toContainText("公告加载失败");
    await expect(panelOf(page)).not.toContainText("暂无公告");

    // 重试：重新注册为成功，点重试后列表出现
    await stubAnnouncements(page);
    await panelOf(page).getByRole("button", { name: "重试" }).click();
    await expect(panelOf(page).locator(".notice-item")).toHaveCount(3);
  });

  test("未启用：铃铛仍在、无徽标，面板给中性空态（不暴露管理端配置）", async ({ page }) => {
    await seedAuth(page);
    await stubAnnouncements(page, { enabled: false, items: [] });
    await page.goto("/home");

    const bell = page.getByRole("button", { name: "站点公告" });
    await expect(bell).toBeVisible();
    await expect(bell.locator(".nt-badge")).toHaveCount(0);

    await bell.click();
    await expect(panelOf(page)).toContainText("暂无公告");
    await expect(panelOf(page)).not.toContainText("未开启");
  });

  test("关闭语义：点外部关闭、Esc 关闭并回焦铃铛、Tab 在面板内循环", async ({ page }) => {
    await seedAuth(page);
    await stubAnnouncements(page);
    await page.goto("/home");

    // 点外部
    await bellOf(page).click();
    await expect(panelOf(page)).toBeVisible();
    await page.locator("h1").first().click();
    await expect(panelOf(page)).toHaveCount(0);

    // Esc：焦点在面板内（打开即聚焦），关闭后回到铃铛
    await bellOf(page).click();
    await expect(panelOf(page)).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(panelOf(page)).toHaveCount(0);
    await expect(bellOf(page)).toBeFocused();

    // Tab 不能把面板关了（默认 Popover 语义是「Tab 即关闭」，公告面板必须 trap）
    await bellOf(page).click();
    await expect(panelOf(page)).toBeVisible();
    for (let i = 0; i < 8; i += 1) await page.keyboard.press("Tab");
    await expect(panelOf(page)).toBeVisible();
    const inside = await page.evaluate(() =>
      !!document.getElementById("notice-panel")?.contains(document.activeElement),
    );
    expect(inside, "Tab 把焦点漏出了面板").toBe(true);
  });
});

test.describe("站点公告弹层 · 管理台（与门户同一实现）", () => {
  test("管理台铃铛同样是按钮 + 弹层：不跳页、URL 不变、侧栏保留", async ({ page }) => {
    await seedAuth(page, { role: 0, username: "admin" });
    await stubAnnouncements(page);

    await page.goto("/admin/dashboard");
    await expect(page.locator(".a-side")).toBeVisible();
    await expect(page.locator(".p-top")).toHaveCount(0);

    // 不再是 Link：管理台没有「公告整页」，入口与门户是同一个 NoticeBell
    const bell = page.getByRole("button", { name: /^站点公告/ });
    await expect(bell).toBeVisible();
    await expect(page.locator('a[href="/admin/announcements"]')).toHaveCount(0);
    // 桩件 3 条里 2 条未读 ⇒ 徽标 2（与门户同一份未读派生逻辑）
    await expect(page.locator(".nt-badge")).toHaveText("2");

    await bell.click();
    await expect(panelOf(page)).toBeVisible();
    await expect(page).toHaveURL(/\/admin\/dashboard$/);
    await expect(panelOf(page).locator(".notice-item")).toHaveCount(3);
    await expect(page.locator(".a-top .crumb b")).toHaveText("仪表盘");

    // 已读口径一致：打开即整组已读，徽标消失
    await expect(page.locator(".nt-badge")).toHaveCount(0);
  });

  test("旧整页 /admin/announcements 已下线（不再有第二套展示路径）", async ({ page }) => {
    await seedAuth(page, { role: 0, username: "admin" });
    await stubAnnouncements(page);
    await page.goto("/admin/announcements");
    // 路由已删除：落到 404，而不是渲染一份与弹层重复的列表
    await expect(page.locator(".empty .title")).toHaveText("404");
  });
});
