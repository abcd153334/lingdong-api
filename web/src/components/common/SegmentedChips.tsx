import type { ReactNode } from "react";
import { cn } from "@/lib/utils";
import type { TabItem } from "./Tabs";

export interface SegmentedChipsProps {
  items: TabItem[];
  /** 单选值（multiple 为 false 时生效） */
  value?: string;
  /** 多选值（multiple 为 true 时生效） */
  values?: string[];
  multiple?: boolean;
  onChange: (key: string) => void;
  /** 自定义胶囊内容（优先于 item.label），用于供应商圆点/计数等 */
  renderItem?: (item: TabItem) => ReactNode;
  /** 追加到每个胶囊的类名（如 inline-flex 布局） */
  itemClassName?: string;
  className?: string;
}

/**
 * 胶囊筛选（对齐原型 .kind-switch .kbtn）
 * 全局 white-space:nowrap + flex-shrink:0（由 .chips > * 规则收口），防窄屏被压扁。
 * multiple=true 时以 values 判定选中；否则以 value 判定。
 */
export function SegmentedChips({
  items,
  value,
  values,
  multiple,
  onChange,
  renderItem,
  itemClassName,
  className,
}: SegmentedChipsProps) {
  const isActive = (key: string) =>
    multiple ? (values ?? []).includes(key) : key === value;

  return (
    <div className={cn("chips", className)} role="group">
      {items.map((it) => (
        <button
          key={it.key}
          type="button"
          aria-pressed={isActive(it.key)}
          className={cn("kbtn", itemClassName, isActive(it.key) && "active")}
          onClick={() => onChange(it.key)}
        >
          {renderItem ? renderItem(it) : it.label}
        </button>
      ))}
    </div>
  );
}
