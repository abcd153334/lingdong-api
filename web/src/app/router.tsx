import {
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  redirect,
} from "@tanstack/react-router";
import { AdminLayout } from "@/app/layouts/AdminLayout";
import { PortalLayout } from "@/app/layouts/PortalLayout";
import { BrandMeta } from "@/components/chrome/BrandMeta";
import { loadPortalMode } from "@/lib/portalMode";
import { loadSession } from "@/lib/session";
import { tabSearch, apiDocsSearch, modelServiceSearch } from "@/lib/searchParams";
import { Login } from "@/pages/Login";
import { Register } from "@/pages/Register";
import { AboutPage } from "@/pages/About";
import { AgreementPage } from "@/pages/Agreement";
import { DocsApp } from "@/docs/DocsApp";
import { NotFound } from "@/pages/NotFound";
import { RouteError, RoutePending } from "@/components/common/RouteError";
import { Showcase } from "@/pages/Showcase";
import { HomePage } from "@/pages/portal/Home";
import { ModelsPage } from "@/pages/portal/Models";
import { KeysPage } from "@/pages/portal/Keys";
import { UsagePage } from "@/pages/portal/Usage";
import { FilesPage } from "@/pages/portal/Files";
import { TopupPage } from "@/pages/portal/Topup";
import { SettingsPage } from "@/pages/portal/Settings";
import { MfaSetup } from "@/pages/MfaSetup";
import { AdminDashboardPage } from "@/pages/admin/Dashboard";
import { AdminAccountPage } from "@/pages/admin/Account";
import { AdminUsersPage } from "@/pages/admin/Users";
import { AdminModelServicePage } from "@/pages/admin/ModelService";
import { DocsAdmin as AdminDocWorkbenchPage } from "@/pages/admin/DocsAdmin";
import { AdminAgentPage } from "@/pages/admin/Agent";
import { AdminOrdersPage } from "@/pages/admin/Orders";
import { AdminRedeemsPage } from "@/pages/admin/Redeems";
import { AdminPackagesPage } from "@/pages/admin/Packages";
import { AdminReconcilePage } from "@/pages/admin/Reconcile";
import { AdminLogsPage } from "@/pages/admin/Logs";
import { AdminTasksPage } from "@/pages/admin/Tasks";
import { AdminConfigPage } from "@/pages/admin/Config";
import { AdminAuditPage } from "@/pages/admin/Audit";
import { AdminAuthEventsPage } from "@/pages/admin/AuthEvents";
import { AdminAppsPage } from "@/pages/admin/Apps";
import { AdminApiDocsPage } from "@/pages/admin/ApiDocs";

/* ============================================================
   路由骨架（S1）
   双端物理分离：门户 / 管理台各自 pathless layout route。
   路径与原型 html 一一对应；未知路径统一 404。
   逐条显式注册（非工厂循环）：保留字面量路径类型，使 Link/redirect 类型安全。
   ============================================================ */

const rootRoute = createRootRoute({
  // BrandMeta 挂在根上：页面标题与浏览器标签图标全站只有一个同步点
  component: () => (
    <>
      <BrandMeta />
      <Outlet />
    </>
  ),
  notFoundComponent: NotFound,
});

/**
 * 门户 layout（无 path，仅承载 chrome）；未登录统一回 /login。
 *
 * 守卫必须 `await`：凭证在 httpOnly Cookie 里，前端读不到，
 * "有没有登录"只能问服务端（loadSession 会做并发合并，首屏只发一次请求）。
 * 同步读 localStorage 的时代结束了 —— 那份缓存本身就不该被信任。
 */
const portalRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: "portal",
  component: PortalLayout,
  beforeLoad: async ({ location }) => {
    const user = await loadSession();
    if (!user) {
      throw redirect({ to: "/login", search: { redirect: window.location.pathname + window.location.search } });
    }
    // 临时口令：后端只放行「改密 / 登出 / 踢会话 / 读自己」四条路径，其余一律 403。
    // 界面必须同步收窄到用户中心，否则用户点哪都是错误提示却不知道起因。
    if (user.must_change_password && location.pathname !== "/settings") {
      throw redirect({ to: "/settings" });
    }
    // 两步验证绑定页是「强制管理员绑定」策略下的唯一自救通道，必须在模式判断**之前**放行：
    // 它在门户路由下（刻意不放 /admin，否则会被 403 自锁），关闭门户时仍要能进。
    if (location.pathname === "/mfa/setup") return;
    if (!(await loadPortalMode())) {
      throw redirect({ to: user.role === 0 ? "/admin/dashboard" : "/login" });
    }
  },
});

/** 管理台 layout（无 path，仅承载 chrome）；需登录且角色为管理员 */
const adminRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: "admin",
  component: AdminLayout,
  beforeLoad: async ({ location }) => {
    const user = await loadSession();
    // 未登录与非管理员都回登录页：不区分是为了不给"这个账号是管理员吗"提供探测口。
    if (!user || user.role !== 0) {
      throw redirect({ to: "/login", search: { redirect: window.location.pathname + window.location.search } });
    }
    // 临时口令：后端对管理台其余端点一律 403 password_change_required，
    // 停在别的页面只会满屏报错。账户页是唯一能改密的地方（两条守卫各自负责自己那一端，
    // 全局 403 拦截做不到按角色分流 —— 那样会把普通会员送进管理台再弹回来）。
    if (user.must_change_password && location.pathname !== "/admin/account") {
      throw redirect({ to: "/admin/account" });
    }
  },
});

/* ---------- 公共 ---------- */
const indexRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/",
  beforeLoad: async () => {
    // 纯服务端模式下没有门户首页：管理员直接进管理台，未登录的访客去登录页。
    const [user, open] = await Promise.all([loadSession(), loadPortalMode()]);
    if (open) throw redirect({ to: "/home" });
    throw redirect({ to: user?.role === 0 ? "/admin/dashboard" : "/login" });
  },
});

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  // 登录成功后回跳的来源页（守卫拦截时把当前路径塞进 ?redirect=）。
  // 只接受站内相对路径，且在 Login 里二次校验（`//host` 属协议相对 URL）。
  // 返回类型显式声明为可选属性：否则 tanstack-router 会把它当成「必填 search」，
  // 于是全站每一处 `to="/login"` 都被要求带 search（几十个无关报错）。
  validateSearch: (search: Record<string, unknown>): { redirect?: string } =>
    typeof search.redirect === "string" ? { redirect: search.redirect } : {},
  component: Login,
});

const registerRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/register",
  beforeLoad: async () => {
    // 门户端关闭 = 系统内注册入口关闭（API 面的注册不受影响，它由注册模式决定）。
    if (!(await loadPortalMode())) throw redirect({ to: "/login" });
  },
  component: Register,
});

/** 组件展示台（设计系统可视化验证） */
const showcaseRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/__showcase",
  component: Showcase,
});

/* ---------- 公开静态内容页（S19：未登录可读，与 /login 同级） ---------- */
const aboutRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/about",
  component: AboutPage,
});
const agreementRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/agreement",
  component: AgreementPage,
});

/**
 * API 文档站（公开路由，未登录可读）。
 *
 * 路由身份是**文档节点 key** 与**接口 key**：一族模型共用一篇（差异在适用模型
 * 清单里），把模型 id 放进路径会让「十几个模型 → 十几条一模一样」的链接重新长出来。
 *
 * 三种路径、两种页面形态（见 `docs/DocsApp`）：`/docs` 落地、`/docs/{node}`
 * 重定向到首个接口、`/docs/{node}/{ep}` 与说明页是真正的页面。
 */
const docsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/docs",
  component: () => <DocsApp />,
});

// 节点：落到它第一个接口（说明页节点直接渲染正文）。
const docsNodeRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/docs/$nodeKey",
  component: function DocsNodeRoute() {
    const { nodeKey } = docsNodeRoute.useParams();
    return <DocsApp nodeKey={nodeKey} />;
  },
});

// 接口页：文档站唯一的正文形态。一页一个接口，可分享、可刷新、可直达。
const docsEndpointRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/docs/$nodeKey/$endpointKey",
  component: function DocsEndpointRoute() {
    const { nodeKey, endpointKey } = docsEndpointRoute.useParams();
    return <DocsApp nodeKey={nodeKey} endpointKey={endpointKey} />;
  },
});

/* ---------- 门户 ---------- */
const homeRoute = createRoute({
  getParentRoute: () => portalRoute,
  path: "/home",
  component: HomePage,
});
const modelsRoute = createRoute({
  getParentRoute: () => portalRoute,
  path: "/models",
  component: ModelsPage,
});
const keysRoute = createRoute({
  getParentRoute: () => portalRoute,
  path: "/keys",
  // create=1：由门户主页「创建 API KEY」快捷入口直达创建弹窗（原型 keys.html?create=1 语义）
  validateSearch: (search: Record<string, unknown>) => ({ create: Number(search.create) || 0 }),
  component: KeysPage,
});
const usageRoute = createRoute({
  getParentRoute: () => portalRoute,
  path: "/usage",
  // ?tab=records|tasks：调用记录/异步任务视为并列视图（进 URL，可深链）
  validateSearch: tabSearch,
  component: UsagePage,
});
const filesRoute = createRoute({
  getParentRoute: () => portalRoute,
  path: "/files",
  component: FilesPage,
});
const topupRoute = createRoute({
  getParentRoute: () => portalRoute,
  path: "/topup",
  // ?tab=pkg|redeem + ?rec=tx|orders：卡内两组页签，第二组必须命名（rec）
  validateSearch: tabSearch,
  component: TopupPage,
});
const settingsRoute = createRoute({
  getParentRoute: () => portalRoute,
  path: "/settings",
  component: SettingsPage,
});
/**
 * 两步验证绑定页。
 *
 * 放在门户（而不是管理台）下：策略 `required_for_admin` 生效时，
 * 未绑定的管理员访问任何 /admin/** 都会被 403，前端据此跳到这个页面 ——
 * 如果它也在 /admin 下，被拦的人就打不开绑定页了（自己锁死自己）。
 */
const mfaSetupRoute = createRoute({
  getParentRoute: () => portalRoute,
  path: "/mfa/setup",
  component: MfaSetup,
});
/** 站点公告（门户侧改为顶栏铃铛弹层，不再有独立页面：/announcements 路由已删除） */

/* ---------- 管理台 ---------- */
const dashboardRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/dashboard",
  component: AdminDashboardPage,
});
const adminUsersRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/users",
  component: AdminUsersPage,
});
/**
 * 文档工作台：`/admin/docs`（左树 / 中编辑 / 右实时预览）。
 *
 * 与公开文档站 `/docs` 是两件事：那边是产物（只读、游客可看），这边是编辑入口。
 * 选中对象放 URL（`?node=&ep=`）：运营常把「某篇文档的编辑页」发给同事，
 * 可直达、可刷新，比在 30 个节点里点进去快得多。
 */
const adminDocsRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/docs",
  component: AdminDocWorkbenchPage,
});
const adminAgentRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/agent",
  component: AdminAgentPage,
});
/**
 * 模型服务：适配器 → 渠道 → 模型（左右两栏）。
 *
 * 取代原来的三个页面（/admin/models、/admin/channels、/admin/adapters）：
 * 三者共享「适配器 id + 渠道 id」，拆成三页时"这条渠道能用哪些模型"要跨三个地址
 * 才能回答，而绑定/解绑又必须回到模型页 —— 这正是新手最容易被绕晕的地方。
 *
 * `?a` / `?c` 进 URL（可深链/可刷新保持）；搜索与分页**不进** URL。
 */
const adminModelServiceRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/model-service",
  validateSearch: modelServiceSearch,
  component: AdminModelServicePage,
});
const adminOrdersRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/orders",
  component: AdminOrdersPage,
});
const adminRedeemsRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/redeems",
  component: AdminRedeemsPage,
});
const adminPackagesRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/packages",
  component: AdminPackagesPage,
});
/**
 * 资金对账：`/admin/reconcile`。
 *
 * 与「订单管理」分开：那边处理单据，这边看的是**账本之间对不上**的计数
 * （余额链 / 冻结 / 到账 / 退款 / 返利欠款），运营定位资金问题先来这里。
 */
const adminReconcileRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/reconcile",
  component: AdminReconcilePage,
});

const adminLogsRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/logs",
  // ?tab=hot|archive：明细 / 归档
  validateSearch: tabSearch,
  component: AdminLogsPage,
});
const adminTasksRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/tasks",
  component: AdminTasksPage,
});
const adminConfigRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/config",
  // ?tab=6 个分区 + ?scene=邮件模板场景。
  // 明确否决子路径（/admin/config/billing）：AdminSidebar 用 pathname === item.path 严格匹配、
  // AdminTopBar 的 adminTitleOf(pathname) 精确匹配、chrome.test 断言 aside nav a 18 项 —— 任一都会被打死。
  validateSearch: tabSearch,
  component: AdminConfigPage,
});
const adminAuditRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/audit",
  component: AdminAuditPage,
});
/**
 * 认证审计（登录/注册/改密/二次验证流水）。
 *
 * 与「审计日志」分两条路由：那边的语义是「管理员做了什么」，
 * 这边的语义是「账号层面发生了什么」，混在一页会让撞库把配置变更淹没。
 */
const adminAuthEventsRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/auth-events",
  component: AdminAuthEventsPage,
});
/** 应用管理（对外 API 的归属主体：密钥、订单、消费按应用归因） */
const adminAppsRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/apps",
  component: AdminAppsPage,
});
/** 开放接口手册（只读：契约与运行期守卫同源，可导出 OpenAPI / Markdown） */
const adminAccountRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/account",
  component: AdminAccountPage,
});

const adminApiDocsRoute = createRoute({
  getParentRoute: () => adminRoute,
  path: "/admin/api-docs",
  // `?ep=keys.reset` 深链到某一条接口：接入方问"这条怎么调"时，
  // 直接把地址发过去比让他自己在一长串列表里找快得多。
  // 填过的参数值不进 URL（里面有密钥与手机号）。
  validateSearch: apiDocsSearch,
  component: AdminApiDocsPage,
});

const routeTree = rootRoute.addChildren([
  indexRoute,
  loginRoute,
  registerRoute,
  showcaseRoute,
  aboutRoute,
  agreementRoute,
  docsRoute,
  docsNodeRoute,
  docsEndpointRoute,
  portalRoute.addChildren([
    homeRoute,
    modelsRoute,
    keysRoute,
    usageRoute,
    filesRoute,
    topupRoute,
    settingsRoute,
    mfaSetupRoute,
  ]),
  adminRoute.addChildren([
    dashboardRoute,
    adminUsersRoute,
    adminModelServiceRoute,
    adminDocsRoute,
    adminAgentRoute,
    adminOrdersRoute,
    adminRedeemsRoute,
    adminPackagesRoute,
    adminReconcileRoute,
    adminLogsRoute,
    adminTasksRoute,
    adminConfigRoute,
    adminAuditRoute,
    adminAuthEventsRoute,
    adminAppsRoute,
    adminApiDocsRoute,
    adminAccountRoute,
  ]),
]);

export const router = createRouter({
  routeTree,
  defaultNotFoundComponent: NotFound,
  // 整仓此前没有 ErrorBoundary：任何渲染异常都会落到 TanStack 英文默认页。
  defaultErrorComponent: RouteError,
  // beforeLoad 里 await loadSession() 期间不再白屏。
  defaultPendingComponent: RoutePending,
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
