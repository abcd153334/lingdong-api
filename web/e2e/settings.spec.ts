/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { E2E_SESSIONS, envelope, PORTAL_CONFIG, seedAuth } from "./helpers";

const ME_BOUND = {
  id: 1,
  username: "alice",
  role: 1,
  status: 1,
  group: "default",
  balance: 123456,
  frozen: 0,
  used: 1000,
  request_count: 7,
  key_count: 1,
  key_count_enabled: 1,
  group_discount: 1_000_000,
  group_configured: false,
  email: "alice@example.com",
  phone: "13800001234",
};

const ME_UNBOUND = { ...ME_BOUND, email: "", phone: "" };

interface StubOptions {
  me?: typeof ME_BOUND;
  /** 发送验证码时后端返回「渠道未启用」（HTTP 400 + 文案） */
  channelDisabled?: boolean;
}

/** 确定性 Mock：/portal/me、/portal/auth/contact/code、/portal/auth/contact/bind */
async function stubPortal(page: Page, options: StubOptions = {}): Promise<void> {
  const me = options.me ?? ME_BOUND;
  await page.route("**/api/portal/**", async (route) => {
    const req = route.request();
    const path = new URL(req.url()).pathname;

    // config 交由 helpers.PORTAL_CONFIG（默认通道开启）桩件处理；本函数注册更晚会优先命中，
    // 故必须 fallback 而不能用 catch-all 的 envelope(null) 吞掉，否则 email/sms_enabled 缺失导致入口禁用。
    // 需模拟「后台关闭通道」的用例，在本函数之后自行注册 config 桩件覆盖即可（后注册者优先）。
    if (path.endsWith("/portal/config")) {
      return route.fallback();
    }
    if (path.endsWith("/portal/me") && req.method() === "GET") {
      return route.fulfill({ json: envelope(me) });
    }
    // 设备列表交给 helpers.E2E_SESSIONS（单一夹具来源）；本函数的 catch-all
    // 会返回 envelope(null)，那样设备卡片只会显示空态。
    if (path.includes("/portal/auth/sessions")) {
      return route.fallback();
    }
    // 二次验证状态同理交给 helpers 的统一桩件（默认未开启）。
    if (path.includes("/portal/auth/mfa/status")) {
      return route.fallback();
    }
    if (path.endsWith("/portal/auth/contact/code")) {
      if (options.channelDisabled) {
        return route.fulfill({
          status: 400,
          json: envelope(null, 400, "该绑定渠道未启用", "bind_channel_disabled"),
        });
      }
      return route.fulfill({ json: envelope({ expires_in: 60 }) });
    }
    if (path.endsWith("/portal/auth/contact/bind")) {
      return route.fulfill({ json: envelope({ email: me.email, phone: me.phone }) });
    }
    return route.fulfill({ json: envelope(null) });
  });
}

const emailRow = (page: Page) => page.locator(".uc-row", { hasText: "绑定邮箱" });
const phoneRow = (page: Page) => page.locator(".uc-row", { hasText: "绑定手机" });

/** 用户中心是弹窗（与管理台「账户设置」同一个组件）：`/settings` 只是它的落地页 */
const dlg = (page: Page) => page.getByRole("dialog", { name: "用户中心" });

/**
 * 进分区才能断言：联系方式在「账户安全」，会话在「登录设备」。
 * 资料分区是默认分区，不需要点。
 */
const openSection = async (page: Page, name: "资料" | "账户安全" | "登录设备") => {
  await expect(dlg(page)).toBeVisible();
  await dlg(page).getByRole("button", { name }).click();
};

test.describe("S2 用户中心 · 绑定与安全", () => {
  test("换绑邮箱：脱敏展示 → 抽屉 → 60s 倒计时 → 换绑成功", async ({ page }) => {
    await seedAuth(page);
    await stubPortal(page);

    await page.goto("/settings");
    await openSection(page, "账户安全");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("用户中心");
    // 受保护邮箱脱敏展示
    await expect(emailRow(page).locator(".rs")).toContainText("a***@example.com");
    await expect(phoneRow(page).locator(".rs")).toContainText("138****1234");

    await emailRow(page).getByRole("button", { name: "换绑" }).click();
    const dialog = page.getByRole("dialog", { name: "换绑邮箱" });
    await expect(dialog).toBeVisible();
    await expect(dialog.getByRole("button", { name: "确认换绑" })).toBeVisible();

    // 目标校验：非法邮箱 → toast，不发送
    await dialog.locator("#bind-target").fill("not-an-email");
    await dialog.getByRole("button", { name: "发送验证码" }).click();
    await expect(page.getByText("邮箱格式不正确")).toBeVisible();

    // 合法目标 → 发送成功 → 倒计时禁用
    await dialog.locator("#bind-target").fill("new@example.com");
    await dialog.getByRole("button", { name: "发送验证码" }).click();
    await expect(page.getByText("验证码已发送")).toBeVisible();
    const sendBtn = dialog.getByRole("button", { name: /重新发送/ });
    await expect(sendBtn).toBeDisabled();
    const first = await sendBtn.textContent();
    // 倒计时真实递减
    await expect
      .poll(async () => sendBtn.textContent(), { timeout: 4000 })
      .not.toBe(first);

    await dialog.locator("#bind-code").fill("246810");
    await dialog.getByRole("button", { name: "确认换绑" }).click();
    await expect(page.getByText("邮箱换绑成功")).toBeVisible();
    await expect(dialog).toBeHidden();
  });

  test("绑定手机（未绑定态）→ 绑定成功", async ({ page }) => {
    await seedAuth(page);
    await stubPortal(page, { me: ME_UNBOUND });

    await page.goto("/settings");
    await openSection(page, "账户安全");
    await expect(emailRow(page).locator(".rs")).toHaveText("未绑定");
    await expect(phoneRow(page).locator(".rs")).toHaveText("未绑定");

    await phoneRow(page).getByRole("button", { name: "绑定" }).click();
    const dialog = page.getByRole("dialog", { name: "绑定手机" });
    await expect(dialog.getByRole("button", { name: "确认绑定" })).toBeVisible();

    await dialog.locator("#bind-target").fill("13900001111");
    await dialog.getByRole("button", { name: "发送验证码" }).click();
    await expect(dialog.getByRole("button", { name: /重新发送/ })).toBeDisabled();

    await dialog.locator("#bind-code").fill("246810");
    await dialog.getByRole("button", { name: "确认绑定" }).click();
    await expect(page.getByText("手机号绑定成功")).toBeVisible();
    await expect(dialog).toBeHidden();
  });

  // 启用态下的「运行期拒绝」路径：入口可点击，但发码接口返回渠道未启用
  test("渠道未启用（运行期拒绝）：入口可点击，发送验证码被拒并提示", async ({ page }) => {
    await seedAuth(page);
    await stubPortal(page, { channelDisabled: true });

    await page.goto("/settings");
    await openSection(page, "账户安全");
    await emailRow(page).getByRole("button", { name: "换绑" }).click();
    const dialog = page.getByRole("dialog", { name: "换绑邮箱" });

    await dialog.locator("#bind-target").fill("new@example.com");
    await dialog.getByRole("button", { name: "发送验证码" }).click();
    await expect(page.getByText("该绑定渠道未启用")).toBeVisible();

    await dialog.getByRole("button", { name: "取消" }).click();
    await expect(dialog).toBeHidden();
  });

  test("用户中心不残留演示文案（含绑定抽屉）", async ({ page }) => {
    await seedAuth(page);
    await stubPortal(page);
    // expect(page) 无 toContainText（Playwright 该断言仅适用于 Locator），故对 body 定位器断言
    const body = page.locator("body");

    await page.goto("/settings");
    await openSection(page, "账户安全");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("用户中心");
    await expect(body).not.toContainText("246810");
    await expect(body).not.toContainText("演示数据");

    // 固定验证码提示原所在处：绑定抽屉
    await emailRow(page).getByRole("button", { name: "换绑" }).click();
    const dialog = page.getByRole("dialog", { name: "换绑邮箱" });
    await expect(dialog).toBeVisible();
    await expect(dialog).not.toContainText("246810");
    await expect(dialog).not.toContainText("演示数据");
  });

  // 禁用态：后台关闭邮件/短信 → 绑定入口文案与按钮直接禁用（不发请求）
  test("S18 配置驱动禁用：后台关闭邮件/短信 → 绑定入口直接禁用（不发请求）", async ({ page }) => {
    await seedAuth(page);
    await stubPortal(page, { me: ME_UNBOUND });
    // 后注册者优先：覆盖 seedAuth 的 config 桩件，模拟管理台关闭两个通道
    await page.route("**/api/portal/config**", (route) =>
      route.fulfill({
        json: envelope({
          ...PORTAL_CONFIG,
          email_enabled: false,
          sms_enabled: false,
        }),
      }),
    );

    await page.goto("/settings");
    await openSection(page, "账户安全");
    await expect(emailRow(page).locator(".rs")).toHaveText("管理员未开启邮件服务，暂时无法绑定");
    await expect(phoneRow(page).locator(".rs")).toHaveText("管理员未开启短信服务，暂时无法绑定");
    await expect(emailRow(page).getByRole("button")).toBeDisabled();
    await expect(phoneRow(page).getByRole("button")).toBeDisabled();
  });

  // 设备列表：本机不可自踢、其它设备可单独撤销、撤销后列表刷新
  test("登录设备：本机标记 + 撤销其它设备后列表刷新", async ({ page }) => {
    await seedAuth(page);
    await stubPortal(page);

    // 有状态桩件（注册晚于 stubPortal → 优先命中）：撤销后只剩当前设备
    let revoked = false;
    let revokeCalls = 0;
    await page.route("**/api/portal/auth/sessions**", (route) => {
      const req = route.request();
      const path = new URL(req.url()).pathname;
      if (req.method() === "POST" && path.endsWith("/revoke")) {
        revokeCalls += 1;
        revoked = true;
        return route.fulfill({ json: envelope(null) });
      }
      return route.fulfill({
        json: envelope({ sessions: revoked ? E2E_SESSIONS.filter((s) => s.current) : E2E_SESSIONS }),
      });
    });

    await page.goto("/settings");
    await openSection(page, "登录设备");
    const card = page.getByTestId("device-list");
    await expect(card).toContainText("共 2 台");
    // 本机那一行标「本机」且没有「退出」按钮（点了等于把自己踢掉却停在页面上）
    await expect(page.getByTestId("device-1")).toContainText("本机");
    await expect(page.getByTestId("device-1").getByRole("button")).toHaveCount(0);
    // 其它设备解析 UA 成可读标签，而不是一整串原始 UA
    await expect(page.getByTestId("device-2")).toContainText("Safari · macOS");
    await expect(page.getByTestId("device-2")).not.toContainText("Mozilla/5.0");

    await page.getByTestId("revoke-2").click();
    await expect(page.getByText("该设备已退出登录")).toBeVisible();
    await expect.poll(() => revokeCalls).toBe(1);
    await expect(page.getByTestId("device-2")).toHaveCount(0);
    await expect(page.getByTestId("device-list")).toContainText("共 1 台");
    // 只剩一台时不再给「退出全部设备」（那等于把仅有的当前设备也踢掉）
    await expect(page.getByTestId("revoke-all-sessions")).toHaveCount(0);
  });

  // 退出全部设备含当前设备 → 必须二次确认 + 本地登出回登录页
  test("两步验证：默认未开启 → 入口跳到绑定页", async ({ page }) => {
    await seedAuth(page);
    await stubPortal(page);
    await page.goto("/settings");
    await openSection(page, "账户安全");

    const card = page.getByTestId("mfa-card");
    await expect(card).toBeVisible();
    await expect(card.getByTestId("mfa-state")).toHaveText("未开启");
    // 弹窗里不该再有「返回账户安全」：那是整页式用户中心的遗留，点它会把弹窗带走
    await expect(dlg(page).getByRole("link", { name: "返回账户安全" })).toHaveCount(0);

    // 绑定要走「二维码 → 输码 → 抄备份码」三步，所以入口是跳页而不是弹窗。
    await card.getByTestId("mfa-enable-entry").click();
    await expect(page).toHaveURL(/\/mfa\/setup$/);
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("两步验证");
  });

  test("两步验证：已开启显示剩余备份码，关闭必须输入动态码", async ({ page }) => {
    await seedAuth(page);
    await stubPortal(page);
    // 已开启 + 只剩 2 个备份码（提示重新生成的那条分支）
    await page.route("**/api/portal/auth/mfa/status", (route) =>
      route.fulfill({ json: envelope({ enabled: true, policy: "optional", backup_codes_left: 2 }) }),
    );
    const posts: { path: string; body: unknown }[] = [];
    await page.route("**/api/portal/auth/mfa/disable", (route) => {
      posts.push({ path: "disable", body: route.request().postDataJSON() });
      return route.fulfill({ json: envelope(null) });
    });

    await page.goto("/settings");
    await openSection(page, "账户安全");
    const card = page.getByTestId("mfa-card");
    await expect(card.getByTestId("mfa-state")).toHaveText("已开启");
    await expect(card.getByTestId("mfa-codes-left")).toHaveText("2");

    // 关闭是降低安全等级：必须先给一次第二因素，不能只凭会话。
    await card.getByRole("button", { name: "关闭两步验证" }).click();
    await expect(card.getByTestId("mfa-confirm")).toBeVisible();
    // 空码不发请求
    await card.getByRole("button", { name: "确认" }).click();
    expect(posts).toHaveLength(0);

    await page.locator("#mfa-confirm-code").fill("123456");
    await card.getByRole("button", { name: "确认" }).click();
    await expect.poll(() => posts.length).toBe(1);
    expect(posts[0].body).toEqual({ code: "123456" });
    // 服务端会作废全部会话（含当前），所以前端必须回登录页
    await expect(page).toHaveURL(/\/login/);
  });

  test("登录设备：退出全部设备需二次确认，成功后回登录页", async ({ page }) => {
    await seedAuth(page);
    await stubPortal(page);

    await page.goto("/settings");
    await openSection(page, "登录设备");
    await page.getByTestId("revoke-all-sessions").click();
    const dialog = page.getByRole("dialog", { name: "退出全部设备" });
    await expect(dialog).toContainText("包括当前设备");

    // 先取消：不能发请求，也不能离开页面
    await dialog.getByRole("button", { name: "取消" }).click();
    await expect(dialog).toBeHidden();
    await expect(page).toHaveURL(/\/settings$/);

    await page.getByTestId("revoke-all-sessions").click();
    await page
      .getByRole("dialog", { name: "退出全部设备" })
      .getByRole("button", { name: "全部退出" })
      .click();
    await expect(page).toHaveURL(/\/login$/);
  });

  /**
   * 高度不变量：`.dialog` 是 fixed + translate 的自高度垂直居中盒，内容一变高就会
   * 上下各弹半格。外壳定高 + 右栏滚动之后，切分区不许改变弹窗的 height/y。
   * （jsdom 不做布局，这条只能由 Playwright 守）
   */
  test("切换三个分区，弹窗尺寸恒定", async ({ page }) => {
    await seedAuth(page);
    await stubPortal(page);
    await page.goto("/settings");

    const dialog = dlg(page);
    await expect(dialog).toBeVisible();
    // 等入场动画（opacity 0→1 / scale .95→1），否则量到的是中间帧
    await expect(dialog).toHaveCSS("opacity", "1");

    const base = (await dialog.boundingBox())!;
    for (const name of ["账户安全", "登录设备", "资料"] as const) {
      await openSection(page, name);
      const box = (await dialog.boundingBox())!;
      expect(Math.abs(box.height - base.height), `${name} 高度变了`).toBeLessThanOrEqual(1);
      expect(Math.abs(box.y - base.y), `${name} 上边缘变了`).toBeLessThanOrEqual(1);
    }
  });
});

/**
 * 头像：默认图随产物打包（`dist/assets/avatar-default-*.png`），上传后顶栏必须**立刻**变。
 *
 * 顶栏读的是会话 store 快照而不是 `useMeQuery` 的数据 —— 只 `invalidateQueries(me)` 的话
 * 会出现「用户中心换了头像，右上角还是旧的」，所以这条断言必须同时看两处。
 */
test.describe("S2 用户中心 · 头像", () => {
  test("未设头像显示默认图；上传后用户中心与顶栏同时更新", async ({ page }) => {
    await seedAuth(page);
    await stubPortal(page);

    // 必须是**能解码**的 data URL：坏图会触发 UserAvatar 的 onError 回落默认图（这是设计行为）
    const uploaded =
      "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==";
    let current = "";
    // 后注册的 route 优先命中（Playwright 反向匹配），所以这两条要写在 stubPortal 之后。
    await page.route("**/api/portal/me", (route) =>
      route.fulfill({ json: envelope({ ...ME_BOUND, avatar_url: current }) }),
    );
    await page.route("**/api/portal/me/avatar", (route) => {
      current = uploaded;
      return route.fulfill({ json: envelope({ avatar_url: uploaded }) });
    });

    await page.goto("/settings");
    const heroBtn = page.locator(".uc-hero .avatar-pick__btn");
    const topBtn = page.getByRole("button", { name: "用户菜单" });
    await expect(heroBtn).toBeVisible();
    await expect(page.locator(".uc-hero .avatar-pick__btn img")).toHaveAttribute(
      "src",
      /avatar-default/,
    );
    await expect(topBtn.locator("img")).toHaveAttribute("src", /avatar-default/);

    // 文件选择器是视觉隐藏的 input，用 force 跳过可见性检查
    await page
      .locator(".uc-hero input[type=file]")
      .setInputFiles("src/assets/avatar-default.png", { force: true });

    await expect(page.locator(".uc-hero .avatar-pick__btn img")).toHaveAttribute("src", uploaded);
    await expect(topBtn.locator("img")).toHaveAttribute("src", uploaded);

    // 面板头的元信息：头像 + 用户名 + 余额（余额来自会话 store）。
    // 弹窗是模态的（遮罩盖住顶栏），先关掉再看菜单 —— 关闭后落在首页，顶栏仍是同一个。
    await page.getByRole("dialog", { name: "用户中心" }).getByRole("button", { name: "关闭" }).click();
    await expect(page).toHaveURL(/\/home$/);
    await topBtn.click();
    const menu = page.getByRole("menu");
    await expect(menu.getByText("alice", { exact: true })).toBeVisible();
    await expect(menu.getByText("余额")).toBeVisible();
    await expect(menu.locator(".p-user-head__balance")).toContainText(/[$¥]/);
  });
});
