import { Link } from "@tanstack/react-router";
import { PageHead } from "./PageHead";
import { SiteLogo } from "./SiteLogo";
import { ThemeToggle } from "@/components/chrome/ThemeToggle";
import { useSite } from "@/lib/site";

/** 正文按空行分段（管理台以空行分隔段落） */
export function splitParagraphs(text?: string): string[] {
  return (text ?? "")
    .split(/\n\s*\n/)
    .map((p) => p.trim())
    .filter(Boolean);
}

export interface DocPageProps {
  title: string;
  /** 标题下方说明（逐字对齐原型） */
  desc: string;
  /** 生效日期（用户协议类页面，渲染在正文之后） */
  effectiveDate?: string;
  paragraphs: string[];
  /** 未配置正文时的空态文案 */
  emptyText: string;
}

/** 公开内容页（关于 / 用户协议共用外壳：最小品牌头部 + PageHead + 卡片正文） */
export function DocPage({ title, desc, effectiveDate, paragraphs, emptyText }: DocPageProps) {
  const site = useSite();
  // 正文若自带「生效日期」段会与下方 .eff-date 重复渲染（同一文案 2 份 → 读屏重复、e2e 严格模式二义）。
  // 后端专用端点 /api/portal/content/agreement 亦按该前缀剥离（portal_catalog.go effectiveDatePrefix）；
  // 公开页走的是未剥原文的 /api/portal/config，故在此兜底同一规则。
  const body = effectiveDate
    ? paragraphs.filter((p) => !p.startsWith("生效日期"))
    : paragraphs;
  return (
    <div className="p-main">
      <div className="ph">
        <div className="ph-main">
          <Link to="/home" className="p-logo">
            <SiteLogo />
            {site.name}
          </Link>
        </div>
        <div className="ph-acts">
          <ThemeToggle />
        </div>
      </div>
      <PageHead title={title} desc={desc} />
      <div className="card">
        <div className="doc">
          {body.length > 0 ? body.map((p, i) => <p key={i}>{p}</p>) : <p>{emptyText}</p>}
        </div>
        {effectiveDate ? (
          <div className="eff-date">
            <b>生效日期</b>　{effectiveDate}
          </div>
        ) : null}
      </div>
    </div>
  );
}
