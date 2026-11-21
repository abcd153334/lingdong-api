import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import { toast } from "sonner";
import { router } from "@/app/router";
import { fetchPricingSnapshot, queryKeys } from "@/lib/api";
import {
  setForbiddenHandler,
  setMFASetupHandler,
  setPortalDisabledHandler,
  setUnauthorizedHandler,
} from "@/lib/apiClient";
import { invalidatePortalMode } from "@/lib/portalMode";
import { loadSession } from "@/lib/session";
import { queryClient, clearAccountQueries } from "@/lib/queryClient";
import { Toaster } from "@/components/common/Toaster";
import { ConfirmDialogHost } from "@/components/common/ConfirmDialog";
import { PageScrollbar } from "@/components/chrome/PageScrollbar";
import "@/styles/app.css";

/**
 * 清掉上一版前端的遗留态。
 *
 * 旧实现把 `{token, user}` 持久化在 localStorage 的 `ld-auth` 里。令牌已经改成
 * httpOnly Cookie，那份缓存不但没用，还会让老用户看到一个「已登录」的顶栏
 * 而每个接口都 401。没有存量兼容义务，直接删。
 */
try {
  window.localStorage.removeItem("ld-auth");
} catch {
  /* 隐私模式下 localStorage 可能抛错：与登录无关，忽略 */
}

// 401 全局拦截：本地态已由 apiClient 清空，这里只把裁决权交回**路由守卫**。
//
// 为什么不能直接 navigate('/login')：公开页（/register、/docs、/about…）首屏
// 探测会话必然吃到一个 401，直接导航会把未登录访客从注册页弹到登录页 ——
// 注册页因此**永远打不开**（e2e auth.spec 抓到）。
// 而「哪些路由需要登录」的真相源是守卫本身（portal / admin 两个 layout route
// 的 beforeLoad），在这里再抄一份路径白名单必然漂移。
//
// invalidate 会重跑当前匹配路由的 beforeLoad：受保护路由自己抛 redirect
// （并带上 ?redirect= 回跳参数），公开路由没有守卫，所以什么都不发生。
setUnauthorizedHandler(() => {
  // 先清账户级缓存再让守卫重跑：queryKey 不含用户维度、gcTime 默认 5 分钟，
  // 不清的话共享终端上「A 掉线、B 登录」会在 5 分钟内看到 A 的余额/密钥/订单。
  // apiClient 只清了 auth store，它不能 import queryClient（会成环），所以放这里。
  clearAccountQueries();
  void router.invalidate();
});

// 403 全局拦截：提示权限不足
setForbiddenHandler((message) => {
  toast.error(message);
});

// 403 portal_disabled：本端缓存的门户模式与后端不一致（多半是刚被关掉）。
// 失效缓存后由上面的 401 收尾把裁决交回守卫，下一次导航就会按新模式走。
setPortalDisabledHandler(() => {
  invalidatePortalMode();
});

// 403 mfa_setup_required：策略要求管理员绑定两步验证。
// 只能导航（不能只弹提示）—— 否则用户停在满屏 403 的管理台里。
setMFASetupHandler(() => {
  if (window.location.pathname !== "/mfa/setup") {
    void router.navigate({ to: "/mfa/setup" });
  }
});

// 首屏探测会话：守卫里也会 await 同一个 promise（并发合并），这里只是提前发起，
// 让登录态在首屏渲染前就绪，避免受保护页面闪一下登录页。
void loadSession();

// 首屏无条件拉一次定价目录（W19）。
//
// 为什么不能只靠页面里的 usePricingQuery：结算口径（币种/符号/qpc）由它顺带注入，
// 而 /admin/dashboard、/portal/home 这些页面**不请求**定价目录。那些页面在
// 用户从未进过模型广场/钱包页时会用 money.ts 的默认口径渲染金额 ——
// 单币种时代只是符号差异，多币种下就是差 7.2 倍的错数。
//
// 失败不阻塞渲染也不弹错误：定价目录挂了的时候页面本就不可用，
// 再叠一个全局 toast 只会盖住真正的报错；这里只留一条 warn 便于排查。
void queryClient
  .fetchQuery({
    queryKey: queryKeys.pricing,
    queryFn: fetchPricingSnapshot,
    staleTime: 60_000,
  })
  .catch(() => {
    console.warn("定价目录拉取失败，金额将按默认口径显示");
  });

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
      {/* 页面级滚动条：必须在路由之外 —— 它对所有页面（含登录页、404）一视同仁，
          且原生页面滚动条已被 app.css 隐藏为 0 宽，不挂它就没有任何滚动反馈。 */}
      <PageScrollbar />
      <Toaster />
      <ConfirmDialogHost />
    </QueryClientProvider>
  </StrictMode>,
);
