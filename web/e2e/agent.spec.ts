/**
 * AI 接入助手（/admin/agent）· 对话式工作台。
 *
 * 契约级（stub）用例，覆盖三件事：
 *  1. **过程顺序**：思考 / 工具 / 提案 / 答复按发生顺序内联，而不是堆在末尾；
 *  2. **写入边界**：AI 只能产出提案，写生产表必须人工确认（且回传 plan_hash）；
 *  3. **配置入口**：全部收在输入框工具栏的弹窗里，页面没有常驻表单。
 *
 * 真实后端联调在 real-backend.spec.ts（只读、不产生 LLM 成本）。
 */
import { expect, test, type Page, type Route } from "@playwright/test";
import { envelope, seedAuth } from "./helpers";

const seedAdmin = (page: Page) => seedAuth(page, { role: 0 });

const SETTINGS_OK = {
  item: {
    id: 1,
    enabled: true,
    model_id: "",
    max_steps: 12,
    max_tokens_per_run: 2_000_000,
    context_window: 0,
    max_output_tokens: 0,
    // 字段名必须与后端 JSON 一致：曾用 daily_quota_budget 做 mock，
    // 于是"页面发错字段名"这个 bug 在 e2e 里永远是绿的。
    daily_amount_budget: 0,
    dry_run_required: true,
  },
  available: true,
  effective_model_id: "gpt-4o-mini",
  today_spend: 120,
  limits: {
    max_steps: [1, 40],
    max_tokens_per_run: [1000, 20_000_000],
    context_window: [32_768, 10_000_000],
    max_output_tokens: [256, 32_768],
  },
  chat_models: [
    { id: "gpt-4o-mini", display_name: "GPT-4o mini", vendor_name: "OpenAI", is_default: true },
    { id: "deepseek-chat", display_name: "DeepSeek Chat", vendor_name: "DeepSeek", is_default: false },
  ],
};

const STATS = {
  days: 7,
  runs: 3,
  failed: 0,
  quota_est: 900,
  tokens_in: 1200,
  tokens_out: 300,
  proposals: 2,
  applied: 1,
  avg_latency_ms: 2100,
  daily_budget: 0,
  today_spend: 120,
};

const SESSION = {
  id: 7,
  title: "接入可灵视频",
  model_id: "",
  status: 1,
  last_seq: 2,
  created_at: "2026-10-25T10:00:00Z",
  updated_at: "2026-10-25T10:01:00Z",
};

const PROPOSAL_DRAFT = {
  id: 11,
  session_id: 7,
  message_id: 2,
  kind: "change",
  status: "draft",
  plan: { summary: "新建可灵规格与渠道", operations: [{}, {}, {}] },
  plan_hash: "abc123",
  diff: {
    stats: { added: 3, changed: 0, removed: 0 },
    items: [
      { op: "create", entity: "channel", key: "可灵-主渠道" },
      { op: "create", entity: "model", key: "kling-v3" },
      { op: "create", entity: "model_bindings", id: 9 },
    ],
  },
  error: "",
  applied_at: null,
  created_at: "2026-10-25T10:01:00Z",
};

const PROPOSAL_STALE = {
  ...PROPOSAL_DRAFT,
  id: 12,
  status: "stale",
  plan_hash: "def456",
  plan: { summary: "改价：可灵 720p", operations: [{}] },
};

const USER_MSG = {
  id: 1,
  session_id: 7,
  seq: 1,
  role: "user",
  content: "帮我把可灵视频接进来",
  parts: null,
  status: "ok",
  created_at: "2026-10-25T10:00:30Z",
};

/** 落库的一轮：思考 → 工具 → 提案 → 答复（parts.items 是过程回放的唯一来源）。 */
const MESSAGES = [
  USER_MSG,
  {
    id: 2,
    session_id: 7,
    seq: 2,
    role: "assistant",
    content: "已生成提案，请确认后我再继续。",
    parts: {
      items: [
        { k: "reason", t: "先看端点，再看计价格。" },
        { k: "text", t: "我先看一下平台支持的端点。" },
        { k: "tool", n: "schema_get", a: '{"section":"endpoints"}', b: '{"ok":true}', ok: true, s: 1, ms: 12 },
        { k: "proposal", id: 11, t: "新建可灵规格与渠道" },
      ],
      tools: ["schema_get"],
      proposals: [11],
    },
    status: "ok",
    created_at: "2026-10-25T10:01:00Z",
  },
];

/** 同一轮但提案已被判 stale（数据在生成后被改过）。 */
const MESSAGES_STALE = [
  USER_MSG,
  {
    ...MESSAGES[1],
    parts: { items: [{ k: "proposal", id: 12 }], proposals: [12] },
  },
];

function sseBody(lines: string[]): string {
  return lines.map((l) => `data: ${l}\n\n`).join("");
}

const SSE_OK = sseBody([
  JSON.stringify({ type: "start", run_id: 3, message_id: 2, model: "gpt-4o-mini" }),
  JSON.stringify({ type: "step", step: 1 }),
  JSON.stringify({ type: "reasoning", text: "先确认端点。", step: 1 }),
  JSON.stringify({
    type: "tool_call",
    call_id: "c1",
    name: "schema_get",
    args_brief: '{"section":"endpoints"}',
    step: 1,
  }),
  JSON.stringify({
    type: "tool_result",
    call_id: "c1",
    name: "schema_get",
    ok: true,
    brief: '{"ok":true}',
    ms: 12,
    step: 1,
  }),
  JSON.stringify({ type: "delta", text: "已生成提案，请确认。", step: 2 }),
  JSON.stringify({
    type: "proposal",
    proposal_id: 11,
    kind: "change",
    summary: "新建可灵规格与渠道",
    status: "draft",
    diff_stats: { added: 3, changed: 0, removed: 0 },
    // 后端 emitProposal 恒带 plan_hash：骨架卡片要靠它渲染确认框，
    // 桩件缺这个字段时"骨架路径"永远测不到（而它会静默退回"读取中"）。
    plan_hash: "h11",
  }),
  JSON.stringify({ type: "usage", tokens_in: 100, tokens_out: 40, quota_est: 70, steps: 2 }),
  JSON.stringify({ type: "done", message_id: 2, partial: false }),
]);

/** 待答卡片（与后端 AskViewOf 同构：expires_in 是剩余秒数，不是时间戳）。 */
const PENDING_ASK = {
  id: 41,
  ask: {
    question: "这个模型按次还是按规格计费？",
    why: "决定 quota_type 与 spec_prices，猜错要重做整条链路",
    options: [
      { value: "per_call", label: "按次计费", detail: "每次 0.02，长视频更亏" },
      { value: "per_spec", label: "按规格", detail: "按秒计费" },
    ],
    multi: false,
    allow_free_text: true,
    expires_in: 60,
  },
};

/**
 * 作答之后服务端会追加的那条 assistant 消息：ask 段已结算成 answered。
 *
 * 桩件必须造出它，否则"POST 成功后界面切成已回答"这条断言只能测到半个流程。
 */
function answeredMessage() {
  return {
    id: 102,
    session_id: 7,
    seq: 102,
    role: "assistant",
    content: "按规格计费，已据此配置 spec_prices。",
    parts: {
      // 末段 text 必须**不出现**：它等于 content，写进 items 会让正文渲染两遍
      // （后端 buildParts 会剔除它，桩件不剔除就复现不了真实形态）。
      items: [
        { k: "ask", q: PENDING_ASK.ask.question, w: PENDING_ASK.ask.why, o: PENDING_ASK.ask.options, ft: true, st: "answered", ans: "按规格", aid: 41 },
      ],
    },
    status: "ok",
    created_at: "2026-10-25T10:02:00Z",
  };
}

interface StubOptions {
  settings?: unknown;
  messages?: unknown[];
  proposals?: unknown[];
  sessions?: unknown[];
  /** 覆盖 POST messages 的行为（默认返回完整 SSE） */
  send?: (route: Route) => Promise<void> | void;
  /** 让 GET messages 返回 500（验证"历史加载失败"不被伪装成空会话） */
  messagesFail?: boolean;
  /** 记录 PATCH /sessions/7 的请求体 */
  onPatchSession?: (body: Record<string, unknown>) => void;
  /** GET messages 下发"正在等回答"的卡片（模拟 B 标签页 / 刷新后恢复） */
  pendingAsk?: unknown | null;
  /** 记录 POST /asks/:id/answer 的请求体 */
  onAnswerAsk?: (body: Record<string, unknown>) => void;
  /** 让作答返回指定状态码（410 = 这一轮已结束） */
  answerStatus?: number;
}

/**
 * 按真实后端时序回放一次发送：先落用户消息，再回 SSE，最后（可选）落 assistant。
 *
 * 为什么必须这样桩：后端在开流**之前**就写了用户消息。若桩件不回显它，
 * "失败后整轮被清空"这个缺陷在 e2e 里永远测不到（历史假绿的直接原因）。
 */
function sendWithPersist(
  messages: unknown[],
  sse: string,
  assistant?: Record<string, unknown>,
): (route: Route) => Promise<void> {
  let nextId = 100;
  return async (route) => {
    const body = (route.request().postDataJSON() ?? {}) as { content?: string };
    const userId = nextId++;
    messages.push({
      id: userId,
      session_id: 7,
      seq: userId,
      role: "user",
      content: body.content ?? "",
      parts: null,
      status: "ok",
      created_at: new Date().toISOString(),
    });
    await route.fulfill({ status: 200, contentType: "text/event-stream", body: sse });
    if (assistant) {
      messages.push({
        id: nextId++,
        session_id: 7,
        seq: userId + 1,
        role: "assistant",
        content: "",
        parts: null,
        status: "failed",
        created_at: new Date().toISOString(),
        ...assistant,
      });
    }
  };
}

/**
 * 单个总桩：按 pathname + method 分派。
 *
 * 为什么不用一堆 glob：`/proposals`、`/proposals/*`、`/sessions`、`/sessions/7`、
 * `/sessions/7/messages` 互相是前缀关系，Playwright 的匹配顺序很容易踩坑
 * （历史上就出过"详情拿到列表体"的静默失败）。集中分派只有一处真相。
 */
async function stubAgent(page: Page, opts: StubOptions = {}): Promise<void> {
  const {
    settings = SETTINGS_OK,
    messages = MESSAGES,
    proposals = [PROPOSAL_DRAFT],
    sessions = [SESSION],
    send,
    messagesFail,
    onPatchSession,
    pendingAsk,
    onAnswerAsk,
    answerStatus,
  } = opts;

  // 作答之后的"世界状态"：真实后端会在这一轮结束时把 ask 段落库成 answered，
  // 桩件也必须跟着变 —— 否则测出来的只是"POST 打出去了"，界面还停在 pending。
  let answered = false;

  await page.route("**/api/admin/agent/**", async (route) => {
    const req = route.request();
    const p = new URL(req.url()).pathname;
    const m = req.method();

    if (p === "/api/admin/agent/settings") {
      if (m === "PUT") {
        await route.fulfill({ json: envelope({ item: SETTINGS_OK.item }) });
        return;
      }
      await route.fulfill({ json: envelope(settings) });
      return;
    }
    if (p === "/api/admin/agent/stats") {
      await route.fulfill({ json: envelope(STATS) });
      return;
    }
    if (p === "/api/admin/agent/docs") {
      await route.fulfill({
        json: envelope({
          items: [
            {
              id: 3,
              title: "可灵接口文档",
              source_type: "paste",
              size_bytes: 20480,
              created_at: "2026-10-25T09:00:00Z",
            },
          ],
          limits: { max_bytes: 262144, max_fetch_bytes: 1048576, preview_bytes: 8192 },
        }),
      });
      return;
    }
    if (/^\/api\/admin\/agent\/docs\/\d+\/preview$/.test(p)) {
      await route.fulfill({
        json: envelope({
          id: 3,
          title: "可灵接口文档",
          size_bytes: 20480,
          text: "POST /v1/videos 创建任务",
          sections: [{ index: 0, title: "创建任务", level: 2, start: 0, end: 100 }],
          truncated: false,
        }),
      });
      return;
    }
    if (p === "/api/admin/agent/sessions/7/messages") {
      if (m === "POST") {
        if (send) {
          await send(route);
          return;
        }
        await route.fulfill({ status: 200, contentType: "text/event-stream", body: SSE_OK });
        return;
      }
      if (messagesFail) {
        await route.fulfill({
          status: 500,
          json: { code: 500, message: "加载失败", data: null },
        });
        return;
      }
      if (answered) {
        await route.fulfill({
          json: envelope({
            items: [...messages, answeredMessage()],
            pending_ask: null,
          }),
        });
        return;
      }
      await route.fulfill({
        json: envelope({ items: messages, pending_ask: pendingAsk ?? null }),
      });
      return;
    }
    if (p === "/api/admin/agent/sessions/7" && m === "PATCH") {
      onPatchSession?.((req.postDataJSON() ?? {}) as Record<string, unknown>);
      await route.fulfill({ json: envelope(null) });
      return;
    }
    if (/^\/api\/admin\/agent\/asks\/\d+\/answer$/.test(p)) {
      if (answerStatus) {
        await route.fulfill({
          status: answerStatus,
          json: { code: answerStatus, message: "这一轮已结束", data: null },
        });
        return;
      }
      onAnswerAsk?.((req.postDataJSON() ?? {}) as Record<string, unknown>);
      answered = true;
      await route.fulfill({ json: envelope({ id: 41, status: "answered" }) });
      return;
    }
    if (p === "/api/admin/agent/sessions") {
      if (m === "POST") {
        await route.fulfill({ json: envelope({ item: { ...SESSION, id: 8, title: "" } }) });
        return;
      }
      await route.fulfill({ json: envelope({ items: sessions, total: sessions.length }) });
      return;
    }
    if (p === "/api/admin/agent/proposals") {
      await route.fulfill({ json: envelope({ items: proposals }) });
      return;
    }
    if (/^\/api\/admin\/agent\/proposals\/\d+$/.test(p)) {
      const id = Number(p.split("/").pop());
      const item = (proposals as { id: number }[]).find((x) => x.id === id) ?? PROPOSAL_DRAFT;
      await route.fulfill({
        json: envelope({ item, diff_stats: { added: 3, changed: 0, removed: 0 } }),
      });
      return;
    }
    if (/^\/api\/admin\/agent\/proposals\/\d+\/apply$/.test(p)) {
      await route.fulfill({ json: envelope({ proposal_id: 11, ops: 3, added: 3, diff: {} }) });
      return;
    }
    await route.fulfill({ json: envelope(null) });
  });
}

test("空态给引导卡，点击只填入输入框、不直接发送", async ({ page }) => {
  await seedAdmin(page);
  let sent = 0;
  await stubAgent(page, { sessions: [], messages: [], proposals: [], send: () => void sent++ });
  await page.goto("/admin/agent");

  await expect(page.getByRole("heading", { name: "今天要接什么？" })).toBeVisible();
  await expect(page.getByText("接入一家新供应商")).toBeVisible();

  await page.getByText("给现有模型改价").click();
  await expect(page.getByTestId("agent-input")).toHaveValue(/720p 档价格/);
  expect(sent).toBe(0);
});

test("落库的一轮按顺序渲染：思考 → 工具 → 提案 → 答复", async ({ page }) => {
  await seedAdmin(page);
  await stubAgent(page);
  await page.goto("/admin/agent");

  const turn = page.getByTestId("agent-turn-0");
  await expect(turn).toContainText("帮我把可灵视频接进来");
  // 过程默认收起：思考块只留一行入口，工具组只留一行摘要
  await expect(turn.getByTestId("agent-thinking-0")).toContainText("思考过程");
  await expect(turn.getByTestId("agent-tools")).toContainText("执行过程 · 1 个操作");
  await expect(turn.getByTestId("agent-tools")).toContainText("读取平台字段手册");
  // 提案卡必须在这一轮里（不是全页堆在底部）
  await expect(turn.getByTestId("agent-proposal-11")).toBeVisible();
  await expect(turn).toContainText("已生成提案，请确认后我再继续。");
});

test("发送后流式输出思考、工具与提案，并保持内联顺序", async ({ page }) => {
  await seedAdmin(page);
  await stubAgent(page);
  await page.goto("/admin/agent");

  await page.getByTestId("agent-input").fill("支持哪些端点？");
  await page.getByTestId("agent-send").click();

  const live = page.getByTestId("agent-turn-1");
  await expect(live).toBeVisible();
  // 流可能已经在断言前结束：思考块的文案会在「正在思考…」与「思考过程」之间切换，两边都算对。
  await expect(live).toContainText(/思考/);
  await expect(live.getByTestId("agent-tools")).toBeVisible();
  await expect(live.getByTestId("agent-proposal-11")).toBeVisible();
  await expect(live).toContainText("已生成提案，请确认。");

  // 顺序断言：思考块在答复之前
  const text = await live.innerText();
  expect(text.indexOf("思考")).toBeLessThan(text.indexOf("已生成提案，请确认。"));
});

test("确认应用要过二次确认，并回传 plan_hash 与 confirm", async ({ page }) => {
  await seedAdmin(page);
  await stubAgent(page);
  let applyBody: Record<string, unknown> | null = null;
  await page.route("**/api/admin/agent/proposals/11/apply", async (route) => {
    applyBody = route.request().postDataJSON() as Record<string, unknown>;
    await route.fulfill({ json: envelope({ proposal_id: 11, ops: 3, added: 3, diff: {} }) });
  });
  await page.goto("/admin/agent");

  await page.getByTestId("agent-apply-11").click();
  // 聊天流里按钮会随滚动经过光标，必须再确认一次才写库
  await expect(page.getByRole("dialog")).toContainText("确认写入这 3 项变更");
  await page.getByRole("button", { name: "确认写入" }).click();

  await expect.poll(() => applyBody).not.toBeNull();
  expect(applyBody).toMatchObject({ plan_hash: "abc123", confirm: true });
  await expect(page.getByTestId("agent-proposal-11")).toContainText("已生效");
});

test("取消二次确认时不会写库", async ({ page }) => {
  await seedAdmin(page);
  await stubAgent(page);
  let applied = 0;
  await page.route("**/api/admin/agent/proposals/11/apply", async (route) => {
    applied++;
    await route.fulfill({ json: envelope({ proposal_id: 11, ops: 3 }) });
  });
  await page.goto("/admin/agent");

  await page.getByTestId("agent-apply-11").click();
  await page.getByRole("button", { name: "取消" }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  expect(applied).toBe(0);
});

test("stale 提案只提示重新生成，不提供应用按钮", async ({ page }) => {
  await seedAdmin(page);
  await stubAgent(page, { proposals: [PROPOSAL_STALE], messages: MESSAGES_STALE });
  await page.goto("/admin/agent");

  const card = page.getByTestId("agent-proposal-12");
  await expect(card).toContainText("需要重新生成");
  // 提示必须在动作块之外：stale 时三个动作全不可用，写在块里就永不显示
  await expect(page.getByTestId("agent-stale-12")).toContainText("数据已变化");
  await expect(page.getByTestId("agent-apply-12")).toHaveCount(0);
});

test("没有可用对话模型时禁用输入并说明原因", async ({ page }) => {
  await seedAdmin(page);
  await stubAgent(page, {
    settings: {
      ...SETTINGS_OK,
      available: false,
      reason: "agent_model_unavailable",
      hint: "请先在模型管理配置一个对话模型并绑定到可用渠道",
      effective_model_id: "",
      chat_models: [],
    },
  });
  await page.goto("/admin/agent");

  await expect(page.getByTestId("agent-banner")).toContainText("请先在模型管理配置一个对话模型");
  await expect(page.getByTestId("agent-input")).toBeDisabled();
  await expect(page.getByTestId("agent-send")).toBeDisabled();
});

test("同一会话并发发送返回 409 时提示稍候", async ({ page }) => {
  await seedAdmin(page);
  await stubAgent(page, {
    send: (route) =>
      route.fulfill({
        status: 409,
        json: {
          code: 409,
          message: "该会话正在处理上一条消息，请稍候",
          error_code: "agent_busy",
          data: null,
        },
      }),
  });
  await page.goto("/admin/agent");

  await page.getByTestId("agent-input").fill("再帮我改个价");
  await page.getByTestId("agent-send").click();
  await expect(page.getByTestId("agent-run-error")).toContainText("正在处理上一条消息");
});

test("预算超限以错误事件收尾：过程与错误都保留，落库后刷新仍在", async ({ page }) => {
  await seedAdmin(page);
  // 真实时序：后端先写用户消息、失败后再写一条 failed 的 assistant（含 error 段）。
  const messages = structuredClone(MESSAGES);
  const errSse = sseBody([
    JSON.stringify({ type: "start", run_id: 9, message_id: 3 }),
    JSON.stringify({ type: "delta", text: "正在核对价目…" }),
    // HTTP 已经是 200（头早发出去了），预算闸只能在流里表达。
    JSON.stringify({
      type: "error",
      code: "agent_budget_exceeded",
      message: "今日额度预算已用尽，请明天再试或调高预算",
      retryable: false,
    }),
    JSON.stringify({ type: "usage", steps: 1 }),
    JSON.stringify({ type: "done", message_id: 3, partial: true }),
  ]);
  await stubAgent(page, {
    messages,
    send: sendWithPersist(messages, errSse, {
      status: "failed",
      parts: {
        items: [
          { k: "text", t: "正在核对价目…" },
          {
            k: "error",
            t: "今日额度预算已用尽，请明天再试或调高预算",
            c: "agent_budget_exceeded",
            r: false,
          },
        ],
      },
    }),
  });
  await page.goto("/admin/agent");

  await page.getByTestId("agent-input").fill("帮我接一个视频模型");
  await page.getByTestId("agent-send").click();
  // 流式占位与落库结果必须是**同一处渲染**：收敛完成后错误只该出现一次。
  // 先断言条数而不是直接 toContainText：全量跑时（单 worker、机器负载高）
  // 落库慢半拍，两个视图会短暂并存，`getByTestId` 直接命中 2 个元素会报
  // strict mode violation；而「最终仍剩 2 条」才是真缺陷，所以断言收敛到 1。
  const runError = page.getByTestId("agent-run-error");
  await expect(runError).toHaveCount(1);
  await expect(runError).toContainText("今日额度预算已用尽");
  // 失败不能把过程清空：这一轮的叙述与错误都要在（落库收敛后由同一条管线渲染）。
  const turn = page.getByTestId("agent-turn-1");
  await expect(turn).toContainText("正在核对价目");
  await expect(turn).toContainText("今日额度预算已用尽");
  // 预算类错误给的是设置入口，不是无意义的重试。
  await expect(page.getByRole("button", { name: "去设置" }).first()).toBeVisible();
  // 额度类失败不能把输入框锁死：人要能改口再发
  await expect(page.getByTestId("agent-input")).toBeEnabled();
  await page.getByTestId("agent-input").fill("那先按最低价接一个");
  await expect(page.getByTestId("agent-send")).toBeEnabled();
});

test("流被掐断（没有 done）时标明已停止，内容保留", async ({ page }) => {
  await seedAdmin(page);
  await stubAgent(page, {
    send: (route) =>
      route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: sseBody([
          JSON.stringify({ type: "start", run_id: 9, message_id: 3 }),
          JSON.stringify({ type: "delta", text: "说到一半" }),
        ]),
      }),
  });
  await page.goto("/admin/agent");

  await page.getByTestId("agent-input").fill("继续");
  await page.getByTestId("agent-send").click();
  await expect(page.getByText("已停止，已生成的内容已保留，可以接着说")).toBeVisible();
});

test("运行参数收在弹窗里：页面上没有常驻表单，改完能保存", async ({ page }) => {
  await seedAdmin(page);
  let saved: Record<string, unknown> | null = null;
  await stubAgent(page);
  await page.route("**/api/admin/agent/settings", async (route) => {
    if (route.request().method() === "PUT") {
      saved = route.request().postDataJSON() as Record<string, unknown>;
      await route.fulfill({ json: envelope({ item: SETTINGS_OK.item }) });
      return;
    }
    await route.fulfill({ json: envelope(SETTINGS_OK) });
  });
  await page.goto("/admin/agent");

  // 弹窗是懒打开的：DOM 里可以存在，但打开前不可见 = 页面上没有常驻表单。
  await expect(page.getByLabel("单轮最多调用步数")).not.toBeVisible();
  await page.getByTestId("agent-params-open").click();
  await expect(page.getByTestId("agent-settings")).toBeVisible();
  await page.getByLabel("单轮最多调用步数").fill("20");
  // 每日预算改一个值：字段名错位（daily_quota_budget）时这条会红，
  // 而旧 mock 用的是错名，所以永远发现不了。
  await page.getByLabel("每日成本上限").fill("12.5");
  await page.getByRole("button", { name: "保存" }).click();

  await expect.poll(() => saved).not.toBeNull();
  expect(saved).toMatchObject({ max_steps: 20 });
  // 12.5 元 = 12_500_000 微（金额入参一律微，前端用 MoneyInput 换算）
  expect(saved).toMatchObject({ daily_amount_budget: 12_500_000 });
  expect(saved).not.toHaveProperty("daily_quota_budget");
  await expect(page.getByTestId("agent-stats")).toContainText("轮");
});

test("模型菜单只影响本会话（PATCH model_id）", async ({ page }) => {
  await seedAdmin(page);
  let patched: Record<string, unknown> | null = null;
  await stubAgent(page, { onPatchSession: (b) => (patched = b) });
  await page.goto("/admin/agent");

  await expect(page.getByTestId("agent-model-chip")).toContainText("跟随默认");
  await page.getByTestId("agent-model-chip").click();
  await expect(page.getByTestId("agent-model-dialog")).toBeVisible();
  await page.getByRole("button", { name: /DeepSeek Chat/ }).click();

  await expect.poll(() => patched).not.toBeNull();
  expect(patched).toMatchObject({ model_id: "deepseek-chat" });
});

test("素材文档弹窗：可预览、可勾选引用", async ({ page }) => {
  await seedAdmin(page);
  await stubAgent(page);
  await page.goto("/admin/agent");

  await page.getByTestId("agent-docs-open").click();
  await expect(page.getByTestId("agent-docs")).toContainText("可灵接口文档");
  await page.getByText("可灵接口文档").click();
  await expect(page.getByText("POST /v1/videos 创建任务")).toBeVisible();

  await page.getByTestId("agent-doc-pick-3").click();
  await page.keyboard.press("Escape");
  // 引用后输入框上方出现 chip
  await expect(page.getByTestId("agent-quote-3")).toBeVisible();
});

test("会话栏可重命名（PATCH 带 title）", async ({ page }) => {
  await seedAdmin(page);
  let patched: Record<string, unknown> | null = null;
  await stubAgent(page, { onPatchSession: (b) => (patched = b) });
  await page.goto("/admin/agent");

  // 操作按钮只在 hover/聚焦时出现（常态不堆图标）
  await page.getByTestId("agent-session-7").hover();
  await page.getByRole("button", { name: "重命名 接入可灵视频" }).click();
  const input = page.getByTestId("agent-rename-input");
  await input.fill("可灵视频接入（二期）");
  await input.press("Enter");

  await expect.poll(() => patched).not.toBeNull();
  expect(patched).toMatchObject({ title: "可灵视频接入（二期）" });
});

test("回车发送、Shift+Enter 换行（输入法组合期不发）", async ({ page }) => {
  await seedAdmin(page);
  let posts = 0;
  await stubAgent(page, {
    send: (route) => {
      posts++;
      return route.fulfill({ status: 200, contentType: "text/event-stream", body: SSE_OK });
    },
  });
  await page.goto("/admin/agent");

  const input = page.getByTestId("agent-input");
  await input.fill("第一行");
  // Shift+Enter 只是换行，不能触发发送
  await input.press("Shift+Enter");
  await input.type("第二行");
  expect(posts).toBe(0);
  await expect(input).toHaveValue(/第一行\n第二行/);

  await input.press("Enter");
  await expect.poll(() => posts).toBe(1);
  await expect(input).toHaveValue("");
});

test("思考块可手动展开，且展开状态不被后续内容重置", async ({ page }) => {
  await seedAdmin(page);
  await stubAgent(page);
  await page.goto("/admin/agent");

  const think = page.getByTestId("agent-thinking-0");
  const toggle = think.getByRole("button");
  // 收起态：只有一行标题（带开头预览），正文不渲染
  await expect(toggle).toHaveAttribute("aria-expanded", "false");
  await expect(think.locator(".agent-think__body")).toHaveCount(0);
  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
  await expect(think.locator(".agent-think__body")).toHaveText("先看端点，再看计价格。");
  await toggle.click();
  await expect(think.locator(".agent-think__body")).toHaveCount(0);
});

test("提案骨架即时渲染：列表还没刷新时也能看到卡片，且骨架不给应用按钮", async ({ page }) => {
  await seedAdmin(page);
  await stubAgent(page, {
    // 列表为空 = 模拟"提案刚生成、列表 query 尚未 refetch"的瞬间。
    // 这时只有 SSE 事件里的摘要可用，卡片必须靠它渲染出来。
    proposals: [],
  });
  await page.goto("/admin/agent");
  await page.getByTestId("agent-input").fill("接入可灵");
  await page.getByTestId("agent-send").click();

  const card = page.getByTestId("agent-proposal-11");
  await expect(card).toBeVisible();
  await expect(card).toContainText("新建可灵规格与渠道");
  // 骨架阶段不能给"确认应用"：运营还没看到改动明细就被引导点确认。
  await expect(page.getByTestId("agent-apply-11")).toHaveCount(0);
});

/* ------------------------- 对话稳定性（失败保留 / 折叠 / 贴底） ------------------------- */

/** 长输出：120 行足够让对话区出现滚动条（贴底与"有新内容"都依赖它）。 */
function longSse(prefix: string): string {
  const lines = Array.from({ length: 120 }, (_, i) =>
    JSON.stringify({
      type: "delta",
      text: `第 ${i + 1} 行：${prefix}，这里是一段用于撑高对话区的输出内容。\n`,
      step: 1,
    }),
  );
  return sseBody([
    JSON.stringify({ type: "start", run_id: 11, message_id: 3, model: "gpt-4o-mini" }),
    JSON.stringify({ type: "reasoning", text: "先确认端点，再逐档核对价格。", step: 1 }),
    ...lines,
    JSON.stringify({ type: "usage", steps: 1 }),
    JSON.stringify({ type: "done", message_id: 3, partial: false }),
  ]);
}

/** 对话区距底的像素数（贴底断言用）。 */
function distanceToBottom(page: Page): Promise<number> {
  return page.evaluate(() => {
    const el = document.querySelector('[data-testid="agent-stream"]') as HTMLElement | null;
    if (!el) return 99999;
    return el.scrollHeight - el.scrollTop - el.clientHeight;
  });
}

test("上游报错但后端未落 assistant 时：实时轮的过程与错误不得被清空", async ({ page }) => {
  await seedAdmin(page);
  const messages = structuredClone(MESSAGES);
  const errSse = sseBody([
    JSON.stringify({ type: "start", run_id: 12, message_id: 3 }),
    JSON.stringify({ type: "reasoning", text: "先读一下厂商文档。", step: 1 }),
    JSON.stringify({
      type: "tool_call", call_id: "c9", name: "docs_read", args_brief: '{"doc_id":3}', step: 1,
    }),
    JSON.stringify({
      type: "tool_result", call_id: "c9", name: "docs_read", ok: true,
      brief: '{"ok":true}', ms: 20, step: 1,
    }),
    JSON.stringify({
      type: "error", code: "upstream_error",
      message: "上游连接中断", retryable: true,
    }),
    JSON.stringify({ type: "done", message_id: 3, partial: true }),
  ]);
  // 只落用户消息，不落 assistant —— 模拟"失败那轮没有 assistant 消息"。
  await stubAgent(page, { messages, send: sendWithPersist(messages, errSse) });
  await page.goto("/admin/agent");

  await page.getByTestId("agent-input").fill("读一下可灵文档");
  await page.getByTestId("agent-send").click();

  const turn = page.getByTestId("agent-turn-1");
  await expect(turn).toContainText("读一下可灵文档");
  // 收敛判据若只看"最后一条消息 id 变大"，这里会只剩一条用户气泡。
  await expect(page.getByTestId("agent-run-error")).toContainText("上游连接中断");
  // 工具卡片标题用真实标签（toolMeta 的 docs_read → 读素材文档）。
  await expect(turn.getByTestId("agent-tools")).toContainText("读素材文档");
  await page.waitForTimeout(400); // 等 onSettled 的 refetch 落地
  await expect(page.getByTestId("agent-run-error")).toContainText("上游连接中断");
  await expect(page.getByTestId("agent-input")).toBeEnabled();
});

test("失败后重试：用同一段用户原话重新发送", async ({ page }) => {
  await seedAdmin(page);
  const messages = structuredClone(MESSAGES);
  const errSse = sseBody([
    JSON.stringify({ type: "start", run_id: 13, message_id: 3 }),
    JSON.stringify({ type: "error", code: "upstream_error", message: "上游超时", retryable: true }),
    JSON.stringify({ type: "done", message_id: 3, partial: true }),
  ]);
  const sent: string[] = [];
  await stubAgent(page, {
    messages,
    send: async (route) => {
      sent.push(((route.request().postDataJSON() ?? {}) as { content?: string }).content ?? "");
      await route.fulfill({ status: 200, contentType: "text/event-stream", body: errSse });
    },
  });
  await page.goto("/admin/agent");

  await page.getByTestId("agent-input").fill("再读一次文档");
  await page.getByTestId("agent-send").click();
  await expect(page.getByTestId("agent-run-error")).toContainText("上游超时");

  await page.getByRole("button", { name: "重试" }).click();
  await expect.poll(() => sent.length).toBe(2);
  expect(sent[1]).toBe("再读一次文档");
});

test("长思考展开后限高 200px，点「展开全部」才看全文", async ({ page }) => {
  await seedAdmin(page);
  const longReason = Array.from(
    { length: 40 },
    (_, i) => `第 ${i + 1} 点：先确认端点是否需要鉴权，再看价格档位是否齐全，最后核对文档分组。`,
  ).join("\n");
  await stubAgent(page, {
    messages: [
      USER_MSG,
      {
        ...MESSAGES[1],
        parts: { items: [{ k: "reason", t: longReason }], tools: [], proposals: [] },
        content: "看完了。",
      },
    ],
  });
  await page.goto("/admin/agent");

  const think = page.getByTestId("agent-thinking-0");
  await think.getByRole("button").first().click();
  const body = think.locator(".agent-think__body");
  await expect(body).toBeVisible();
  const clamped = await body.boundingBox();
  expect(clamped!.height).toBeLessThanOrEqual(201);
  await expect(think.getByRole("button", { name: "展开全部思考过程" })).toBeVisible();

  await think.getByRole("button", { name: "展开全部思考过程" }).click();
  const full = await body.boundingBox();
  expect(full!.height).toBeGreaterThan(201);

  // 头部仍可整块收起。
  await think.getByRole("button").first().click();
  await expect(body).toHaveCount(0);
});

test("流式输出始终贴底：结束时距底不超过 4px", async ({ page }) => {
  await seedAdmin(page);
  const messages = structuredClone(MESSAGES);
  // 不落 assistant：保留实时轮的长内容，专测滚动。
  await stubAgent(page, { messages, send: sendWithPersist(messages, longSse("贴底测试")) });
  await page.goto("/admin/agent");

  await page.getByTestId("agent-input").fill("长文输出");
  await page.getByTestId("agent-send").click();
  await expect(page.getByTestId("agent-turn-1")).toContainText("第 120 行");

  await expect.poll(() => distanceToBottom(page)).toBeLessThanOrEqual(4);
});

test("手动上滚后不抢滚动，出现「有新内容」并可跳回底部", async ({ page }) => {
  await seedAdmin(page);
  const messages = structuredClone(MESSAGES);
  await stubAgent(page, { messages, send: sendWithPersist(messages, longSse("回看测试")) });
  await page.goto("/admin/agent");

  await page.getByTestId("agent-input").fill("长文输出");
  await page.getByTestId("agent-send").click();
  await expect(page.getByTestId("agent-turn-1")).toContainText("第 120 行");
  await expect.poll(() => distanceToBottom(page)).toBeLessThanOrEqual(4);

  // 用户回看历史：内容再长也不能把他拽回底部。
  await page.evaluate(() => {
    const el = document.querySelector('[data-testid="agent-stream"]') as HTMLElement | null;
    if (el) el.scrollTop = 0;
  });
  const before = await page.evaluate(() => {
    const el = document.querySelector('[data-testid="agent-stream"]') as HTMLElement | null;
    return el ? el.scrollTop : -1;
  });

  // 展开思考块 = 内容高度增长：跟随态下应自动到底，非跟随态只提示。
  await page.getByTestId("agent-thinking-0").getByRole("button").first().click();
  await expect(page.getByRole("button", { name: "有新内容" })).toBeVisible();
  const after = await page.evaluate(() => {
    const el = document.querySelector('[data-testid="agent-stream"]') as HTMLElement | null;
    return el ? el.scrollTop : -1;
  });
  expect(after).toBe(before);

  await page.getByRole("button", { name: "有新内容" }).click();
  await expect.poll(() => distanceToBottom(page)).toBeLessThanOrEqual(4);
});

test("历史消息加载失败：显示行内错误与重试，不伪装成空会话", async ({ page }) => {
  await seedAdmin(page);
  await stubAgent(page, { messagesFail: true });
  await page.goto("/admin/agent");

  await expect(page.getByTestId("agent-messages-error")).toContainText("历史消息加载失败");
  await expect(page.getByRole("button", { name: "重试" }).first()).toBeVisible();
  // 关键：绝不能退化成"今天要接什么？"的新会话引导卡。
  await expect(page.getByRole("heading", { name: "今天要接什么？" })).toHaveCount(0);
});

/* ── 智能追问（ask_user） ────────────────────────────────────────
   两个必须守的点：
   1. 卡片能作答，且 POST 的内容就是运营选的那个 value（不是模型猜的、不是前端翻译的）。
   2. 这一轮结束后卡片必须变成只读：一个点得动却报 410 的按钮比没有按钮更糟。 */

test("待答问题：选一项提交后卡片切成已回答，后续内容接在同一轮", async ({ page }) => {
  await seedAdmin(page);
  const bodies: Record<string, unknown>[] = [];
  await stubAgent(page, {
    pendingAsk: PENDING_ASK,
    onAnswerAsk: (b) => bodies.push(b),
  });
  await page.goto("/admin/agent");

  const card = page.getByTestId("agent-ask");
  await expect(card).toHaveCount(1);
  await expect(card).toContainText("这个模型按次还是按规格计费？");
  await expect(card).toContainText("决定 quota_type");

  // 没选任何选项时提交按钮不可用：空提交会把"没回答"当成"回答了空"。
  await expect(page.getByTestId("agent-ask-submit")).toBeDisabled();
  await card.getByRole("button", { name: /^按规格/ }).click();
  await page.getByTestId("agent-ask-submit").click();

  expect(bodies).toHaveLength(1);
  expect(bodies[0]).toMatchObject({ selected: ["per_spec"], skipped: false });

  // 重取消息后卡片变终态：按钮消失、答案留痕。
  await expect(page.getByTestId("agent-ask")).toContainText("已回答：按规格");
  await expect(page.getByTestId("agent-ask-submit")).toHaveCount(0);
  await expect(page.getByText("按规格计费，已据此配置 spec_prices。")).toBeVisible();
});

test("提问卡在这一轮结束后必须只读：不能留下点了报 410 的按钮", async ({ page }) => {
  await seedAdmin(page);
  const messages = structuredClone(MESSAGES);
  // 流内提问 + 立刻 done：真实场景是"问完 60 秒超时"，run 收尾但 ask 段还在。
  const sse =
    'data: {"type":"start","run_id":9,"message_id":2,"model":"gpt-4o-mini"}\n\n' +
    'data: {"type":"step","step":1}\n\n' +
    'data: {"type":"ask","ask_id":41,"ask":{"question":"用哪个分组？","why":"决定折扣","options":[{"value":"default","label":"默认"},{"value":"vip","label":"VIP"}],"multi":false,"allow_free_text":true,"expires_in":60}}\n\n' +
    'data: {"type":"usage"}\n\n' +
    'data: {"type":"done","message_id":2}\n\n';
  await stubAgent(page, { messages, send: sendWithPersist(messages, sse) });

  await page.goto("/admin/agent");
  await page.getByTestId("agent-input").fill("接入一家中转");
  await page.getByTestId("agent-send").click();

  const card = page.getByTestId("agent-ask");
  await expect(card).toContainText("用哪个分组？");
  // done 之后立刻结算：选项按钮全部禁用、提交按钮消失。
  await expect(card).toContainText("已随本轮结束");
  await expect(page.getByTestId("agent-ask-submit")).toHaveCount(0);
  await expect(card.getByText("默认")).toBeDisabled();
});
