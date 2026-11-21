import { Input } from "@/components/common/Input";
import { Select } from "@/components/common/Select";
import { fieldOf } from "@/lib/pluginTypes";
import type { PluginEntity, PluginFilter } from "@/types";

/** 筛选值：`eq`/`contains` 是字符串；`range` 是 `a..b`（任一侧可为空）。 */
export type FilterValues = Record<string, string>;

/** 把 `a..b` 拆成两端（没有 `..` 时两端都空 —— 服务端会拒绝，不在这里猜）。 */
export function splitRange(v: string): [string, string] {
  const i = v.indexOf("..");
  if (i < 0) return ["", ""];
  return [v.slice(0, i), v.slice(i + 2)];
}

function joinRange(lo: string, hi: string): string {
  if (!lo && !hi) return "";
  return `${lo}..${hi}`;
}

/**
 * 插件筛选条：**完全按后端下发的 `op` 渲染**。
 *
 * 为什么不在前端按 type 推一遍控件：算子（eq/range/contains）是 manifest 的声明，
 * 服务端按它拼 WHERE；前端再推一次就是第二个真相源，两边一漂就会出现
 * "界面给了区间、服务端按等值查"的静默错。
 *
 * 布局：整体 `flex-wrap`，每项是"标签 + 控件"；range 用两个输入框。
 * 375px 下自然折行 —— 不写横向滚动，否则手机上永远看不全筛选条。
 */
export function PluginFilters({
  entity,
  values,
  onChange,
  disabled,
}: {
  entity: PluginEntity;
  values: FilterValues;
  onChange: (key: string, value: string) => void;
  disabled?: boolean;
}) {
  const filters = entity.list.filters;
  if (!filters.length) return null;
  return (
    <div className="plugin-filters">
      {filters.map((f) => {
        const id = `plugin-filter-${f.key}`;
        const v = values[f.key] ?? "";
        return (
          <div className="plugin-filter" key={f.key}>
            <label className="plugin-filter__label" htmlFor={id}>
              {f.label}
            </label>
            <Control id={id} filter={f} entity={entity} value={v} disabled={disabled} onChange={onChange} />
          </div>
        );
      })}
    </div>
  );
}

function Control({
  id,
  filter,
  entity,
  value,
  disabled,
  onChange,
}: {
  id: string;
  filter: PluginFilter;
  entity: PluginEntity;
  value: string;
  disabled?: boolean;
  onChange: (key: string, value: string) => void;
}) {
  if (filter.op === "eq" && filter.type === "bool") {
    return (
      <Select
        id={id}
        allLabel="全部"
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(filter.key, e.target.value)}
        items={[
          ["1", "是"],
          ["0", "否"],
        ]}
      />
    );
  }
  if (filter.op === "eq") {
    // 取值来自字段表（与表单下拉同源），不在 descriptor 的 filters 里再存一份。
    const opts = fieldOf(entity, filter.key)?.options ?? [];
    return (
      <Select
        id={id}
        allLabel="全部"
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(filter.key, e.target.value)}
        items={opts}
      />
    );
  }
  if (filter.op === "range") {
    const [lo, hi] = splitRange(value);
    return (
      <div className="plugin-filter__range">
        <Input
          id={id}
          type={filter.type === "date" ? "date" : "number"}
          aria-label={`${filter.label}（起）`}
          value={lo}
          disabled={disabled}
          onChange={(e) => onChange(filter.key, joinRange(e.target.value, hi))}
        />
        <span className="muted small">至</span>
        <Input
          type={filter.type === "date" ? "date" : "number"}
          aria-label={`${filter.label}（止）`}
          value={hi}
          disabled={disabled}
          onChange={(e) => onChange(filter.key, joinRange(lo, e.target.value))}
        />
      </div>
    );
  }
  return (
    <Input
      id={id}
      type="search"
      placeholder={`按${filter.label}筛选`}
      aria-label={`按${filter.label}筛选`}
      value={value}
      disabled={disabled}
      onChange={(e) => onChange(filter.key, e.target.value)}
    />
  );
}
