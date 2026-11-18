/* ============================================================
   页签类路由的 validateSearch 统一入口
   ------------------------------------------------------------
   为什么必须共用一份：
   1) router 用 qss 解析 query，`?tab=7` 会被 toValue() 还原成 **number**、
      `?tab=true` 还原成 boolean、`?tab=` 还原成空串。若各路由自己 pick，
      必然出现「同一参数在不同页面行为不一致」。
   2) 必须 `{...s}` 透传：不吃掉 `?create=1` 之类的既有参数
      （`Keys.tsx` 现在只返 `{create}`，是历史写法，不要照抄）。
   3) 只做类型归一，**不放业务默认**：默认值归 usePageTabs(fallback)，
      否则 `#redeem` 兼容判不出「用户没指定」。
   ============================================================ */

export type TabSearch = {
  tab?: string;
  rec?: string;
  scene?: string;
  /** 其余参数原样透传（`?create=1` 等） */
  [k: string]: unknown;
};

const KEYS = ["tab", "rec", "scene"] as const;

/** 把单个 query 值归一成非空字符串；数组/对象/空串一律视为未指定 */
function toTabValue(v: unknown): string {
  if (typeof v === "string") return v.trim();
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  return "";
}

export function tabSearch(search: Record<string, unknown>): TabSearch {
  const next: TabSearch = { ...search };
  for (const key of KEYS) {
    const value = toTabValue(search[key]);
    if (value) next[key] = value;
    else delete next[key];
  }
  return next;
}

/* ============================================================
   开放接口手册：`?ep=<端点 key>`
   ------------------------------------------------------------
   只放"选中了哪条接口"这一位：
   1) 它是唯一值得发给同事的状态（"你看一下 keys.reset 这条"）；
   2) 试运行面板的展开与否、填了什么值**不进 URL** —— 那里面会有密钥
      与手机号，进了 URL 就会被粘进聊天记录、被浏览器历史保存。
   ============================================================ */

export type ApiDocsSearch = {
  /** 选中的端点 key（如 `keys.reset`）；空表示让页面自己选第一条 */
  ep?: string;
  [k: string]: unknown;
};

/** 端点 key 的形态：`分组.动作`，只允许小写字母、数字与下划线。 */
const EP_KEY = /^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$/;

export function apiDocsSearch(search: Record<string, unknown>): ApiDocsSearch {
  const next: ApiDocsSearch = { ...search };
  const ep = toTabValue(search.ep);
  // 用正则白名单而不是"非空就收"：`?ep=<script>` 会被原样渲染进 DOM
  // （面包屑、标题都会用它），而它来自 URL —— 唯一的不可信输入。
  if (EP_KEY.test(ep)) next.ep = ep;
  else delete next.ep;
  return next;
}

/* ============================================================
   模型服务：`?a=<适配器 id>&c=<渠道 id>`
   ------------------------------------------------------------
   只放"当前在看哪条渠道"这一位：它是唯一值得发给同事的状态
   （"你看一下这条渠道的模型"）。搜索/筛选/分页**不进 URL** —— 那属于当前会话的
   浏览状态，进了 URL 会让"发给同事"变成"把我的筛选项也塞给他"。
   ============================================================ */

export type ModelServiceSearch = {
  /** 展开的适配器（`openai` / `spec:<slug>`）；空表示由页面自己选第一个有渠道的 */
  a?: string;
  /** 选中的渠道 id（正整数）；空表示未选中，右栏显示引导态 */
  c?: number;
  [k: string]: unknown;
};

/** 适配器 id 的形态：内置注册名或 `spec:<slug>`。 */
const ADAPTER_ID = /^(spec:)?[a-z0-9][a-z0-9._-]*$/;

export function modelServiceSearch(search: Record<string, unknown>): ModelServiceSearch {
  const next: ModelServiceSearch = {};
  // 用白名单而不是"非空就收"：`?a=<script>` 会进 DOM（选中判断与 data 属性），
  // 而它来自 URL —— 唯一的不可信输入。
  const a = toTabValue(search.a);
  if (ADAPTER_ID.test(a)) next.a = a;
  const cid = Number(toTabValue(search.c));
  if (Number.isSafeInteger(cid) && cid > 0) next.c = cid;
  return next;
}

/* ============================================================
   文档编辑页：`?s=<板块>&node=<节点>&ep=<接口>&view=preview`
   ------------------------------------------------------------
   三个选中参数**互斥**（板块 / 节点 / 接口），写入时一次写全：
   同时存在两个是非法态，靠纠正 effect 收敛会闪一帧、且历史记录里留下脏 URL。

   `view` 只允许 `preview`：默认编辑，URL 干净（带 `?view=edit` 的链接
   看起来像是另一种模式，而它其实就是默认）。
   ============================================================ */

export type DocsAdminSearch = {
  /** 正在编辑的板块 key（与 node/ep 互斥） */
  s?: string;
  /** 选中的节点 key */
  node?: string;
  /** 选中的接口 key（必须同时有 node） */
  ep?: string;
  /** 右栏页签：缺省=编辑 */
  view?: "preview";
  [k: string]: unknown;
};

/** 标识形态：小写字母开头，允许数字与 `- _ .`（与后端 validKey 同源）。 */
const DOC_KEY = /^[a-z][a-z0-9._-]*$/;

export function docsAdminSearch(search: Record<string, unknown>): DocsAdminSearch {
  const next: DocsAdminSearch = {};
  const s = toTabValue(search.s);
  if (DOC_KEY.test(s)) next.s = s;
  const node = toTabValue(search.node);
  if (DOC_KEY.test(node)) next.node = node;
  const ep = toTabValue(search.ep);
  if (DOC_KEY.test(ep) && next.node) next.ep = ep;
  if (toTabValue(search.view) === "preview") next.view = "preview";
  return next;
}
