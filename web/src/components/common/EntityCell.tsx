import type { ReactNode } from "react";

import { EntityIcon, type EntityIconSize } from "./EntityIcon";
import { cn } from "@/lib/utils";

export interface EntityCellProps {
  /** 图标地址（后端下发的 `icon_url`；空/未设置 → 首字占位） */
  iconUrl?: string;
  /** 实体名（首字占位也用它） */
  name: string;
  /** 品牌色（首字占位底色） */
  color?: string;
  size?: EntityIconSize;
  /** 等宽显示（模型 ID / 渠道 slug 之类） */
  mono?: boolean;
  /** 名称之后的附加说明（如渠道的规格 slug）—— 与名称同一行，不单独占列 */
  children?: ReactNode;
  className?: string;
}

/**
 * 「图标 + 文本」单元格：列表里所有实体名的**唯一**排版入口。
 *
 * 为什么必须收敛成一个组件：`.ent-cell` 原先在十几个页面里手写，
 * 每一处都自己决定图标尺寸、间距、是否 `mono`。结果是同一列模型名在
 * 「调用记录」和「任务」里对齐不同、字号不同 —— 看起来像两个产品。
 *
 * `aria-hidden` 只覆盖图标（EntityIcon 内部处理），文本照常可读：
 * 读屏应该读模型名，不是把图片 alt 再读一遍。
 */
export function EntityCell({
  iconUrl,
  name,
  color,
  size = "sm",
  mono = false,
  children,
  className,
}: EntityCellProps) {
  return (
    <span className={cn("ent-cell", className)}>
      <EntityIcon url={iconUrl} name={name} color={color} size={size} />
      <span className={cn("ent-cell-text", mono && "mono")}>{name}</span>
      {children}
    </span>
  );
}
