import { useEffect, useState } from "react";
import DEFAULT_AVATAR from "@/assets/avatar-default.png";
import { cn } from "@/lib/utils";

/** 40px 是**最大档**：默认头像素材是 64×64，40px 在 2x 屏只需 80px，放大 1.25 倍可控。 */
export type UserAvatarSize = "sm" | "md" | "lg";

export interface UserAvatarProps {
  /** 后端下发的头像 data URL；空 / 未设置 → 默认头像 */
  url?: string;
  /** sm=30（移动顶栏）/ md=34（桌面顶栏）/ lg=40（用户中心、账户弹窗） */
  size?: UserAvatarSize;
  className?: string;
}

/**
 * 用户头像（全站唯一实现）。
 *
 * 为什么不吃 `initialOf`：默认图是**打包进产物**的一张图，「没有头像」的语义统一成
 * 「显示默认图」，再留一条首字回落就是第二个默认值。实体图标（EntityIcon / SiteLogo）
 * 仍有首字逻辑，但那是「模型/站点」的占位，不是用户头像。
 *
 * 默认图由 Vite 处理成 `dist/assets/avatar-default-<hash>.png`（4.3KB > 4KB 内联阈值），
 * 部署必须同步整个 `dist/assets/`。
 *
 * onError 的两个约束：
 *  - `url` 变化必须重置失败态（否则换过坏图后新图永远不显示）；
 *  - 失败回落时**不再对默认图重试**：否则默认图自己 404 会变成 onError 死循环。
 */
export function UserAvatar({ url, size = "md", className }: UserAvatarProps) {
  const [failed, setFailed] = useState(false);
  useEffect(() => setFailed(false), [url]);

  const src = url && !failed ? url : DEFAULT_AVATAR;

  return (
    <span className={cn("u-avatar", `u-avatar--${size}`, className)} aria-hidden="true">
      <img
        src={src}
        alt=""
        decoding="async"
        onError={() => {
          if (src !== DEFAULT_AVATAR) setFailed(true);
        }}
      />
    </span>
  );
}
