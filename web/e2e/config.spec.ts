/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, expectNoHScroll, isAdminPageRequest, seedAuth } from "./helpers";
import { PAY_CHANNEL_SCHEMAS } from "./fixtures/payChannelSchemas";

/**
 * 云存储渠道 schema（GET /api/admin/config 的 `storage_schemes`）。
 *
 * 与 backend/internal/service/systemconfig.StorageSchemes() 逐字一致：配置页的
 * 渠道胶囊与参数表单**完全由它驱动**，前端不再自带一份会漂移的字段清单
 * （漂移的表现是「后端支持的字段在前端填不了」，例如 COS 的私有化域名）。
 */
const STORAGE_SCHEMES = [
  { id: "none", name: "本地磁盘", fields: [] },
  {
    id: "s3",
    name: "S3 兼容",
    fields: [
      ["endpoint", "Endpoint", "1"],
      ["bucket", "Bucket", "1"],
      ["access_key", "Access Key", "1"],
      ["secret_key", "Secret Key", "1"],
      ["region", "地域（默认 us-east-1）", "0"],
      ["path_style", "Path Style（true 走路径寻址）", "0"],
    ],
  },
  {
    id: "minio",
    name: "MinIO",
    fields: [
      ["endpoint", "Endpoint", "1"],
      ["bucket", "Bucket", "1"],
      ["access_key", "Access Key", "1"],
      ["secret_key", "Secret Key", "1"],
      ["region", "地域（默认 us-east-1）", "0"],
      ["path_style", "Path Style（true 走路径寻址）", "0"],
    ],
  },
  {
    id: "oss",
    name: "阿里云 OSS",
    fields: [
      ["endpoint", "Endpoint（如 oss-cn-hangzhou.aliyuncs.com）", "1"],
      ["bucket", "Bucket", "1"],
      ["access_key_id", "AccessKey ID", "1"],
      ["access_key_secret", "AccessKey Secret", "1"],
    ],
  },
  {
    id: "cos",
    name: "腾讯云 COS",
    fields: [
      ["region", "地域（如 ap-guangzhou）", "1"],
      ["bucket", "Bucket（name-appid）", "1"],
      ["secret_id", "SecretId", "1"],
      ["secret_key", "SecretKey", "1"],
      ["endpoint", "访问域名（可选，私有化/加速域名）", "0"],
    ],
  },
];

/** 管理台配置全量 fixture（32 键，与 `systemconfig.Keys` 逐字一致；敏感叶子以 Mask 回显） */
const CONFIG = {
  // 运营模式：默认开启（关闭 = 纯服务端）
  portal_enabled: true,
  // 发布清单地址（一键更新的版本来源）；空 = 不检查
  release_manifest_url: "",
  site: {
    site_name: "灵动 API",
    logo_image: "",
    favicon_image: "",
    site_url: "",
    api_base_urls: ["https://api.example.com"],
    contact_email: "support@example.com",
    footer_text: "灵动 API · 大模型开放平台",
    icp_beian: "京ICP备00000000号-1",
    // 真实后端会下发币种（site.currency）：金额口径与展示符号都由它派生
    currency: "CNY",
    currency_symbol: "¥",
  },
  // 折扣按**微比例**存（1e6 = 不打折）：1 元 100% → 0.9 折 = 900000
  group_discount: { default: 1000000, vip: 900000 },
  cny_per_usd: 7.2,
  balance_limit: 50000000,
  min_topup: 1,
  custom_topup_enabled: true,
  pay_channels: [
    {
      id: "epay",
      name: "易支付",
      protocol: "epay",
      enabled: true,
      // 枚举 code（后端 Methods 归一化后的落库值）；中文原文只存在于历史配置
      methods: ["wechat", "alipay"],
      params: { pay_address: "https://pay.example.com", pid: "1001", key: "******" },
    },
    {
      id: "xunhupay",
      name: "虎皮椒（微信）",
      protocol: "xunhupay",
      enabled: false,
      methods: ["wechat"],
      params: {
        pay_address: "https://api.xunhupay.com/payment/do.html",
        app_id: "20200101",
        app_secret: "******",
      },
    },
  ],
  retry_times: 2,
  file_retention_days: 7,
  poll_deadline_min: 30,
  logs_retention_days: 30,
  archive_retention_days: 60,
  audit_retention_days: 90,
  rate_limit_defaults: { rpm: 600, concurrency: 60 },
  auth_captcha: {
    scenes: { login: "after_fails" },
  },
  auth_login: {
    fail_threshold: 5,
    backoff_seconds: [60, 300, 900],
    ip_fail_limit: 20,
    ip_fail_window_sec: 600,
    delay_max_ms: 800,
  },
  auth_session: { absolute_ttl_sec: 604800, idle_ttl_sec: 43200, max_active: 10, touch_interval_sec: 300 },
  auth_register: { mode: "open", contact_verify: "off", contact_channels: ["email"] },
  auth_mfa: { policy: "optional" },
  auth_event_retention_days: 90,
  storage: { active: "none", channels: {} },
  uploads: {
    enabled: true,
    daily_limit_per_key: 200,
    retention_hours: 24,
    user_total_mb: 2048,
    concurrency: 3,
    types: {
      png: "image", jpg: "image", webp: "image", gif: "image",
      mp3: "audio", mp4: "video", pdf: "document", txt: "text",
    },
    limits: { image: 20, audio: 100, video: 200, document: 30, text: 5 },
    compress: { enabled: true, quality: 82, format: "webp" },
  },
  email: {
    enabled: false,
    smtp_host: "",
    smtp_port: 465,
    encryption: "ssl",
    username: "",
    password: "",
    from_addr: "",
    sender_name: "",
    scenes: { task_done: true, low_balance: true, price_change: false },
    // 真实种子形状：每场景一个空对象（非 {subject:"",body:""}）；前端须补齐默认文案后方可保存
    templates: { task_done: {}, low_balance: {}, price_change: {}, reset_password: {} },
  },
  sms: { enabled: false, active: "aliyun", daily_limit: 100, channels: { aliyun: {}, tencent: {} } },
  aff: {
    enabled: true,
    inviter_rate: 100_000,
    invitee_amount: 5_000_000,
    max_rewards: 3,
    hold_hours: 48,
    min_transfer: 100_000,
    daily_invite_cap: 20,
  },
  monitor: { auto_disable: true, fail_threshold: 0.5, fail_window: 5, auto_enable: true, probe_interval_min: 5 },
  announcements: {
    enabled: true,
    items: [
      {
        id: 1,
        content: "**充值活动** 上线，详见活动页",
        publishDate: "2026-10-15T10:00:00+08:00",
        type: "success",
        extra: "",
      },
    ],
  },
  about_content: "关于灵动 API 的说明",
  agreement_content: "用户协议正文",
  modality_defaults: {
    chat: "gpt-4o",
    image: "doubao-seedream-4.0",
    video: "kling-v2-master",
    audio: "tts-1-hd",
    embedding: "text-embedding-3-large",
    rerank: "qwen3-reranker",
    moderation: "omni-moderation",
  },
};

const MODELS = {
  items: [
    { id: "gpt-4o", display_name: "GPT-4o", modality: "chat", enabled: true },
    { id: "gpt-4o-mini", display_name: "GPT-4o mini", modality: "chat", enabled: true },
    { id: "claude-sonnet", display_name: "Claude Sonnet", modality: "chat", enabled: false },
    { id: "doubao-seedream-4.0", display_name: "Seedream 4.0", modality: "image", enabled: true },
  ],
};

const ANNOUNCEMENTS = {
  enabled: true,
  items: [
    {
      id: 1,
      content: "**充值活动** 上线，详见活动页",
      publishDate: "2026-10-15T10:00:00+08:00",
      type: "success",
      extra: "详情见活动页",
    },
  ],
  unread: 1,
};

interface StubOptions {
  /** 云存储探针结果 */
  storageOk?: boolean;
  /** 通知测试发送结果 */
  notifyOk?: boolean;
  /** 支付渠道为空（空态用例：新库默认没有渠道） */
  emptyPayChannels?: boolean;
}

/** 统一挂载 /api/admin/** 与 /api/portal/** 桩件，返回已发生的请求记录 */
async function stub(page: Page, opts: StubOptions = {}): Promise<string[]> {
  const calls: string[] = [];
  await page.route(
    (url) => isAdminPageRequest(url),
    (route) => {
      const { pathname } = new URL(route.request().url());
      const method = route.request().method();
      calls.push(`${method} ${pathname}`);

      if (pathname === "/api/admin/config" && method === "GET") {
        // 派生数据必须放 config 外层：config 是 28 键全量包，会被原样 PUT 回去
        return route.fulfill({
          json: envelope({
            config: opts.emptyPayChannels ? { ...CONFIG, pay_channels: [] } : CONFIG,
            mask: "******",
            notify_base: "https://api.example.com/api/pay/",
            // 参数声明是前端渲染渠道表单的唯一依据（键/分组/必填/密文）。
            // 与 backend/internal/service/payment/schema.go 保持一致。
            pay_channel_schemas: PAY_CHANNEL_SCHEMAS,
            // 云存储渠道 schema 的唯一真相源在后端（StorageSchemes），前端只负责渲染
            storage_schemes: STORAGE_SCHEMES,
            // 进程里正在用的写入位置（storage.Effective）：与表单草稿值 storage.active 不同
            storage_effective: {
              active: "none", label: "本地磁盘", bucket: "", endpoint: "", writable: true,
            },
            // 旧字段：新前端不再使用，保留以模拟真实响应（兼容期后端仍会下发）
            pay_channel_templates: {
              epay: { pay_address: "", pid: "", key: "" },
              xunhupay: { pay_address: "https://api.xunhupay.com/payment/do.html", app_id: "", app_secret: "" },
            },
            pay_method_options: [
              { value: "wechat", label: "微信支付" },
              { value: "alipay", label: "支付宝" },
            ],
          }),
        });
      }
      if (pathname === "/api/admin/config" && method === "PUT") {
        return route.fulfill({ json: envelope({ keys: Object.keys(CONFIG) }) });
      }
      if (pathname === "/api/admin/models") {
        return route.fulfill({ json: envelope(MODELS) });
      }
      if (pathname === "/api/admin/storage/test") {
        const result =
          opts.storageOk === false
            ? { ok: false, backend: "s3", message: "连接超时" }
            : { ok: true, backend: "s3", message: "写入 / 读取校验通过" };
        return route.fulfill({ json: envelope(result) });
      }
      if (pathname === "/api/admin/notify/test") {
        if (opts.notifyOk === false) {
          return route.fulfill({
            json: envelope(null, 400, "邮件通道未启用", "notify_channel_disabled"),
          });
        }
        return route.fulfill({ json: envelope({ channel: "email", target: "ops@example.com" }) });
      }
      if (pathname === "/api/portal/config") {
        return route.fulfill({
          json: envelope({
            site: CONFIG.site,
            api: { bases: CONFIG.site.api_base_urls, source: "config" },
            group_ratios: CONFIG.group_ratios,
            about_content: CONFIG.about_content,
            agreement_content: CONFIG.agreement_content,
            auth: {
              register_mode: "open",
              contact_verify: "off",
              contact_channels: ["email"],
              captcha_register: "always",
              captcha_login: "after_fails",
              captcha_reset: "always",
              login_fail_threshold: 5,
            },
            custom_topup_enabled: true,
            min_topup: 1,
            email_enabled: false,
          }),
        });
      }
      if (pathname === "/api/portal/announcements") {
        return route.fulfill({ json: envelope(ANNOUNCEMENTS) });
      }
      if (pathname === "/api/portal/announcements/read") {
        // 与 GET 同形状：前端拿整份 payload 替换缓存（不再有 unread 字段）
        return route.fulfill({
          json: envelope({
            enabled: ANNOUNCEMENTS.enabled,
            items: ANNOUNCEMENTS.items.map((item) => ({ ...item, read: true })),
          }),
        });
      }
      if (pathname === "/api/portal/me") {
        return route.fulfill({
          json: envelope({
            id: 1,
            username: "admin",
            role: 0,
            status: 1,
            group: "default",
            // /portal/me 的金额字段是 balance / frozen / used（旧名 quota* 已删除，
            // 留着等于给后端伪造一份不存在的契约）。
            balance: 1000000,
            frozen: 0,
            used: 0,
            request_count: 0,
            key_count: 1,
            key_count_enabled: 1,
            group_discount: 1_000_000,
            group_configured: false,
            email: "admin@example.com",
            phone: "",
          }),
        });
      }
      return route.fulfill({ json: envelope(null) });
    },
  );
  return calls;
}

const card = (page: Page, title: string) => page.locator(".card", { hasText: title });

/** 表单控件（label 与 input 为兄弟节点，按 form-item 文本定位） */
const field = (root: ReturnType<typeof card>, label: string) =>
  root.locator(".form-item", { hasText: label }).locator("input");

test.describe("S10 系统配置与通知 · 管理台", () => {
  test("站点品牌：无 Logo 字符项、图标走同一兜底链、地址可增删并即时上报数组", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);

    const payloads: Record<string, unknown>[] = [];
    await page.route(
      (url) => url.pathname === "/api/admin/config",
      async (route) => {
        if (route.request().method() === "PUT") payloads.push(route.request().postDataJSON());
        await route.fallback();
      },
    );

    await page.goto("/admin/config");
    const brand = card(page, "站点品牌");

    // 「Logo 字符」已从数据模型里删除：无图时的降级恒为**站名首字**，
    // 单独维护 1–2 字只会在改名后与站名不一致（用户看到两个品牌）。
    await expect(brand.locator(".form-item", { hasText: "Logo 字符" })).toHaveCount(0);
    await expect(brand.locator(".form-item", { hasText: "站点图标" })).toHaveCount(1);

    // 图标预览走同一条兜底链（图标 → Logo → 首字）：未上传时显示首字，而不是空框
    const icon = brand.locator(".logo-preview--icon");
    await expect(icon.locator(".dot")).toHaveText("灵");

    // 地址一行一条，首条带「主」标记
    const rows = brand.locator(".gr-row--url");
    await expect(rows).toHaveCount(1);
    await expect(rows.first().locator("input")).toHaveValue("https://api.example.com");
    await expect(rows.first().locator(".badge")).toHaveText("主");

    // 新增 → 填写 → 上移，上移后它成为主地址（顺序即优先级）
    await brand.getByRole("button", { name: "添加地址" }).click();
    await expect(rows).toHaveCount(2);
    await rows.nth(1).locator("input").fill("https://api2.example.com");
    // 非首行才有「上移」：首行上移没有意义，按钮存在只会让人以为排错了
    await expect(rows.first().getByRole("button", { name: "上移" })).toHaveCount(0);
    await rows.nth(1).getByRole("button", { name: "上移" }).click();
    await expect(rows.first().locator("input")).toHaveValue("https://api2.example.com");
    await expect(rows.first().locator(".badge")).toHaveText("主");

    // 上传图标：dataURI 落进 favicon_image（走的是与 Logo 同一套校验）
    await brand.locator("input[type=file]").nth(1).setInputFiles({
      name: "icon.png",
      mimeType: "image/png",
      buffer: Buffer.from(
        "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
        "base64",
      ),
    });
    await expect(icon.locator("img")).toBeVisible();

    await page.getByRole("button", { name: "保存本页" }).click();
    await expect(page.getByText("本页配置已保存并即时生效")).toBeVisible();

    const saved = payloads[0] as { site: { api_base_urls: string[]; favicon_image: string } };
    expect(saved.site.api_base_urls).toEqual(["https://api2.example.com", "https://api.example.com"]);
    expect(saved.site.favicon_image.startsWith("data:image/png;base64,")).toBe(true);
  });

  test("浏览器标签同步：标题取站名、图标取配置（无图标时用站名首字生成）", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);

    await page.goto("/admin/config");
    // index.html 里的 <title> 是构建期常量：不在运行期同步，改名后标签页永远是旧站名
    await expect(page).toHaveTitle("灵动 API");

    const favicon = page.locator('head link#site-favicon');
    await expect(favicon).toHaveCount(1);
    await expect(favicon).toHaveAttribute("rel", "icon");
    // 配置里没有图标也没有 Logo ⇒ 用站名首字生成的内联 svg（恒有图标，不会空着）
    await expect(favicon).toHaveAttribute("href", /^data:image\/svg\+xml,/);
  });

  test("七个 TAB 覆盖全部分区，默认落在「通用」", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);

    await page.goto("/admin/config");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("系统配置");
    await expect(page.locator("#pageDesc")).toHaveText(
      "站点、计费、支付、存储、通知与安全策略；按页签保存，只提交本页的键",
    );
    await expect(page.getByRole("tab")).toHaveCount(7);

    // 默认页签 = 通用（品牌与准入是最先要配的东西），且不带 URL 参数
    await expect(page).toHaveURL(/\/admin\/config$/);
    await expect(page.getByRole("tab", { name: "通用" })).toHaveAttribute("aria-selected", "true");
    await expect(page.locator(".card")).toHaveCount(6);
    for (const title of ["运营模式", "版本与更新", "站点品牌", "注册与准入", "站点公告", "站点内容"]) {
      await expect(card(page, title)).toBeVisible();
    }

    await page.getByRole("tab", { name: "计费" }).click();
    await expect(page.locator(".card")).toHaveCount(2);
    for (const title of ["计费参数", "邀请返利"]) {
      await expect(card(page, title)).toBeVisible();
    }

    // 支付渠道单独成页签：它与计费是两条操作链（改折扣不该被「密钥没填完」拦下）
    await page.getByRole("tab", { name: "支付渠道" }).click();
    await expect(page.locator(".card")).toHaveCount(1);
    await expect(card(page, "支付渠道")).toBeVisible();

    await page.getByRole("tab", { name: "存储" }).click();
    await expect(page.locator(".card")).toHaveCount(2);
    for (const title of ["云存储", "文件上传"]) {
      await expect(card(page, title)).toBeVisible();
    }

    await page.getByRole("tab", { name: "通知" }).click();
    await expect(page.locator(".card")).toHaveCount(3);
    for (const title of ["邮件通知", "短信通知", "邮件模板"]) {
      await expect(card(page, title)).toBeVisible();
    }

    await page.getByRole("tab", { name: "模型与调用" }).click();
    await expect(page.locator(".card")).toHaveCount(3);
    for (const title of ["各类型默认模型", "路由与限流", "渠道监控"]) {
      await expect(card(page, title)).toBeVisible();
    }

    await page.getByRole("tab", { name: "安全与数据" }).click();
    await expect(page.locator(".card")).toHaveCount(3);
    for (const title of ["登录防护", "会话", "数据保留"]) {
      await expect(card(page, title)).toBeVisible();
    }
  });

  test("运营模式：关闭需二次确认，保存包带上 portal_enabled", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);

    let payload: Record<string, unknown> | null = null;
    await page.route(
      (url) => url.pathname === "/api/admin/config",
      async (route) => {
        if (route.request().method() === "PUT") payload = route.request().postDataJSON();
        await route.fallback();
      },
    );

    await page.goto("/admin/config");
    const modeCard = card(page, "运营模式");
    const toggle = modeCard.locator(".switch input");
    await expect(toggle).toBeChecked();

    // 关闭是「断掉一条对外通道」：必须先确认，且取消时不能改状态
    await toggle.click();
    await expect(page.getByRole("dialog")).toContainText("关闭门户端");
    await page.getByRole("button", { name: "取消" }).click();
    await expect(toggle).toBeChecked();

    await toggle.click();
    await page.getByRole("button", { name: "关闭门户" }).click();
    await expect(toggle).not.toBeChecked();
    // 关闭态才出现的同步说明（避免运营以为「保存即全节点生效」）
    await expect(modeCard).toContainText("其它节点最迟 30 秒同步");

    await page.getByRole("button", { name: "保存本页" }).click();
    await expect(page.getByText("本页配置已保存并即时生效")).toBeVisible();
    expect((payload as { portal_enabled?: boolean } | null)?.portal_enabled).toBe(false);
  });

  test("七个页签的 URL 与 DOM id 唯一性", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);

    await page.goto("/admin/config");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("系统配置");

    const dupIds = async () =>
      page.evaluate(() => {
        const seen = new Map<string, number>();
        document.querySelectorAll("[id]").forEach((el) => {
          if (!el.id) return;
          seen.set(el.id, (seen.get(el.id) ?? 0) + 1);
        });
        return [...seen.entries()].filter(([, n]) => n > 1).map(([id]) => id);
      });

    // 默认页签不写参数（默认值即删参数 → 规范 URL 收敛）
    await expect(page).toHaveURL(/\/admin\/config$/);
    expect(await dupIds()).toEqual([]);

    for (const [label, param] of [
      ["计费", "billing"],
      ["支付渠道", "pay"],
      ["存储", "storage"],
      ["通知", "notify"],
      ["模型与调用", "models"],
      ["安全与数据", "security"],
    ] as const) {
      await page.getByRole("tab", { name: label, exact: true }).click();
      await expect(page).toHaveURL(new RegExp(`tab=${param}`));
      expect(await dupIds(), `页签「${label}」存在重复 id`).toEqual([]);
    }

    // 深链 + 刷新保持
    await page.goto("/admin/config?tab=security");
    await expect(page.getByRole("tab", { name: "安全与数据" })).toHaveAttribute("aria-selected", "true");
    await page.reload();
    await expect(page.getByRole("tab", { name: "安全与数据" })).toHaveAttribute("aria-selected", "true");

    // 非法值 → 回落默认页签，且地址栏被纠偏（replace，无残留）
    await page.goto("/admin/config?tab=nope");
    await expect(page.getByRole("tab", { name: "通用" })).toHaveAttribute("aria-selected", "true");
    await expect(page).toHaveURL(/\/admin\/config$/);
  });

  test("保存动作在页头常显：跨页签不重建，未保存才可用", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);
    await page.setViewportSize({ width: 1440, height: 700 });

    await page.goto("/admin/config?tab=notify");
    const acts = page.locator(".ph-acts");
    await expect(acts).toBeVisible();
    await expect(acts.getByRole("button")).toHaveText(["还原本页", "保存本页"]);
    // 没有改动就不给保存：否则「点一下」会白白发一个空包，还让人以为改了什么
    await expect(acts.getByRole("button", { name: "保存本页" })).toBeDisabled();
    // 干净时连说明节点都不渲染（避免页头右侧出现一段空白）
    await expect(acts.locator(".ph-acts-note")).toHaveCount(0);

    // 它是页头的一部分：跟着滚动走，但不是文档流里的固定条（也不吸附在底部）
    const box = await acts.evaluate((el) => ({
      sticky: getComputedStyle(el).position,
      inHead: el.closest(".ph") !== null,
    }));
    expect(box.inHead, "保存动作必须在页头 .ph 内，不能散落到面板里").toBe(true);
    expect(box.sticky, "保存动作不是粘性条（粘性条会遮住末卡）").toBe("static");

    // 切页签后仍在（跨页签常显的前提：它必须渲染在 PageTabs 之外）
    await page.getByRole("tab", { name: "安全与数据" }).click();
    await expect(acts).toBeVisible();
    await expect(page).toHaveURL(/tab=security/);

    // 未保存提示跟着当前页签走
    await page.locator(".form-item", { hasText: "空闲超时（小时）" }).locator("input").fill("3");
    await expect(acts.locator(".ph-acts-note")).toHaveText("本页有未保存的改动");
    await expect(acts.getByRole("button", { name: "保存本页" })).toBeEnabled();
  });

  test("页签面板块间距 18px（间距所有权在容器，卡片不再贴身）", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);
    await page.setViewportSize({ width: 1440, height: 700 });

    // 每个页签的面板里都有多个块（卡 / 栅格）。旧实现把 18px 挂在 .page-tabs 上、
    // 面板是块级流，而 .card 已不带 margin-bottom ⇒ 实测每一屏的相邻卡片间距都是 0。
    for (const t of ["general", "billing", "storage", "notify", "models", "security"]) {
      await page.goto(`/admin/config?tab=${t}`);
      await expect(page.locator(".tab-panel")).toBeVisible();

      const gaps = await page.evaluate(() => {
        const panel = document.querySelector(".tab-panel")!;
        // 只量参与文档流的块：吸顶/吸底元素（rect 不构成文档间距）与零高元素跳过
        const blocks = Array.from(panel.children).filter((el) => {
          const pos = getComputedStyle(el).position;
          return pos !== "sticky" && pos !== "fixed" && el.getBoundingClientRect().height > 0;
        });
        const out: number[] = [];
        for (let i = 1; i < blocks.length; i++) {
          const prev = blocks[i - 1].getBoundingClientRect();
          const cur = blocks[i].getBoundingClientRect();
          out.push(Math.round((cur.top - prev.bottom) * 10) / 10);
        }
        return out;
      });

      for (const g of gaps) {
        expect(g, `页签「${t}」的块间距是 ${g}px，应为 18px`).toBeGreaterThan(16);
        expect(g, `页签「${t}」的块间距是 ${g}px，应为 18px`).toBeLessThan(20);
      }
    }
  });

  test("页签条吸附顶栏下沿：滚动时不再跟着内容滚走", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);

    /** 页签条上沿与顶栏下沿的距离（吸附生效时应为 0） */
    const pinGap = () =>
      page.evaluate(() => {
        const bar = document.querySelector(".page-tabs-bar")!;
        const top = document.querySelector(".a-top")!.getBoundingClientRect();
        const r = bar.getBoundingClientRect();
        return {
          position: getComputedStyle(bar).position,
          delta: Math.round((r.top - top.bottom) * 10) / 10,
          y: Math.round(r.top),
        };
      });

    // 桌面：顶栏 64
    await page.setViewportSize({ width: 1440, height: 700 });
    await page.goto("/admin/config?tab=notify");
    await expect(page.locator(".page-tabs-bar")).toBeVisible();
    const before = await pinGap();
    expect(before.position, "页签条必须是 sticky，否则滚动时会整条滚走").toBe("sticky");
    expect(before.y, "未滚动时页签条应在文档流内（页头下方）").toBeGreaterThan(64);

    const maxScroll = await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight);
    expect(maxScroll, "通知页应可滚动").toBeGreaterThan(400);
    await page.evaluate(() => window.scrollTo(0, 400));
    await page.waitForTimeout(150);
    const pinned = await pinGap();
    expect(Math.abs(pinned.delta), `页签条未钉在顶栏下沿（delta=${pinned.delta}）`).toBeLessThanOrEqual(1);

    // 移动端顶栏是 52px：吸附位置必须跟着顶栏高度令牌走，写死 64 会钻进顶栏下方
    await page.setViewportSize({ width: 375, height: 720 });
    await page.goto("/admin/config?tab=notify");
    await expect(page.locator(".page-tabs-bar")).toBeVisible();
    await page.evaluate(() => window.scrollTo(0, 300));
    await page.waitForTimeout(150);
    const mobile = await pinGap();
    expect(Math.abs(mobile.delta), `移动端页签条未贴合顶栏（delta=${mobile.delta}）`).toBeLessThanOrEqual(1);
    expect(mobile.y, "移动端页签条应停在 52px 顶栏下沿").toBeLessThanOrEqual(53);
  });

  test("375px 七个页签单行横滚（不换行、不溢出）", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);
    await page.setViewportSize({ width: 375, height: 780 });
    await page.goto("/admin/config");
    await expect(page.getByRole("tab", { name: "通用" })).toBeVisible();

    const strip = page.locator(".page-tabs-bar > .tabs");
    const box = await strip.evaluate((el) => ({
      wrap: getComputedStyle(el).flexWrap,
      scrollWidth: el.scrollWidth,
      clientWidth: el.clientWidth,
      height: el.getBoundingClientRect().height,
    }));
    // 移动端规则把 .tabs 从 wrap 改成 nowrap + overflow-x:auto（右缘渐隐）
    expect(box.wrap).toBe("nowrap");
    expect(box.scrollWidth, "7 个页签应需要横滚").toBeGreaterThan(box.clientWidth);
    expect(box.height, "页签条不应折成两行").toBeLessThan(60);

    await expectNoHScroll(page, "页签横滚不得造成页面横向溢出");
  });

  test("页头在 768–1023 换行：标题不被压成省略号，页签不折行", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);
    await page.setViewportSize({ width: 900, height: 800 });
    await page.goto("/admin/config?tab=notify");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("系统配置");
    // 页头在加载态就已渲染（h1 恒在），必须等面板出来再量动作区与页签
    await expect(page.locator(".page-tabs-bar")).toBeVisible();

    const head = await page.evaluate(() => {
      const h1 = document.querySelector(".ph h1")!;
      const desc = document.querySelector(".ph-desc")!;
      const acts = document.querySelector(".ph-acts")!;
      const tabs = document.querySelector(".page-tabs-bar > .tabs")!;
      return {
        h1Truncated: h1.scrollWidth > h1.clientWidth + 1,
        h1Top: Math.round(h1.getBoundingClientRect().top),
        descTop: Math.round(desc.getBoundingClientRect().top),
        actsTop: Math.round(acts.getBoundingClientRect().top),
        tabsWrap: getComputedStyle(tabs).flexWrap,
        tabsHeight: Math.round(tabs.getBoundingClientRect().height),
      };
    });
    expect(head.h1Truncated, "标题被压成省略号：页头必须允许换行").toBe(false);
    // 标题与说明上下排列（同一列），动作区整体掉到下一行
    expect(head.descTop, "说明应排在标题下方").toBeGreaterThan(head.h1Top);
    expect(head.actsTop, "动作区应排在标题与说明之后").toBeGreaterThan(head.descTop);
    // 页签条在 768–1023 也必须单行横滚（7 项会折成两行，看上去像多出一个分组）
    expect(head.tabsWrap).toBe("nowrap");
    expect(head.tabsHeight, "页签条不应折行").toBeLessThan(60);
  });

  test("四列栅格在 1024–1199 降两列：标签不被压成两行", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);
    await page.setViewportSize({ width: 1100, height: 900 });
    await page.goto("/admin/config?tab=billing");

    const grid = page.locator(".form-grid--4").first();
    const cols = await grid.evaluate((el) => getComputedStyle(el).gridTemplateColumns.split(" ").length);
    expect(cols, "1024–1199 时四列每列只剩 ~165px，必须降到两列").toBe(2);

    // 标签不换行（被压成两行的直接证据）
    const label = page.locator(".form-item", { hasText: "汇率（1 美元兑人民币）" }).locator("label");
    const lineHeight = await label.evaluate((el) => el.getBoundingClientRect().height);
    expect(lineHeight, `标签高度 ${lineHeight}px，说明已被压成两行`).toBeLessThan(30);
  });

  test("云存储单选胶囊级联：仅渲染选中渠道的必填参数", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);

    await page.goto("/admin/config");
    await page.getByRole("tab", { name: "存储" }).click();

    const storage = card(page, "云存储");
    // 「当前写入」回显的是进程正在用的位置，不是表单草稿值
    await expect(field(storage, "当前写入")).toHaveValue(/本地磁盘/);
    await expect(storage.locator(".kbtn", { hasText: "本地磁盘" })).toHaveClass(/active/);
    await expect(storage.locator(".form-item", { hasText: "Endpoint" })).toHaveCount(0);

    await storage.locator(".kbtn", { hasText: "阿里云 OSS" }).click();
    await expect(storage.locator(".kbtn", { hasText: "阿里云 OSS" })).toHaveClass(/active/);
    for (const label of ["Endpoint", "Bucket", "AccessKey ID", "AccessKey Secret"]) {
      await expect(field(storage, label)).toBeVisible();
    }
    // s3 专属可选字段不出现
    await expect(storage.locator(".form-item", { hasText: "Path Style" })).toHaveCount(0);
  });

  test("云存储探针与通知测试发送：结果经 toast 呈现", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page, { storageOk: true, notifyOk: true });

    await page.goto("/admin/config");
    await page.getByRole("tab", { name: "存储" }).click();
    await card(page, "云存储").getByRole("button", { name: "测试连通性" }).click();
    await expect(page.getByText(/s3：写入 \/ 读取校验通过/)).toBeVisible();
    expect(calls).toContain("POST /api/admin/storage/test");

    await page.getByRole("tab", { name: "通知" }).click();
    const mail = card(page, "邮件通知");
    await mail.getByPlaceholder("test@example.com").fill("ops@example.com");
    await mail.getByRole("button", { name: "测试发送" }).click();
    await expect(page.getByText(/测试邮件已发送至 ops@example.com/)).toBeVisible();
    expect(calls).toContain("POST /api/admin/notify/test");
  });

  test("通知通道未启用时给出业务错误提示", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page, { notifyOk: false });

    await page.goto("/admin/config");
    await page.getByRole("tab", { name: "通知" }).click();
    const mail = card(page, "邮件通知");
    await mail.getByPlaceholder("test@example.com").fill("ops@example.com");
    await mail.getByRole("button", { name: "测试发送" }).click();

    await expect(page.getByText("邮件通道未启用")).toBeVisible();
  });

  test("邮件模板：空模板回落默认文案并支持恢复默认", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);

    await page.goto("/admin/config");
    await page.getByRole("tab", { name: "通知" }).click();

    const tpl = card(page, "邮件模板");
    await expect(tpl.getByRole("tab", { name: "密码重置" })).toHaveAttribute("aria-selected", "true");
    await expect(tpl.locator("textarea")).toHaveValue(/验证码：\{\{code\}\}/);

    await tpl.locator("textarea").fill("自定义正文");
    await tpl.getByRole("button", { name: "恢复默认模板" }).click();
    await expect(tpl.locator("textarea")).toHaveValue(/验证码：\{\{code\}\}/);

    await tpl.getByRole("tab", { name: "价目变动" }).click();
    await expect(tpl.locator("textarea")).toHaveValue(/模型价目/);
  });

  test("站点公告：弹窗新增后进入列表", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    const calls = await stub(page);

    await page.goto("/admin/config");
    const notice = card(page, "站点公告");
    await expect(notice.locator(".nt-adm-row")).toHaveCount(1);

    await notice.getByRole("button", { name: "新增公告" }).click();
    const dialog = page.getByRole("dialog", { name: "新增公告" });
    await expect(dialog).toBeVisible();
    await dialog.locator("textarea").fill("**维护通知** 今晚 23:00 起短时不可用");
    await dialog.getByRole("button", { name: "保存" }).click();

    await expect(dialog).toBeHidden();
    await expect(notice.locator(".nt-adm-row")).toHaveCount(2);
    await expect(notice.locator(".nt-adm-row").first()).toContainText("维护通知");

    await page.getByRole("button", { name: "保存本页" }).click();
    await expect(page.getByText("本页配置已保存并即时生效")).toBeVisible();
    expect(calls).toContain("PUT /api/admin/config");
  });

  test("按页签保存：只提交本页的键，其它页签的改动不跟着走", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);

    const payloads: Record<string, unknown>[] = [];
    await page.route(
      (url) => url.pathname === "/api/admin/config",
      async (route) => {
        if (route.request().method() === "PUT") payloads.push(route.request().postDataJSON());
        await route.fallback();
      },
    );

    await page.goto("/admin/config");
    const saveBtn = page.getByRole("button", { name: "保存本页" });
    // 没有改动就不给保存：否则「点一下」会白白发一个空包，还让人以为改了什么
    await expect(saveBtn).toBeDisabled();

    // 计费页签：只改汇率
    await page.getByRole("tab", { name: "计费" }).click();
    const rate = page.locator(".form-item", { hasText: "汇率（1 美元兑人民币）" }).locator("input");
    await rate.fill("7.3");
    await expect(saveBtn).toBeEnabled();
    // 没保存的页签带小圆点：分页签保存后，不标出来就会「切页签时以为已经存过」
    await expect(page.getByRole("tab", { name: "计费" }).locator(".tab-dot")).toHaveCount(1);
    await expect(page.getByRole("tab", { name: "通知" }).locator(".tab-dot")).toHaveCount(0);
    // 类名必须真有样式：`.tab-dot` 若没定义，span 是空的内联元素 → 宽 0，肉眼看不见
    const dotSize = await page
      .getByRole("tab", { name: "计费" })
      .locator(".tab-dot")
      .evaluate((el) => el.getBoundingClientRect().width);
    expect(dotSize).toBeGreaterThanOrEqual(5);
    await saveBtn.click();
    await expect(page.getByText("本页配置已保存并即时生效")).toBeVisible();

    expect(payloads).toHaveLength(1);
    // 只带计费页签的 6 个键 + site（币种是计费页签的字段）。
    // 支付渠道已是独立页签；通知/模型/安全等未改动的页签**不能**进包：那些页签里的
    // 半填草稿一旦被顺带提交，整次保存会被后端拒绝，运营只能在「先保存」和「继续填」之间二选一。
    expect(Object.keys(payloads[0]).sort()).toEqual([
      "aff",
      "balance_limit",
      "cny_per_usd",
      "custom_topup_enabled",
      "group_discount",
      "min_topup",
      "site",
    ]);
    expect(payloads[0].cny_per_usd).toBe(7.3);
    // site 只写币种，其余字段回填已落库值
    expect((payloads[0].site as { currency: string; site_name: string }).currency).toBe("CNY");
    expect((payloads[0].site as { site_name: string }).site_name).toBe("灵动 API");

    // 基线已推进：切走再切回来，汇率仍是 7.3、保存按钮重新变灰、圆点消失
    await page.getByRole("tab", { name: "通知" }).click();
    await page.getByRole("tab", { name: "计费" }).click();
    await expect(rate).toHaveValue("7.3");
    await expect(saveBtn).toBeDisabled();
    await expect(page.getByRole("tab", { name: "计费" }).locator(".tab-dot")).toHaveCount(0);

    // 通知页签的改动只提交 email/sms
    await page.getByRole("tab", { name: "通知" }).click();
    await expect(saveBtn).toBeDisabled();
    const mail = card(page, "邮件通知");
    await mail.locator(".switch input").first().check();
    await saveBtn.click();
    // 只等请求落地，不等 toast：上一条成功提示可能还没消失（同文案 toast 会叠两个）
    await expect.poll(() => payloads.length).toBe(2);
    expect(Object.keys(payloads[1]).sort()).toEqual(["email", "sms"]);

    // 支付渠道页签的改动只提交 pay_channels（与计费彻底隔离）
    await page.getByRole("tab", { name: "支付渠道" }).click();
    await expect(saveBtn).toBeDisabled();
    await page.locator(".pc-card", { hasText: "虎皮椒" }).locator(".switch input").check();
    await saveBtn.click();
    await expect.poll(() => payloads.length).toBe(3);
    expect(Object.keys(payloads[2])).toEqual(["pay_channels"]);
  });

  test("二次验证策略：三选项齐全，强制管理员绑定随保存下发", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);

    let payload: Record<string, unknown> | null = null;
    await page.route(
      (url) => url.pathname === "/api/admin/config",
      async (route) => {
        if (route.request().method() === "PUT") payload = route.request().postDataJSON();
        await route.fallback();
      },
    );

    await page.goto("/admin/config");

    // required_for_admin 曾因「登录未接入 TOTP」被挡在下拉之外；现在后端已放开，选项必须齐全
    const policy = page.locator(".form-item", { hasText: "二次验证策略" }).locator("select");
    await expect(policy.locator("option")).toHaveText(["关闭", "用户可选", "强制管理员绑定"]);

    await policy.selectOption("required_for_admin");
    await page.getByRole("button", { name: "保存本页" }).click();
    await expect(page.getByText("本页配置已保存并即时生效")).toBeVisible();

    expect(payload).not.toBeNull();
    expect((payload as { auth_mfa?: { policy?: string } }).auth_mfa?.policy).toBe("required_for_admin");
    // 注册与准入与二次验证同属「通用」页签：两者一起提交，不会拆到两个分片里
    expect(Object.keys(payload as unknown as Record<string, unknown>)).toContain("auth_register");
  });

  test("支付渠道：方式多选/单选、回调地址展示、新增停用渠道、参数按协议渲染、删除二次确认", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);

    let payload: Record<string, unknown> | null = null;
    await page.route(
      (url) => url.pathname === "/api/admin/config",
      async (route) => {
        if (route.request().method() === "PUT") payload = route.request().postDataJSON();
        await route.fallback();
      },
    );

    await page.goto("/admin/config");
    await page.getByRole("tab", { name: "支付渠道" }).click();
    const pc = page.locator(".card", { hasText: "支付渠道" });

    // 易支付：一个商户号通吃 → 两种方式可同时选中
    const epay = pc.locator(".pc-card", { hasText: "易支付" });
    await expect(epay.locator(".kbtn", { hasText: "微信支付" })).toHaveClass(/active/);
    await expect(epay.locator(".kbtn", { hasText: "支付宝" })).toHaveClass(/active/);
    // 参数区由 schema 渲染：易支付 basic 恰好 3 项（网关 / 商户 PID / 商户密钥）。
    // 「结算币种」不在参数里 —— 它是**协议属性**（schema.Currencies），
    // 归到参数区会让同一个币种在每套参数里各存一份（铁律 24）。
    await expect(epay.locator(".pc-params").first().locator(".form-item")).toHaveCount(3);
    // 官方直连预留字段（mch_id 等）不再出现在任何协议的表单里。
    // 必须 scope 到 .pc-params：支付方式的问号文案里也含「商户号」三个字
    // （Tip 的隐藏描述节点参与 hasText 匹配）。
    await expect(epay.locator(".pc-params .form-item", { hasText: "商户号" })).toHaveCount(0);

    // 虎皮椒：微信/支付宝是两套 APPID → 单选，第二个点上去会顶掉第一个
    const xunhu = pc.locator(".pc-card", { hasText: "虎皮椒" });
    await expect(xunhu.locator(".kbtn", { hasText: "微信支付" })).toHaveClass(/active/);
    await xunhu.locator(".kbtn", { hasText: "支付宝" }).click();
    await expect(xunhu.locator(".kbtn", { hasText: "支付宝" })).toHaveClass(/active/);
    await expect(xunhu.locator(".kbtn", { hasText: "微信支付" })).not.toHaveClass(/active/);

    // 实际回调地址必须可复制：运营漏配到上游后台，该类渠道永远不入账
    await expect(xunhu.locator(".pc-notify")).toContainText(
      "https://api.example.com/api/pay/xunhupay/notify",
    );
    // callback_url 已声明为 hidden：它配了也不生效（真实地址由后端生成）
    await expect(xunhu.locator(".form-item", { hasText: "异步通知地址" })).toHaveCount(0);

    // 新增渠道：一律停用落地（还没填密钥就启用 = 给用户一个必然失败的选项）
    await pc.getByRole("button", { name: "新增渠道" }).click();
    const dlg = page.getByRole("dialog", { name: "新增支付渠道" });
    await expect(dlg).toBeVisible();
    // 默认协议是 schemas 里的第一个（epay），这里显式切到虎皮椒
    await dlg.locator(".kbtn", { hasText: "虎皮椒" }).click();
    await dlg.locator(".form-item", { hasText: "渠道名称" }).locator("input").fill("虎皮椒（微信二号）");
    await dlg.locator(".kbtn", { hasText: "微信支付" }).click();
    await dlg.getByRole("button", { name: "新增" }).click();
    await expect(page.getByText("渠道已新增")).toBeVisible();
    const created = pc.locator(".pc-card", { hasText: "虎皮椒（微信二号）" });
    await expect(created).toBeVisible();
    await expect(created.locator(".switch input")).not.toBeChecked();
    // 新渠道的参数键集由 schema 决定：4 项全平铺（网关/APPID/APP SECRET/结算币种），高级设置折叠区已彻底移除
    // 虎皮椒 basic 3 项（网关 / APPID / APP SECRET）；结算币种是协议属性，不在参数区
    await expect(created.locator(".pc-params").first().locator(".form-item")).toHaveCount(3);
    await expect(created.locator(".pc-adv")).toHaveCount(0);
    await expect(created.locator(".form-item", { hasText: "异步通知地址" })).toHaveCount(0);
    await expect(created.locator(".form-item", { hasText: "支付后跳回地址" })).toHaveCount(0);
    await expect(created.locator(".form-item", { hasText: "查单地址" })).toHaveCount(0);
    await expect(created.locator(".form-item", { hasText: "退款地址" })).toHaveCount(0);
    // 密钥初始为空 → 标「未设置」（历史渠道回显 ****** 时标「已设置」）
    await expect(created.locator(".form-item", { hasText: "APP SECRET" })).toContainText("未设置");

    // 排序：顺序即「同一种支付方式的优先级」。前端只改顺序，密钥值必须仍在各自卡片里
    // （真正的「按 id 对齐脱敏回填」由 Go 单测 TestRestoreSensitiveArrayReorderedById 保证）
    await created.getByRole("button", { name: "↑" }).click();
    // 原顺序 [易支付, 虎皮椒, 虎皮椒二号] → 上移后变成第 2 张
    await expect(pc.locator(".pc-card").nth(1)).toContainText("虎皮椒（微信二号）");
    await expect(epay.locator("input[type='password']")).toHaveValue("******");

    // 保存后提交的包里，新渠道必须是枚举 code 且 enabled=false
    await page.getByRole("button", { name: "保存本页" }).click();
    await expect(page.getByText("本页配置已保存并即时生效")).toBeVisible();
    const channels = (
      payload as unknown as { pay_channels: { id: string; methods: string[]; enabled: boolean; params: object }[] }
    ).pay_channels;
    const added = channels.find((c) => c.id === "xunhupay-2");
    expect(added).toMatchObject({ enabled: false, methods: ["wechat"] });
    // 提交体键集确定（fillDefaults 契约）：只含 schema 声明的 3 个字段。
    // 查单/退款/跳回/异步通知已从界面移除，新渠道不再带这些键；
    // 结算币种也不再是渠道参数（虎皮椒恒为人民币，由协议决定）。
    expect(Object.keys(added!.params).sort()).toEqual(["app_id", "app_secret", "pay_address"]);

    // 删除要二次确认
    await created.getByRole("button", { name: "删除" }).click();
    const confirm = page.getByRole("dialog", { name: "删除支付渠道" });
    await expect(confirm).toContainText("只能回退平台余额");
    await expect(confirm).toContainText("保存后才会真正删除");
    await confirm.getByRole("button", { name: "删除" }).click();
    await expect(pc.locator(".pc-card", { hasText: "虎皮椒（微信二号）" })).toHaveCount(0);

    // 删除只改表单态，必须显式说「保存后生效」并给撤销 ——
    // 否则后端一旦因在途订单拒绝，运营看到卡片消失就以为删成功了。
    const pending = pc.locator(".pc-pending-del");
    await expect(pending).toContainText("保存后生效");
    await pending.getByRole("button", { name: "撤销" }).click();
    await expect(pending).toHaveCount(0);
    await expect(pc.locator(".pc-card", { hasText: "虎皮椒（微信二号）" })).toHaveCount(1);
  });

  test("官方直连：方式按协议白名单过滤、单方式自动预选、PEM 用多行输入", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);

    let payload: Record<string, unknown> | null = null;
    await page.route(
      (url) => url.pathname === "/api/admin/config",
      async (route) => {
        if (route.request().method() === "PUT") payload = route.request().postDataJSON();
        await route.fallback();
      },
    );

    await page.goto("/admin/config");
    await page.getByRole("tab", { name: "支付渠道" }).click();
    const pc = page.locator(".card", { hasText: "支付渠道" });

    // —— 微信直连：方式区只剩「微信支付」，且已自动选中 ——
    await pc.getByRole("button", { name: "新增渠道" }).click();
    let dlg = page.getByRole("dialog", { name: "新增支付渠道" });
    await dlg.locator(".kbtn", { hasText: "微信直连" }).click();
    // 方式 chips 用 .pcs-methods 单独定位：协议 chips 里也有「微信直连」，用 hasText 会撞多元素。
    await expect(dlg.locator(".pcs-methods .kbtn")).toHaveCount(1);
    await expect(dlg.locator(".pcs-methods .kbtn").first()).toHaveText("微信支付");
    // 单方式协议必须自动预选，否则运营忘点一下就会被后端「启用必须选方式」拦死。
    await expect(dlg.locator(".pcs-methods .kbtn").first()).toHaveClass(/active/);
    // ID 按协议给建议（创建后不可改）
    await expect(dlg.locator(".form-item", { hasText: "渠道 ID" }).locator("input")).toHaveValue("wxpay");
    await dlg.locator(".form-item", { hasText: "渠道名称" }).locator("input").fill("微信直连一号");
    await dlg.getByRole("button", { name: "新增" }).click();
    await expect(page.getByText("渠道已新增")).toBeVisible();

    const wx = pc.locator(".pc-card", { hasText: "微信直连一号" });
    await expect(wx).toBeVisible();
    // 验签方式默认公钥 → 字段全平铺（无折叠区）。
    //
    // 这里**不用总项数**断言：`show_when` 字段的可见性取决于已填值，而密钥是异步
    // 解密回填的 —— 数总数会随加载时序在 6/7 之间跳（曾因此红过两次）。
    // 逐字段断言既能钉住"字段集"，又与时序无关。
    // 结算币种不在参数区：它是协议属性（schema.Currencies）。
    await expect(wx.locator(".pc-mode .kbtn")).toHaveText(["微信支付公钥", "平台证书"]);
    await expect(wx.locator(".pc-mode .kbtn").first()).toHaveClass(/active/);
    for (const label of ["AppID", "商户号", "证书序列号", "APIv3 密钥", "商户私钥", "微信支付公钥", "公钥 ID"]) {
      await expect(
        wx.locator(".pc-params .form-item", { hasText: label }),
        `公钥模式应有「${label}」`,
      ).toHaveCount(1);
    }
    // PEM 必须是多行文本框（单行 input 无法核对 BEGIN/END 行）：商户私钥 + 微信支付公钥
    await expect(wx.locator(".pc-params").first().locator("textarea")).toHaveCount(2);
    // scope 到 .pc-params：「验签方式」这一行也是 .form-item，且 chip 文本里就有「平台证书」
    await expect(wx.locator(".pc-params .form-item", { hasText: "平台证书" })).toHaveCount(0);
    // 微信没有 http 地址兜底能力，return_url 对它是死配置 → 不再渲染
    await expect(wx.locator(".form-item", { hasText: "支付后跳回地址" })).toHaveCount(0);

    // —— 切到「平台证书」：公钥两框让位给「平台证书」——
    // 互斥字段只显示一组：这条只能用「有/无」断言，总数同样不可靠（见上）。
    await wx.locator(".pc-mode .kbtn", { hasText: "平台证书" }).click();
    await expect(wx.locator(".pc-params .form-item", { hasText: "平台证书" })).toHaveCount(1);
    await expect(wx.locator(".pc-params .form-item", { hasText: "微信支付公钥" })).toHaveCount(0);
    await expect(wx.locator(".pc-params .form-item", { hasText: "公钥 ID" })).toHaveCount(0);
    // 切回公钥模式：公钥两框回来
    await wx.locator(".pc-mode .kbtn", { hasText: "微信支付公钥" }).click();
    await expect(wx.locator(".pc-params .form-item", { hasText: "微信支付公钥" })).toHaveCount(1);

    // —— 支付宝直连：方式区只剩「支付宝」，网关预填正式地址 ——
    await pc.getByRole("button", { name: "新增渠道" }).click();
    dlg = page.getByRole("dialog", { name: "新增支付渠道" });
    await dlg.locator(".kbtn", { hasText: "支付宝直连" }).click();
    await expect(dlg.locator(".pcs-methods .kbtn")).toHaveCount(1);
    await expect(dlg.locator(".pcs-methods .kbtn").first()).toHaveText("支付宝");
    await expect(dlg.locator(".pcs-methods .kbtn").first()).toHaveClass(/active/);
    await dlg.locator(".form-item", { hasText: "渠道名称" }).locator("input").fill("支付宝直连一号");
    await dlg.getByRole("button", { name: "新增" }).click();
    // 不在这里断言 toast：上一条提示可能还没消失，会造成 strict 多元素；卡片出现即等价。

    const ali = pc.locator(".pc-card", { hasText: "支付宝直连一号" });
    await expect(ali).toBeVisible();
    // basic 5 项：应用 AppID / 网关地址 / 应用私钥 / 支付宝公钥 / 结算币种
    // 支付宝直连 4 项（AppID / 网关 / 应用私钥 / 支付宝公钥）；结算币种是协议属性
    await expect(ali.locator(".pc-params").first().locator(".form-item")).toHaveCount(4);
    await expect(
      ali.locator(".form-item", { hasText: "网关地址" }).locator("input"),
    ).toHaveValue("https://openapi.alipay.com/gateway.do");
    // 两个 PEM 字段（应用私钥 + 支付宝公钥）都是多行
    await expect(ali.locator(".pc-params").first().locator("textarea")).toHaveCount(2);

    // —— 保存后的请求体：方式只能是协议允许的那个，参数键集由 schema 决定 ——
    await page.getByRole("button", { name: "保存本页" }).click();
    await expect(page.getByText("本页配置已保存并即时生效")).toBeVisible();

    const channels = (
      payload as unknown as {
        pay_channels: { id: string; protocol: string; methods: string[]; enabled: boolean; params: object }[];
      }
    ).pay_channels;
    const wxSaved = channels.find((c) => c.id === "wxpay");
    expect(wxSaved).toMatchObject({ protocol: "wxpay", enabled: false, methods: ["wechat"] });
    // 8 个非 hidden 键（return_url / callback_url 隐藏，不进请求体；
    // 结算币种已不是渠道参数 —— 微信恒为人民币，由协议决定）
    expect(Object.keys(wxSaved!.params).sort()).toEqual([
      "api_v3_key",
      "app_id",
      "cert_serial_no",
      "mch_id",
      "platform_cert",
      "private_key",
      "public_key",
      "public_key_id",
    ]);

    const aliSaved = channels.find((c) => c.id === "alipay");
    expect(aliSaved).toMatchObject({ protocol: "alipay", enabled: false, methods: ["alipay"] });
    expect(Object.keys(aliSaved!.params).sort()).toEqual([
      "app_id",
      "pay_address",
      "private_key",
      "public_key",
    ]);
  });

  test("支付渠道空态：只保留一个添加入口，点击即打开新增弹窗", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page, { emptyPayChannels: true });

    await page.goto("/admin/config");
    await page.getByRole("tab", { name: "支付渠道" }).click();
    const pc = page.locator(".card", { hasText: "支付渠道" });

    await expect(pc.locator(".empty")).toContainText("还没有支付渠道");
    // 空态时不渲染卡头动作：否则页面上会同时出现两个「添加」按钮（e2e strict 也会撞）
    await expect(pc.getByRole("button", { name: /添加支付渠道|新增渠道/ })).toHaveCount(1);
    // 空列表时不应渲染任何卡片与参数区
    await expect(pc.locator(".pc-card")).toHaveCount(0);
    await expect(pc.locator(".pc-grid")).toHaveCount(0);

    await pc.getByRole("button", { name: "添加支付渠道" }).click();
    await expect(page.getByRole("dialog", { name: "新增支付渠道" })).toBeVisible();
  });

  test("默认模型：候选仅列已启用模型，含当前值兜底项", async ({ page }) => {
    await seedAuth(page, { role: 0 });
    await stub(page);

    await page.goto("/admin/config");
    await page.getByRole("tab", { name: "模型与调用" }).click();

    const grid = page.locator(".default-grid .dcard");
    // 8 类模态（chat/image/video/audio/music/embedding/rerank/moderation）各一张卡
    await expect(grid).toHaveCount(8);
    const chat = page.locator(".dcard", { hasText: "文本对话" });
    // 控件是带图标的 select-only combobox（原生 option 放不了图标）：
    // 收起时只显示**当前值**，全部候选在展开后的 listbox 里。
    const picker = chat.getByRole("combobox", { name: "文本对话默认模型" });
    await expect(picker).toContainText("GPT-4o");
    await picker.click();
    const list = page.getByRole("listbox", { name: "文本对话默认模型" });
    // 首项恒为「自动选择」（空值 = 交给派生逻辑回落到首个在售模型）：没有它，
    // 运营想把某类交回自动就无路可走，只能钉死一个具体模型。
    await expect(list.getByRole("option").first()).toHaveText("自动选择");
    await expect(list.getByRole("option")).toHaveCount(3);
    await expect(list).toContainText("GPT-4o mini");
    await expect(list.getByRole("option").filter({ hasText: "Claude Sonnet" })).toHaveCount(0);
  });

  test("门户品牌与公告入口：页脚与铃铛弹层消费配置", async ({ page }) => {
    await seedAuth(page, { role: 1, username: "alice" });
    const calls = await stub(page);

    await page.goto("/home");
    await expect(page.locator(".p-foot")).toContainText("灵动 API · 大模型开放平台");
    await expect(page.locator(".p-foot")).toContainText("京ICP备00000000号-1");

    // 铃铛是按钮：点击开弹层，不再跳页
    const bell = page.getByRole("button", { name: /公告/ });
    await expect(bell.locator(".nt-badge")).toHaveText("1");
    await bell.click();

    const panel = page.getByRole("dialog", { name: "站点公告" });
    await expect(panel).toBeVisible();
    await expect(page).toHaveURL(/\/home$/);
    await expect(panel.locator(".notice-list")).toContainText("充值活动");

    // 打开即整组已读：服务端回同形状 payload → 顶栏徽标清空
    await expect(bell.locator(".nt-badge")).toHaveCount(0);
    expect(calls).toContain("POST /api/portal/announcements/read");
  });
});
