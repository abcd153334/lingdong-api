import { useEffect, useState, type CSSProperties } from "react";

import { cn, initialOf } from "@/lib/utils";

export type EntityIconSize = "xs" | "sm" | "md" | "lg";

export interface EntityIconProps {
  /**
   * 图标的**绝对地址**（后端下发的 `icon_url` / 图库项 `url`）。
   *
   * 它不是 token：地址形态由后端 `service/iconref` 唯一决定，前端只负责渲染。
   * 空 → 首字占位（未配图标，或该图标指向的对象已不存在）。
   */
  url?: string;
  /** 首字占位用的名字（实体名） */
  name?: string;
  /** 品牌色（首字占位底色；有图标时不生效） */
  color?: string;
  size?: EntityIconSize;
  className?: string;
}

/**
 * 实体图标的**唯一**渲染入口（模型 / 渠道 / 供应商 / 文档站成员共用）。
 *
 * 三条规则写死在这里，各页不要再各写一份：
 *   1. 有地址 → `<img>`（白底砖 + `object-fit: contain`）；
 *   2. 图片加载失败 → 回落首字（不能显示浏览器的坏图图标：
 *      地址有效但对象丢了时，坏图会让整张表格看起来"坏了"）；
 *   3. 无地址 → 首字占位（底色 = 实体品牌色）。
 *
 * 尺寸只由 CSS 类决定，**不写内联宽高**：内联宽高一旦出现在页面上，
 * 页面层的布局样式门禁（M3）就失效了，且换个尺寸要改 n 处。
 *
 * 容器带 `aria-hidden`：图标是纯装饰，读屏应该读实体名而不是重复一遍。
 */
export function EntityIcon({ url = "", name, color, size = "sm", className }: EntityIconProps) {
  const [failed, setFailed] = useState(false);

  // url 变化时重置失败态：列表里同一个组件实例会换数据（按 id 复用），
  // 不重置就会出现"某一行坏过一次之后，好图也永远显示首字"。
  useEffect(() => {
    setFailed(false);
  }, [url]);

  const base = cn("ent-icon", `ent-icon--${size}`, className);

  if (url && !failed) {
    return (
      <span className={base} aria-hidden="true">
        <img src={url} alt="" decoding="async" loading="lazy" onError={() => setFailed(true)} />
      </span>
    );
  }

  // 品牌色走 CSS 变量而不是 background：只有占位模式需要它，
  // 而变量形式不会在图片模式下留下一个未使用的内联样式。
  const style = color ? ({ "--ent-icon-bg": color } as CSSProperties) : undefined;
  return (
    <span className={cn(base, "ent-icon--text")} style={style} aria-hidden="true">
      {initialOf(name)}
    </span>
  );
}
