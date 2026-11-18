import { useSite } from "@/lib/site";
import { cn, initialOf } from "@/lib/utils";

export interface SiteLogoProps {
  /** 覆盖品牌图（配置预览用：传表单里的草稿值）；不传取站点配置 */
  image?: string;
  /** 覆盖降级首字来源（预览用）；不传取站点名称 */
  name?: string;
  /** 上下文尺寸类：由 .p-logo/.a-side .logo/.auth-card .brand 上下文决定尺寸 */
  className?: string;
  alt?: string;
}

/**
 * 站点品牌标（唯一工厂）
 * 消费点：门户顶栏 / 管理台侧栏 / 登录 / 注册 / 文档站 / 配置预览
 *
 * 不再有「Logo 字符」配置项：无图时的降级恒为**站点名称首字**。
 * 单独维护一份 1–2 字文案只会在改名后与站名不一致（用户看到的是两个品牌）。
 */
export function SiteLogo({ image, name, className, alt }: SiteLogoProps) {
  const site = useSite();
  const src = image ?? site.logoImage;
  const label = name ?? site.name;
  if (src) {
    return <img src={src} alt={alt ?? label} className={cn("logo-img", className)} />;
  }
  // 首字规则与实体图标共用（initialOf）：两处各写一份 slice(0,1) 的结局是
  // emoji 被劈成半个码元、英文大小写规则不一致。
  return <span className={cn("dot", className)}>{initialOf(label)}</span>;
}
