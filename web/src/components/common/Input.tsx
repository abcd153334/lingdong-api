import { cn } from "@/lib/utils";
import type { InputHTMLAttributes, Ref } from "react";

export interface InputProps extends InputHTMLAttributes<HTMLInputElement> {
  ref?: Ref<HTMLInputElement>;
}

/**
 * 文本类输入框唯一入口（S22）。
 *
 * 为什么需要它：项目此前没有 input 组件，样式完全依赖 components.css 里的
 * `input[type="..."]` 元素选择器。任何漏写 `type` 的裸 `<input>` 都会脱离这套选择器，
 * 落回 Tailwind preflight 的零边框重置，渲染成 23px 无框盒子——系统配置页「站点」页签
 * 整片输入框风格割裂就是这么来的。
 *
 * 本组件做两件事：
 *   1. `type` 默认 `text`，杜绝“忘写 type”这一整类缺陷；
 *   2. 语义化转发 ref / 原生属性，保持与裸 `<input>` 等价的可控性。
 *
 * 约定：文本、密码、数字、邮箱、日期等一律用 `<Input>`；checkbox/radio 请用
 * 设计系统里的开关（SwitchRow）或按需显式声明 `type`。
 */
export function Input({ type = "text", className, ...rest }: InputProps) {
  return <input type={type} className={cn(className)} {...rest} />;
}
