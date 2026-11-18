import { cloneElement, isValidElement, useId, type ReactElement, type ReactNode } from "react";
import { cn } from "@/lib/utils";
import { Tip } from "./Tip";

export interface FieldProps {
  /** 标签文字 */
  label: string;
  /** 必填标记（默认 false） */
  req?: boolean;
  /** 问号提示（hover / 点击 / Enter 弹出） */
  tip?: string;
  /** label 内的附加内联节点（如字数计数） */
  labelExtra?: ReactNode;
  /** 底部提示文字（**只用于表单错误**，静态、非问号） */
  hint?: ReactNode;
  /** 底部提示是否为警告色 */
  hintWarn?: boolean;
  /** 占两列（仅在 `.form-grid` 容器生效） */
  span?: boolean;
  /** 自定义 className 添加到 `.form-item` */
  className?: string;
  /** 显式指定 label 关联的控件 id；不传则自动生成 */
  htmlFor?: string;
  children: ReactNode;
}

/**
 * 表单字段共享组件（对齐原型 `.form-item` 结构）。
 *
 * 两个 prop 的分工是硬约定，不要混用：
 *   - `tip`   规则解释 → 问号气泡。**默认选择**：只要用户会「误解」才写，
 *             直观可推断的一律不写（见 docs §4.12）。
 *   - `hint`  表单错误 / 实时反馈 → 常驻小字。**必须留在明面**：
 *             出错时用户不会去 hover 找原因，藏起来等于让他盲改。
 *
 * 可访问性：`tip` 存在时本组件会给子控件注入 `id` 与 `aria-describedby`，
 * 指向 `Tip` 里的隐藏描述节点。少了这一步，输入框就没有任何可访问名——
 * 支付渠道参数表单之前在很长一段时间都是这个状态。
 */
export function Field({
  label,
  req,
  tip,
  labelExtra,
  hint,
  hintWarn,
  span,
  className,
  htmlFor,
  children,
}: FieldProps) {
  const uid = useId();
  const tipId = tip ? `${uid}-tip` : undefined;
  const controlId = htmlFor ?? `${uid}-ctl`;

  // 只在「单个元素子节点」时注入属性：Fragment / 数组 / 文本原样渲染。
  let control = children;
  if (isValidElement(children)) {
    const child = children as ReactElement<{ id?: string; "aria-describedby"?: string }>;
    const patch: Record<string, unknown> = {};
    // 传了 htmlFor 说明调用方已经知道目标控件是谁（可能子节点是包裹结构，
    // 真正带 id 的 input 在里层）。此时再给 children 注入同名 id 会撞出两个
    // 同 id 元素 —— layout 的「DOM 内 id 唯一」门禁就是这么挂的。
    if (!htmlFor && !child.props.id) patch.id = controlId;
    const describedBy = [child.props["aria-describedby"], tipId].filter(Boolean).join(" ");
    if (describedBy) patch["aria-describedby"] = describedBy;
    if (Object.keys(patch).length > 0) {
      control = cloneElement(child, patch);
    }
  }

  return (
    <div className={cn("form-item", span && "span-2", className)}>
      <label htmlFor={controlId}>
        {label}
        {req && <span className="req">*</span>}
        {tip && <Tip text={tip} id={tipId} />}
        {labelExtra}
      </label>
      {control}
      {hint && <div className={cn("hint", hintWarn && "warn")}>{hint}</div>}
    </div>
  );
}
