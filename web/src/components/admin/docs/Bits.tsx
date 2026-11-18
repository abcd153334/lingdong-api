import { useState, type ReactNode } from "react";

import { Icon, type IconName } from "@/components/common/Icon";
import { Input } from "@/components/common/Input";
import { SwitchRow } from "@/components/common/SwitchRow";
import { Textarea } from "@/components/common/Textarea";
import { Tip } from "@/components/common/Tip";
import { cn } from "@/lib/utils";
import type { DocKV } from "@/types/docsadmin";

/**
 * 编辑页的四个最小积木。
 *
 * 只做「一个内容列 ↔ 一个控件」的映射，不掺业务判断：
 * 卡片只负责分节与标题，控件只负责「读出来什么、存回去还是什么」。
 */

/** 分节卡片：图标 + 标题 + 可选问号提示 + 右侧动作。 */
export function Card({
  title,
  icon,
  tip,
  actions,
  children,
  className,
}: {
  title: string;
  icon: IconName;
  /** 只有「不解释会误解」才给：默认无提示 */
  tip?: string;
  actions?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <section className={cn("adp-card", className)}>
      <div className="adp-card__hd">
        <Icon name={icon} size={16} className="adp-card__ico" />
        {/* 不用 <h3>：门禁 M7 禁止手写标题层级，板块标题本就靠视觉表达 */}
        <div className="adp-card__t">
          {title}
          {tip ? <Tip text={tip} /> : null}
        </div>
        {actions ? <div className="adp-card__acts">{actions}</div> : null}
      </div>
      <div className="adp-card__bd">{children}</div>
    </section>
  );
}

/**
 * 字符串数组 ↔ 多行文本。
 *
 * 这是「一行一条」类内容的唯一输入形态：注意事项、别名、涉及字段、上传字段。
 * 用数组而不是逗号分隔：值本身可能含逗号（`/v1/a,b`），拆错一次就少一个值。
 */
export function LinesField({
  value,
  onChange,
  rows = 2,
  placeholder,
  ariaLabel,
  className,
}: {
  value: string[];
  onChange: (v: string[]) => void;
  rows?: number;
  placeholder?: string;
  ariaLabel: string;
  className?: string;
}) {
  return (
    <Textarea
      className={className}
      rows={rows}
      value={value.join("\n")}
      placeholder={placeholder}
      aria-label={ariaLabel}
      onChange={(e) => onChange(e.target.value.split("\n"))}
    />
  );
}

/**
 * 有序对象编辑器（预设体 / 响应示例）。
 *
 * 值保持「用户输入的那段字」：数字写成 `2` 就存 `2`，写成 `"2"` 就存字符串 ——
 * 输入框里看到什么、请求里发什么，不能由前端悄悄改类型。
 */
export function KeyValueEditor({
  value,
  onChange,
  keyPlaceholder = "键",
  valuePlaceholder = "值",
}: {
  value: DocKV[];
  onChange: (v: DocKV[]) => void;
  keyPlaceholder?: string;
  valuePlaceholder?: string;
}) {
  const patch = (i: number, p: Partial<DocKV>) => {
    const next = value.slice();
    next[i] = { ...next[i], ...p };
    onChange(next);
  };
  return (
    <div className="adp-table">
      {value.length > 0 ? (
        <div className="adp-thead adp-thead--kv">
          <span>{keyPlaceholder}</span>
          <span>{valuePlaceholder}</span>
          <span />
        </div>
      ) : null}
      {value.map((it, i) => (
        <div className="adp-trow adp-trow--kv" key={i}>
          <Input
            className="adp-cell-k"
            value={it.k}
            placeholder={keyPlaceholder}
            aria-label={`第 ${i + 1} 行的键`}
            onChange={(e) => patch(i, { k: e.target.value })}
          />
          <Input
            className="adp-cell-v"
            value={textOf(it.v)}
            placeholder={valuePlaceholder}
            aria-label={`第 ${i + 1} 行的值`}
            onChange={(e) => patch(i, { v: parseLoose(e.target.value) })}
          />
          <button
            type="button"
            className="icon-btn adp-cell-del"
            aria-label={`删除第 ${i + 1} 行`}
            onClick={() => onChange(value.filter((_, k) => k !== i))}
          >
            <Icon name="trash" size={14} />
          </button>
        </div>
      ))}
      <button
        type="button"
        className="btn btn-ghost btn-sm"
        onClick={() => onChange([...value, { k: "", v: "" }])}
      >
        <Icon name="plus" size={14} />
        加一行
      </button>
    </div>
  );
}

/** 文本 ↔ 值：能当 JSON 解析就按 JSON（数字/布尔/null/对象），否则按字符串。 */
export function textOf(v: unknown): string {
  if (v === null || v === undefined) return "";
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  return JSON.stringify(v);
}

function parseLoose(s: string): unknown {
  const t = s.trim();
  if (t === "") return "";
  // 只有「看起来像 JSON 字面量」才解析：`123abc` 不能被当成 123。
  if (t === "true" || t === "false" || t === "null" || /^-?\d+(\.\d+)?$/.test(t) || t[0] === "{" || t[0] === "[") {
    try {
      return JSON.parse(t);
    } catch {
      return s;
    }
  }
  return s;
}

/**
 * 一行开关（隐藏 / 原样转发 / 不进骨架）。
 *
 * 走设计系统的 `SwitchRow` 而不是手写 checkbox：裸控件会落回浏览器默认样式
 * （与全站开关两套外观），而页面层的裸控件是门禁 M7 的必拦项。
 */
export function ToggleLine({
  label,
  checked,
  onChange,
  tip,
}: {
  label: string;
  checked: boolean;
  onChange: (v: boolean) => void;
  tip?: string;
}) {
  return (
    <SwitchRow
      label={label}
      {...(tip ? { tip } : {})}
      checked={checked}
      onChange={onChange}
    />
  );
}

/** 本地的「展开/收起」状态（字段卡用）：不受控、不进内容。 */
export function useToggle(initial = false): [boolean, () => void, (v: boolean) => void] {
  const [on, setOn] = useState(initial);
  return [on, () => setOn((v) => !v), setOn];
}
