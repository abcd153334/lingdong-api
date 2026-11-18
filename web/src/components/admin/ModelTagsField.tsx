import { useState, type CSSProperties } from "react";
import { cn } from "@/lib/utils";
import { ModelTag } from "@/components/common/ModelTag";
import type { TagColorOption } from "@/types";

/** 一条已添加的标签（日期是业务时区的 YYYY-MM-DD，提交时原样作为 until）。 */
export interface TagFormRow {
  label: string;
  /** 12 色板 key（小写）或 `#RRGGBB`（大写，与后端存储一致） */
  color: string;
  /** `""` = 长期有效 */
  untilDate: string;
}

interface Props {
  /**
   * 由外层 `Field` 注入：`<Field label>` 会把 id 传给子节点，
   * 这里转交到「标签名称」输入框，否则 label 的 `for` 指向不存在的元素。
   */
  id?: string;
  value: TagFormRow[];
  onChange: (next: TagFormRow[]) => void;
  /** 后端下发的 12 色板（唯一色表；为空说明列表还没加载，此时不让添加） */
  palette: TagColorOption[];
}

const MAX_TAGS = 3;
const MAX_LABEL = 12;
/** 色板里查不到 key 时的兜底色（只有手改过库才会命中） */
const FALLBACK_HEX = "#64748B";

/**
 * 模型营销标签编辑器。
 *
 * 交互只有两种动作：**添加**与**删除**。标签不支持就地修改 —— 改一个字也要
 * 删掉重加，这样列表里每一枚胶囊就是线上真实展示的那一枚（所见即所得），
 * 不存在「草稿态」与「已保存态」两套外观。
 *
 * 四条约束：
 *  1. 单个添加表单（名称 + 可选到期日 + 添加，下一行选主题），不再每条标签一套表单；
 *  2. 列表顺序 = 数组顺序 = 线上展示顺序，不提供排序控件（要换位置就删了重加）；
 *  3. 不自建色表：色块来自 `palette`，自定义色用原生取色器；
 *  4. 不判权威：文案长度/重复/日期只做行内提示，真正的判定在后端。
 */
export function ModelTagsField({ id, value, onChange, palette }: Props) {
  const [label, setLabel] = useState("");
  const [untilDate, setUntilDate] = useState("");
  // 只记 key：空串表示「跟着色板第一项走」，色板晚到时也不会停在无色状态。
  const [color, setColor] = useState("");
  const activeColor = color || palette[0]?.key || "";

  const today = new Date().toISOString().slice(0, 10);
  const isExpired = (until: string) => until !== "" && until < today;
  const trimmed = label.trim();
  const full = value.length >= MAX_TAGS;
  const duplicate = trimmed !== "" && value.some((t) => t.label.trim() === trimmed);
  const expired = isExpired(untilDate);

  const warning = duplicate
    ? "该标签已存在"
    : expired
      ? "到期日已过，添加后不会展示"
      : "";
  const canAdd = trimmed !== "" && !duplicate && !expired && !full && activeColor !== "";

  const addDisabledReason = full
    ? `最多 ${MAX_TAGS} 个标签`
    : palette.length === 0
      ? "主题尚未加载"
      : !trimmed
        ? "先填标签名称"
        : warning || "添加";

  const hexOf = (key: string) =>
    key.startsWith("#")
      ? key
      : (palette.find((c) => c.key === key)?.hex ?? FALLBACK_HEX);

  const add = () => {
    if (!canAdd) return;
    onChange([...value, { label: trimmed, color: activeColor, untilDate }]);
    setLabel("");
    setUntilDate("");
    setColor("");
  };

  return (
    <div className="model-tags-editor">
      <div className="model-tags-current">
        {value.length === 0 ? (
          <span className="model-tags-empty">还没有标签</span>
        ) : (
          value.map((t, i) => (
            <span
              key={`${t.label}-${i}`}
              className="model-tags-chip"
              title={t.untilDate ? `到期 ${t.untilDate}（到期后自动隐藏）` : "长期展示"}
            >
              <ModelTag
                tag={{
                  label: t.label,
                  color_hex: hexOf(t.color),
                  expired: isExpired(t.untilDate),
                }}
              />
              <button
                type="button"
                className="model-tags-del"
                aria-label={`删除标签 ${t.label}`}
                title="删除"
                onClick={() => onChange(value.filter((_, idx) => idx !== i))}
              >
                ✕
              </button>
            </span>
          ))
        )}
      </div>

      <div className="model-tags-add">
        <div className="model-tags-add__row">
          <input
            id={id}
            type="text"
            maxLength={MAX_LABEL}
            placeholder="标签名称，如：限时免费"
            aria-label="标签名称"
            value={label}
            onChange={(e) => setLabel(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                add();
              }
            }}
          />
          <input
            type="date"
            className="model-tags-add__until"
            aria-label="到期日期（可选）"
            title="可选：到期后自动隐藏；留空 = 长期有效"
            value={untilDate}
            onChange={(e) => setUntilDate(e.target.value)}
          />
          <button
            type="button"
            className="btn"
            disabled={!canAdd}
            title={canAdd ? undefined : addDisabledReason}
            onClick={add}
          >
            添加
          </button>
        </div>
        <div className="model-tags-add__colors">
          {palette.map((c) => (
            <button
              key={c.key}
              type="button"
              className={cn("model-tags-swatch", activeColor === c.key && "is-on")}
              style={{ background: c.hex } as CSSProperties}
              title={c.label}
              aria-label={c.label}
              aria-pressed={activeColor === c.key}
              onClick={() => setColor(c.key)}
            />
          ))}
          <input
            type="color"
            className="model-tags-color"
            aria-label="自定义颜色"
            title="自定义颜色"
            value={hexOf(activeColor).toLowerCase()}
            onChange={(e) => setColor(e.target.value.toUpperCase())}
          />
        </div>
        {warning ? <span className="model-tags-warn">{warning}</span> : null}
      </div>
    </div>
  );
}
