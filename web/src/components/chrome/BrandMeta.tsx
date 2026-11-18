import { useEffect } from "react";
import { useSite } from "@/lib/site";

/**
 * 浏览器标签的品牌同步（唯一出口）。挂在**根路由**：门户、登录、注册、管理台
 * 都会经过它，于是标题与图标在全站只有一个来源，不必每个 layout 各挂一次。
 *
 * 为什么需要：`index.html` 里的 `<title>` 是**构建期**常量，站点改名后标签页
 * 仍显示旧站名；图标此前根本没有 `<link rel="icon">`，改配置也不会有任何效果。
 */
export function BrandMeta() {
  const site = useSite();

  useEffect(() => {
    document.title = site.name;
    // 用固定 id 复用同一个节点：多枚 rel=icon 时浏览器选哪枚是不确定的，
    // 每次 effect 都 append 一个会留下"旧图标仍然生效"的诡异现象。
    let link = document.getElementById("site-favicon") as HTMLLinkElement | null;
    if (!link) {
      link = document.createElement("link");
      link.id = "site-favicon";
      link.rel = "icon";
      document.head.appendChild(link);
    }
    link.href = site.favicon;
    // type 由来源决定：dataURI 自带 mime，外链图片则不声明（写死 png 会让 svg 不被识别）
    link.type = site.favicon.startsWith("data:image/svg") ? "image/svg+xml" : "";
  }, [site.name, site.favicon]);

  return null;
}
