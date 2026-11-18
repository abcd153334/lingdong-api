import { useEffect, useMemo } from "react";

import { Icon } from "@/components/common/Icon";
import { Input } from "@/components/common/Input";
import { Select } from "@/components/common/Select";
import { SwitchRow } from "@/components/common/SwitchRow";
import { Textarea } from "@/components/common/Textarea";
import { cn } from "@/lib/utils";
import type { DocsAdminField } from "@/types/docsadmin";

import { useToggle } from "./Bits";

/**
 * 一个请求字段（或返回字段）的折叠卡。
 *
 * 收起态只显示「一眼要判断的东西」：名字、类型、必填、说明首行、几枚 chip
 * （只模型 / 原样转发 / 厂商扩展 / 条件必填）。展开态按**四个分组**铺开：
 *
 *   身份（2 列）    字段名 / 类型 / 位置 / 必填 / 默认值 / 仅这些模型
 *   说明（整行）    多行文本，换行即分段
 *   取值（子块）    取值来源 / 示例值 → 可选值表 → 取值约束
 *   前台表现（子块）层级 / 原样转发 / 不进骨架
 *
 * 为什么是 2 列而不是 3 列：request 6 项、returns 4 项都能被 2 整除，
 * **永远不会出现"一行只剩半格"**；3 列时 returns 模式必有孤儿单元。
 * 为什么开关不进网格：塞进网格单元后它和上面的下拉不在一条基线上（历史截图可证）。
 */
const TYPES = ["string", "integer", "number", "boolean", "array", "object", "file", "binary", "none", "any"];
const LOCATIONS: [string, string][] = [
  ["body", "Body"],
  ["query", "Query"],
  ["path", "Path"],
  ["header", "Header"],
  ["form", "Form"],
];
const REQUIRED: [string, string][] = [
  ["required", "必填"],
  ["optional", "选填"],
  ["conditional", "条件必填"],
];
/**
 * 层级：四个值都会在前台挂 chip（P 用弱化样式）。
 *
 * `S` 曾经被写成「S 响应」—— 那是错的：S 是 Standard（标准字段），
 * 与"响应"没有任何关系。写错的后果是运营照着下拉框把请求字段标成"响应"，
 * 而前台又只显示 V/N，错得没人发现。
 */
const TIERS: [string, string][] = [
  ["P", "平台字段"],
  ["S", "标准字段"],
  ["V", "厂商扩展"],
  ["N", "兼容写法"],
  ["standard", "标准字段（旧值）"],
];
/** 取值来源：决定前台是否显示「取值随模型」。 */
const SOURCES: [string, string][] = [
  ["static", "静态列表"],
  ["none", "无"],
  ["group_members", "模型清单"],
  ["image_tier", "随模型档位"],
];

export function FieldCard({
  field,
  index,
  total,
  onChange,
  onMove,
  onRemove,
  mode,
  openAll,
}: {
  field: DocsAdminField;
  index: number;
  total: number;
  onChange: (f: DocsAdminField) => void;
  onMove: (dir: -1 | 1) => void;
  onRemove: () => void;
  mode: "request" | "returns";
  /** 批量展开信号：数值变化时跟随（0 = 全收起） */
  openAll?: number;
}) {
  const [open, , setOpen] = useToggle(false);
  const patch = (p: Partial<DocsAdminField>) => onChange({ ...field, ...p });

  // 「全部展开/收起」只在信号变化时跟随：用户自己点的展开不会被下一次 render 覆盖。
  useEffect(() => {
    if (openAll === undefined) return;
    setOpen(openAll > 0);
  }, [openAll, setOpen]);

  const chips = useMemo(() => {
    const out: string[] = [];
    if (field.required === "conditional") out.push("条件必填");
    if (field.only_models) out.push(`仅 ${field.only_models}`);
    if (field.tier === "V") out.push("厂商扩展");
    if (field.tier === "N") out.push("兼容别名");
    if (field.passthrough) out.push("原样转发");
    if (mode === "returns" && field.sketch_skip) out.push("不进骨架");
    if (field.enum?.length) out.push(`${field.enum.length} 个可选值`);
    if (field.constraints?.length) out.push(`${field.constraints.length} 条约束`);
    return out;
  }, [field, mode]);

  const summary = field.desc.split("\n")[0]?.trim() ?? "";

  return (
    <div className={cn("adp-fld", open && "on")}>
      <div className="adp-fld__hd">
        <button type="button" className="adp-fld__hit" aria-expanded={open} onClick={() => setOpen(!open)}>
          <Icon name={open ? "chevronDown" : "chevronRight"} size={13} className="adp-caret" />
          <span className="adp-fld__name">{field.name || "未命名字段"}</span>
          <span className="adp-fld__type">{field.type}</span>
          {mode === "request" ? (
            <span className="adp-fld__where">
              {LOCATIONS.find(([v]) => v === field.in)?.[1] ?? field.in}
            </span>
          ) : null}
          <span
            className={cn(
              "adp-fld__req",
              field.required === "required" && "must",
              field.required === "conditional" && "cond",
            )}
          >
            {REQUIRED.find(([v]) => v === field.required)?.[1] ?? field.required}
          </span>
          {summary ? <span className="adp-fld__sum">{summary}</span> : null}
        </button>
        <div className="adp-fld__acts">
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
          <button type="button" className="icon-btn adp-danger" aria-label="删除字段" onClick={onRemove}>
            <Icon name="trash" size={14} />
          </button>
        </div>
      </div>

      {chips.length > 0 ? (
        <div className="adp-fld__chips">
          {chips.map((c) => (
            <span className="adp-chip" key={c}>
              {c}
            </span>
          ))}
        </div>
      ) : null}

      {open ? (
        <div className="adp-fld__bd">
          {/* 身份 */}
          <div className="adp-grid adp-grid--2">
            <label className="adp-mini">
              <span>字段名</span>
              <Input value={field.name} onChange={(e) => patch({ name: e.target.value })} />
            </label>
            <label className="adp-mini">
              <span>类型</span>
              <Select items={TYPES} value={field.type} onChange={(e) => patch({ type: e.target.value })} />
            </label>
            {mode === "request" ? (
              <label className="adp-mini">
                <span>位置</span>
                <Select items={LOCATIONS} value={field.in} onChange={(e) => patch({ in: e.target.value })} />
              </label>
            ) : null}
            {mode === "request" ? (
              <label className="adp-mini">
                <span>必填</span>
                <Select
                  items={REQUIRED}
                  value={field.required}
                  onChange={(e) => patch({ required: e.target.value })}
                />
              </label>
            ) : null}
            <label className="adp-mini">
              <span>默认值</span>
              <Input value={field.default} onChange={(e) => patch({ default: e.target.value })} />
            </label>
            <label className="adp-mini">
              <span>仅这些模型</span>
              <Input
                value={field.only_models ?? ""}
                placeholder="如 gpt-image 系列；留空=全部适用"
                onChange={(e) => patch({ only_models: e.target.value })}
              />
            </label>
          </div>

          {field.required === "conditional" ? (
            <label className="adp-mini">
              <span>条件必填的说明（发布前必填）</span>
              <Input
                value={field.required_note ?? ""}
                placeholder="如「与 image 至少给一个」"
                onChange={(e) => patch({ required_note: e.target.value })}
              />
            </label>
          ) : null}

          {/* 说明 */}
          <label className="adp-mini">
            <span>说明（换行即分段，页面上原样显示）</span>
            <Textarea
              rows={4}
              value={field.desc}
              placeholder="填什么、影响什么；边界与例外；需要时一段一行"
              onChange={(e) => patch({ desc: e.target.value })}
            />
          </label>

          {/* 取值 */}
          <div className="adp-sub">
            <div className="adp-sub__t">取值</div>
            <div className="adp-grid adp-grid--2">
              <label className="adp-mini">
                <span>取值来源</span>
                <Select
                  items={SOURCES}
                  value={field.value_source || "static"}
                  onChange={(e) => patch({ value_source: e.target.value })}
                />
              </label>
              <label className="adp-mini">
                <span>示例值（参数表上的「示例」chip，同时参与生成示例代码）</span>
                <Input
                  value={exampleText(field.example)}
                  placeholder="留空不显示"
                  onChange={(e) => patch({ example: exampleValue(e.target.value) })}
                />
              </label>
            </div>

            <EnumRows value={field.enum ?? []} onChange={(v) => patch({ enum: v })} />

            <label className="adp-mini">
              <span>取值约束（一条一行，原文显示成 chip）</span>
              <Textarea
                rows={3}
                value={(field.constraints ?? []).map((c) => c.text).join("\n")}
                placeholder={"最长边不超过 3840px\n两条边都是 16 的倍数"}
                onChange={(e) =>
                  patch({
                    constraints: e.target.value
                      .split("\n")
                      .map((t) => t.trim())
                      .filter(Boolean)
                      .map((text) => ({ kind: "text", value: 0, text })),
                  })
                }
              />
            </label>
          </div>

          {/* 前台表现 */}
          <div className="adp-sub">
            <div className="adp-sub__t">前台表现</div>
            <label className="adp-mini">
              <span>层级（V/N 会在参数表上挂 chip）</span>
              <Select
                items={TIERS}
                value={field.tier || "standard"}
                onChange={(e) => patch({ tier: e.target.value })}
              />
            </label>
            <div className="adp-toglets">
              <SwitchRow
                label="原样转发"
                checked={!!field.passthrough}
                onChange={(v) => patch({ passthrough: v })}
              />
              {mode === "returns" ? (
                <SwitchRow
                  label="不进骨架"
                  checked={!!field.sketch_skip}
                  onChange={(v) => patch({ sketch_skip: v })}
                />
              ) : null}
            </div>
          </div>
        </div>
      ) : null}
    </div>
  );
}

function EnumRows({
  value,
  onChange,
}: {
  value: NonNullable<DocsAdminField["enum"]>;
  onChange: (v: NonNullable<DocsAdminField["enum"]>) => void;
}) {
  const patch = (i: number, p: Partial<{ value: string; desc: string; deprecated: boolean }>) => {
    const next = value.slice();
    next[i] = { ...next[i], ...p };
    onChange(next);
  };
  return (
    <div className="adp-table">
      {value.length > 0 ? (
        <div className="adp-thead adp-thead--enum">
          <span>可选值</span>
          <span>这个值是什么意思</span>
          <span>废弃</span>
          <span />
        </div>
      ) : null}
      {value.map((it, i) => (
        <div className="adp-trow adp-trow--enum" key={i}>
          <Input
            className="adp-cell-val adp-mono"
            value={it.value}
            placeholder="值"
            aria-label={`第 ${i + 1} 个可选值`}
            onChange={(e) => patch(i, { value: e.target.value })}
          />
          <Input
            className="adp-cell-edesc"
            value={it.desc ?? ""}
            placeholder="说明（可空）"
            aria-label={`第 ${i + 1} 个可选值的说明`}
            onChange={(e) => patch(i, { desc: e.target.value })}
          />
          <button
            type="button"
            className="adp-tgl adp-cell-tgl"
            aria-pressed={!!it.deprecated}
            aria-label={`第 ${i + 1} 个可选值标记为废弃`}
            title="标记为已废弃：页面上这一行会加删除线"
            onClick={() => patch(i, { deprecated: !it.deprecated })}
          >
            废弃
          </button>
          <button
            type="button"
            className="icon-btn adp-cell-del"
            aria-label={`删除第 ${i + 1} 个可选值`}
            onClick={() => onChange(value.filter((_, k) => k !== i))}
          >
            <Icon name="trash" size={14} />
          </button>
        </div>
      ))}
      <button
        type="button"
        className="btn btn-ghost btn-sm"
        onClick={() => onChange([...value, { value: "", desc: "" }])}
      >
        <Icon name="plus" size={14} />
        加一个可选值
      </button>
    </div>
  );
}

/** 示例值 ↔ 文本框：字符串原样，其余按 JSON 文本。 */
function exampleText(v: unknown): string {
  if (v === null || v === undefined) return "";
  if (typeof v === "string") return v;
  return JSON.stringify(v);
}

function exampleValue(s: string): unknown {
  const t = s.trim();
  if (t === "") return undefined;
  if (t === "true" || t === "false" || t === "null" || /^-?\d+(\.\d+)?$/.test(t) || t[0] === "{" || t[0] === "[") {
    try {
      return JSON.parse(t);
    } catch {
      return s;
    }
  }
  return s;
}
