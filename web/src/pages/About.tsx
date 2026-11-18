import { DocPage, splitParagraphs } from "@/components/common/DocPage";
import { usePortalConfigQuery } from "@/lib/api";

/**
 * 关于页（S19，公开路由，未登录可读；设计 §4.13.1 #21「品牌信息 + about_content」）。
 * 正文取 /api/portal/config 的 about_content（管理台「站点内容」卡编辑，空行分段）。
 */
export function AboutPage() {
  const { data } = usePortalConfigQuery();

  return (
    <DocPage
      title="关于"
      desc="平台介绍与联系方式"
      paragraphs={splitParagraphs(data?.about_content)}
      emptyText="站点尚未配置关于页内容"
    />
  );
}
