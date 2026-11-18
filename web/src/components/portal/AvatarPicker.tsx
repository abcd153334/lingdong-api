import { useRef } from "react";
import { toast } from "sonner";
import { Icon } from "@/components/common/Icon";
import { UserAvatar, type UserAvatarSize } from "@/components/common/UserAvatar";
import { useAvatarDeleteMutation, useAvatarUploadMutation } from "@/lib/api";
import { cn } from "@/lib/utils";

/** 与后端 `upload.AvatarMaxBytes` 对齐；前端预检只是省一次往返，权威判定仍在服务端。 */
const MAX_BYTES = 2 << 20;
const ACCEPT = "image/png,image/jpeg,image/webp";

export interface AvatarPickerProps {
  url?: string;
  size?: UserAvatarSize;
  /** 提供「移除」（恢复默认头像）。账户弹窗两端都开 —— 同一个接口，不为不存在的差异留分支 */
  showRemove?: boolean;
  disabled?: boolean;
  className?: string;
}

/**
 * 头像选择器：点整块头像 → 选文件 → 立即上传。
 *
 * 两点不能省：
 *  - **点整块**（不是 hover 才出现的按钮）：触屏没有 hover，移动端等于没有入口；
 *  - `<input type="file">` 必须视觉隐藏但仍可被自动化命中（Playwright `setInputFiles`）。
 *
 * 「移除」做成头像右上角的角标而不是旁边的文字按钮：账户弹窗左栏只有 200px，
 * 文字按钮会把「名称 + 角色」那一栏挤到只剩 60px。角标常显，触屏同样可点。
 */
export function AvatarPicker({
  url,
  size = "lg",
  showRemove = false,
  disabled = false,
  className,
}: AvatarPickerProps) {
  const inputRef = useRef<HTMLInputElement>(null);
  const upload = useAvatarUploadMutation();
  const remove = useAvatarDeleteMutation();
  const pending = upload.isPending || remove.isPending;
  const locked = disabled || pending;

  const onFile = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    // 先清空 value：否则连续选同一张图不会触发 change。
    e.target.value = "";
    if (!file) return;
    if (file.size > MAX_BYTES) {
      toast.error("头像不能超过 2MB");
      return;
    }
    upload.mutate(file, {
      onSuccess: () => toast.success("头像已更新"),
      onError: (err) => toast.error(err instanceof Error ? err.message : "头像上传失败"),
    });
  };

  return (
    <div className={cn("avatar-pick", className)}>
      <button
        type="button"
        className="avatar-pick__btn"
        aria-label={url ? "更换头像" : "设置头像"}
        title="更换头像"
        disabled={locked}
        onClick={() => inputRef.current?.click()}
      >
        <UserAvatar url={url} size={size} />
        {!disabled && (
          <span className="avatar-pick__badge" aria-hidden="true">
            <Icon name="upload" size={10} />
          </span>
        )}
      </button>
      <input
        ref={inputRef}
        type="file"
        accept={ACCEPT}
        hidden
        aria-label="选择头像文件"
        onChange={onFile}
      />
      {showRemove && url && !disabled ? (
        <button
          type="button"
          className="avatar-pick__remove"
          aria-label="移除头像"
          title="恢复默认头像"
          disabled={pending}
          onClick={() =>
            remove.mutate(undefined, {
              onSuccess: () => toast.success("已恢复默认头像"),
              onError: (err) => toast.error(err instanceof Error ? err.message : "移除失败"),
            })
          }
        >
          <Icon name="close" size={10} />
        </button>
      ) : null}
    </div>
  );
}
