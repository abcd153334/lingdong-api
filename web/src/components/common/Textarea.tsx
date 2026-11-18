import { cn } from "@/lib/utils";
import type { Ref, TextareaHTMLAttributes } from "react";

export interface TextareaProps extends TextareaHTMLAttributes<HTMLTextAreaElement> {
  ref?: Ref<HTMLTextAreaElement>;
}

/**
 * 多行文本输入的唯一入口（与 `Input` 同构）。
 *
 * 为什么需要它：PEM 类字段（商户私钥 / 平台证书 / 应用私钥）必须多行展示，
 * 否则运营无法核对「BEGIN/END 行是否完整」——而一行残缺的 PEM 只有在
 * 真正发起支付时才会暴露成「签名错误」。
 *
 * 样式不走 `input[type=...]` 那套元素选择器（textarea 匹配不到），
 * 所以这里固定挂 `ta` 类，由 components.css 统一提供外观。
 */
export function Textarea({ className, rows = 6, ...rest }: TextareaProps) {
  return <textarea rows={rows} className={cn("ta", className)} {...rest} />;
}
