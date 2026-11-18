import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "@tanstack/react-router";

import { useOverlayBehavior } from "@/components/common/useOverlayBehavior";
import { usePortalConfigQuery } from "@/lib/api";
import { useDocEndpoint, useDocNode, useDocsTree } from "@/lib/api/docsv7";
import { useIsDocsDrawer } from "@/lib/responsive";
import { useSite } from "@/lib/site";
import { useAuthStore } from "@/stores/auth";
import { DOC_V7_SHAPE, type DocV7Catalog, type DocV7Endpoint } from "@/types/docsv7";

import "./docs.css";
import { EndpointDoc } from "./EndpointDoc";
import { GuideView } from "./GuideView";
import { Rail } from "./Rail";
import { Sidebar } from "./Sidebar";

/**
 * 文档站（公开站）——唯一入口组件。
 *
 * 两种页面形态，没有第三种：
 *   - 接口页：侧栏 + 正文 + 右栏（三栏）
 *   - 说明页：侧栏 + 正文（两栏）
 *
 * 节点不再是页面：`/docs/{node}` 在目录就绪后 `replace` 到该节点的第一个接口。
 * 之前的节点页是一张「适用模型 + 接口清单」的稀疏页 —— 读者点节点本来就想看
 * 某个接口，多一次点击只多一次迷路。
 *
 * 状态判定的顺序不可换：**先等目录**，再判节点/接口。目录未到时就说「没有这篇
 * 文档」，刷新瞬间会先闪一下 404 再跳走。
 */

type RailMode = "docs" | "try";

/** 落地页目标：第一篇说明页 → 第一个有接口的节点 → null（空站）。 */
function landingOf(cat: DocV7Catalog): { node: string; ep?: string } | null {
  for (const s of cat.sections) {
    for (const n of s.nodes) if (n.kind === "guide") return { node: n.key };
  }
  for (const s of cat.sections) {
    for (const n of s.nodes) if (n.endpoints.length > 0) return { node: n.key, ep: n.endpoints[0].key };
  }
  return null;
}

function findNode(cat: DocV7Catalog, key: string) {
  for (const s of cat.sections) {
    for (const n of s.nodes) if (n.key === key) return n;
  }
  return null;
}

export function DocsApp({ nodeKey = "", endpointKey = "" }: { nodeKey?: string; endpointKey?: string }) {
  const navigate = useNavigate();
  const site = useSite();
  const tree = useDocsTree();
  const [sideOpen, setSideOpen] = useState(false);
  const [railMode, setRailMode] = useState<RailMode>("docs");
  const railRef = useRef<HTMLDivElement | null>(null);
  const navRef = useRef<HTMLElement | null>(null);
  const mainRef = useRef<HTMLDivElement | null>(null);
  const isDrawer = useIsDocsDrawer();

  // 品牌入口的两个判定，与全站既有先例同源（AdminTopBar / PortalLayout）：
  //   - 门户关闭时不给「回门户」——/home 的守卫会把人弹回登录页，链接等于陷阱；
  //   - 只有 role===0 才给「管理后台」。
  const portalCfg = usePortalConfigQuery();
  const portalOpen = portalCfg.data?.portal_enabled !== false;
  const isAdmin = useAuthStore((s) => s.user?.role === 0);

  const cat = tree.data;
  const node = cat && nodeKey ? findNode(cat, nodeKey) : null;
  const isGuide = node?.kind === "guide";

  // 目录没到时一律不判存在性（否则刷新闪 404）。
  const guide = useDocNode(nodeKey, !!isGuide);
  const ep = useDocEndpoint(nodeKey, endpointKey, !!cat && !!node && !isGuide && !!endpointKey);

  // 切接口/切节点：右栏模式必须复位，且 Rail 用 key 重建 —— 否则上一个接口的
  // 勾选、Key、响应体会原样留在下一个接口上（同一路由实例不会卸载）。
  // 三栏各自滚动后，`.d-main`/`.d-rail` 是页面内的滚动容器，路由切换只复位 window，
  // 不复位它们就会出现「点下一个接口，正文停在上一页的中部」。
  useEffect(() => {
    setRailMode("docs");
    setSideOpen(false);
    mainRef.current?.scrollTo({ top: 0 });
    railRef.current?.scrollTo({ top: 0 });
  }, [nodeKey, endpointKey]);

  // 抽屉的行为链（锁滚动 / Esc / 焦点陷阱 / 关闭后归还焦点）全站只有这一个实现。
  // `open` 必须带 isDrawer：只写 sideOpen 的话，用户从 1024 拖到 1600 时会残留
  // sideOpen=true，页面被永久锁滚。
  useOverlayBehavior(isDrawer && sideOpen, () => setSideOpen(false), navRef);

  // 落地与重定向。只在目录就绪后跑，且用 replace：读者按返回键不该回到
  // 「会立刻跳走」的那一页。
  useEffect(() => {
    if (!cat) return;
    if (!nodeKey) {
      const target = landingOf(cat);
      if (!target) return;
      if (target.ep) {
        void navigate({
          to: "/docs/$nodeKey/$endpointKey",
          params: { nodeKey: target.node, endpointKey: target.ep },
          replace: true,
        });
      } else {
        void navigate({ to: "/docs/$nodeKey", params: { nodeKey: target.node }, replace: true });
      }
      return;
    }
    if (!node || node.kind === "guide" || endpointKey) return;
    if (node.endpoints.length > 0) {
      void navigate({
        to: "/docs/$nodeKey/$endpointKey",
        params: { nodeKey, endpointKey: node.endpoints[0].key },
        replace: true,
      });
    }
  }, [cat, nodeKey, endpointKey, node, navigate]);

  const title = useMemo(() => {
    const doc = ep.data;
    if (doc) return `${doc.name} · ${site.name} 文档`;
    if (isGuide && guide.data) return `${guide.data.title} · ${site.name} 文档`;
    return `${site.name} · API 文档`;
  }, [ep.data, isGuide, guide.data, site.name]);

  useEffect(() => {
    const prev = document.title;
    document.title = title;
    return () => {
      document.title = prev;
    };
  }, [title]);

  // 右栏模式的唯一出口：切模式时把右栏滚回顶部。否则「先把示例滚到底、再点调试」
  // 会让运行面板带着原来的 scrollTop 出现 —— 标题与发送按钮都在视口外。
  const setMode = (m: RailMode) => {
    setRailMode(m);
    railRef.current?.scrollTo({ top: 0 });
  };

  const openTry = () => {
    setMode("try");
    // 桌面右栏就在旁边，切换即可；窄屏右栏在正文下方，不滚过去读者看不到反应。
    // `scroll-margin-top:52px`（docs.css 媒体块）负责让出顶栏高度。
    if (isDrawer) railRef.current?.scrollIntoView({ behavior: "smooth", block: "start" });
  };

  /* ── 失败态（互不合并：每种原因给不同的下一步） ── */

  if (tree.isLoading) {
    return (
      <div className="d-root">
        <div className="d-app">
          <div className="d-side d-side-skel" />
          <div className="d-main">
            <div className="d-main-in">
              <div className="d-skel w40" />
              <div className="d-skel w80" />
              <div className="d-skel w60" />
            </div>
          </div>
        </div>
      </div>
    );
  }

  if (tree.isError || !cat) {
    return (
      <div className="d-root">
        <div className="d-empty">
          <p>文档加载失败。</p>
          <p className="d-hint">
            可能是网络问题；如果本部署没有开启文档站，这里也会是空的。
          </p>
          <button type="button" className="d-btn-try" onClick={() => void tree.refetch()}>
            重试
          </button>
        </div>
      </div>
    );
  }

  if (cat.shape !== DOC_V7_SHAPE) {
    return (
      <div className="d-root">
        <div className="d-empty">
          <p>文档版本不一致。</p>
          <p className="d-hint">页面缓存可能过期，刷新一次即可。</p>
          <button type="button" className="d-btn-try" onClick={() => window.location.reload()}>
            刷新
          </button>
        </div>
      </div>
    );
  }

  if (!landingOf(cat)) {
    return (
      <div className="d-root">
        <div className="d-empty">
          <p>文档尚未生成。</p>
          <p className="d-hint">接口发布后会自动出现在这里。</p>
        </div>
      </div>
    );
  }

  // 4 处 Shell 调用的公共底座：抽一次。漏传 navRef 会让抽屉的焦点管理失联，
  // 漏传 drawer 会让桌面态带上 inert —— 这类分叉在 4 份重复里迟早出现。
  const shellCommon = {
    cat,
    siteName: site.name,
    siteLogo: site.logoImage,
    sideOpen,
    onCloseSide: () => setSideOpen(false),
    onOpenSide: () => setSideOpen(true),
    drawer: isDrawer,
    navRef,
    mainRef,
    homeHref: portalOpen ? "/home" : "",
    isAdmin,
  };

  // 目录就绪但目标不在其中：真 404（不是加载态）。
  if (nodeKey && !node) {
    return (
      <div className="d-root">
        <Shell {...shellCommon} nodeKey={nodeKey} epKey="">
          <div className="d-empty">
            <p>没有这篇文档。</p>
            <button type="button" className="d-btn-try" onClick={() => void navigate({ to: "/docs" })}>
              回到首页
            </button>
          </div>
        </Shell>
      </div>
    );
  }

  /* ── 说明页（两栏） ── */

  if (node && node.kind === "guide") {
    return (
      <div className="d-root">
        <Shell {...shellCommon} nodeKey={nodeKey} epKey="">
          {guide.isLoading ? (
            <BodySkeleton />
          ) : guide.data ? (
            <div className="d-main-in">
              <GuideView node={guide.data} />
            </div>
          ) : (
            <div className="d-empty">
              <p>这篇说明页加载失败。</p>
              <button type="button" className="d-btn-try" onClick={() => void guide.refetch()}>
                重试
              </button>
            </div>
          )}
        </Shell>
      </div>
    );
  }

  /* ── 接口页（三栏） ── */

  if (!endpointKey) {
    // `/docs/{node}`：节点没有接口（重定向 effect 会处理有接口的情形）。
    return (
      <div className="d-root">
        <Shell {...shellCommon} nodeKey={nodeKey} epKey="">
          <div className="d-empty">
            <p>本节点暂无已发布接口。</p>
          </div>
        </Shell>
      </div>
    );
  }

  return (
    <div className="d-root">
      <Shell
        {...shellCommon}
        nodeKey={nodeKey}
        epKey={endpointKey}
        railRef={railRef}
        rail={
          ep.data ? (
            <Rail
              key={ep.data.key}
              doc={ep.data}
              baseUrl={cat.base_url}
              mode={railMode}
              onMode={setMode}
            />
          ) : null
        }
      >
        {ep.isLoading ? (
          <BodySkeleton />
        ) : ep.data ? (
          <div className="d-main-in">
            <EndpointDoc doc={ep.data} onTry={openTry} />
          </div>
        ) : (
          <div className="d-empty">
            <p>这篇接口文档加载失败。</p>
            <button type="button" className="d-btn-try" onClick={() => void ep.refetch()}>
              重试
            </button>
          </div>
        )}
      </Shell>
    </div>
  );
}

/** 三栏外壳：移动顶栏 + 侧栏抽屉 + 正文 + 右栏。 */
function Shell({
  cat,
  nodeKey,
  epKey,
  siteName,
  siteLogo,
  sideOpen,
  onCloseSide,
  onOpenSide,
  drawer,
  navRef,
  mainRef,
  homeHref,
  isAdmin,
  rail,
  railRef,
  children,
}: {
  cat: DocV7Catalog;
  nodeKey: string;
  epKey: string;
  siteName: string;
  siteLogo: string;
  sideOpen: boolean;
  onCloseSide: () => void;
  onOpenSide: () => void;
  /** 处于抽屉形态（≤1100）。决定 inert / role / 遮罩是否渲染。 */
  drawer: boolean;
  navRef: React.Ref<HTMLElement>;
  mainRef: React.Ref<HTMLDivElement>;
  /** 门户首页地址；空串 = 门户已关闭，品牌降级为不可点。 */
  homeHref: string;
  isAdmin: boolean;
  rail?: React.ReactNode;
  railRef?: React.Ref<HTMLDivElement>;
  children: React.ReactNode;
}) {
  return (
    <div className={`d-app${sideOpen ? " side-open" : ""}`}>
      {/* 顶栏是 .d-app 的兄弟而不是 .d-main 的子节点：放进正文会被正文的 16px
          内边距包住，两侧各短一截、上方还多一条空带。 */}
      <div className="d-topbar">
        <b>{siteName}</b>
        <button
          type="button"
          className="d-burger"
          aria-label="打开目录"
          aria-expanded={sideOpen}
          aria-controls="docs-side"
          onClick={onOpenSide}
        >
          ☰
        </button>
      </div>

      <Sidebar
        id="docs-side"
        catalog={cat}
        nodeKey={nodeKey}
        epKey={epKey}
        siteName={siteName}
        siteLogo={siteLogo}
        drawer={drawer}
        sideOpen={sideOpen}
        onOpen={onOpenSide}
        onNavigate={onCloseSide}
        homeHref={homeHref}
        isAdmin={isAdmin}
        navRef={navRef}
      />

      {/* 条件渲染：桌面态不存在这个按钮，也就不会被 Tab 聚焦到一个看不见的东西上 */}
      {drawer && sideOpen ? (
        <button type="button" className="d-scrim" aria-label="关闭目录" onClick={onCloseSide} />
      ) : null}

      <div className="d-main" ref={mainRef}>
        {children}
      </div>

      {rail ? (
        <div className="d-rail" ref={railRef} aria-label="示例与调试">
          {rail}
        </div>
      ) : null}
    </div>
  );
}

function BodySkeleton() {
  return (
    <div className="d-main-in">
      <div className="d-skel w40" />
      <div className="d-skel w80" />
      <div className="d-skel w60" />
    </div>
  );
}

/**
 * 工作台右栏预览（`/admin/docs`）。
 *
 * 与公开站共用 `EndpointDoc` / `Rail`，差别只有三处：没有侧栏与顶栏、
 * 不显示 Markdown 与调试入口、右栏落到正文下方。**外观必须一致** ——
 * 运营在预览里看到的排版就是读者看到的排版，否则预览没有意义。
 *
 * 基址取自目录（`catalog.base_url`，与后端示例代码同源）。预览不需要联网取
 * 目录时也能工作：取不到就传空串，Rail 只用来展示示例文本。
 */
export function DocsPreview({ doc, baseUrl: given }: { doc: DocV7Endpoint; baseUrl?: string }) {
  // 基址由调用方给（管理台从 /admin/docs/tree 拿到），不再自己拉一次公开目录。
  const tree = useDocsTree(given === undefined);
  const baseUrl = given ?? tree.data?.base_url ?? "";
  return (
    <div className="d-root d-embed" data-testid="docs-embed">
      <EndpointDoc doc={doc} embedded />
      <div className="d-rail">
        <Rail doc={doc} baseUrl={baseUrl} mode="docs" onMode={() => {}} embedded />
      </div>
    </div>
  );
}
