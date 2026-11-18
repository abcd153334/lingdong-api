import dayjs from "dayjs";

import type { AnnouncementType, PortalAnnouncementItem } from "@/types";

/**
 * 公告的**唯一**语义来源：类型色板、未读数、时间格式。
 *
 * 弹层与整页共用这三件事。分散在两处时最容易漂的是「未读」——
 * 一处按接口字段、一处按条目状态，徽标和列表就会各说各话。
 */

/** 公告类型色板与文案（5 枚举，冻结；颜色直接消费主题变量）。外部只通过 typeMeta 读 */
const TYPE_META: Record<AnnouncementType, { label: string; color: string }> = {
  default: { label: "公告", color: "var(--t3)" },
  ongoing: { label: "进行中", color: "var(--primary)" },
  success: { label: "已完成", color: "var(--success)" },
  warning: { label: "注意", color: "var(--warning)" },
  error: { label: "重要", color: "var(--danger)" },
};

/**
 * 取类型元信息；未知枚举回退「公告」。
 *
 * 参数收 string 而不是 AnnouncementType：类型是服务端下发的，配置被手改过
 * （或未来扩枚举）时前端不该崩，也不该出现空白标签。
 */
export function typeMeta(type: string): { label: string; color: string } {
  return TYPE_META[type as AnnouncementType] ?? TYPE_META.default;
}

/** 未读数 = 条目里 read=false 的条数（接口不再下发 unread，这是唯一算法）。 */
export function unreadOf(items?: PortalAnnouncementItem[]): number {
  if (!items?.length) return 0;
  return items.filter((item) => !item.read).length;
}

/** 公告时间：同年省略年份（弹层只有 384px），跨年补全，避免「去年 12-31」被当今年。 */
export function noticeTime(value: string): string {
  const t = dayjs(value);
  if (!t.isValid()) return "";
  return t.year() === dayjs().year() ? t.format("MM-DD HH:mm") : t.format("YYYY-MM-DD");
}
