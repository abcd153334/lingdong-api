import { cn } from "@/lib/utils";
import { Tip } from "./Tip";

export interface SwitchRowProps {
  /** 可视文本；传空串表示「无文本 label」（如表格单元格内仅需一个裸开关） */
  label: string;
  /** 规则解释（渲染为问号提示） */
  tip?: string;
  id?: string;
  checked: boolean;
  onChange: (checked: boolean) => void;
  disabled?: boolean;
  className?: string;
  /** 无障碍名覆盖（默认取 label；label 为空时必须提供，否则控件无名称） */
  ariaLabel?: string;
  /** 测试锚点（挂在真实的 checkbox 上，便于 e2e 断言 checked） */
  testId?: string;
}

/**
 * 开关行（单一 DOM 契约：label.switch > input + i；样式见 components.css .switch）
 * 原型曾存在两套 switch 尺寸/DOM，此处收口为一套，间距由 .switch-row 类承担（去内联 style）。
 * label 为空串时不渲染可视文本（表格单元格等紧凑场景），此时须以 ariaLabel 提供无障碍名。
 */
export function SwitchRow({
  label,
  tip,
  id,
  checked,
  onChange,
  disabled,
  className,
  ariaLabel,
  testId,
}: SwitchRowProps) {
  return (
    <div className={cn("switch-row", className)}>
      {label ? (
        <span>
          {label}
          {tip ? (
            <span className="hint-inline">
              <Tip text={tip} />
            </span>
          ) : null}
        </span>
      ) : null}
      <label className="switch">
        <input
          id={id}
          type="checkbox"
          data-testid={testId}
          aria-label={ariaLabel ?? (label || undefined)}
          checked={checked}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked)}
        />
        <i />
      </label>
    </div>
  );
}
