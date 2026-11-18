import { expect, test, type Page } from "@playwright/test";
import { PORTAL_CONFIG, envelope, fillCaptcha, seedAuth } from "./helpers";

/**
 * 登录 / 注册 / 找回密码（S2）
 *
 * 本轮改造后的三条硬约束，用例必须按它们写：
 * 1. **凭证是 httpOnly Cookie**：前端没有任何可播种的登录态，也没有 token；
 * 2. **验证码由服务端出图**：答案前端读不到，e2e 只验证「字段出现 + 能提交」；
 * 3. **登录失败不再前端锁账号**：达阈值后服务端返回 `captcha_required`，
 *    页面把验证码字段亮出来 —— 而不是显示"已锁定 N 秒"。
 */

/** 打桩门户配置 + 定价目录（登录页要读 auth 策略） */
async function stubPublicConfig(page: Page, auth: Partial<Record<string, unknown>> = {}) {
  const config = { ...PORTAL_CONFIG, auth: { ...PORTAL_CONFIG.auth, ...auth } };
  await page.route("**/api/portal/config**", (route) => route.fulfill({ json: envelope(config) }));
  await page.route("**/api/pricing**", (route) =>
    route.fulfill({
      json: envelope({
        models: [],
        vendors: [],
        defaults: {},
        group_ratios: { default: 1 },
        quota_per_unit: 500_000,
        currency: "USD",
        currency_symbol: "$",
        cny_per_usd: 7.2,
        other_currency: "CNY",
        other_quota_per_unit: 69444,
      }),
    }),
  );
}

test.describe("登录", () => {
  test("登录成功跳转 /home", async ({ page }) => {
    // seedAuth 先注册（兜底优先级最低）：/home 会请求 announcements、usage、overview，
    // 缺桩就会打到真后端 → 401 → 全局登出，表现是「登录成功却停在登录页」。
    // stubPublicConfig 与 login 桩件必须晚于它（后注册优先）。
    await seedAuth(page, { username: "alice" });
    await stubPublicConfig(page);
    await page.route("**/api/portal/auth/login", (route) =>
      route.fulfill({
        json: envelope({
          user: { id: 1, username: "alice", role: 1, group: "default", balance: 5_000_000, email: "", phone: "" },
          expires_in: 604_800,
        }),
      }),
    );

    await page.goto("/login");
    await page.fill("#ld-account", "alice");
    await page.fill("#ld-password", "Passw0rd!");
    await page.getByRole("button", { name: "登 录" }).click();
    await expect(page).toHaveURL(/\/home/);
    // 顶栏头像：未设自定义头像时渲染打包进产物的默认图（全名在用户菜单里）
    await expect(page.getByRole("button", { name: "用户菜单" }).locator("img")).toHaveAttribute(
      "src",
      /avatar-default/,
    );
    await page.getByRole("button", { name: "用户菜单" }).click();
    await expect(page.getByRole("menu").getByText("alice", { exact: true })).toBeVisible();
  });

  test("已开两步验证：第一步要动态码，验证后才跳 /home", async ({ page }) => {
    await seedAuth(page, { username: "alice" });
    await stubPublicConfig(page);
    // 第一步：密码通过但**不发 Cookie**，只回挑战。
    await page.route("**/api/portal/auth/login", (route) =>
      route.fulfill({
        json: envelope({ mfa_required: true, challenge: "ch-e2e", expires_in: 300 }),
      }),
    );
    const mfaPosts: unknown[] = [];
    await page.route("**/api/portal/auth/login/mfa", (route) => {
      mfaPosts.push(route.request().postDataJSON());
      return route.fulfill({
        json: envelope({
          user: { id: 1, username: "alice", role: 1, group: "default", balance: 5_000_000, email: "", phone: "" },
          expires_in: 604_800,
        }),
      });
    });

    await page.goto("/login");
    await page.fill("#ld-account", "alice");
    await page.fill("#ld-password", "Passw0rd!");
    await page.getByRole("button", { name: "登 录" }).click();

    // 关键：**不能**跳 /home —— 第二因素还没过，服务端也没发凭证。
    await expect(page.locator("#ld-otp")).toBeVisible();
    await expect(page).toHaveURL(/\/login/);
    expect(mfaPosts).toHaveLength(0);

    await page.locator("#ld-otp").fill("123456");
    await page.getByRole("button", { name: "验 证" }).click();
    await expect(page).toHaveURL(/\/home/);
    expect(mfaPosts).toEqual([{ challenge: "ch-e2e", code: "123456" }]);
  });

  test("两步验证失败：码错停在原地并可返回重新登录", async ({ page }) => {
    await stubPublicConfig(page);
    await page.route("**/api/portal/auth/login", (route) =>
      route.fulfill({ json: envelope({ mfa_required: true, challenge: "ch-2", expires_in: 300 }) }),
    );
    await page.route("**/api/portal/auth/login/mfa", (route) =>
      route.fulfill({ status: 400, json: envelope(null, 400, "动态码或备份码不正确", "mfa_code_invalid") }),
    );

    await page.goto("/login");
    await page.fill("#ld-account", "alice");
    await page.fill("#ld-password", "Passw0rd!");
    await page.getByRole("button", { name: "登 录" }).click();
    await page.locator("#ld-otp").fill("000000");
    await page.getByRole("button", { name: "验 证" }).click();
    await expect(page.getByText("动态码或备份码不正确")).toBeVisible();
    await expect(page).toHaveURL(/\/login/);

    // 「返回重新登录」= 丢掉挑战回到第一步（挑战是一次性的，留着也没用）
    await page.getByRole("link", { name: "返回重新登录" }).click();
    await expect(page.locator("#ld-account")).toBeVisible();
    await expect(page.locator("#ld-otp")).toHaveCount(0);
  });

  test("凭证错误按 error_code 显示文案（不触发全局登出）", async ({ page }) => {
    await stubPublicConfig(page);
    await page.route("**/api/portal/auth/login", (route) =>
      route.fulfill({ status: 401, json: envelope(null, 401, "账号或密码错误", "invalid_credential") }),
    );

    await page.goto("/login");
    await page.fill("#ld-account", "alice");
    await page.fill("#ld-password", "wrong-pass");
    await page.getByRole("button", { name: "登 录" }).click();
    await expect(page.getByText("账号或密码错误")).toBeVisible();
    // 401 + invalid_credential 属业务错误：必须留在登录页（不能当成会话过期跳走）
    await expect(page).toHaveURL(/\/login/);
  });

  test("来源校验失败（403 csrf_rejected）不得显示成「账号已被禁用」", async ({ page }) => {
    await stubPublicConfig(page);
    await page.route("**/api/portal/auth/login", (route) =>
      route.fulfill({
        status: 403,
        json: envelope(null, 403, "请求来源校验失败，请刷新页面后重试", "csrf_rejected"),
      }),
    );

    await page.goto("/login");
    await page.fill("#ld-account", "alice");
    await page.fill("#ld-password", "Passw0rd!");
    await page.getByRole("button", { name: "登 录" }).click();

    // 曾经的 bug：前端把所有 403 兜底成「账号已被禁用」，
    // 于是「刷新一下就好」的来源校验失败被用户读成「我的号被封了」。
    await expect(page.getByText("请求来源校验失败，请刷新页面后重试")).toBeVisible();
    await expect(page.getByText("账号已被禁用，请联系管理员")).toHaveCount(0);
    // 公开认证端点的 403 由页面内联展示，不再额外弹全局 toast
    await expect(page.locator(".toast, [data-toast]")).toHaveCount(0);
  });

  test("验证码关闭时，窗口期内与配置到达后都不渲染任何验证码控件", async ({ page }) => {
    // 延迟下发门户配置：拉开「配置还没到」的窗口。
    // 曾经的 bug：配置未就绪时策略兜底成 always，验证码先渲染再消失 ——
    // 关掉验证码的站点每次打开登录页都会闪一下。
    const config = { ...PORTAL_CONFIG, auth: { ...PORTAL_CONFIG.auth, captcha_login: "off" } };
    await page.route("**/api/portal/config**", async (route) => {
      await new Promise((resolve) => setTimeout(resolve, 800));
      await route.fulfill({ json: envelope(config) });
    });
    await page.route("**/api/pricing**", (route) =>
      route.fulfill({
        json: envelope({
          models: [],
          vendors: [],
          defaults: {},
          group_ratios: { default: 1 },
          quota_per_unit: 500_000,
          currency: "USD",
          currency_symbol: "$",
          cny_per_usd: 7.2,
          other_currency: "CNY",
          other_quota_per_unit: 69444,
        }),
      }),
    );

    await page.goto("/login");
    const submit = page.getByRole("button", { name: "登 录" });
    // 窗口期内：表单已渲染，但**没有**验证码（不知道策略就不该白取一张图）。
    await expect(page.locator("#ld-account")).toBeVisible();
    await expect(page.locator("#ld-captcha")).toHaveCount(0);
    // 配置到达后仍是 off：依旧没有。
    await expect(submit).toBeEnabled();
    await expect(page.locator("#ld-captcha")).toHaveCount(0);
  });

  test("after_fails：首屏不显示，服务端要求后亮出验证码，填对即可登录", async ({ page }) => {
    await seedAuth(page, { username: "bob" });
    await stubPublicConfig(page, { captcha_login: "after_fails" });

    // 第一次：没带验证码 → 服务端要求验证；第二次：带了 → 放行。
    let calls = 0;
    await page.route("**/api/portal/auth/login", (route) => {
      calls += 1;
      const body = route.request().postDataJSON() as { captcha_answer?: string };
      if (!body?.captcha_answer) {
        return route.fulfill({
          status: 400,
          json: envelope(null, 400, "请先完成人机验证", "captcha_required"),
        });
      }
      return route.fulfill({
        json: envelope({
          user: { id: 1, username: "bob", role: 1, group: "default", balance: 0, email: "", phone: "" },
          expires_in: 604_800,
        }),
      });
    });

    await page.goto("/login");
    // after_fails：首屏没有验证码控件
    await expect(page.locator("#ld-captcha")).toHaveCount(0);

    await page.fill("#ld-account", "bob");
    await page.fill("#ld-password", "Passw0rd!");
    await page.getByRole("button", { name: "登 录" }).click();
    await expect(page.getByText("请先完成人机验证")).toBeVisible();

    // 服务端说「要验证」之后，控件才出现，并且必须填对才能继续
    await expect(page.locator("#ld-captcha")).toBeVisible();
    await fillCaptcha(page, "12345", "login");
    await page.getByRole("button", { name: "登 录" }).click();
    await expect(page).toHaveURL(/\/home/);
    expect(calls).toBeGreaterThanOrEqual(2);
  });

  test("未登录访问受保护页 → 回登录页并带上回跳参数", async ({ page }) => {
    await stubPublicConfig(page);
    // 不打桩 /api/portal/me：vite 代理到后端（或直接失败）→ 守卫判定未登录
    await page.route("**/api/portal/me", (route) =>
      route.fulfill({ status: 401, json: envelope(null, 401, "未登录", "unauthorized") }),
    );
    await page.goto("/home");
    await expect(page).toHaveURL(/\/login/);
    await expect(page.url()).toContain("redirect=");
  });
});

test.describe("注册", () => {
  test("开放注册 + 人机验证始终显示", async ({ page }) => {
    await stubPublicConfig(page, { register_mode: "open", contact_verify: "off" });
    await page.route("**/api/portal/auth/register", (route) =>
      route.fulfill({ json: envelope({ id: 7, username: "newbie" }) }),
    );

    await page.goto("/register");

    await page.fill("#rg-username", "newbie");
    await page.fill("#rg-password", "Passw0rd!");
    await page.fill("#rg-password2", "Passw0rd!");
    await fillCaptcha(page, "12345", "register");
    await page.getByRole("checkbox").check();
    await page.getByRole("button", { name: "注 册" }).click();
    // 注册成功不自动登录：回登录页
    await expect(page).toHaveURL(/\/login/);
  });

  test("关闭注册时不渲染表单", async ({ page }) => {
    await stubPublicConfig(page, { register_mode: "closed" });
    await page.goto("/register");
    await expect(page.getByText("本站当前已关闭注册")).toBeVisible();
    await expect(page.locator("#rg-username")).toHaveCount(0);
  });

  test("仅邀请模式：邀请码变必填标记", async ({ page }) => {
    await stubPublicConfig(page, { register_mode: "invite_only" });
    await page.goto("/register");
    await expect(page.getByText("（本站仅限邀请注册）")).toBeVisible();
  });
});

test.describe("渠道智能加载", () => {
  /**
   * 桩件的 available_contact_channels 必须是**已按优先级排序**的数组：
   * 前端 `resolveChannel` 不排序（顺序是后端 `authcfg.channelPriority` 的职责），
   * 给它一个未排序的数组等于用桩件把被测逻辑架空了。
   */
  test("多渠道就绪：注册默认手机号，发码带 channel=sms", async ({ page }) => {
    await stubPublicConfig(page, {
      contact_verify: "required",
      contact_channels: ["email", "sms"],
      available_contact_channels: ["sms", "email"],
    });
    const sent: string[] = [];
    await page.route("**/api/portal/auth/register/contact-code", (route) => {
      const body = (route.request().postDataJSON() ?? {}) as { channel?: string };
      sent.push(String(body.channel));
      return route.fulfill({ json: envelope({ expires_in: 60 }) });
    });

    await page.goto("/register");

    // 默认渠道 = 下发数组首项（手机）：输入框类型、占位符、选中态三者一致
    await expect(page.locator("#rg-target")).toHaveAttribute("type", "tel");
    await expect(page.locator('#rg-target[type="email"]')).toHaveCount(0);
    await expect(page.locator("input[name=rg-channel]")).toHaveCount(2);
    await expect(page.getByRole("radio", { name: /手机号/ })).toBeChecked();

    await page.fill("#rg-target", "13800138000");
    await page.getByRole("button", { name: "发送验证码" }).click();
    await expect.poll(() => sent.length).toBe(1);
    expect(sent[0], "发码渠道必须跟着默认渠道走，不能再硬编码 email").toBe("sms");
  });

  test("只开短信：不渲染邮箱框，提交的 channel 是 sms", async ({ page }) => {
    await stubPublicConfig(page, {
      contact_verify: "required",
      contact_channels: ["sms"],
      available_contact_channels: ["sms"],
    });
    await page.route("**/api/portal/auth/register/contact-code", (route) =>
      route.fulfill({ json: envelope({ expires_in: 60 }) }),
    );
    const bodies: Record<string, unknown>[] = [];
    await page.route("**/api/portal/auth/register", (route) => {
      bodies.push((route.request().postDataJSON() ?? {}) as Record<string, unknown>);
      return route.fulfill({ json: envelope({ id: 7, username: "newbie" }) });
    });

    await page.goto("/register");

    // 单渠道：只渲染那一个输入框，且不出现「二选一」单选（没有第二个可选项）
    await expect(page.locator("#rg-target[type=tel]")).toBeVisible();
    await expect(page.locator('#rg-target[type="email"]')).toHaveCount(0);
    await expect(page.locator("input[name=rg-channel]")).toHaveCount(0);

    await page.fill("#rg-username", "newbie");
    await page.fill("#rg-target", "13800138000");
    await page.fill("#rg-code", "123456");
    await page.fill("#rg-password", "Passw0rd!");
    await page.fill("#rg-password2", "Passw0rd!");
    await fillCaptcha(page, "12345", "register");
    await page.getByRole("checkbox").check();
    await page.getByRole("button", { name: "注 册" }).click();

    await expect(page).toHaveURL(/\/login/);
    expect(bodies[0]?.channel, "注册提交必须带当前渠道，否则后端只能按邮箱处理").toBe("sms");
    expect(bodies[0]?.target).toBe("13800138000");
  });

  test("策略勾了邮箱但未就绪：整块联系方式字段都不渲染", async ({ page }) => {
    await stubPublicConfig(page, {
      contact_verify: "off",
      contact_channels: ["email"],
      available_contact_channels: [],
    });
    await page.goto("/register");
    await expect(page.locator("#rg-username")).toBeVisible();
    // 未就绪的渠道不再给用户填 —— 填了也必然发不出码
    await expect(page.locator("#rg-target")).toHaveCount(0);
    await expect(page.locator("#rg-code")).toHaveCount(0);
  });

  test("要求验证但无可用渠道：阻断并说明原因（不画空表单）", async ({ page }) => {
    await stubPublicConfig(page, {
      contact_verify: "required",
      contact_channels: ["email"],
      available_contact_channels: [],
    });
    await page.goto("/register");
    await expect(page.getByText("管理员尚未配置可用的发送通道")).toBeVisible();
    await expect(page.locator("#rg-username")).toHaveCount(0);
  });

  test("找回密码抽屉同样默认手机号", async ({ page }) => {
    await stubPublicConfig(page, {
      contact_verify: "required",
      contact_channels: ["email", "sms"],
      available_contact_channels: ["sms", "email"],
    });
    const sent: string[] = [];
    await page.route("**/api/portal/auth/password/reset/contact-code", (route) => {
      const body = (route.request().postDataJSON() ?? {}) as { channel?: string };
      sent.push(String(body.channel));
      return route.fulfill({ json: envelope({ expires_in: 60 }) });
    });

    await page.goto("/login");
    await page.getByRole("link", { name: "忘记密码？" }).click();

    await expect(page.locator("#rst-target")).toHaveAttribute("type", "tel");
    await expect(page.getByRole("radio", { name: /短信/ })).toBeChecked();

    await page.fill("#rst-target", "13800138000");
    await fillCaptcha(page, "12345", "reset");
    await page.getByRole("button", { name: "发送验证码" }).click();
    await expect.poll(() => sent.length).toBe(1);
    expect(sent[0]).toBe("sms");
  });
});

test.describe("找回密码", () => {
  test("三步：发码 → 校验换凭据 → 设置新密码", async ({ page }) => {
    await stubPublicConfig(page, { captcha_reset: "always", contact_channels: ["email"] });
    await page.route("**/api/portal/auth/password/reset/contact-code", (route) =>
      route.fulfill({ json: envelope({ expires_in: 60 }) }),
    );
    await page.route("**/api/portal/auth/password/reset/verify", (route) =>
      route.fulfill({ json: envelope({ reset_ticket: "ticket-1", expires_in: 600 }) }),
    );
    await page.route("**/api/portal/auth/password/reset", (route) =>
      route.fulfill({ json: envelope(null) }),
    );

    await page.goto("/login");
    await page.getByRole("link", { name: "忘记密码？" }).click();

    // 第 1 步：填邮箱 + 验证码 → 发码
    await page.fill("#rst-target", "alice@example.com");
    await fillCaptcha(page, "12345", "reset");
    await page.getByRole("button", { name: "发送验证码" }).click();

    // 第 2 步：填 6 位验证码 → 下一步
    await page.fill("#rst-code", "123456");
    await page.getByRole("button", { name: "下一步" }).click();

    // 第 3 步：设置新密码
    await expect(page.locator("#rst-pass")).toBeVisible();
    await page.fill("#rst-pass", "NewPassw0rd!");
    await page.fill("#rst-pass2", "NewPassw0rd!");
    await page.getByRole("button", { name: "重置密码" }).click();
    await expect(page.getByText("密码已重置，请使用新密码登录")).toBeVisible();
  });
});
