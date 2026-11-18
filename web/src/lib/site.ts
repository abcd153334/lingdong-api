import { usePortalConfigQuery } from "@/lib/api";
import { initialOf } from "@/lib/utils";

/**
 * 站点品牌降级常量（唯一一份）。
 *
 * 这里**不含任何域名**：曾经兜底过 `https://api.example.com`，于是自建部署
 * 在配置未填时会把别人的域名显示给用户、还让用户复制走。对外地址的兜底由后端
 * `siteurl.DisplayBases` 统一决定（配置 → 站点地址 → 请求 origin），前端只展示。
 */
export const SITE_FALLBACK = {
  siteName: "灵动 API",
  footerText: "",
  icpBeian: "",
} as const;

/** 站点品牌快照：配置优先，缺失回落 SITE_FALLBACK */
export interface SiteView {
  name: string;
  /** 品牌图（dataURI/URL）；空则渲染站名首字块 */
  logoImage: string;
  /** 浏览器标签图标：独立配置 → Logo → 站名首字（恒非空，可直接挂 link[rel=icon]） */
  favicon: string;
  contactEmail: string;
  siteUrl: string;
  /** 首条对外地址（origin，**不含 /v1**）：回调、产物直链、示例都用它 */
  apiBase: string;
  /** 全部对外地址（origin）：密钥页逐条列出供用户自选 */
  apiBases: string[];
  /** 地址来源，用于在界面上如实标注「这是兜底值」 */
  apiSource: "config" | "site_url" | "request";
  footerText: string;
  icpBeian: string;
}

/**
 * 给 origin 补 `/v1`（OpenAI 兼容基址）。
 *
 * 库里恒存 origin：产物直链是 `{base}/f/...`、支付回调是 `{base}/api/pay/...`，
 * 把 `/v1` 写进配置会把这两条链一起带歪。`/v1` 只在**展示与复制**时追加。
 * 幂等：运营手填的 `https://api.x.com/v1` 已在保存期被剥掉，这里再判一次
 * 是为了不依赖「保存一定经过后端」这个假设（例如有人直接改库）。
 */
export function openaiBase(origin: string): string {
  const v = (origin || "").trim().replace(/\/+$/, "");
  if (!v) return "";
  return v.endsWith("/v1") ? v : `${v}/v1`;
}

/** 站名首字生成的内联 SVG 图标（不依赖任何外部资源，也不需要上传） */
export function initialFavicon(name: string, color = "#2563eb"): string {
  const ch = initialOf(name, "A");
  const esc = ch.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  const svg =
    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64">` +
    `<rect width="64" height="64" rx="14" fill="${color}"/>` +
    `<text x="32" y="45" text-anchor="middle" fill="#fff" font-size="36" ` +
    `font-family="-apple-system,Segoe UI,Microsoft YaHei,sans-serif" font-weight="600">${esc}</text></svg>`;
  return `data:image/svg+xml,${encodeURIComponent(svg)}`;
}

/** 全站品牌消费点统一入口（门户/管理台/登录/注册/文档/预览共用同一份缓存） */
export function useSite(): SiteView {
  const { data } = usePortalConfigQuery();
  const s = data?.site;
  const name = s?.site_name || SITE_FALLBACK.siteName;
  const logoImage = s?.logo_image || "";
  // 兜底链在这里收口一次：其它调用点只管读 site.favicon
  const favicon = s?.favicon_image || logoImage || initialFavicon(name);
  const apiBases = data?.api?.bases ?? [];
  return {
    name,
    logoImage,
    favicon,
    contactEmail: s?.contact_email || "",
    siteUrl: s?.site_url || "",
    // 首屏（配置未返回）用同源：既不会显示别人的域名，也不会闪一个空串
    apiBase: apiBases[0] || (typeof window === "undefined" ? "" : window.location.origin),
    apiBases,
    apiSource: data?.api?.source ?? "request",
    footerText: s?.footer_text || SITE_FALLBACK.footerText,
    icpBeian: s?.icp_beian || SITE_FALLBACK.icpBeian,
  };
}
