import { beforeEach, describe, expect, it } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  RouterProvider,
  createMemoryHistory,
  createRouter,
} from "@tanstack/react-router";
import { router as appRouter } from "@/app/router";
import { ADMIN_NAV_COLLAPSED_KEY } from "@/lib/nav";
import { NotFound } from "@/pages/NotFound";
import { useAuthStore } from "@/stores/auth";

/**
 * 用真实 routeTree + 内存历史渲染指定路径。
 * chrome 组件依赖 useLocation 与已注册路径的 Link，必须置于真实路由上下文；
 * S2 起门户/管理台布局挂载会话与登出 mutation，故需 QueryClient 与登录态。
 */
function renderAt(path: string, role: 0 | 1 = 0) {
  // 直接写用户态：setUser 同时把 loaded 置真，路由守卫的 loadSession() 会命中
  // 缓存不再发请求（jsdom 里没有 /portal/me 可比）。
  useAuthStore.getState().setUser({ id: 1, username: "tester", role });
  const testRouter = createRouter({
    routeTree: appRouter.routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
    defaultNotFoundComponent: NotFound,
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={testRouter} />
    </QueryClientProvider>,
  );
  return testRouter;
}

describe("门户 chrome", () => {
  it("桌面顶栏：导航项齐全且当前路径高亮，移动 Tab Bar 不参与桌面导航", async () => {
    renderAt("/home");
    await screen.findAllByText("模型广场");
    const navLinks = document.querySelectorAll(".p-nav a");
    // 6 项（文档 /docs 已移入页脚 portalFooterNav，不占顶栏）
    expect(navLinks).toHaveLength(6);
    // 每项都带图标：图标与文字同源于 portalNav 的 icon 字段
    expect(document.querySelectorAll(".p-nav a svg")).toHaveLength(6);
    expect(document.querySelector(".p-nav a.active")?.textContent).toBe("主页");
    // 页脚次级链接：文档 + 关于我们 + 用户协议
    expect(document.querySelectorAll(".p-foot-links a")).toHaveLength(3);
    // role=0（管理员）→ 移动 Tab Bar 追加「管理台」，共 6 项
    expect(document.querySelectorAll(".p-tabbar a")).toHaveLength(6);
    expect(screen.getByRole("link", { name: /钱包余额/ })).toBeInTheDocument();
    // 公告入口是 button（点击开弹层，不再跳页），未展开时 aria-expanded=false
    const bell = screen.getByRole("button", { name: /公告/ });
    expect(bell).toHaveAttribute("aria-haspopup", "dialog");
    expect(bell).toHaveAttribute("aria-expanded", "false");
    expect(bell).not.toHaveAttribute("aria-controls");
  });

  it("管理员门户可见进入管理台入口（桌面菜单 + 移动 Tab）", async () => {
    renderAt("/home", 0);
    await screen.findAllByText("模型广场");
    expect(screen.getByRole("menuitem", { name: /切到管理台/ })).toBeInTheDocument();
    expect(document.querySelectorAll(".p-tabbar a")).toHaveLength(6);
  });

  it("普通用户门户不存在管理台入口（DOM 无该节点）", async () => {
    renderAt("/home", 1);
    await screen.findAllByText("模型广场");
    expect(screen.queryByRole("menuitem", { name: /切到管理台/ })).toBeNull();
    expect(document.querySelectorAll(".p-tabbar a")).toHaveLength(5);
    expect(document.querySelector('.p-tabbar a[href="/admin/dashboard"]')).toBeNull();
  });

  it("门户页头渲染标题并按页面归属加载对应页面", async () => {
    renderAt("/files");
    expect(await screen.findByRole("heading", { level: 1 })).toHaveTextContent("文件");
    expect(await screen.findByText("我的文件")).toBeInTheDocument();
    expect(document.querySelector(".p-nav a.active")?.textContent).toBe("文件");
  });
});

describe("管理台 chrome", () => {
  // jsdom 同一文件共享 localStorage：上一个用例收起的组会让下一个用例少几条链接
  // （只删本模块的键，别用 clear() 误伤 ld-theme 等）。
  beforeEach(() => localStorage.removeItem(ADMIN_NAV_COLLAPSED_KEY));

  it("侧栏按分区渲染全部导航项，当前项高亮，顶栏面包屑同步", async () => {
    renderAt("/admin/users");
    await screen.findByRole("heading", { level: 1 });
    // 15 项 = 5 顶级项（仪表盘/系统配置/模型服务/用户管理/应用管理）+ 3 组（4+4+2）；
    // 账户设置在头像菜单里、AI 接入助手收敛进模型服务弹窗，都不占侧栏。
    expect(document.querySelectorAll(".a-side nav a")).toHaveLength(15);
    expect(document.querySelector(".a-side nav a.active")?.textContent).toContain("用户管理");
    expect(document.querySelector(".a-top .crumb b")?.textContent).toBe("用户管理");
    // 组头只有 3 个（顶级项不画标题）；8 个分区各占一个 .nav-sec
    expect(document.querySelectorAll(".a-side .nav-group")).toHaveLength(3);
    expect(document.querySelectorAll(".a-side nav .nav-sec")).toHaveLength(8);
    // 每个组头 = 语义图标 + 折叠箭头（2 个 svg）；每条菜单项恒 1 个 —— 「开关 vs 目的」
    // 的可见区别就在这里，也是「没有 caret 的行不得被当成组头」的判据。
    expect(
      [...document.querySelectorAll(".a-side .nav-group")].map(
        (h) => h.querySelectorAll("svg").length,
      ),
    ).toEqual([2, 2, 2]);
    expect(
      [...document.querySelectorAll(".a-side nav a")].map((a) => a.querySelectorAll("svg").length),
    ).toEqual(Array(15).fill(1));
    // 层级：缩进容器只挂在**有组标题**的分区上（3 个）。
    // 顶级项（仪表盘 / 系统配置 / 模型服务 / 用户管理 / 应用管理）是同级入口，
    // 若被塞进 .nav-sub 就会被缩进成某组的子项 —— 这正是"顶级项"与"分组"必须靠结构区分的原因。
    expect(document.querySelectorAll(".a-side .nav-sub")).toHaveLength(3);
    expect(document.querySelector(".a-side nav .nav-sec")?.querySelector(".nav-sub")).toBeNull();
    // 行骨架与角色类由组件显式给出，不再依赖「在 nav 里」这个偶然属性
    expect(document.querySelectorAll(".a-side .nav-row")).toHaveLength(18); // 3 组头 + 15 项
    expect(document.querySelectorAll(".a-side .nav-item")).toHaveLength(15);
    // 读屏拿不到 class："你在这里"必须走 aria-current
    expect(document.querySelector('.a-side nav a[aria-current="page"]')?.textContent).toContain(
      "用户管理",
    );
  });

  it("分组可折叠：只加类不删节点", async () => {
    renderAt("/admin/dashboard");
    await screen.findByRole("heading", { level: 1 });
    // 折叠样本换成「交易与资金」：原样本「用户与应用」已拆成两个顶级项（不可折叠）
    const head = screen.getByRole("button", { name: "交易与资金" });
    const box = document.getElementById("nav-sec-billing");
    expect(head).toHaveAttribute("aria-expanded", "true");
    expect(box).not.toHaveClass("is-collapsed");

    fireEvent.click(head);
    expect(head).toHaveAttribute("aria-expanded", "false");
    expect(box).toHaveClass("is-collapsed");
    // 关键：链接仍在 DOM（可见性由 CSS 负责，jsdom 不加载样式表，断言可见性会假通过）
    expect(box?.querySelectorAll("a")).toHaveLength(4);
    expect(document.querySelectorAll(".a-side nav a")).toHaveLength(15);

    fireEvent.click(head);
    expect(head).toHaveAttribute("aria-expanded", "true");
    expect(box).not.toHaveClass("is-collapsed");
  });

  it("落在已收起分区里的页面会自动展开该分区", async () => {
    // 书签 / 刷新 / 浏览器后退的典型场景
    localStorage.setItem(ADMIN_NAV_COLLAPSED_KEY, JSON.stringify(["runtime"]));
    renderAt("/admin/logs");
    await screen.findByRole("heading", { level: 1 });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "运行与审计" })).toHaveAttribute(
        "aria-expanded",
        "true",
      ),
    );
    // 展开同时落盘（空集合删键），下次进来不会又收起
    expect(localStorage.getItem(ADMIN_NAV_COLLAPSED_KEY)).toBeNull();
  });

  it("管理台菜单含返回门户入口", async () => {
    renderAt("/admin/dashboard");
    await screen.findByRole("heading", { level: 1 });
    expect(screen.getByRole("menuitem", { name: /返回门户/ })).toBeInTheDocument();
  });

  it("管理台公告入口与门户是同一个按钮（公告整页已删除）", async () => {
    renderAt("/admin/dashboard");
    await screen.findByRole("heading", { level: 1 });
    const bell = screen.getByRole("button", { name: /公告/ });
    expect(bell).toHaveAttribute("aria-haspopup", "dialog");
    expect(bell).toHaveAttribute("aria-expanded", "false");
    // 不能退回「点铃铛跳整页」：那会重新长出第二套公告展示实现
    expect(document.querySelector('a[href="/admin/announcements"]')).toBeNull();
  });
});

describe("路由骨架", () => {
  it("根路径重定向到 /home", async () => {
    const r = renderAt("/");
    await screen.findByRole("heading", { level: 1 });
    expect(r.state.location.pathname).toBe("/home");
  });

  it("未知路径落到 404", async () => {
    renderAt("/nope");
    expect(await screen.findByText("页面不存在或已被移除")).toBeInTheDocument();
  });

  it("展示台路由可渲染设计系统样例", async () => {
    renderAt("/__showcase");
    expect(await screen.findByRole("heading", { level: 1 })).toHaveTextContent("设计系统展示台");
    expect(screen.getByText("统计卡与徽标")).toBeInTheDocument();
  });
});
