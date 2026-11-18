import type { SelectHTMLAttributes } from "react";
import { cn } from "@/lib/utils";

/** 选项：字符串，或 [value, label] 元组（对齐原型 fillSelect 契约） */
export type SelectItem = string | [string | number, string];

export interface SelectProps
  extends Omit<SelectHTMLAttributes<HTMLSelectElement>, "children"> {
  items: SelectItem[];
  /** 传入即渲染「全部」首项 */
  allLabel?: string;
  allValue?: string;
}

/** 下拉填充（对齐原型 fillSelect：allLabel 首项 + items 列表） */
export function Select({ items, allLabel, allValue = "", className, ...rest }: SelectProps) {
  return (
    <select className={cn(className)} {...rest}>
      {allLabel !== undefined ? <option value={allValue}>{allLabel}</option> : null}
      {items.map((it, i) =>
        Array.isArray(it) ? (
          <option key={`${it[0]}-${i}`} value={it[0]}>
            {it[1]}
          </option>
        ) : (
          <option key={`${it}-${i}`} value={it}>
            {it}
          </option>
        ),
      )}
    </select>
  );
}
