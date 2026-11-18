import type { CSSProperties } from "react";
import { cn } from "@/lib/utils";
import type { ModelTag as ModelTagData } from "@/types";

/**
 * 模型营销标签的唯一渲染实现。
 *
 * 颜色只认后端下发的 `color_hex`：前端若维护一份「色板 key → hex」映射，
 * 就成了第二张色表 —— 后端改色板时两边必然漂移，且同一标签在两处显示不同色。
 * `--model-tag-fg` 在 components.css 里定义（X7 要求行首声明），这里只覆盖值。
 */
type TagLike = Pick<ModelTagData, "label" | "color_hex"> & {
  /** 管理台回显带这个字段（对外永远不会出现：过期项已被后端过滤） */
  expired?: boolean;
};

export function ModelTag({ tag, className }: { tag: TagLike; className?: string }) {
  return (
    <span
      className={cn("model-tag", tag.expired && "is-expired", className)}
      style={{ "--model-tag-fg": tag.color_hex } as CSSProperties}
      title={tag.expired ? "已过期，不会对外展示" : undefined}
    >
      {tag.label}
    </span>
  );
}

/**
 * 标签列表：按后端给的顺序渲染，超出 `max` 折叠成 `+N`。
 *
 * 不排序、不过滤 —— 顺序与可见性都是后端的口径；前端再排一次就会出现
 * 「管理台看到的顺序和门户不一样」。
 */
export function ModelTagList({
  tags,
  max,
  className,
}: {
  tags?: readonly TagLike[] | null;
  max?: number;
  className?: string;
}) {
  if (!tags || tags.length === 0) return null;
  const shown = max && max > 0 ? tags.slice(0, max) : tags;
  const rest = tags.slice(shown.length);
  return (
    <span className={cn("model-tag-list", className)}>
      {shown.map((t, i) => (
        <ModelTag key={`${t.label}-${i}`} tag={t} />
      ))}
      {rest.length > 0 ? (
        <span className="model-tag-list__more" title={rest.map((t) => t.label).join("、")}>
          +{rest.length}
        </span>
      ) : null}
    </span>
  );
}
