import type { IconName } from "@/components/common/Icon";

/**
 * 双端导航单一源（冻结契约）
 * 路径与原型页面一一对应（home.html → /home）。
 * tab 语义：1 = 双端（桌面导航 + 移动端 TabBar）；"mobile-only" = 仅移动端；
 *           未标注 = 桌面 only（移动端经主页快捷格进入）。
 */

export interface PortalNavItem {
  title: string;
  path: string;
  /** Icon 组件图标键（对齐原型 IC 图标表语义） */
  icon: IconName;
  tab?: 1 | "mobile-only";
  /** 移动端 TabBar 文案（缺省用 title） */
  mobileLabel?: string;
}

export const portalNav: PortalNavItem[] = [
  { title: "主页", path: "/home", icon: "home", tab: 1 },
  { title: "模型广场", path: "/models", icon: "grid", tab: 1 },
  // 文档（/docs）刻意**不在此表**：它是低频查阅型入口，挂在页脚一级链接里
  // （见 portalFooterNav）。顶栏放的是「每天要点」的六项，加一项就会把窄屏挤爆。
  { title: "API 密钥", path: "/keys", icon: "key", tab: 1 },
  { title: "调用记录", path: "/usage", icon: "rows", tab: 1 },
  { title: "文件", path: "/files", icon: "doc" },
  { title: "钱包", path: "/topup", icon: "wallet" },
  {
    title: "用户中心",
    path: "/settings",
    icon: "user",
    tab: "mobile-only",
    mobileLabel: "我的",
  },
];

/** 桌面顶栏导航项（排除 mobile-only） */
export const portalDesktopNav: PortalNavItem[] = portalNav.filter(
  (n) => n.tab !== "mobile-only",
);

/** 移动端底部 TabBar 导航项 */
export const portalMobileNav: PortalNavItem[] = portalNav.filter(
  (n) => n.tab === 1 || n.tab === "mobile-only",
);

/**
 * 页脚次级入口。
 *
 * 这里只放「常驻但低频」的入口：不占顶栏宽度、也不进移动 TabBar，靠页脚一行文字链接兜底。
 * 与 portalNav **互斥**——一个路径只在一处登记，否则就会出现「顶栏改了、页脚没改」的双真相。
 */
export interface PortalFooterLink {
  title: string;
  path: string;
}

export const portalFooterNav: PortalFooterLink[] = [
  // 文档页本体挂公开路由 /docs（未登录可读），与「关于我们 / 用户协议」同属页脚一行。
  { title: "API 文档", path: "/docs" },
];

/** 管理台入口（管理员在门户移动端可见；桌面入口在 UserMenu，见 J4） */
export const portalAdminItem: PortalNavItem = {
  title: "管理台",
  path: "/admin/dashboard",
  icon: "shield",
  tab: "mobile-only",
  mobileLabel: "管理台",
};

/** 按角色返回移动端 TabBar 项（role=0 追加「管理台」；非管理员不追加，DOM 中不存在） */
export function portalMobileNavFor(isAdmin: boolean): PortalNavItem[] {
  return isAdmin ? [...portalMobileNav, portalAdminItem] : portalMobileNav;
}

export interface AdminNavItem {
  title: string;
  path: string;
  icon: IconName;
}

/**
 * 管理台导航分区（唯一真相源）。
 *
 * 顶级项 = 没有 `title` 的分区（仪表盘 / 系统配置 / 模型服务 / 用户管理 / 应用管理）：
 * 不画组标题、不可折叠。
 * 顶级项之间是并列的主菜单，顺序即「日常频次 + 产品决策」：越靠前越常点。
 * 用「有 title 必有 icon」的可辨识联合表达，而不是 `title?: string` + `icon?: IconName`：
 * 后者允许「有组标题却忘配图标」，只能靠运行时兜底或测试兜住，编译期就该报错。
 *
 * 排序依据：项目没有菜单点击埋点，也不为一次性排序加埋点；下面的顺序是
 * 「日常运营操作频次」的业务判断（+「系统配置最常用」的产品决策）。
 * 哪天要做真频次排序应另立需求 —— audit_logs 只记写操作，读页面访问没有记录。
 */
export type AdminNavSection =
  | {
      /** 稳定键：折叠状态持久化与 aria-controls 用；改文案不许改 id */
      id: string;
      title: string;
      /** 分组头图标（与菜单项同尺寸渲染） */
      icon: IconName;
      items: AdminNavItem[];
    }
  | { id: string; title?: undefined; items: AdminNavItem[] };

export const adminNav: AdminNavSection[] = [
  // 前五个是顶级项：最常点的入口不能被折叠藏起来。
  { id: "dashboard", items: [{ title: "仪表盘", path: "/admin/dashboard", icon: "home" }] },
  { id: "config", items: [{ title: "系统配置", path: "/admin/config", icon: "gear" }] },
  // 模型服务是**顶级项**：适配器 → 渠道 → 模型现在是一个页面的三栏，
  // 再挂成可折叠分组等于给一个页面配三个子项（子项地址已不存在）。
  {
    id: "models",
    items: [{ title: "模型服务", path: "/admin/model-service", icon: "brain" }],
  },
  // 用户管理 / 应用管理紧跟在模型服务之后，**各占一个顶级项**：
  // 它们原来合成「用户与应用」一个可折叠组，而一组只有两项 —— 折叠挡不住任何东西，
  // 反而给「谁在接入、谁在用」这两个最高频的主体多了一层点击才到位。
  // 拆开后组的数量 4→3、分区 7→8（项目数不变，仍是 15）。
  {
    id: "users",
    items: [{ title: "用户管理", path: "/admin/users", icon: "user" }],
  },
  {
    id: "apps",
    items: [{ title: "应用管理", path: "/admin/apps", icon: "external" }],
  },
  {
    id: "billing",
    title: "交易与资金",
    icon: "creditCard",
    items: [
      { title: "订单管理", path: "/admin/orders", icon: "wallet" },
      // 对账是资金链的收口，不是日志：放资金组，不放运行组。
      { title: "资金对账", path: "/admin/reconcile", icon: "check" },
      // doc 在管理台未使用于本组其余项；视觉上不与既有项重复。
      { title: "充值套餐", path: "/admin/packages", icon: "doc" },
      { title: "兑换码", path: "/admin/redeems", icon: "coin" },
    ],
  },
  {
    id: "runtime",
    title: "运行与审计",
    icon: "activity",
    items: [
      // 调用日志排首位：日常排障第一眼就是它。
      { title: "调用日志", path: "/admin/logs", icon: "rows" },
      { title: "异步任务", path: "/admin/tasks", icon: "refresh" },
      // 认证审计 / 审计日志是「出事之后查号、查人」，与运行日志同属一类。
      { title: "认证审计", path: "/admin/auth-events", icon: "shield" },
      { title: "审计日志", path: "/admin/audit", icon: "shield" },
    ],
  },
  {
    id: "docs",
    title: "文档与接口",
    icon: "library",
    items: [
      { title: "API 文档", path: "/admin/docs", icon: "book" },
      { title: "开放接口", path: "/admin/api-docs", icon: "book" },
    ],
  },
  // 账户设置**不在侧栏**：入口在顶栏头像菜单里，点开是弹窗（关掉回到原页面）。
  // 路由仍保留 —— 强制改密要有一个落地 URL，深链也要能用。
];

/** 管理台导航扁平表（布局与面包屑共用） */
export const adminNavFlat: AdminNavItem[] = adminNav.flatMap((s) => s.items);

/**
 * 非导航路由的标题（只有「路由存在但不进侧栏」的页面需要登记）。
 *
 * 不塞回 adminNav：那会让它重新变成侧栏第 19 项，且面包屑会显示一个点不进去的条目。
 */
const ADMIN_OFF_NAV_TITLES: Record<string, string> = {
  "/admin/account": "账户设置",
  // 接入助手保留独立工作台（深链可用），但入口已收敛到「模型服务 → 新建适配器」，
  // 不再占侧栏一项：它的产物就是"规格 + 渠道 + 模型"，在模型服务里就地生成。
  "/admin/agent": "AI 接入助手",
};

/**
 * 路径归属判定：精确命中，或**以 `p + "/"` 为前缀**。
 *
 * 为什么不是裸 `startsWith`：`/admin/apps-archive` 会被 `/admin/apps` 吃掉，
 * 而这类近似路径在加页时最容易出现。边界字符让"子路径"与"同前缀的另一个页面"分开。
 *
 * 为什么需要它：子路径页面（如 `/admin/config?tab=billing` 之外的运行期深链）
 * 不该逐条登记进 nav —— 那会让侧栏项数断言全变。面包屑、侧栏高亮、分区展开
 * 三处都靠这个函数把它们归到所属分区。
 */
export function matchPath(prefix: string, path: string): boolean {
  return path === prefix || path.startsWith(`${prefix}/`);
}

/**
 * 按路径取导航标题（面包屑用）。
 *
 * 顺序：非导航页的精确表 → 导航精确命中 → **最长前缀**。
 * 精确表必须先查：`/admin/agent` 是刻意不进侧栏的页面，
 * 若先走前缀，它会因为"没人匹配"而回落成空标题。
 */
export function adminTitleOf(path: string): string {
  const exact = ADMIN_OFF_NAV_TITLES[path] ?? adminNavFlat.find((n) => n.path === path)?.title;
  if (exact) return exact;
  let best: { title: string; len: number } | null = null;
  for (const n of adminNavFlat) {
    if (matchPath(n.path, path) && (!best || n.path.length > best.len)) {
      best = { title: n.title, len: n.path.length };
    }
  }
  return best?.title ?? "";
}

/** 当前路径所属分区 id（顶级项也返回自己的 id）；未知路径返回 undefined。 */
export function adminSectionIdOf(path: string): string | undefined {
  let best: { id: string; len: number } | null = null;
  for (const s of adminNav) {
    for (const i of s.items) {
      if (matchPath(i.path, path) && (!best || i.path.length > best.len)) {
        best = { id: s.id, len: i.path.length };
      }
    }
  }
  return best?.id;
}

/** 折叠状态存储键：值是「已收起的分区 id」数组。 */
export const ADMIN_NAV_COLLAPSED_KEY = "ld-admin-nav-collapsed";

/** 读取已收起的分区 id。脏数据 / 存储被禁用一律返回空数组（= 全展开），绝不抛。 */
export function readCollapsedSections(): string[] {
  try {
    const v: unknown = JSON.parse(localStorage.getItem(ADMIN_NAV_COLLAPSED_KEY) ?? "[]");
    // 只留非空字符串：被外部写脏成 [1,null,""] 时，空 id 既匹配不到任何分区，
    // 还会在下一次写回时被当成「用户收起过一个组」永久留着。
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string" && x !== "") : [];
  } catch {
    return [];
  }
}

/** 写回；空集合直接删键（键不存在 = 全展开，语义单一，断言简单）。 */
export function writeCollapsedSections(ids: string[]): void {
  try {
    if (ids.length) localStorage.setItem(ADMIN_NAV_COLLAPSED_KEY, JSON.stringify(ids));
    else localStorage.removeItem(ADMIN_NAV_COLLAPSED_KEY);
  } catch {
    /* 隐私模式 / 配额满：纯外观偏好，丢了不影响使用 */
  }
}
