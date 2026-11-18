import { DocPage, splitParagraphs } from "@/components/common/DocPage";
import { usePortalConfigQuery } from "@/lib/api";

/**
 * 用户协议页（S19，公开路由，未登录可读；设计 §4.13.1 #22「agreement_content + 生效日期」）。
 * 正文取 /api/portal/config 的 agreement_content（管理台「站点内容」卡编辑，空行分段）。
 * 生效日期：system_config 无独立配置键（U5 裁决）→ 页面内静态常量，配置中心扩展该键后替换。
 */
const EFFECTIVE_DATE = "2026-10-01";

export function AgreementPage() {
  const { data } = usePortalConfigQuery();

  return (
    <DocPage
      title="用户协议"
      desc="注册与使用本服务前请完整阅读"
      effectiveDate={EFFECTIVE_DATE}
      paragraphs={splitParagraphs(data?.agreement_content)}
      emptyText="站点尚未配置用户协议内容"
    />
  );
}
