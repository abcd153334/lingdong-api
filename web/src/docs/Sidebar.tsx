import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "@tanstack/react-router";

import { Icon } from "@/components/common/Icon";
import type { DocV7Catalog } from "@/types/docsv7";

/**
 * 侧栏：品牌 + 搜索 + 级联目录（板块 → 节点 → 接口）+ 全站导出。
 *
 * 三段结构与原型一致：板块可折叠、节点可折叠、当前所在那条默认展开。
 *
 * 折叠状态存 `sessionStorage` 而不是只在内存里：点开一个接口会走路由跳转，
 * 组件不卸载但读者从别的页面回来时希望保持原样；只在内存里的话，刷新一次
 * 目录就回到默认展开，而默认展开几百个接口时找不到位置。
 *
 * 搜索在前端做：目录一次就拿全了（名称 / 方法 / 路径），几十条量级，
 * 再开一个后端搜索端点只会多一套分页与排序。
 */

const CLOSED_KEY = "ld_docs_closed";

type Closed = { sec: string[]; node: string[] };

function readClosed(): Closed | null {
  try {
    const raw = sessionStorage.getItem(CLOSED_KEY);
    if (!raw) return null;
    const v = JSON.parse(raw) as Closed;
    return Array.isArray(v?.sec) && Array.isArray(v?.node) ? v : null;
  } catch {
    return null;
  }
}

function writeClosed(v: Closed): void {
  try {
    sessionStorage.setItem(CLOSED_KEY, JSON.stringify(v));
  } catch {
    // 隐私模式可能抛：折叠状态退回内存态，不影响阅读。
  }
}

interface Hit {
  node: string;
  ep: string;
  name: string;
  method: string;
  path: string;
  group: string;
  rank: number;
}

export function Sidebar({
  id,
  catalog,
  nodeKey,
  epKey,
  siteName,
  siteLogo,
  drawer,
  sideOpen,
  onOpen,
  onNavigate,
  homeHref,
  isAdmin,
  navRef,
}: {
  id?: string;
  catalog: DocV7Catalog;
  nodeKey: string;
  epKey: string;
  siteName: string;
  siteLogo: string;
  /** 处于抽屉形态（≤1100）；桌面态恒 false，决定 inert/role 是否生效。 */
  drawer: boolean;
  sideOpen: boolean;
  onOpen: () => void;
  onNavigate: () => void;
  /** 门户首页地址；空串 = 门户已关闭，品牌降级为不可点（给了链接也会被守卫弹回）。 */
  homeHref: string;
  isAdmin: boolean;
  navRef?: React.Ref<HTMLElement>;
}) {
  const navigate = useNavigate();
  const [q, setQ] = useState("");
  const [idx, setIdx] = useState(0);
  const [resOpen, setResOpen] = useState(false);
  const inputRef = useRef<HTMLInputElement | null>(null);
  const boxRef = useRef<HTMLDivElement | null>(null);

  // null = 读者还没动过，按「当前所在 + 第一个板块」的默认规则展开。
  const [closed, setClosed] = useState<Closed | null>(readClosed);

  useEffect(() => {
    if (closed) writeClosed(closed);
  }, [closed]);

  const hits = useMemo(() => {
    const kw = q.trim().toLowerCase();
    if (!kw) return [] as Hit[];
    const out: Hit[] = [];
    for (const s of catalog.sections) {
      for (const n of s.nodes) {
        for (const e of n.endpoints) {
          const hay = [e.name, e.path, e.method, n.title, s.name].join(" ").toLowerCase();
          if (!hay.includes(kw)) continue;
          out.push({
            node: n.key,
            ep: e.key,
            name: e.name,
            method: e.method,
            path: e.path,
            group: n.title || s.name,
            rank: e.name.toLowerCase().includes(kw) ? 0 : 1,
          });
        }
      }
    }
    out.sort((a, b) => a.rank - b.rank || a.name.localeCompare(b.name));
    return out.slice(0, 8);
  }, [q, catalog]);

  useEffect(() => setIdx(0), [q]);

  // 点外部关下拉：用 pointerdown 而不是 click —— 在输入框里拖选文字、松手落在
  // 框外时，click 会把刚选中的搜索结果关掉。
  useEffect(() => {
    if (!resOpen) return;
    const onDown = (e: PointerEvent) => {
      if (!boxRef.current?.contains(e.target as Node)) setResOpen(false);
    };
    document.addEventListener("pointerdown", onDown);
    return () => document.removeEventListener("pointerdown", onDown);
  }, [resOpen]);

  // `/` 聚焦搜索、⌘K / Ctrl+K 同理。都只在焦点不在输入框时接管，
  // 否则读者在参数搜索框里打斜杠会被侧栏抢走。
  useEffect(() => {
    const onKey = (ev: KeyboardEvent) => {
      const el = document.activeElement;
      const typing = el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement;
      const cmdK = (ev.metaKey || ev.ctrlKey) && ev.key.toLowerCase() === "k";
      const slash = ev.key === "/" && !ev.metaKey && !ev.ctrlKey && !ev.altKey;
      if (!cmdK && !(slash && !typing)) return;
      ev.preventDefault();
      // 抽屉关着时侧栏是 inert，直接聚焦会静默失败 —— 先把抽屉打开，
      // 由 useOverlayBehavior 的初始聚焦把焦点落到第一个可聚焦元素上。
      if (drawer && !sideOpen) {
        onOpen();
        return;
      }
      inputRef.current?.focus();
      inputRef.current?.select();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [drawer, sideOpen, onOpen]);

  const go = (node: string, ep: string) => {
    setQ("");
    onNavigate();
    void navigate({ to: "/docs/$nodeKey/$endpointKey", params: { nodeKey: node, endpointKey: ep } });
  };

  /** 用户第一次折叠时的基线：按默认规则算出「本来是关着的」。 */
  const defaultClosed = (): Closed => {
    const sec: string[] = [];
    catalog.sections.forEach((s, i) => {
      const active = s.nodes.some((n) => n.key === nodeKey);
      if (!active && i !== 0) sec.push(s.id);
    });
    const node: string[] = [];
    for (const s of catalog.sections) {
      for (const n of s.nodes) {
        if (n.key !== nodeKey && n.endpoints.length > 0) node.push(n.key);
      }
    }
    return { sec, node };
  };

  const setOpenSec = (id: string, open: boolean) =>
    setClosed((prev) => {
      const base = prev ?? defaultClosed();
      return {
        sec: open ? base.sec.filter((x) => x !== id) : [...new Set([...base.sec, id])],
        node: base.node,
      };
    });

  const setOpenNode = (key: string, open: boolean) =>
    setClosed((prev) => {
      const base = prev ?? defaultClosed();
      return {
        sec: base.sec,
        node: open ? base.node.filter((x) => x !== key) : [...new Set([...base.node, key])],
      };
    });

  const isSecOpen = (id: string) => {
    if (closed) return !closed.sec.includes(id);
    const s = catalog.sections.find((x) => x.id === id);
    const active = !!s?.nodes.some((n) => n.key === nodeKey);
    return active || catalog.sections.findIndex((x) => x.id === id) === 0;
  };

  const isNodeOpen = (key: string) => {
    if (closed) return !closed.node.includes(key);
    return key === nodeKey;
  };

  return (
    <aside
      id={id}
      className="d-side"
      ref={navRef}
      aria-label="文档目录"
      /* 只在「是抽屉且关着」时 inert：桌面态一旦被 inert，1600 视口下按 `/`
         聚焦搜索框会静默失败（e2e 与真实键盘用户同时失效）。 */
      inert={drawer && !sideOpen}
      role={drawer && sideOpen ? "dialog" : undefined}
      aria-modal={drawer && sideOpen ? true : undefined}
    >
      <div className="d-side-brand">
        {homeHref ? (
          <Link to="/home" className="d-brand-home" onClick={onNavigate} title={`${siteName} 门户首页`}>
            <BrandMark siteLogo={siteLogo} siteName={siteName} />
          </Link>
        ) : (
          <div className="d-brand-home">
            <BrandMark siteLogo={siteLogo} siteName={siteName} />
          </div>
        )}
        {/* 管理员入口常显（不做悬停出现）：移动端没有 hover，而抽屉正是移动端
            唯一的导航入口，藏起来等于没有。 */}
        {isAdmin ? (
          <Link to="/admin/dashboard" className="d-brand-admin" onClick={onNavigate}>
            管理后台
          </Link>
        ) : null}
        <button type="button" className="d-side-x" onClick={onNavigate} aria-label="关闭目录">
          ✕
        </button>
      </div>

      <div className="d-side-search">
        <div className="d-search-box" ref={boxRef}>
          {/* 图标必须是 Icon 组件的 SVG：此前用的是 `⌕`（U+2315）文字字形，
              它没有统一度量 —— 不同字体下大小/基线都不同，看着像「一个字」而不是图标。 */}
          <span className="d-search-ico" aria-hidden="true">
            <Icon name="search" size={14} />
          </span>
          <input
            ref={inputRef}
            type="search"
            value={q}
            placeholder="搜索接口或路径"
            autoComplete="off"
            aria-label="搜索接口"
            onFocus={() => setResOpen(true)}
            onChange={(e) => {
              setQ(e.target.value);
              setResOpen(true);
            }}
            onKeyDown={(e) => {
              if (e.key === "Escape") {
                // 只清词、不关抽屉：抽屉的 Esc 由 useOverlayBehavior 挂在 document 上，
                // 这里必须截断冒泡，否则一次 Esc 会同时清词和关抽屉。
                e.stopPropagation();
                setQ("");
                setResOpen(false);
                e.currentTarget.blur();
                return;
              }
              if (!hits.length) return;
              if (e.key === "ArrowDown" || e.key === "ArrowUp") {
                e.preventDefault();
                setIdx((i) => (i + (e.key === "ArrowDown" ? 1 : -1) + hits.length) % hits.length);
                return;
              }
              if (e.key === "Enter") {
                e.preventDefault();
                const h = hits[idx];
                if (h) go(h.node, h.ep);
              }
            }}
          />
          <span className="d-search-kbd">/</span>
          {resOpen && q.trim() ? (
            <div className="d-search-res">
              {hits.length === 0 ? (
                <div className="d-sempty">没有匹配的接口</div>
              ) : (
                hits.map((h, i) => (
                  <button
                    key={`${h.node}/${h.ep}`}
                    type="button"
                    className={`d-sres${i === idx ? " on" : ""}`}
                    onMouseEnter={() => setIdx(i)}
                    onClick={() => go(h.node, h.ep)}
                  >
                    <span className="nm">{h.name}</span>
                    <span className="grp">{h.group}</span>
                    <span className={`d-mth ${h.method}`}>{h.method}</span>
                  </button>
                ))
              )}
            </div>
          ) : null}
        </div>
      </div>

      <div className="d-side-list">
        {catalog.sections.map((s) => {
          const openSec = isSecOpen(s.id);
          return (
            <div key={s.id} className={`d-sec${openSec ? "" : " closed"}`}>
              <button
                type="button"
                className="d-sec-h"
                aria-expanded={openSec}
                onClick={() => setOpenSec(s.id, !openSec)}
              >
                {s.name}
                <span className="d-chev" />
              </button>
              <div className="d-sec-body">
                {s.nodes.map((n) => {
                  const active = n.key === nodeKey;
                  // 说明页与没有接口的节点：直接一个链接（没有第三层可展开）。
                  if (n.kind === "guide" || n.endpoints.length === 0) {
                    return (
                      <Link
                        key={n.key}
                        to="/docs/$nodeKey"
                        params={{ nodeKey: n.key }}
                        className={`d-ep d-ep-solo${active ? " on" : ""}`}
                        aria-current={active ? "page" : undefined}
                        onClick={onNavigate}
                      >
                        <span className="nm">{n.title}</span>
                      </Link>
                    );
                  }
                  const open = isNodeOpen(n.key);
                  return (
                    <div key={n.key}>
                      <button
                        type="button"
                        className={`d-mod${open ? "" : " closed"}`}
                        aria-expanded={open}
                        onClick={() => setOpenNode(n.key, !open)}
                      >
                        {n.title}
                        <span className="d-chev" />
                      </button>
                      <div className={`d-eps${open ? "" : " closed"}`}>
                        {n.endpoints.map((e) => {
                          const on = active && e.key === epKey;
                          return (
                            <Link
                              key={e.key}
                              to="/docs/$nodeKey/$endpointKey"
                              params={{ nodeKey: n.key, endpointKey: e.key }}
                              className={`d-ep${on ? " on" : ""}`}
                              aria-current={on ? "page" : undefined}
                              onClick={onNavigate}
                            >
                              <span className="nm">{e.name}</span>
                              <span className={`d-mth ${e.method}`}>{e.method}</span>
                            </Link>
                          );
                        })}
                      </div>
                    </div>
                  );
                })}
              </div>
            </div>
          );
        })}
      </div>

      <div className="d-side-foot">
        <a className="d-side-dl" href="/api/docs/markdown" download="api-docs.md">
          <Icon name="download" size={14} />
          下载全站 Markdown
        </a>
      </div>
    </aside>
  );
}

/** 品牌标记（logo + 站名），链接态与静态态共用同一份结构。 */
function BrandMark({ siteLogo, siteName }: { siteLogo: string; siteName: string }) {
  return (
    <>
      <span className="d-brand-logo">
        {siteLogo ? <img src={siteLogo} alt="" /> : (siteName || "A").slice(0, 1)}
      </span>
      <span className="d-brand-tx">
        <b>{siteName}</b>
        <span>API 文档</span>
      </span>
    </>
  );
}
