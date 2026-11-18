/** S20 契约级（stub）用例：page.route 打桩后端响应，验证前端区块渲染与请求契约；真实后端联调用例见 real-backend.spec.ts */
import { expect, test, type Page } from "@playwright/test";
import { envelope, expectNoHScroll, seedAuth } from "./helpers";

/** 门户用户信息（门户 chrome 加载余额口径） */
const ME = {
  id: 1,
  username: "e2e",
  role: 1,
  status: 1,
  group: "default",
  balance: 5000000,
  frozen: 0,
  used: 0,
  request_count: 0,
  key_count: 0,
  key_count_enabled: 0,
  group_discount: 1_000_000,
  group_configured: false,
  email: "",
  phone: "",
};

/** 文件配置桩件（总闸开启；today_count=3 / limit=100；在存 1229 MB / 2 GB） */
const CONFIG = {
  enabled: true,
  daily_limit_per_key: 100,
  retention_hours: 24,
  user_total_mb: 2048,
  concurrency: 5,
  today_count: 3,
  total_bytes: 1288490188,
  // 计数主体由服务端下发（门户为账号）；后缀与上限同理，前端不再自带映射
  quota_scope: "account",
  types: [
    { ext: "png", kind: "image", max_mb: 20 },
    { ext: "jpg", kind: "image", max_mb: 20 },
    { ext: "webp", kind: "image", max_mb: 20 },
    { ext: "pdf", kind: "document", max_mb: 30 },
    { ext: "txt", kind: "text", max_mb: 5 },
    { ext: "mp4", kind: "video", max_mb: 200 },
  ],
  limits: { image: 20, audio: 100, video: 200, document: 30, text: 5 },
};

/** 文件条目桩件（图片 2h 剩余 ok 态；视频 30m 剩余 danger 态） */
const FILES = [
  {
    id: "01J8ZQ7W6Y5X4V3T2S1R0Q9P8N",
    filename: "cat.png",
    safe_name: "cat.png",
    size_bytes: 2845312,
    content_type: "image/png",
    url: "https://api.example.com/f/01J8ZQ7W6Y5X4V3T2S1R0Q9P8N/cat.png",
    key_id: 1,
    key_name: "生产密钥",
    created_at: "2026-10-20T02:00:00Z",
    expires_at: "2026-10-21T02:00:00Z",
    remaining_seconds: 7200,
  },
  {
    id: "01J8ZQ7W6Y5X4V3T2S1R0Q9P8M",
    filename: "demo.mp4",
    safe_name: "demo.mp4",
    size_bytes: 20971520,
    content_type: "video/mp4",
    url: "https://api.example.com/f/01J8ZQ7W6Y5X4V3T2S1R0Q9P8M/demo.mp4",
    key_id: 2,
    key_name: "演示密钥",
    created_at: "2026-10-20T03:00:00Z",
    expires_at: "2026-10-21T03:00:00Z",
    remaining_seconds: 1800,
  },
];

/** 打桩：/api/portal/files 与 /api/portal/files/config（按路径 + 方法分流，避免路由顺序耦合） */
async function stubFiles(
  page: Page,
  config: typeof CONFIG = CONFIG,
  onUpload?: (filename: string) => void,
): Promise<void> {
  await page.route("**/api/portal/me", (route) => route.fulfill({ json: envelope(ME) }));
  await page.route("**/api/portal/files**", (route) => {
    const req = route.request();
    const { pathname } = new URL(req.url());
    if (pathname.endsWith("/config")) {
      return route.fulfill({ json: envelope(config) });
    }
    // 门户直传（会话身份，不需要 API KEY）：控制台成功一律 200 + envelope（不是 /v1 的 201）
    if (req.method() === "POST") {
      const created = { ...FILES[0], id: "01J8ZQ7W6Y5X4V3T2S1R0Q9P8Z", filename: "upload.png" };
      onUpload?.(created.filename);
      return route.fulfill({ status: 200, json: envelope(created) });
    }
    return route.fulfill({
      json: envelope({ items: FILES, total: FILES.length, page: 1, page_size: 50 }),
    });
  });
}

test.describe("S8 文件存储 · 门户", () => {
  test("列表 / 配额 / TTL / 直链渲染，删除触发确认弹窗", async ({ page }) => {
    await seedAuth(page);
    await stubFiles(page);

    await page.goto("/files");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("文件存储");

    // 上传卡：拖拽区 + 限额文案（数值取自 config）
    await expect(page.getByTestId("upload-zone")).toBeVisible();
    await expect(page.getByTestId("upload-card")).toContainText("图片 ≤");
    await expect(page.getByTestId("upload-card")).toContainText("有效期");

    // 配额条：计数主体由服务端下发（门户 = 账号，不是「本 KEY」）+ 在存容量（MB→GB 自适应）
    const quota = page.getByTestId("files-quota");
    await expect(quota).toContainText("3 / 100");
    await expect(quota).toContainText("今日上传（账号）");
    await expect(page.getByTestId("upload-card")).toContainText("每日 100 次");
    await expect(page.getByTestId("quota-row-storage")).toContainText("2 GB");

    // 文件条目：类型徽标 / 文件名 / TTL 双态 / mono 直链 / KEY（存储后端不下发）
    const cards = page.getByTestId("file-card");
    await expect(cards).toHaveCount(2);
    await expect(cards.first().getByTestId("file-type")).toHaveText("图片");
    await expect(cards.first().getByTestId("file-name")).toHaveText("cat.png");
    await expect(cards.first().getByTestId("file-ttl")).toHaveText("剩 2 小时 0 分钟");
    await expect(cards.first()).toContainText("KEY 生产密钥");
    await expect(cards.first().getByTestId("file-url")).toHaveText(FILES[0].url);
    await expect(cards.nth(1).getByTestId("file-type")).toHaveText("视频");
    await expect(cards.nth(1).getByTestId("file-ttl")).toHaveText("剩 30 分钟");

    // 删除 → 确认弹窗（文案含 410 失效说明）
    await cards.first().getByRole("button", { name: "删除" }).click();
    await expect(page.getByRole("dialog")).toBeVisible();
    await expect(page.getByRole("dialog")).toContainText("删除文件");
    await expect(page.getByRole("dialog")).toContainText("存储对象与记录将立即清除");
  });

  test("门户直传：选文件即 POST /portal/files（无需 API KEY），成功刷新列表", async ({ page }) => {
    await seedAuth(page);
    const uploaded: string[] = [];
    await stubFiles(page, CONFIG, (name) => uploaded.push(name));

    await page.goto("/files");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("文件存储");

    await page.getByTestId("upload-zone").locator('input[type="file"]').setInputFiles({
      name: "upload.png",
      mimeType: "image/png",
      buffer: Buffer.from("89504e470d0a1a0a", "hex"),
    });

    await expect.poll(() => uploaded).toEqual(["upload.png"]);
    await expect(page.getByText("上传成功，直链已生成")).toBeVisible();
  });

  test("本地前置校验：白名单外的扩展名不发请求", async ({ page }) => {
    await seedAuth(page);
    const uploaded: string[] = [];
    await stubFiles(page, CONFIG, (name) => uploaded.push(name));

    await page.goto("/files");
    await expect(page.getByTestId("upload-zone")).toBeVisible();

    await page.getByTestId("upload-zone").locator('input[type="file"]').setInputFiles({
      name: "evil.exe",
      mimeType: "application/octet-stream",
      buffer: Buffer.from("MZ", "utf8"),
    });

    await expect(page.getByText(/不支持 \.exe/)).toBeVisible();
    expect(uploaded).toEqual([]);
  });

  test("无筛选区且翻页透传 page 到 /portal/files", async ({ page }) => {
    const urls: string[] = [];
    await seedAuth(page);
    await page.route("**/api/portal/me", (route) => route.fulfill({ json: envelope(ME) }));
    await page.route("**/api/portal/files**", (route) => {
      const { pathname, searchParams } = new URL(route.request().url());
      if (pathname.endsWith("/config")) return route.fulfill({ json: envelope(CONFIG) });
      urls.push(route.request().url());
      return route.fulfill({
        json: envelope({ items: FILES, total: 60, page: Number(searchParams.get("page") ?? 1), page_size: 20 }),
      });
    });

    await page.goto("/files");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("文件存储");
    await expect.poll(() => urls.some((u) => u.includes("page=1"))).toBe(true);
    expect(urls.some((u) => u.includes("key_id=")), "无筛选区时不应携带 key_id").toBe(false);

    // 原型无筛选区：FilterBar 已删除
    await expect(page.locator(".pbar-wrap")).toHaveCount(0);

    // 翻页 → 请求 URL 携带 page=2
    await page.getByRole("button", { name: "下一页" }).click();
    await expect.poll(() => urls.some((u) => u.includes("page=2"))).toBe(true);
  });
});

test.describe("S8 文件存储 · 总闸关闭（375px）", () => {
  test.use({ viewport: { width: 375, height: 800 } });

  test("上传卡隐藏、横幅可见、列表仍渲染且无横向滚动", async ({ page }) => {
    await seedAuth(page);
    await stubFiles(page, { ...CONFIG, enabled: false });

    await page.goto("/files");
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("文件存储");

    // 上传卡隐藏，顶部信息横幅可见
    await expect(page.getByTestId("upload-card")).toHaveCount(0);
    await expect(page.getByTestId("files-gate-banner")).toContainText("管理员已关闭文件上传");

    // 存量文件列表与删除操作照常
    await expect(page.getByTestId("file-card")).toHaveCount(2);
    await expect(page.getByTestId("file-card").first().getByRole("button", { name: "删除" })).toBeVisible();

    // 375px 无横向滚动
    await expectNoHScroll(page, "375px 出现横向溢出");
  });
});
