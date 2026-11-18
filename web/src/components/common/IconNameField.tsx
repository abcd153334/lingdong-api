import { IconPicker } from "./IconPicker";
import { Input } from "./Input";
import { cn } from "@/lib/utils";

export interface IconNameFieldProps {
  /**
   * 文本框的 DOM id。**必须显式接收并转发**：
   * `Field.tsx` 传了 `htmlFor` 时不再给子元素注入 id（避免撞出两个同 id 元素），
   * 而它的 `<label for>` 指的就是这个 id —— 不转发的话点 label 毫无反应。
   */
  id: string;
  /** 由 `Field` 注入（tip 存在时指向隐藏描述节点），必须落到 `<input>` 上 */
  "aria-describedby"?: string;
  /** 自己设的图标 token（写回用） */
  icon: string;
  /** 该 token 对应的可渲染地址（来自实体响应；未保存的新选择由选择器自己记） */
  iconUrl?: string;
  onIconChange: (token: string) => void;
  /** 继承来的图标 token（模型 = 所属供应商）——只用于「来源」说明，不用于渲染 */
  fallback?: string;
  /** 继承来的图标地址（渲染用） */
  fallbackUrl?: string;
  fallbackLabel?: string;
  /** 首字占位底色 */
  color?: string;
  /** 首字母兜底（显示名为空时用实体 id 之类的稳定值，避免占位块变成「?」） */
  initialFrom?: string;
  text: string;
  onTextChange: (value: string) => void;
  placeholder?: string;
  maxLength?: number;
  /** 单值标识（模型 ID 之类）用等宽字体 */
  mono?: boolean;
  disabled?: boolean;
  className?: string;
}

/**
 * 「左图右文」身份行：一块图标砖 + 一个名称输入框。
 *
 * 全项目只有这一份实现。模型、渠道、供应商都是「图 + 名」，各页各写一遍的结果
 * 是间距/对齐/角标三处不一致，而这类不一致看起来就像"这个页面没做完"。
 *
 * 为什么图标与名称必须同一行：两者回答的是同一个问题（"这是谁"）。
 * 拆成两行后，运营改完名字要再往下找一个叫「图标」的字段，
 * 而那一行里除了一块图什么都没有 —— 排版上是纯浪费。
 */
export function IconNameField({
  id,
  "aria-describedby": describedBy,
  icon,
  iconUrl,
  onIconChange,
  fallback,
  fallbackUrl,
  fallbackLabel,
  color,
  initialFrom,
  text,
  onTextChange,
  placeholder,
  maxLength,
  mono,
  disabled,
  className,
}: IconNameFieldProps) {
  return (
    <div className={cn("icon-name", className)}>
      <IconPicker
        value={icon}
        valueUrl={iconUrl}
        onChange={onIconChange}
        fallback={fallback}
        fallbackUrl={fallbackUrl}
        fallbackLabel={fallbackLabel}
        color={color}
        name={text || initialFrom}
      />
      <Input
        id={id}
        type="text"
        className={cn(mono && "mono")}
        value={text}
        placeholder={placeholder}
        maxLength={maxLength}
        disabled={disabled}
        aria-describedby={describedBy}
        onChange={(e) => onTextChange(e.target.value)}
      />
    </div>
  );
}
