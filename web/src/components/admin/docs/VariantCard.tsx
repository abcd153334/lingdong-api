import { Icon } from "@/components/common/Icon";
import { Input } from "@/components/common/Input";
import { Select } from "@/components/common/Select";
import { SwitchRow } from "@/components/common/SwitchRow";
import { Textarea } from "@/components/common/Textarea";

import { LinesField } from "./Bits";
import { FieldCard } from "./FieldCard";
import type { DocsAdminField, DocsAdminVariant } from "@/types/docsadmin";

/**
 * 一个状态码的响应形态。
 *
 * `kind` / `envelope` 是**逐变体**的：202 是 JSON 任务对象，200 下载产物是 image/png。
 * 折叠成整篇一个值，就会出现「预览里 200 变成 JSON、发布后变二进制」这种只能靠肉眼
 * 发现的分叉。
 */
const KINDS: [string, string][] = [
  ["json", "JSON"],
  ["binary", "二进制"],
  ["sse", "事件流"],
  ["none", "无响应体"],
];

export function VariantCard({
  variant,
  index,
  total,
  onChange,
  onMove,
  onRemove,
}: {
  variant: DocsAdminVariant;
  index: number;
  total: number;
  onChange: (v: DocsAdminVariant) => void;
  onMove: (dir: -1 | 1) => void;
  onRemove: () => void;
}) {
  const patch = (p: Partial<DocsAdminVariant>) => onChange({ ...variant, ...p });
  const patchField = (i: number, f: DocsAdminField) => {
    const next = variant.returns.slice();
    next[i] = f;
    patch({ returns: next });
  };

  return (
    <div className="adp-var">
      <div className="adp-var__hd">
        <span className="adp-var__status">{variant.status}</span>
        <Input
          value={variant.label}
          aria-label="这条响应的名字"
          placeholder="如「成功」「已受理」"
          onChange={(e) => patch({ label: e.target.value })}
        />
        <Select
          items={KINDS.map(([v, l]) => [v, l] as [string, string])}
          value={variant.kind || "json"}
          aria-label="响应格式"
          onChange={(e) => patch({ kind: e.target.value })}
        />
        <SwitchRow
          className="adp-inline"
          label="统一封套"
          checked={variant.envelope}
          onChange={(v) => patch({ envelope: v })}
        />
        <div className="adp-var__acts">
          <button type="button" className="icon-btn" aria-label="上移" disabled={index === 0} onClick={() => onMove(-1)}>
            <Icon name="chevronUp" size={14} />
          </button>
          <button
            type="button"
            className="icon-btn"
            aria-label="下移"
            disabled={index === total - 1}
            onClick={() => onMove(1)}
          >
            <Icon name="chevronDown" size={14} />
          </button>
          <button type="button" className="icon-btn adp-danger" aria-label="删除这条响应" onClick={onRemove}>
            <Icon name="trash" size={14} />
          </button>
        </div>
      </div>

      <div className="adp-grid adp-grid--2">
        <label className="adp-mini">
          <span>状态码</span>
          <Input
            type="number"
            value={variant.status}
            onChange={(e) => patch({ status: Number(e.target.value) || 200 })}
          />
        </label>
        <label className="adp-mini">
          <span>Content-Type（非 JSON 时才要写）</span>
          <Input
            value={variant.content_type}
            placeholder="如 image/png"
            onChange={(e) => patch({ content_type: e.target.value })}
          />
        </label>
      </div>

      <div className="adp-sub">
        <div className="adp-sub__t">返回字段</div>
        {variant.returns.length === 0 ? (
          <p className="adp-empty">这条响应还没有返回字段。</p>
        ) : (
          variant.returns.map((f, i) => (
            <FieldCard
              key={i}
              field={f}
              index={i}
              total={variant.returns.length}
              mode="returns"
              onChange={(nf) => patchField(i, nf)}
              onMove={(dir) => patch({ returns: move(variant.returns, i, i + dir) })}
              onRemove={() => patch({ returns: variant.returns.filter((_, k) => k !== i) })}
            />
          ))
        )}
        <button
          type="button"
          className="btn btn-ghost btn-sm"
          onClick={() => patch({ returns: [...variant.returns, blankField("body")] })}
        >
          <Icon name="plus" size={14} />
          加返回字段
        </button>
      </div>

      <div className="adp-grid adp-grid--2">
        <label className="adp-mini">
          <span>非 JSON 响应的原文（事件流等）</span>
          <Textarea
            rows={4}
            value={variant.raw}
            placeholder="留空则不显示"
            onChange={(e) => patch({ raw: e.target.value })}
          />
        </label>
        <label className="adp-mini">
          <span>这条响应的说明（一行一条）</span>
          <LinesField
            value={variant.notes}
            rows={4}
            ariaLabel="这条响应的说明"
            onChange={(notes) => patch({ notes })}
          />
        </label>
      </div>
    </div>
  );
}

export function move<T>(list: T[], from: number, to: number): T[] {
  if (to < 0 || to >= list.length) return list;
  const next = list.slice();
  const [it] = next.splice(from, 1);
  next.splice(to, 0, it);
  return next;
}

export function blankField(inWhich: string): DocsAdminField {
  return {
    name: "",
    type: "string",
    in: inWhich,
    required: "optional",
    default: "",
    desc: "",
    tier: "standard",
  };
}
