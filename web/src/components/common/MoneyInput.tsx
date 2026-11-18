import { useEffect, useRef, useState, type InputHTMLAttributes } from "react";
import { microOf, microToInput } from "@/lib/money";

/**
 * 除受控值外的原生属性一律透传（`id`/`name`/`placeholder`/`aria-*`/`data-*`/`min`…）。
 *
 * 为什么必须透传：`Field` 会把 `aria-describedby`（指向问号气泡的隐藏描述节点）
 * 注入到子控件上；props 只收固定几项时这条注入会被**静默丢弃**，
 * 表现是「有问号、读屏读不到规则」——`Field` 注释里承诺的关联在金额框这一路是断的。
 */
export interface MoneyInputProps
  extends Omit<
    InputHTMLAttributes<HTMLInputElement>,
    "value" | "onChange" | "type" | "defaultValue" | "onBlur"
  > {
  /** 当前值（微）；null 表示"空"（未填 / 不限制，取决于调用方语义） */
  value: number | null | undefined;
  /** 每次输入**可解析**时回调；清空时回调 null */
  onChange: (micro: number | null) => void;
}

/**
 * 金额输入框（受控，但**允许空与中间态**）。
 *
 * 为什么不能直接 `value={microToInput(micro)}` + `onChange` 回写：
 *   1. 删到空串时 `microOf("")` 是 null，父级不更新 → 值又弹回去，**永远清不空**；
 *   2. 输入 "1." 时 `microOf("1.")` 合法（= 1 元），父级立刻回写成 "1"，
 *      小数点被吞掉，于是 1.5 元永远打不进去。
 *
 * 所以这里保留一份字符串草稿：草稿是用户看到的、包含中间态的真实文本；
 * 只有能解析时才向父级提交，失焦时才把草稿规范化（或回退到最后一次合法值）。
 */
export function MoneyInput({ value, onChange, ...rest }: MoneyInputProps) {
  const [draft, setDraft] = useState(() => microToInput(value ?? undefined));
  /** 最后一次向父级提交的值；用来区分「外部改了值」与「自己刚提交的」 */
  const emitted = useRef<number | null>(value ?? null);

  useEffect(() => {
    const v = value ?? null;
    if (v !== emitted.current) {
      emitted.current = v;
      setDraft(microToInput(v ?? undefined));
    }
  }, [value]);

  const commit = (raw: string) => {
    setDraft(raw);
    if (raw.trim() === "") {
      emitted.current = null;
      onChange(null);
      return;
    }
    const micro = microOf(raw);
    if (micro === null) return; // 中间态 / 非法：只留在草稿里，不打扰父级
    emitted.current = micro;
    onChange(micro);
  };

  return (
    <input
      {...rest}
      type="text"
      inputMode="decimal"
      autoComplete="off"
      value={draft}
      onChange={(e) => commit(e.target.value)}
      onBlur={() => {
        // 失焦时把草稿拉回"最后一次合法值"，避免页面上留着一串不会被提交的字符
        if (draft.trim() !== "" && microOf(draft) === null) {
          setDraft(microToInput(emitted.current ?? undefined));
        }
      }}
    />
  );
}
