import type { ReactNode } from "react";
import { cn } from "@/lib/utils";
import { Icon, type IconName } from "./Icon";

export type BtnVariant =
  | "default"
  | "primary"
  | "danger"
  | "grad-text"
  | "ghost-danger";

const VARIANT_CLASS: Record<BtnVariant, string> = {
  default: "btn",
  primary: "btn btn-primary",
  danger: "btn btn-danger",
  "grad-text": "btn btn-grad-text",
  "ghost-danger": "btn btn-ghost-danger",
};

/** 按钮变体 → 类名（页头/筛选条/弹窗页脚共用同一映射） */
export function btnClass(variant: BtnVariant = "default"): string {
  return VARIANT_CLASS[variant];
}

/** 页头动作（仅 btn / link 两型，对齐原型 renderPageHead） */
export interface PageAction {
  type: "btn" | "link";
  label: string;
  icon?: IconName;
  variant?: BtnVariant;
  /** link 型的跳转地址 */
  href?: string;
  id?: string;
  onClick?: () => void;
  /** 按钮禁用（如上传总闸关闭）；link 型忽略 */
  disabled?: boolean;
}

export interface PageHeadProps {
  title: string;
  desc?: string;
  /** ≤2 个（右侧） */
  actions?: PageAction[];
  /**
   * 动作区左侧的状态说明（如系统配置「本页有未保存的改动」）。
   *
   * 由 `.ph-acts-note` 承担：它在 flex 里 `margin-right:auto` 吃掉左侧余量，
   * 于是说明靠左、按钮靠右。页面层不许为此写内联布局（门禁 M3）。
   */
  note?: string;
  /** 标题 id（供 aria-labelledby 引用），默认 pageTitle */
  titleId?: string;
  /** 描述 id，默认 pageDesc */
  descId?: string;
}

/** 页头（对齐原型 renderPageHead：标题与 desc 同行 baseline，actions 右端） */
export function PageHead({
  title,
  desc,
  actions,
  note,
  titleId = "pageTitle",
  descId = "pageDesc",
}: PageHeadProps) {
  return (
    <div className="ph">
      <div className="ph-main">
        <h1 id={titleId}>{title}</h1>
        {desc ? (
          <p className="ph-desc" id={descId}>
            {desc}
          </p>
        ) : null}
      </div>
      {actions?.length || note ? (
        <div className="ph-acts">
          {note ? <span className="ph-acts-note">{note}</span> : null}
          {actions?.map((a) => {
            const cls = btnClass(a.variant ?? "default");
            const inner: ReactNode = (
              <>
                {a.icon ? <Icon name={a.icon} size={15} /> : null}
                {a.label}
              </>
            );
            return a.type === "link" ? (
              <a key={a.label} id={a.id} href={a.href} className={cn(cls)}>
                {inner}
              </a>
            ) : (
              <button
                key={a.label}
                id={a.id}
                type="button"
                className={cn(cls)}
                disabled={a.disabled}
                onClick={a.onClick}
              >
                {inner}
              </button>
            );
          })}
        </div>
      ) : null}
    </div>
  );
}
