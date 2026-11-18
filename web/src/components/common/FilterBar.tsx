import { Fragment, useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { cn } from "@/lib/utils";
import { Icon, type IconName } from "./Icon";
import { Select, type SelectItem } from "./Select";
import { Sheet } from "./Dialog";
import { btnClass, type BtnVariant } from "./PageHead";

export interface FilterField {
  id: string;
  /** search/text=输入框；date=日期选择；select=下拉；btn=动作按钮（置于右侧 acts 区） */
  t: "search" | "text" | "select" | "btn" | "date";
  label?: string;
  placeholder?: string;
  items?: SelectItem[];
  allLabel?: string;
  allValue?: string;
  /** 默认值（用于「已生效条件」chip 判定） */
  def?: string;
  /**
   * date 型专用：相邻的两个（或多个）range 字段合并为一个「起 – 止」控件。
   * 桌面渲染为单一外框（.pbar-range），移动端 Sheet 内仍逐项独立成行。
   */
  range?: boolean;
  /** btn 型按钮样式与图标 */
  variant?: BtnVariant;
  icon?: IconName;
  /**
   * 自定义控件（提供时忽略 t 的内置渲染），参数为当前值、回写函数与**控件 id**。
   *
   * id 只有移动端 Sheet 会传（那里有 `<label htmlFor>` 要指过来）；桌面端不传 ——
   * 两端同时存在两个相同的 id 会让 label 指向隐藏的那一个。
   */
  renderer?: (value: string, onChange: (value: string) => void, id?: string) => ReactNode;
  /**
   * 已生效条件 chip 里显示的值文本。
   *
   * 默认直接显示原始值，对 `image` / `openai` 这类代码值是能看的；
   * 但下拉里显示的是"图像"/"OpenAI 官方"时，chip 上写代码值就成了两套说法
   * （用户按 chip 找不到下拉里那一项）。实体型筛选必须提供。
   */
  display?: (value: string) => string;
}

/** 胶囊筛选分组（桌面仅渲染内容，Sheet 内额外渲染 label） */
export interface FilterChipGroup {
  label?: string;
  node: ReactNode;
}

export interface FilterBarProps {
  fields: FilterField[];
  /** 受控值（字段 id → 值）；未提供视为空 */
  value: Record<string, string>;
  onChange: (id: string, value: string) => void;
  /** 重置全部条件 */
  onReset: () => void;
  /** btn 型字段点击回调 */
  onAction?: (id: string) => void;
  /** 胶囊槽位（供应商/模态等非字段型条件）：桌面渲染在筛选条下方，移动端渲染于 Sheet 内 */
  chips?: FilterChipGroup[];
  /** 胶囊区已生效条件数（并入计数徽标与「重置」可见性） */
  extraActive?: number;
  className?: string;
  /**
   * 文本类字段（search/text）的输入防抖毫秒数，默认 300（P2-15）。
   *
   * 为什么在组件内做：输入框是唯一知道「用户还在打字」的地方。由调用方防抖意味着
   * 每个页面都要复制一份定时器逻辑，而漏掉一个就是「每敲一个字符发一次请求 + 结果乱序」。
   * 下拉/日期不防抖（一次交互即终态）。
   */
  debounceMs?: number;
}

/** 输入框 type 映射（date 走原生日期选择器，其余保持 text/search） */
const inputType = (t: FilterField["t"]) => (t === "search" ? "search" : t === "date" ? "date" : "text");

/**
 * 把「相邻且标记 range 的 date 字段」归并为数组项，其余字段原样返回。
 * 归并只影响桌面渲染分组，值契约（每个字段一个 id）与移动端 Sheet 均不变。
 */
function groupFields(fields: FilterField[]): Array<FilterField | FilterField[]> {
  const out: Array<FilterField | FilterField[]> = [];
  for (let i = 0; i < fields.length; i++) {
    const f = fields[i];
    if (f.t !== "date" || !f.range) {
      out.push(f);
      continue;
    }
    const group = [f];
    let j = i + 1;
    while (j < fields.length && fields[j].t === "date" && fields[j].range) {
      group.push(fields[j]);
      j++;
    }
    out.push(group);
    i = j - 1;
  }
  return out;
}

/**
 * 筛选条（对齐原型 renderFilterBar 契约）
 * 受控适配：原型返回 {val,set,setItems,reset,onChange} 的命令式 API，
 * React 侧收敛为 {value,onChange,onReset}（值由调用方或 TanStack Query 状态持有），
 * setItems 通过重渲染 fields.items 表达。
 * 桌面内联展开；≤767px 折叠为单行触发条 + 底部 Sheet（.filter-sheet 语义类）。
 * 已生效条件渲染为可移除 chip，避免「筛选生效不可见」。
 */
export function FilterBar({
  fields,
  value,
  onChange,
  onReset,
  onAction,
  chips,
  extraActive = 0,
  className,
  debounceMs = 300,
}: FilterBarProps) {
  const [sheetOpen, setSheetOpen] = useState(false);
  // 文本框的「即时值」：受控于本地 draft，外部（value）只在没有待提交的输入时回灌，
  // 否则防抖窗口内每敲一次都会被父组件的旧值覆盖回去。
  const [draft, setDraft] = useState<Record<string, string>>({});
  const timers = useRef<Record<string, number>>({});

  // 外部值变化（重置 / 移除 chip / 后退）时丢弃对应 draft，让输入框跟随外部状态。
  useEffect(() => {
    setDraft((prev) => {
      let changed = false;
      const next: Record<string, string> = { ...prev };
      for (const id of Object.keys(prev)) {
        if (timers.current[id]) continue;
        if ((value[id] ?? "") !== prev[id]) {
          delete next[id];
          changed = true;
        }
      }
      return changed ? next : prev;
    });
  }, [value]);

  useEffect(
    () => () => {
      for (const t of Object.values(timers.current)) window.clearTimeout(t);
    },
    [],
  );

  const emitText = useCallback(
    (id: string, next: string) => {
      setDraft((d) => ({ ...d, [id]: next }));
      const pending = timers.current[id];
      if (pending) window.clearTimeout(pending);
      if (debounceMs <= 0) {
        onChange(id, next);
        return;
      }
      timers.current[id] = window.setTimeout(() => {
        delete timers.current[id];
        onChange(id, next);
      }, debounceMs);
    },
    [debounceMs, onChange],
  );

  /** 立刻提交（回车 / 失焦）：不等待防抖窗口。 */
  const flushText = useCallback(
    (id: string) => {
      const pending = timers.current[id];
      if (!pending) return;
      window.clearTimeout(pending);
      delete timers.current[id];
      onChange(id, draft[id] ?? "");
    },
    [draft, onChange],
  );

  const inputs = fields.filter((f) => f.t !== "btn");
  const actions = fields.filter((f) => f.t === "btn");

  const effective = inputs.filter((f) => {
    const v = value[f.id] ?? "";
    return v !== "" && v !== (f.def ?? "");
  });
  const activeCount = effective.length + extraActive;

  const renderField = (f: FilterField) => {
    const v = value[f.id] ?? f.def ?? "";
    if (f.renderer) return f.renderer(v, (next) => onChange(f.id, next));
    if (f.t === "select") {
      return (
        <Select
          key={f.id}
          items={f.items ?? []}
          allLabel={f.allLabel}
          allValue={f.allValue}
          value={v}
          aria-label={f.label ?? f.placeholder ?? f.id}
          onChange={(e) => onChange(f.id, e.target.value)}
        />
      );
    }
    const text = draft[f.id] ?? v;
    return (
      <input
        key={f.id}
        type={inputType(f.t)}
        placeholder={f.placeholder}
        aria-label={f.label ?? f.placeholder ?? f.id}
        value={text}
        onChange={(e) => emitText(f.id, e.target.value)}
        onBlur={() => flushText(f.id)}
        onKeyDown={(e) => {
          if (e.key === "Enter") flushText(f.id);
        }}
      />
    );
  };

  const renderSheetField = (f: FilterField) => (
    <div className="pfield" key={f.id}>
      <label className="pfield-lb" htmlFor={`filter-${f.id}`}>
        {f.label ?? f.placeholder ?? f.id}
      </label>
      {f.renderer ? (
        f.renderer(value[f.id] ?? f.def ?? "", (next) => onChange(f.id, next), `filter-${f.id}`)
      ) : f.t === "select" ? (
        <Select
          id={`filter-${f.id}`}
          items={f.items ?? []}
          allLabel={f.allLabel}
          allValue={f.allValue}
          value={value[f.id] ?? f.def ?? ""}
          onChange={(e) => onChange(f.id, e.target.value)}
        />
      ) : (
        <input
          id={`filter-${f.id}`}
          type={inputType(f.t)}
          placeholder={f.placeholder}
          value={draft[f.id] ?? value[f.id] ?? f.def ?? ""}
          onChange={(e) => emitText(f.id, e.target.value)}
          onBlur={() => flushText(f.id)}
          onKeyDown={(e) => {
            if (e.key === "Enter") flushText(f.id);
          }}
        />
      )}
    </div>
  );

  return (
    <div className={cn("pbar-wrap", activeCount > 0 && "has-tokens", className)}>
      <div className="pbar">
        <div className="pbar-fields">
          {groupFields(inputs).map((item) =>
            Array.isArray(item) ? (
              <span className="pfield" key={item.map((f) => f.id).join("+")}>
                <span className="pbar-range" role="group" aria-label="时间范围">
                  {item.map((f, i) => (
                    <Fragment key={f.id}>
                      {i > 0 ? (
                        <span className="dash" aria-hidden="true">
                          –
                        </span>
                      ) : null}
                      {renderField(f)}
                    </Fragment>
                  ))}
                </span>
              </span>
            ) : (
              <span className="pfield" key={item.id}>
                {renderField(item)}
              </span>
            ),
          )}
        </div>
        <button type="button" className="pbar-trigger" onClick={() => setSheetOpen(true)}>
          <Icon name="filter" size={15} />
          筛选
          {activeCount > 0 ? <span className="pbar-n">{activeCount}</span> : null}
        </button>
        <div className="acts">
          {activeCount > 0 ? (
            <button type="button" className="btn" onClick={onReset}>
              重置
            </button>
          ) : null}
          {actions.map((f) => (
            <button
              key={f.id}
              type="button"
              className={btnClass(f.variant ?? "default")}
              onClick={() => onAction?.(f.id)}
            >
              {f.icon ? <Icon name={f.icon} size={15} /> : null}
              {f.label}
            </button>
          ))}
        </div>
      </div>

      {chips?.length ? (
        <div className="pbar-chips">
          {chips.map((g, i) => (
            <div key={g.label ?? i}>{g.node}</div>
          ))}
        </div>
      ) : null}

      {effective.length > 0 ? (
        <div className="ptokens">
          {effective.map((f) => (
            <span className="ptoken" key={f.id}>
              <b>{f.label ?? f.placeholder ?? f.id}</b>
              <span>{f.display ? f.display(value[f.id] ?? "") : value[f.id]}</span>
              <button
                type="button"
                className="x"
                aria-label={`移除筛选 ${f.label ?? f.id}`}
                onClick={() => onChange(f.id, f.def ?? "")}
              >
                ×
              </button>
            </span>
          ))}
          <button type="button" className="ptoken-clear" onClick={onReset}>
            清空全部
          </button>
        </div>
      ) : null}

      <Sheet
        open={sheetOpen}
        onClose={() => setSheetOpen(false)}
        title="筛选"
        className="filter-sheet"
        actions={[
          { label: "重置", onClick: onReset },
          { label: "完成", variant: "primary", onClick: () => setSheetOpen(false) },
        ]}
      >
        {/* 仅在 Sheet 打开时渲染 Sheet 字段：常驻渲染会让同一控件产生 2 份 DOM
            （桌面 aria-label + Sheet label[for]），导致 getByLabel 严格模式命中 2 个 */}
        {sheetOpen ? inputs.map(renderSheetField) : null}
        {sheetOpen && chips?.length
          ? chips.map((g, i) => (
              <div className="pfield" key={g.label ?? i}>
                {g.label ? <label className="pfield-lb">{g.label}</label> : null}
                {g.node}
              </div>
            ))
          : null}
      </Sheet>
    </div>
  );
}
